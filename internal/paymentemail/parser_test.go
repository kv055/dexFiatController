package paymentemail

import (
	"os"
	"strings"
	"testing"
	"time"
)

func wiseFixture(body string) []byte {
	return []byte(strings.Join([]string{
		"From: Wise <notifications@wise.com>",
		"To: payments@example.test",
		"Subject: You received money",
		"Message-ID: <wise-event-1@example.test>",
		"Date: Thu, 23 Jul 2026 10:15:00 +0000",
		"Authentication-Results: mx.example.test; dkim=pass header.d=wise.com; spf=pass; dmarc=pass",
		"Content-Type: text/plain; charset=utf-8",
		"",
		body,
	}, "\r\n"))
}

func TestParseWiseTemplateAndLocaleAmounts(t *testing.T) {
	tests := []struct {
		amount string
		minor  int64
	}{
		{"1,234.56", 123456},
		{"1.234,56", 123456},
		{"1234,56", 123456},
	}
	for _, test := range tests {
		raw := wiseFixture("You received\r\nAmount: " + test.amount + " EUR\r\nFrom: Jane Doe\r\nReference: order-42")
		message, err := decodePaymentMail(raw)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		event, err := parseTemplateMail(message, time.Now())
		if err != nil {
			t.Fatalf("parse %s: %v", test.amount, err)
		}
		if event.Provider != "WISE" || event.AmountMinor != test.minor || event.Currency != "EUR" {
			t.Errorf("event = %+v", event)
		}
		if event.Sender != "Jane Doe" || event.Description != "order-42" {
			t.Errorf("sender/reference = %q/%q", event.Sender, event.Description)
		}
	}
}

func TestCommittedSanitizedFixtures(t *testing.T) {
	tests := []struct {
		path     string
		provider string
		minor    int64
	}{
		{"testdata/wise_received_v1.eml", "WISE", 12345},
		{"testdata/paypal_received_v1.eml", "PayPal", 6789},
	}
	for _, test := range tests {
		raw, err := os.ReadFile(test.path)
		if err != nil {
			t.Fatal(err)
		}
		message, err := decodePaymentMail(raw)
		if err != nil {
			t.Fatal(err)
		}
		event, err := parseTemplateMail(message, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if event.Provider != test.provider || event.AmountMinor != test.minor {
			t.Errorf("%s event = %+v", test.path, event)
		}
	}
}

func TestTemplateRejectsUnapprovedDKIMAndUnknownLayout(t *testing.T) {
	raw := strings.ReplaceAll(string(wiseFixture("You received\r\nAmount: 10.00 EUR\r\nFrom: Jane Doe\r\nReference: x")), "header.d=wise.com", "header.d=attacker.test")
	message, err := decodePaymentMail([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseTemplateMail(message, time.Now()); err == nil || !strings.Contains(err.Error(), "approved") {
		t.Fatalf("unapproved DKIM error = %v", err)
	}
	mixed := strings.ReplaceAll(
		string(wiseFixture("You received\r\nAmount: 10.00 EUR\r\nFrom: Jane Doe\r\nReference: x")),
		"dkim=pass header.d=wise.com",
		"dkim=fail header.d=wise.com; dkim=pass header.d=attacker.test",
	)
	message, err = decodePaymentMail([]byte(mixed))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseTemplateMail(message, time.Now()); err == nil {
		t.Fatal("passing attacker signature was paired with failing Wise signature")
	}

	unknown := wiseFixture("You received 10 EUR from Jane Doe")
	message, err = decodePaymentMail(unknown)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseTemplateMail(message, time.Now()); err == nil || !strings.Contains(err.Error(), "no active exact") {
		t.Fatalf("unknown layout error = %v", err)
	}
}

func TestHTMLNormalizationAndAttachmentsIgnored(t *testing.T) {
	raw := []byte(strings.Join([]string{
		"From: PayPal <service@paypal.com>",
		"To: payments@example.test",
		"Subject: You've got money",
		"Message-ID: <paypal-event-1@example.test>",
		"Date: Thu, 23 Jul 2026 10:15:00 +0000",
		"Authentication-Results: mx.example.test; dkim=pass header.d=paypal.com",
		"Content-Type: multipart/mixed; boundary=outer",
		"",
		"--outer",
		"Content-Type: text/html; charset=utf-8",
		"",
		"<div>Payment received</div><div>Amount: USD 12.34</div><div>From: John Smith</div><div>Transaction ID: PP-123</div>",
		"--outer",
		"Content-Type: text/plain",
		"Content-Disposition: attachment; filename=evil.txt",
		"",
		"Amount: USD 999999.99",
		"--outer--",
	}, "\r\n"))
	message, err := decodePaymentMail(raw)
	if err != nil {
		t.Fatal(err)
	}
	event, err := parseTemplateMail(message, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if event.AmountMinor != 1234 || event.Description != "PP-123" {
		t.Fatalf("event = %+v", event)
	}
}

func TestDecodeRejectsOversizedAndMalformedMail(t *testing.T) {
	if _, err := decodePaymentMail(make([]byte, maxPaymentEmailBytes+1)); err == nil {
		t.Fatal("oversized message accepted")
	}
	if _, err := decodePaymentMail([]byte("not a header")); err == nil {
		t.Fatal("malformed message accepted")
	}
}

func TestSignalExposesVerificationForNeutralCaller(t *testing.T) {
	now := time.Now().UTC()
	cfg := PaymentEmailConfig{
		Mode: Active, ActivationUID: 4,
		ActivationTime: now.Add(-time.Hour),
	}
	valid := Signal(
		wiseFixture("You received\r\nAmount: 10.00 EUR\r\nFrom: Jane Doe\r\nReference: x"),
		5, cfg, now,
	)
	if !valid.Validation.Accepted || valid.Event.Provider != "WISE" || valid.Rollout.UID != 5 {
		t.Fatalf("valid signal = %+v", valid)
	}

	raw := strings.ReplaceAll(
		string(wiseFixture("You received\r\nAmount: 10.00 EUR\r\nFrom: Jane Doe\r\nReference: x")),
		"header.d=wise.com", "header.d=attacker.test",
	)
	rejected := Signal([]byte(raw), 6, cfg, now)
	if rejected.Validation.Accepted || rejected.Validation.Reason == "" {
		t.Fatalf("rejected signal validation = %+v", rejected.Validation)
	}
	if rejected.Event.SourceEventID == "" || rejected.Event.RawMessageHash == "" {
		t.Fatalf("rejected signal lost audit identity: %+v", rejected.Event)
	}
}
