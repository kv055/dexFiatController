package pushpush

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// CallbackAttempt is a safe operational audit record for a phone callback. It
// deliberately excludes request bodies and credentials.
type CallbackAttempt struct {
	ReceivedAt time.Time `json:"received_at"`
	Endpoint   string    `json:"endpoint"`
	Outcome    string    `json:"outcome"`
	StatusCode int       `json:"status_code"`
	DeviceID   string    `json:"device_id,omitempty"`
	App        string    `json:"app,omitempty"`
	Detail     string    `json:"detail,omitempty"`
}

const phoneCallbackAttemptsSchema = `
CREATE TABLE IF NOT EXISTS fiat_phone_callback_attempts (
    id BIGSERIAL PRIMARY KEY,
    received_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    endpoint VARCHAR(32) NOT NULL,
    outcome VARCHAR(16) NOT NULL,
    status_code SMALLINT NOT NULL,
    device_id VARCHAR(128),
    app VARCHAR(64),
    detail TEXT
);
CREATE INDEX IF NOT EXISTS idx_fiat_phone_callback_attempts_received_at
    ON fiat_phone_callback_attempts (received_at DESC);
`

// CallbackAttemptStore records accepted, rejected, and malformed callback
// attempts so the authenticated dashboard can diagnose phone connectivity.
type CallbackAttemptStore struct {
	db *sql.DB
}

func NewCallbackAttemptStore(db *sql.DB) (*CallbackAttemptStore, error) {
	if db == nil {
		return nil, fmt.Errorf("pushpush: callback attempt store requires a non-nil db")
	}
	if _, err := db.Exec(phoneCallbackAttemptsSchema); err != nil {
		var exists bool
		checkErr := db.QueryRow(`SELECT to_regclass('public.fiat_phone_callback_attempts') IS NOT NULL`).Scan(&exists)
		if checkErr != nil || !exists {
			return nil, fmt.Errorf("pushpush: ensure callback attempt schema: %w", err)
		}
	}
	return &CallbackAttemptStore{db: db}, nil
}

func (s *CallbackAttemptStore) Record(ctx context.Context, a CallbackAttempt) error {
	_, err := s.db.ExecContext(ctx, `
        INSERT INTO fiat_phone_callback_attempts
            (endpoint, outcome, status_code, device_id, app, detail)
        VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, ''), NULLIF($6, ''))
    `, a.Endpoint, a.Outcome, a.StatusCode, a.DeviceID, a.App, a.Detail)
	if err != nil {
		return fmt.Errorf("pushpush: record callback attempt: %w", err)
	}
	return nil
}

func (s *CallbackAttemptStore) List(ctx context.Context, limit int) ([]CallbackAttempt, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
        SELECT received_at, endpoint, outcome, status_code,
               COALESCE(device_id, ''), COALESCE(app, ''), COALESCE(detail, '')
        FROM fiat_phone_callback_attempts
        ORDER BY received_at DESC
        LIMIT $1
    `, limit)
	if err != nil {
		return nil, fmt.Errorf("pushpush: list callback attempts: %w", err)
	}
	defer rows.Close()

	attempts := make([]CallbackAttempt, 0, limit)
	for rows.Next() {
		var a CallbackAttempt
		if err := rows.Scan(&a.ReceivedAt, &a.Endpoint, &a.Outcome, &a.StatusCode, &a.DeviceID, &a.App, &a.Detail); err != nil {
			return nil, fmt.Errorf("pushpush: scan callback attempt: %w", err)
		}
		attempts = append(attempts, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pushpush: iterate callback attempts: %w", err)
	}
	return attempts, nil
}
