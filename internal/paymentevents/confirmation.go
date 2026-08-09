package paymentevents

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	ConfirmationReasonSingleChannel   = "single_channel"
	ConfirmationReasonChannelMismatch = "channel_mismatch"
	ConfirmationReasonUserSelection   = "user_selection"

	confirmationDecisionPending  = "pending"
	confirmationDecisionApproved = "approved"
	confirmationDecisionRejected = "rejected"
	confirmationDecisionExpired  = "expired"

	confirmationDeliveryPending = "pending"
	confirmationDeliverySent    = "sent"
	confirmationDeliveryError   = "error"
)

const paymentConfirmationRequestsSchema = `
CREATE TABLE IF NOT EXISTS fiat_payment_confirmation_requests (
    id BIGSERIAL PRIMARY KEY,
    transaction_id BIGINT NOT NULL REFERENCES transactions(id) ON DELETE CASCADE,
    payment_event_id BIGINT NOT NULL REFERENCES fiat_payment_events(id) ON DELETE RESTRICT,
    reason VARCHAR(32) NOT NULL CHECK (reason IN ('single_channel','channel_mismatch','user_selection')),
    comparison JSONB NOT NULL DEFAULT '{}'::jsonb,
    message_text TEXT NOT NULL,
    telegram_chat_id TEXT,
    telegram_message_id BIGINT,
    delivery_status VARCHAR(16) NOT NULL DEFAULT 'pending'
        CHECK (delivery_status IN ('pending','sent','error')),
    decision_status VARCHAR(16) NOT NULL DEFAULT 'pending'
        CHECK (decision_status IN ('pending','approved','rejected','expired')),
    error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    decided_at TIMESTAMPTZ,
    UNIQUE (payment_event_id)
);
CREATE INDEX IF NOT EXISTS fiat_payment_confirmation_requests_pending_idx
    ON fiat_payment_confirmation_requests (decision_status, delivery_status)
    WHERE decision_status = 'pending';
`

// PaymentConfirmationRequest is the durable representation of a Telegram
// ownership question. Event and Candidate are populated when a pending request
// is loaded for a callback or delivery retry.
type PaymentConfirmationRequest struct {
	ID                int64
	TransactionID     uint
	PaymentEventID    int64
	Reason            string
	Comparison        json.RawMessage
	MessageText       string
	TelegramChatID    string
	TelegramMessageID int64
	DeliveryStatus    string
	DecisionStatus    string
	Error             string
	TransactionStatus string
	Event             PaymentEvent
	Candidate         Candidate
}

type paymentFacts struct {
	Source    string    `json:"source"`
	Provider  string    `json:"provider"`
	Sender    string    `json:"sender,omitempty"`
	Amount    int64     `json:"amount_minor"`
	Currency  string    `json:"currency"`
	Reference string    `json:"reference,omitempty"`
	Timestamp time.Time `json:"provider_timestamp,omitempty"`
}

type paymentComparison struct {
	Reason        string        `json:"reason"`
	Mismatches    []string      `json:"mismatches,omitempty"`
	Current       paymentFacts  `json:"current"`
	Other         *paymentFacts `json:"other,omitempty"`
	Transaction   uint          `json:"transaction_id"`
	PaymentMethod string        `json:"payment_method"`
}

var (
	confirmationSpaces = regexp.MustCompile(`\s+`)
	pushReferenceRE    = regexp.MustCompile(`(?i)(?:reference|transaction\s+id|payment\s+reference)\s*[:#]?\s*([[:alnum:]][[:alnum:]./_-]*)`)
)

func manualConfirmationReason(event PaymentEvent, other *PaymentEvent, required []string) (string, []string) {
	if len(required) == 1 {
		return ConfirmationReasonSingleChannel, nil
	}
	if other == nil {
		return "", nil
	}
	mismatches := comparePaymentEvents(event, *other)
	if len(mismatches) > 0 {
		return ConfirmationReasonChannelMismatch, mismatches
	}
	return "", nil
}

func comparePaymentEvents(a, b PaymentEvent) []string {
	var mismatches []string
	if NormalizeProvider(a.Provider) != NormalizeProvider(b.Provider) {
		mismatches = append(mismatches, "provider")
	}
	if a.AccountID != nil && b.AccountID != nil && *a.AccountID != *b.AccountID {
		mismatches = append(mismatches, "account")
	}
	if a.AmountMinor != b.AmountMinor {
		mismatches = append(mismatches, "amount")
	}
	if !strings.EqualFold(strings.TrimSpace(a.Currency), strings.TrimSpace(b.Currency)) {
		mismatches = append(mismatches, "currency")
	}
	if comparableTextDiffers(a.Sender, b.Sender) {
		mismatches = append(mismatches, "sender")
	}
	if comparableTextDiffers(paymentReference(a), paymentReference(b)) {
		mismatches = append(mismatches, "reference")
	}
	if !a.ProviderTimestamp.IsZero() && !b.ProviderTimestamp.IsZero() {
		delta := a.ProviderTimestamp.Sub(b.ProviderTimestamp)
		if delta < 0 {
			delta = -delta
		}
		if delta > 5*time.Minute {
			mismatches = append(mismatches, "timestamp")
		}
	}
	sort.Strings(mismatches)
	return mismatches
}

func comparableTextDiffers(a, b string) bool {
	a, b = normalizeConfirmationText(a), normalizeConfirmationText(b)
	return a != "" && b != "" && a != b
}

func normalizeConfirmationText(value string) string {
	return strings.ToLower(strings.TrimSpace(confirmationSpaces.ReplaceAllString(value, " ")))
}

func paymentReference(event PaymentEvent) string {
	value := strings.TrimSpace(event.Description)
	if value == "" {
		return ""
	}
	if event.Source == SourceEmail {
		return value
	}
	match := pushReferenceRE.FindStringSubmatch(value)
	if len(match) == 2 {
		return strings.TrimSpace(match[1])
	}
	return ""
}

func factsFor(event PaymentEvent) paymentFacts {
	return paymentFacts{
		Source: event.Source, Provider: NormalizeProvider(event.Provider),
		Sender: strings.TrimSpace(event.Sender), Amount: event.AmountMinor,
		Currency:  strings.ToUpper(strings.TrimSpace(event.Currency)),
		Reference: paymentReference(event), Timestamp: event.ProviderTimestamp,
	}
}

func createPaymentConfirmationRequestTx(
	ctx context.Context,
	tx *sql.Tx,
	candidate Candidate,
	event PaymentEvent,
	eventID int64,
	other *PaymentEvent,
	reason string,
	mismatches []string,
) (PaymentConfirmationRequest, error) {
	comparison := paymentComparison{
		Reason: reason, Mismatches: mismatches, Current: factsFor(event),
		Transaction: candidate.ID, PaymentMethod: candidate.PaymentMethod,
	}
	if other != nil {
		facts := factsFor(*other)
		comparison.Other = &facts
	}
	raw, err := json.Marshal(comparison)
	if err != nil {
		return PaymentConfirmationRequest{}, fmt.Errorf("payment events: encode confirmation comparison: %w", err)
	}
	message := paymentConfirmationMessage(candidate, event, other, reason, mismatches)
	var request PaymentConfirmationRequest
	err = tx.QueryRowContext(ctx, `
		INSERT INTO fiat_payment_confirmation_requests
		    (transaction_id, payment_event_id, reason, comparison, message_text)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (payment_event_id) DO UPDATE
		SET updated_at=CURRENT_TIMESTAMP
		RETURNING id, transaction_id, payment_event_id, reason, comparison,
		          message_text, delivery_status, decision_status,
		          COALESCE(telegram_chat_id, ''), COALESCE(telegram_message_id, 0),
		          COALESCE(error, '')
	`, candidate.ID, eventID, reason, raw, message).Scan(
		&request.ID, &request.TransactionID, &request.PaymentEventID,
		&request.Reason, &request.Comparison, &request.MessageText,
		&request.DeliveryStatus, &request.DecisionStatus,
		&request.TelegramChatID, &request.TelegramMessageID, &request.Error,
	)
	if err != nil {
		return PaymentConfirmationRequest{}, fmt.Errorf("payment events: create confirmation request: %w", err)
	}
	request.Event = event
	request.Candidate = candidate
	return request, nil
}

func paymentConfirmationMessage(candidate Candidate, current PaymentEvent, other *PaymentEvent, reason string, mismatches []string) string {
	push, email := "not received", "not received"
	setEvent := func(event PaymentEvent) {
		line := paymentEventLine(event)
		if event.Source == SourcePush {
			push = line
		} else if event.Source == SourceEmail {
			email = line
		}
	}
	setEvent(current)
	if other != nil {
		setEvent(*other)
	}
	reasonText := "Only one notification channel is configured."
	if reason == ConfirmationReasonChannelMismatch {
		reasonText = "Email and push details differ: " + strings.Join(mismatches, ", ") + "."
	} else if reason == ConfirmationReasonUserSelection {
		reasonText = "This transaction was selected from multiple possible matches."
	}
	return fmt.Sprintf(
		"⚠️ Confirm incoming payment\n\n"+
			"Transaction #%d\nDEX: %s\nPayment method: %s\nExpected amount: %.2f %s\n\n"+
			"Push: %s\nEmail: %s\n\n"+
			"Reason: %s\n\nDoes this payment belong to transaction #%d?",
		candidate.ID, displayValue(candidate.Dex), displayValue(candidate.PaymentMethod),
		candidate.FiatAmount, strings.ToUpper(candidate.FiatCurrency),
		push, email, reasonText, candidate.ID,
	)
}

func paymentEventLine(event PaymentEvent) string {
	facts := factsFor(event)
	sender := displayValue(facts.Sender)
	reference := displayValue(facts.Reference)
	timestamp := "not provided"
	if !facts.Timestamp.IsZero() {
		timestamp = facts.Timestamp.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf(
		"%s | sender: %s | reference: %s | time: %s",
		formatMinorAmount(facts.Amount, facts.Currency), sender, reference, timestamp,
	)
}

func formatMinorAmount(amount int64, currency string) string {
	return fmt.Sprintf("%.2f %s", float64(amount)/100, strings.ToUpper(strings.TrimSpace(currency)))
}

func displayValue(value string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return "not provided"
}

func listConfirmationStateTx(ctx context.Context, tx *sql.Tx, txID uint, accountID int64, otherThanSource string) ([]string, *PaymentEvent, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT c.channel, e.id, e.source, e.provider, e.account_id,
		       e.source_event_id, COALESCE(e.sender, ''), COALESCE(e.amount_minor, 0),
		       COALESCE(e.currency::text, ''), COALESCE(e.description, ''),
		       e.provider_timestamp, e.received_timestamp,
		       COALESCE(e.audit_metadata, '{}'::jsonb), COALESCE(e.template_version, ''),
		       COALESCE(e.authentication_outcome, ''), e.raw_message_hash
		FROM fiat_transaction_confirmations c
		JOIN fiat_payment_events e ON e.id=c.payment_event_id
		WHERE c.transaction_id=$1 AND c.account_id=$2
		ORDER BY c.channel
	`, txID, accountID)
	if err != nil {
		return nil, nil, fmt.Errorf("payment events: list transaction confirmations: %w", err)
	}
	defer rows.Close()
	var present []string
	var other *PaymentEvent
	for rows.Next() {
		var event PaymentEvent
		var account sql.NullInt64
		var providerTime sql.NullTime
		var audit []byte
		var channel string
		if err := rows.Scan(
			&channel, &event.ID, &event.Source, &event.Provider, &account,
			&event.SourceEventID, &event.Sender, &event.AmountMinor,
			&event.Currency, &event.Description, &providerTime,
			&event.ReceivedTimestamp, &audit, &event.TemplateVersion,
			&event.AuthenticationOutcome, &event.RawMessageHash,
		); err != nil {
			return nil, nil, err
		}
		if account.Valid {
			value := account.Int64
			event.AccountID = &value
		}
		if providerTime.Valid {
			event.ProviderTimestamp = providerTime.Time
		}
		event.AuditMetadata = audit
		present = append(present, channel)
		if otherThanSource != "" && channel != otherThanSource {
			copy := event
			other = &copy
		}
	}
	return present, other, rows.Err()
}

func markPaymentConfirmationApprovedTx(ctx context.Context, tx *sql.Tx, requestID int64) error {
	res, err := tx.ExecContext(ctx, `
		UPDATE fiat_payment_confirmation_requests
		SET decision_status=$1, decided_at=CURRENT_TIMESTAMP,
		    error=NULL, updated_at=CURRENT_TIMESTAMP
		WHERE id=$2 AND decision_status=$3
	`, confirmationDecisionApproved, requestID, confirmationDecisionPending)
	if err != nil {
		return fmt.Errorf("payment events: approve confirmation request: %w", err)
	}
	count, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("payment events: confirmation request is already handled")
	}
	return nil
}

func (s *EventStore) MarkConfirmationDelivered(ctx context.Context, requestID int64, chatID string, messageID int64) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE fiat_payment_confirmation_requests
		SET delivery_status=$1, telegram_chat_id=$2, telegram_message_id=$3,
		    error=NULL, updated_at=CURRENT_TIMESTAMP
		WHERE id=$4 AND decision_status=$5
	`, confirmationDeliverySent, chatID, messageID, requestID, confirmationDecisionPending)
	if err != nil {
		return fmt.Errorf("payment events: mark confirmation delivered: %w", err)
	}
	return nil
}

func (s *EventStore) MarkConfirmationDeliveryError(ctx context.Context, requestID int64, text string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE fiat_payment_confirmation_requests
		SET delivery_status=$1, error=NULLIF($2,''), updated_at=CURRENT_TIMESTAMP
		WHERE id=$3 AND decision_status=$4
	`, confirmationDeliveryError, text, requestID, confirmationDecisionPending)
	if err != nil {
		return fmt.Errorf("payment events: mark confirmation delivery error: %w", err)
	}
	return nil
}

func (s *EventStore) RejectConfirmation(ctx context.Context, requestID int64) (PaymentConfirmationRequest, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PaymentConfirmationRequest{}, err
	}
	defer tx.Rollback()
	request, err := loadPaymentConfirmationRequest(ctx, tx, requestID, true)
	if err != nil {
		return PaymentConfirmationRequest{}, err
	}
	if request.DecisionStatus != confirmationDecisionPending {
		return request, fmt.Errorf("payment events: confirmation request is already handled")
	}
	var txStatus string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM transactions WHERE id=$1`, request.TransactionID).Scan(&txStatus); err != nil {
		return request, err
	}
	decision := confirmationDecisionRejected
	eventStatus := "user_rejected"
	if txStatus != statusDexPositionTaken {
		decision = confirmationDecisionExpired
		eventStatus = "confirmation_expired"
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE fiat_payment_confirmation_requests
		SET decision_status=$1, decided_at=CURRENT_TIMESTAMP, updated_at=CURRENT_TIMESTAMP
		WHERE id=$2 AND decision_status=$3
	`, decision, requestID, confirmationDecisionPending); err != nil {
		return request, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE fiat_payment_events SET status=$1, updated_at=CURRENT_TIMESTAMP WHERE id=$2
	`, eventStatus, request.PaymentEventID); err != nil {
		return request, err
	}
	request.DecisionStatus = decision
	if err := tx.Commit(); err != nil {
		return request, err
	}
	return request, nil
}

func (s *EventStore) LoadConfirmationRequest(ctx context.Context, requestID int64) (PaymentConfirmationRequest, error) {
	return loadPaymentConfirmationRequest(ctx, s.db, requestID, false)
}

func (s *EventStore) PendingConfirmationRequests(ctx context.Context) ([]PaymentConfirmationRequest, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id
		FROM fiat_payment_confirmation_requests r
		JOIN transactions t ON t.id=r.transaction_id
		WHERE r.decision_status=$1 AND r.delivery_status<>$2 AND t.status=$3
		ORDER BY r.created_at
	`, confirmationDecisionPending, confirmationDeliverySent, statusDexPositionTaken)
	if err != nil {
		return nil, fmt.Errorf("payment events: list pending confirmation requests: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	requests := make([]PaymentConfirmationRequest, 0, len(ids))
	for _, id := range ids {
		request, err := s.LoadConfirmationRequest(ctx, id)
		if err != nil {
			return nil, err
		}
		requests = append(requests, request)
	}
	return requests, nil
}

type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func loadPaymentConfirmationRequest(ctx context.Context, q rowQuerier, requestID int64, forUpdate bool) (PaymentConfirmationRequest, error) {
	suffix := ""
	if forUpdate {
		suffix = " FOR UPDATE OF r"
	}
	var request PaymentConfirmationRequest
	var account sql.NullInt64
	var providerTime sql.NullTime
	var audit []byte
	var counterparty sql.NullString
	var channels string
	err := q.QueryRowContext(ctx, `
		SELECT r.id, r.transaction_id, r.payment_event_id, r.reason, r.comparison,
		       r.message_text, COALESCE(r.telegram_chat_id, ''),
		       COALESCE(r.telegram_message_id, 0), r.delivery_status,
		       r.decision_status, COALESCE(r.error, ''),
		       e.id, e.source, e.provider, e.account_id, e.source_event_id,
		       COALESCE(e.sender, ''), COALESCE(e.amount_minor, 0),
		       COALESCE(e.currency::text, ''), COALESCE(e.description, ''),
		       e.provider_timestamp, e.received_timestamp,
		       COALESCE(e.audit_metadata, '{}'::jsonb),
		       COALESCE(e.template_version, ''), COALESCE(e.authentication_outcome, ''),
		       e.raw_message_hash,
		       t.id, t.fiat_amount, t.fiat_currency, COALESCE(t.dex::text, ''),
		       COALESCE(t.payment_method, ''), a.id, a.provider,
		       COALESCE(array_to_string(a.notification_channels::text[], ','), ''),
		       COALESCE(NULLIF(t.dex_offer_taken, '0001-01-01'::timestamptz), t.created_at),
		       t.counterparty_payment_info, t.status
		FROM fiat_payment_confirmation_requests r
		JOIN fiat_payment_events e ON e.id=r.payment_event_id
		JOIN transactions t ON t.id=r.transaction_id
		JOIN payment_methods pm ON lower(pm.name)=lower(t.payment_method)
		JOIN accounts a ON a.id=pm.account_id
		WHERE r.id=$1`+suffix,
		requestID,
	).Scan(
		&request.ID, &request.TransactionID, &request.PaymentEventID,
		&request.Reason, &request.Comparison, &request.MessageText,
		&request.TelegramChatID, &request.TelegramMessageID,
		&request.DeliveryStatus, &request.DecisionStatus, &request.Error,
		&request.Event.ID, &request.Event.Source, &request.Event.Provider,
		&account, &request.Event.SourceEventID, &request.Event.Sender,
		&request.Event.AmountMinor, &request.Event.Currency,
		&request.Event.Description, &providerTime,
		&request.Event.ReceivedTimestamp, &audit,
		&request.Event.TemplateVersion, &request.Event.AuthenticationOutcome,
		&request.Event.RawMessageHash,
		&request.Candidate.ID, &request.Candidate.FiatAmount,
		&request.Candidate.FiatCurrency, &request.Candidate.Dex,
		&request.Candidate.PaymentMethod, &request.Candidate.AccountID,
		&request.Candidate.AccountProvider, &channels,
		&request.Candidate.PendingSince, &counterparty, &request.TransactionStatus,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return request, fmt.Errorf("payment events: confirmation request not found")
		}
		return request, fmt.Errorf("payment events: load confirmation request: %w", err)
	}
	if account.Valid {
		value := account.Int64
		request.Event.AccountID = &value
	}
	if providerTime.Valid {
		request.Event.ProviderTimestamp = providerTime.Time
	}
	request.Event.AuditMetadata = audit
	request.Candidate.NotificationChannels = SplitChannels(channels)
	if counterparty.Valid {
		request.Candidate.CounterpartyPaymentInfo = json.RawMessage(counterparty.String)
	}
	return request, nil
}
