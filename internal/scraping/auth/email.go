package auth

import (
	"context"
	"fmt"
	"regexp"
	"time"
)

type EmailProvider interface {
	GetLatestCode(ctx context.Context, recipient string, within time.Duration) (string, error)
	WaitForCode(ctx context.Context, recipient string, timeout time.Duration) (string, error)
}

type IMAPProvider struct {
	host     string
	port     int
	username string
	password string
}

func NewIMAPProvider(host string, port int, username, password string) *IMAPProvider {
	return &IMAPProvider{
		host:     host,
		port:     port,
		username: username,
		password: password,
	}
}

func (i *IMAPProvider) GetLatestCode(ctx context.Context, recipient string, within time.Duration) (string, error) {
	return "", fmt.Errorf("auth/email: GetLatestCode not yet implemented for IMAP provider")
}

func (i *IMAPProvider) WaitForCode(ctx context.Context, recipient string, timeout time.Duration) (string, error) {
	return "", fmt.Errorf("auth/email: WaitForCode not yet implemented for IMAP provider")
}

type MockEmailProvider struct {
	pendingCodes chan string
}

func NewMockEmailProvider() *MockEmailProvider {
	return &MockEmailProvider{
		pendingCodes: make(chan string, 10),
	}
}

func (m *MockEmailProvider) InjectCode(code string) {
	m.pendingCodes <- code
}

func (m *MockEmailProvider) GetLatestCode(ctx context.Context, recipient string, within time.Duration) (string, error) {
	select {
	case code := <-m.pendingCodes:
		return code, nil
	case <-time.After(within):
		return "", fmt.Errorf("auth/email: no code received within %v", within)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (m *MockEmailProvider) WaitForCode(ctx context.Context, recipient string, timeout time.Duration) (string, error) {
	select {
	case code := <-m.pendingCodes:
		return code, nil
	case <-time.After(timeout):
		return "", fmt.Errorf("auth/email: timeout waiting for code after %v", timeout)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

var emailCodePattern = regexp.MustCompile(`\b\d{4,8}\b`)

func ExtractCodeFromEmail(body string) string {
	matches := emailCodePattern.FindAllString(body, -1)
	if len(matches) == 0 {
		return ""
	}
	return matches[len(matches)-1]
}
