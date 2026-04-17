package revolut

import (
	"dexFiatController/internal/browser"
)

func RegisterSelectors(r *browser.SelectorRegistry) {
	r.Register("login_email", "input[name='email']", "Revolut login email/phone input field",
		"input[type='email']", "input[type='tel']", "#email",
		"input[data-testid='login-email']", "input[autocomplete='email']")

	r.Register("login_password", "input[name='password']", "Revolut login password input field",
		"input[type='password']", "#password", "input[data-testid='login-password']")

	r.Register("login_btn", "button[type='submit']", "Revolut login submit button",
		"button[data-testid='login-button']", "input[type='submit']")

	r.Register("sms_code_input", "input[name='code']", "Revolut SMS/email verification code input",
		"input[type='tel']", "input[autocomplete='one-time-code']",
		"input[data-testid='otp-input']", "input[placeholder*='code']")

	r.Register("sms_submit", "button[type='submit']", "Revolut verification code submit button",
		"button[data-testid='submit-button']", "input[type='submit']")

	r.Register("email_code_input", "input[name='emailCode']", "Revolut email verification code input",
		"input[type='tel']", "input[name='code']", "input[data-testid='email-otp-input']")

	r.Register("activity_link", "a[href='/transactions']", "Revolut transactions navigation link",
		"a[data-testid='transactions-link']", "nav a[href*='transactions']",
		"a[aria-label='Transactions']", "[data-testid='nav-transactions']")

	r.Register("send_link", "a[href='/payments/new']", "Revolut send money navigation link",
		"a[data-testid='send-link']", "nav a[href*='payments/new']",
		"button[aria-label='Send']", "[data-testid='nav-send']")

	r.Register("send_recipient", "input[name='recipient']", "Revolut send money recipient input",
		"input[type='email']", "input[type='tel']", "input[data-testid='recipient-input']",
		"input[placeholder*='name']", "input[placeholder*='email']")

	r.Register("send_amount", "input[name='amount']", "Revolut send money amount input",
		"input[type='number']", "input[data-testid='amount-input']",
		"input[placeholder*='amount']")

	r.Register("send_currency", "select[name='currency']", "Revolut send money currency selector",
		"select[data-testid='currency-select']")

	r.Register("send_submit", "button[type='submit']", "Revolut send money submit button",
		"button[data-testid='continue-button']", "button:has-text('Continue')")

	r.Register("send_confirm", "button[data-testid='confirm-button']", "Revolut send money confirm button",
		"button[type='submit']", "button:has-text('Confirm')")

	r.Register("transaction_list", ".transaction-item", "Revolut transaction list items",
		"[data-testid='transaction-item']", ".transfer-item", ".tx-item")

	r.Register("transaction_date", ".transaction-date", "Transaction date element",
		"[data-testid='transaction-date']", "time", ".date")

	r.Register("transaction_desc", ".transaction-description", "Transaction description element",
		"[data-testid='transaction-desc']", ".description", ".title")

	r.Register("transaction_amount", ".transaction-amount", "Transaction amount element",
		"[data-testid='transaction-amount']", ".amount", ".value")

	r.Register("balance_amount", ".balance-value", "Revolut account balance display",
		"[data-testid='balance-amount']", ".balance", ".available-balance")

	r.Register("logout_btn", "button[aria-label='Log out']", "Revolut logout button",
		"a[data-testid='logout-link']", "a[href='/logout']", "[data-testid='sign-out']")

	r.Register("passkey_btn", "button[data-testid='passkey-login']", "Revolut passkey login button",
		"button:has-text('Passkey')", "button[aria-label*='passkey']",
		"[data-testid='use-passkey']", "button:has-text('Use passkey')")

	r.Register("passkey_create_btn", "button[data-testid='create-passkey']", "Revolut create passkey button",
		"button:has-text('Create passkey')", "button[aria-label*='Create']",
		"[data-testid='create-passkey']")

	r.Register("passkey_skip_btn", "button[data-testid='skip-passkey']", "Revolut skip passkey setup button",
		"button:has-text('Skip')", "button:has-text('Not now')",
		"[data-testid='skip-passkey']", "button[aria-label='Skip']")
}
