package models

import (
	"time"
)

type TransactionDirection string

const (
	DirectionInbound  TransactionDirection = "inbound"
	DirectionOutbound TransactionDirection = "outbound"
)

type TransactionStatus string

const (
	TxStatusPending   TransactionStatus = "pending"
	TxStatusCompleted TransactionStatus = "completed"
	TxStatusFailed    TransactionStatus = "failed"
	TxStatusCancelled TransactionStatus = "cancelled"
)

type Transaction struct {
	ID          string
	ConnectorID string
	Direction   TransactionDirection
	Status      TransactionStatus
	Amount      float64
	Currency    string
	Counterparty string
	Description string
	Reference   string
	Timestamp   time.Time
	RawData     map[string]any
}

type SendPaymentRequest struct {
	Amount      float64
	Currency    string
	Recipient   string
	Description string
	Reference   string
}

type SendPaymentResult struct {
	TransactionID string
	Status        TransactionStatus
	Timestamp     time.Time
}

type Balance struct {
	Currency string
	Amount   float64
	Available float64
	Hold     float64
}

type ConnectorConfig struct {
	Email      string
	Password   string
	TOTPSecret string
	Phone      string
	Headless   bool
}
