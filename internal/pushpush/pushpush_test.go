package pushpush

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseAmount(t *testing.T) {
	cases := []struct {
		text     string
		amount   float64
		currency string
	}{
		{"You received €1,234.56 from Jane Doe", 1234.56, "EUR"},
		{"$10.00 payment", 10.00, "USD"},
		{"100.00 GBP received", 100.00, "GBP"},
		{"no money here", 0, ""},
	}
	for _, c := range cases {
		a, cur := parseAmount(c.text)
		if abs(a-c.amount) > 0.0001 {
			t.Errorf("parseAmount(%q) amount = %v, want %v", c.text, a, c.amount)
		}
		if cur != c.currency {
			t.Errorf("parseAmount(%q) currency = %q, want %q", c.text, cur, c.currency)
		}
	}
}

func TestNormalizeStripsAmountAndNames(t *testing.T) {
	got := normalize("You received €1,234.56 from Jane Doe - rent")
	if want := "you received from - rent"; got != want {
		t.Errorf("normalize = %q, want %q", got, want)
	}
}

func TestPaymentSignalProducesNeutralSourceOutput(t *testing.T) {
	now := time.Now().UTC()
	notification := Notification{
		App: "Wise", Title: "You received money",
		Text: "You received €10.00 from Jane Doe", Time: now.Format(time.RFC3339),
	}
	signal := PaymentSignal(notification, []byte(`{"app":"Wise"}`), "push-event-1", now, 12)
	if signal.Event.Source != "push" || signal.Event.Provider != "WISE" {
		t.Fatalf("event = %+v", signal.Event)
	}
	if signal.Event.AmountMinor != 1000 || signal.Event.Currency != "EUR" {
		t.Fatalf("amount/currency = %d/%s", signal.Event.AmountMinor, signal.Event.Currency)
	}
	if signal.Presentation.App != "Wise" || signal.Presentation.Normalized == "" || signal.AuditID != 12 {
		t.Fatalf("signal = %+v", signal)
	}
}

func TestRuleStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rules.json")

	rs := NewRuleStore(path)
	rs.Add("BankApp", "incoming wire from", true)
	rs.Add("BankApp", "marketing newsletter", false)

	allowed, denied := rs.Match("bankapp", "incoming wire from jane doe")
	if !allowed || denied {
		t.Errorf("expected allowed match, got allowed=%v denied=%v", allowed, denied)
	}
	allowed, denied = rs.Match("bankapp", "marketing newsletter q3")
	if allowed || !denied {
		t.Errorf("expected denied match, got allowed=%v denied=%v", allowed, denied)
	}
	allowed, denied = rs.Match("bankapp", "unrelated text")
	if allowed || denied {
		t.Errorf("expected no match, got allowed=%v denied=%v", allowed, denied)
	}

	// Reload from disk to verify persistence.
	rs2 := NewRuleStore(path)
	allowed, _ = rs2.Match("bankapp", "incoming wire from x")
	if !allowed {
		t.Errorf("expected persisted allow rule to match after reload, got allowed=%v", allowed)
	}
}

func TestRuleStoreDenyPrecedence(t *testing.T) {
	rs := NewRuleStore("")
	rs.Add("app", "payment", true)
	rs.Add("app", "payment spam", false)
	allowed, denied := rs.Match("app", "payment spam q3")
	if allowed || !denied {
		t.Fatalf("deny must take precedence, got allowed=%v denied=%v", allowed, denied)
	}
}

// TestLogStoreInsertUpdate exercises the fiat_tx_push_logs round-trip against a real
// Postgres. Skipped unless PUSHPUSH_TEST_DB is set.
func TestLogStoreInsertUpdate(t *testing.T) {
	dsn := os.Getenv("PUSHPUSH_TEST_DB")
	if dsn == "" {
		t.Skip("set PUSHPUSH_TEST_DB to run the Postgres integration test")
	}
	store, err := OpenTxStore(dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	logs, err := NewLogStore(store.DB())
	if err != nil {
		t.Fatalf("new log store: %v", err)
	}
	ctx := context.Background()

	n := Notification{App: "bankapp", Title: "Received", Text: "€100.00 from Jane Doe"}
	id, err := logs.Insert(ctx, n, json.RawMessage(`{"app":"bankapp"}`), "received from")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	defer func() { _, _ = store.DB().ExecContext(ctx, `DELETE FROM fiat_tx_push_logs WHERE id = $1`, id) }()

	// Verify initial row.
	var status, ruleDecision sql.NullString
	var matched sql.NullInt64
	if err = store.DB().QueryRowContext(ctx,
		`SELECT status, rule_decision, matched_tx_id FROM fiat_tx_push_logs WHERE id = $1`, id,
	).Scan(&status, &ruleDecision, &matched); err != nil {
		t.Fatalf("query inserted row: %v", err)
	}
	if status.String != string(LogReceived) {
		t.Errorf("initial status = %q, want %q", status.String, LogReceived)
	}
	if matched.Valid {
		t.Errorf("expected no matched_tx_id initially, got %d", matched.Int64)
	}

	// Advance through stages: matched → released.
	txID := uint(42)
	if err := logs.Update(ctx, id, LogUpdate{Status: LogPicked, MatchedTxID: &txID}); err != nil {
		t.Fatalf("update picked: %v", err)
	}
	if err := logs.Update(ctx, id, LogUpdate{Status: LogReleased}); err != nil {
		t.Fatalf("update released: %v", err)
	}

	if err = store.DB().QueryRowContext(ctx,
		`SELECT status, rule_decision, matched_tx_id, error FROM fiat_tx_push_logs WHERE id = $1`, id,
	).Scan(&status, &ruleDecision, &matched, new(sql.NullString)); err != nil {
		t.Fatalf("query advanced row: %v", err)
	}
	if status.String != string(LogReleased) {
		t.Errorf("advanced status = %q, want %q", status.String, LogReleased)
	}
	if !matched.Valid || matched.Int64 != 42 {
		t.Errorf("expected matched_tx_id carried forward as 42, got %v", matched)
	}

	// Error field overwrites and clears.
	if err := logs.Update(ctx, id, LogUpdate{Status: LogReleaseError, Error: "boom"}); err != nil {
		t.Fatalf("update error: %v", err)
	}
	var errMsg sql.NullString
	if err = store.DB().QueryRowContext(ctx,
		`SELECT error FROM fiat_tx_push_logs WHERE id = $1`, id,
	).Scan(&errMsg); err != nil {
		t.Fatalf("query error: %v", err)
	}
	if !errMsg.Valid || errMsg.String != "boom" {
		t.Errorf("expected error 'boom', got %v", errMsg)
	}
	if err := logs.Update(ctx, id, LogUpdate{Status: LogReleased, Error: ""}); err != nil {
		t.Fatalf("clear error: %v", err)
	}
	if err = store.DB().QueryRowContext(ctx,
		`SELECT error FROM fiat_tx_push_logs WHERE id = $1`, id,
	).Scan(&errMsg); err != nil {
		t.Fatalf("query cleared error: %v", err)
	}
	if errMsg.Valid {
		t.Errorf("expected error cleared, got %q", errMsg.String)
	}
}

// TestExtractJSONObject covers forwarders that wrap the JSON payload in stray
// whitespace or prefix text (observed MacroDroid bodies).
func TestExtractJSONObject(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
		ok    bool
	}{
		{
			name:  "clean json",
			input: `{"app":"Revolut","title":"You received €1","text":"Payment received from Kilian Reimar Joergen Voss. Tap for details","time":"1783110191"}`,
			want:  `{"app":"Revolut","title":"You received €1","text":"Payment received from Kilian Reimar Joergen Voss. Tap for details","time":"1783110191"}`,
			ok:    true,
		},
		{
			name:  "leading newlines",
			input: "\n\n" + `{"app":"Revolut","title":"You received €1","text":"x","time":"1783110191"}`,
			want:  `{"app":"Revolut","title":"You received €1","text":"x","time":"1783110191"}`,
			ok:    true,
		},
		{
			name:  "prefix junk before json",
			input: "Kilian Voss has received your bank transfer of €1\n\n" + `{"app":"N26","title":"N26","text":"Kilian Voss has received your bank transfer of €1","time":"1783110200"}`,
			want:  `{"app":"N26","title":"N26","text":"Kilian Voss has received your bank transfer of €1","time":"1783110200"}`,
			ok:    true,
		},
		{
			name:  "braces inside string literals",
			input: `{"app":"N26","title":"N26","text":"You received €2 {promo} from Kilian Voss.","time":"1783110450"}`,
			want:  `{"app":"N26","title":"N26","text":"You received €2 {promo} from Kilian Voss.","time":"1783110450"}`,
			ok:    true,
		},
		{
			name:  "invalid json typo falls back",
			input: `{"app":"Revolut","title":"Transfer sent ✅","text":"You sent €2 to Kilian Reimar Joergen Voss.","date":"time":"1783110442"}`,
			ok:    false,
		},
		{
			name:  "no object at all",
			input: "just plain text no json here",
			ok:    false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := extractJSONObject([]byte(c.input))
			if ok != c.ok {
				t.Fatalf("ok: got %v want %v (got=%q)", ok, c.ok, got)
			}
			if ok && string(got) != c.want {
				t.Errorf("extracted: got %q want %q", got, c.want)
			}
		})
	}
}

// TestCapturedShapesParse verifies the two real captured notification shapes
// (Revolut and N26) parse into Notification and yield the expected amount,
// currency, and sender via the matcher's free-text extraction.
func TestCapturedShapesParse(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		app      string
		amount   float64
		currency string
		sender   string
	}{
		{
			name:     "revolut received",
			body:     `{"app":"Revolut","title":"You received €1","text":"Payment received from Kilian Reimar Joergen Voss. Tap for details","time":"1783110191"}`,
			app:      "Revolut",
			amount:   1,
			currency: "EUR",
			sender:   "Kilian Reimar Joergen Voss",
		},
		{
			name:     "n26 received with sender",
			body:     `{"app":"N26","title":"N26","text":"You received €2 from Kilian Voss.","time":"1783110450"}`,
			app:      "N26",
			amount:   2,
			currency: "EUR",
			sender:   "Kilian Voss",
		},
		{
			name:     "n26 received without sender in text",
			body:     `{"app":"N26","title":"N26","text":"Kilian Voss has received your bank transfer of €1","time":"1783110200"}`,
			app:      "N26",
			amount:   1,
			currency: "EUR",
			// This N26 notification type names the receiver (the user), not the
			// sender; the regex still extracts the name, but it is not a real
			// counterparty sender — matching relies on amount+currency instead.
			sender: "Kilian Voss",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			obj, ok := extractJSONObject([]byte(c.body))
			if !ok {
				t.Fatal("expected extractJSONObject to find the JSON object")
			}
			var n Notification
			if err := json.Unmarshal(obj, &n); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if n.App != c.app {
				t.Errorf("app: got %q want %q", n.App, c.app)
			}
			amt, cur := n.amountAndCurrency()
			if amt != c.amount || cur != c.currency {
				t.Errorf("amount/currency: got (%v,%q) want (%v,%q)", amt, cur, c.amount, c.currency)
			}
			if got := n.senderName(); got != c.sender {
				t.Errorf("sender: got %q want %q", got, c.sender)
			}
		})
	}
}

// forwarders (e.g. MacroDroid) that POST a raw plain-text body with no
// Content-Type. The whole body becomes the notification text and amount /
// sender must be mined from it via parseAmount and parseSenderName.
//
// Plain text is best-effort: when digits are glued to a name (e.g. "€1Kilian")
// the word-boundary name regex cannot recover the sender, so such notifications
// fall back to amount+currency matching and escalate to Telegram when
// ambiguous. Structured JSON avoids this entirely.
func TestPlainTextNotificationParsing(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		amount      float64
		currency    string
		sender      string
		senderExact bool // false when the sender is glued to digits and may not extract
	}{
		{
			name:        "revolut style",
			body:        "You received €1Payment received from Kilian Reimar Joergen Voss. Tap for details",
			amount:      1,
			currency:    "EUR",
			sender:      "Kilian Reimar Joergen Voss",
			senderExact: true,
		},
		{
			name:        "n26 style names receiver not sender",
			body:        "N26Kilian Voss has received your bank transfer of €1Kilian Voss has received your bank transfer of €1",
			amount:      1,
			currency:    "EUR",
			sender:      "",
			senderExact: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var n Notification
			if json.Valid([]byte(c.body)) {
				t.Fatal("plain-text body should not parse as JSON")
			}
			n = Notification{Text: c.body, App: appFromUserAgent("macrodroid/5.64.11")}

			amt, cur := parseAmount(n.Title + " " + n.Text)
			if amt != c.amount {
				t.Errorf("amount: got %v want %v", amt, c.amount)
			}
			if cur != c.currency {
				t.Errorf("currency: got %q want %q", cur, c.currency)
			}
			got := parseSenderName(n.Title + " " + n.Text)
			if c.senderExact {
				if got != c.sender {
					t.Errorf("sender: got %q want %q", got, c.sender)
				}
			} else if got != "" {
				t.Errorf("sender: expected empty (glued to digits, will escalate), got %q", got)
			}
			if n.App != "macrodroid" {
				t.Errorf("app: got %q want %q", n.App, "macrodroid")
			}
		})
	}
}
