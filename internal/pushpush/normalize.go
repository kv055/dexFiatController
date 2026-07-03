package pushpush

import (
	"regexp"
	"strconv"
	"strings"
)

// amountRe matches a fiat amount with optional currency symbol/code, e.g. "$1,234.56",
// "€10", "100.00 EUR".
var (
	amountRe   = regexp.MustCompile(`[\$€£]?\s?\d[\d,]*\.?\d*\s?(USD|EUR|GBP)?`)
	nameRe     = regexp.MustCompile(`\b([A-Z][a-z]+(\s[A-Z][a-z]+)+)\b`) // "Jane Doe"
	multiSpace = regexp.MustCompile(`\s+`)
)

// currencySymbol maps a detected currency symbol to its ISO code.
var currencySymbol = map[string]string{
	"$": "USD", "€": "EUR", "£": "GBP",
}

// currencyWord maps a trailing currency word to its ISO code.
var currencyWord = map[string]string{
	"usd": "USD", "eur": "EUR", "gbp": "GBP",
}

// parseAmount extracts the first plausible numeric amount from text and its currency
// code (uppercased) if determinable. Returns (0, "") when no amount is found.
func parseAmount(text string) (float64, string) {
	loc := amountRe.FindStringSubmatchIndex(text)
	if loc == nil {
		return 0, ""
	}
	match := strings.TrimSpace(text[loc[0]:loc[1]])
	if match == "" {
		return 0, ""
	}

	currency := ""
	for sym, code := range currencySymbol {
		if strings.HasPrefix(match, sym) {
			currency = code
			break
		}
	}
	if currency == "" && loc[3] >= 0 {
		if code, ok := currencyWord[strings.ToLower(strings.TrimSpace(text[loc[2]:loc[3]]))]; ok {
			currency = code
		}
	}

	cleaned := strings.NewReplacer(",", "", "$", "", "€", "", "£", "", "USD", "", "EUR", "", "GBP", "").Replace(match)
	cleaned = strings.TrimSpace(cleaned)
	amount, err := strconv.ParseFloat(cleaned, 64)
	if err != nil {
		return 0, currency
	}
	return amount, currency
}

// parseSenderName extracts the first "First Last" proper name from text, or "" if none.
func parseSenderName(text string) string {
	m := nameRe.FindStringSubmatch(text)
	if len(m) >= 2 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// normalize strips amounts and likely proper names from text, lowercases it, and collapses
// whitespace. The resulting "shape" is stable for a given notification type so future
// messages of the same shape match the same allow/deny rule.
func normalize(text string) string {
	t := amountRe.ReplaceAllString(text, "")
	t = nameRe.ReplaceAllString(t, "")
	t = strings.ToLower(t)
	t = multiSpace.ReplaceAllString(t, " ")
	return strings.TrimSpace(t)
}
