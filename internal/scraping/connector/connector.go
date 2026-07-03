package connector

import (
	"context"

	"dexFiatController/internal/scraping/models"
)

type Connector interface {
	Name() string
	Login(ctx context.Context) error
	IsLoggedIn(ctx context.Context) (bool, error)
	Logout(ctx context.Context) error
	GetTransactions(ctx context.Context, opts TransactionQuery) ([]models.Transaction, error)
	SendPayment(ctx context.Context, req models.SendPaymentRequest) (*models.SendPaymentResult, error)
	GetBalance(ctx context.Context, currency string) (*models.Balance, error)
	Close() error
}

type TransactionQuery struct {
	Currency  string
	Direction models.TransactionDirection
	Limit     int
	Offset    int
}

type TwoFAMethod string

const (
	TwoFANone    TwoFAMethod = "none"
	TwoFATOTP   TwoFAMethod = "totp"
	TwoFAPasskey TwoFAMethod = "passkey"
	TwoFASMS    TwoFAMethod = "sms"
	TwoFAEmail  TwoFAMethod = "email"
	TwoFACall   TwoFAMethod = "call"
)

type TwoFAPreference struct {
	Primary   TwoFAMethod
	Secondary TwoFAMethod
	Fallback  TwoFAMethod
}
