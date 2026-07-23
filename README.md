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
+-- internal/paymentemail/          # IMAP reader, MIME safety, and exact email templates
+-- internal/paymentingestion/      # Neutral legitimacy, matching, and escalation caller
+-- internal/paymentevents/         # Source-neutral matching, channel gate, and release
+-- internal/pushpush/              # Push HTTP transport and Telegram presentation
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

## Payment Event Ingestion

Push notifications and authenticated Wise/PayPal email are normalized into
`fiat_payment_events`. Matching is scoped to the provider account linked by the
transaction's canonical `payment_method`; amount/currency are mandatory and sender plus
reference are disambiguation evidence. Raw MIME stays in Maddy and Postgres stores only
the parsed fields and SHA-256.

The source packages stop at a shared `paymentevents.InboundSignal` contract.
`paymentemail` supplies MIME/template/DKIM evidence and `pushpush` supplies the
authenticated HTTP payload. `paymentingestion.Coordinator` is the only caller that
accepts those outputs and decides whether to reject, match, record without action, or
open a Telegram classification/selection. Telegram decisions return to that same
coordinator and cannot bypass channel confirmation.

Each selected account channel is mandatory. An account configured with `push,email`
advances only after both channels confirm the same transaction; `push` or `email` alone
requires only that channel. Confirmation and `DexFiatHasArrived` advancement are atomic,
and the release webhook is called only by the winning transition.

The payment-email reader uses TLS IMAP IDLE plus periodic UID reconciliation and bounded
reconnect backoff. Rollout modes are:

- `capture`: record message metadata and hashes only.
- `shadow`: authenticate and exact-template parse, then record prospective matches
  without confirmations, transaction changes, or release.
- `active`: process only messages newer than both `PAYMENT_EMAIL_ACTIVATION_UID` and
  `PAYMENT_EMAIL_ACTIVATION_TIME`.

Active mode refuses to start without both watermarks. Wise/PayPal release parsing uses
versioned embedded templates and requires a passing DKIM result from the template's
approved signing domain. A new layout or forwarding path that breaks DKIM remains
rejected; it is never routed through a generic or AI parser.

`GET /notification-health?payment_method=Wise` reports the linked account's selected
channels and each channel's last-success health. It uses the heartbeat shared secret and
fails when the method is not active and linked.

## MacroDroid Phone Heartbeat

The phone that forwards bank-payment notifications must periodically check in with
PushPush. This makes a stopped MacroDroid instance, a dead phone, or a lost network
connection visible before a fiat payment is missed.

PushPush provides two protected endpoints:

| Method | Path | Purpose |
| --- | --- | --- |
| `POST` | `/phone-heartbeat` | Record that the phone is online. |
| `GET` | `/phone-heartbeat` | Read the latest check-in and whether it is still online. |

Both endpoints require the `X-Webhook-Secret` header when a heartbeat secret is
configured. Set `PUSHPUSH_HEARTBEAT_SECRET` to a dedicated random value. If it is
unset, PushPush uses `PUSHPUSH_NOTIFY_SECRET`, so existing MacroDroid notification
authentication can be reused. `PUSHPUSH_HEARTBEAT_MAX_AGE` controls when the phone is
considered offline; it defaults to `15m`.

Add these settings to the Fiat Controller environment before deploying it:

```dotenv
PUSHPUSH_HEARTBEAT_SECRET=<a-long-random-secret>
PUSHPUSH_HEARTBEAT_MAX_AGE=15m
```

### MacroDroid setup

1. Create a new macro named **PushPush heartbeat**.
2. Add the **Timer** trigger. Use a five-minute interval; this leaves room for Android
   battery/network delays while remaining well below the default 15-minute maximum age.
3. Add an **HTTP Request** action with these values:

   | Setting | Value |
   | --- | --- |
   | Method | `POST` |
   | URL | `https://<your-public-fiat-controller-host>/phone-heartbeat` |
   | Content type | `application/json` |
   | Request body | `{"device_id":"<stable-phone-name>"}` |
   | Header | `X-Webhook-Secret: <PUSHPUSH_HEARTBEAT_SECRET>` |

   Use the same externally reachable HTTPS host that MacroDroid uses for its existing
   `/notify` requests. The `device_id` is a label for operators, not a secret; use a
   stable value such as `revolut-payment-phone`.
4. In MacroDroid, exclude the app from battery optimization and permit background data.
   Otherwise Android may delay the timer and produce false offline reports.
5. Run the macro once manually. A successful response is HTTP `200` and contains
   `"online":true` plus the server timestamp.

Check the current status from an authenticated machine:

```bash
curl -H 'X-Webhook-Secret: <PUSHPUSH_HEARTBEAT_SECRET>' \
  https://<your-public-fiat-controller-host>/phone-heartbeat
```

The response contains `online`, `last_seen`, and `max_age_seconds`. `online` becomes
`false` when no heartbeat has ever arrived or the latest one is older than the maximum
age. Every accepted ping is retained in the shared Postgres
`fiat_phone_heartbeats` audit table; the latest heartbeat is also retained in
`/data/phone-heartbeat.json`, so restarting the Fiat Controller does not clear an
offline condition.

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
| `PAYMENT_EMAIL_MODE` | `capture` | Payment mail rollout mode: capture, shadow, or active |
| `PAYMENT_EMAIL_IMAP_HOST` | unset | Maddy's internal IMAP host |
| `PAYMENT_EMAIL_IMAP_USER` | unset | Dedicated payment mailbox |
| `PAYMENT_EMAIL_IMAP_PASS` | unset | Dedicated payment mailbox secret |
| `PAYMENT_EMAIL_TLS_SERVER_NAME` | unset | Certificate name expected on IMAP TLS |
| `PAYMENT_EMAIL_TLS_CA_FILE` | unset | Optional private CA/fullchain trust file |
| `PAYMENT_EMAIL_ACTIVATION_UID` | `0` | First safe active-run UID watermark |
| `PAYMENT_EMAIL_ACTIVATION_TIME` | unset | RFC3339 active-run time watermark |
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
