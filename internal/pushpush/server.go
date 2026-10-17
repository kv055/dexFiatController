// Package pushpush implements the HTTP adapter for push-based fiat-arrival
// notifications and the Telegram presentation flow. Source-neutral matching,
// channel correlation and release coordination live in package paymentevents.
package pushpush

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"commons/metrics"
	"commons/telegram"

	"dexFiatController/internal/obs"
	"dexFiatController/internal/paymentemail"
	"dexFiatController/internal/paymentevents"
	"dexFiatController/internal/paymentingestion"
)

// maxJSONBodyBytes bounds request bodies so a client cannot stream an
// unbounded body into the JSON decoders (generated handlers included).
const maxJSONBodyBytes = 1 << 20 // 1 MiB

// limitRequestBody caps the body of state-changing requests. Handlers that
// need a stricter limit apply their own MaxBytesReader on top.
func limitRequestBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch:
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
			}
		}
		next.ServeHTTP(w, r)
	})
}

// Server is the PushPush HTTP transport and Telegram presentation adapter.
type Server struct {
	cfg       Config
	store     *TxStore
	logs      *LogStore
	ingestion *paymentingestion.Coordinator
	tg        *TelegramClient
	logger    *Logger

	// telegramVersion is the config_version baseline used by the hot-reload
	// poller started in Run to detect shared telegram_settings changes.
	telegramVersion int64

	captureOnly   bool
	capture       *RawCapture
	heartbeat     *HeartbeatStore
	heartbeatLogs heartbeatLogger
	attempts      *CallbackAttemptStore
	emailHealth   *paymentemail.EmailHealth
	emailReader   *paymentemail.EmailReader

	// metrics is the commons service registry; when set, Run serves /metrics and
	// wraps the mux with the RED request middleware.
	metrics *metrics.ServiceRegistry
}

type heartbeatLogger interface {
	Insert(ctx context.Context, deviceID, remoteAddr, userAgent string) error
}

// WithMetrics attaches the Prometheus service registry to the server.
func (s *Server) WithMetrics(reg *metrics.ServiceRegistry) *Server {
	s.metrics = reg
	return s
}

// New wires all pushpush components from cfg and returns a ready server. In capture-only
// mode (no DATABASE_URL or PUSHPUSH_CAPTURE_ONLY=true) it skips all DB-dependent stores
// and records every raw request instead.
func New(cfg Config) (*Server, error) {
	heartbeat, err := NewHeartbeatStore(filepath.Join(cfg.DataDir, "phone-heartbeat.json"))
	if err != nil {
		return nil, fmt.Errorf("pushpush: open heartbeat store: %w", err)
	}
	if cfg.CaptureOnly {
		cap, err := NewRawCapture(filepath.Join(cfg.DataDir, "capture.jsonl"))
		if err != nil {
			return nil, fmt.Errorf("pushpush: open capture log: %w", err)
		}
		logger, _ := NewLogger(filepath.Join(cfg.DataDir, "notifications.jsonl"))
		return &Server{
			cfg:         cfg,
			logger:      logger,
			captureOnly: true,
			capture:     cap,
			heartbeat:   heartbeat,
		}, nil
	}

	store, err := OpenTxStore(cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}

	logs, err := NewLogStore(store.DB())
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	eventStore, err := paymentevents.NewEventStore(store.DB())
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	heartbeatLogs, err := NewHeartbeatLogStore(store.DB())
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	attempts, err := NewCallbackAttemptStore(store.DB())
	if err != nil {
		_ = store.Close()
		return nil, err
	}

	logger, err := NewLogger(filepath.Join(cfg.DataDir, "notifications.jsonl"))
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("pushpush: open audit log: %w", err)
	}

	// Seed Telegram config from the shared DB row when present (env fallback
	// otherwise), and remember the baseline config_version for the hot-reload
	// poller started in Run.
	tgCfg := cfg.TelegramCfg
	var tgVersion int64
	if seedCfg, v := seedTelegramFromDB(context.Background(), store.DB(), cfg.TelegramCfg); seedCfg.Token != "" {
		tgCfg = seedCfg
		tgVersion = v
	}

	emailHealth := &paymentemail.EmailHealth{}
	release := NewReleaseClient(cfg.ReleaseURL, cfg.ReleaseToken, cfg.HTTPClient)
	processor := paymentevents.NewProcessor(eventStore, release)
	rules := NewRuleStore(filepath.Join(cfg.DataDir, "rules.json"))
	telegramClient := NewTelegramClient(tgCfg.Token, tgCfg.ChatID, cfg.HTTPClient)
	srv := &Server{
		cfg:             cfg,
		store:           store,
		logs:            logs,
		tg:              telegramClient,
		logger:          logger,
		captureOnly:     false,
		telegramVersion: tgVersion,
		heartbeat:       heartbeat,
		heartbeatLogs:   heartbeatLogs,
		attempts:        attempts,
		emailHealth:     emailHealth,
	}
	srv.ingestion = paymentingestion.New(processor, rules, telegramClient, srv)
	emailReader, err := paymentemail.NewEmailReader(cfg.PaymentEmail, emailHealth, func(ctx context.Context, signal paymentevents.InboundSignal) error {
		_, handleErr := srv.ingestion.Handle(ctx, signal)
		return handleErr
	})
	if err != nil {
		_ = store.Close()
		_ = logger.Close()
		return nil, fmt.Errorf("pushpush: configure payment email reader: %w", err)
	}
	srv.emailReader = emailReader
	srv.cfg.PaymentEmail = emailReader.Config()
	// Re-register the PUSHPUSH webhook at startup when fully configured.
	if srv.tg != nil && strings.TrimSpace(tgCfg.WebhookURL) != "" {
		if err := srv.tg.client().SetWebhook(context.Background(), tgCfg.WebhookURL); err != nil {
			log.Printf("pushpush: warning: telegram setWebhook: %v", err)
		}
	}
	return srv, nil
}

// Run serves HTTP until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	s.registerRoutes(mux)
	if !s.captureOnly {
		// Hot-reload the shared Telegram bot config from the DB row.
		go runTelegramSettingsPoller(ctx, s.store.DB(), s.tg, s.cfg.HTTPClient, s.telegramVersion)
		go s.emailReader.Run(ctx)
		go func() {
			if err := s.ingestion.RetryPendingConfirmations(ctx); err != nil {
				log.Printf("pushpush: retry pending payment confirmations: %v", err)
			}
		}()
	}

	var handler http.Handler = limitRequestBody(mux)
	if s.metrics != nil {
		mux.Handle("GET /metrics", s.metrics.Handler())
		handler = s.metrics.Middleware(handler)
	}

	srv := &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("pushpush: listening on %s", s.cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

// Close releases all underlying resources.
func (s *Server) Close() error {
	var errs []error
	if s.capture != nil {
		if err := s.capture.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.store != nil {
		if err := s.store.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.logger != nil {
		if err := s.logger.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("pushpush: close errors: %v", errs)
	}
	return nil
}

// handleHealth is a liveness probe.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handlePhoneHeartbeat records a check-in from the phone running MacroDroid.
func (s *Server) handlePhoneHeartbeat(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeHeartbeat(r) {
		s.recordCallbackAttempt(r.Context(), CallbackAttempt{Endpoint: "phone-heartbeat", Outcome: "blocked", StatusCode: http.StatusUnauthorized, Detail: "unauthorized"})
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	var request struct {
		DeviceID string `json:"device_id"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&request); err != nil && !errors.Is(err, io.EOF) {
			s.recordCallbackAttempt(r.Context(), CallbackAttempt{Endpoint: "phone-heartbeat", Outcome: "invalid", StatusCode: http.StatusBadRequest, Detail: "invalid JSON body"})
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
			return
		}
	}
	if !validDeviceID(request.DeviceID) {
		s.recordCallbackAttempt(r.Context(), CallbackAttempt{Endpoint: "phone-heartbeat", Outcome: "invalid", StatusCode: http.StatusBadRequest, DeviceID: strings.TrimSpace(request.DeviceID), Detail: "invalid device_id"})
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "device_id is required and must be at most 128 characters"})
		return
	}
	deviceID := strings.TrimSpace(request.DeviceID)
	if s.heartbeatLogs == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "heartbeat database logging unavailable"})
		return
	}
	if err := s.heartbeatLogs.Insert(r.Context(), deviceID, r.RemoteAddr, r.UserAgent()); err != nil {
		log.Printf("pushpush: record phone heartbeat in database: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "record heartbeat"})
		return
	}
	heartbeat, err := s.heartbeat.Record(deviceID, time.Now())
	if err != nil {
		log.Printf("pushpush: record phone heartbeat: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "record heartbeat"})
		return
	}
	s.recordCallbackAttempt(r.Context(), CallbackAttempt{Endpoint: "phone-heartbeat", Outcome: "accepted", StatusCode: http.StatusOK, DeviceID: deviceID})
	writeJSON(w, http.StatusOK, map[string]any{"online": true, "device_id": heartbeat.DeviceID, "last_seen": heartbeat.LastSeen})
}

// handlePhoneHeartbeatStatus reports whether the most recent heartbeat is fresh.
func (s *Server) handlePhoneHeartbeatStatus(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeHeartbeat(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	heartbeat := s.heartbeat.Latest()
	online := !heartbeat.LastSeen.IsZero() && time.Since(heartbeat.LastSeen) <= s.cfg.HeartbeatMaxAge
	writeJSON(w, http.StatusOK, map[string]any{
		"online":          online,
		"device_id":       heartbeat.DeviceID,
		"last_seen":       heartbeat.LastSeen,
		"max_age_seconds": int64(s.cfg.HeartbeatMaxAge.Seconds()),
	})
}

func (s *Server) handlePhoneCallbackHistory(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeHeartbeat(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if s.attempts == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "callback history unavailable"})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	attempts, err := s.attempts.List(r.Context(), limit)
	if err != nil {
		log.Printf("pushpush: list callback history: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "list callback history"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"attempts": attempts})
}

// handleNotificationHealth reports every channel selected on the account linked
// to a canonical payment method. It shares heartbeat authentication so the
// pre-trade checker does not need another controller secret.
func (s *Server) handleNotificationHealth(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeHeartbeat(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	if s.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "database unavailable"})
		return
	}
	method := strings.TrimSpace(r.URL.Query().Get("payment_method"))
	if method == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "payment_method is required"})
		return
	}
	var provider, channels string
	err := s.store.DB().QueryRowContext(r.Context(), `
		SELECT a.provider, COALESCE(array_to_string(a.notification_channels::text[], ','), '')
		FROM payment_methods pm
		JOIN accounts a ON a.id=pm.account_id AND a.is_active
		WHERE lower(pm.name)=lower($1) AND pm.is_active
	`, method).Scan(&provider, &channels)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "active linked payment account not found"})
		return
	}
	selected := paymentevents.SplitChannels(channels)
	heartbeat := s.heartbeat.Latest()
	pushHealthy := !heartbeat.LastSeen.IsZero() && time.Since(heartbeat.LastSeen) <= s.cfg.HeartbeatMaxAge
	emailLast := time.Time{}
	if s.emailHealth != nil {
		emailLast = s.emailHealth.LastSuccess()
	}
	emailHealthy := !emailLast.IsZero() && time.Since(emailLast) <= s.cfg.PaymentEmail.HealthMaxAge
	writeJSON(w, http.StatusOK, map[string]any{
		"payment_method":    method,
		"provider":          provider,
		"selected_channels": selected,
		"channels": map[string]any{
			"push":  map[string]any{"healthy": pushHealthy, "last_success": heartbeat.LastSeen},
			"email": map[string]any{"healthy": emailHealthy, "last_success": emailLast},
		},
	})
}

// handleCapture records the raw request in capture-only mode and acknowledges it.
func (s *Server) handleCapture(w http.ResponseWriter, r *http.Request) {
	s.capture.Record(r)
	log.Printf("pushpush: captured %s %s %s (len=%d)", r.Method, r.URL.Path, r.RemoteAddr, r.ContentLength)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"captured":true}`))
}

// handleNotify ingests a payment notification from the bank/provider forwarder. Every
// decoded request is recorded in fiat_tx_push_logs at status "received" before the
// pipeline runs, so the row exists regardless of downstream outcome.
func (s *Server) handleNotify(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeNotify(r) {
		s.recordCallbackAttempt(r.Context(), CallbackAttempt{Endpoint: "notify", Outcome: "blocked", StatusCode: http.StatusUnauthorized, Detail: "unauthorized"})
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	rawBody, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		s.recordCallbackAttempt(r.Context(), CallbackAttempt{Endpoint: "notify", Outcome: "invalid", StatusCode: http.StatusBadRequest, Detail: "read body"})
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("read body: %v", err)})
		return
	}
	var n Notification
	// Forwarders send a JSON object matching Notification {app,title,text,time}.
	// Some (e.g. MacroDroid) wrap it in stray leading whitespace or prefix text,
	// so extract the first valid {...} object rather than requiring the whole
	// body to be JSON. When no object is found, fall back to treating the raw
	// payload as the notification text and mine amount/sender from it.
	jsonObj, isJSON := extractJSONObject(rawBody)
	if isJSON {
		if err := json.Unmarshal(jsonObj, &n); err != nil {
			s.recordCallbackAttempt(r.Context(), CallbackAttempt{Endpoint: "notify", Outcome: "invalid", StatusCode: http.StatusBadRequest, Detail: "decode notification"})
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("decode notification: %v", err)})
			return
		}
	} else if len(bytes.TrimSpace(rawBody)) > 0 {
		n = Notification{Text: string(rawBody)}
	}
	// Some forwarders (e.g. MacroDroid) carry no app field; tag by User-Agent so
	// rules can still key on a stable app identifier.
	if n.App == "" {
		n.App = appFromUserAgent(r.Header.Get("User-Agent"))
	}

	// The fiat_tx_push_logs.raw column is jsonb; store the parsed JSON object when
	// available, otherwise wrap the plain-text body as a JSON string so the column
	// always holds valid JSON regardless of the source shape.
	var rawForLog json.RawMessage
	if isJSON {
		rawForLog = json.RawMessage(jsonObj)
	} else {
		wrapped, err := json.Marshal(string(rawBody))
		if err != nil {
			rawForLog = json.RawMessage(`null`)
		} else {
			rawForLog = json.RawMessage(wrapped)
		}
	}

	norm := normalize(n.Title + " " + n.Text)
	logID, logErr := s.logs.Insert(r.Context(), n, rawForLog, norm)
	if logErr != nil {
		log.Printf("pushpush: insert push log: %v", logErr)
	}
	obs.FiatPushPipeline.WithLabelValues(string(LogReceived)).Inc()
	s.logger.Log("received", n, map[string]string{"normalized": norm})

	signal := PaymentSignal(n, rawBody, r.Header.Get("X-Event-ID"), time.Now(), logID)
	receipt, eventErr := s.ingestion.Accept(r.Context(), signal)
	if eventErr != nil {
		s.recordCallbackAttempt(r.Context(), CallbackAttempt{Endpoint: "notify", Outcome: "error", StatusCode: http.StatusInternalServerError, App: n.App, Detail: "persist payment event"})
		s.updateLog(r.Context(), logID, LogUpdate{Status: LogMarkError, Error: eventErr.Error()})
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "persist payment event"})
		return
	}
	if receipt.Duplicate {
		s.recordCallbackAttempt(r.Context(), CallbackAttempt{Endpoint: "notify", Outcome: "accepted", StatusCode: http.StatusOK, App: n.App, Detail: "duplicate"})
		writeJSON(w, http.StatusOK, map[string]any{"accepted": true, "duplicate": true, "event_id": receipt.EventID, "log_id": logID})
		return
	}
	s.recordCallbackAttempt(r.Context(), CallbackAttempt{Endpoint: "notify", Outcome: "accepted", StatusCode: http.StatusAccepted, App: n.App})
	go func() {
		if err := s.ingestion.Process(context.Background(), receipt); err != nil {
			log.Printf("payment ingestion: process push signal: %v", err)
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true, "event_id": receipt.EventID, "log_id": logID})
}

func (s *Server) recordCallbackAttempt(ctx context.Context, attempt CallbackAttempt) {
	if s.attempts == nil {
		return
	}
	if err := s.attempts.Record(ctx, attempt); err != nil {
		log.Printf("pushpush: record callback attempt: %v", err)
	}
}

func (s *Server) authorizeNotify(r *http.Request) bool {
	return s.captureOnly || (s.cfg.NotifySecret != "" && r.Header.Get("X-Webhook-Secret") == s.cfg.NotifySecret)
}

func (s *Server) authorizeHeartbeat(r *http.Request) bool {
	return s.captureOnly || (s.cfg.NotifySecret != "" && r.Header.Get("X-Webhook-Secret") == s.cfg.NotifySecret)
}

// appFromUserAgent maps forwarder User-Agents to a stable app key. Forwarders
// such as MacroDroid post plain-text bodies with no app field; tagging them
// keeps rule learning keyed on a consistent identifier.
func appFromUserAgent(ua string) string {
	switch {
	case strings.HasPrefix(strings.ToLower(ua), "macrodroid/"):
		return "macrodroid"
	default:
		return ""
	}
}

// extractJSONObject locates the first valid JSON object ({...}) in raw, tolerating
// leading whitespace or arbitrary prefix/suffix text that some forwarders (e.g.
// MacroDroid) emit around the payload. It scans for '{', brace-matches while
// respecting string literals and escapes, and returns the matching slice only if
// it is valid JSON. Returns (nil, false) when no valid object is found.
func extractJSONObject(raw []byte) ([]byte, bool) {
	for start := 0; start < len(raw); {
		idx := bytes.IndexByte(raw[start:], '{')
		if idx < 0 {
			return nil, false
		}
		begin := start + idx
		depth := 0
		inStr := false
		end := -1
		for i := begin; i < len(raw); i++ {
			c := raw[i]
			if inStr {
				if c == '\\' {
					i++ // skip escaped char
					continue
				}
				if c == '"' {
					inStr = false
				}
				continue
			}
			switch c {
			case '"':
				inStr = true
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					end = i
				}
			}
			if end >= 0 {
				break
			}
		}
		if end >= 0 {
			slice := raw[begin : end+1]
			if json.Valid(slice) {
				return slice, true
			}
		}
		// Not valid here; continue searching after this '{'.
		start = begin + 1
	}
	return nil, false
}

// updateLog advances the log row; failures are logged but never block the pipeline.
func (s *Server) updateLog(ctx context.Context, logID int64, u LogUpdate) {
	if logID == 0 || s.logs == nil {
		return
	}
	// LogReceived is excluded: it is only re-used as a pre-match marker and the
	// real "received" event is already counted at ingest in handleNotify.
	if u.Status != "" && u.Status != LogReceived {
		obs.FiatPushPipeline.WithLabelValues(string(u.Status)).Inc()
	}
	if err := s.logs.Update(ctx, logID, u); err != nil {
		log.Printf("pushpush: update push log %d: %v", logID, err)
	}
}

// Report maps neutral coordinator outcomes onto the existing PushPush audit log.
// It contains no legitimacy, matching, escalation, or release decisions.
func (s *Server) Report(ctx context.Context, signal paymentevents.InboundSignal, outcome paymentingestion.Outcome) {
	n := Notification{
		App: signal.Presentation.App, Title: signal.Presentation.Title,
		Text: signal.Presentation.Text,
		Time: signal.Event.ProviderTimestamp.Format(time.RFC3339),
	}
	update := LogUpdate{MatchedTxID: outcome.TxID, TelegramPendingID: outcome.PendingID, Error: outcome.Reason}
	action := string(outcome.Kind)
	switch outcome.Kind {
	case paymentingestion.OutcomeDenied:
		update.Status, update.RuleDecision = LogDenied, "deny"
	case paymentingestion.OutcomeUnmatched, paymentingestion.OutcomeRejected:
		update.Status = LogUnmatched
	case paymentingestion.OutcomeAwaitingClassify:
		update.Status = LogEscalatedClassify
	case paymentingestion.OutcomeAwaitingSelection:
		update.Status = LogEscalatedPick
	case paymentingestion.OutcomeAwaitingConfirmation:
		update.Status = LogEscalatedPick
	case paymentingestion.OutcomeNoClassification:
		update.Status = LogUnclassifiedNoEscalation
	case paymentingestion.OutcomeNoSelection:
		update.Status = LogAmbiguousNoEscalation
	case paymentingestion.OutcomeNoConfirmation:
		update.Status = LogAmbiguousNoEscalation
	case paymentingestion.OutcomeListError:
		update.Status = LogListError
	case paymentingestion.OutcomeTelegramError:
		update.Status = LogTelegramError
	case paymentingestion.OutcomeUserAllowed:
		update.Status, update.RuleDecision = LogUserAllowed, "allow"
	case paymentingestion.OutcomeUserDenied:
		update.Status, update.RuleDecision = LogUserDenied, "deny"
	case paymentingestion.OutcomePaymentRejected:
		update.Status = LogUserDenied
	case paymentingestion.OutcomePicked, paymentingestion.OutcomePaymentApproved, paymentingestion.OutcomeWaitingChannels:
		update.Status = LogPicked
	case paymentingestion.OutcomeConfirmationError:
		update.Status = LogMarkError
	case paymentingestion.OutcomeReleaseError:
		update.Status = LogReleaseError
	case paymentingestion.OutcomeReleased:
		update.Status = LogReleased
	case paymentingestion.OutcomeDuplicateConfirmation:
		return
	default:
		// Capture, shadow, and duplicate-email outcomes have no push log row.
		if signal.AuditID == 0 {
			return
		}
		update.Status = LogReceived
	}
	details := map[string]any{
		"tx_id": outcome.TxID, "pending_id": outcome.PendingID,
		"required": outcome.Required, "present": outcome.Present,
		"reason": outcome.Reason,
	}
	s.logger.Log(action, n, details)
	s.updateLog(ctx, signal.AuditID, update)
}

// handleTelegramWebhook receives callback-query decisions from Telegram.
func (s *Server) handleTelegramWebhook(w http.ResponseWriter, r *http.Request) {
	if !telegram.WebhookAuthorized(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var u telegram.Update
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)).Decode(&u); err != nil || u.CallbackQuery == nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	ctx := context.Background()
	data := u.CallbackQuery.Data
	parts := strings.SplitN(data, ":", 3)
	if len(parts) < 2 {
		s.tg.AnswerCallback(ctx, u.CallbackQuery.ID, "Malformed callback.")
		w.WriteHeader(http.StatusOK)
		return
	}

	switch parts[0] {
	case "yes", "no":
		s.handleClassifyDecision(ctx, u.CallbackQuery.ID, parts[0], parts[1])
	case "pick":
		if len(parts) != 3 {
			s.tg.AnswerCallback(ctx, u.CallbackQuery.ID, "Malformed pick callback.")
			break
		}
		s.handlePickDecision(ctx, u.CallbackQuery.ID, parts[1], parts[2])
	case "pay":
		if len(parts) != 3 {
			s.tg.AnswerCallback(ctx, u.CallbackQuery.ID, "Malformed payment callback.")
			break
		}
		chatID := ""
		if u.CallbackQuery.Message != nil && u.CallbackQuery.Message.Chat != nil {
			chatID = strconv.FormatInt(u.CallbackQuery.Message.Chat.ID, 10)
		}
		s.handlePaymentConfirmationDecision(ctx, u.CallbackQuery.ID, chatID, parts[1], parts[2])
	default:
		s.tg.AnswerCallback(ctx, u.CallbackQuery.ID, "Unknown callback.")
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handlePaymentConfirmationDecision(ctx context.Context, callbackID, chatID, decision, requestID string) {
	if decision != "yes" && decision != "no" {
		s.tg.AnswerCallback(ctx, callbackID, "Unknown payment decision.")
		return
	}
	id, err := strconv.ParseInt(requestID, 36, 64)
	if err != nil || id <= 0 {
		s.tg.AnswerCallback(ctx, callbackID, "Malformed payment callback.")
		return
	}
	result, request, err := s.ingestion.DecidePaymentConfirmation(ctx, id, chatID, decision == "yes")
	if err != nil {
		if result.Advanced && request.ID != 0 {
			_ = s.tg.EditPaymentConfirmation(ctx, request, "⚠️ Confirmed, but release failed; operator action is required.")
			s.tg.AnswerCallback(ctx, callbackID, "Confirmed, but release failed.")
			return
		}
		if request.DecisionStatus == "expired" {
			_ = s.tg.EditPaymentConfirmation(ctx, request, "⌛ Expired — the transaction is no longer pending.")
			s.tg.AnswerCallback(ctx, callbackID, "This confirmation expired.")
			return
		}
		s.tg.AnswerCallback(ctx, callbackID, "Already handled, expired, or invalid chat.")
		return
	}
	if decision == "no" {
		if err := s.tg.EditPaymentConfirmation(ctx, request, "❌ Rejected — the transaction remains pending."); err != nil {
			log.Printf("pushpush: edit rejected payment confirmation: %v", err)
		}
		s.tg.AnswerCallback(ctx, callbackID, "Payment rejected.")
		return
	}
	suffix := fmt.Sprintf("✅ Confirmed — transaction #%d was released.", request.TransactionID)
	if result.State == paymentevents.CompletionWaiting {
		suffix = "✅ Confirmed — waiting for the other notification channel."
	}
	if err := s.tg.EditPaymentConfirmation(ctx, request, suffix); err != nil {
		log.Printf("pushpush: edit approved payment confirmation: %v", err)
	}
	s.tg.AnswerCallback(ctx, callbackID, "Payment confirmed.")
}

// handleClassifyDecision processes a yes/no answer to "is this a payment?".
func (s *Server) handleClassifyDecision(ctx context.Context, callbackID, decision, id string) {
	if err := s.ingestion.DecideClassification(ctx, id, decision == "yes"); err != nil {
		s.tg.AnswerCallback(ctx, callbackID, "Already handled or expired.")
		return
	}
	if decision == "yes" {
		s.tg.AnswerCallback(ctx, callbackID, "Whitelisted.")
		return
	}
	s.tg.AnswerCallback(ctx, callbackID, "Blacklisted.")
}

// handlePickDecision processes a human's choice among ambiguous candidates.
func (s *Server) handlePickDecision(ctx context.Context, callbackID, id, txIDStr string) {
	txID, err := strconv.ParseUint(txIDStr, 10, 64)
	if err != nil {
		s.tg.AnswerCallback(ctx, callbackID, "Candidate not found.")
		return
	}
	if err := s.ingestion.DecideSelection(ctx, id, uint(txID)); err != nil {
		s.tg.AnswerCallback(ctx, callbackID, "Already handled, expired, or candidate not found.")
		return
	}
	s.tg.AnswerCallback(ctx, callbackID, fmt.Sprintf("Confirmed tx %d; release remains channel-gated.", txID))
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
