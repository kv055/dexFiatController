package pushpush

import (
	"net/http"
	"os"
	"strings"
	"time"
)

// Config holds all runtime configuration for the pushpush server.
type Config struct {
	Addr string

	// NotifySecret gates POST /notify via the X-Webhook-Secret header. Empty disables
	// the check (not recommended in production).
	NotifySecret string

	// DataDir is where rules.json and notifications.jsonl are persisted.
	DataDir string

	// DatabaseURL is the shared Postgres DSN (same one the scraping secrets store
	// uses).
	DatabaseURL string

	// ReleaseURL is the dexMmController dex-release webhook.
	ReleaseURL   string
	ReleaseToken string

	// ReleaseTimeout bounds the webhook HTTP call.
	ReleaseTimeout time.Duration

	// Telegram escalation. TelegramToken/ChatID empty disables escalation.
	TelegramToken      string
	TelegramChatID     string
	TelegramWebhookURL string

	HTTPClient *http.Client

	// CaptureOnly runs the server without a database: every request is dumped (body,
	// headers, query, remote addr) to capture.jsonl for payload-shape discovery. Enabled
	// when DATABASE_URL is empty or PUSHPUSH_CAPTURE_ONLY=true.
	CaptureOnly bool
}

// LoadConfig reads pushpush configuration from environment variables.
func LoadConfig() Config {
	dataDir := strings.TrimSpace(os.Getenv("PUSHPUSH_DATA_DIR"))
	if dataDir == "" {
		dataDir = "/data"
	}

	timeout := 10 * time.Second
	if v := strings.TrimSpace(os.Getenv("PUSHPUSH_RELEASE_TIMEOUT")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			timeout = d
		}
	}

	return Config{
		Addr:               strings.TrimSpace(os.Getenv("PUSHPUSH_ADDR")),
		NotifySecret:       strings.TrimSpace(os.Getenv("PUSHPUSH_NOTIFY_SECRET")),
		DataDir:            dataDir,
		DatabaseURL:        strings.TrimSpace(os.Getenv("DATABASE_URL")),
		ReleaseURL:         strings.TrimSpace(os.Getenv("PUSHPUSH_DEX_RELEASE_URL")),
		ReleaseToken:       strings.TrimSpace(os.Getenv("PUSHPUSH_DEX_RELEASE_TOKEN")),
		ReleaseTimeout:     timeout,
		TelegramToken:      strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN")),
		TelegramChatID:     strings.TrimSpace(os.Getenv("TELEGRAM_CHAT_ID")),
		TelegramWebhookURL: strings.TrimSpace(os.Getenv("PUSHPUSH_TELEGRAM_WEBHOOK_URL")),
		HTTPClient:         &http.Client{Timeout: timeout},
		CaptureOnly:        strings.TrimSpace(os.Getenv("DATABASE_URL")) == "" || os.Getenv("PUSHPUSH_CAPTURE_ONLY") == "true",
	}
}

// EscalationEnabled reports whether Telegram escalation is configured.
func (c Config) EscalationEnabled() bool {
	return c.TelegramToken != "" && c.TelegramChatID != ""
}
