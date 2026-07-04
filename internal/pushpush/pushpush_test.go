package pushpush

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestParseAmount(t *testing.T) {
	cases := []struct {
		text     string
		amount   float64
		currency string
	}{
		{"You received €1,234.56 from Jane Doe", 1234.56, "EUR"},
		{"$10.00 payment", 10.00, "USD"},
		{"100.00 GBP received", 100.00, "GBP"},
		{"no money here", 0, ""},
	}
	for _, c := range cases {
		a, cur := parseAmount(c.text)
		if abs(a-c.amount) > 0.0001 {
			t.Errorf("parseAmount(%q) amount = %v, want %v", c.text, a, c.amount)
		}
		if cur != c.currency {
			t.Errorf("parseAmount(%q) currency = %q, want %q", c.text, cur, c.currency)
		}
	}
}

func TestNormalizeStripsAmountAndNames(t *testing.T) {
	got := normalize("You received €1,234.56 from Jane Doe - rent")
	if want := "you received from - rent"; got != want {
		t.Errorf("normalize = %q, want %q", got, want)
	}
}

func TestMatcherSingleMatch(t *testing.T) {
	candidates := []Candidate{
		{ID: 1, FiatAmount: 100.00, FiatCurrency: "EUR", Dex: "HodlHodl"},
		{ID: 2, FiatAmount: 50.00, FiatCurrency: "EUR", Dex: "Bisq"},
	}
	m := NewMatcher()
	n := Notification{App: "bank", Title: "Received", Text: "€100.00 from Jane Doe"}
	candidate, ambiguous := m.Match(n, candidates)
	if candidate == nil || candidate.ID != 1 {
		t.Fatalf("expected single match on tx 1, got candidate=%v ambiguous=%v", candidate, ambiguous)
	}
	if len(ambiguous) != 0 {
		t.Fatalf("expected no ambiguous, got %v", ambiguous)
	}
}

func TestMatcherAmountFromText(t *testing.T) {
	candidates := []Candidate{
		{ID: 7, FiatAmount: 42.50, FiatCurrency: "USD", Dex: "RoboSats"},
	}
	m := NewMatcher()
	// Amount and currency are mined from Title+Text, not structured fields.
	n := Notification{App: "bank", Title: "Payment", Text: "$42.50 received"}
	candidate, _ := m.Match(n, candidates)
	if candidate == nil || candidate.ID != 7 {
		t.Fatalf("expected match on tx 7 via text-parsed amount, got %v", candidate)
	}
}

func TestMatcherAmbiguousEscalates(t *testing.T) {
	candidates := []Candidate{
		{ID: 1, FiatAmount: 100.00, FiatCurrency: "EUR", Dex: "HodlHodl"},
		{ID: 2, FiatAmount: 100.00, FiatCurrency: "EUR", Dex: "Bisq"},
	}
	m := NewMatcher()
	// No sender name → cannot disambiguate.
	n := Notification{App: "bank", Title: "Payment", Text: "100.00 EUR"}
	candidate, ambiguous := m.Match(n, candidates)
	if candidate != nil {
		t.Fatalf("expected no confident candidate, got %v", candidate)
	}
	if len(ambiguous) != 2 {
		t.Fatalf("expected 2 ambiguous, got %d", len(ambiguous))
	}
}

func TestMatcherNameDisambiguation(t *testing.T) {
	c1 := Candidate{ID: 1, FiatAmount: 100.00, FiatCurrency: "EUR", Dex: "HodlHodl",
		CounterpartyPaymentInfo: json.RawMessage(`{"account_holder":"Jane Doe","iban":"DE00"}`)}
	c2 := Candidate{ID: 2, FiatAmount: 100.00, FiatCurrency: "EUR", Dex: "Bisq",
		CounterpartyPaymentInfo: json.RawMessage(`{"name":"Bob Smith"}`)}
	m := NewMatcher()
	n := Notification{App: "bank", Title: "Received", Text: "€100.00 from Jane Doe"}
	candidate, ambiguous := m.Match(n, []Candidate{c1, c2})
	if candidate == nil || candidate.ID != 1 {
		t.Fatalf("expected name disambiguation to pick tx 1, got candidate=%v ambiguous=%v", candidate, ambiguous)
	}
}

func TestMatcherNoCandidate(t *testing.T) {
	candidates := []Candidate{
		{ID: 1, FiatAmount: 100.00, FiatCurrency: "EUR", Dex: "HodlHodl"},
	}
	m := NewMatcher()
	n := Notification{App: "bank", Title: "Received", Text: "€999.00"}
	candidate, ambiguous := m.Match(n, candidates)
	if candidate != nil {
		t.Fatalf("expected no candidate for unmatched amount, got %v", candidate)
	}
	if len(ambiguous) != 0 {
		t.Fatalf("expected no ambiguous for unmatched amount, got %v", ambiguous)
	}
}

func TestExtractHolderNameNested(t *testing.T) {
	raw := json.RawMessage(`{"exchange":"hodlhodl","raw":{"counterparty":{"full_name":"Alice Example"}}}`)
	if got := extractHolderName(raw); got != "Alice Example" {
		t.Errorf("extractHolderName nested = %q, want %q", got, "Alice Example")
	}
}

func TestNameSimilarity(t *testing.T) {
	cases := []struct {
		a, b string
		want float64
	}{
		{"Jane Doe", "jane doe", 1.0},
		{"Jane Doe", "Doe Jane", 1.0}, // order-independent token overlap
		{"Jane Doe", "Jane", 0.9},     // contains
		{"Jane Doe", "Bob Smith", 0},
	}
	for _, c := range cases {
		got := nameSimilarity(c.a, c.b)
		if abs(got-c.want) > 0.01 {
			t.Errorf("nameSimilarity(%q,%q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestRuleStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rules.json")

	rs := NewRuleStore(path)
	rs.Add("BankApp", "incoming wire from", true)
	rs.Add("BankApp", "marketing newsletter", false)

	allowed, denied := rs.Match("bankapp", "incoming wire from jane doe")
	if !allowed || denied {
		t.Errorf("expected allowed match, got allowed=%v denied=%v", allowed, denied)
	}
	allowed, denied = rs.Match("bankapp", "marketing newsletter q3")
	if allowed || !denied {
		t.Errorf("expected denied match, got allowed=%v denied=%v", allowed, denied)
	}
	allowed, denied = rs.Match("bankapp", "unrelated text")
	if allowed || denied {
		t.Errorf("expected no match, got allowed=%v denied=%v", allowed, denied)
	}

	// Reload from disk to verify persistence.
	rs2 := NewRuleStore(path)
	allowed, denied = rs2.Match("bankapp", "incoming wire from x")
	if !allowed {
		t.Errorf("expected persisted allow rule to match after reload, got allowed=%v", allowed)
	}
}

func TestRuleStoreDenyPrecedence(t *testing.T) {
	rs := NewRuleStore("")
	rs.Add("app", "payment", true)
	rs.Add("app", "payment spam", false)
	allowed, denied := rs.Match("app", "payment spam q3")
	if allowed || !denied {
		t.Fatalf("deny must take precedence, got allowed=%v denied=%v", allowed, denied)
	}
}

func TestPendingStoreAddGet(t *testing.T) {
	p := NewPendingStore()
	id := p.Add(PendingEntry{Notification: Notification{App: "bank"}, Kind: pendingClassify})
	if id == "" {
		t.Fatal("expected non-empty id")
	}
	entry, ok := p.Get(id)
	if !ok {
		t.Fatalf("expected to get entry for %s", id)
	}
	if entry.Notification.App != "bank" {
		t.Errorf("got app %q", entry.Notification.App)
	}
	if _, ok := p.Get(id); ok {
		t.Fatal("expected Get to consume the entry")
	}
}

// TestLogStoreInsertUpdate exercises the fiat_tx_push_logs round-trip against a real
// Postgres. Skipped unless PUSHPUSH_TEST_DB is set (same env as TestMarkFiatArrivedStatusGuard).
func TestLogStoreInsertUpdate(t *testing.T) {
	dsn := os.Getenv("PUSHPUSH_TEST_DB")
	if dsn == "" {
		t.Skip("set PUSHPUSH_TEST_DB to run the Postgres integration test")
	}
	store, err := OpenTxStore(dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	logs, err := NewLogStore(store.DB())
	if err != nil {
		t.Fatalf("new log store: %v", err)
	}
	ctx := context.Background()

	n := Notification{App: "bankapp", Title: "Received", Text: "€100.00 from Jane Doe"}
	id, err := logs.Insert(ctx, n, json.RawMessage(`{"app":"bankapp"}`), "received from")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	defer func() { _, _ = store.DB().ExecContext(ctx, `DELETE FROM fiat_tx_push_logs WHERE id = $1`, id) }()

	// Verify initial row.
	var status, ruleDecision sql.NullString
	var matched sql.NullInt64
	if err = store.DB().QueryRowContext(ctx,
		`SELECT status, rule_decision, matched_tx_id FROM fiat_tx_push_logs WHERE id = $1`, id,
	).Scan(&status, &ruleDecision, &matched); err != nil {
		t.Fatalf("query inserted row: %v", err)
	}
	if status.String != string(LogReceived) {
		t.Errorf("initial status = %q, want %q", status.String, LogReceived)
	}
	if matched.Valid {
		t.Errorf("expected no matched_tx_id initially, got %d", matched.Int64)
	}

	// Advance through stages: matched → released.
	txID := uint(42)
	if err := logs.Update(ctx, id, LogUpdate{Status: LogPicked, MatchedTxID: &txID}); err != nil {
		t.Fatalf("update picked: %v", err)
	}
	if err := logs.Update(ctx, id, LogUpdate{Status: LogReleased}); err != nil {
		t.Fatalf("update released: %v", err)
	}

	if err = store.DB().QueryRowContext(ctx,
		`SELECT status, rule_decision, matched_tx_id, error FROM fiat_tx_push_logs WHERE id = $1`, id,
	).Scan(&status, &ruleDecision, &matched, new(sql.NullString)); err != nil {
		t.Fatalf("query advanced row: %v", err)
	}
	if status.String != string(LogReleased) {
		t.Errorf("advanced status = %q, want %q", status.String, LogReleased)
	}
	if !matched.Valid || matched.Int64 != 42 {
		t.Errorf("expected matched_tx_id carried forward as 42, got %v", matched)
	}

	// Error field overwrites and clears.
	if err := logs.Update(ctx, id, LogUpdate{Status: LogReleaseError, Error: "boom"}); err != nil {
		t.Fatalf("update error: %v", err)
	}
	var errMsg sql.NullString
	if err = store.DB().QueryRowContext(ctx,
		`SELECT error FROM fiat_tx_push_logs WHERE id = $1`, id,
	).Scan(&errMsg); err != nil {
		t.Fatalf("query error: %v", err)
	}
	if !errMsg.Valid || errMsg.String != "boom" {
		t.Errorf("expected error 'boom', got %v", errMsg)
	}
	if err := logs.Update(ctx, id, LogUpdate{Status: LogReleased, Error: ""}); err != nil {
		t.Fatalf("clear error: %v", err)
	}
	if err = store.DB().QueryRowContext(ctx,
		`SELECT error FROM fiat_tx_push_logs WHERE id = $1`, id,
	).Scan(&errMsg); err != nil {
		t.Fatalf("query cleared error: %v", err)
	}
	if errMsg.Valid {
		t.Errorf("expected error cleared, got %q", errMsg.String)
	}
}

// TestMarkFiatArrivedStatusGuard exercises the idempotent status guard against a real
// Postgres. It is skipped unless PUSHPUSH_TEST_DB is set (points at a DB that already has
// the transactions schema). Follows the project's pure-unit-test default: no forced DB.
func TestMarkFiatArrivedStatusGuard(t *testing.T) {
	dsn := os.Getenv("PUSHPUSH_TEST_DB")
	if dsn == "" {
		t.Skip("set PUSHPUSH_TEST_DB to run the Postgres integration test")
	}
	store, err := OpenTxStore(dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	// Insert a minimal pending row.
	var id uint
	err = store.db.QueryRowContext(ctx, `
		INSERT INTO transactions (direction, fiat_amount, fiat_currency, status, transaction_complete)
		VALUES ($1, $2, $3, $4, FALSE)
		RETURNING id
	`, directionBUY, 12.34, "EUR", statusDexPositionTaken).Scan(&id)
	if err != nil {
		t.Fatalf("seed transaction: %v", err)
	}
	defer func() {
		_, _ = store.db.ExecContext(ctx, `DELETE FROM transactions WHERE id = $1`, id)
	}()

	advanced, err := store.MarkFiatArrived(ctx, id, json.RawMessage(`{"source":"test"}`))
	if err != nil {
		t.Fatalf("first MarkFiatArrived: %v", err)
	}
	if !advanced {
		t.Fatal("expected first MarkFiatArrived to advance the row")
	}
	// Second call must not advance (status is no longer DexPositionTaken).
	advanced, err = store.MarkFiatArrived(ctx, id, json.RawMessage(`{"source":"test"}`))
	if err != nil {
		t.Fatalf("second MarkFiatArrived: %v", err)
	}
	if advanced {
		t.Fatal("expected second MarkFiatArrived to be a no-op (status guard)")
	}
}

// TestExtractJSONObject covers forwarders that wrap the JSON payload in stray
// whitespace or prefix text (observed MacroDroid bodies).
func TestExtractJSONObject(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
		ok    bool
	}{
		{
			name:  "clean json",
			input: `{"app":"Revolut","title":"You received €1","text":"Payment received from Kilian Reimar Joergen Voss. Tap for details","time":"1783110191"}`,
			want:  `{"app":"Revolut","title":"You received €1","text":"Payment received from Kilian Reimar Joergen Voss. Tap for details","time":"1783110191"}`,
			ok:    true,
		},
		{
			name:  "leading newlines",
			input: "\n\n" + `{"app":"Revolut","title":"You received €1","text":"x","time":"1783110191"}`,
			want:  `{"app":"Revolut","title":"You received €1","text":"x","time":"1783110191"}`,
			ok:    true,
		},
		{
			name:  "prefix junk before json",
			input: "Kilian Voss has received your bank transfer of €1\n\n" + `{"app":"N26","title":"N26","text":"Kilian Voss has received your bank transfer of €1","time":"1783110200"}`,
			want:  `{"app":"N26","title":"N26","text":"Kilian Voss has received your bank transfer of €1","time":"1783110200"}`,
			ok:    true,
		},
		{
			name:  "braces inside string literals",
			input: `{"app":"N26","title":"N26","text":"You received €2 {promo} from Kilian Voss.","time":"1783110450"}`,
			want:  `{"app":"N26","title":"N26","text":"You received €2 {promo} from Kilian Voss.","time":"1783110450"}`,
			ok:    true,
		},
		{
			name:  "invalid json typo falls back",
			input: `{"app":"Revolut","title":"Transfer sent ✅","text":"You sent €2 to Kilian Reimar Joergen Voss.","date":"time":"1783110442"}`,
			ok:    false,
		},
		{
			name:  "no object at all",
			input: "just plain text no json here",
			ok:    false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := extractJSONObject([]byte(c.input))
			if ok != c.ok {
				t.Fatalf("ok: got %v want %v (got=%q)", ok, c.ok, got)
			}
			if ok && string(got) != c.want {
				t.Errorf("extracted: got %q want %q", got, c.want)
			}
		})
	}
}

// TestCapturedShapesParse verifies the two real captured notification shapes
// (Revolut and N26) parse into Notification and yield the expected amount,
// currency, and sender via the matcher's free-text extraction.
func TestCapturedShapesParse(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		app      string
		amount   float64
		currency string
		sender   string
	}{
		{
			name:     "revolut received",
			body:     `{"app":"Revolut","title":"You received €1","text":"Payment received from Kilian Reimar Joergen Voss. Tap for details","time":"1783110191"}`,
			app:      "Revolut",
			amount:   1,
			currency: "EUR",
			sender:   "Kilian Reimar Joergen Voss",
		},
		{
			name:     "n26 received with sender",
			body:     `{"app":"N26","title":"N26","text":"You received €2 from Kilian Voss.","time":"1783110450"}`,
			app:      "N26",
			amount:   2,
			currency: "EUR",
			sender:   "Kilian Voss",
		},
		{
			name:     "n26 received without sender in text",
			body:     `{"app":"N26","title":"N26","text":"Kilian Voss has received your bank transfer of €1","time":"1783110200"}`,
			app:      "N26",
			amount:   1,
			currency: "EUR",
			// This N26 notification type names the receiver (the user), not the
			// sender; the regex still extracts the name, but it is not a real
			// counterparty sender — matching relies on amount+currency instead.
			sender: "Kilian Voss",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			obj, ok := extractJSONObject([]byte(c.body))
			if !ok {
				t.Fatal("expected extractJSONObject to find the JSON object")
			}
			var n Notification
			if err := json.Unmarshal(obj, &n); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if n.App != c.app {
				t.Errorf("app: got %q want %q", n.App, c.app)
			}
			amt, cur := n.amountAndCurrency()
			if amt != c.amount || cur != c.currency {
				t.Errorf("amount/currency: got (%v,%q) want (%v,%q)", amt, cur, c.amount, c.currency)
			}
			if got := n.senderName(); got != c.sender {
				t.Errorf("sender: got %q want %q", got, c.sender)
			}
		})
	}
}

// forwarders (e.g. MacroDroid) that POST a raw plain-text body with no
// Content-Type. The whole body becomes the notification text and amount /
// sender must be mined from it via parseAmount and parseSenderName.
//
// Plain text is best-effort: when digits are glued to a name (e.g. "€1Kilian")
// the word-boundary name regex cannot recover the sender, so such notifications
// fall back to amount+currency matching and escalate to Telegram when
// ambiguous. Structured JSON avoids this entirely.
func TestPlainTextNotificationParsing(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		amount      float64
		currency    string
		sender      string
		senderExact bool // false when the sender is glued to digits and may not extract
	}{
		{
			name:        "revolut style",
			body:        "You received €1Payment received from Kilian Reimar Joergen Voss. Tap for details",
			amount:      1,
			currency:    "EUR",
			sender:      "Kilian Reimar Joergen Voss",
			senderExact: true,
		},
		{
			name:        "n26 style names receiver not sender",
			body:        "N26Kilian Voss has received your bank transfer of €1Kilian Voss has received your bank transfer of €1",
			amount:      1,
			currency:    "EUR",
			sender:      "",
			senderExact: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var n Notification
			if json.Valid([]byte(c.body)) {
				t.Fatal("plain-text body should not parse as JSON")
			}
			n = Notification{Text: c.body, App: appFromUserAgent("macrodroid/5.64.11")}

			amt, cur := parseAmount(n.Title + " " + n.Text)
			if amt != c.amount {
				t.Errorf("amount: got %v want %v", amt, c.amount)
			}
			if cur != c.currency {
				t.Errorf("currency: got %q want %q", cur, c.currency)
			}
			got := parseSenderName(n.Title + " " + n.Text)
			if c.senderExact {
				if got != c.sender {
					t.Errorf("sender: got %q want %q", got, c.sender)
				}
			} else if got != "" {
				t.Errorf("sender: expected empty (glued to digits, will escalate), got %q", got)
			}
			if n.App != "macrodroid" {
				t.Errorf("app: got %q want %q", n.App, "macrodroid")
			}
		})
	}
}
