package pushpush

import (
	"database/sql"
	"fmt"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// TxStore owns the database connection shared by neutral payment stores and
// PushPush-specific audit stores.
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
