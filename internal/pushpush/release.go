package pushpush

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ReleaseClient triggers BTC release on dexMmController via the existing
// POST /api/webhooks/dex-release webhook (bearer-token auth, body {transaction_id}).
type ReleaseClient struct {
	url      string
	token    string
	client   *http.Client
	maxTries int
	backoff  time.Duration
}

// NewReleaseClient creates a release client. A nil/empty url returns nil (release
// disabled, matching attempts will log a warning instead of erroring).
func NewReleaseClient(url, token string, client *http.Client) *ReleaseClient {
	url = strings.TrimSpace(url)
	if url == "" {
		return nil
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &ReleaseClient{url: url, token: token, client: client, maxTries: 3, backoff: 3 * time.Second}
}

// Release posts the release webhook for txID. 200 (already_released) and 202 (accepted)
// are success; 503 (queue full) is retried with backoff. The neutral payment
// coordinator calls it only after the atomic channel gate advances a transaction;
// the dexMmController webhook is independently idempotent.
func (r *ReleaseClient) Release(ctx context.Context, txID uint) error {
	if r == nil {
		return fmt.Errorf("pushpush: release client not configured")
	}
	body, err := json.Marshal(map[string]uint{"transaction_id": txID})
	if err != nil {
		return fmt.Errorf("marshal release body: %w", err)
	}

	var lastErr error
	for attempt := 1; attempt <= r.maxTries; attempt++ {
		err := r.doOnce(ctx, body)
		if err == nil {
			return nil
		}
		lastErr = err
		if attempt < r.maxTries && isRetryable(err) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(r.backoff):
			}
			continue
		}
		break
	}
	return lastErr
}

func (r *ReleaseClient) doOnce(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build release request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("send release request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch {
	case resp.StatusCode == http.StatusOK, resp.StatusCode == http.StatusAccepted:
		return nil
	case resp.StatusCode == http.StatusServiceUnavailable:
		return releaseErr{status: resp.StatusCode, body: string(raw), retryable: true}
	default:
		return releaseErr{status: resp.StatusCode, body: string(raw)}
	}
}

type releaseErr struct {
	status    int
	body      string
	retryable bool
}

func (e releaseErr) Error() string {
	return fmt.Sprintf("dex-release webhook returned status %d: %s", e.status, e.body)
}

func isRetryable(err error) bool {
	e, ok := err.(releaseErr)
	return ok && e.retryable
}
