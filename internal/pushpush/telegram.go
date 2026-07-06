package pushpush

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"commons/telegram"
)

// TelegramClient sends escalation messages via the shared commons/telegram client.
// The inner *telegram.Client is mutable at runtime via SetClient so pushpush
// picks up hot-reloaded bot credentials (written to the shared telegram_settings
// row by the dexMmController UI) without a restart.
type TelegramClient struct {
	mu    sync.RWMutex
	inner *telegram.Client
}

// NewTelegramClient creates a Telegram client. Returns nil if token or chatID is empty.
func NewTelegramClient(token, chatID string, client *http.Client) *TelegramClient {
	inner := telegram.NewClient(token, chatID, client)
	if inner == nil {
		return nil
	}
	return &TelegramClient{inner: inner}
}

// SetClient swaps the inner Telegram client. Pass nil to disable escalation.
func (t *TelegramClient) SetClient(c *telegram.Client) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.inner = c
	t.mu.Unlock()
}

func (t *TelegramClient) client() *telegram.Client {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.inner
}

// SendClassify asks whether a notification is an incoming payment, with yes/no buttons
// carrying callback_data "yes:<id>" / "no:<id>".
func (t *TelegramClient) SendClassify(ctx context.Context, id string, n Notification) error {
	inner := t.client()
	if inner == nil {
		return fmt.Errorf("telegram client not configured")
	}
	text := fmt.Sprintf("New %s notification:\n\n%s\n%s\n\nIs this a payment received?", n.App, n.Title, n.Text)
	kb := telegram.InlineKeyboard{}
	kb.Row(
		telegram.Button("Yes — payment", "yes:"+id),
		telegram.Button("No — ignore", "no:"+id),
	)
	return inner.SendMessage(ctx, text, &kb)
}

// SendPick asks a human to choose which pending transaction a payment corresponds to.
// Each button carries callback_data "pick:<id>:<txID>" (kept short for the 64-byte limit).
func (t *TelegramClient) SendPick(ctx context.Context, id string, n Notification, candidates []Candidate) error {
	inner := t.client()
	if inner == nil {
		return fmt.Errorf("telegram client not configured")
	}
	text := fmt.Sprintf("Ambiguous payment via %s:\n\n%s\n%s\n\nWhich transaction is this?", n.App, n.Title, n.Text)
	kb := telegram.InlineKeyboard{}
	for _, c := range candidates {
		label := fmt.Sprintf("tx %d: %.2f %s (%s)", c.ID, c.FiatAmount, strings.ToUpper(c.FiatCurrency), c.Dex)
		kb.Row(telegram.Button(label, fmt.Sprintf("pick:%s:%d", id, c.ID)))
	}
	return inner.SendMessage(ctx, text, &kb)
}

// AnswerCallback acknowledges a callback query with a short toast text.
func (t *TelegramClient) AnswerCallback(ctx context.Context, callbackID, text string) {
	if inner := t.client(); inner != nil {
		inner.AnswerCallback(ctx, callbackID, text)
	}
}
