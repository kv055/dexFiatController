package notifications

import (
	"dexFiatController/internal/scraping/models"
	"testing"
	"time"
)

func TestEventFromTransaction(t *testing.T) {
	tx := models.Transaction{
		ID:           "tx-123",
		ConnectorID:  "paypal",
		Direction:    models.DirectionInbound,
		Status:       models.TxStatusCompleted,
		Amount:       42.5,
		Currency:     "eur",
		Counterparty: "Alice",
		Description:  "Invoice settled",
		Reference:    "ref-1",
		Timestamp:    time.Date(2026, 4, 18, 7, 0, 0, 0, time.UTC),
		RawData: map[string]any{
			"dex":      "Bisq",
			"bank_ref": "abc",
		},
	}

	event := EventFromTransaction("RoboSats", "paypal", tx)

	if event.Type != PaymentTypeIncoming {
		t.Fatalf("expected incoming type, got %q", event.Type)
	}
	if event.DEX != "Bisq" {
		t.Fatalf("expected dex from raw data, got %q", event.DEX)
	}
	if event.TransferMessage != "Invoice settled" {
		t.Fatalf("expected description as transfer message, got %q", event.TransferMessage)
	}
	if event.Metadata["bank_ref"] != "abc" {
		t.Fatalf("expected raw metadata to be preserved")
	}
}

func TestEventFromSendPaymentResult(t *testing.T) {
	req := models.SendPaymentRequest{
		Amount:      15,
		Currency:    "usd",
		Recipient:   "bob@example.com",
		Description: "",
		Reference:   "rent",
	}
	result := &models.SendPaymentResult{
		TransactionID: "tx-out-1",
		Status:        models.TxStatusCompleted,
		Timestamp:     time.Date(2026, 4, 18, 9, 0, 0, 0, time.UTC),
	}

	event := EventFromSendPaymentResult("RoboSats", "wise", req, result, map[string]any{
		"dex": "Bisq",
	})

	if event.Type != PaymentTypeOutgoing {
		t.Fatalf("expected outgoing type, got %q", event.Type)
	}
	if event.TransferMessage != "rent" {
		t.Fatalf("expected reference fallback as transfer message, got %q", event.TransferMessage)
	}
	if event.DEX != "Bisq" {
		t.Fatalf("expected dex from metadata, got %q", event.DEX)
	}
}
