package paymentevents

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestMatcherScopesProviderChannelAndTimestamp(t *testing.T) {
	now := time.Now().UTC()
	candidates := []Candidate{
		{ID: 1, FiatAmount: 10, FiatCurrency: "EUR", AccountID: 1, AccountProvider: "WISE", NotificationChannels: []string{"push"}, PendingSince: now.Add(-time.Minute)},
		{ID: 2, FiatAmount: 10, FiatCurrency: "EUR", AccountID: 2, AccountProvider: "PayPal", NotificationChannels: []string{"email"}, PendingSince: now.Add(-time.Minute)},
	}
	event := PaymentEvent{Source: SourceEmail, Provider: "WISE", AmountMinor: 1000, Currency: "EUR", ProviderTimestamp: now}
	candidate, ambiguous := NewMatcher().Match(event, candidates, now)
	if candidate != nil || len(ambiguous) != 0 {
		t.Fatalf("unselected/wrong-provider event matched: %v %v", candidate, ambiguous)
	}
	candidates[0].NotificationChannels = []string{SourceEmail}
	candidate, _ = NewMatcher().Match(event, candidates, now)
	if candidate == nil || candidate.ID != 1 {
		t.Fatalf("selected Wise email did not match: %v", candidate)
	}
	event.ProviderTimestamp = candidates[0].PendingSince.Add(-3 * time.Minute)
	candidate, _ = NewMatcher().Match(event, candidates, now)
	if candidate != nil {
		t.Fatalf("stale event matched: %v", candidate)
	}
}

func TestAllChannelsPresent(t *testing.T) {
	if !AllChannelsPresent([]string{SourceEmail, SourcePush}, []string{SourcePush, SourceEmail}) {
		t.Fatal("both channels should satisfy gate in either order")
	}
	if AllChannelsPresent([]string{SourceEmail, SourcePush}, []string{SourcePush}) {
		t.Fatal("missing email should not satisfy gate")
	}
	if AllChannelsPresent(nil, nil) {
		t.Fatal("empty channel selection must fail closed")
	}
}

func TestEventStoreChannelGateIntegration(t *testing.T) {
	dsn := os.Getenv("PUSHPUSH_TEST_DB")
	if dsn == "" {
		t.Skip("set PUSHPUSH_TEST_DB to run the Postgres integration test")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	eventStore, err := NewEventStore(db)
	if err != nil {
		t.Fatal(err)
	}
	releaser := &recordingReleaser{}
	processor := NewProcessor(eventStore, releaser)
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	method := fmt.Sprintf("Wise Test %d", suffix)

	var accountID int64
	err = db.QueryRowContext(ctx, `
		INSERT INTO accounts (provider, category, notification_channels)
		VALUES ('WISE', NULL, ARRAY['push','email']::notification_channel[])
		RETURNING id
	`).Scan(&accountID)
	if err != nil {
		t.Fatal(err)
	}
	var methodID, transactionID int64
	err = db.QueryRowContext(ctx, `
		INSERT INTO payment_methods (name, category, account_id, is_active)
		VALUES ($1, 'Fiat Transmitter', $2, TRUE)
		RETURNING id
	`, method, accountID).Scan(&methodID)
	if err != nil {
		t.Fatal(err)
	}
	err = db.QueryRowContext(ctx, `
		INSERT INTO transactions
		    (direction, fiat_amount, fiat_currency, payment_method, status, transaction_complete)
		VALUES ('buy', 12.34, 'EUR', $1, $2, FALSE)
		RETURNING id
	`, method, "DexPositionTaken").Scan(&transactionID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM fiat_transaction_confirmations WHERE transaction_id=$1`, transactionID)
		_, _ = db.ExecContext(ctx, `DELETE FROM fiat_payment_events WHERE matched_transaction_id=$1 OR account_id=$2`, transactionID, accountID)
		_, _ = db.ExecContext(ctx, `DELETE FROM transactions WHERE id=$1`, transactionID)
		_, _ = db.ExecContext(ctx, `DELETE FROM payment_methods WHERE id=$1`, methodID)
		_, _ = db.ExecContext(ctx, `DELETE FROM accounts WHERE id=$1`, accountID)
	}()

	makeEvent := func(source string) (PaymentEvent, int64) {
		event := PaymentEvent{
			Source: source, Provider: "WISE", AccountID: &accountID,
			SourceEventID: fmt.Sprintf("%s-%d", source, suffix),
			AmountMinor:   1234, Currency: "EUR",
			ProviderTimestamp: time.Now(), ReceivedTimestamp: time.Now(),
			RawMessageHash: fmt.Sprintf("%064d", suffix),
		}
		id, duplicate, insertErr := eventStore.Insert(ctx, event, "parsed")
		if insertErr != nil || duplicate {
			t.Fatalf("insert %s event: id=%d duplicate=%v err=%v", source, id, duplicate, insertErr)
		}
		return event, id
	}
	push, pushID := makeEvent(SourcePush)
	first, err := processor.Complete(ctx, push, pushID, Candidate{ID: uint(transactionID), PaymentMethod: method})
	if err != nil {
		t.Fatal(err)
	}
	if first.State != CompletionWaiting || first.Ready || first.Advanced {
		t.Fatalf("push alone bypassed email gate: %+v", first)
	}
	email, emailID := makeEvent(SourceEmail)
	second, err := processor.Complete(ctx, email, emailID, Candidate{ID: uint(transactionID), PaymentMethod: method})
	if err != nil {
		t.Fatal(err)
	}
	if second.State != CompletionReleased || !second.Ready || !second.Advanced {
		t.Fatalf("both channels did not advance: %+v", second)
	}
	if len(releaser.transactionIDs) != 1 || releaser.transactionIDs[0] != uint(transactionID) {
		t.Fatalf("release calls = %v", releaser.transactionIDs)
	}
	var status string
	if err := db.QueryRowContext(ctx, `SELECT status FROM transactions WHERE id=$1`, transactionID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "DexFiatHasArrived" {
		t.Fatalf("status = %q", status)
	}
	if _, duplicate, err := eventStore.Insert(ctx, email, "parsed"); err != nil || !duplicate {
		t.Fatalf("email deduplication: duplicate=%v err=%v", duplicate, err)
	}
}

type recordingReleaser struct {
	transactionIDs []uint
}

func (r *recordingReleaser) Release(_ context.Context, transactionID uint) error {
	r.transactionIDs = append(r.transactionIDs, transactionID)
	return nil
}
