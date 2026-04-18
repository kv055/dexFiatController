package notifications

import (
	"bytes"
	"context"
	"dexFiatController/internal/models"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

type PaymentType string

const (
	PaymentTypeIncoming PaymentType = "incoming"
	PaymentTypeOutgoing PaymentType = "outgoing"
)

type Event struct {
	Amount          float64        `json:"amount"`
	Currency        string         `json:"currency"`
	Type            PaymentType    `json:"type"`
	TransactionID   string         `json:"transaction_id"`
	DEX             string         `json:"dex,omitempty"`
	Connector       string         `json:"connector"`
	TransferMessage string         `json:"transfer_message,omitempty"`
	Metadata        map[string]any `json:"metadata,omitempty"`
	Timestamp       time.Time      `json:"timestamp"`
}

type Config struct {
	Endpoint   string
	Secret     string
	DefaultDEX string
	Timeout    time.Duration
	HTTPClient *http.Client
}

type Client struct {
	endpoint   string
	secret     string
	defaultDEX string
	httpClient *http.Client

	mu        sync.Mutex
	published map[string]time.Time
}

func NewClient(cfg Config) *Client {
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		return nil
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		httpClient = &http.Client{Timeout: timeout}
	}

	return &Client{
		endpoint:   endpoint,
		secret:     strings.TrimSpace(cfg.Secret),
		defaultDEX: strings.TrimSpace(cfg.DefaultDEX),
		httpClient: httpClient,
		published:  make(map[string]time.Time),
	}
}

func (c *Client) PublishTransaction(ctx context.Context, service string, tx models.Transaction) error {
	if c == nil {
		return nil
	}
	event := EventFromTransaction(c.defaultDEX, service, tx)
	return c.publish(ctx, dedupeKey(service, event.Type, event.TransactionID), event)
}

func (c *Client) PublishSendPaymentResult(ctx context.Context, service string, req models.SendPaymentRequest, result *models.SendPaymentResult, metadata map[string]any) error {
	if c == nil || result == nil {
		return nil
	}
	event := EventFromSendPaymentResult(c.defaultDEX, service, req, result, metadata)
	return c.publish(ctx, dedupeKey(service, event.Type, event.TransactionID), event)
}

func EventFromTransaction(defaultDEX, service string, tx models.Transaction) Event {
	message := strings.TrimSpace(tx.Description)
	if message == "" {
		message = strings.TrimSpace(tx.Reference)
	}

	metadata := map[string]any{
		"connector_id": tx.ConnectorID,
		"counterparty": tx.Counterparty,
		"description":  tx.Description,
		"reference":    tx.Reference,
		"status":       tx.Status,
	}
	for key, value := range tx.RawData {
		metadata[key] = value
	}

	ts := tx.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}

	return Event{
		Amount:          tx.Amount,
		Currency:        strings.ToUpper(strings.TrimSpace(tx.Currency)),
		Type:            paymentTypeFromDirection(tx.Direction),
		TransactionID:   strings.TrimSpace(tx.ID),
		DEX:             resolveDEX(defaultDEX, tx.RawData),
		Connector:       strings.TrimSpace(service),
		TransferMessage: message,
		Metadata:        metadata,
		Timestamp:       ts.UTC(),
	}
}

func EventFromSendPaymentResult(defaultDEX, service string, req models.SendPaymentRequest, result *models.SendPaymentResult, metadata map[string]any) Event {
	message := strings.TrimSpace(req.Description)
	if message == "" {
		message = strings.TrimSpace(req.Reference)
	}

	mergedMetadata := map[string]any{
		"recipient": req.Recipient,
		"reference": req.Reference,
		"status":    result.Status,
	}
	for key, value := range metadata {
		mergedMetadata[key] = value
	}

	ts := result.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}

	return Event{
		Amount:          req.Amount,
		Currency:        strings.ToUpper(strings.TrimSpace(req.Currency)),
		Type:            PaymentTypeOutgoing,
		TransactionID:   strings.TrimSpace(result.TransactionID),
		DEX:             resolveDEX(defaultDEX, metadata),
		Connector:       strings.TrimSpace(service),
		TransferMessage: message,
		Metadata:        mergedMetadata,
		Timestamp:       ts.UTC(),
	}
}

func (c *Client) publish(ctx context.Context, key string, event Event) error {
	if c.alreadyPublished(key) {
		return nil
	}

	body, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal payment notification: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build payment notification request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.secret != "" {
		req.Header.Set("X-Webhook-Secret", c.secret)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.forget(key)
		return fmt.Errorf("send payment notification: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.forget(key)
		return fmt.Errorf("send payment notification: unexpected status %s", resp.Status)
	}
	return nil
}

func (c *Client) alreadyPublished(key string) bool {
	if key == "" {
		return false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	for existingKey, publishedAt := range c.published {
		if now.Sub(publishedAt) > 24*time.Hour {
			delete(c.published, existingKey)
		}
	}

	if _, ok := c.published[key]; ok {
		return true
	}
	c.published[key] = now
	return false
}

func (c *Client) forget(key string) {
	if key == "" {
		return
	}
	c.mu.Lock()
	delete(c.published, key)
	c.mu.Unlock()
}

func dedupeKey(service string, paymentType PaymentType, transactionID string) string {
	transactionID = strings.TrimSpace(transactionID)
	if transactionID == "" {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(service)) + ":" + string(paymentType) + ":" + transactionID
}

func paymentTypeFromDirection(direction models.TransactionDirection) PaymentType {
	if direction == models.DirectionOutbound {
		return PaymentTypeOutgoing
	}
	return PaymentTypeIncoming
}

func resolveDEX(defaultDEX string, metadata map[string]any) string {
	for _, key := range []string{"dex", "exchange", "associated_dex"} {
		if value, ok := metadata[key]; ok {
			if resolved := strings.TrimSpace(fmt.Sprint(value)); resolved != "" && resolved != "<nil>" {
				return resolved
			}
		}
	}
	return defaultDEX
}
