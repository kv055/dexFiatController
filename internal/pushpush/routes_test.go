package pushpush

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRegisterRoutes(t *testing.T) {
	t.Run("normal", func(t *testing.T) {
		mux := http.NewServeMux()
		(&Server{}).registerRoutes(mux)
		assertRoutePattern(t, mux, http.MethodPost, "/notify", "POST /notify")
		assertRoutePattern(t, mux, http.MethodPost, "/telegram-webhook", "POST /telegram-webhook")
		assertRoutePattern(t, mux, http.MethodGet, "/health", "GET /health")
		assertRoutePattern(t, mux, http.MethodPost, "/phone-heartbeat", "POST /phone-heartbeat")
		assertRoutePattern(t, mux, http.MethodGet, "/phone-heartbeat", "GET /phone-heartbeat")
		assertRoutePattern(t, mux, http.MethodGet, "/phone-callback-history", "GET /phone-callback-history")
		assertRoutePattern(t, mux, http.MethodGet, "/notification-health", "GET /notification-health")
	})

	t.Run("capture", func(t *testing.T) {
		mux := http.NewServeMux()
		(&Server{captureOnly: true}).registerRoutes(mux)
		assertRoutePattern(t, mux, http.MethodGet, "/health", "GET /health")
		assertRoutePattern(t, mux, http.MethodPost, "/unknown/path", "/{path...}")
	})
}

func assertRoutePattern(t *testing.T, mux *http.ServeMux, method, path, want string) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	_, got := mux.Handler(req)
	if got != want {
		t.Fatalf("%s %s matched pattern %q, want %q", method, path, got, want)
	}
}
