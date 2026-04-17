package wise

import (
	"dexFiatController/internal/browser"
)

func RegisterSelectors(r *browser.SelectorRegistry) {
	r.Register("login_email", "input[name='email']", "Wise login email input field",
		"input[type='email']", "#email", "input[data-testid='email-input']")

	r.Register("login_password", "input[name='password']", "Wise login password input field",
		"input[type='password']", "#password", "input[data-testid='password-input']")

	r.Register("login_btn", "button[type='submit']", "Wise login submit button",
		"button[data-testid='login-button']", "input[type='submit']", ".btn-login")

	r.Register("totp_input", "input[name='code']", "Wise TOTP/2FA verification code input",
		"input[autocomplete='one-time-code']", "input[type='tel']", "input[data-testid='otp-input']",
		"input[name='verificationCode']", "input[placeholder*='code']")

	r.Register("totp_submit", "button[type='submit']", "Wise TOTP submit button",
		"button[data-testid='submit-button']", "input[type='submit']")

	r.Register("sms_code_input", "input[name='smsCode']", "Wise SMS verification code input",
		"input[type='tel']", "input[name='code']", "input[data-testid='sms-input']")

	r.Register("activity_link", "a[href='/activity']", "Wise activity/transactions navigation link",
		"a[data-testid='activity-link']", "nav a[href*='activity']", "a[aria-label='Activity']")

	r.Register("send_link", "a[href='/transfer']", "Wise send money navigation link",
		"a[data-testid='send-link']", "nav a[href*='transfer']", "a[aria-label='Send money']")

	r.Register("send_recipient", "input[name='recipient']", "Wise send money recipient input",
		"input[type='email']", "input[data-testid='recipient-input']",
		"input[placeholder*='recipient']")

	r.Register("send_amount", "input[name='amount']", "Wise send money amount input",
		"input[type='number']", "input[data-testid='amount-input']",
		"input[placeholder*='amount']")

	r.Register("send_currency", "select[name='sourceCurrency']", "Wise send money source currency selector",
		"select[data-testid='source-currency']", "select[name='currency']")

	r.Register("send_target_currency", "select[name='targetCurrency']", "Wise send money target currency selector",
		"select[data-testid='target-currency']")

	r.Register("send_submit", "button[type='submit']", "Wise send money submit button",
		"button[data-testid='continue-button']", "button span:text('Continue')")

	r.Register("send_confirm", "button[data-testid='confirm-button']", "Wise send money confirm button",
		"button[type='submit']", ".btn-confirm")

	r.Register("transaction_list", ".transaction-list-item", "Wise transaction list items",
		"[data-testid='transaction-item']", ".transfer-item", ".activity-item")

	r.Register("transaction_date", ".transaction-date", "Transaction date element",
		"[data-testid='transaction-date']", "time", ".date")

	r.Register("transaction_desc", ".transaction-description", "Transaction description element",
		"[data-testid='transaction-desc']", ".description", ".title")

	r.Register("transaction_amount", ".transaction-amount", "Transaction amount element",
		"[data-testid='transaction-amount']", ".amount", ".value")

	r.Register("balance_amount", ".balance-value", "Wise account balance display",
		"[data-testid='balance-amount']", ".balance", ".available-balance")

	r.Register("logout_btn", "a[href='/logout']", "Wise logout link",
		"a[data-testid='logout-link']", "button[aria-label='Log out']")

	r.Register("choose_totp_method", "button[data-testid='choose-authenticator']", "Wise choose authenticator app 2FA method button",
		"button:has-text('Authenticator')", "a:has-text('authenticator')",
		"[data-testid='method-authenticator']")

	r.Register("passkey_btn", "button[data-testid='passkey-login']", "Wise passkey login button",
		"button:has-text('Passkey')", "button[aria-label*='passkey']",
		"[data-testid='use-passkey']", "button:has-text('Use passkey')")

	r.Register("passkey_create_btn", "button[data-testid='create-passkey']", "Wise create passkey button",
		"button:has-text('Create passkey')", "button[aria-label*='Create']",
		"[data-testid='create-passkey']")

	r.Register("passkey_skip_btn", "button[data-testid='skip-passkey']", "Wise skip passkey setup button",
		"button:has-text('Skip')", "button:has-text('Not now')",
		"[data-testid='skip-passkey']", "button[aria-label='Skip']")
}
