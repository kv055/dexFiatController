package paymentemail

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
)

type PaymentEmailMode string

const (
	Capture PaymentEmailMode = "capture"
	Shadow  PaymentEmailMode = "shadow"
	Active  PaymentEmailMode = "active"
)

// PaymentEmailConfig is intentionally separate from EMAIL_IMAP_* used for
// provider-login 2FA.
type PaymentEmailConfig struct {
	Mode           PaymentEmailMode
	Host           string
	Port           int
	ServerName     string
	Username       string
	Password       string
	Mailbox        string
	CAFile         string
	PollInterval   time.Duration
	HealthMaxAge   time.Duration
	ReconnectMin   time.Duration
	ReconnectMax   time.Duration
	ActivationUID  uint32
	ActivationTime time.Time
}

func LoadConfig() PaymentEmailConfig {
	cfg := PaymentEmailConfig{
		Mode:         PaymentEmailMode(strings.ToLower(strings.TrimSpace(os.Getenv("PAYMENT_EMAIL_MODE")))),
		Host:         strings.TrimSpace(os.Getenv("PAYMENT_EMAIL_IMAP_HOST")),
		ServerName:   strings.TrimSpace(os.Getenv("PAYMENT_EMAIL_TLS_SERVER_NAME")),
		Username:     strings.TrimSpace(os.Getenv("PAYMENT_EMAIL_IMAP_USER")),
		Password:     os.Getenv("PAYMENT_EMAIL_IMAP_PASS"),
		Mailbox:      strings.TrimSpace(os.Getenv("PAYMENT_EMAIL_IMAP_MAILBOX")),
		CAFile:       strings.TrimSpace(os.Getenv("PAYMENT_EMAIL_TLS_CA_FILE")),
		PollInterval: durationEnv("PAYMENT_EMAIL_POLL_INTERVAL", 5*time.Minute),
		HealthMaxAge: durationEnv("PAYMENT_EMAIL_HEALTH_MAX_AGE", 15*time.Minute),
		ReconnectMin: durationEnv("PAYMENT_EMAIL_RECONNECT_MIN", 2*time.Second),
		ReconnectMax: durationEnv("PAYMENT_EMAIL_RECONNECT_MAX", time.Minute),
	}
	if cfg.Mode == "" {
		cfg.Mode = Capture
	}
	if cfg.Mailbox == "" {
		cfg.Mailbox = "INBOX"
	}
	cfg.Port = 993
	if value, err := strconv.Atoi(strings.TrimSpace(os.Getenv("PAYMENT_EMAIL_IMAP_PORT"))); err == nil && value > 0 {
		cfg.Port = value
	}
	if value, err := strconv.ParseUint(strings.TrimSpace(os.Getenv("PAYMENT_EMAIL_ACTIVATION_UID")), 10, 32); err == nil {
		cfg.ActivationUID = uint32(value)
	}
	if value := strings.TrimSpace(os.Getenv("PAYMENT_EMAIL_ACTIVATION_TIME")); value != "" {
		cfg.ActivationTime, _ = time.Parse(time.RFC3339, value)
	}
	return cfg
}

func durationEnv(name string, fallback time.Duration) time.Duration {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		if parsed, err := time.ParseDuration(value); err == nil && parsed > 0 {
			return parsed
		}
	}
	return fallback
}

func (c PaymentEmailConfig) Enabled() bool {
	return c.Host != "" && c.Username != "" && c.Password != ""
}

func (c PaymentEmailConfig) Validate() error {
	switch c.Mode {
	case Capture, Shadow:
	case Active:
		if c.ActivationUID == 0 || c.ActivationTime.IsZero() {
			return fmt.Errorf("active mode requires PAYMENT_EMAIL_ACTIVATION_UID and PAYMENT_EMAIL_ACTIVATION_TIME")
		}
	default:
		return fmt.Errorf("unsupported PAYMENT_EMAIL_MODE %q", c.Mode)
	}
	if c.Enabled() && c.ServerName == "" {
		return fmt.Errorf("PAYMENT_EMAIL_TLS_SERVER_NAME is required")
	}
	return nil
}

type EmailHealth struct {
	mu          sync.RWMutex
	lastSuccess time.Time
}

func (h *EmailHealth) Success(at time.Time) {
	h.mu.Lock()
	h.lastSuccess = at.UTC()
	h.mu.Unlock()
}

func (h *EmailHealth) LastSuccess() time.Time {
	if h == nil {
		return time.Time{}
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.lastSuccess
}

type EmailReader struct {
	cfg    PaymentEmailConfig
	health *EmailHealth
	handle func(context.Context, uint32, []byte) error
}

func NewEmailReader(cfg PaymentEmailConfig, health *EmailHealth, handle func(context.Context, uint32, []byte) error) (*EmailReader, error) {
	if cfg.Mode == "" {
		cfg.Mode = Capture
	}
	if cfg.Mailbox == "" {
		cfg.Mailbox = "INBOX"
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 5 * time.Minute
	}
	if cfg.HealthMaxAge <= 0 {
		cfg.HealthMaxAge = 15 * time.Minute
	}
	if cfg.ReconnectMin <= 0 {
		cfg.ReconnectMin = 2 * time.Second
	}
	if cfg.ReconnectMax < cfg.ReconnectMin {
		cfg.ReconnectMax = time.Minute
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if handle == nil {
		return nil, fmt.Errorf("payment email handler is required")
	}
	return &EmailReader{cfg: cfg, health: health, handle: handle}, nil
}

func (r *EmailReader) Config() PaymentEmailConfig { return r.cfg }

// Run reconnects with bounded exponential backoff. Each connected cycle uses
// IMAP IDLE, interrupted periodically for a UID reconciliation poll.
func (r *EmailReader) Run(ctx context.Context) {
	if !r.cfg.Enabled() {
		log.Printf("payment email reader disabled: PAYMENT_EMAIL_IMAP_* credentials are incomplete")
		return
	}
	backoff := r.cfg.ReconnectMin
	for ctx.Err() == nil {
		if err := r.runConnected(ctx); err != nil && ctx.Err() == nil {
			log.Printf("payment email reader disconnected: %v", err)
		}
		if ctx.Err() != nil {
			return
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		backoff *= 2
		if backoff > r.cfg.ReconnectMax {
			backoff = r.cfg.ReconnectMax
		}
	}
}

func (r *EmailReader) runConnected(ctx context.Context) error {
	tlsConfig, err := r.tlsConfig()
	if err != nil {
		return err
	}
	address := net.JoinHostPort(r.cfg.Host, strconv.Itoa(r.cfg.Port))
	imapClient, err := client.DialTLS(address, tlsConfig)
	if err != nil {
		return fmt.Errorf("dial IMAP TLS: %w", err)
	}
	defer imapClient.Logout()
	imapClient.Timeout = 30 * time.Second
	if err := imapClient.Login(r.cfg.Username, r.cfg.Password); err != nil {
		return fmt.Errorf("login IMAP: %w", err)
	}
	if _, err := imapClient.Select(r.cfg.Mailbox, true); err != nil {
		return fmt.Errorf("select IMAP mailbox: %w", err)
	}
	for ctx.Err() == nil {
		if err := r.reconcile(ctx, imapClient); err != nil {
			return err
		}
		r.health.Success(time.Now())

		stopIdle := make(chan struct{})
		idleDone := make(chan error, 1)
		go func() {
			idleDone <- imapClient.Idle(stopIdle, &client.IdleOptions{
				LogoutTimeout: 24 * time.Minute, PollInterval: r.cfg.PollInterval,
			})
		}()
		timer := time.NewTimer(r.cfg.PollInterval)
		select {
		case <-ctx.Done():
			close(stopIdle)
			timer.Stop()
			<-idleDone
			return ctx.Err()
		case <-timer.C:
			close(stopIdle)
			if err := <-idleDone; err != nil {
				return fmt.Errorf("IMAP IDLE: %w", err)
			}
		case err := <-idleDone:
			timer.Stop()
			if err != nil {
				return fmt.Errorf("IMAP IDLE: %w", err)
			}
		}
	}
	return ctx.Err()
}

func (r *EmailReader) tlsConfig() (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: r.cfg.ServerName}
	if r.cfg.CAFile == "" {
		return cfg, nil
	}
	pem, err := os.ReadFile(r.cfg.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read payment email CA: %w", err)
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = x509.NewCertPool()
	}
	if !roots.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("PAYMENT_EMAIL_TLS_CA_FILE contains no certificate")
	}
	cfg.RootCAs = roots
	return cfg, nil
}

func (r *EmailReader) reconcile(ctx context.Context, imapClient *client.Client) error {
	uids, err := imapClient.UidSearch(&imap.SearchCriteria{})
	if err != nil {
		return fmt.Errorf("search IMAP UIDs: %w", err)
	}
	for _, uid := range uids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := r.fetchAndIngest(ctx, imapClient, uid); err != nil {
			log.Printf("payment email UID %d rejected: %v", uid, err)
		}
	}
	return nil
}

func (r *EmailReader) fetchAndIngest(ctx context.Context, imapClient *client.Client, uid uint32) error {
	seq := new(imap.SeqSet)
	seq.AddNum(uid)
	section := &imap.BodySectionName{Peek: true}
	messages := make(chan *imap.Message, 1)
	done := make(chan error, 1)
	go func() {
		done <- imapClient.UidFetch(seq, []imap.FetchItem{imap.FetchUid, section.FetchItem()}, messages)
	}()
	var raw []byte
	for message := range messages {
		body := message.GetBody(section)
		if body == nil {
			continue
		}
		raw, _ = io.ReadAll(io.LimitReader(body, maxPaymentEmailBytes+1))
	}
	if err := <-done; err != nil {
		return fmt.Errorf("fetch UID %d: %w", uid, err)
	}
	if len(raw) == 0 {
		return fmt.Errorf("UID %d has no body", uid)
	}
	return r.handle(ctx, uid, raw)
}
