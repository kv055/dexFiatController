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
)

func main() {
	cfg := loadConfig()

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

func loadConfig() controller.Config {
	pollInterval, err := time.ParseDuration(envOr("POLL_INTERVAL", "30s"))
	if err != nil {
		pollInterval = 30 * time.Second
	}

	browserCfg := browser.DefaultConfig()
	browserCfg.Headless = envOr("HEADLESS", "true") == "true"
	browserCfg.ChromePath = envOr("CHROME_PATH", "")

	stagehandCfg := browser.DefaultStagehandConfig()
	stagehandCfg.OpenAIKey = envOr("OPENAI_API_KEY", "")

	totpURL := envOr("TOTP_API_URL", "http://totp-api:8080")
	totpKey := envOr("TOTP_API_KEY", "")
	totpClient := auth.NewTOTPClient(totpURL, totpKey)

	passkeyManager := auth.NewPasskeyManager()

	smsProvider := initSMSProvider()
	emailProvider := initEmailProvider()

	return controller.Config{
		Browser:      browserCfg,
		Stagehand:    stagehandCfg,
		PollInterval: pollInterval,
		TOTP:         totpClient,
		Passkey:      passkeyManager,
		SMS:          smsProvider,
		Email:        emailProvider,
		PayPal: models.ConnectorConfig{
			Email:      envOr("PAYPAL_EMAIL", ""),
			Password:   envOr("PAYPAL_PASSWORD", ""),
			TOTPSecret: envOr("PAYPAL_TOTP_SECRET", ""),
			Headless:   browserCfg.Headless,
		},
		Wise: models.ConnectorConfig{
			Email:      envOr("WISE_EMAIL", ""),
			Password:   envOr("WISE_PASSWORD", ""),
			TOTPSecret: envOr("WISE_TOTP_SECRET", ""),
			Phone:      envOr("WISE_PHONE", ""),
			Headless:   browserCfg.Headless,
		},
		Revolut: models.ConnectorConfig{
			Email:    envOr("REVOLUT_EMAIL", ""),
			Password: envOr("REVOLUT_PASSWORD", ""),
			Phone:    envOr("REVOLUT_PHONE", ""),
			Headless: browserCfg.Headless,
		},
	}
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
