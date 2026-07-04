package pushpush

import (
	"encoding/json"
	"strings"
)

// Notification is a payment notification forwarded by a bank/provider app
// (e.g. Revolut, N26) via a push forwarder such as MacroDroid. The amount,
// currency, and sender are not provided as structured fields — they are mined
// from Title+Text via regex (see parseAmount/parseSenderName).
type Notification struct {
	App   string `json:"app"`
	Title string `json:"title"`
	Text  string `json:"text"`
	Time  string `json:"time"`
}

// amountTolerance is the maximum fiat-amount delta (in major currency units) for a match.
const amountTolerance = 0.01

// nameScoreThreshold is the minimum normalized name-similarity score (0..1) to treat a
// sender name as a confident confirmation rather than just a tie-breaker.
const nameScoreThreshold = 0.6

// Matcher pairs a notification with a pending bid transaction.
type Matcher struct{}

// NewMatcher creates a Matcher.
func NewMatcher() *Matcher { return &Matcher{} }

// Match returns the single confident candidate, or nil. When multiple candidates match
// on amount+currency and none is a clear name winner, the ambiguous set is returned so
// the caller can escalate a human pick. candidate is nil whenever len(ambiguous) > 0.
func (m *Matcher) Match(notif Notification, candidates []Candidate) (candidate *Candidate, ambiguous []Candidate) {
	amount, currency := notif.amountAndCurrency()
	sender := notif.senderName()

	// Filter by currency (case-insensitive) then amount within tolerance.
	var currencyMatches []Candidate
	for _, c := range candidates {
		if !sameCurrency(c.FiatCurrency, currency) {
			continue
		}
		currencyMatches = append(currencyMatches, c)
	}
	if len(currencyMatches) == 0 {
		return nil, nil
	}

	var amountMatches []Candidate
	for _, c := range currencyMatches {
		if abs(c.FiatAmount-amount) <= amountTolerance {
			amountMatches = append(amountMatches, c)
		}
	}
	if len(amountMatches) == 0 {
		return nil, nil
	}
	if len(amountMatches) == 1 {
		return &amountMatches[0], nil
	}

	// Multiple amount+currency matches: score by sender-name similarity against the
	// counterparty payment info stored on each candidate.
	if strings.TrimSpace(sender) == "" {
		// No sender to disambiguate → escalate all amount matches.
		return nil, amountMatches
	}

	type scored struct {
		c     Candidate
		score float64
	}
	var picks []scored
	for _, c := range amountMatches {
		holder := extractHolderName(c.CounterpartyPaymentInfo)
		picks = append(picks, scored{c: c, score: nameSimilarity(sender, holder)})
	}

	// Find the best score.
	bestIdx := -1
	var bestScore float64
	tied := false
	for i, p := range picks {
		if p.score > bestScore {
			bestIdx = i
			bestScore = p.score
			tied = false
			continue
		}
		if p.score == bestScore && p.score > 0 {
			tied = true
		}
	}

	if bestIdx >= 0 && !tied && bestScore >= nameScoreThreshold {
		c := picks[bestIdx].c
		return &c, nil
	}
	return nil, amountMatches
}

// amountAndCurrency extracts the amount and currency from Title+Text via regex.
func (n Notification) amountAndCurrency() (float64, string) {
	a, c := parseAmount(n.Title + " " + n.Text)
	return a, strings.ToUpper(strings.TrimSpace(c))
}

// senderName extracts the sender's name from Title+Text via regex.
func (n Notification) senderName() string {
	return parseSenderName(n.Title + " " + n.Text)
}

func sameCurrency(a, b string) bool {
	a = strings.ToUpper(strings.TrimSpace(a))
	b = strings.ToUpper(strings.TrimSpace(b))
	if a == "" || b == "" {
		// An unknown currency on either side is treated as compatible so a forwarder
		// that omits currency still matches; amount+name must then carry the weight.
		return true
	}
	return a == b
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// holderNameKeys are the JSON keys searched (case-insensitively, recursively) when
// extracting a human holder/sender name from counterparty_payment_info.
var holderNameKeys = map[string]bool{
	"name": true, "holder": true, "account_holder": true, "accountholder": true,
	"full_name": true, "fullname": true, "holder_name": true, "holdername": true,
	"sender_name": true, "sendername": true, "sender": true, "buyer": true,
	"counterparty": true, "owner": true, "user": true, "username": true,
	"real_name": true, "realname": true, "display_name": true, "displayname": true,
}

// extractHolderName walks a counterparty_payment_info jsonb blob and returns the first
// string value found under any holder-name key. Returns "" when nothing plausible is
// present (the model comment notes the shape is venue-specific).
func extractHolderName(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var anyVal any
	if err := json.Unmarshal(raw, &anyVal); err != nil {
		return ""
	}
	if name := findName(anyVal); name != "" {
		return name
	}
	return ""
}

func findName(v any) string {
	switch val := v.(type) {
	case map[string]any:
		for k, inner := range val {
			if holderNameKeys[strings.ToLower(strings.TrimSpace(k))] {
				if s, ok := inner.(string); ok && isPlausibleName(s) {
					return s
				}
			}
		}
		// Recurse into nested objects/arrays for venue-specific shapes.
		for _, inner := range val {
			if name := findName(inner); name != "" {
				return name
			}
		}
	case []any:
		for _, inner := range val {
			if name := findName(inner); name != "" {
				return name
			}
		}
	}
	return ""
}

func isPlausibleName(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 2 {
		return false
	}
	for _, r := range s {
		if !(r == ' ' || r == '-' || r == '.' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			return false
		}
	}
	return true
}

// normalizeName lowercases, trims, and collapses whitespace.
func normalizeName(s string) string {
	return strings.TrimSpace(multiSpace.ReplaceAllString(strings.ToLower(s), " "))
}

// nameSimilarity returns a 0..1 similarity between two names. Empty inputs score 0.
// Order-independent: "jane doe" matches "doe jane".
func nameSimilarity(a, b string) float64 {
	a = normalizeName(a)
	b = normalizeName(b)
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 1
	}
	if strings.Contains(a, b) || strings.Contains(b, a) {
		return 0.9
	}
	// Token overlap (order-independent name parts).
	at := strings.Fields(a)
	bt := strings.Fields(b)
	if len(at) == 0 || len(bt) == 0 {
		return 0
	}
	matched := 0
	for _, x := range at {
		for _, y := range bt {
			if x == y {
				matched++
				break
			}
		}
	}
	maxLen := len(at)
	if len(bt) > maxLen {
		maxLen = len(bt)
	}
	return float64(matched) / float64(maxLen)
}
