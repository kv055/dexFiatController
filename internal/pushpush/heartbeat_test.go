package pushpush

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeHeartbeatLogger struct{ calls int }

func (f *fakeHeartbeatLogger) Insert(context.Context, string, string, string) error {
	f.calls++
	return nil
}

func TestPhoneHeartbeatRecordsAndReportsOnline(t *testing.T) {
	store, err := NewHeartbeatStore(filepath.Join(t.TempDir(), "phone-heartbeat.json"))
	if err != nil {
		t.Fatalf("NewHeartbeatStore: %v", err)
	}
	logs := &fakeHeartbeatLogger{}
	s := &Server{cfg: Config{HeartbeatSecret: "heartbeat-secret", HeartbeatMaxAge: time.Minute}, heartbeat: store, heartbeatLogs: logs}

	req := httptest.NewRequest(http.MethodPost, "/phone-heartbeat", strings.NewReader(`{"device_id":"payment-phone"}`))
	req.Header.Set("X-Webhook-Secret", "heartbeat-secret")
	res := httptest.NewRecorder()
	s.handlePhoneHeartbeat(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("heartbeat status = %d, body = %s", res.Code, res.Body.String())
	}
	if logs.calls != 1 {
		t.Fatalf("heartbeat log inserts = %d, want 1", logs.calls)
	}

	statusReq := httptest.NewRequest(http.MethodGet, "/phone-heartbeat", nil)
	statusReq.Header.Set("X-Webhook-Secret", "heartbeat-secret")
	statusRes := httptest.NewRecorder()
	s.handlePhoneHeartbeatStatus(statusRes, statusReq)
	if statusRes.Code != http.StatusOK || !strings.Contains(statusRes.Body.String(), `"online":true`) {
		t.Fatalf("status response = %d, body = %s", statusRes.Code, statusRes.Body.String())
	}

	reloaded, err := NewHeartbeatStore(store.path)
	if err != nil {
		t.Fatalf("reload heartbeat store: %v", err)
	}
	if got := reloaded.Latest(); got.DeviceID != "payment-phone" || got.LastSeen.IsZero() {
		t.Fatalf("persisted heartbeat = %#v", got)
	}
}

func TestPhoneHeartbeatRejectsWrongSecretAndReportsStale(t *testing.T) {
	store, err := NewHeartbeatStore(filepath.Join(t.TempDir(), "phone-heartbeat.json"))
	if err != nil {
		t.Fatalf("NewHeartbeatStore: %v", err)
	}
	if _, err := store.Record("payment-phone", time.Now().Add(-2*time.Minute)); err != nil {
		t.Fatalf("Record: %v", err)
	}
	s := &Server{cfg: Config{HeartbeatSecret: "heartbeat-secret", HeartbeatMaxAge: time.Minute}, heartbeat: store, heartbeatLogs: &fakeHeartbeatLogger{}}

	unauthorized := httptest.NewRequest(http.MethodPost, "/phone-heartbeat", strings.NewReader(`{"device_id":"payment-phone"}`))
	unauthorizedRes := httptest.NewRecorder()
	s.handlePhoneHeartbeat(unauthorizedRes, unauthorized)
	if unauthorizedRes.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized heartbeat status = %d", unauthorizedRes.Code)
	}

	statusReq := httptest.NewRequest(http.MethodGet, "/phone-heartbeat", nil)
	statusReq.Header.Set("X-Webhook-Secret", "heartbeat-secret")
	statusRes := httptest.NewRecorder()
	s.handlePhoneHeartbeatStatus(statusRes, statusReq)
	if statusRes.Code != http.StatusOK || !strings.Contains(statusRes.Body.String(), `"online":false`) {
		t.Fatalf("status response = %d, body = %s", statusRes.Code, statusRes.Body.String())
	}
}

func TestPublicCallbacksFailClosedWithoutCredentials(t *testing.T) {
	s := &Server{cfg: Config{}}
	request := httptest.NewRequest(http.MethodPost, "/notify", nil)
	if s.authorizeNotify(request) {
		t.Fatal("normal-mode notify authorization accepted an empty configured secret")
	}
	if s.authorizeHeartbeat(request) {
		t.Fatal("normal-mode heartbeat authorization accepted an empty configured secret")
	}
	capture := &Server{captureOnly: true}
	if !capture.authorizeNotify(request) || !capture.authorizeHeartbeat(request) {
		t.Fatal("explicit capture-only discovery mode must remain separate and permissive")
	}
}
