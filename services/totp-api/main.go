package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

type Service struct {
	mu      sync.RWMutex
	secrets map[string]string
	apiKey  string
}

func NewService(apiKey string) *Service {
	return &Service{
		secrets: make(map[string]string),
		apiKey:  apiKey,
	}
}

type RegisterRequest struct {
	Service string `json:"service"`
	Secret  string `json:"secret"`
}

type RegisterResponse struct {
	Status string `json:"status"`
}

type CodeRequest struct {
	Service string `json:"service"`
	At      int64  `json:"at,omitempty"`
}

type CodeResponse struct {
	Service   string `json:"service"`
	Code      string `json:"code"`
	ExpiresIn int    `json:"expires_in"`
	Generated string `json:"generated_at"`
}

type ListResponse struct {
	Services []string `json:"services"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}

func main() {
	port := envOr("PORT", "8080")
	apiKey := envOr("API_KEY", "")

	svc := NewService(apiKey)

	if secrets := os.Getenv("TOTP_SECRETS"); secrets != "" {
		for _, pair := range strings.Split(secrets, ",") {
			parts := strings.SplitN(pair, "=", 2)
			if len(parts) == 2 {
				svc.secrets[parts[0]] = parts[1]
				log.Printf("loaded secret for service %q", parts[0])
			}
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", svc.handleHealth)
	mux.HandleFunc("/register", svc.withAuth(svc.handleRegister))
	mux.HandleFunc("/code", svc.withAuth(svc.handleCode))
	mux.HandleFunc("/list", svc.withAuth(svc.handleList))

	log.Printf("totp-api listening on :%s", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatalf("listen: %v", err)
	}
}

func (s *Service) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.apiKey == "" {
			next(w, r)
			return
		}

		key := r.Header.Get("X-API-Key")
		if key == "" {
			key = r.URL.Query().Get("api_key")
		}

		if subtle.ConstantTimeCompare([]byte(key), []byte(s.apiKey)) != 1 {
			jsonError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next(w, r)
	}
}

func (s *Service) handleHealth(w http.ResponseWriter, _ *http.Request) {
	jsonResp(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Service) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}

	var req RegisterRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid json")
		return
	}

	if req.Service == "" || req.Secret == "" {
		jsonError(w, http.StatusBadRequest, "service and secret are required")
		return
	}

	_, err := totp.GenerateCode(req.Secret, time.Now().UTC())
	if err != nil {
		jsonError(w, http.StatusBadRequest, fmt.Sprintf("invalid totp secret: %v", err))
		return
	}

	s.mu.Lock()
	s.secrets[req.Service] = req.Secret
	s.mu.Unlock()

	log.Printf("registered totp secret for %q", req.Service)
	jsonResp(w, http.StatusOK, RegisterResponse{Status: "registered"})
}

func (s *Service) handleCode(w http.ResponseWriter, r *http.Request) {
	var req CodeRequest

	if r.Method == http.MethodPost {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, "invalid json")
			return
		}
	} else {
		req.Service = r.URL.Query().Get("service")
		if atStr := r.URL.Query().Get("at"); atStr != "" {
			req.At, _ = strconv.ParseInt(atStr, 10, 64)
		}
	}

	if req.Service == "" {
		jsonError(w, http.StatusBadRequest, "service is required")
		return
	}

	s.mu.RLock()
	secret, ok := s.secrets[req.Service]
	s.mu.RUnlock()

	if !ok {
		jsonError(w, http.StatusNotFound, fmt.Sprintf("no secret for service %q", req.Service))
		return
	}

	now := time.Now().UTC()
	if req.At > 0 {
		now = time.Unix(req.At, 0).UTC()
	}

	code, err := totp.GenerateCodeCustom(secret, now, totp.ValidateOpts{
		Period:    30,
		Digits:    otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		jsonError(w, http.StatusInternalServerError, fmt.Sprintf("generate code: %v", err))
		return
	}

	elapsed := now.Unix() % 30
	expiresIn := 30 - int(elapsed)

	jsonResp(w, http.StatusOK, CodeResponse{
		Service:   req.Service,
		Code:      code,
		ExpiresIn: expiresIn,
		Generated: now.Format(time.RFC3339),
	})
}

func (s *Service) handleList(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	names := make([]string, 0, len(s.secrets))
	for k := range s.secrets {
		names = append(names, k)
	}
	jsonResp(w, http.StatusOK, ListResponse{Services: names})
}

func jsonResp(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	jsonResp(w, status, ErrorResponse{Error: msg})
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
