package paypal

import (
	"dexFiatController/internal/scraping/browser"
)

func RegisterSelectors(r *browser.SelectorRegistry) {
	r.Register("login_email", "#email", "PayPal login email input field",
		"#login_email", "input[name='login_email']", "input[type='email']", "#email")

	r.Register("login_password", "#password", "PayPal login password input field",
		"#login_password", "input[name='login_password']", "input[type='password']", "#password")

	r.Register("login_btn", "#btnLogin", "PayPal login submit button",
		"button[name='btnLogin']", "input[type='submit']", "#btnLogin", "button[data-testid='login-button']")

	r.Register("login_next_btn", "#btnNext", "PayPal 'Next' button after email entry",
		"button[name='btnNext']", "#btnNext", "button[data-testid='next-button']")

	r.Register("totp_input", "#verificationCode", "PayPal TOTP verification code input",
		"input[name='verificationCode']", "input[autocomplete='one-time-code']", "#verificationCode",
		"input[type='tel']", "input[data-testid='otp-input']")

	r.Register("totp_submit", "#verifyCodeBtn", "PayPal TOTP submit/verify button",
		"button[name='verifyCodeBtn']", "#verifyCodeBtn", "button[data-testid='verify-button']",
		"input[type='submit']")

	r.Register("activity_link", "a[data-testid='activity-link']", "PayPal activity/transactions navigation link",
		"a[href*='/activity']", "a[data-testid='activity-link']", "#activityLink",
		"nav a[href*='activity']", "a[aria-label='Activity']")

	r.Register("send_link", "a[data-testid='send-link']", "PayPal send money navigation link",
		"a[href*='/myaccount/transfer']", "a[data-testid='send-link']", "#sendLink",
		"nav a[href*='transfer']", "a[aria-label='Send']")

	r.Register("send_recipient", "#recipient", "PayPal send money recipient input",
		"input[name='recipient']", "input[type='email']", "input[placeholder*='email']",
		"input[data-testid='recipient-input']")

	r.Register("send_amount", "#amount", "PayPal send money amount input",
		"input[name='amount']", "input[type='number']", "input[data-testid='amount-input']")

	r.Register("send_currency", "#currency", "PayPal send money currency selector",
		"select[name='currency']", "select[data-testid='currency-select']")

	r.Register("send_submit", "#sendButton", "PayPal send money submit button",
		"button[data-testid='send-button']", "button[type='submit']")

	r.Register("send_confirm", "#confirmButton", "PayPal send money confirm button",
		"button[data-testid='confirm-button']", "button[type='submit']")

	r.Register("transaction_list", ".transaction-item", "PayPal transaction list items",
		"[data-testid='transaction-item']", ".activity-line-item", ".transactionRow")

	r.Register("transaction_date", ".transaction-date", "Transaction date element",
		"[data-testid='transaction-date']", ".date", "time")

	r.Register("transaction_desc", ".transaction-desc", "Transaction description element",
		"[data-testid='transaction-desc']", ".description", ".name")

	r.Register("transaction_amount", ".transaction-amount", "Transaction amount element",
		"[data-testid='transaction-amount']", ".amount", ".value")

	r.Register("balance_amount", ".balance-amount", "PayPal account balance display",
		"[data-testid='balance-amount']", ".balance", ".available-balance")

	r.Register("logout_btn", "a[data-testid='logout-link']", "PayPal logout link",
		"a[href*='/signin/logout']", "a[data-testid='logout-link']", "button[aria-label='Log out']")

	r.Register("passkey_btn", "button[data-testid='passkey-login']", "PayPal passkey login button",
		"button:has-text('Passkey')", "button[aria-label*='passkey']",
		"[data-testid='use-passkey']", "button:has-text('Use passkey')")

	r.Register("passkey_create_btn", "button[data-testid='create-passkey']", "PayPal create passkey button",
		"button:has-text('Create passkey')", "button[aria-label*='Create']",
		"[data-testid='create-passkey']")

	r.Register("passkey_skip_btn", "button[data-testid='skip-passkey']", "PayPal skip passkey setup button",
		"button:has-text('Skip')", "button:has-text('Not now')",
		"[data-testid='skip-passkey']", "button[aria-label='Skip']")

	r.Register("totp_choice_btn", "button[data-testid='choose-totp']", "PayPal choose authenticator app 2FA button",
		"button:has-text('Authenticator')", "a:has-text('authenticator')",
		"[data-testid='method-authenticator']")
}
