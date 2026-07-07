package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"dexFiatController/internal/obs"
	"dexFiatController/internal/pushpush"

	"commons/logging"
	"commons/metrics"
	"commons/telegram"

	scrapingauth "dexFiatController/internal/scraping/auth"
	scrapingbrowser "dexFiatController/internal/scraping/browser"
	scrapingcontroller "dexFiatController/internal/scraping/controller"
	scrapingmodels "dexFiatController/internal/scraping/models"
	scrapingnotifications "dexFiatController/internal/scraping/notifications"
	scrapingsecrets "dexFiatController/internal/scraping/secrets"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Structured JSON logs on stdout (Alloy → Loki). slog.SetDefault also routes
	// the stdlib log package through the same handler, so existing log.Printf
	// call sites come out as JSON too.
	logging.Init("fiatcontroller")
	metricsReg := metrics.NewServiceRegistry("fiatcontroller")
	obs.Register(metricsReg.Registry)
	telegram.RegisterMetrics(metricsReg.Registry)

	// pushpush is the primary fiat-arrival path: it receives pushed payment
	// notifications, matches them to pending transactions, and triggers BTC release.
	ppCfg := pushpush.LoadConfig()
	if ppCfg.Addr == "" {
		ppCfg.Addr = ":8080"
	}
	ppServer, err := pushpush.New(ppCfg)
	if err != nil {
		log.Fatalf("failed to create pushpush server: %v", err)
	}
	ppServer.WithMetrics(metricsReg)
	defer ppServer.Close()

	go func() {
		if err := ppServer.Run(ctx); err != nil && ctx.Err() == nil {
			log.Printf("pushpush server: %v", err)
		}
	}()
	log.Printf("pushpush listening on %s (escalation=%v)", ppCfg.Addr, ppCfg.EscalationEnabled())

	// Scraping is the last-resort path: only run when explicitly enabled.
	if envOr("SCRAPING_ENABLED", "false") == "true" {
		runScraping(ctx)
	} else {
		log.Println("scraping controller disabled (set SCRAPING_ENABLED=true to enable as last resort)")
	}

	<-ctx.Done()
	log.Println("shutting down...")
}

// runScraping starts the browser-automation fiat controller. It is gated behind
// SCRAPING_ENABLED because provider websites change often and push-based arrival is
// preferred wherever a connector can push notifications.
func runScraping(ctx context.Context) {
	cfg, cleanup := loadScrapingConfig()
	defer cleanup()

	fc, err := scrapingcontroller.New(cfg)
	if err != nil {
		log.Fatalf("failed to create fiat controller: %v", err)
	}
	defer fc.Close()

	log.Printf("initialized scraping connectors: %v", fc.ListConnectors())

	for _, name := range fc.ListConnectors() {
		pref := fc.Get2FAPreferences(name)
		log.Printf("[2fa] %s: primary=%s secondary=%s fallback=%s", name, pref.Primary, pref.Secondary, pref.Fallback)
	}

	log.Println("logging into all scraping connectors...")
	if err := fc.LoginAll(ctx); err != nil {
		obs.ScrapeLoginErrors.Inc()
		log.Printf("warning: some logins failed: %v", err)
	}

	log.Println("starting scraping transaction monitor...")
	go func() {
		if err := fc.MonitorTransactions(ctx, func(service string, tx scrapingmodels.Transaction) {
			obs.ScrapeTransactionsSeen.WithLabelValues(service).Inc()
			log.Printf("[monitor] %s: %s %s %.2f %s -> %s",
				service, tx.Direction, tx.Status, tx.Amount, tx.Currency, tx.Counterparty)
		}); err != nil && ctx.Err() == nil {
			log.Printf("scraping monitor failed: %v", err)
		}
	}()
}

func loadScrapingConfig() (scrapingcontroller.Config, func()) {
	pollInterval, err := time.ParseDuration(envOr("POLL_INTERVAL", "30s"))
	if err != nil {
		pollInterval = 30 * time.Second
	}

	cleanup := func() {
		// no secrets DB opened; nothing to close
	}

	browserCfg := scrapingbrowser.DefaultConfig()
	browserCfg.Headless = envOr("HEADLESS", "true") == "true"
	browserCfg.ChromePath = envOr("CHROME_PATH", "")

	stagehandCfg := scrapingbrowser.DefaultStagehandConfig()
	stagehandCfg.OpenAIKey = envOr("OPENAI_API_KEY", "")

	totpURL := envOr("TOTP_API_URL", "http://totp-api:8080")
	totpKey := envOr("TOTP_API_KEY", "")
	totpClient := scrapingauth.NewTOTPClient(totpURL, totpKey)

	cfg := scrapingcontroller.Config{
		Browser:      browserCfg,
		Stagehand:    stagehandCfg,
		PollInterval: pollInterval,
		Notifier: scrapingnotifications.NewClient(scrapingnotifications.Config{
			Endpoint:   envOr("PAYMENT_NOTIFICATIONS_API_URL", ""),
			Secret:     envOr("PAYMENT_NOTIFICATIONS_SHARED_SECRET", ""),
			DefaultDEX: envOr("PAYMENT_NOTIFICATIONS_DEX", ""),
			Timeout:    10 * time.Second,
		}),
		TOTP: totpClient,
		PayPal: scrapingmodels.ConnectorConfig{
			Headless: browserCfg.Headless,
		},
		Wise: scrapingmodels.ConnectorConfig{
			Headless: browserCfg.Headless,
		},
		Revolut: scrapingmodels.ConnectorConfig{
			Headless: browserCfg.Headless,
		},
	}

	var passkeyStore scrapingauth.PasskeyStore
	if databaseURL := envOr("DATABASE_URL", ""); databaseURL != "" {
		secretStore, err := scrapingsecrets.Open(databaseURL)
		if err != nil {
			log.Fatalf("failed to open secrets database: %v", err)
		}
		cleanup = func() {
			if err := secretStore.Close(); err != nil {
				log.Printf("warning: close secrets database: %v", err)
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := hydrateAccountsFromDB(ctx, secretStore, &cfg); err != nil {
			log.Fatalf("failed to load account credentials from database: %v", err)
		}
		if err := hydrateTOTPSecretsFromDB(ctx, secretStore, &cfg); err != nil {
			log.Fatalf("failed to load TOTP secrets from database: %v", err)
		}

		passkeyStore = secretStore
	} else {
		log.Fatal("DATABASE_URL is required for unified TOTP/passkey storage")
	}

	passkeyManager, err := scrapingauth.NewPasskeyManager(context.Background(), passkeyStore)
	if err != nil {
		log.Fatalf("failed to initialize passkey manager: %v", err)
	}
	cfg.Passkey = passkeyManager

	cfg.SMS = initSMSProvider()
	cfg.Email = initEmailProvider()

	return cfg, cleanup
}

func initSMSProvider() scrapingauth.SMSProvider {
	provider := envOr("SMS_PROVIDER", "")
	switch provider {
	case "twilio":
		return scrapingauth.NewTwilioSMSProvider(
			envOr("SMS_ACCOUNT_SID", ""),
			envOr("SMS_AUTH_TOKEN", ""),
			envOr("SMS_PHONE_NUMBER", ""),
		)
	default:
		log.Println("no SMS provider configured; SMS-based 2FA will not be available")
		return nil
	}
}

func initEmailProvider() scrapingauth.EmailProvider {
	provider := envOr("EMAIL_PROVIDER", "")
	switch provider {
	case "imap":
		return scrapingauth.NewIMAPProvider(
			envOr("EMAIL_IMAP_HOST", ""),
			993,
			envOr("EMAIL_IMAP_USER", ""),
			envOr("EMAIL_IMAP_PASS", ""),
		)
	default:
		log.Println("no email provider configured; email-based 2FA will not be available")
		return nil
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func hydrateAccountsFromDB(ctx context.Context, store *scrapingsecrets.Store, cfg *scrapingcontroller.Config) error {
	accountsByProvider, err := store.LoadAccounts(ctx)
	if err != nil {
		return err
	}

	if account, ok := accountsByProvider["paypal"]; ok {
		if cfg.PayPal.Email == "" {
			cfg.PayPal.Email = account.Email
		}
		if cfg.PayPal.Password == "" {
			cfg.PayPal.Password = account.Password
		}
	}
	if account, ok := accountsByProvider["wise"]; ok {
		if cfg.Wise.Email == "" {
			cfg.Wise.Email = account.Email
		}
		if cfg.Wise.Password == "" {
			cfg.Wise.Password = account.Password
		}
		if cfg.Wise.Phone == "" {
			cfg.Wise.Phone = account.Phone
		}
	}
	if account, ok := accountsByProvider["revolut"]; ok {
		if cfg.Revolut.Email == "" {
			cfg.Revolut.Email = account.Email
		}
		if cfg.Revolut.Password == "" {
			cfg.Revolut.Password = account.Password
		}
		if cfg.Revolut.Phone == "" {
			cfg.Revolut.Phone = account.Phone
		}
	}
	return nil
}

func hydrateTOTPSecretsFromDB(ctx context.Context, store *scrapingsecrets.Store, cfg *scrapingcontroller.Config) error {
	secretsByProvider, err := store.LoadActiveTOTPSecrets(ctx)
	if err != nil {
		return err
	}

	if cfg.PayPal.TOTPSecret == "" {
		cfg.PayPal.TOTPSecret = secretsByProvider["paypal"]
	}
	if cfg.Wise.TOTPSecret == "" {
		cfg.Wise.TOTPSecret = secretsByProvider["wise"]
	}
	return nil
}
