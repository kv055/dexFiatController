package pushpush

import "strings"

// Notification is the transport payload forwarded by a provider app through a
// push forwarder such as MacroDroid.
type Notification struct {
	App   string `json:"app"`
	Title string `json:"title"`
	Text  string `json:"text"`
	Time  string `json:"time"`
}

func (n Notification) amountAndCurrency() (float64, string) {
	amount, currency := parseAmount(n.Title + " " + n.Text)
	return amount, strings.ToUpper(strings.TrimSpace(currency))
}

func (n Notification) senderName() string {
	return parseSenderName(n.Title + " " + n.Text)
}

func abs(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}
