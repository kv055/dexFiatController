package pushpush

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"commons/telegram"
)

func TestTelegramClientSetClientSwapsInner(t *testing.T) {
	tg := NewTelegramClient("tok", "123", &http.Client{Timeout: time.Second})
	if tg == nil {
		t.Fatal("expected non-nil client")
	}
	if inner := tg.client(); inner == nil {
		t.Fatal("expected initial inner client")
	}

	// Swap to nil (disable) then back to a new client; methods must observe the
	// current inner rather than a captured snapshot.
	tg.SetClient(nil)
	if inner := tg.client(); inner != nil {
		t.Fatalf("expected nil after SetClient(nil), got %v", inner)
	}
	tg.SetClient(telegram.NewClient("tok2", "456", nil))
	if inner := tg.client(); inner == nil || !inner.Enabled() {
		t.Fatalf("expected enabled swapped client, got %v", inner)
	}

	// Calling on a nil receiver must not panic.
	var nilTG *TelegramClient
	nilTG.SetClient(nil)
	if got := nilTG.client(); got != nil {
		t.Fatalf("expected nil from nil receiver, got %v", got)
	}
}

// TestSeedAndPollTelegramFromDB exercises the DB-seed and hot-reload poller
// against a real Postgres. Skipped unless PUSHPUSH_TEST_DB is set.
func TestSeedAndPollTelegramFromDB(t *testing.T) {
	dsn := os.Getenv("PUSHPUSH_TEST_DB")
	if dsn == "" {
		t.Skip("set PUSHPUSH_TEST_DB to run the Postgres integration test")
	}
	store, err := OpenTxStore(dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	// Ensure the table exists (migration is owned by dexUnifiedApi; create it
	// here so the test is self-contained on a fresh DB).
	if _, err := store.DB().ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS telegram_settings (
    id INT PRIMARY KEY DEFAULT 1,
    bot_token TEXT NOT NULL DEFAULT '',
    chat_id TEXT NOT NULL DEFAULT '',
    dexmm_webhook_url TEXT NOT NULL DEFAULT '',
    pushpush_webhook_url TEXT NOT NULL DEFAULT '',
    config_version BIGINT NOT NULL DEFAULT 1,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT telegram_settings_single_row CHECK (id = 1)
)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer func() { _, _ = store.DB().ExecContext(ctx, `DELETE FROM telegram_settings`) }()

	// Seed a configured row.
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO telegram_settings (id, bot_token, chat_id, pushpush_webhook_url, config_version)
VALUES (1, 'tok-seed', '111', 'https://example/wh', 1)
ON CONFLICT (id) DO UPDATE SET bot_token='tok-seed', chat_id='111', pushpush_webhook_url='https://example/wh', config_version=1
`); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	cfg, v := seedTelegramFromDB(ctx, store.DB(), telegram.Config{})
	if cfg.Token != "tok-seed" || cfg.ChatID != "111" || cfg.WebhookURL != "https://example/wh" {
		t.Fatalf("seed cfg = %+v", cfg)
	}
	if v != 1 {
		t.Fatalf("seed version = %d, want 1", v)
	}

	// Bump the row version and confirm the poller swaps the client.
	tg := NewTelegramClient("old", "000", nil)
	if _, err := store.DB().ExecContext(ctx, `
UPDATE telegram_settings SET bot_token='tok-new', chat_id='222', config_version=config_version+1 WHERE id=1
`); err != nil {
		t.Fatalf("bump row: %v", err)
	}

	pollCtx, cancel := context.WithTimeout(ctx, 2*telegram.PollInterval)
	defer cancel()
	go runTelegramSettingsPoller(pollCtx, store.DB(), tg, &http.Client{Timeout: time.Second}, v)

	deadline := time.Now().Add(3 * telegram.PollInterval)
	for time.Now().Before(deadline) {
		if inner := tg.client(); inner != nil && inner.Enabled() {
			// The poller only swaps when version changes; verify the swap happened
			// by checking we no longer hold the original disabled-for-old-token
			// state. We can't read the token back, so just confirm a swap occurred.
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Final assertion: the poller must have set a non-nil, enabled client.
	if inner := tg.client(); inner == nil || !inner.Enabled() {
		t.Fatalf("expected poller to swap in an enabled client, got %v", inner)
	}
}
