package pushpush

import (
	"encoding/json"
	"math"
	"strings"
	"time"

	"dexFiatController/internal/paymentevents"
)

// PaymentEvent is an alias retained for pending audit structures and tests.
type PaymentEvent = paymentevents.PaymentEvent

// PaymentSignal converts a transport-level push notification into the neutral
// source output consumed by paymentingestion. It does not classify, persist,
// match, escalate, or release the signal.
func PaymentSignal(n Notification, raw []byte, sourceEventID string, received time.Time, auditID int64) paymentevents.InboundSignal {
	amount, currency := n.amountAndCurrency()
	if strings.TrimSpace(sourceEventID) == "" {
		sourceEventID = paymentevents.EventIDFromBytes(raw)
	}
	providerTime := parseNotificationTime(n.Time)
	if providerTime.IsZero() {
		providerTime = received.UTC()
	}
	var audit json.RawMessage
	if auditID > 0 {
		audit, _ = json.Marshal(map[string]int64{"push_log_id": auditID})
	}
	return paymentevents.InboundSignal{
		Event: PaymentEvent{
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
			AuditMetadata:     audit,
		},
		Presentation: paymentevents.Presentation{
			App: n.App, Title: n.Title, Text: n.Text,
			Normalized: normalize(n.Title + " " + n.Text),
		},
		Validation: paymentevents.Validation{Accepted: true},
		AuditID:    auditID,
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
