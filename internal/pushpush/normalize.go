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

// parseAmount extracts a plausible numeric amount from text and its currency
// code (uppercased) if determinable. Returns (0, "") when no amount is found.
//
// It scans all candidate matches and prefers one carrying a currency symbol or
// trailing currency code (e.g. "€1", "100.00 EUR"). Digit runs glued to a
// preceding letter — such as the "26" in the bank-app name "N26" — are skipped
// so they don't shadow the real amount.
func parseAmount(text string) (float64, string) {
	matches := amountRe.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return 0, ""
	}

	type cand struct {
		loc      []int
		currency string
		hasSym   bool
	}

	var best *cand
	var fallback *cand
	for _, loc := range matches {
		match := strings.TrimSpace(text[loc[0]:loc[1]])
		if match == "" {
			continue
		}
		// Locate the first digit inside the match; the regex may start at a
		// leading space or currency symbol, so loc[0] is not necessarily the
		// digit position.
		di := loc[0]
		for di < loc[1] && (text[di] < '0' || text[di] > '9') {
			di++
		}
		// Skip digit runs immediately preceded by a letter (e.g. "N26", "IBAN123").
		if di > loc[0] && di > 0 {
			if r := rune(text[di-1]); (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				continue
			}
		}

		currency := ""
		hasSym := false
		for sym, code := range currencySymbol {
			if strings.HasPrefix(match, sym) {
				currency = code
				hasSym = true
				break
			}
		}
		if currency == "" && loc[3] >= 0 {
			if code, ok := currencyWord[strings.ToLower(strings.TrimSpace(text[loc[2]:loc[3]]))]; ok {
				currency = code
			}
		}

		c := &cand{loc: loc, currency: currency, hasSym: hasSym}
		if best == nil || (hasSym && !best.hasSym) {
			best = c
		}
		if fallback == nil {
			fallback = c
		}
	}

	chosen := best
	if chosen == nil {
		chosen = fallback
	}
	if chosen == nil {
		return 0, ""
	}

	match := strings.TrimSpace(text[chosen.loc[0]:chosen.loc[1]])
	currency := chosen.currency

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
