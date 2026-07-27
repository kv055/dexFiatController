package paymentevents

import (
	"strings"
	"testing"
	"time"
)

func TestComparePaymentEventsUsesComparableBusinessFields(t *testing.T) {
	now := time.Now().UTC()
	push := PaymentEvent{
		Source: SourcePush, Provider: "wise", Sender: " Jane   Doe ",
		AmountMinor: 1234, Currency: "eur",
		Description:       "Payment received. Reference: Order-42",
		ProviderTimestamp: now,
	}
	email := PaymentEvent{
		Source: SourceEmail, Provider: "WISE", Sender: "jane doe",
		AmountMinor: 1234, Currency: "EUR", Description: "order-42",
		ProviderTimestamp: now.Add(4 * time.Minute),
	}
	if got := comparePaymentEvents(push, email); len(got) != 0 {
		t.Fatalf("normalized events should reconcile, mismatches=%v", got)
	}
}

func TestComparePaymentEventsIgnoresMissingOptionalValues(t *testing.T) {
	push := PaymentEvent{
		Source: SourcePush, Provider: "WISE", AmountMinor: 1000, Currency: "EUR",
	}
	email := PaymentEvent{
		Source: SourceEmail, Provider: "WISE", Sender: "Jane Doe",
		AmountMinor: 1000, Currency: "EUR", Description: "order-42",
	}
	if got := comparePaymentEvents(push, email); len(got) != 0 {
		t.Fatalf("missing optional values should not mismatch, got=%v", got)
	}
}

func TestComparePaymentEventsReportsPopulatedDifferences(t *testing.T) {
	now := time.Now().UTC()
	push := PaymentEvent{
		Source: SourcePush, Provider: "WISE", Sender: "Jane Doe",
		AmountMinor: 1000, Currency: "EUR",
		Description:       "Reference: push-1",
		ProviderTimestamp: now,
	}
	email := PaymentEvent{
		Source: SourceEmail, Provider: "WISE", Sender: "John Doe",
		AmountMinor: 1000, Currency: "EUR", Description: "email-2",
		ProviderTimestamp: now.Add(6 * time.Minute),
	}
	got := strings.Join(comparePaymentEvents(push, email), ",")
	if got != "reference,sender,timestamp" {
		t.Fatalf("mismatches=%q", got)
	}
}

func TestPaymentConfirmationMessageDisplaysTransactionAndReason(t *testing.T) {
	candidate := Candidate{
		ID: 42, FiatAmount: 12.34, FiatCurrency: "EUR",
		Dex: "Bisq", PaymentMethod: "Wise",
	}
	event := PaymentEvent{
		Source: SourcePush, Provider: "WISE", Sender: "Jane Doe",
		AmountMinor: 1234, Currency: "EUR",
	}
	message := paymentConfirmationMessage(
		candidate, event, nil, ConfirmationReasonSingleChannel, nil,
	)
	for _, wanted := range []string{
		"⚠️ Confirm incoming payment", "Transaction #42", "DEX: Bisq",
		"Payment method: Wise", "Expected amount: 12.34 EUR",
		"Only one notification channel is configured.",
		"Does this payment belong to transaction #42?",
	} {
		if !strings.Contains(message, wanted) {
			t.Fatalf("message missing %q:\n%s", wanted, message)
		}
	}
}
