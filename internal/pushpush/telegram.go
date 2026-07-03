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

// TelegramClient sends escalation messages and answers callback queries via the Telegram
// Bot API. A nil token/chatID disables it.
type TelegramClient struct {
	token  string
	chatID string
	client *http.Client
}

// NewTelegramClient creates a Telegram client. Returns nil if token or chatID is empty.
func NewTelegramClient(token, chatID string, client *http.Client) *TelegramClient {
	token = strings.TrimSpace(token)
	chatID = strings.TrimSpace(chatID)
	if token == "" || chatID == "" {
		return nil
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &TelegramClient{token: token, chatID: chatID, client: client}
}

func (t *TelegramClient) api(method string) string {
	return fmt.Sprintf("https://api.telegram.org/bot%s/%s", t.token, method)
}

func (t *TelegramClient) post(ctx context.Context, method string, body map[string]any) ([]byte, int, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.api(method), bytes.NewReader(b))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return raw, resp.StatusCode, nil
}

// SendClassify asks whether a notification is an incoming payment, with yes/no buttons
// carrying callback_data "yes:<id>" / "no:<id>".
func (t *TelegramClient) SendClassify(ctx context.Context, id string, n Notification) error {
	if t == nil {
		return fmt.Errorf("telegram client not configured")
	}
	text := fmt.Sprintf("New %s notification:\n\n%s\n%s\n\nIs this a payment received?", n.App, n.Title, n.Text)
	body := map[string]any{
		"chat_id": t.chatID,
		"text":    text,
		"reply_markup": map[string]any{
			"inline_keyboard": [][]map[string]string{
				{
					{"text": "Yes — payment", "callback_data": "yes:" + id},
					{"text": "No — ignore", "callback_data": "no:" + id},
				},
			},
		},
	}
	raw, status, err := t.post(ctx, "sendMessage", body)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("telegram sendMessage error: %s", raw)
	}
	return nil
}

// SendPick asks a human to choose which pending transaction a payment corresponds to.
// Each button carries callback_data "pick:<id>:<txID>" (kept short for the 64-byte limit).
func (t *TelegramClient) SendPick(ctx context.Context, id string, n Notification, candidates []Candidate) error {
	if t == nil {
		return fmt.Errorf("telegram client not configured")
	}
	text := fmt.Sprintf("Ambiguous payment via %s:\n\n%s\n%s\n\nWhich transaction is this?", n.App, n.Title, n.Text)
	keyboard := make([][]map[string]string, 0, len(candidates))
	for _, c := range candidates {
		label := fmt.Sprintf("tx %d: %.2f %s (%s)", c.ID, c.FiatAmount, strings.ToUpper(c.FiatCurrency), c.Dex)
		keyboard = append(keyboard, []map[string]string{
			{"text": label, "callback_data": fmt.Sprintf("pick:%s:%d", id, c.ID)},
		})
	}
	body := map[string]any{
		"chat_id":       t.chatID,
		"text":          text,
		"reply_markup":  map[string]any{"inline_keyboard": keyboard},
	}
	raw, status, err := t.post(ctx, "sendMessage", body)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("telegram sendMessage error: %s", raw)
	}
	return nil
}

// AnswerCallback acknowledges a callback query with a short toast text.
func (t *TelegramClient) AnswerCallback(ctx context.Context, callbackID, text string) {
	if t == nil {
		return
	}
	_, _, _ = t.post(ctx, "answerCallbackQuery", map[string]any{
		"callback_query_id": callbackID,
		"text":              text,
	})
}

// telegramUpdate is the minimal webhook payload shape we consume.
type telegramUpdate struct {
	CallbackQuery *struct {
		ID   string `json:"id"`
		Data string `json:"data"`
	} `json:"callback_query"`
}
