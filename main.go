package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"dexFiatController/internal/auth"
	"dexFiatController/internal/browser"
	"dexFiatController/internal/controller"
	"dexFiatController/internal/models"
	"dexFiatController/internal/notifications"
	"dexFiatController/internal/secrets"
)

func main() {
	cfg, cleanup := loadConfig()
	defer cleanup()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fc, err := controller.New(cfg)
	if err != nil {
		log.Fatalf("failed to create fiat controller: %v", err)
	}
	defer fc.Close()

	log.Printf("initialized connectors: %v", fc.ListConnectors())

	for _, name := range fc.ListConnectors() {
		pref := fc.Get2FAPreferences(name)
		log.Printf("[2fa] %s: primary=%s secondary=%s fallback=%s", name, pref.Primary, pref.Secondary, pref.Fallback)
	}

	log.Println("logging into all connectors...")
	if err := fc.LoginAll(ctx); err != nil {
		log.Printf("warning: some logins failed: %v", err)
	}

	log.Println("starting transaction monitor...")
	go func() {
		if err := fc.MonitorTransactions(ctx, func(service string, tx models.Transaction) {
			log.Printf("[monitor] %s: %s %s %.2f %s -> %s",
				service, tx.Direction, tx.Status, tx.Amount, tx.Currency, tx.Counterparty)
		}); err != nil && ctx.Err() == nil {
			log.Printf("monitor failed: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down...")
}

func loadConfig() (controller.Config, func()) {
	pollInterval, err := time.ParseDuration(envOr("POLL_INTERVAL", "30s"))
	if err != nil {
		pollInterval = 30 * time.Second
	}

	cleanup := func() {}

	browserCfg := browser.DefaultConfig()
	browserCfg.Headless = envOr("HEADLESS", "true") == "true"
	browserCfg.ChromePath = envOr("CHROME_PATH", "")

	stagehandCfg := browser.DefaultStagehandConfig()
	stagehandCfg.OpenAIKey = envOr("OPENAI_API_KEY", "")

	totpURL := envOr("TOTP_API_URL", "http://totp-api:8080")
	totpKey := envOr("TOTP_API_KEY", "")
	totpClient := auth.NewTOTPClient(totpURL, totpKey)

	cfg := controller.Config{
		Browser:      browserCfg,
		Stagehand:    stagehandCfg,
		PollInterval: pollInterval,
		Notifier: notifications.NewClient(notifications.Config{
			Endpoint:   envOr("PAYMENT_NOTIFICATIONS_API_URL", ""),
			Secret:     envOr("PAYMENT_NOTIFICATIONS_SHARED_SECRET", ""),
			DefaultDEX: envOr("PAYMENT_NOTIFICATIONS_DEX", ""),
			Timeout:    10 * time.Second,
		}),
		TOTP: totpClient,
		PayPal: models.ConnectorConfig{
			Headless: browserCfg.Headless,
		},
		Wise: models.ConnectorConfig{
			Headless: browserCfg.Headless,
		},
		Revolut: models.ConnectorConfig{
			Headless: browserCfg.Headless,
		},
	}

	var passkeyStore auth.PasskeyStore
	if databaseURL := envOr("DATABASE_URL", ""); databaseURL != "" {
		secretStore, err := secrets.Open(databaseURL)
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

	passkeyManager, err := auth.NewPasskeyManager(context.Background(), passkeyStore)
	if err != nil {
		log.Fatalf("failed to initialize passkey manager: %v", err)
	}
	cfg.Passkey = passkeyManager

	smsProvider := initSMSProvider()
	emailProvider := initEmailProvider()
	cfg.SMS = smsProvider
	cfg.Email = emailProvider

	return cfg, cleanup
}

func initSMSProvider() auth.SMSProvider {
	provider := envOr("SMS_PROVIDER", "")
	switch provider {
	case "twilio":
		return auth.NewTwilioSMSProvider(
			envOr("SMS_ACCOUNT_SID", ""),
			envOr("SMS_AUTH_TOKEN", ""),
			envOr("SMS_PHONE_NUMBER", ""),
		)
	default:
		log.Println("no SMS provider configured; SMS-based 2FA will not be available")
		return nil
	}
}

func initEmailProvider() auth.EmailProvider {
	provider := envOr("EMAIL_PROVIDER", "")
	switch provider {
	case "imap":
		return auth.NewIMAPProvider(
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

func hydrateAccountsFromDB(ctx context.Context, store *secrets.Store, cfg *controller.Config) error {
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

func hydrateTOTPSecretsFromDB(ctx context.Context, store *secrets.Store, cfg *controller.Config) error {
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
