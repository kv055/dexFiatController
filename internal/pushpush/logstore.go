package pushpush

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// LogStatus is the pipeline stage recorded on a fiat_tx_push_logs row.
type LogStatus string

const (
	LogReceived                 LogStatus = "received"
	LogDenied                   LogStatus = "denied"
	LogUnmatched                LogStatus = "unmatched"
	LogEscalatedClassify        LogStatus = "escalated_classify"
	LogEscalatedPick            LogStatus = "escalated_pick"
	LogUnclassifiedNoEscalation LogStatus = "unclassified_no_escalation"
	LogAmbiguousNoEscalation    LogStatus = "ambiguous_no_escalation"
	LogReleased                 LogStatus = "released"
	LogAlreadyReleased          LogStatus = "already_released"
	LogReleaseError             LogStatus = "release_error"
	LogMarkError                LogStatus = "mark_error"
	LogListError                LogStatus = "list_error"
	LogTelegramError            LogStatus = "telegram_error"
	LogUserAllowed              LogStatus = "user_allowed"
	LogUserDenied               LogStatus = "user_denied"
	LogPicked                   LogStatus = "picked"
)

// fiatTxPushLogsSchema mirrors dexUnifiedApi/db/schema/19_fiat_tx_push_logs.sql so the
// pushpush service can create the table itself if it boots before dexUnifiedApi applies
// the shared schema. Keep in sync with that file.
const fiatTxPushLogsSchema = `
CREATE TABLE IF NOT EXISTS fiat_tx_push_logs (
    id BIGSERIAL PRIMARY KEY,
    received_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    app VARCHAR(64),
    title TEXT,
    text TEXT,
    raw JSONB,
    normalized TEXT,
    amount DOUBLE PRECISION,
    currency CHAR(3),
    sender TEXT,
    status VARCHAR(40) NOT NULL,
    matched_tx_id BIGINT,
    rule_decision VARCHAR(16),
    telegram_pending_id VARCHAR(32),
    error TEXT,
    CONSTRAINT fiat_tx_push_logs_status_check CHECK (
        status IN (
            'received','denied','unmatched','escalated_classify','escalated_pick',
            'unclassified_no_escalation','ambiguous_no_escalation',
            'released','already_released','release_error','mark_error','list_error',
            'telegram_error','user_allowed','user_denied','picked'
        )
    )
);
CREATE INDEX IF NOT EXISTS idx_fiat_tx_push_logs_received_at
    ON fiat_tx_push_logs (received_at DESC);
CREATE INDEX IF NOT EXISTS idx_fiat_tx_push_logs_status
    ON fiat_tx_push_logs (status);
CREATE INDEX IF NOT EXISTS idx_fiat_tx_push_logs_app
    ON fiat_tx_push_logs (app);
CREATE INDEX IF NOT EXISTS idx_fiat_tx_push_logs_matched_tx
    ON fiat_tx_push_logs (matched_tx_id) WHERE matched_tx_id IS NOT NULL;
`

// LogStore records inbound notifications and their pipeline progress in the
// fiat_tx_push_logs table. It shares the TxStore's *sql.DB.
type LogStore struct {
	db *sql.DB
}

// NewLogStore opens the log store, ensuring the table exists (idempotent).
func NewLogStore(db *sql.DB) (*LogStore, error) {
	if db == nil {
		return nil, fmt.Errorf("pushpush: log store requires a non-nil db")
	}
	if _, err := db.Exec(fiatTxPushLogsSchema); err != nil {
		// In production the restricted fiat-controller role has DML-only access;
		// dexUnifiedApi owns schema creation. Retain the self-bootstrap path for
		// local/dev databases, but accept a pre-existing table when DDL is denied.
		var exists bool
		checkErr := db.QueryRow(`SELECT to_regclass('public.fiat_tx_push_logs') IS NOT NULL`).Scan(&exists)
		if checkErr != nil || !exists {
			return nil, fmt.Errorf("pushpush: ensure fiat_tx_push_logs schema: %w", err)
		}
	}
	return &LogStore{db: db}, nil
}

// LogInsert creates a row for an inbound notification at status "received" and returns
// its id. raw is the verbatim request body; normalized is the normalized title+text.
func (l *LogStore) Insert(ctx context.Context, n Notification, raw json.RawMessage, normalized string) (int64, error) {
	amount, currency := n.amountAndCurrency()
	sender := n.senderName()

	var rawArg any
	if len(raw) > 0 {
		rawArg = []byte(raw)
	}
	amountArg := sql.NullFloat64{Float64: amount, Valid: amount > 0}
	currencyArg := sql.NullString{String: currency, Valid: currency != ""}
	senderArg := sql.NullString{String: sender, Valid: sender != ""}

	var id int64
	err := l.db.QueryRowContext(ctx, `
		INSERT INTO fiat_tx_push_logs (app, title, text, raw, normalized, amount, currency, sender, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id
	`, n.App, n.Title, n.Text, rawArg, normalized, amountArg, currencyArg, senderArg, string(LogReceived)).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("pushpush: insert push log: %w", err)
	}
	return id, nil
}

// LogUpdate carries the fields that can change as a notification moves through the
// pipeline. Carry-forward fields (MatchedTxID, RuleDecision, TelegramPendingID) are only
// set when non-nil/non-empty; Error overwrites (and clears when empty).
type LogUpdate struct {
	Status            LogStatus
	MatchedTxID       *uint
	RuleDecision      string
	TelegramPendingID string
	Error             string
}

// Update advances a log row to a new stage. Carry-forward fields preserve their previous
// value when omitted; Error is overwritten (empty clears it).
func (l *LogStore) Update(ctx context.Context, id int64, u LogUpdate) error {
	var matched sql.NullInt64
	if u.MatchedTxID != nil {
		matched = sql.NullInt64{Int64: int64(*u.MatchedTxID), Valid: true}
	}
	_, err := l.db.ExecContext(ctx, `
		UPDATE fiat_tx_push_logs
		SET status = $1,
		    matched_tx_id = COALESCE($2, matched_tx_id),
		    rule_decision = COALESCE(NULLIF($3, ''), rule_decision),
		    telegram_pending_id = COALESCE(NULLIF($4, ''), telegram_pending_id),
		    error = NULLIF($5, ''),
		    updated_at = CURRENT_TIMESTAMP
		WHERE id = $6
	`, string(u.Status), matched, u.RuleDecision, u.TelegramPendingID, u.Error, id)
	if err != nil {
		return fmt.Errorf("pushpush: update push log %d: %w", id, err)
	}
	return nil
}
