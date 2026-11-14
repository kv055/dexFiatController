package pushpush

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"strings"
	"time"

	"commons/telegram"
)

// telegramSettingsRow mirrors the single shared telegram_settings row (see
// dexUnifiedApi/db/schema/22_telegram_settings.sql). pushpush reads it via raw
// SQL on the same *sql.DB it uses for pending transactions — no gorm dependency.
type telegramSettingsRow struct {
	BotToken           string
	ChatID             string
	PushpushWebhookURL string
	ConfigVersion      int64
}

const telegramSettingsSelect = `
SELECT bot_token, chat_id, pushpush_webhook_url, config_version
FROM telegram_settings
WHERE id = 1
`

// loadTelegramSettings reads the single shared telegram_settings row.
// Returns (zero, false, nil) when the row has not been seeded yet.
func loadTelegramSettings(ctx context.Context, db *sql.DB) (telegramSettingsRow, bool, error) {
	var row telegramSettingsRow
	err := db.QueryRowContext(ctx, telegramSettingsSelect).Scan(
		&row.BotToken, &row.ChatID, &row.PushpushWebhookURL, &row.ConfigVersion,
	)
	if err == sql.ErrNoRows {
		return telegramSettingsRow{}, false, nil
	}
	if err != nil {
		return telegramSettingsRow{}, false, err
	}
	return row, true, nil
}

// telegramSettingsVersion returns the current config_version, or 0 when the row
// is absent. Cheap polling primitive used by the hot-reload poller.
func telegramSettingsVersion(ctx context.Context, db *sql.DB) (int64, error) {
	var v int64
	err := db.QueryRowContext(ctx, `SELECT config_version FROM telegram_settings WHERE id = 1`).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return v, nil
}

// seedTelegramFromDB overrides cfg.TelegramCfg with the shared DB row when
// present and fully configured. Returns the effective config and the loaded row
// (for the poller's initial version baseline). Env values remain the fallback
// when the row is absent or has no bot token.
func seedTelegramFromDB(ctx context.Context, db *sql.DB, fallback telegram.Config) (telegram.Config, int64) {
	row, ok, err := loadTelegramSettings(ctx, db)
	if err != nil {
		log.Printf("pushpush: load telegram settings: %v", err)
		return fallback, 0
	}
	if !ok || strings.TrimSpace(row.BotToken) == "" {
		return fallback, 0
	}
	return telegram.Config{
		Token:      row.BotToken,
		ChatID:     row.ChatID,
		WebhookURL: row.PushpushWebhookURL,
	}, row.ConfigVersion
}

// runTelegramSettingsPoller watches the shared telegram_settings row for
// config_version changes and applies them to the pushpush TelegramClient at
// runtime: it rebuilds the inner *telegram.Client and re-registers the PUSHPUSH
// webhook with Telegram when one is configured. This is the hot-reload path that
// lets the dexTakerController UI update the shared bot credentials without a
// pushpush restart.
func runTelegramSettingsPoller(ctx context.Context, db *sql.DB, tg *TelegramClient, httpClient *http.Client, lastVersion int64) {
	if db == nil || tg == nil {
		return
	}
	ticker := time.NewTicker(telegram.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			v, err := telegramSettingsVersion(ctx, db)
			if err != nil {
				log.Printf("pushpush: telegram poller: %v", err)
				continue
			}
			if v == lastVersion {
				continue
			}
			row, ok, err := loadTelegramSettings(ctx, db)
			if err != nil {
				log.Printf("pushpush: telegram poller: load: %v", err)
				continue
			}
			if !ok {
				continue
			}
			lastVersion = row.ConfigVersion
			tg.SetClient(telegram.NewClient(row.BotToken, row.ChatID, httpClient))
			if inner := tg.client(); inner != nil && strings.TrimSpace(row.PushpushWebhookURL) != "" {
				if err := inner.SetWebhook(ctx, row.PushpushWebhookURL); err != nil {
					log.Printf("pushpush: telegram poller: setWebhook: %v", err)
				}
			}
			log.Printf("pushpush: telegram settings reloaded (version=%d)", row.ConfigVersion)
		}
	}
}
