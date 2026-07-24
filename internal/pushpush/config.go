package pushpush

import (
	"net/http"
	"os"
	"strings"
	"time"

	"commons/telegram"
	"dexFiatController/internal/paymentemail"
)

// Config holds all runtime configuration for the pushpush server.
type Config struct {
	Addr string

	// NotifySecret gates POST /notify via the X-Webhook-Secret header. In normal
	// operation an empty value rejects every request; only explicit capture-only
	// discovery mode accepts unauthenticated payloads.
	NotifySecret string

	// HeartbeatSecret authorizes phone heartbeat requests. When empty,
	// LoadConfig falls back to NotifySecret so a MacroDroid installation only
	// needs one shared secret by default.
	HeartbeatSecret string
	// HeartbeatMaxAge is how long a received heartbeat keeps the phone online.
	HeartbeatMaxAge time.Duration

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

	// Telegram escalation (TelegramCfg.Token/ChatID empty disables escalation).
	TelegramCfg telegram.Config

	HTTPClient *http.Client

	// CaptureOnly runs the server without a database: every request is dumped (body,
	// headers, query, remote addr) to capture.jsonl for payload-shape discovery. Enabled
	// when DATABASE_URL is empty or PUSHPUSH_CAPTURE_ONLY=true.
	CaptureOnly bool

	PaymentEmail paymentemail.PaymentEmailConfig
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
	heartbeatMaxAge := 15 * time.Minute
	if v := strings.TrimSpace(os.Getenv("PUSHPUSH_HEARTBEAT_MAX_AGE")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			heartbeatMaxAge = d
		}
	}
	notifySecret := strings.TrimSpace(os.Getenv("PUSHPUSH_NOTIFY_SECRET"))
	heartbeatSecret := strings.TrimSpace(os.Getenv("PUSHPUSH_HEARTBEAT_SECRET"))
	if heartbeatSecret == "" {
		heartbeatSecret = notifySecret
	}

	return Config{
		Addr:            strings.TrimSpace(os.Getenv("PUSHPUSH_ADDR")),
		NotifySecret:    notifySecret,
		HeartbeatSecret: heartbeatSecret,
		HeartbeatMaxAge: heartbeatMaxAge,
		DataDir:         dataDir,
		DatabaseURL:     strings.TrimSpace(os.Getenv("DATABASE_URL")),
		ReleaseURL:      strings.TrimSpace(os.Getenv("PUSHPUSH_DEX_RELEASE_URL")),
		ReleaseToken:    strings.TrimSpace(os.Getenv("PUSHPUSH_DEX_RELEASE_TOKEN")),
		ReleaseTimeout:  timeout,
		TelegramCfg:     telegram.LoadConfig("PUSHPUSH"),
		HTTPClient:      &http.Client{Timeout: timeout},
		CaptureOnly:     strings.TrimSpace(os.Getenv("DATABASE_URL")) == "" || os.Getenv("PUSHPUSH_CAPTURE_ONLY") == "true",
		PaymentEmail:    paymentemail.LoadConfig(),
	}
}

// EscalationEnabled reports whether Telegram escalation is configured.
func (c Config) EscalationEnabled() bool {
	return c.TelegramCfg.Enabled()
}
