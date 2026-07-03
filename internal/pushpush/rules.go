package pushpush

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// RuleStore persists allow/deny classification rules keyed by app + normalized phrase.
// A notification whose normalized text contains a deny phrase is dropped; one that
// contains an allow phrase skips Telegram classification and goes straight to matching.
type RuleStore struct {
	mu    sync.Mutex
	path  string
	Allow map[string][]string `json:"allow"`
	Deny  map[string][]string `json:"deny"`
}

// NewRuleStore loads rules from path (or starts empty if the file does not exist). path
// is the on-disk location used by save on every mutation.
func NewRuleStore(path string) *RuleStore {
	rs := &RuleStore{
		path:  path,
		Allow: map[string][]string{},
		Deny:  map[string][]string{},
	}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, rs)
		if rs.Allow == nil {
			rs.Allow = map[string][]string{}
		}
		if rs.Deny == nil {
			rs.Deny = map[string][]string{}
		}
	}
	return rs
}

func (rs *RuleStore) save() {
	if rs.path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(rs.path), 0o755); err != nil {
		return
	}
	b, err := json.MarshalIndent(rs, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(rs.path, b, 0o644)
}

// Match reports whether normalized is allowed or denied for app. Deny takes precedence.
func (rs *RuleStore) Match(app, normalized string) (allowed, denied bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	app = strings.ToLower(strings.TrimSpace(app))
	for _, p := range rs.Deny[app] {
		if p != "" && strings.Contains(normalized, p) {
			return false, true
		}
	}
	for _, p := range rs.Allow[app] {
		if p != "" && strings.Contains(normalized, p) {
			return true, false
		}
	}
	return false, false
}

// Add appends a normalized phrase to the allow or deny list for app and persists.
func (rs *RuleStore) Add(app, normalized string, allow bool) {
	app = strings.ToLower(strings.TrimSpace(app))
	normalized = strings.TrimSpace(normalized)
	if normalized == "" {
		return
	}

	rs.mu.Lock()
	defer rs.mu.Unlock()
	if allow {
		rs.Allow[app] = appendUnique(rs.Allow[app], normalized)
	} else {
		rs.Deny[app] = appendUnique(rs.Deny[app], normalized)
	}
	rs.save()
}

func appendUnique(list []string, phrase string) []string {
	for _, existing := range list {
		if existing == phrase {
			return list
		}
	}
	return append(list, phrase)
}
