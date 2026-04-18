package secrets

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"fmt"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib"

	"dexFiatController/internal/auth"
)

type Store struct {
	db *sql.DB
}

type Account struct {
	Provider string
	Email    string
	Password string
}

func Open(databaseURL string) (*Store, error) {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) LoadActiveTOTPSecrets(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT a.provider, ts.secret
		FROM totp_secrets ts
		JOIN accounts a ON a.id = ts.account_id
		WHERE ts.is_active = TRUE
	`)
	if err != nil {
		return nil, fmt.Errorf("load totp secrets: %w", err)
	}
	defer rows.Close()

	secrets := make(map[string]string)
	for rows.Next() {
		var provider string
		var secret string
		if err := rows.Scan(&provider, &secret); err != nil {
			return nil, fmt.Errorf("scan totp secret: %w", err)
		}
		secrets[provider] = secret
	}
	return secrets, rows.Err()
}

func (s *Store) LoadAccounts(ctx context.Context) (map[string]Account, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT provider, COALESCE(email, ''), COALESCE(password, '')
		FROM accounts
	`)
	if err != nil {
		return nil, fmt.Errorf("load accounts: %w", err)
	}
	defer rows.Close()

	accounts := make(map[string]Account)
	for rows.Next() {
		var account Account
		if err := rows.Scan(&account.Provider, &account.Email, &account.Password); err != nil {
			return nil, fmt.Errorf("scan account: %w", err)
		}
		accounts[account.Provider] = account
	}
	return accounts, rows.Err()
}

func (s *Store) UpsertAccount(ctx context.Context, provider, email, password string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin account upsert: %w", err)
	}
	defer tx.Rollback()

	if _, err := ensureAccount(ctx, tx, Account{
		Provider: provider,
		Email:    email,
		Password: password,
	}); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit account upsert for %s: %w", provider, err)
	}
	return nil
}

func (s *Store) UpsertTOTPSecret(ctx context.Context, provider, secret string) error {
	if strings.TrimSpace(secret) == "" {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin totp upsert: %w", err)
	}
	defer tx.Rollback()

	accountID, err := ensureAccount(ctx, tx, Account{Provider: provider})
	if err != nil {
		return err
	}

	var existingID int64
	var existingSecret string
	err = tx.QueryRowContext(ctx, `
		SELECT id, secret
		FROM totp_secrets
		WHERE account_id = $1 AND is_active = TRUE
	`, accountID).Scan(&existingID, &existingSecret)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("query active totp secret for %s: %w", provider, err)
	}

	switch {
	case err == sql.ErrNoRows:
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO totp_secrets (account_id, secret, is_active)
			VALUES ($1, $2, TRUE)
		`, accountID, secret); err != nil {
			return fmt.Errorf("insert totp secret for %s: %w", provider, err)
		}
	case existingSecret == secret:
		// No rotation needed.
	default:
		if _, err := tx.ExecContext(ctx, `
			UPDATE totp_secrets
			SET is_active = FALSE, rotated_at = CURRENT_TIMESTAMP
			WHERE id = $1
		`, existingID); err != nil {
			return fmt.Errorf("retire totp secret for %s: %w", provider, err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO totp_secrets (account_id, secret, is_active)
			VALUES ($1, $2, TRUE)
		`, accountID, secret); err != nil {
			return fmt.Errorf("insert rotated totp secret for %s: %w", provider, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit totp upsert for %s: %w", provider, err)
	}
	return nil
}

func (s *Store) LoadCredentials(ctx context.Context) ([]*auth.PasskeyCredential, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT credential_id, rp_id, private_key_pkcs8, user_handle, sign_count
		FROM passkey_credentials
		WHERE is_active = TRUE
		ORDER BY id ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("load passkey credentials: %w", err)
	}
	defer rows.Close()

	var credentials []*auth.PasskeyCredential
	for rows.Next() {
		var credentialID string
		var rpID string
		var privateKey string
		var userHandle string
		var signCount int64
		if err := rows.Scan(&credentialID, &rpID, &privateKey, &userHandle, &signCount); err != nil {
			return nil, fmt.Errorf("scan passkey credential: %w", err)
		}

		credID, err := base64.StdEncoding.DecodeString(credentialID)
		if err != nil {
			return nil, fmt.Errorf("decode passkey credential id: %w", err)
		}
		privKeyBytes, err := base64.StdEncoding.DecodeString(privateKey)
		if err != nil {
			return nil, fmt.Errorf("decode passkey private key: %w", err)
		}
		parsedKey, err := x509.ParsePKCS8PrivateKey(privKeyBytes)
		if err != nil {
			return nil, fmt.Errorf("parse passkey private key: %w", err)
		}
		ecdsaKey, ok := parsedKey.(*ecdsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("unexpected passkey private key type %T", parsedKey)
		}
		userHandleBytes, err := base64.StdEncoding.DecodeString(userHandle)
		if err != nil {
			return nil, fmt.Errorf("decode passkey user handle: %w", err)
		}

		credentials = append(credentials, &auth.PasskeyCredential{
			CredentialID: credID,
			PrivateKey:   ecdsaKey,
			RPID:         rpID,
			UserHandle:   userHandleBytes,
			SignCount:    signCount,
		})
	}
	return credentials, rows.Err()
}

func (s *Store) SaveCredentials(ctx context.Context, credentials []*auth.PasskeyCredential) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin passkey save: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM passkey_credentials`); err != nil {
		return fmt.Errorf("clear passkey credentials: %w", err)
	}

	for _, cred := range credentials {
		accountID, err := ensureAccount(ctx, tx, Account{Provider: providerForRPID(cred.RPID)})
		if err != nil {
			return err
		}
		privKeyBytes, err := x509.MarshalPKCS8PrivateKey(cred.PrivateKey)
		if err != nil {
			return fmt.Errorf("marshal passkey private key for %s: %w", cred.RPID, err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO passkey_credentials (
				account_id, credential_id, rp_id, private_key_pkcs8, user_handle, sign_count, is_active
			) VALUES ($1, $2, $3, $4, $5, $6, TRUE)
		`,
			accountID,
			base64.StdEncoding.EncodeToString(cred.CredentialID),
			cred.RPID,
			base64.StdEncoding.EncodeToString(privKeyBytes),
			base64.StdEncoding.EncodeToString(cred.UserHandle),
			cred.SignCount,
		); err != nil {
			return fmt.Errorf("insert passkey credential for %s: %w", cred.RPID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit passkey save: %w", err)
	}
	return nil
}

func ensureAccount(ctx context.Context, tx *sql.Tx, account Account) (int64, error) {
	account.Provider = strings.ToLower(strings.TrimSpace(account.Provider))
	if account.Provider == "" {
		return 0, fmt.Errorf("provider is required")
	}

	var id int64
	err := tx.QueryRowContext(ctx, `
		INSERT INTO accounts (provider, email, password)
		VALUES ($1, NULLIF($2, ''), NULLIF($3, ''))
		ON CONFLICT (provider)
		DO UPDATE SET
			email = COALESCE(NULLIF(EXCLUDED.email, ''), accounts.email),
			password = COALESCE(NULLIF(EXCLUDED.password, ''), accounts.password),
			updated_at = CURRENT_TIMESTAMP
		RETURNING id
	`, account.Provider, account.Email, account.Password).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("ensure account %s: %w", account.Provider, err)
	}
	return id, nil
}

func providerForRPID(rpID string) string {
	rpID = strings.ToLower(strings.TrimSpace(rpID))
	switch {
	case strings.Contains(rpID, "paypal"):
		return "paypal"
	case strings.Contains(rpID, "wise"):
		return "wise"
	case strings.Contains(rpID, "revolut"):
		return "revolut"
	default:
		return rpID
	}
}
