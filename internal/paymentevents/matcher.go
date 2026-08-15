package paymentevents

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

const (
	directionBid       = "bid"
	nameScoreThreshold = 0.6
)

// Candidate is a pending fiat transaction joined to the canonical payment
// account selected before the trade started.
type Candidate struct {
	ID                      uint
	FiatAmount              float64
	FiatCurrency            string
	Dex                     string
	PaymentMethod           string
	AccountID               int64
	AccountProvider         string
	NotificationChannels    []string
	PendingSince            time.Time
	CounterpartyPaymentInfo json.RawMessage
}

// ListPending returns only bid transactions that still await fiat. Account
// linkage resolves through payment_methods.account_id for both Fiat
// Transmitter and Bank catalog methods.
func (s *EventStore) ListPending(ctx context.Context) ([]Candidate, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.id, t.fiat_amount, t.fiat_currency, COALESCE(t.dex::text, ''),
		       t.counterparty_payment_info, COALESCE(t.payment_method, ''),
		       COALESCE(a.id, 0), COALESCE(a.provider, ''),
		       COALESCE(array_to_string(a.notification_channels::text[], ','), ''),
		       COALESCE(NULLIF(t.dex_offer_taken, '0001-01-01'::timestamptz), t.created_at)
		FROM transactions t
		LEFT JOIN payment_methods pm ON lower(pm.name)=lower(t.payment_method)
		LEFT JOIN accounts a ON a.id=pm.account_id AND a.is_active
		WHERE t.status=$1 AND t.direction=$2 AND t.deleted_at IS NULL AND pm.is_active
	`, statusDexPositionTaken, directionBid)
	if err != nil {
		return nil, fmt.Errorf("payment events: list pending fiat: %w", err)
	}
	defer rows.Close()

	var out []Candidate
	for rows.Next() {
		var candidate Candidate
		var raw sql.NullString
		var channels string
		if err := rows.Scan(
			&candidate.ID, &candidate.FiatAmount, &candidate.FiatCurrency,
			&candidate.Dex, &raw, &candidate.PaymentMethod,
			&candidate.AccountID, &candidate.AccountProvider, &channels,
			&candidate.PendingSince,
		); err != nil {
			return nil, fmt.Errorf("payment events: scan pending fiat: %w", err)
		}
		candidate.NotificationChannels = SplitChannels(channels)
		if raw.Valid && raw.String != "" {
			candidate.CounterpartyPaymentInfo = json.RawMessage(raw.String)
		}
		out = append(out, candidate)
	}
	return out, rows.Err()
}

type Matcher struct{}

func NewMatcher() *Matcher { return &Matcher{} }

// Match applies the shared account, channel, timestamp, amount, currency,
// sender and reference rules identically for push and email events.
func (m *Matcher) Match(event PaymentEvent, candidates []Candidate, now time.Time) (*Candidate, []Candidate) {
	if event.AmountMinor <= 0 || strings.TrimSpace(event.Currency) == "" {
		return nil, nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var eligible []Candidate
	for _, candidate := range candidates {
		if candidate.AccountID == 0 || !ContainsChannel(candidate.NotificationChannels, event.Source) {
			continue
		}
		if event.AccountID != nil && candidate.AccountID != *event.AccountID {
			continue
		}
		if !strings.EqualFold(NormalizeProvider(event.Provider), NormalizeProvider(candidate.AccountProvider)) {
			continue
		}
		if !event.ProviderTimestamp.IsZero() &&
			(event.ProviderTimestamp.Before(candidate.PendingSince.Add(-2*time.Minute)) ||
				event.ProviderTimestamp.After(now.Add(5*time.Minute))) {
			continue
		}
		if !sameCurrency(candidate.FiatCurrency, event.Currency) ||
			int64(math.Round(candidate.FiatAmount*100)) != event.AmountMinor {
			continue
		}
		eligible = append(eligible, candidate)
	}
	if len(eligible) == 0 {
		return nil, nil
	}
	if len(eligible) == 1 {
		return &eligible[0], nil
	}
	best, tied := bestCandidateByEvidence(event, eligible)
	if best != nil && !tied {
		return best, nil
	}
	return nil, eligible
}

func EligibilityStatus(event PaymentEvent, candidates []Candidate, now time.Time) (string, string) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if !event.ProviderTimestamp.IsZero() && event.ProviderTimestamp.After(now.Add(5*time.Minute)) {
		return "rejected", "provider timestamp is outside the allowed future clock skew"
	}
	providerSeen, stale := false, false
	for _, candidate := range candidates {
		if event.AccountID != nil && candidate.AccountID != *event.AccountID {
			continue
		}
		if !strings.EqualFold(NormalizeProvider(event.Provider), NormalizeProvider(candidate.AccountProvider)) {
			continue
		}
		providerSeen = true
		if ContainsChannel(candidate.NotificationChannels, event.Source) {
			if !event.ProviderTimestamp.IsZero() &&
				int64(math.Round(candidate.FiatAmount*100)) == event.AmountMinor &&
				sameCurrency(candidate.FiatCurrency, event.Currency) &&
				event.ProviderTimestamp.Before(candidate.PendingSince.Add(-2*time.Minute)) {
				stale = true
				continue
			}
			return "unmatched", ""
		}
	}
	if stale {
		return "rejected", "payment predates the pending transaction"
	}
	if providerSeen {
		return "rejected", fmt.Sprintf("%s channel is not selected on the linked payment account", event.Source)
	}
	return "unmatched", ""
}

func bestCandidateByEvidence(event PaymentEvent, candidates []Candidate) (*Candidate, bool) {
	bestIndex, bestScore, tied := -1, 0.0, false
	description := normalizeText(event.Description)
	for index, candidate := range candidates {
		score := nameSimilarity(event.Sender, extractHolderName(candidate.CounterpartyPaymentInfo))
		rawEvidence := normalizeText(string(candidate.CounterpartyPaymentInfo))
		for _, token := range strings.Fields(description) {
			if len(token) >= 5 && strings.Contains(rawEvidence, token) {
				score += 0.15
				break
			}
		}
		switch {
		case score > bestScore:
			bestIndex, bestScore, tied = index, score, false
		case score == bestScore && score > 0:
			tied = true
		}
	}
	if bestIndex < 0 || bestScore < nameScoreThreshold {
		return nil, true
	}
	picked := candidates[bestIndex]
	return &picked, tied
}

var (
	multiSpace = regexp.MustCompile(`\s+`)
	nameKeys   = map[string]bool{
		"name": true, "holder": true, "account_holder": true, "accountholder": true,
		"full_name": true, "fullname": true, "holder_name": true, "holdername": true,
		"sender_name": true, "sendername": true, "sender": true, "buyer": true,
		"counterparty": true, "owner": true, "user": true, "username": true,
		"real_name": true, "realname": true, "display_name": true, "displayname": true,
	}
)

func extractHolderName(raw json.RawMessage) string {
	var value any
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return findName(value)
}

func findName(value any) string {
	switch typed := value.(type) {
	case map[string]any:
		for key, inner := range typed {
			if nameKeys[strings.ToLower(strings.TrimSpace(key))] {
				if text, ok := inner.(string); ok && len(strings.TrimSpace(text)) >= 2 {
					return text
				}
			}
		}
		for _, inner := range typed {
			if name := findName(inner); name != "" {
				return name
			}
		}
	case []any:
		for _, inner := range typed {
			if name := findName(inner); name != "" {
				return name
			}
		}
	}
	return ""
}

func normalizeText(value string) string {
	return strings.TrimSpace(multiSpace.ReplaceAllString(strings.ToLower(value), " "))
}

func nameSimilarity(a, b string) float64 {
	a, b = normalizeText(a), normalizeText(b)
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 1
	}
	if strings.Contains(a, b) || strings.Contains(b, a) {
		return 0.9
	}
	aTokens, bTokens := strings.Fields(a), strings.Fields(b)
	matched := 0
	for _, left := range aTokens {
		for _, right := range bTokens {
			if left == right {
				matched++
				break
			}
		}
	}
	maxLen := len(aTokens)
	if len(bTokens) > maxLen {
		maxLen = len(bTokens)
	}
	return float64(matched) / float64(maxLen)
}

func sameCurrency(a, b string) bool {
	a, b = strings.ToUpper(strings.TrimSpace(a)), strings.ToUpper(strings.TrimSpace(b))
	return a != "" && b != "" && a == b
}
