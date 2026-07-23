package pushpush

import (
	"math"
	"strings"
	"time"

	"dexFiatController/internal/paymentevents"
)

// PaymentEvent is an alias to the neutral domain model. PushPush only adapts
// its source-specific Notification into this type.
type PaymentEvent = paymentevents.PaymentEvent

func notificationEvent(n Notification, raw []byte, sourceEventID string, received time.Time) PaymentEvent {
	amount, currency := n.amountAndCurrency()
	if strings.TrimSpace(sourceEventID) == "" {
		sourceEventID = paymentevents.EventIDFromBytes(raw)
	}
	providerTime := parseNotificationTime(n.Time)
	if providerTime.IsZero() {
		providerTime = received.UTC()
	}
	return PaymentEvent{
		Source:            paymentevents.SourcePush,
		Provider:          providerFromNotification(n),
		SourceEventID:     strings.TrimSpace(sourceEventID),
		Sender:            n.senderName(),
		AmountMinor:       int64(math.Round(amount * 100)),
		Currency:          strings.ToUpper(strings.TrimSpace(currency)),
		Description:       strings.TrimSpace(n.Title + " " + n.Text),
		ProviderTimestamp: providerTime,
		ReceivedTimestamp: received.UTC(),
		RawMessageHash:    paymentevents.EventIDFromBytes(raw),
	}
}

func providerFromNotification(n Notification) string {
	if provider := paymentevents.NormalizeProvider(n.App); provider == "WISE" || provider == "PayPal" || provider == "Revolut" || provider == "N26" {
		return provider
	}
	content := strings.ToLower(n.App + " " + n.Title + " " + n.Text)
	for _, candidate := range []string{"paypal", "wise", "revolut", "n26"} {
		if strings.Contains(content, candidate) {
			return paymentevents.NormalizeProvider(candidate)
		}
	}
	return paymentevents.NormalizeProvider(n.App)
}

func parseNotificationTime(value string) time.Time {
	value = strings.TrimSpace(value)
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}
