package controller

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"dexFiatController/internal/auth"
	"dexFiatController/internal/browser"
	"dexFiatController/internal/connector"
	"dexFiatController/internal/models"
	"dexFiatController/internal/notifications"

	paypal "dexFiatController/internal/connector/paypal"
	revolut "dexFiatController/internal/connector/revolut"
	wise "dexFiatController/internal/connector/wise"
)

type Config struct {
	PayPal       models.ConnectorConfig
	Wise         models.ConnectorConfig
	Revolut      models.ConnectorConfig
	Browser      browser.Config
	Stagehand    browser.StagehandConfig
	PollInterval time.Duration
	Notifier     *notifications.Client
	TOTP         *auth.TOTPClient
	Passkey      *auth.PasskeyManager
	SMS          auth.SMSProvider
	Email        auth.EmailProvider
}

type FiatController struct {
	mu         sync.RWMutex
	connectors map[string]connector.Connector
	browser    *browser.Instance
	cfg        Config
}

func New(cfg Config) (*FiatController, error) {
	inst, err := browser.NewInstance(context.Background(), cfg.Browser)
	if err != nil {
		return nil, fmt.Errorf("controller: create browser instance: %w", err)
	}

	fc := &FiatController{
		connectors: make(map[string]connector.Connector),
		browser:    inst,
		cfg:        cfg,
	}

	if err := fc.initConnectors(); err != nil {
		inst.Close()
		return nil, err
	}

	return fc, nil
}

func (fc *FiatController) initConnectors() error {
	if fc.cfg.PayPal.Email != "" {
		c, err := paypal.New(fc.cfg.PayPal, fc.browser, fc.cfg.Stagehand, fc.cfg.TOTP, fc.cfg.Passkey)
		if err != nil {
			return fmt.Errorf("controller: init paypal: %w", err)
		}
		fc.connectors["paypal"] = c
		log.Println("[controller] paypal connector initialized")
	}

	if fc.cfg.Wise.Email != "" {
		c, err := wise.New(fc.cfg.Wise, fc.browser, fc.cfg.Stagehand, fc.cfg.TOTP, fc.cfg.Passkey, fc.cfg.SMS)
		if err != nil {
			return fmt.Errorf("controller: init wise: %w", err)
		}
		fc.connectors["wise"] = c
		log.Println("[controller] wise connector initialized")
	}

	if fc.cfg.Revolut.Email != "" {
		c, err := revolut.New(fc.cfg.Revolut, fc.browser, fc.cfg.Stagehand, fc.cfg.Passkey, fc.cfg.SMS, fc.cfg.Email)
		if err != nil {
			return fmt.Errorf("controller: init revolut: %w", err)
		}
		fc.connectors["revolut"] = c
		log.Println("[controller] revolut connector initialized")
	}

	return nil
}

func (fc *FiatController) LoginAll(ctx context.Context) error {
	var errs []error
	for name, c := range fc.connectors {
		log.Printf("[controller] logging into %s...", name)
		if err := c.Login(ctx); err != nil {
			log.Printf("[controller] %s login failed: %v", name, err)
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		log.Printf("[controller] %s logged in", name)
	}

	if len(errs) > 0 {
		return fmt.Errorf("controller: %d login failures: %v", len(errs), errs)
	}
	return nil
}

func (fc *FiatController) Login(ctx context.Context, service string) error {
	c, ok := fc.getConnector(service)
	if !ok {
		return fmt.Errorf("controller: unknown service %q", service)
	}
	return c.Login(ctx)
}

func (fc *FiatController) GetTransactions(ctx context.Context, service string, opts connector.TransactionQuery) ([]models.Transaction, error) {
	c, ok := fc.getConnector(service)
	if !ok {
		return nil, fmt.Errorf("controller: unknown service %q", service)
	}
	return c.GetTransactions(ctx, opts)
}

func (fc *FiatController) SendPayment(ctx context.Context, service string, req models.SendPaymentRequest) (*models.SendPaymentResult, error) {
	c, ok := fc.getConnector(service)
	if !ok {
		return nil, fmt.Errorf("controller: unknown service %q", service)
	}
	result, err := c.SendPayment(ctx, req)
	if err != nil {
		return nil, err
	}
	if err := fc.cfg.Notifier.PublishSendPaymentResult(ctx, service, req, result, nil); err != nil {
		log.Printf("[controller] %s: publish send-payment notification failed: %v", service, err)
	}
	return result, nil
}

func (fc *FiatController) GetBalance(ctx context.Context, service, currency string) (*models.Balance, error) {
	c, ok := fc.getConnector(service)
	if !ok {
		return nil, fmt.Errorf("controller: unknown service %q", service)
	}
	return c.GetBalance(ctx, currency)
}

func (fc *FiatController) IsLoggedIn(ctx context.Context, service string) (bool, error) {
	c, ok := fc.getConnector(service)
	if !ok {
		return false, fmt.Errorf("controller: unknown service %q", service)
	}
	return c.IsLoggedIn(ctx)
}

func (fc *FiatController) MonitorTransactions(ctx context.Context, handler func(service string, tx models.Transaction)) error {
	if fc.cfg.PollInterval == 0 {
		fc.cfg.PollInterval = 30 * time.Second
	}

	ticker := time.NewTicker(fc.cfg.PollInterval)
	defer ticker.Stop()

	seen := make(map[string]map[string]bool)
	for name := range fc.connectors {
		seen[name] = make(map[string]bool)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			for name, c := range fc.connectors {
				txns, err := c.GetTransactions(ctx, connector.TransactionQuery{Limit: 50})
				if err != nil {
					log.Printf("[controller] %s: fetch transactions failed: %v", name, err)
					continue
				}
				for _, tx := range txns {
					if !seen[name][tx.ID] {
						seen[name][tx.ID] = true
						if err := fc.cfg.Notifier.PublishTransaction(ctx, name, tx); err != nil {
							log.Printf("[controller] %s: publish transaction notification failed: %v", name, err)
						}
						handler(name, tx)
					}
				}
			}
		}
	}
}

func (fc *FiatController) ListConnectors() []string {
	fc.mu.RLock()
	defer fc.mu.RUnlock()

	names := make([]string, 0, len(fc.connectors))
	for name := range fc.connectors {
		names = append(names, name)
	}
	return names
}

func (fc *FiatController) Get2FAPreferences(service string) connector.TwoFAPreference {
	return auth.TwoFAPreferences(service)
}

func (fc *FiatController) Close() error {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	var errs []error
	for name, c := range fc.connectors {
		if err := c.Close(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}

	fc.browser.Close()

	if len(errs) > 0 {
		return fmt.Errorf("controller: close errors: %v", errs)
	}
	return nil
}

func (fc *FiatController) getConnector(service string) (connector.Connector, bool) {
	fc.mu.RLock()
	defer fc.mu.RUnlock()
	c, ok := fc.connectors[service]
	return c, ok
}
