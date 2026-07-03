package pushpush

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// StatusDexPositionTaken mirrors db.StatusDexPositionTaken in dexMmController. The
// dexFiatController module cannot import dexMmController, so the status string is
// duplicated here. Keep in sync with dexMmController/internal/db/models.go.
const (
	statusDexPositionTaken   = "DexPositionTaken"
	statusDexFiatHasArrived  = "DexFiatHasArrived"
	directionBUY             = "BUY"
)

// Candidate is a pending bid transaction awaiting fiat arrival.
type Candidate struct {
	ID                      uint            `json:"id"`
	FiatAmount              float64         `json:"fiat_amount"`
	FiatCurrency            string          `json:"fiat_currency"`
	Dex                     string          `json:"dex"`
	CounterpartyPaymentInfo json.RawMessage `json:"counterparty_payment_info"`
}

// TxStore reads pending transactions and advances the fiat-arrival status. It uses the
// same DATABASE_URL / pgx connection as the scraping secrets store.
type TxStore struct {
	db *sql.DB
	// ownsDB is true when this store opened the connection itself and should close it.
	ownsDB bool
}

// OpenTxStore opens a new pgx connection pool for DATABASE_URL.
func OpenTxStore(databaseURL string) (*TxStore, error) {
	if strings.TrimSpace(databaseURL) == "" {
		return nil, fmt.Errorf("pushpush: DATABASE_URL is required")
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("pushpush: open transactions db: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pushpush: ping transactions db: %w", err)
	}
	return &TxStore{db: db, ownsDB: true}, nil
}

// NewTxStore wraps an already-opened *sql.DB (e.g. shared with the secrets store).
func NewTxStore(db *sql.DB) *TxStore {
	return &TxStore{db: db}
}

// Close releases the connection if this store opened it.
func (s *TxStore) Close() error {
	if s == nil || !s.ownsDB || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// DB returns the underlying connection pool, so other stores (e.g. LogStore) can share it
// instead of opening a second pool against the same database.
func (s *TxStore) DB() *sql.DB {
	if s == nil {
		return nil
	}
	return s.db
}

// ListPendingFiat returns all bid transactions waiting for the counterparty's fiat to
// land (status DexPositionTaken, direction BUY). These are the rows a push payment
// notification can match against.
func (s *TxStore) ListPendingFiat(ctx context.Context) ([]Candidate, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, fiat_amount, fiat_currency, COALESCE(dex, ''), counterparty_payment_info
		FROM transactions
		WHERE status = $1 AND direction = $2 AND deleted_at IS NULL
	`, statusDexPositionTaken, directionBUY)
	if err != nil {
		return nil, fmt.Errorf("pushpush: list pending fiat: %w", err)
	}
	defer rows.Close()

	var out []Candidate
	for rows.Next() {
		var c Candidate
		var raw sql.NullString
		if err := rows.Scan(&c.ID, &c.FiatAmount, &c.FiatCurrency, &c.Dex, &raw); err != nil {
			return nil, fmt.Errorf("pushpush: scan pending fiat: %w", err)
		}
		if raw.Valid && raw.String != "" {
			c.CounterpartyPaymentInfo = json.RawMessage(raw.String)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// MarkFiatArrived advances a transaction from DexPositionTaken to DexFiatHasArrived and
// records the arrival evidence in dex_fiat_arrival. The WHERE status='DexPositionTaken'
// guard makes it idempotent: a concurrent scraper or healthcheck that already advanced
// the row yields advanced=false with no error.
func (s *TxStore) MarkFiatArrived(ctx context.Context, id uint, evidence json.RawMessage) (bool, error) {
	if len(evidence) == 0 {
		evidence = json.RawMessage(`{}`)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE transactions
		SET status = $1,
		    dex_fiat_arrival = COALESCE($2, dex_fiat_arrival),
		    updated_at = CURRENT_TIMESTAMP
		WHERE id = $3 AND status = $4 AND deleted_at IS NULL
	`, statusDexFiatHasArrived, []byte(evidence), id, statusDexPositionTaken)
	if err != nil {
		return false, fmt.Errorf("pushpush: mark fiat arrived for tx %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("pushpush: rows affected for tx %d: %w", id, err)
	}
	return n > 0, nil
}
