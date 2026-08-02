package pushpush

import "net/http"

// registerRoutes keeps transport concerns separate from Run's background-work
// and shutdown lifecycle. Capture mode deliberately retains its wildcard route.
func (s *Server) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("POST /phone-heartbeat", s.handlePhoneHeartbeat)
	mux.HandleFunc("GET /phone-heartbeat", s.handlePhoneHeartbeatStatus)
	mux.HandleFunc("GET /phone-callback-history", s.handlePhoneCallbackHistory)
	mux.HandleFunc("GET /notification-health", s.handleNotificationHealth)

	if s.captureOnly {
		mux.HandleFunc("/{path...}", s.handleCapture)
		return
	}

	mux.HandleFunc("POST /notify", s.handleNotify)
	mux.HandleFunc("POST /telegram-webhook", s.handleTelegramWebhook)
}
