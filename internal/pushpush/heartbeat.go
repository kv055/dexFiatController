package pushpush

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Heartbeat is the most recent successful check-in from the phone forwarding
// payment notifications. It is persisted so a service restart cannot make an
// offline phone appear healthy.
type Heartbeat struct {
	DeviceID string    `json:"device_id"`
	LastSeen time.Time `json:"last_seen"`
}

// HeartbeatStore keeps the latest phone heartbeat in a small JSON file.
type HeartbeatStore struct {
	mu   sync.RWMutex
	path string
	last Heartbeat
}

func NewHeartbeatStore(path string) (*HeartbeatStore, error) {
	s := &HeartbeatStore{path: path}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read heartbeat: %w", err)
	}
	if len(data) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(data, &s.last); err != nil {
		return nil, fmt.Errorf("decode heartbeat: %w", err)
	}
	return s, nil
}

func (s *HeartbeatStore) Record(deviceID string, at time.Time) (Heartbeat, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last = Heartbeat{DeviceID: deviceID, LastSeen: at.UTC()}
	data, err := json.Marshal(s.last)
	if err != nil {
		return Heartbeat{}, fmt.Errorf("encode heartbeat: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o750); err != nil {
		return Heartbeat{}, fmt.Errorf("create heartbeat directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".heartbeat-*")
	if err != nil {
		return Heartbeat{}, fmt.Errorf("create heartbeat temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return Heartbeat{}, fmt.Errorf("write heartbeat: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return Heartbeat{}, fmt.Errorf("set heartbeat permissions: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return Heartbeat{}, fmt.Errorf("close heartbeat: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return Heartbeat{}, fmt.Errorf("persist heartbeat: %w", err)
	}
	return s.last, nil
}

func (s *HeartbeatStore) Latest() Heartbeat {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.last
}

func validDeviceID(deviceID string) bool {
	deviceID = strings.TrimSpace(deviceID)
	return deviceID != "" && len(deviceID) <= 128
}
