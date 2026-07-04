package pushpush

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// RawCapture is an append-only jsonl recorder of every inbound request, used in
// capture-only mode to discover the payload shape a forwarder sends. No DB required.
type RawCapture struct {
	mu sync.Mutex
	f  *os.File
}

// NewRawCapture opens (creating if needed) the capture log at path.
func NewRawCapture(path string) (*RawCapture, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &RawCapture{f: f}, nil
}

// Close flushes and closes the underlying file.
func (c *RawCapture) Close() error {
	if c == nil || c.f == nil {
		return nil
	}
	return c.f.Close()
}

// Record appends one request entry. The body is read up to 1 MiB; valid UTF-8 is stored
// verbatim, otherwise base64. Failures are silent so capture never blocks a response.
func (c *RawCapture) Record(r *http.Request) {
	if c == nil {
		return
	}
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
