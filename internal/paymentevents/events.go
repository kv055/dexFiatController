package paymentevents

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	SourcePush  = "push"
	SourceEmail = "email"

	statusDexPositionTaken  = "DexPositionTaken"
	statusDexFiatHasArrived = "DexFiatHasArrived"
)

// PaymentEvent is the source-neutral payment signal consumed by the matching
// and channel-confirmation pipeline. AmountMinor deliberately avoids floating
// point comparisons at the ingestion boundary.
type PaymentEvent struct {
	ID                    int64           `json:"id,omitempty"`
	Source                string          `json:"source"`
	Provider              string          `json:"provider"`
	AccountID             *int64          `json:"account_id,omitempty"`
	SourceEventID         string          `json:"source_event_id"`
	Sender                string          `json:"sender,omitempty"`
	AmountMinor           int64           `json:"amount_minor,omitempty"`
	Currency              string          `json:"currency,omitempty"`
	Description           string          `json:"description,omitempty"`
	ProviderTimestamp     time.Time       `json:"provider_timestamp,omitempty"`
	ReceivedTimestamp     time.Time       `json:"received_timestamp"`
	AuditMetadata         json.RawMessage `json:"audit_metadata,omitempty"`
	TemplateVersion       string          `json:"template_version,omitempty"`
	AuthenticationOutcome string          `json:"authentication_outcome,omitempty"`
	RawMessageHash        string          `json:"raw_message_hash,omitempty"`
}

func NormalizeProvider(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "wise", "transferwise":
		return "WISE"
	case "paypal":
		return "PayPal"
	case "revolut":
		return "Revolut"
	case "n26":
		return "N26"
	default:
		return strings.TrimSpace(value)
	}
}

func EventIDFromBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

const fiatPaymentEventsSchema = `
CREATE TABLE IF NOT EXISTS fiat_payment_events (
    id BIGSERIAL PRIMARY KEY,
    source notification_channel NOT NULL,
    provider TEXT NOT NULL,
    account_id BIGINT REFERENCES accounts(id) ON DELETE SET NULL,
    source_event_id TEXT NOT NULL,
    sender TEXT,
    amount_minor BIGINT,
    currency fiat_currency,
    description TEXT,
    provider_timestamp TIMESTAMPTZ,
    received_timestamp TIMESTAMPTZ NOT NULL,
    audit_metadata JSONB,
    template_version TEXT,
    authentication_outcome TEXT,
    status VARCHAR(40) NOT NULL,
    matched_transaction_id BIGINT REFERENCES transactions(id) ON DELETE SET NULL,
    raw_message_hash CHAR(64) NOT NULL,
    error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (source, source_event_id)
);
CREATE INDEX IF NOT EXISTS fiat_payment_events_received_idx
    ON fiat_payment_events (received_timestamp DESC);
CREATE INDEX IF NOT EXISTS fiat_payment_events_match_idx
    ON fiat_payment_events (matched_transaction_id) WHERE matched_transaction_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS fiat_transaction_confirmations (
    transaction_id BIGINT NOT NULL REFERENCES transactions(id) ON DELETE CASCADE,
    account_id BIGINT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    channel notification_channel NOT NULL,
    payment_event_id BIGINT NOT NULL REFERENCES fiat_payment_events(id) ON DELETE RESTRICT,
    confirmed_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (transaction_id, channel),
    UNIQUE (payment_event_id)
);
`

// EventStore persists normalized audit events and atomically correlates the
// required account channels with a transaction.
type EventStore struct {
	db *sql.DB
}

func NewEventStore(db *sql.DB) (*EventStore, error) {
	if db == nil {
		return nil, fmt.Errorf("payment events: event store requires a non-nil db")
	}
	if _, err := db.Exec(fiatPaymentEventsSchema + paymentConfirmationRequestsSchema); err != nil {
		var exists bool
		checkErr := db.QueryRow(`SELECT to_regclass('public.fiat_payment_events') IS NOT NULL
			AND to_regclass('public.fiat_transaction_confirmations') IS NOT NULL
			AND to_regclass('public.fiat_payment_confirmation_requests') IS NOT NULL`).Scan(&exists)
		if checkErr != nil || !exists {
			return nil, fmt.Errorf("payment events: ensure payment event schema: %w", err)
		}
	}
	return &EventStore{db: db}, nil
}

// Insert creates an immutable event identity. A repeated source event returns
// the original row and duplicate=true, without running matching again.
func (s *EventStore) Insert(ctx context.Context, event PaymentEvent, status string) (id int64, duplicate bool, err error) {
	event.Source = strings.ToLower(strings.TrimSpace(event.Source))
	event.Provider = NormalizeProvider(event.Provider)
	event.Currency = strings.ToUpper(strings.TrimSpace(event.Currency))
	if event.ReceivedTimestamp.IsZero() {
		event.ReceivedTimestamp = time.Now().UTC()
	}
	if event.SourceEventID == "" || event.RawMessageHash == "" {
		return 0, false, fmt.Errorf("payment events: source event id and raw message hash are required")
	}
	var account any
	if event.AccountID != nil {
		account = *event.AccountID
	}
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO fiat_payment_events (
			source, provider, account_id, source_event_id, sender, amount_minor,
			currency, description, provider_timestamp, received_timestamp,
			audit_metadata, template_version, authentication_outcome, status,
			raw_message_hash
		)
		VALUES ($1,$2,$3,$4,NULLIF($5,''),NULLIF($6,0),NULLIF($7,''),
		        NULLIF($8,''),$9,$10,$11,
		        NULLIF($12,''),NULLIF($13,''),$14,$15)
		ON CONFLICT (source, source_event_id) DO NOTHING
		RETURNING id
	`, event.Source, event.Provider, account, event.SourceEventID, event.Sender,
		event.AmountMinor, event.Currency, event.Description, nullableTime(event.ProviderTimestamp),
		event.ReceivedTimestamp, nullableJSON(event.AuditMetadata), event.TemplateVersion,
		event.AuthenticationOutcome, status, event.RawMessageHash).Scan(&id)
	if err == nil {
		return id, false, nil
	}
	if err != sql.ErrNoRows {
		return 0, false, fmt.Errorf("payment events: insert payment event: %w", err)
	}
	err = s.db.QueryRowContext(ctx, `
		SELECT id FROM fiat_payment_events WHERE source=$1 AND source_event_id=$2
	`, event.Source, event.SourceEventID).Scan(&id)
	if err != nil {
		return 0, false, fmt.Errorf("payment events: read duplicate payment event: %w", err)
	}
	return id, true, nil
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UTC()
}

func nullableJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return []byte(raw)
}

func (s *EventStore) UpdateStatus(ctx context.Context, id int64, status string, txID *uint, errText string) error {
	var matched any
	if txID != nil {
		matched = int64(*txID)
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE fiat_payment_events
		SET status=$1,
		    matched_transaction_id=COALESCE($2, matched_transaction_id),
		    error=NULLIF($3,''),
		    updated_at=CURRENT_TIMESTAMP
		WHERE id=$4
	`, status, matched, errText, id)
	if err != nil {
		return fmt.Errorf("payment events: update payment event %d: %w", id, err)
	}
	return nil
}

// ConfirmationResult describes the channel gate after recording one event.
type ConfirmationResult struct {
	Required       []string
	Present        []string
	Ready          bool
	Advanced       bool
	ManualRequired bool
	Request        *PaymentConfirmationRequest
}

// Confirm atomically locks the transaction, validates that the event belongs
// to the account linked by its canonical payment method, records the channel,
// and advances only when every currently selected channel is present.
func (s *EventStore) Confirm(ctx context.Context, event PaymentEvent, eventID int64, txID uint, evidence json.RawMessage, approvalRequestID int64, forceManual bool) (ConfirmationResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ConfirmationResult{}, fmt.Errorf("payment events: begin confirmation: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(txID)); err != nil {
		return ConfirmationResult{}, fmt.Errorf("payment events: lock transaction %d: %w", txID, err)
	}

	var accountID int64
	var provider, channels, status, dex, paymentMethod string
	var fiatAmount float64
	var fiatCurrency string
	var counterparty sql.NullString
	var pendingSince time.Time
	err = tx.QueryRowContext(ctx, `
		SELECT a.id, a.provider,
		       COALESCE(array_to_string(a.notification_channels::text[], ','), ''),
		       t.status, t.fiat_amount, t.fiat_currency, COALESCE(t.dex::text, ''),
		       COALESCE(t.payment_method, ''), t.counterparty_payment_info,
		       COALESCE(NULLIF(t.dex_offer_taken, '0001-01-01'::timestamptz), t.created_at)
		FROM transactions t
		JOIN payment_methods pm ON lower(pm.name)=lower(t.payment_method)
		JOIN accounts a ON a.id=pm.account_id AND a.is_active
		WHERE t.id=$1 AND t.deleted_at IS NULL AND pm.is_active
	`, txID).Scan(
		&accountID, &provider, &channels, &status, &fiatAmount, &fiatCurrency,
		&dex, &paymentMethod, &counterparty, &pendingSince,
	)
	if err != nil {
		return ConfirmationResult{}, fmt.Errorf("payment events: resolve transaction payment account: %w", err)
	}
	if event.AccountID != nil && *event.AccountID != accountID {
		return ConfirmationResult{}, fmt.Errorf("payment events: event account %d does not match transaction account %d", *event.AccountID, accountID)
	}
	if !strings.EqualFold(NormalizeProvider(event.Provider), NormalizeProvider(provider)) {
		return ConfirmationResult{}, fmt.Errorf("payment events: event provider %q does not match transaction provider %q", event.Provider, provider)
	}
	required := SplitChannels(channels)
	if len(required) == 0 {
		return ConfirmationResult{}, fmt.Errorf("payment events: payment account has no selected notification channel")
	}
	if !ContainsChannel(required, event.Source) {
		return ConfirmationResult{}, fmt.Errorf("payment events: %s is not selected on payment account %d", event.Source, accountID)
	}
	if status != statusDexPositionTaken {
		return ConfirmationResult{Required: required}, fmt.Errorf("payment events: transaction %d is not pending fiat (status %s)", txID, status)
	}
	candidate := Candidate{
		ID: txID, FiatAmount: fiatAmount, FiatCurrency: fiatCurrency,
		Dex: dex, PaymentMethod: paymentMethod, AccountID: accountID,
		AccountProvider: provider, NotificationChannels: required,
		PendingSince: pendingSince,
	}
	if counterparty.Valid {
		candidate.CounterpartyPaymentInfo = json.RawMessage(counterparty.String)
	}

	if approvalRequestID > 0 {
		var requestEventID, requestTxID int64
		var requestStatus string
		err := tx.QueryRowContext(ctx, `
			SELECT payment_event_id, transaction_id, decision_status
			FROM fiat_payment_confirmation_requests
			WHERE id=$1
			FOR UPDATE
		`, approvalRequestID).Scan(&requestEventID, &requestTxID, &requestStatus)
		if err != nil {
			return ConfirmationResult{}, fmt.Errorf("payment events: load confirmation request: %w", err)
		}
		if requestStatus != confirmationDecisionPending ||
			requestEventID != eventID || requestTxID != int64(txID) {
			return ConfirmationResult{}, fmt.Errorf("payment events: confirmation request is already handled or does not match")
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE fiat_payment_events
		SET account_id=$1, matched_transaction_id=$2, updated_at=CURRENT_TIMESTAMP
		WHERE id=$3
	`, accountID, txID, eventID); err != nil {
		return ConfirmationResult{}, fmt.Errorf("payment events: bind payment event to account: %w", err)
	}

	present, otherEvent, err := listConfirmationStateTx(ctx, tx, txID, accountID, event.Source)
	if err != nil {
		return ConfirmationResult{}, err
	}
	if approvalRequestID == 0 {
		reason, mismatches := manualConfirmationReason(event, otherEvent, required)
		if forceManual {
			reason = ConfirmationReasonUserSelection
		}
		if reason != "" {
			request, requestErr := createPaymentConfirmationRequestTx(
				ctx, tx, candidate, event, eventID, otherEvent, reason, mismatches,
			)
			if requestErr != nil {
				return ConfirmationResult{}, requestErr
			}
			if err := tx.Commit(); err != nil {
				return ConfirmationResult{}, err
			}
			return ConfirmationResult{
				Required: required, Present: present,
				ManualRequired: true, Request: &request,
			}, nil
		}
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO fiat_transaction_confirmations
		    (transaction_id, account_id, channel, payment_event_id)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (transaction_id, channel) DO NOTHING
	`, txID, accountID, event.Source, eventID); err != nil {
		return ConfirmationResult{}, fmt.Errorf("payment events: record %s confirmation: %w", event.Source, err)
	}
	present, _, err = listConfirmationStateTx(ctx, tx, txID, accountID, "")
	if err != nil {
		return ConfirmationResult{}, err
	}

	result := ConfirmationResult{Required: required, Present: present}
	if !AllChannelsPresent(required, present) {
		if approvalRequestID > 0 {
			if err := markPaymentConfirmationApprovedTx(ctx, tx, approvalRequestID); err != nil {
				return ConfirmationResult{}, err
			}
		}
		if err := tx.Commit(); err != nil {
			return ConfirmationResult{}, err
		}
		return result, nil
	}
	result.Ready = true
	if len(evidence) == 0 {
		evidence = json.RawMessage(`{}`)
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE transactions
		SET status=$1, dex_fiat_arrival=$2, updated_at=CURRENT_TIMESTAMP
		WHERE id=$3 AND status=$4 AND deleted_at IS NULL
	`, statusDexFiatHasArrived, []byte(evidence), txID, statusDexPositionTaken)
	if err != nil {
		return ConfirmationResult{}, fmt.Errorf("payment events: advance confirmed transaction: %w", err)
	}
	count, err := res.RowsAffected()
	if err != nil {
		return ConfirmationResult{}, err
	}
	result.Advanced = count == 1
	if approvalRequestID > 0 {
		if err := markPaymentConfirmationApprovedTx(ctx, tx, approvalRequestID); err != nil {
			return ConfirmationResult{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return ConfirmationResult{}, err
	}
	return result, nil
}

func SplitChannels(value string) []string {
	var result []string
	for _, part := range strings.Split(value, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		if (part == SourcePush || part == SourceEmail) && !ContainsChannel(result, part) {
			result = append(result, part)
		}
	}
	sort.Strings(result)
	return result
}

func ContainsChannel(channels []string, wanted string) bool {
	for _, channel := range channels {
		if channel == strings.ToLower(strings.TrimSpace(wanted)) {
			return true
		}
	}
	return false
}

func AllChannelsPresent(required, present []string) bool {
	for _, channel := range required {
		if !ContainsChannel(present, channel) {
			return false
		}
	}
	return len(required) > 0
}
