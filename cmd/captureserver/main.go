// Command captureserver is a standalone, dependency-free HTTP recorder used to
// discover the payload shape a payment-notification forwarder (e.g. MacroDroid
// forwarding Revolut/N26 notifications) sends. It accepts ANY method on ANY
// path and appends each request verbatim to a JSONL log. No database, no
// parsing, no pipeline — purely for capturing raw traffic during development.
//
// Run:
//
//	go run ./cmd/captureserver
//	# or build a static binary:
//	CGO_ENABLED=0 go build -o capturer ./cmd/captureserver
//
// Configure via env (all optional):
//
//	CAPTURE_ADDR  listen address            (default ":8088")
//	CAPTURE_DIR   output directory          (default "./capture-data")
//	CAPTURE_FILE  JSONL filename            (default "capture.jsonl")
//
// Then point your forwarder at http://<this-host>:8088/notify and inspect
// <CAPTURE_DIR>/<CAPTURE_FILE> to see the exact request shape.
package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

func main() {
	addr := flag.String("addr", envOr("CAPTURE_ADDR", ":8088"), "listen address")
	dir := flag.String("dir", envOr("CAPTURE_DIR", "./capture-data"), "output directory")
	file := flag.String("file", envOr("CAPTURE_FILE", "capture.jsonl"), "JSONL filename")
	flag.Parse()

	cap, err := newCapture(filepath.Join(*dir, *file))
	if err != nil {
		log.Fatalf("capture: open log: %v", err)
	}
	defer cap.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/{path...}", func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		log.Printf("captured %s %s %s (len=%d)", r.Method, r.URL.Path, r.RemoteAddr, r.ContentLength)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"captured":true}`))
	})

	log.Printf("captureserver: listening on %s, logging to %s", *addr, cap.path)
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("captureserver: %v", err)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

type capture struct {
	path string
	mu   sync.Mutex
	f    *os.File
}

func newCapture(path string) (*capture, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &capture{path: path, f: f}, nil
}

func (c *capture) Close() error { return c.f.Close() }

func (c *capture) record(r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))

	entry := map[string]any{
		"ts":             time.Now().UTC().Format(time.RFC3339Nano),
		"remote_addr":    r.RemoteAddr,
		"method":         r.Method,
		"path":           r.URL.Path,
		"query":          r.URL.RawQuery,
		"content_type":   r.Header.Get("Content-Type"),
		"content_length": r.ContentLength,
		"headers":        headerMap(r.Header),
		"body_len":       len(body),
	}
	if utf8.Valid(body) {
		entry["body_encoding"] = "utf8"
		entry["body"] = string(body)
	} else {
		entry["body_encoding"] = "base64"
		entry["body_base64"] = base64.StdEncoding.EncodeToString(body)
	}

	b, err := json.Marshal(entry)
	if err != nil {
		return
	}
	b = append(b, '\n')

	c.mu.Lock()
	_, _ = c.f.Write(b)
	c.mu.Unlock()
}

func headerMap(h http.Header) map[string]string {
	m := make(map[string]string, len(h))
	for k, v := range h {
		m[k] = strings.Join(v, ", ")
	}
	return m
}
