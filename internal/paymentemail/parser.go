package paymentemail

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"

	"dexFiatController/internal/paymentevents"
	messagecharset "github.com/emersion/go-message/charset"
	"golang.org/x/net/html"
)

const maxPaymentEmailBytes = 2 << 20

var multiSpace = regexp.MustCompile(`\s+`)

type mailTemplate struct {
	Version        string
	Provider       string
	SigningDomains []string
	Subject        *regexp.Regexp
	Body           *regexp.Regexp
}

// paymentMailTemplates is deliberately embedded and versioned. These strict
// layouts are test/capture-derived contracts; unmatched variants never reach
// the release pipeline and must be added as a new reviewed version.
var paymentMailTemplates = []mailTemplate{
	{
		Version: "wise.received.v1", Provider: "WISE",
		SigningDomains: []string{"wise.com"},
		Subject:        regexp.MustCompile(`(?i)^you received money$`),
		Body:           regexp.MustCompile(`(?is)^you received\r?\namount:\s*(?P<amount>[0-9][0-9., ]*)\s+(?P<currency>[A-Z]{3})\r?\nfrom:\s*(?P<sender>[^\r\n]+)\r?\nreference:\s*(?P<reference>[^\r\n]+)\s*$`),
	},
	{
		Version: "paypal.received.v1", Provider: "PayPal",
		SigningDomains: []string{"paypal.com"},
		Subject:        regexp.MustCompile(`(?i)^you've got money$`),
		Body:           regexp.MustCompile(`(?is)^payment received\r?\namount:\s*(?P<currency>[A-Z]{3})\s+(?P<amount>[0-9][0-9., ]*)\r?\nfrom:\s*(?P<sender>[^\r\n]+)\r?\ntransaction id:\s*(?P<reference>[A-Z0-9-]+)\s*$`),
	},
}

type decodedMail struct {
	Header      mail.Header
	Subject     string
	Plain       string
	HTMLText    string
	MessageID   string
	Date        time.Time
	RawHash     string
	RawSize     int
	AuthResults []string
}

type ParsedMessage struct {
	Event   paymentevents.PaymentEvent
	Subject string
}

// Capture decodes only safe metadata and a hash. It never template-parses an
// amount or emits release-capable fields.
func CaptureMessage(raw []byte, uid uint32, received time.Time) (paymentevents.PaymentEvent, error) {
	message, err := decodePaymentMail(raw)
	if err != nil {
		return paymentevents.PaymentEvent{
			Source: paymentevents.SourceEmail, Provider: "unknown",
			SourceEventID:     fmt.Sprintf("uid:%d:%s", uid, paymentevents.EventIDFromBytes(raw)),
			ReceivedTimestamp: received.UTC(), RawMessageHash: paymentevents.EventIDFromBytes(raw),
			AuthenticationOutcome: "decode_failed",
		}, err
	}
	audit, _ := json.Marshal(map[string]any{
		"uid": uid, "message_id": message.MessageID, "subject": message.Subject,
		"size": message.RawSize, "authentication_results_present": len(message.AuthResults) > 0,
	})
	return paymentevents.PaymentEvent{
		Source: paymentevents.SourceEmail, Provider: "unknown",
		SourceEventID: sourceIDForMail(message), ReceivedTimestamp: received.UTC(),
		AuditMetadata: audit, RawMessageHash: message.RawHash,
		AuthenticationOutcome: "capture_only",
	}, nil
}

// ParseMessage applies the exact embedded provider templates and DKIM-domain
// gate, returning only normalized event data to the neutral processor.
func ParseMessage(raw []byte, received time.Time) (ParsedMessage, error) {
	message, err := decodePaymentMail(raw)
	if err != nil {
		return ParsedMessage{
			Event: paymentevents.PaymentEvent{
				Source: paymentevents.SourceEmail, Provider: "unknown",
				SourceEventID:     paymentevents.EventIDFromBytes(raw),
				ReceivedTimestamp: received.UTC(), RawMessageHash: paymentevents.EventIDFromBytes(raw),
				AuthenticationOutcome: "decode_failed",
			},
		}, err
	}
	event, parseErr := parseTemplateMail(message, received)
	return ParsedMessage{Event: event, Subject: message.Subject}, parseErr
}

func decodePaymentMail(raw []byte) (decodedMail, error) {
	if len(raw) == 0 {
		return decodedMail{}, fmt.Errorf("empty message")
	}
	if len(raw) > maxPaymentEmailBytes {
		return decodedMail{}, fmt.Errorf("message exceeds %d bytes", maxPaymentEmailBytes)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return decodedMail{}, fmt.Errorf("read MIME message: %w", err)
	}
	decoder := &mime.WordDecoder{CharsetReader: messagecharset.Reader}
	subject, err := decoder.DecodeHeader(msg.Header.Get("Subject"))
	if err != nil {
		return decodedMail{}, fmt.Errorf("decode subject: %w", err)
	}
	plain, htmlText, err := readMailParts(msg.Header, msg.Body, int64(maxPaymentEmailBytes))
	if err != nil {
		return decodedMail{}, err
	}
	date, _ := msg.Header.Date()
	return decodedMail{
		Header:      msg.Header,
		Subject:     strings.TrimSpace(subject),
		Plain:       normalizeMailText(plain),
		HTMLText:    normalizeMailText(htmlText),
		MessageID:   strings.Trim(strings.TrimSpace(msg.Header.Get("Message-ID")), "<>"),
		Date:        date.UTC(),
		RawHash:     paymentevents.EventIDFromBytes(raw),
		RawSize:     len(raw),
		AuthResults: append([]string(nil), msg.Header["Authentication-Results"]...),
	}, nil
}

func readMailParts(header mail.Header, body io.Reader, remaining int64) (plain, htmlText string, err error) {
	mediaType, params, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil && header.Get("Content-Type") != "" {
		return "", "", fmt.Errorf("parse content type: %w", err)
	}
	if mediaType == "" {
		mediaType = "text/plain"
	}
	if strings.HasPrefix(strings.ToLower(header.Get("Content-Disposition")), "attachment") {
		return "", "", nil
	}
	if strings.HasPrefix(mediaType, "multipart/") {
		reader := multipart.NewReader(body, params["boundary"])
		for {
			part, nextErr := reader.NextPart()
			if nextErr == io.EOF {
				break
			}
			if nextErr != nil {
				return "", "", fmt.Errorf("read MIME part: %w", nextErr)
			}
			partHeader := mail.Header(part.Header)
			p, h, partErr := readMailParts(partHeader, part, remaining)
			part.Close()
			if partErr != nil {
				return "", "", partErr
			}
			if plain == "" && p != "" {
				plain = p
			}
			if htmlText == "" && h != "" {
				htmlText = h
			}
		}
		return plain, htmlText, nil
	}
	decoded := decodeTransferEncoding(body, header.Get("Content-Transfer-Encoding"))
	charsetName := params["charset"]
	if charsetName != "" && !strings.EqualFold(charsetName, "utf-8") && !strings.EqualFold(charsetName, "us-ascii") {
		decoded, err = messagecharset.Reader(charsetName, decoded)
		if err != nil {
			return "", "", fmt.Errorf("decode charset %q: %w", charsetName, err)
		}
	}
	raw, err := io.ReadAll(io.LimitReader(decoded, remaining+1))
	if err != nil {
		return "", "", fmt.Errorf("read MIME body: %w", err)
	}
	if int64(len(raw)) > remaining {
		return "", "", fmt.Errorf("decoded message exceeds size limit")
	}
	switch strings.ToLower(mediaType) {
	case "text/plain":
		return string(raw), "", nil
	case "text/html":
		text, err := htmlToText(raw)
		return "", text, err
	default:
		return "", "", nil
	}
}

func decodeTransferEncoding(reader io.Reader, encoding string) io.Reader {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, reader)
	case "quoted-printable":
		return quotedprintable.NewReader(reader)
	default:
		return reader
	}
}

func htmlToText(raw []byte) (string, error) {
	root, err := html.Parse(bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("parse HTML mail: %w", err)
	}
	var out strings.Builder
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.TextNode {
			out.WriteString(node.Data)
		}
		if node.Type == html.ElementNode {
			switch node.Data {
			case "br", "p", "div", "tr", "li":
				out.WriteByte('\n')
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
		if node.Type == html.ElementNode {
			switch node.Data {
			case "p", "div", "tr", "li":
				out.WriteByte('\n')
			}
		}
	}
	walk(root)
	return out.String(), nil
}

func normalizeMailText(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	lines := strings.Split(value, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(multiSpace.ReplaceAllString(line, " "))
		if line != "" {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

func parseTemplateMail(message decodedMail, received time.Time) (paymentevents.PaymentEvent, error) {
	for _, tmpl := range paymentMailTemplates {
		if !tmpl.Subject.MatchString(message.Subject) {
			continue
		}
		body := message.Plain
		if body == "" {
			body = message.HTMLText
		}
		match := tmpl.Body.FindStringSubmatch(body)
		if match == nil {
			continue
		}
		signingDomain, ok := authenticatedSigningDomain(message.AuthResults, tmpl.SigningDomains)
		if !ok {
			return paymentevents.PaymentEvent{
				Source: paymentevents.SourceEmail, Provider: tmpl.Provider,
				SourceEventID: sourceIDForMail(message), ReceivedTimestamp: received,
				RawMessageHash: message.RawHash, TemplateVersion: tmpl.Version,
				AuthenticationOutcome: "dkim_fail_or_unapproved_domain",
			}, fmt.Errorf("message lacks passing DKIM from an approved %s signing domain", tmpl.Provider)
		}
		values := namedMatches(tmpl.Body, match)
		amountMinor, err := parseMinorAmount(values["amount"])
		if err != nil {
			return paymentevents.PaymentEvent{}, err
		}
		audit, _ := json.Marshal(map[string]any{
			"message_id": message.MessageID, "subject": message.Subject,
			"size": message.RawSize, "dkim_signing_domain": signingDomain,
		})
		return paymentevents.PaymentEvent{
			Source: paymentevents.SourceEmail, Provider: tmpl.Provider,
			SourceEventID: sourceIDForMail(message), Sender: strings.TrimSpace(values["sender"]),
			AmountMinor: amountMinor, Currency: strings.ToUpper(values["currency"]),
			Description: strings.TrimSpace(values["reference"]), ProviderTimestamp: message.Date,
			ReceivedTimestamp: received.UTC(), AuditMetadata: audit,
			TemplateVersion: tmpl.Version, AuthenticationOutcome: "dkim_pass:" + signingDomain,
			RawMessageHash: message.RawHash,
		}, nil
	}
	return paymentevents.PaymentEvent{
		Source: paymentevents.SourceEmail, SourceEventID: sourceIDForMail(message),
		ReceivedTimestamp: received.UTC(), RawMessageHash: message.RawHash,
		AuthenticationOutcome: "not_evaluated",
	}, fmt.Errorf("no active exact payment template matched")
}

func sourceIDForMail(message decodedMail) string {
	if message.MessageID != "" {
		return message.MessageID
	}
	return message.RawHash
}

func namedMatches(pattern *regexp.Regexp, match []string) map[string]string {
	result := make(map[string]string)
	for i, name := range pattern.SubexpNames() {
		if i > 0 && name != "" && i < len(match) {
			result[name] = strings.TrimSpace(match[i])
		}
	}
	return result
}

func authenticatedSigningDomain(results, allowed []string) (string, bool) {
	for _, header := range results {
		// Authentication-Results mechanisms are semicolon-delimited. Require
		// dkim=pass and header.d to occur in the same result clause so a passing
		// attacker signature cannot be paired with a failing provider signature.
		for _, clause := range strings.Split(strings.ToLower(header), ";") {
			if !regexp.MustCompile(`\bdkim\s*=\s*pass\b`).MatchString(clause) {
				continue
			}
			domainMatches := regexp.MustCompile(`\bheader\.d\s*=\s*([a-z0-9._-]+)`).FindAllStringSubmatch(clause, -1)
			for _, match := range domainMatches {
				for _, domain := range allowed {
					if strings.EqualFold(match[1], domain) {
						return match[1], true
					}
				}
			}
		}
	}
	return "", false
}

func parseMinorAmount(value string) (int64, error) {
	value = strings.ReplaceAll(strings.TrimSpace(value), " ", "")
	if value == "" {
		return 0, fmt.Errorf("empty amount")
	}
	lastComma := strings.LastIndex(value, ",")
	lastDot := strings.LastIndex(value, ".")
	decimalAt := lastComma
	if lastDot > decimalAt {
		decimalAt = lastDot
	}
	normalized := value
	if decimalAt >= 0 {
		fractionLen := len(value) - decimalAt - 1
		if fractionLen == 2 {
			whole := strings.NewReplacer(",", "", ".", "").Replace(value[:decimalAt])
			normalized = whole + "." + value[decimalAt+1:]
		} else {
			normalized = strings.NewReplacer(",", "", ".", "").Replace(value)
		}
	}
	amount, err := strconv.ParseFloat(normalized, 64)
	if err != nil || amount <= 0 {
		return 0, fmt.Errorf("invalid amount %q", value)
	}
	return int64(amount*100 + 0.5), nil
}
