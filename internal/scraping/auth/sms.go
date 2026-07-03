package auth

import (
	"context"
	"fmt"
	"regexp"
	"time"
)

type SMSProvider interface {
	GetLatestCode(ctx context.Context, phone string, within time.Duration) (string, error)
	WaitForCode(ctx context.Context, phone string, timeout time.Duration) (string, error)
}

type TwilioSMSProvider struct {
	accountSID string
	authToken  string
	fromNumber string
}

func NewTwilioSMSProvider(accountSID, authToken, fromNumber string) *TwilioSMSProvider {
	return &TwilioSMSProvider{
		accountSID: accountSID,
		authToken:  authToken,
		fromNumber: fromNumber,
	}
}

func (t *TwilioSMSProvider) GetLatestCode(ctx context.Context, phone string, within time.Duration) (string, error) {
	return "", fmt.Errorf("auth/sms: GetLatestCode not yet implemented for Twilio provider")
}

func (t *TwilioSMSProvider) WaitForCode(ctx context.Context, phone string, timeout time.Duration) (string, error) {
	return "", fmt.Errorf("auth/sms: WaitForCode not yet implemented for Twilio provider")
}

type MockSMSProvider struct {
	pendingCodes chan string
}

func NewMockSMSProvider() *MockSMSProvider {
	return &MockSMSProvider{
		pendingCodes: make(chan string, 10),
	}
}

func (m *MockSMSProvider) InjectCode(code string) {
	m.pendingCodes <- code
}

func (m *MockSMSProvider) GetLatestCode(ctx context.Context, phone string, within time.Duration) (string, error) {
	select {
	case code := <-m.pendingCodes:
		return code, nil
	case <-time.After(within):
		return "", fmt.Errorf("auth/sms: no code received within %v", within)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (m *MockSMSProvider) WaitForCode(ctx context.Context, phone string, timeout time.Duration) (string, error) {
	select {
	case code := <-m.pendingCodes:
		return code, nil
	case <-time.After(timeout):
		return "", fmt.Errorf("auth/sms: timeout waiting for code after %v", timeout)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

var codePattern = regexp.MustCompile(`\b\d{4,8}\b`)

func ExtractCodeFromSMS(body string) string {
	matches := codePattern.FindAllString(body, -1)
	if len(matches) == 0 {
		return ""
	}
	return matches[len(matches)-1]
}
