package revolut

import (
	"context"
	"fmt"
	"log"
	neturl "net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/chromedp"

	"dexFiatController/internal/scraping/auth"
	"dexFiatController/internal/scraping/browser"
	"dexFiatController/internal/scraping/connector"
	"dexFiatController/internal/scraping/models"
)

const (
	loginURL    = "https://app.revolut.com/login"
	activityURL = "https://app.revolut.com/transactions"
	sendURL     = "https://app.revolut.com/payments/new"
	balanceURL  = "https://app.revolut.com/dashboard"
	serviceName = "revolut"
)

type Connector struct {
	sh       *browser.Stagehand
	browser  *browser.Instance
	passkey  *auth.PasskeyManager
	auth     *auth.Resolver
	sms      auth.SMSProvider
	email    auth.EmailProvider
	cfg      models.ConnectorConfig
	loggedIn bool
}

func New(cfg models.ConnectorConfig, inst *browser.Instance, shCfg browser.StagehandConfig, passkey *auth.PasskeyManager, sms auth.SMSProvider, email auth.EmailProvider) (*Connector, error) {
	registry := browser.NewSelectorRegistry(serviceName)
	RegisterSelectors(registry)
	sh := browser.NewStagehand(serviceName, shCfg, registry)

	resolver := auth.NewResolver(nil, passkey, sms, email)

	return &Connector{
		sh:      sh,
		browser: inst,
		passkey: passkey,
		auth:    resolver,
		sms:     sms,
		email:   email,
		cfg:     cfg,
	}, nil
}

func (c *Connector) Name() string {
	return serviceName
}

func (c *Connector) Login(ctx context.Context) error {
	browserCtx, cancel := c.browser.NewContext(ctx)
	defer cancel()

	if c.passkey != nil {
		if err := c.passkey.Enable(browserCtx); err != nil {
			log.Printf("[revolut] warning: passkey setup failed: %v", err)
		}
	}

	if err := c.sh.Navigate(browserCtx, loginURL); err != nil {
		return fmt.Errorf("revolut: navigate to login: %w", err)
	}
	time.Sleep(2 * time.Second)

	if err := c.sh.WaitVisible(browserCtx, "login_email"); err != nil {
		return fmt.Errorf("revolut: email/phone field not visible: %w", err)
	}

	loginID := c.cfg.Email
	if c.cfg.Phone != "" {
		loginID = c.cfg.Phone
	}

	if err := c.sh.SendKeys(browserCtx, "login_email", loginID); err != nil {
		return fmt.Errorf("revolut: enter email/phone: %w", err)
	}

	if err := c.sh.SendKeys(browserCtx, "login_password", c.cfg.Password); err != nil {
		return fmt.Errorf("revolut: enter password: %w", err)
	}

	if err := c.sh.Click(browserCtx, "login_btn"); err != nil {
		return fmt.Errorf("revolut: click login: %w", err)
	}
	time.Sleep(3 * time.Second)

	if c.requires2FA(browserCtx) {
		if err := c.handle2FA(browserCtx); err != nil {
			return fmt.Errorf("revolut: 2fa: %w", err)
		}
	}
	time.Sleep(2 * time.Second)

	loggedIn, err := c.checkLoginSuccess(browserCtx)
	if err != nil {
		return fmt.Errorf("revolut: login verification: %w", err)
	}
	if !loggedIn {
		return fmt.Errorf("revolut: login failed")
	}

	c.loggedIn = true
	if err := c.maybeRegisterPasskey(browserCtx); err != nil {
		log.Printf("[revolut] warning: passkey registration skipped: %v", err)
	}
	log.Printf("[revolut] logged in as %s", loginID)
	return nil
}

func (c *Connector) requires2FA(ctx context.Context) bool {
	var pageTitle string
	if err := browser.RunInto(ctx, chromedp.Title(), &pageTitle); err != nil {
		return false
	}
	lower := strings.ToLower(pageTitle)
	return strings.Contains(lower, "verify") ||
		strings.Contains(lower, "security") ||
		strings.Contains(lower, "code") ||
		strings.Contains(lower, "confirm")
}

func (c *Connector) handle2FA(ctx context.Context) error {
	pref := auth.TwoFAPreferences(serviceName)

	if pref.Primary == connector.TwoFAPasskey && c.passkey != nil {
		if err := c.handlePasskey(ctx); err == nil {
			return nil
		}
		log.Printf("[revolut] passkey 2FA failed, trying SMS fallback")
	}

	if pref.Secondary == connector.TwoFASMS && c.sms != nil {
		if err := c.handleSMS(ctx); err == nil {
			return nil
		}
		log.Printf("[revolut] SMS 2FA failed, trying email fallback")
	}

	if pref.Fallback == connector.TwoFAEmail && c.email != nil {
		if err := c.handleEmail(ctx); err == nil {
			return nil
		}
	}

	code, err := c.auth.ResolveChallenge(ctx, serviceName, pref)
	if err != nil {
		return err
	}
	return c.submit2FACode(ctx, code)
}

func (c *Connector) handlePasskey(ctx context.Context) error {
	if err := auth.HandlePasskeyAuthentication(ctx, c.passkey); err != nil {
		return fmt.Errorf("revolut: passkey setup: %w", err)
	}

	_ = c.sh.Click(ctx, "passkey_btn")
	time.Sleep(3 * time.Second)

	log.Printf("[revolut] passkey authentication triggered - virtual authenticator will auto-respond")
	return nil
}

func (c *Connector) handleSMS(ctx context.Context) error {
	code, err := c.sms.WaitForCode(ctx, c.cfg.Phone, 120*time.Second)
	if err != nil {
		return fmt.Errorf("revolut: wait for sms code: %w", err)
	}
	return c.submit2FACode(ctx, code)
}

func (c *Connector) handleEmail(ctx context.Context) error {
	code, err := c.email.WaitForCode(ctx, c.cfg.Email, 120*time.Second)
	if err != nil {
		return fmt.Errorf("revolut: wait for email code: %w", err)
	}
	return c.submit2FACode(ctx, code)
}

func (c *Connector) submit2FACode(ctx context.Context, code string) error {
	if err := c.sh.WaitVisible(ctx, "sms_code_input"); err != nil {
		return fmt.Errorf("revolut: 2fa input not visible: %w", err)
	}
	if err := c.sh.SendKeys(ctx, "sms_code_input", code); err != nil {
		return fmt.Errorf("revolut: enter 2fa code: %w", err)
	}
	if err := c.sh.Click(ctx, "sms_submit"); err != nil {
		return fmt.Errorf("revolut: submit 2fa: %w", err)
	}
	time.Sleep(2 * time.Second)
	return nil
}

func (c *Connector) checkLoginSuccess(ctx context.Context) (bool, error) {
	var currentURL string
	if err := browser.RunInto(ctx, chromedp.Location(), &currentURL); err != nil {
		return false, err
	}
	return !strings.Contains(currentURL, "login") &&
		!strings.Contains(currentURL, "verify") &&
		!strings.Contains(currentURL, "authenticate"), nil
}

func (c *Connector) maybeRegisterPasskey(ctx context.Context) error {
	if c.passkey == nil {
		return nil
	}

	exists, err := c.sh.Exists(ctx, "passkey_create_btn")
	if err != nil {
		return fmt.Errorf("revolut: detect passkey enrollment prompt: %w", err)
	}
	if !exists {
		return nil
	}

	host, err := currentHost(ctx)
	if err != nil {
		return fmt.Errorf("revolut: determine current host: %w", err)
	}

	if c.passkey.HasCredentialsForHost(host) {
		_, err := c.sh.MaybeClick(ctx, "passkey_skip_btn")
		if err != nil {
			return fmt.Errorf("revolut: skip duplicate passkey prompt: %w", err)
		}
		log.Printf("[revolut] passkey already exists for host %s; skipping enrollment prompt", host)
		return nil
	}

	registered, err := auth.RegisterPasskey(ctx, c.passkey, func() error {
		return c.sh.Click(ctx, "passkey_create_btn")
	})
	if err != nil {
		_, _ = c.sh.MaybeClick(ctx, "passkey_skip_btn")
		return fmt.Errorf("revolut: register passkey: %w", err)
	}
	if registered {
		log.Printf("[revolut] passkey registered for host %s", host)
	}
	return nil
}

func currentHost(ctx context.Context) (string, error) {
	var currentURL string
	if err := browser.RunInto(ctx, chromedp.Location(), &currentURL); err != nil {
		return "", err
	}

	parsed, err := neturl.Parse(currentURL)
	if err != nil {
		return "", err
	}
	return parsed.Hostname(), nil
}

func (c *Connector) IsLoggedIn(ctx context.Context) (bool, error) {
	if !c.loggedIn {
		return false, nil
	}
	browserCtx, cancel := c.browser.NewContext(ctx)
	defer cancel()

	if err := c.sh.Navigate(browserCtx, balanceURL); err != nil {
		return false, nil
	}
	time.Sleep(2 * time.Second)

	var currentURL string
	if err := browser.RunInto(browserCtx, chromedp.Location(), &currentURL); err != nil {
		return false, nil
	}
	return !strings.Contains(currentURL, "login"), nil
}

func (c *Connector) Logout(ctx context.Context) error {
	browserCtx, cancel := c.browser.NewContext(ctx)
	defer cancel()

	if err := c.sh.Click(browserCtx, "logout_btn"); err != nil {
		return fmt.Errorf("revolut: click logout: %w", err)
	}
	c.loggedIn = false
	log.Printf("[revolut] logged out")
	return nil
}

func (c *Connector) GetTransactions(ctx context.Context, opts connector.TransactionQuery) ([]models.Transaction, error) {
	if !c.loggedIn {
		return nil, fmt.Errorf("revolut: not logged in")
	}

	browserCtx, cancel := c.browser.NewContext(ctx)
	defer cancel()

	if err := c.sh.Navigate(browserCtx, activityURL); err != nil {
		return nil, fmt.Errorf("revolut: navigate to transactions: %w", err)
	}
	time.Sleep(3 * time.Second)

	var txHTML string
	if err := browser.RunInto(browserCtx, chromedp.InnerHTML(chromedp.CSS("main")), &txHTML); err != nil {
		return nil, fmt.Errorf("revolut: read transactions page: %w", err)
	}

	transactions := parseTransactions(txHTML)

	if opts.Currency != "" {
		filtered := make([]models.Transaction, 0, len(transactions))
		for _, tx := range transactions {
			if tx.Currency == opts.Currency {
				filtered = append(filtered, tx)
			}
		}
		transactions = filtered
	}
	if opts.Limit > 0 && len(transactions) > opts.Limit {
		transactions = transactions[:opts.Limit]
	}

	return transactions, nil
}

func parseTransactions(html string) []models.Transaction {
	var transactions []models.Transaction
	itemRegex := regexp.MustCompile(`(?i)(transaction-item|transfer-item|tx-item)`)
	items := itemRegex.FindAllStringIndex(html, -1)

	for range items {
		tx := models.Transaction{
			ConnectorID: serviceName,
			Status:      models.TxStatusCompleted,
			Timestamp:   time.Now(),
			RawData:     make(map[string]any),
		}
		transactions = append(transactions, tx)
	}

	if len(transactions) == 0 {
		transactions = append(transactions, models.Transaction{
			ConnectorID: serviceName,
			Status:      models.TxStatusPending,
			Timestamp:   time.Now(),
			RawData:     map[string]any{"html_preview": html[:min(500, len(html))]},
		})
	}
	return transactions
}

func (c *Connector) SendPayment(ctx context.Context, req models.SendPaymentRequest) (*models.SendPaymentResult, error) {
	if !c.loggedIn {
		return nil, fmt.Errorf("revolut: not logged in")
	}

	browserCtx, cancel := c.browser.NewContext(ctx)
	defer cancel()

	if err := c.sh.Navigate(browserCtx, sendURL); err != nil {
		return nil, fmt.Errorf("revolut: navigate to send: %w", err)
	}
	time.Sleep(2 * time.Second)

	if err := c.sh.WaitVisible(browserCtx, "send_recipient"); err != nil {
		return nil, fmt.Errorf("revolut: recipient field not visible: %w", err)
	}

	if err := c.sh.SendKeys(browserCtx, "send_recipient", req.Recipient); err != nil {
		return nil, fmt.Errorf("revolut: enter recipient: %w", err)
	}

	amountStr := strconv.FormatFloat(req.Amount, 'f', 2, 64)
	if err := c.sh.SendKeys(browserCtx, "send_amount", amountStr); err != nil {
		return nil, fmt.Errorf("revolut: enter amount: %w", err)
	}

	if err := c.sh.Click(browserCtx, "send_submit"); err != nil {
		return nil, fmt.Errorf("revolut: click send: %w", err)
	}
	time.Sleep(2 * time.Second)

	if err := c.sh.WaitVisible(browserCtx, "send_confirm"); err != nil {
		return nil, fmt.Errorf("revolut: confirm button not visible: %w", err)
	}

	if err := c.sh.Click(browserCtx, "send_confirm"); err != nil {
		return nil, fmt.Errorf("revolut: confirm payment: %w", err)
	}
	time.Sleep(3 * time.Second)

	log.Printf("[revolut] sent %.2f %s to %s", req.Amount, req.Currency, req.Recipient)

	return &models.SendPaymentResult{
		TransactionID: fmt.Sprintf("revolut-send-%d", time.Now().UnixMilli()),
		Status:        models.TxStatusCompleted,
		Timestamp:     time.Now(),
	}, nil
}

func (c *Connector) GetBalance(ctx context.Context, currency string) (*models.Balance, error) {
	if !c.loggedIn {
		return nil, fmt.Errorf("revolut: not logged in")
	}

	browserCtx, cancel := c.browser.NewContext(ctx)
	defer cancel()

	if err := c.sh.Navigate(browserCtx, balanceURL); err != nil {
		return nil, fmt.Errorf("revolut: navigate to dashboard: %w", err)
	}
	time.Sleep(3 * time.Second)

	var balanceText string
	if err := c.sh.GetText(browserCtx, "balance_amount", &balanceText); err != nil {
		return nil, fmt.Errorf("revolut: read balance: %w", err)
	}

	amount, err := parseAmount(balanceText)
	if err != nil {
		return nil, fmt.Errorf("revolut: parse balance %q: %w", balanceText, err)
	}

	cur := currency
	if cur == "" {
		cur = "EUR"
	}

	return &models.Balance{
		Currency:  cur,
		Amount:    amount,
		Available: amount,
	}, nil
}

func parseAmount(s string) (float64, error) {
	s = strings.TrimSpace(s)
	s = regexp.MustCompile(`[^\d.,-]`).ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, ",", "")
	return strconv.ParseFloat(s, 64)
}

func (c *Connector) Close() error {
	c.loggedIn = false
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
