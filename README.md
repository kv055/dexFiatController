# dexFiatController

A Go-based fiat on-ramp/off-ramp controller for the dexArbTrading platform. Automates login, transaction monitoring, and payment sending across **PayPal**, **Wise**, and **Revolut** via headless browser automation. Handles multi-factor authentication automatically with cascading fallback through passkeys, TOTP, SMS, and email.

## Architecture Overview

```
main.go
  |
  v
FiatController (internal/controller)
  |
  +-- PayPal Connector (internal/connector/paypal)
  +-- Wise Connector  (internal/connector/wise)
  +-- Revolut Connector (internal/connector/revolut)
  |
  +-- Browser Instance (internal/browser)
  |     +-- Stagehand (self-healing CSS selectors)
  |
  +-- Auth (internal/auth)
        +-- PasskeyManager (CDP WebAuthn virtual authenticator)
        +-- TOTPClient (HTTP client to totp-api service)
        +-- SMSProvider (Twilio / Mock)
        +-- EmailProvider (IMAP / Mock)
        +-- Resolver (2FA cascade orchestration)

[TOTP API Microservice] (services/totp-api)
  Standalone HTTP server generating TOTP codes
  Called by TOTPClient over the Docker network
```

## Directory Structure

```
dexFiatController/
├── main.go                                  Entry point
├── Dockerfile                               Multi-stage Docker build
├── go.mod / go.sum                          Go module (dexFiatController)
├── .env.example                             Environment variable template
├── .gitignore
│
├── internal/
│   ├── models/
│   │   └── models.go                        Data models: Transaction, Balance, SendPayment, ConnectorConfig
│   │
│   ├── browser/
│   │   ├── browser.go                        Chromedp browser instance wrapper
│   │   └── stagehand.go                      Self-healing selector system (Stagehand v3 style)
│   │
│   ├── auth/
│   │   ├── passkey.go                        CDP WebAuthn virtual authenticator manager
│   │   ├── totp.go                           HTTP client for the TOTP API microservice
│   │   ├── sms.go                            SMS providers (Twilio, Mock)
│   │   ├── email.go                          Email providers (IMAP, Mock)
│   │   └── twofa.go                          2FA resolver: cascade through passkey → TOTP → SMS → email
│   │
│   ├── connector/
│   │   └── connector.go                     Connector interface, TwoFAMethod, TwoFAPreference types
│   │
│   ├── connector/paypal/
│   │   ├── selectors.go                      CSS selector registry for PayPal pages
│   │   └── connector.go                      PayPal connector implementation
│   │
│   ├── connector/wise/
│   │   ├── selectors.go                      CSS selector registry for Wise pages
│   │   └── connector.go                      Wise connector implementation
│   │
│   ├── connector/revolut/
│   │   ├── selectors.go                      CSS selector registry for Revolut pages
│   │   └── connector.go                      Revolut connector implementation
│   │
│   └── controller/
│       └── controller.go                     FiatController: orchestrates all connectors
│
└── services/
    └── totp-api/
        ├── main.go                           Standalone TOTP HTTP microservice
        ├── Dockerfile                         Multi-stage Docker build
        └── go.mod / go.sum                    Separate Go module
```

---

## Module Reference

### `main.go` — Entry Point

The entry point does three things:

1. **Configuration loading** — reads all environment variables and constructs a `controller.Config`. This includes creating the `PasskeyManager`, `TOTPClient`, `SMSProvider`, and `EmailProvider` instances based on which env vars are set.

2. **Controller initialization** — calls `controller.New(cfg)` which creates the shared browser instance and initializes all connectors whose email is configured (non-empty `PAYPAL_EMAIL`, `WISE_EMAIL`, `REVOLUT_EMAIL`).

3. **Runtime loop** — logs in to all connectors, starts the transaction monitor in a goroutine, and blocks on OS signals (`SIGINT`, `SIGTERM`) for graceful shutdown.

**Environment variables loaded by `main.go`:**

| Variable | Default | Purpose |
|---|---|---|
| `DATABASE_URL` | `postgres://...` | Shared Postgres database used for TOTP and passkey secret storage |
| `HEADLESS` | `true` | Run Chrome in headless mode |
| `CHROME_PATH` | `""` | Override Chrome binary path |
| `OPENAI_API_KEY` | `""` | For Stagehand AI self-healing |
| `TOTP_API_URL` | `http://totp-api:8080` | TOTP API service URL |
| `TOTP_API_KEY` | `""` | API key for TOTP service authentication |
| `PAYPAL_EMAIL` | `""` | PayPal login email |
| `PAYPAL_PASSWORD` | `""` | PayPal login password |
| `PAYPAL_TOTP_SECRET` | `""` | Optional bootstrap value that is written into Postgres as the active PayPal TOTP secret |
| `WISE_EMAIL` | `""` | Wise login email |
| `WISE_PASSWORD` | `""` | Wise login password |
| `WISE_TOTP_SECRET` | `""` | Optional bootstrap value that is written into Postgres as the active Wise TOTP secret |
| `WISE_PHONE` | `""` | Wise phone number (for SMS 2FA) |
| `REVOLUT_EMAIL` | `""` | Revolut login email |
| `REVOLUT_PASSWORD` | `""` | Revolut login password |
| `REVOLUT_PHONE` | `""` | Revolut phone number (for SMS 2FA) |
| `SMS_PROVIDER` | `""` | `twilio` to enable Twilio SMS |
| `SMS_ACCOUNT_SID` | `""` | Twilio Account SID |
| `SMS_AUTH_TOKEN` | `""` | Twilio Auth Token |
| `SMS_PHONE_NUMBER` | `""` | Twilio phone number |
| `EMAIL_PROVIDER` | `""` | `imap` to enable IMAP email |
| `EMAIL_IMAP_HOST` | `""` | IMAP server hostname |
| `EMAIL_IMAP_USER` | `""` | IMAP username |
| `EMAIL_IMAP_PASS` | `""` | IMAP password |
| `POLL_INTERVAL` | `30s` | Transaction polling interval |
| `PAYMENT_NOTIFICATIONS_API_URL` | `""` | Endpoint on `dexUnifiedApi` that accepts connector payment events |
| `PAYMENT_NOTIFICATIONS_SHARED_SECRET` | `""` | Optional webhook secret sent as `X-Webhook-Secret` |
| `PAYMENT_NOTIFICATIONS_DEX` | `""` | Default DEX label attached when a connector does not provide one in metadata |

---

### `internal/models` — Data Models

Defines the shared data types used across all connectors and the controller:

**`Transaction`** — A single financial transaction:
- `ID`, `ConnectorID` — unique and source identifiers
- `Direction` — `inbound` or `outbound`
- `Status` — `pending`, `completed`, `failed`, or `cancelled`
- `Amount`, `Currency` — monetary value
- `Counterparty`, `Description`, `Reference` — contextual metadata
- `Timestamp` — when the transaction occurred
- `RawData` — arbitrary key-value map for connector-specific data

**`SendPaymentRequest`** — Request to send money:
- `Amount`, `Currency`, `Recipient`, `Description`, `Reference`

**`SendPaymentResult`** — Result of a send payment:
- `TransactionID`, `Status`, `Timestamp`

**`Balance`** — Account balance snapshot:
- `Currency`, `Amount`, `Available`, `Hold`

**`ConnectorConfig`** — Per-connector credentials:
- `Email`, `Password`, `TOTPSecret`, `Phone`, `Headless`

---

### Payment Notifications

`dexFiatController` now includes a shared notifier in `internal/notifications` that every connector-facing flow can reuse.

- Payloads are sent as JSON to `dexUnifiedApi`
- Standard fields: `amount`, `currency`, `type`, `transaction_id`, `dex`, `connector`, `transfer_message`
- `metadata` carries connector-specific unstructured fields such as reference, counterparty, status, and raw parsed fields
- Observed transactions from the polling loop are forwarded automatically
- Direct `SendPayment(...)` calls also forward an outgoing payment notification immediately

---

### `internal/browser` — Browser Automation

#### `browser.go` — Chrome Instance Wrapper

Wraps `chromedp` to manage a shared Chrome browser process.

**`Config`** struct:
- `Headless` — run Chrome without a visible window
- `ChromePath` — override the Chrome binary location
- `Timeout` — default action timeout (30s)

**`Instance`** lifecycle:
1. `NewInstance(ctx, cfg)` — creates a chromedp exec allocator with flags:
   - `headless`, `disable-gpu`, `no-sandbox`, `disable-dev-shm-usage`
   - Window size `1920x1080`
   - Custom Windows Chrome user-agent string
   - If `ChromePath` is set, uses `chromedp.ExecPath`
2. `NewContext(parent)` — creates a new browser context (tab) with logging
3. `NewTab(parent)` — creates an additional tab in the same browser
4. `Close()` — tears down the allocator, killing the Chrome process

The shared `Instance` is created once by the `FiatController` and passed to all connectors. Each connector operation creates its own browser context (tab) using `NewContext`, which is deferred-cancel when the operation completes.

#### `stagehand.go` — Self-Healing Selector System

Implements a **Stagehand v3** style self-healing mechanism. When a CSS selector breaks because a website updated its frontend, Stagehand automatically recovers using fallback chains and AI-powered healing.

**`SelectorEntry`** — A registered selector with metadata:
- `CSS` — the current primary CSS selector
- `Description` — human-readable description of the target element
- `FallbackChain` — ordered list of alternative selectors to try
- `LastVerifiedAt` — timestamp of last successful use
- `HealedFrom` — how the selector was last healed (`"fallback"` or `"ai"`)

**`SelectorRegistry`** — Thread-safe map of `key → SelectorEntry`:
- `Register(key, css, description, fallbacks...)` — registers a selector with its fallback chain
- `Get(key)` — returns a copy of the entry
- `UpdateHealed(key, newCSS, healedFrom)` — demotes the current CSS to a fallback and promotes `newCSS` as primary

**`Stagehand`** struct — the high-level automation API:
- `Click(ctx, selectorKey)` — click an element by key
- `SendKeys(ctx, selectorKey, value)` — type into an input by key
- `GetValue(ctx, selectorKey, result)` — read an input value by key
- `GetText(ctx, selectorKey, result)` — read element text content by key
- `WaitVisible(ctx, selectorKey)` — wait until element is visible
- `WaitReady(ctx, selectorKey)` — wait until element is in the DOM
- `Navigate(ctx, url)` — navigate to a URL
- `Execute(ctx, actions...)` — run raw chromedp actions

**Healing algorithm** (`executeWithHealing`):

1. Look up the selector entry by key. If not found, error immediately.
2. Try the primary CSS selector. If it works, return.
3. Try each fallback selector in order. If one works, promote it via `UpdateHealed(key, workingSelector, "fallback")` and return.
4. If all selectors fail and OpenAI is configured:
   a. Capture a DOM snapshot by evaluating JavaScript that walks the DOM tree (max depth 6), recording tag names, IDs, classes, attributes (`type`, `role`, `aria-label`, `placeholder`), and text content.
   b. Truncate the snapshot to 12,000 characters.
   c. Send the snapshot to OpenAI (GPT-4o-mini) with a prompt that describes the broken selector and asks for a new one. The prompt enforces: prefer ID selectors, then attribute selectors, then class selectors; keep selectors short and stable.
   d. The AI returns a CSS selector string. It is validated against injection patterns (`javascript:`, `<script>`, `eval`, `fetch(`, etc.) and length limits.
   e. If valid, promote the AI-suggested selector via `UpdateHealed(key, newSelector, "ai")` and execute the action with it.
5. If healing also fails, return an error.

**`StagehandConfig`**:
- `OpenAIKey` — API key for AI healing (if empty, AI healing is disabled)
- `Model` — OpenAI model to use (default: `gpt-4o-mini`)
- `MaxRetries` — number of retry attempts (default: 2)
- `HealTimeout` — timeout for a single AI healing request (default: 15s)

---

### `internal/connector` — Connector Interface

**`Connector`** interface — the contract every service connector must implement:

| Method | Description |
|---|---|
| `Name() string` | Returns the service name (`"paypal"`, `"wise"`, `"revolut"`) |
| `Login(ctx) error` | Logs into the service, handling 2FA |
| `IsLoggedIn(ctx) (bool, error)` | Checks if the session is still valid |
| `Logout(ctx) error` | Logs out of the service |
| `GetTransactions(ctx, opts) ([]Transaction, error)` | Fetches recent transactions |
| `SendPayment(ctx, req) (*SendPaymentResult, error)` | Sends money to a recipient |
| `GetBalance(ctx, currency) (*Balance, error)` | Reads account balance |
| `Close() error` | Cleans up connector resources |

**`TransactionQuery`** — filtering for `GetTransactions`:
- `Currency` — filter by currency code
- `Direction` — filter by inbound/outbound
- `Limit` / `Offset` — pagination

**`TwoFAMethod`** — enum for 2FA types: `none`, `totp`, `passkey`, `sms`, `email`, `call`

**`TwoFAPreference`** — ordered preference for a service:
- `Primary` — preferred method (always `passkey` for all three services)
- `Secondary` — second choice
- `Fallback` — last resort

---

### `internal/auth` — Authentication & 2FA

#### `passkey.go` — CDP WebAuthn Virtual Authenticator

The core passkey automation engine. Uses Chrome DevTools Protocol's **WebAuthn domain** to create a virtual authenticator that automatically responds to WebAuthn challenges without any user interaction.

**`PasskeyManager`** — manages virtual authenticators per browser context:
- `authenticators` — map of `contextKey → AuthenticatorID`, one per chromedp browser context
- `privateKeys` — map of `rpID → ECDSA private key` for credential signing
- `credentials` — map of `rpID → []*PasskeyCredential` for re-provisioning to new contexts
- `store` — pluggable persistence backend (Postgres in the normal deployment)

**Why per-context?** Each connector operation creates a fresh browser context via `browser.NewContext()`. CDP WebAuthn commands are scoped to the browser context they are sent on. The `PasskeyManager` therefore tracks one virtual authenticator per context, keyed by `chromedp.FromContext(ctx).Target.TargetID`.

**`Enable(ctx)`** — creates a virtual authenticator in the given browser context:
1. Enables the WebAuthn CDP domain via `webauthn.Enable()`
2. Adds a virtual authenticator with options:
   - `Protocol: ctap2`, `Ctap2version: ctap2_1`
   - `Transport: internal`
   - `HasResidentKey: true`, `HasUserVerification: true`, `IsUserVerified: true`
   - `AutomaticPresenceSimulation: true` — the authenticator is "always present", so WebAuthn ceremonies complete instantly
   - `DefaultBackupEligibility: true`, `DefaultBackupState: true` — signals to the relying party that the credential is synced (as a real passkey would be)
3. Re-provisions all previously added credentials to the new authenticator (so credentials survive across browser contexts)

**`Disable(ctx)`** — removes the virtual authenticator for a context and disables the WebAuthn domain.

**`SetUserVerified(ctx, verified)`** — signals to the virtual authenticator whether the user has been verified. Called with `true` before a WebAuthn authentication ceremony.

**`AddCredential(ctx, cred)`** — adds a specific WebAuthn credential to the virtual authenticator for the current context. The credential's private key is marshaled to PKCS8, base64-encoded, and sent via `webauthn.AddCredential()`. Also stored in the `credentials` map for re-provisioning to future contexts and persisted via the configured store backend.

**`GetCredentials(ctx)`**, **`RemoveCredential(ctx, id)`**, **`ClearCredentials(ctx)`** — manage credentials on the virtual authenticator and keep the persisted credential store in sync.

**`SyncCredentialsFromAuthenticator(ctx)`** — imports any credentials that were created by a live `navigator.credentials.create()` registration flow inside Chrome's virtual authenticator, merging them into the in-memory store and persistent backend.

**`RegisterPasskey(ctx, pm, trigger)`** — helper for initial enrollment. It snapshots the current credential set, runs the UI action that triggers passkey creation, waits for the browser to create the new WebAuthn credential, then syncs that credential into the controller's persistent store.

**Persistence model** — on startup, `NewPasskeyManager(ctx, store)` loads any previously saved credentials from the configured backend into memory. Both TOTP secrets and passkey credentials live in the shared Postgres schema managed by `dexUnifiedApi`.

**`PasskeyCredential`** — a WebAuthn credential:
- `CredentialID` — random 32-byte identifier
- `PrivateKey` — ECDSA P-256 key pair
- `RPID` — relying party ID (e.g. `"paypal.com"`)
- `UserHandle` — random 16-byte user identifier
- `SignCount` — signature counter (starts at 0)

**`GeneratePasskeyCredential(rpID, userHandle)`** — generates a random `PasskeyCredential` suitable for registration.

**`HandlePasskeyAuthentication(ctx, pm)`** — convenience function: ensures the PasskeyManager is enabled for the context and sets user verified to `true`. This is called by connectors when they encounter a passkey 2FA challenge.

**Flow during login/authentication:**
1. Connector calls `passkey.Enable(browserCtx)` right after creating the browser context, before any navigation. This ensures the virtual authenticator is ready before the page triggers a WebAuthn request.
2. When the service presents a passkey login option, the connector clicks the passkey button.
3. The browser issues a WebAuthn `navigator.credentials.get()` challenge.
4. Because `AutomaticPresenceSimulation: true`, the virtual authenticator immediately responds with a signed assertion. No user interaction is needed.

**Flow during initial passkey enrollment:**
1. After a successful password/2FA login, the connector checks whether the service shows a `create passkey` prompt.
2. If a passkey already exists for the current host, the connector clicks `skip` to avoid duplicate registration.
3. Otherwise, the connector calls `RegisterPasskey(...)` and clicks the `create passkey` button.
4. The page triggers `navigator.credentials.create()`, Chrome stores the new credential in the virtual authenticator, and `SyncCredentialsFromAuthenticator(...)` imports it into the controller's persistent store.
5. On future logins, that persisted credential is automatically re-provisioned into new browser contexts and used for passkey authentication.

#### `totp.go` — TOTP API Client

HTTP client that communicates with the standalone TOTP API microservice (`services/totp-api`).

**`TOTPClient`** struct:
- `baseURL` — the TOTP API base URL (default: `http://totp-api:8080`)
- `apiKey` — optional API key sent as `X-API-Key` header
- `client` — `http.Client` with 10s timeout

**Methods:**

| Method | Endpoint | Description |
|---|---|---|
| `Register(ctx, service, secret)` | `POST /register` | Registers a TOTP secret for a service |
| `GenerateCode(ctx, service)` | `GET /code?service=` | Generates the current TOTP code |
| `WaitForNextPeriod(ctx)` | — | Waits until the next 30s TOTP period starts |
| `ListServices(ctx)` | `GET /list` | Lists all registered services |
| `HealthCheck(ctx)` | `GET /health` | Checks if the TOTP API is healthy |

`GenerateCode` returns the 6-digit code and the number of seconds until it expires (`expires_in`).

`WaitForNextPeriod` is used when a code is about to expire (< 3 seconds remaining): it waits the remaining time plus 30 seconds to get a fresh code window.

#### `sms.go` — SMS Providers

**`SMSProvider`** interface:
- `GetLatestCode(ctx, phone, within)` — gets the most recent code received within a time window
- `WaitForCode(ctx, phone, timeout)` — blocks until a code arrives or timeout

**Implementations:**

| Provider | Description |
|---|---|
| `TwilioSMSProvider` | Twilio integration (stub — `GetLatestCode` and `WaitForCode` return `not yet implemented`) |
| `MockSMSProvider` | In-memory channel-based mock for testing. Use `InjectCode(code)` to push codes. |

**`ExtractCodeFromSMS(body)`** — regex helper that extracts the last 4-8 digit number from an SMS body.

#### `email.go` — Email Providers

**`EmailProvider`** interface — identical shape to `SMSProvider`:
- `GetLatestCode(ctx, recipient, within)`
- `WaitForCode(ctx, recipient, timeout)`

**Implementations:**

| Provider | Description |
|---|---|
| `IMAPProvider` | IMAP mailbox integration (stub — methods return `not yet implemented`) |
| `MockEmailProvider` | In-memory channel-based mock for testing. Use `InjectCode(code)` to push codes. |

**`ExtractCodeFromEmail(body)`** — regex helper that extracts the last 4-8 digit number from an email body.

#### `twofa.go` — 2FA Resolver

The `Resolver` orchestrates the cascade through multiple 2FA methods.

**`Resolver`** struct holds references to all auth providers:
- `totp *TOTPClient`
- `passkey *PasskeyManager`
- `sms SMSProvider`
- `email EmailProvider`

**`ResolveChallenge(ctx, service, pref)`** — tries each method in the preference order:
1. Try `pref.Primary` — if it succeeds, return the code
2. Try `pref.Secondary` — if it succeeds, return the code
3. Try `pref.Fallback` — if it succeeds, return the code
4. If all fail, return an error

`TwoFANone` entries are skipped.

**Method handlers:**

| Method | Action | Returns |
|---|---|---|
| `handlePasskey` | Enables the virtual authenticator and sets user verified | `"passkey:authenticated"` (the browser auto-responds to the challenge; no code needs to be entered) |
| `handleTOTP` | Calls `totp.GenerateCode(ctx, service)` | 6-digit TOTP code string |
| `handleSMS` | Calls `sms.WaitForCode(ctx, phone, 120s)` | code from SMS |
| `handleEmail` | Calls `email.WaitForCode(ctx, email, 120s)` | code from email |

**`TwoFAPreferences(service)`** — returns the hardcoded preference chain for each service:

| Service | Primary | Secondary | Fallback |
|---|---|---|---|
| PayPal | Passkey | TOTP | — |
| Wise | Passkey | TOTP | SMS |
| Revolut | Passkey | SMS | Email |
| Default | Passkey | — | — |

Passkeys are primary for all services because they are the most seamless: no codes to read, no out-of-band channels, and the virtual authenticator auto-responds instantly.

---

### `internal/connector/paypal` — PayPal Connector

#### `selectors.go` — PayPal CSS Selectors

Registers all PayPal-specific CSS selectors with the Stagehand registry. Each entry has a key, a primary CSS selector, a description, and a fallback chain.

| Key | Primary Selector | Description |
|---|---|---|
| `login_email` | `#email` | Email input on login page |
| `login_password` | `#password` | Password input |
| `login_btn` | `#btnLogin` | Login submit button |
| `login_next_btn` | `#btnNext` | Next button after email entry |
| `totp_input` | `#verificationCode` | TOTP code input field |
| `totp_submit` | `#verifyCodeBtn` | TOTP verify button |
| `totp_choice_btn` | `button[data-testid='choose-totp']` | Choose authenticator app method |
| `passkey_btn` | `button[data-testid='passkey-login']` | Passkey login button |
| `passkey_create_btn` | `button[data-testid='create-passkey']` | Create passkey button |
| `passkey_skip_btn` | `button[data-testid='skip-passkey']` | Skip passkey setup button |
| `send_recipient` | `#recipient` | Send money recipient input |
| `send_amount` | `#amount` | Send money amount input |
| `send_submit` | `#sendButton` | Send submit button |
| `send_confirm` | `#confirmButton` | Send confirm button |
| `balance_amount` | `.balance-amount` | Account balance display |
| `logout_btn` | `a[data-testid='logout-link']` | Logout link |
| `activity_link` | `a[data-testid='activity-link']` | Activity page link |
| `transaction_list` | `.transaction-item` | Transaction list items |

All entries have 2-5 fallback selectors (ID variants, `data-testid` attributes, `aria-label`, etc.).

#### `connector.go` — PayPal Connector Implementation

**`New(cfg, inst, shCfg, totp, passkey)`**:
1. Creates a `SelectorRegistry`, registers all PayPal selectors, and creates a `Stagehand` instance.
2. If TOTP client and secret are available, registers the TOTP secret with the API service.
3. Creates an `auth.Resolver` with TOTP and passkey providers (no SMS or email for PayPal).
4. Returns the `Connector` struct.

**Login flow:**
1. Create a new browser context. Enable the passkey virtual authenticator on it.
2. Navigate to `https://www.paypal.com/signin`
3. Wait for the email field, enter email, click Next
4. Wait for the password field, enter password, click Login
5. Check if 2FA is required (page title contains "verify", "security", "authentication", or "code")
6. If 2FA required, call `handle2FA` which cascades through:
   - **Passkey** — calls `HandlePasskeyAuthentication` to ready the virtual authenticator, clicks the passkey button. The authenticator auto-responds to the WebAuthn challenge.
   - **TOTP** — clicks the "choose authenticator" button, generates a TOTP code via the API, enters it, clicks verify.
   - **Resolver fallback** — as a last resort, calls `auth.ResolveChallenge` which tries all methods in the preference chain.
7. Verify login success by checking the current URL (should not contain "signin" unless "verify" is also present).
8. Set `loggedIn = true`.

**GetTransactions:**
1. Navigate to the activity/transactions page
2. Read the `innerHTML` of the `<main>` element
3. Parse with `parseTransactions` which uses regex to find transaction item patterns
4. Apply currency filter and limit from `TransactionQuery`

**SendPayment:**
1. Navigate to the transfer/send page
2. Wait for the recipient field, enter recipient email
3. Enter the amount
4. Click submit, wait for confirm button, click confirm
5. Return a `SendPaymentResult` with a generated transaction ID

**GetBalance:**
1. Navigate to the account summary page
2. Read the balance text element
3. Parse the amount string (strip non-numeric characters, remove commas)
4. Default currency is USD

**2FA preference:** Passkey → TOTP (no SMS or email for PayPal)

---

### `internal/connector/wise` — Wise Connector

#### `selectors.go` — Wise CSS Selectors

Same pattern as PayPal but with Wise-specific selectors. Notable differences:
- `choose_totp_method` — Wise has a separate button to select the authenticator app method
- `sms_code_input` — separate selector for SMS code entry
- `send_target_currency` — Wise supports source and target currency selection
- All login selectors use `input[name='...']` as primary (Wise uses `name` attributes, not `id`)

#### `connector.go` — Wise Connector Implementation

**`New(cfg, inst, shCfg, totp, passkey, sms)`**:
Same as PayPal, but additionally receives the `SMSProvider`. Creates a `Resolver` with TOTP, passkey, and SMS.

**Login flow:**
1. Create browser context, enable passkey authenticator
2. Navigate to `https://wise.com/login`
3. Enter email and password on the same page, click login
4. If 2FA required, cascade through:
   - **Passkey** — same as PayPal
   - **TOTP** — click "choose authenticator" button, generate code, submit
   - **SMS** — wait for SMS code via `sms.WaitForCode`, submit
   - **Resolver fallback**

**2FA preference:** Passkey → TOTP → SMS

---

### `internal/connector/revolut` — Revolut Connector

#### `selectors.go` — Revolut CSS Selectors

Revolut-specific selectors. Notable differences:
- `login_email` accepts both email and phone (type `email` or `tel`)
- `email_code_input` — separate selector for email verification codes
- Revolut uses `app.revolut.com` URLs

#### `connector.go` — Revolut Connector Implementation

**`New(cfg, inst, shCfg, passkey, sms, email)`**:
No TOTP client — Revolut doesn't support TOTP. Creates a `Resolver` with passkey, SMS, and email.

**Login flow:**
1. Create browser context, enable passkey authenticator
2. Navigate to `https://app.revolut.com/login`
3. Enter email or phone (phone preferred if configured), enter password, click login
4. If 2FA required, cascade through:
   - **Passkey** — same as other connectors
   - **SMS** — wait for SMS code, submit
   - **Email** — wait for email code, submit
   - **Resolver fallback**

**2FA preference:** Passkey → SMS → Email

---

### `internal/controller` — FiatController Orchestrator

The `FiatController` is the top-level orchestrator that manages all connectors.

**`Config`** struct:
- `PayPal`, `Wise`, `Revolut` — per-service `ConnectorConfig`
- `Browser` — browser instance configuration
- `Stagehand` — Stagehand AI healing configuration
- `PollInterval` — how often to poll for new transactions
- `TOTP` — TOTP API client
- `Passkey` — passkey manager
- `SMS` — SMS provider
- `Email` — email provider

**`FiatController`** struct:
- `connectors` — map of service name → `connector.Connector`
- `browser` — shared browser instance
- `cfg` — configuration

**Lifecycle:**

1. `New(cfg)` — creates the shared browser instance, then calls `initConnectors()`.
2. `initConnectors()` — for each service with a non-empty email, creates the appropriate connector:
   - PayPal: `paypal.New(cfg.PayPal, browser, stagehand, totp, passkey)`
   - Wise: `wise.New(cfg.Wise, browser, stagehand, totp, passkey, sms)`
   - Revolut: `revolut.New(cfg.Revolut, browser, stagehand, passkey, sms, email)`
3. `LoginAll(ctx)` — iterates over all connectors and calls `Login()`. Collects errors and returns them as a group.
4. `MonitorTransactions(ctx, handler)` — starts a ticker loop:
   - Every `PollInterval`, fetches transactions from each connector with `Limit: 50`
   - Maintains a `seen` map per service to track which transaction IDs have already been processed
   - For each new (unseen) transaction, calls the handler function with the service name and transaction
5. `Close()` — closes all connectors and the browser instance.

**Service-level operations:**
- `Login(ctx, service)` — log in to a specific service
- `GetTransactions(ctx, service, opts)` — fetch transactions from one service
- `SendPayment(ctx, service, req)` — send payment via one service
- `GetBalance(ctx, service, currency)` — check balance for one service
- `IsLoggedIn(ctx, service)` — check if a service is logged in
- `Get2FAPreferences(service)` — return the 2FA preference chain for a service
- `ListConnectors()` — list the names of all initialized connectors

---

### `services/totp-api` — TOTP Microservice

A standalone HTTP server that generates TOTP codes. Runs as a separate Docker container to isolate secret handling.

**Endpoints:**

| Endpoint | Method | Description |
|---|---|---|
| `GET /health` | GET | Health check, returns `{"status": "ok"}` |
| `POST /register` | POST | Register a TOTP secret for a service |
| `GET /code?service=X` | GET/POST | Generate current TOTP code for a service |
| `GET /list` | GET | List all registered services |

**`POST /register`** body:
```json
{"service": "paypal", "secret": "JBSWY3DPEHPK3PXP"}
```
Validates the secret by attempting to generate a code with it before storing.

**`GET /code`** response:
```json
{
  "service": "paypal",
  "code": "123456",
  "expires_in": 22,
  "generated_at": "2026-04-18T14:30:00Z"
}
```

`expires_in` is the number of seconds until the current 30-second TOTP period expires. Codes are generated with standard TOTP parameters: 30-second period, 6 digits, SHA-1 algorithm.

**Authentication:** If `API_KEY` env var is set, all endpoints (except `/health`) require an `X-API-Key` header or `api_key` query parameter. The key is compared using constant-time comparison to prevent timing attacks.

**Pre-loaded secrets:** The `TOTP_SECRETS` env var accepts a comma-separated list of `service=secret` pairs (e.g. `"paypal=ABC123,wise=DEF456"`) that are loaded at startup.

**Docker:** The TOTP API has its own Dockerfile and Go module, separate from the main controller. It is built as a minimal Alpine image with just `ca-certificates`.

---

## Operational Flow

### Startup Sequence

```
1. main.go reads environment variables
2. Creates PasskeyManager (empty, no authenticators yet)
3. Creates TOTPClient pointed at totp-api service
4. Creates SMSProvider (Twilio or nil) and EmailProvider (IMAP or nil)
5. Assembles controller.Config
6. controller.New(cfg):
   a. Creates shared browser.Instance (launches Chrome)
   b. initConnectors():
      - For each service with a non-empty email:
        - Creates SelectorRegistry + registers all CSS selectors
        - Creates Stagehand instance with the registry
        - Registers TOTP secret with totp-api (if available)
        - Creates auth.Resolver with service-appropriate providers
        - Stores connector in the connectors map
7. Logs 2FA preferences for each connector
8. LoginAll(ctx):
   - For each connector, calls Login():
     a. Creates a fresh browser context (tab)
     b. Enables passkey virtual authenticator on that context
     c. Navigates to login URL
     d. Fills in credentials and submits
     e. If 2FA required, cascades through passkey → TOTP → SMS → email
     f. Verifies login success
9. Starts MonitorTransactions goroutine
10. Blocks on SIGINT/SIGTERM
```

### 2FA Challenge Flow (PayPal example)

```
PayPal Login page
  |
  v
Submit email + password
  |
  v
2FA page detected (title contains "verify")
  |
  v
handle2FA():
  |
  +-- Try passkey:
  |     a. HandlePasskeyAuthentication():
  |        - PasskeyManager.Enable(browserCtx) [already done at Login start]
  |        - PasskeyManager.SetUserVerified(browserCtx, true)
  |     b. Click "passkey_btn"
  |     c. Browser triggers WebAuthn navigator.credentials.get()
  |     d. Virtual authenticator auto-responds with signed assertion
  |     e. Wait 3 seconds for page to process
  |     f. If success → done
  |
  +-- Try TOTP (if passkey failed):
  |     a. Click "totp_choice_btn" to select authenticator method
  |     b. TOTPClient.GenerateCode(ctx, "paypal")
  |        - HTTP GET to totp-api:8080/code?service=paypal
  |        - Returns 6-digit code + expires_in
  |     c. Enter code in "totp_input"
  |     d. Click "totp_submit"
  |     e. If success → done
  |
  +-- Resolver fallback:
        a. auth.ResolveChallenge(ctx, "paypal", pref)
        b. Tries passkey → TOTP → (no fallback) in order
        c. If a method returns a code, submit it
        d. If all fail → error
```

### Transaction Monitor Flow

```
Every POLL_INTERVAL (default 30s):
  |
  v
For each connector:
  |
  v
GetTransactions(ctx, TransactionQuery{Limit: 50})
  |
  v
For each returned transaction:
  |
  +-- If tx.ID not in seen[service]:
  |     - Add to seen map
  |     - Call handler(service, tx)
  |     - Handler logs: "[monitor] paypal: inbound completed 150.00 USD -> alice@example.com"
  |
  +-- If tx.ID already seen: skip
```

### Self-Healing Flow

```
Stagehand.Click(ctx, "login_email")
  |
  v
Look up selector entry: {CSS: "#email", FallbackChain: [...]}
  |
  v
Try "#email" → fails (PayPal changed their frontend)
  |
  v
Try fallbacks: "#login_email", "input[name='login_email']", etc.
  |
  +-- If a fallback works:
  |     - Promote it: UpdateHealed("login_email", "input[name='login_email']", "fallback")
  |     - Old primary becomes first fallback
  |     - Return success
  |
  +-- If all fallbacks fail:
        - Capture DOM snapshot (walk DOM tree, max depth 6)
        - Send to OpenAI: "The selector '#email' for 'PayPal login email input field' 
          has stopped working. Here is the current DOM snapshot. Find the NEW CSS selector."
        - AI returns: "input[data-testid='email-input']"
        - Validate selector (no injection, reasonable length)
        - Promote: UpdateHealed("login_email", "input[data-testid='email-input']", "ai")
        - Execute Click with the new selector
        - Return success
```

---

## Docker Deployment

Both services are defined in the root `docker-compose.yml`:

### totp-api

```yaml
totp-api:
  image: dexarbtrading/totp-api
  build:
    context: ./dexFiatController/services/totp-api
  environment:
    - PORT=8080
    - API_KEY=${TOTP_API_KEY:-}
    - TOTP_SECRETS=${TOTP_SECRETS:-}
  healthcheck:
    test: ["CMD", "wget", "-qO-", "http://localhost:8080/health"]
  resources:
    limits: { memory: 64M, cpus: '0.25' }
```

### fiat-controller

```yaml
fiat-controller:
  image: dexarbtrading/fiat-controller
  build:
    context: ./dexFiatController
  depends_on:
    totp-api: { condition: service_healthy }
  environment:
    - HEADLESS=true
    - CHROME_PATH=/usr/bin/chromium-browser
    - OPENAI_API_KEY=${OPENAI_API_KEY:-}
    - TOTP_API_URL=http://totp-api:8080
    - TOTP_API_KEY=${TOTP_API_KEY:-}
    - PAYPAL_EMAIL=${PAYPAL_EMAIL:-}
    - PAYPAL_PASSWORD=${PAYPAL_PASSWORD:-}
    - PAYPAL_TOTP_SECRET=${PAYPAL_TOTP_SECRET:-}
    - WISE_EMAIL=${WISE_EMAIL:-}
    - WISE_PASSWORD=${WISE_PASSWORD:-}
    - WISE_TOTP_SECRET=${WISE_TOTP_SECRET:-}
    - WISE_PHONE=${WISE_PHONE:-}
    - REVOLUT_EMAIL=${REVOLUT_EMAIL:-}
    - REVOLUT_PASSWORD=${REVOLUT_PASSWORD:-}
    - REVOLUT_PHONE=${REVOLUT_PHONE:-}
    - SMS_PROVIDER=${SMS_PROVIDER:-}
    - EMAIL_PROVIDER=${EMAIL_PROVIDER:-}
    # ... plus SMS and IMAP env vars
  resources:
    limits: { memory: 1024M, cpus: '1.0' }
```

The fiat-controller depends on `totp-api` being healthy before starting. Chrome and all its dependencies (nss, freetype, harfbuzz, fonts) are installed in the Alpine runtime image.

### Building

```bash
# Build both images
docker compose build totp-api fiat-controller

# Start the full stack
docker compose up -d totp-api fiat-controller
```

---

## Known Limitations & Future Work

| Area | Current State | Needed |
|---|---|---|
| **Passkey registration coverage** | Passkey registration works when the service shows a post-login `create passkey` prompt, and the new credential is persisted automatically. | Add service-specific navigation into security settings for providers that do not offer the prompt during login. |
| **Twilio SMS provider** | Stub — `GetLatestCode` and `WaitForCode` return "not yet implemented". | Implement Twilio API calls to fetch recent messages and extract verification codes. |
| **IMAP email provider** | Stub — same as SMS. | Implement IMAP mailbox polling to find and extract verification codes from emails. |
| **Transaction parsing** | Uses simple regex patterns to detect transaction items in HTML. | Parse structured data from the DOM (amount, date, counterparty) instead of just detecting presence. |
| **Session persistence** | Each `GetTransactions`/`SendPayment`/`GetBalance` call opens a new browser context and navigates from scratch. | Reuse the authenticated browser context from login for subsequent operations. |
| **Rate limiting / backoff** | No retry backoff on failures. | Add exponential backoff for login retries and transaction polling failures. |
| **Tests** | No unit or integration tests. | Add tests for TOTP API, Stagehand healing logic, connector interface compliance, and 2FA resolver cascade. |
