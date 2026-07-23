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
	"strings"
	"time"

	"commons/metrics"
	"commons/telegram"

	"dexFiatController/internal/obs"
	"dexFiatController/internal/paymentemail"
	"dexFiatController/internal/paymentevents"
)

// Server is the pushpush HTTP server and pipeline orchestrator.
type Server struct {
	cfg     Config
	store   *TxStore
	logs    *LogStore
	events  *paymentevents.Processor
	rules   *RuleStore
	pending *PendingStore
	tg      *TelegramClient
	logger  *Logger

	// telegramVersion is the config_version baseline used by the hot-reload
	// poller started in Run to detect shared telegram_settings changes.
	telegramVersion int64

	captureOnly   bool
	capture       *RawCapture
	heartbeat     *HeartbeatStore
	heartbeatLogs heartbeatLogger
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
			pending:     NewPendingStore(),
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
	srv := &Server{
		cfg:             cfg,
		store:           store,
		logs:            logs,
		events:          paymentevents.NewProcessor(eventStore, release),
		rules:           NewRuleStore(filepath.Join(cfg.DataDir, "rules.json")),
		pending:         NewPendingStore(),
		tg:              NewTelegramClient(tgCfg.Token, tgCfg.ChatID, cfg.HTTPClient),
		logger:          logger,
		captureOnly:     false,
		telegramVersion: tgVersion,
		heartbeat:       heartbeat,
		heartbeatLogs:   heartbeatLogs,
		emailHealth:     emailHealth,
	}
	emailIngestor := paymentemail.NewIngestor(cfg.PaymentEmail, srv.events, srv)
	emailReader, err := paymentemail.NewEmailReader(cfg.PaymentEmail, emailHealth, emailIngestor.Handle)
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
	if s.captureOnly {
		// Discovery mode: capture every request verbatim, regardless of path/method.
		mux.HandleFunc("GET /health", s.handleHealth)
		mux.HandleFunc("POST /phone-heartbeat", s.handlePhoneHeartbeat)
		mux.HandleFunc("GET /phone-heartbeat", s.handlePhoneHeartbeatStatus)
		mux.HandleFunc("GET /notification-health", s.handleNotificationHealth)
		mux.HandleFunc("/{path...}", s.handleCapture)
	} else {
		mux.HandleFunc("POST /notify", s.handleNotify)
		mux.HandleFunc("POST /phone-heartbeat", s.handlePhoneHeartbeat)
		mux.HandleFunc("GET /phone-heartbeat", s.handlePhoneHeartbeatStatus)
		mux.HandleFunc("GET /notification-health", s.handleNotificationHealth)
		mux.HandleFunc("POST /telegram-webhook", s.handleTelegramWebhook)
		mux.HandleFunc("GET /health", s.handleHealth)
		// Hot-reload the shared Telegram bot config from the DB row.
		go runTelegramSettingsPoller(ctx, s.store.DB(), s.tg, s.cfg.HTTPClient, s.telegramVersion)
		go s.emailReader.Run(ctx)
	}

	var handler http.Handler = mux
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
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	var request struct {
		DeviceID string `json:"device_id"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&request); err != nil && !errors.Is(err, io.EOF) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
			return
		}
	}
	if !validDeviceID(request.DeviceID) {
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
		LEFT JOIN bank_accounts ba ON ba.id=pm.bank_account_id
		JOIN accounts a ON a.id=COALESCE(pm.account_id, ba.account_id)
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
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	rawBody, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
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

	event := notificationEvent(n, rawBody, r.Header.Get("X-Event-ID"), time.Now())
	eventID, duplicate, eventErr := s.events.Record(r.Context(), event, "received")
	if eventErr != nil {
		s.updateLog(r.Context(), logID, LogUpdate{Status: LogMarkError, Error: eventErr.Error()})
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "persist payment event"})
		return
	}
	if duplicate {
		writeJSON(w, http.StatusOK, map[string]any{"accepted": true, "duplicate": true, "event_id": eventID, "log_id": logID})
		return
	}
	go s.handleNotification(context.Background(), n, norm, logID, event, eventID)
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true, "event_id": eventID, "log_id": logID})
}

func (s *Server) authorizeNotify(r *http.Request) bool {
	if s.cfg.NotifySecret == "" {
		return true
	}
	return r.Header.Get("X-Webhook-Secret") == s.cfg.NotifySecret
}

func (s *Server) authorizeHeartbeat(r *http.Request) bool {
	if s.cfg.HeartbeatSecret == "" {
		return true
	}
	return r.Header.Get("X-Webhook-Secret") == s.cfg.HeartbeatSecret
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

// handleNotification is the classification → match → release pipeline.
func (s *Server) handleNotification(ctx context.Context, n Notification, norm string, logID int64, event PaymentEvent, eventID int64) {
	allowed, denied := s.rules.Match(n.App, norm)
	switch {
	case denied:
		s.logger.Log("denied", n, norm)
		s.updateLog(ctx, logID, LogUpdate{Status: LogDenied, RuleDecision: "deny"})
		_ = s.events.Update(ctx, eventID, "denied", nil, "")
		return
	case allowed:
		s.logger.Log("allowed", n, norm)
		s.updateLog(ctx, logID, LogUpdate{Status: LogReceived, RuleDecision: "allow"}) // pre-match marker
		s.attemptMatch(ctx, n, logID, event, eventID)
		return
	}

	// Unknown shape → ask a human whether this is a payment at all.
	if !s.cfg.EscalationEnabled() {
		s.logger.Log("unclassified_no_escalation", n, norm)
		s.updateLog(ctx, logID, LogUpdate{Status: LogUnclassifiedNoEscalation})
		_ = s.events.Update(ctx, eventID, "unclassified", nil, "")
		return
	}
	id := s.pending.Add(PendingEntry{Notification: n, Event: event, EventID: eventID, Normalized: norm, Kind: pendingClassify, LogID: logID})
	s.logger.Log("escalated_classify", n, map[string]string{"id": id, "normalized": norm})
	s.updateLog(ctx, logID, LogUpdate{Status: LogEscalatedClassify, TelegramPendingID: id})
	_ = s.events.Update(ctx, eventID, "awaiting_classification", nil, "")
	if err := s.tg.SendClassify(ctx, id, n); err != nil {
		s.logger.Log("telegram_send_error", n, err.Error())
		s.updateLog(ctx, logID, LogUpdate{Status: LogTelegramError, Error: err.Error()})
	}
}

// attemptMatch runs the matcher and, on a confident single hit, advances the row and
// triggers release.
func (s *Server) attemptMatch(ctx context.Context, n Notification, logID int64, event PaymentEvent, eventID int64) {
	candidate, ambiguous, candidates, err := s.events.Match(ctx, event, time.Now())
	if err != nil {
		s.logger.Log("list_pending_error", n, err.Error())
		s.updateLog(ctx, logID, LogUpdate{Status: LogListError, Error: err.Error()})
		return
	}

	switch {
	case candidate != nil:
		s.confirmAndRelease(ctx, n, event, eventID, *candidate, logID)
	case len(ambiguous) > 0:
		s.escalatePick(ctx, n, event, eventID, ambiguous, logID)
	default:
		s.logger.Log("unmatched", n, nil)
		s.updateLog(ctx, logID, LogUpdate{Status: LogUnmatched})
		status, reason := eventEligibilityStatus(event, candidates)
		_ = s.events.Update(ctx, eventID, status, nil, reason)
	}
}

func eventEligibilityStatus(event PaymentEvent, candidates []paymentevents.Candidate) (string, string) {
	return paymentevents.EligibilityStatus(event, candidates, time.Now())
}

// confirmAndRelease presents the outcome of the neutral channel-and-release
// coordinator in PushPush's existing logs.
func (s *Server) confirmAndRelease(ctx context.Context, n Notification, event PaymentEvent, eventID int64, c paymentevents.Candidate, logID int64) {
	txID := c.ID
	result, err := s.events.Complete(ctx, event, eventID, c)
	if err != nil {
		status := LogMarkError
		action := "confirmation_error"
		if result.Advanced {
			status = LogReleaseError
			action = "release_error"
		}
		s.logger.Log(action, n, map[string]any{"tx_id": txID, "error": err.Error()})
		s.updateLog(ctx, logID, LogUpdate{Status: status, MatchedTxID: &txID, Error: err.Error()})
		return
	}
	if result.State == paymentevents.CompletionWaiting {
		s.logger.Log("awaiting_channels", n, map[string]any{"tx_id": txID, "required": result.Required, "present": result.Present})
		s.updateLog(ctx, logID, LogUpdate{Status: LogPicked, MatchedTxID: &txID})
		return
	}
	if result.State == paymentevents.CompletionDuplicate {
		return
	}
	s.logger.Log("released", n, map[string]any{"tx_id": txID})
	s.updateLog(ctx, logID, LogUpdate{Status: LogReleased, MatchedTxID: &txID})
	log.Printf("pushpush: released tx %d (%.2f %s) via %s", txID, c.FiatAmount, c.FiatCurrency, n.App)
}

// ConfirmEmail is the presentation callback used by paymentemail after the
// neutral processor has produced a single candidate.
func (s *Server) ConfirmEmail(ctx context.Context, event paymentevents.PaymentEvent, eventID int64, candidate paymentevents.Candidate, subject string) {
	notification := Notification{
		App: event.Provider, Title: subject, Text: event.Description,
		Time: event.ProviderTimestamp.Format(time.RFC3339),
	}
	s.confirmAndRelease(ctx, notification, event, eventID, candidate, 0)
}

// AmbiguousEmail delegates a neutral ambiguous result to the existing Telegram
// selection UI; selection still records only the email channel.
func (s *Server) AmbiguousEmail(ctx context.Context, event paymentevents.PaymentEvent, eventID int64, candidates []paymentevents.Candidate, subject string) {
	notification := Notification{
		App: event.Provider, Title: subject, Text: event.Description,
		Time: event.ProviderTimestamp.Format(time.RFC3339),
	}
	s.escalatePick(ctx, notification, event, eventID, candidates, 0)
}

// escalatePick asks a human to choose among ambiguous amount+currency matches.
func (s *Server) escalatePick(ctx context.Context, n Notification, event PaymentEvent, eventID int64, candidates []paymentevents.Candidate, logID int64) {
	if !s.cfg.EscalationEnabled() {
		s.logger.Log("ambiguous_no_escalation", n, map[string]int{"candidates": len(candidates)})
		s.updateLog(ctx, logID, LogUpdate{Status: LogAmbiguousNoEscalation})
		_ = s.events.Update(ctx, eventID, "ambiguous", nil, "")
		return
	}
	id := s.pending.Add(PendingEntry{Notification: n, Event: event, EventID: eventID, Candidates: candidates, Kind: pendingPick, LogID: logID})
	s.logger.Log("escalated_pick", n, map[string]any{"id": id, "candidates": candidateIDs(candidates)})
	s.updateLog(ctx, logID, LogUpdate{Status: LogEscalatedPick, TelegramPendingID: id})
	_ = s.events.Update(ctx, eventID, "awaiting_selection", nil, "")
	if err := s.tg.SendPick(ctx, id, n, candidates); err != nil {
		s.logger.Log("telegram_send_error", n, err.Error())
		s.updateLog(ctx, logID, LogUpdate{Status: LogTelegramError, Error: err.Error()})
	}
}

// handleTelegramWebhook receives callback-query decisions from Telegram.
func (s *Server) handleTelegramWebhook(w http.ResponseWriter, r *http.Request) {
	if !telegram.WebhookAuthorized(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var u telegram.Update
	if err := json.NewDecoder(r.Body).Decode(&u); err != nil || u.CallbackQuery == nil {
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
	default:
		s.tg.AnswerCallback(ctx, u.CallbackQuery.ID, "Unknown callback.")
	}
	w.WriteHeader(http.StatusOK)
}

// handleClassifyDecision processes a yes/no answer to "is this a payment?".
func (s *Server) handleClassifyDecision(ctx context.Context, callbackID, decision, id string) {
	entry, ok := s.pending.Get(id)
	if !ok {
		s.tg.AnswerCallback(ctx, callbackID, "Already handled or expired.")
		return
	}
	if decision == "yes" {
		s.rules.Add(entry.Notification.App, entry.Normalized, true)
		s.logger.Log("user_allowed", entry.Notification, entry.Normalized)
		s.updateLog(ctx, entry.LogID, LogUpdate{Status: LogUserAllowed, RuleDecision: "allow"})
		s.tg.AnswerCallback(ctx, callbackID, "Whitelisted.")
		s.attemptMatch(ctx, entry.Notification, entry.LogID, entry.Event, entry.EventID)
		return
	}
	s.rules.Add(entry.Notification.App, entry.Normalized, false)
	s.logger.Log("user_denied", entry.Notification, entry.Normalized)
	s.updateLog(ctx, entry.LogID, LogUpdate{Status: LogUserDenied, RuleDecision: "deny"})
	_ = s.events.Update(ctx, entry.EventID, "denied", nil, "")
	s.tg.AnswerCallback(ctx, callbackID, "Blacklisted.")
}

// handlePickDecision processes a human's choice among ambiguous candidates.
func (s *Server) handlePickDecision(ctx context.Context, callbackID, id, txIDStr string) {
	entry, ok := s.pending.Get(id)
	if !ok {
		s.tg.AnswerCallback(ctx, callbackID, "Already handled or expired.")
		return
	}
	var picked *paymentevents.Candidate
	for i := range entry.Candidates {
		if fmt.Sprintf("%d", entry.Candidates[i].ID) == txIDStr {
			picked = &entry.Candidates[i]
			break
		}
	}
	if picked == nil {
		s.tg.AnswerCallback(ctx, callbackID, "Candidate not found.")
		return
	}
	txID := picked.ID
	s.updateLog(ctx, entry.LogID, LogUpdate{Status: LogPicked, MatchedTxID: &txID})
	s.confirmAndRelease(ctx, entry.Notification, entry.Event, entry.EventID, *picked, entry.LogID)
	s.tg.AnswerCallback(ctx, callbackID, fmt.Sprintf("Confirmed tx %d; release remains channel-gated.", picked.ID))
}

// buildEvidence is the jsonb stored in transactions.dex_fiat_arrival as proof of arrival.
func buildEvidence(n Notification, c Candidate) json.RawMessage {
	amount, currency := n.amountAndCurrency()
	doc := map[string]any{
		"source":      "pushpush",
		"app":         n.App,
		"title":       n.Title,
		"text":        n.Text,
		"sender":      n.senderName(),
		"amount":      amount,
		"currency":    strings.ToUpper(currency),
		"normalized":  normalize(n.Title + " " + n.Text),
		"received_at": time.Now().UTC().Format(time.RFC3339),
		"matched_tx": map[string]any{
			"id":            c.ID,
			"fiat_amount":   c.FiatAmount,
			"fiat_currency": c.FiatCurrency,
			"dex":           c.Dex,
		},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

func candidateIDs(cs []paymentevents.Candidate) []uint {
	ids := make([]uint, 0, len(cs))
	for _, c := range cs {
		ids = append(ids, c.ID)
	}
	return ids
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
