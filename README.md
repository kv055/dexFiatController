# dexFiatController

`dexFiatController` is the fiat automation service for dexArbTrading. It monitors fiat-provider accounts, handles multi-factor authentication, and forwards observed payment events to `dexUnifiedApi`.

The controller currently targets PayPal, Wise, and Revolut through browser automation. Credentials and account secrets are read from the shared Postgres database rather than from environment variables.

## Runtime Role

On startup the service:

1. Loads browser, TOTP, SMS, email, notification, and database configuration.
2. Opens the shared secret/account store in Postgres.
3. Loads connector credentials for PayPal, Wise, and Revolut from the database.
4. Loads active TOTP secrets and passkey credentials from the database.
5. Creates a shared Chromium automation instance.
6. Initializes connectors for configured accounts.
7. Logs into all available connectors.
8. Starts a polling loop that emits transaction notifications.
9. Shuts down cleanly on `SIGINT` or `SIGTERM`.

## Architecture

```text
main.go
  |
  v
FiatController (internal/controller)
  |
  +-- PayPal connector
  +-- Wise connector
  +-- Revolut connector
  |
  +-- Browser instance (chromedp)
  |     +-- Stagehand selector healing
  |
  +-- Auth providers
        +-- PasskeyManager
        +-- TOTPClient
        +-- SMSProvider
        +-- EmailProvider
        +-- 2FA Resolver

services/totp-api
  Standalone HTTP service that returns TOTP codes to authenticated callers.
```

## Directory Layout

```text
dexFiatController/
+-- main.go                         # Runtime wiring and config loading
+-- internal/auth/                  # Passkey, TOTP, SMS, email, and 2FA resolver
+-- internal/browser/               # Chromium wrapper and Stagehand selector healing
+-- internal/connector/             # Connector interface and shared types
+-- internal/controller/            # Connector orchestration and polling loop
+-- internal/models/                # Transaction, payment, balance, config models
+-- internal/notifications/         # Client for dexUnifiedApi payment notifications
+-- internal/secrets/               # Shared Postgres secret/account store
+-- services/totp-api/              # TOTP microservice
+-- Dockerfile
```

## Connectors

Each connector implements the shared connector interface:

- `Name()`
- `Login(ctx)`
- `IsLoggedIn(ctx)`
- `Logout(ctx)`
- `GetTransactions(ctx, opts)`
- `SendPayment(ctx, req)`
- `GetBalance(ctx, currency)`
- `Close()`

The controller calls `LoginAll` and then repeatedly polls configured connectors for transactions. Observed transactions and direct send-payment results are converted into payment notifications.

## Authentication

The 2FA resolver can attempt methods in a configured order:

- passkey through Chrome DevTools Protocol WebAuthn virtual authenticators
- TOTP through the local `totp-api` service
- SMS through Twilio
- email through IMAP

If no SMS or email provider is configured, those methods are skipped. TOTP and passkey material is loaded from the shared database.

## Stagehand Selector Healing

The browser layer wraps `chromedp` and includes a Stagehand-style selector registry:

- primary selectors are registered by stable logical keys
- fallback selector chains are attempted when primary selectors fail
- working fallbacks are promoted
- optional AI healing can inspect a DOM snapshot and propose a replacement selector when `OPENAI_API_KEY` is configured

AI healing is best treated as an operational fallback. Stable explicit selectors should still be preferred whenever a connector flow is known.

## Payment Notifications

`internal/notifications` posts JSON payloads to `dexUnifiedApi`.

Standard fields:

- `amount`
- `currency`
- `type`
- `transaction_id`
- `dex`
- `connector`
- `transfer_message`
- `metadata`
- `timestamp`

Set `PAYMENT_NOTIFICATIONS_API_URL` to the unified API internal endpoint:

```text
http://unified-api:5010/internal/payment_notifications
```

Set `PAYMENT_NOTIFICATIONS_SHARED_SECRET` when the unified API is configured to require `X-Webhook-Secret`.

## TOTP API

The TOTP helper service lives in `services/totp-api`.

Endpoints:

- `GET /health`
- `POST /register`
- `GET /code` or `POST /code`
- `GET /list`

All non-health endpoints are protected by `API_KEY` when it is configured.

In the root Compose stack this service runs as `totp-api` on port `8080` inside the Docker network.

## Environment

Copy the example file:

```bash
cp .env.example .env
```

Important variables:

| Variable | Default | Description |
| --- | --- | --- |
| `DATABASE_URL` | unset | Shared Postgres database for account, TOTP, and passkey storage |
| `HEADLESS` | `true` | Run Chromium in headless mode |
| `CHROME_PATH` | `/usr/bin/chromium-browser` | Chromium binary path |
| `OPENAI_API_KEY` | unset | Enables optional AI selector healing |
| `TOTP_API_URL` | `http://totp-api:8080` | TOTP helper URL |
| `TOTP_API_KEY` | unset | TOTP helper API key |
| `SMS_PROVIDER` | unset | Use `twilio` to enable SMS |
| `SMS_ACCOUNT_SID` | unset | Twilio account SID |
| `SMS_AUTH_TOKEN` | unset | Twilio auth token |
| `SMS_PHONE_NUMBER` | unset | Twilio sender number |
| `EMAIL_PROVIDER` | unset | Use `imap` to enable email codes |
| `EMAIL_IMAP_HOST` | unset | IMAP host |
| `EMAIL_IMAP_USER` | unset | IMAP username |
| `EMAIL_IMAP_PASS` | unset | IMAP password |
| `POLL_INTERVAL` | `30s` | Transaction polling interval |
| `PAYMENT_NOTIFICATIONS_API_URL` | unset | Unified API notification receiver |
| `PAYMENT_NOTIFICATIONS_SHARED_SECRET` | unset | Optional webhook secret |
| `PAYMENT_NOTIFICATIONS_DEX` | unset | Default DEX label attached to notifications |

PayPal, Wise, and Revolut email/password/phone records are loaded from Postgres. They are intentionally not accepted from `.env`.

## Local Development

Run the controller:

```bash
go run .
```

Run the TOTP helper:

```bash
cd services/totp-api
go run .
```

Run tests:

```bash
go test ./...
```

For realistic browser behavior, run through Docker or ensure local Chromium is installed and reachable through `CHROME_PATH`.

## Operational Notes

- Browser automation can break when provider websites change. Keep connector selectors small, stable, and backed by fallbacks.
- Use the dashboard trading-account administration flow to manage account records and secrets in the shared database.
- Headful mode is useful while debugging login flows locally.
- Do not commit provider credentials, TOTP secrets, passkeys, or local `.env` files.
