package pushpush

import (
	"net/http"

	internalapi "dexFiatController/internal/api/gen/internalapi"
	publicapi "dexFiatController/internal/api/gen/public"
)

// registerRoutes keeps transport concerns separate from Run's background-work
// and shutdown lifecycle. Capture mode deliberately retains its wildcard route.
func (s *Server) registerRoutes(mux *http.ServeMux) {
	legacyMux := http.NewServeMux()
	s.registerLegacyRoutes(legacyMux)

	internalapi.RegisterLegacyStrictHandler(legacyMux, mux)
	if s.captureOnly {
		// Capture mode is intentionally a wildcard raw transport: public callback
		// paths must reach the recorder without generated JSON body validation.
		mux.HandleFunc("/{path...}", s.handleCapture)
		return
	}
	publicapi.RegisterLegacyStrictHandler(legacyMux, mux)
}

// registerLegacyRoutes preserves response behavior while the generated strict
// adapters take ownership of normal route registration and request binding.
func (s *Server) registerLegacyRoutes(mux *http.ServeMux) {
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
