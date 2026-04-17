package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"log"
	"sync"

	"github.com/chromedp/cdproto/webauthn"
	"github.com/chromedp/chromedp"
)

type PasskeyManager struct {
	mu            sync.Mutex
	authenticators map[contextKey]webauthn.AuthenticatorID
	privateKeys   map[string]*ecdsa.PrivateKey
	credentials   map[string][]*PasskeyCredential
}

type contextKey string

func contextID(ctx context.Context) contextKey {
	if c := chromedp.FromContext(ctx); c != nil && c.Target != nil {
		return contextKey(c.Target.TargetID)
	}
	return contextKey(fmt.Sprintf("%p", ctx))
}

type PasskeyCredential struct {
	CredentialID  []byte
	PrivateKey    *ecdsa.PrivateKey
	RPID          string
	UserHandle    []byte
	SignCount     int64
}

func NewPasskeyManager() *PasskeyManager {
	return &PasskeyManager{
		authenticators: make(map[contextKey]webauthn.AuthenticatorID),
		privateKeys:    make(map[string]*ecdsa.PrivateKey),
		credentials:    make(map[string][]*PasskeyCredential),
	}
}

func (pm *PasskeyManager) Enable(ctx context.Context) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	cid := contextID(ctx)

	if _, exists := pm.authenticators[cid]; exists {
		return nil
	}

	if err := webauthn.Enable().Do(ctx); err != nil {
		return fmt.Errorf("auth/passkey: enable webauthn domain: %w", err)
	}

	authID, err := webauthn.AddVirtualAuthenticator(&webauthn.VirtualAuthenticatorOptions{
		Protocol:                    webauthn.AuthenticatorProtocolCtap2,
		Ctap2version:                webauthn.Ctap2versionCtap21,
		Transport:                   webauthn.AuthenticatorTransportInternal,
		HasResidentKey:              true,
		HasUserVerification:         true,
		IsUserVerified:              true,
		AutomaticPresenceSimulation: true,
		DefaultBackupEligibility:    true,
		DefaultBackupState:          true,
	}).Do(ctx)
	if err != nil {
		return fmt.Errorf("auth/passkey: add virtual authenticator: %w", err)
	}

	pm.authenticators[cid] = authID

	for rpID, creds := range pm.credentials {
		for _, cred := range creds {
			privKeyBytes, err := x509.MarshalPKCS8PrivateKey(cred.PrivateKey)
			if err != nil {
				continue
			}
			privKeyPEM := base64.StdEncoding.EncodeToString(privKeyBytes)
			credID := base64.StdEncoding.EncodeToString(cred.CredentialID)
			userHandle := base64.StdEncoding.EncodeToString(cred.UserHandle)

			_ = webauthn.AddCredential(authID, &webauthn.Credential{
				CredentialID:         credID,
				IsResidentCredential: true,
				RpID:                 rpID,
				PrivateKey:           privKeyPEM,
				UserHandle:           userHandle,
				SignCount:            cred.SignCount,
			}).Do(ctx)
		}
	}

	log.Printf("[passkey] virtual authenticator created for context %s: %s", cid, authID)
	return nil
}

func (pm *PasskeyManager) Disable(ctx context.Context) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	cid := contextID(ctx)
	authID, exists := pm.authenticators[cid]
	if !exists {
		return nil
	}

	if err := webauthn.RemoveVirtualAuthenticator(authID).Do(ctx); err != nil {
		return fmt.Errorf("auth/passkey: remove virtual authenticator: %w", err)
	}

	if err := webauthn.Disable().Do(ctx); err != nil {
		return fmt.Errorf("auth/passkey: disable webauthn domain: %w", err)
	}

	delete(pm.authenticators, cid)
	log.Printf("[passkey] virtual authenticator removed for context %s", cid)
	return nil
}

func (pm *PasskeyManager) SetUserVerified(ctx context.Context, verified bool) error {
	pm.mu.Lock()
	cid := contextID(ctx)
	authID, exists := pm.authenticators[cid]
	pm.mu.Unlock()

	if !exists {
		return fmt.Errorf("auth/passkey: no authenticator for context %s - call Enable() first", cid)
	}

	return webauthn.SetUserVerified(authID, verified).Do(ctx)
}

func (pm *PasskeyManager) AddCredential(ctx context.Context, cred *PasskeyCredential) error {
	pm.mu.Lock()
	cid := contextID(ctx)
	authID, exists := pm.authenticators[cid]
	pm.mu.Unlock()

	if !exists {
		return fmt.Errorf("auth/passkey: no authenticator for context %s - call Enable() first", cid)
	}

	privKeyBytes, err := x509.MarshalPKCS8PrivateKey(cred.PrivateKey)
	if err != nil {
		return fmt.Errorf("auth/passkey: marshal private key: %w", err)
	}

	privKeyPEM := base64.StdEncoding.EncodeToString(privKeyBytes)
	credID := base64.StdEncoding.EncodeToString(cred.CredentialID)
	userHandle := base64.StdEncoding.EncodeToString(cred.UserHandle)

	if err := webauthn.AddCredential(authID, &webauthn.Credential{
		CredentialID:         credID,
		IsResidentCredential: true,
		RpID:                 cred.RPID,
		PrivateKey:           privKeyPEM,
		UserHandle:           userHandle,
		SignCount:            cred.SignCount,
	}).Do(ctx); err != nil {
		return fmt.Errorf("auth/passkey: add credential: %w", err)
	}

	pm.mu.Lock()
	pm.privateKeys[cred.RPID] = cred.PrivateKey
	pm.credentials[cred.RPID] = append(pm.credentials[cred.RPID], cred)
	pm.mu.Unlock()

	log.Printf("[passkey] added credential for rpID=%s", cred.RPID)
	return nil
}

func (pm *PasskeyManager) GetCredentials(ctx context.Context) ([]*webauthn.Credential, error) {
	pm.mu.Lock()
	cid := contextID(ctx)
	authID, exists := pm.authenticators[cid]
	pm.mu.Unlock()

	if !exists {
		return nil, fmt.Errorf("auth/passkey: no authenticator for context %s", cid)
	}

	return webauthn.GetCredentials(authID).Do(ctx)
}

func (pm *PasskeyManager) RemoveCredential(ctx context.Context, credentialID string) error {
	pm.mu.Lock()
	cid := contextID(ctx)
	authID, exists := pm.authenticators[cid]
	pm.mu.Unlock()

	if !exists {
		return fmt.Errorf("auth/passkey: no authenticator for context %s", cid)
	}

	return webauthn.RemoveCredential(authID, credentialID).Do(ctx)
}

func (pm *PasskeyManager) ClearCredentials(ctx context.Context) error {
	pm.mu.Lock()
	cid := contextID(ctx)
	authID, exists := pm.authenticators[cid]
	pm.mu.Unlock()

	if !exists {
		return fmt.Errorf("auth/passkey: no authenticator for context %s", cid)
	}

	return webauthn.ClearCredentials(authID).Do(ctx)
}

func (pm *PasskeyManager) IsEnabled(ctx context.Context) bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	_, exists := pm.authenticators[contextID(ctx)]
	return exists
}

func (pm *PasskeyManager) AuthenticatorID(ctx context.Context) (webauthn.AuthenticatorID, bool) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	authID, exists := pm.authenticators[contextID(ctx)]
	return authID, exists
}

func GeneratePasskeyCredential(rpID string, userHandle []byte) (*PasskeyCredential, error) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("auth/passkey: generate ECDSA key: %w", err)
	}

	credID := make([]byte, 32)
	if _, err := rand.Read(credID); err != nil {
		return nil, fmt.Errorf("auth/passkey: generate credential ID: %w", err)
	}

	if userHandle == nil {
		userHandle = make([]byte, 16)
		if _, err := rand.Read(userHandle); err != nil {
			return nil, fmt.Errorf("auth/passkey: generate user handle: %w", err)
		}
	}

	return &PasskeyCredential{
		CredentialID:  credID,
		PrivateKey:    privateKey,
		RPID:          rpID,
		UserHandle:    userHandle,
		SignCount:     0,
	}, nil
}

func SetupPasskeyOnPage(ctx context.Context, pm *PasskeyManager) error {
	if err := pm.Enable(ctx); err != nil {
		return err
	}

	return pm.SetUserVerified(ctx, true)
}

func HandlePasskeyAuthentication(ctx context.Context, pm *PasskeyManager) error {
	if !pm.IsEnabled(ctx) {
		if err := pm.Enable(ctx); err != nil {
			return err
		}
	}

	return pm.SetUserVerified(ctx, true)
}

func WaitForPasskeyDialog(ctx context.Context, timeoutMs int) chromedp.Action {
	return chromedp.WaitReady("body", chromedp.ByQuery)
}
