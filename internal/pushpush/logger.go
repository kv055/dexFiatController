package pushpush

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Logger is an append-only jsonl audit log of every notification and decision.
type Logger struct {
	mu sync.Mutex
	f  *os.File
}

// NewLogger opens (creating if needed) the jsonl audit log at path.
func NewLogger(path string) (*Logger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &Logger{f: f}, nil
}

// Log writes a single tagged entry. Failures are silent (audit logging must not block
// the pipeline).
func (l *Logger) Log(tag string, n Notification, extra any) {
	if l == nil {
		return
	}
	entry := map[string]any{
		"ts":           time.Now().UTC().Format(time.RFC3339),
		"tag":          tag,
		"notification": n,
		"extra":        extra,
	}
	b, err := json.Marshal(entry)
	if err != nil {
		return
	}
	b = append(b, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.f.Write(b)
}

// Close flushes and closes the underlying file.
func (l *Logger) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	return l.f.Close()
}
