// Package pushpush implements the push-based fiat-arrival server. It receives payment
// notifications, matches them to pending bid transactions in the shared transactions
// table, advances matched rows to DexFiatHasArrived, and triggers BTC release on
// dexMmController. Ambiguous or unclassified notifications are escalated to a human via
// Telegram.
package pushpush

import (
	"context"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

// Server is the pushpush HTTP server and pipeline orchestrator.
type Server struct {
	cfg     Config
	store   *TxStore
	logs    *LogStore
	rules   *RuleStore
	pending *PendingStore
	matcher *Matcher
	release *ReleaseClient
	tg      *TelegramClient
	logger  *Logger

	captureOnly bool
	capture     *RawCapture
}

// New wires all pushpush components from cfg and returns a ready server. In capture-only
// mode (no DATABASE_URL or PUSHPUSH_CAPTURE_ONLY=true) it skips all DB-dependent stores
// and records every raw request instead.
func New(cfg Config) (*Server, error) {
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

	logger, err := NewLogger(filepath.Join(cfg.DataDir, "notifications.jsonl"))
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("pushpush: open audit log: %w", err)
	}

	return &Server{
		cfg:         cfg,
		store:       store,
		logs:        logs,
		rules:       NewRuleStore(filepath.Join(cfg.DataDir, "rules.json")),
		pending:     NewPendingStore(),
		matcher:     NewMatcher(),
		release:     NewReleaseClient(cfg.ReleaseURL, cfg.ReleaseToken, cfg.HTTPClient),
		tg:          NewTelegramClient(cfg.TelegramToken, cfg.TelegramChatID, cfg.HTTPClient),
		logger:      logger,
		captureOnly: false,
	}, nil
}

// Run serves HTTP until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	if s.captureOnly {
		// Discovery mode: capture every request verbatim, regardless of path/method.
		mux.HandleFunc("GET /health", s.handleHealth)
		mux.HandleFunc("/{path...}", s.handleCapture)
	} else {
		mux.HandleFunc("POST /notify", s.handleNotify)
		mux.HandleFunc("POST /telegram-webhook", s.handleTelegramWebhook)
		mux.HandleFunc("GET /health", s.handleHealth)
	}

	srv := &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           mux,
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
	if err := s.capture.Close(); err != nil {
		errs = append(errs, err)
	}
	if err := s.store.Close(); err != nil {
		errs = append(errs, err)
	}
	if err := s.logger.Close(); err != nil {
		errs = append(errs, err)
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
	s.logger.Log("received", n, map[string]string{"normalized": norm})

	go s.handleNotification(context.Background(), n, norm, logID)
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true, "log_id": logID})
}

func (s *Server) authorizeNotify(r *http.Request) bool {
	if s.cfg.NotifySecret == "" {
		return true
	}
	return r.Header.Get("X-Webhook-Secret") == s.cfg.NotifySecret
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
	if err := s.logs.Update(ctx, logID, u); err != nil {
		log.Printf("pushpush: update push log %d: %v", logID, err)
	}
}

// handleNotification is the classification → match → release pipeline.
func (s *Server) handleNotification(ctx context.Context, n Notification, norm string, logID int64) {
	allowed, denied := s.rules.Match(n.App, norm)
	switch {
	case denied:
		s.logger.Log("denied", n, norm)
		s.updateLog(ctx, logID, LogUpdate{Status: LogDenied, RuleDecision: "deny"})
		return
	case allowed:
		s.logger.Log("allowed", n, norm)
		s.updateLog(ctx, logID, LogUpdate{Status: LogReceived, RuleDecision: "allow"}) // pre-match marker
		s.attemptMatch(ctx, n, logID)
		return
	}

	// Unknown shape → ask a human whether this is a payment at all.
	if !s.cfg.EscalationEnabled() {
		s.logger.Log("unclassified_no_escalation", n, norm)
		s.updateLog(ctx, logID, LogUpdate{Status: LogUnclassifiedNoEscalation})
		return
	}
	id := s.pending.Add(PendingEntry{Notification: n, Normalized: norm, Kind: pendingClassify, LogID: logID})
	s.logger.Log("escalated_classify", n, map[string]string{"id": id, "normalized": norm})
	s.updateLog(ctx, logID, LogUpdate{Status: LogEscalatedClassify, TelegramPendingID: id})
	if err := s.tg.SendClassify(ctx, id, n); err != nil {
		s.logger.Log("telegram_send_error", n, err.Error())
		s.updateLog(ctx, logID, LogUpdate{Status: LogTelegramError, Error: err.Error()})
	}
}

// attemptMatch runs the matcher and, on a confident single hit, advances the row and
// triggers release.
func (s *Server) attemptMatch(ctx context.Context, n Notification, logID int64) {
	candidates, err := s.store.ListPendingFiat(ctx)
	if err != nil {
		s.logger.Log("list_pending_error", n, err.Error())
		s.updateLog(ctx, logID, LogUpdate{Status: LogListError, Error: err.Error()})
		return
	}

	candidate, ambiguous := s.matcher.Match(n, candidates)
	switch {
	case candidate != nil:
		s.advanceAndRelease(ctx, n, *candidate, logID)
	case len(ambiguous) > 0:
		s.escalatePick(ctx, n, ambiguous, logID)
	default:
		s.logger.Log("unmatched", n, nil)
		s.updateLog(ctx, logID, LogUpdate{Status: LogUnmatched})
	}
}

// advanceAndRelease marks the row as fiat-arrived and calls the dexMmController release
// webhook. Idempotent: a row already advanced by a concurrent path logs and exits.
func (s *Server) advanceAndRelease(ctx context.Context, n Notification, c Candidate, logID int64) {
	txID := c.ID
	evidence := buildEvidence(n, c)
	advanced, err := s.store.MarkFiatArrived(ctx, txID, evidence)
	if err != nil {
		s.logger.Log("mark_arrived_error", n, map[string]any{"tx_id": txID, "error": err.Error()})
		s.updateLog(ctx, logID, LogUpdate{Status: LogMarkError, MatchedTxID: &txID, Error: err.Error()})
		return
	}
	if !advanced {
		s.logger.Log("already_arrived", n, map[string]uint{"tx_id": txID})
		// Still attempt release: it is idempotent on dexMmController and may have been
		// missed on the prior advance.
	}

	if err := s.release.Release(ctx, txID); err != nil {
		s.logger.Log("release_error", n, map[string]any{"tx_id": txID, "advanced": advanced, "error": err.Error()})
		status := LogReleaseError
		if !advanced {
			status = LogAlreadyReleased
		}
		s.updateLog(ctx, logID, LogUpdate{Status: status, MatchedTxID: &txID, Error: err.Error()})
		return
	}

	if advanced {
		s.logger.Log("released", n, map[string]any{"tx_id": txID, "advanced": advanced})
		s.updateLog(ctx, logID, LogUpdate{Status: LogReleased, MatchedTxID: &txID})
	} else {
		s.logger.Log("released", n, map[string]any{"tx_id": txID, "advanced": advanced})
		s.updateLog(ctx, logID, LogUpdate{Status: LogAlreadyReleased, MatchedTxID: &txID})
	}
	log.Printf("pushpush: released tx %d (%.2f %s) via %s", txID, c.FiatAmount, c.FiatCurrency, n.App)
}

// escalatePick asks a human to choose among ambiguous amount+currency matches.
func (s *Server) escalatePick(ctx context.Context, n Notification, candidates []Candidate, logID int64) {
	if !s.cfg.EscalationEnabled() {
		s.logger.Log("ambiguous_no_escalation", n, map[string]int{"candidates": len(candidates)})
		s.updateLog(ctx, logID, LogUpdate{Status: LogAmbiguousNoEscalation})
		return
	}
	id := s.pending.Add(PendingEntry{Notification: n, Candidates: candidates, Kind: pendingPick, LogID: logID})
	s.logger.Log("escalated_pick", n, map[string]any{"id": id, "candidates": candidateIDs(candidates)})
	s.updateLog(ctx, logID, LogUpdate{Status: LogEscalatedPick, TelegramPendingID: id})
	if err := s.tg.SendPick(ctx, id, n, candidates); err != nil {
		s.logger.Log("telegram_send_error", n, err.Error())
		s.updateLog(ctx, logID, LogUpdate{Status: LogTelegramError, Error: err.Error()})
	}
}

// handleTelegramWebhook receives callback-query decisions from Telegram.
func (s *Server) handleTelegramWebhook(w http.ResponseWriter, r *http.Request) {
	var u telegramUpdate
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
		s.attemptMatch(ctx, entry.Notification, entry.LogID)
		return
	}
	s.rules.Add(entry.Notification.App, entry.Normalized, false)
	s.logger.Log("user_denied", entry.Notification, entry.Normalized)
	s.updateLog(ctx, entry.LogID, LogUpdate{Status: LogUserDenied, RuleDecision: "deny"})
	s.tg.AnswerCallback(ctx, callbackID, "Blacklisted.")
}

// handlePickDecision processes a human's choice among ambiguous candidates.
func (s *Server) handlePickDecision(ctx context.Context, callbackID, id, txIDStr string) {
	entry, ok := s.pending.Get(id)
	if !ok {
		s.tg.AnswerCallback(ctx, callbackID, "Already handled or expired.")
		return
	}
	var picked *Candidate
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
	s.advanceAndRelease(ctx, entry.Notification, *picked, entry.LogID)
	s.tg.AnswerCallback(ctx, callbackID, fmt.Sprintf("Released tx %d.", picked.ID))
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
			"id":           c.ID,
			"fiat_amount":  c.FiatAmount,
			"fiat_currency": c.FiatCurrency,
			"dex":          c.Dex,
		},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

func candidateIDs(cs []Candidate) []uint {
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
