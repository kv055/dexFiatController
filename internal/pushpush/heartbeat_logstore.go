package pushpush

import (
	"context"
	"database/sql"
	"fmt"
)

// HeartbeatLogStore records every accepted MacroDroid heartbeat in the shared
// database. Keeping an append-only history makes missed pings auditable.
type HeartbeatLogStore struct {
	db *sql.DB
}

const phoneHeartbeatSchema = `
CREATE TABLE IF NOT EXISTS fiat_phone_heartbeats (
    id BIGSERIAL PRIMARY KEY,
    received_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    device_id VARCHAR(128) NOT NULL,
    remote_addr TEXT,
    user_agent TEXT
);
CREATE INDEX IF NOT EXISTS idx_fiat_phone_heartbeats_received_at
    ON fiat_phone_heartbeats (received_at DESC);
`

func NewHeartbeatLogStore(db *sql.DB) (*HeartbeatLogStore, error) {
	if db == nil {
		return nil, fmt.Errorf("pushpush: heartbeat log store requires a non-nil db")
	}
	if _, err := db.Exec(phoneHeartbeatSchema); err != nil {
		var exists bool
		checkErr := db.QueryRow(`SELECT to_regclass('public.fiat_phone_heartbeats') IS NOT NULL`).Scan(&exists)
		if checkErr != nil || !exists {
			return nil, fmt.Errorf("pushpush: ensure phone heartbeat schema: %w", err)
		}
	}
	return &HeartbeatLogStore{db: db}, nil
}

func (s *HeartbeatLogStore) Insert(ctx context.Context, deviceID, remoteAddr, userAgent string) error {
	if _, err := s.db.ExecContext(ctx, `
        INSERT INTO fiat_phone_heartbeats (device_id, remote_addr, user_agent)
        VALUES ($1, $2, $3)
    `, deviceID, remoteAddr, userAgent); err != nil {
		return fmt.Errorf("pushpush: insert phone heartbeat: %w", err)
	}
	return nil
}
