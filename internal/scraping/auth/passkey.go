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
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/webauthn"
	"github.com/chromedp/chromedp"
)

type PasskeyManager struct {
	mu             sync.Mutex
	authenticators map[contextKey]webauthn.AuthenticatorID
	privateKeys    map[string]*ecdsa.PrivateKey
	credentials    map[string][]*PasskeyCredential
	store          PasskeyStore
}

type contextKey string

func contextID(ctx context.Context) contextKey {
	if c := chromedp.FromContext(ctx); c != nil && c.Target != nil {
		return contextKey(c.Target.TargetID)
	}
	return contextKey(fmt.Sprintf("%p", ctx))
}

type PasskeyCredential struct {
	CredentialID []byte
	PrivateKey   *ecdsa.PrivateKey
	RPID         string
	UserHandle   []byte
	SignCount    int64
}

type PasskeyStore interface {
	LoadCredentials(ctx context.Context) ([]*PasskeyCredential, error)
	SaveCredentials(ctx context.Context, credentials []*PasskeyCredential) error
}

func NewPasskeyManager(ctx context.Context, store PasskeyStore) (*PasskeyManager, error) {
	pm := &PasskeyManager{
		authenticators: make(map[contextKey]webauthn.AuthenticatorID),
		privateKeys:    make(map[string]*ecdsa.PrivateKey),
		credentials:    make(map[string][]*PasskeyCredential),
		store:          store,
	}

	if err := pm.loadFromStore(ctx); err != nil {
		return nil, err
	}

	return pm, nil
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
	err = pm.persistLocked()
	pm.mu.Unlock()
	if err != nil {
		return err
	}

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

	if err := webauthn.RemoveCredential(authID, credentialID).Do(ctx); err != nil {
		return err
	}

	pm.mu.Lock()
	removed := pm.removeCredentialLocked(credentialID)
	err := pm.persistLocked()
	pm.mu.Unlock()
	if err != nil {
		return err
	}
	if removed {
		log.Printf("[passkey] removed credential %s", credentialID)
	}

	return nil
}

func (pm *PasskeyManager) ClearCredentials(ctx context.Context) error {
	pm.mu.Lock()
	cid := contextID(ctx)
	authID, exists := pm.authenticators[cid]
	pm.mu.Unlock()

	if !exists {
		return fmt.Errorf("auth/passkey: no authenticator for context %s", cid)
	}

	if err := webauthn.ClearCredentials(authID).Do(ctx); err != nil {
		return err
	}

	pm.mu.Lock()
	pm.privateKeys = make(map[string]*ecdsa.PrivateKey)
	pm.credentials = make(map[string][]*PasskeyCredential)
	err := pm.persistLocked()
	pm.mu.Unlock()
	if err != nil {
		return err
	}

	log.Printf("[passkey] cleared all credentials")
	return nil
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

func (pm *PasskeyManager) HasCredentials(rpID string) bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return len(pm.credentials[rpID]) > 0
}

func (pm *PasskeyManager) HasCredentialsForHost(host string) bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	for rpID, creds := range pm.credentials {
		if len(creds) == 0 {
			continue
		}
		if host == rpID || strings.HasSuffix(host, "."+rpID) || strings.HasSuffix(rpID, "."+host) {
			return true
		}
	}
	return false
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
		CredentialID: credID,
		PrivateKey:   privateKey,
		RPID:         rpID,
		UserHandle:   userHandle,
		SignCount:    0,
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

func RegisterPasskey(ctx context.Context, pm *PasskeyManager, trigger func() error) (bool, error) {
	if pm == nil {
		return false, fmt.Errorf("auth/passkey: no passkey manager configured")
	}

	if err := HandlePasskeyAuthentication(ctx, pm); err != nil {
		return false, err
	}

	before := pm.credentialIDs()
	if err := trigger(); err != nil {
		return false, err
	}

	waitCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		added, err := pm.SyncCredentialsFromAuthenticator(waitCtx)
		if err != nil {
			return false, err
		}
		if added > 0 || pm.hasNewCredentialSince(before) {
			return true, nil
		}

		select {
		case <-waitCtx.Done():
			return false, fmt.Errorf("auth/passkey: registration did not create a new credential before timeout")
		case <-ticker.C:
		}
	}
}

func (pm *PasskeyManager) loadFromStore(ctx context.Context) error {
	if pm.store == nil {
		return nil
	}

	credentials, err := pm.store.LoadCredentials(ctx)
	if err != nil {
		return err
	}

	for _, cred := range credentials {
		pm.privateKeys[cred.RPID] = cred.PrivateKey
		pm.credentials[cred.RPID] = append(pm.credentials[cred.RPID], cred)
	}

	if len(credentials) > 0 {
		log.Printf("[passkey] loaded %d persisted credentials", len(credentials))
	}
	return nil
}

func (pm *PasskeyManager) SyncCredentialsFromAuthenticator(ctx context.Context) (int, error) {
	pm.mu.Lock()
	cid := contextID(ctx)
	authID, exists := pm.authenticators[cid]
	pm.mu.Unlock()
	if !exists {
		return 0, fmt.Errorf("auth/passkey: no authenticator for context %s", cid)
	}

	credentials, err := webauthn.GetCredentials(authID).Do(ctx)
	if err != nil {
		return 0, fmt.Errorf("auth/passkey: get credentials: %w", err)
	}

	pm.mu.Lock()
	added := 0
	changed := false
	for _, cred := range credentials {
		passkeyCred, err := decodeCDPCredential(cred)
		if err != nil {
			pm.mu.Unlock()
			return 0, fmt.Errorf("auth/passkey: decode credential %s: %w", cred.CredentialID, err)
		}

		rpCreds := pm.credentials[passkeyCred.RPID]
		matched := false
		for _, existing := range rpCreds {
			if base64.StdEncoding.EncodeToString(existing.CredentialID) == cred.CredentialID {
				existing.PrivateKey = passkeyCred.PrivateKey
				existing.UserHandle = passkeyCred.UserHandle
				existing.SignCount = passkeyCred.SignCount
				pm.privateKeys[passkeyCred.RPID] = passkeyCred.PrivateKey
				matched = true
				break
			}
		}
		if matched {
			continue
		}

		pm.credentials[passkeyCred.RPID] = append(pm.credentials[passkeyCred.RPID], passkeyCred)
		pm.privateKeys[passkeyCred.RPID] = passkeyCred.PrivateKey
		added++
		changed = true
	}

	if changed {
		if err := pm.persistLocked(); err != nil {
			pm.mu.Unlock()
			return 0, err
		}
	}
	pm.mu.Unlock()

	if added > 0 {
		log.Printf("[passkey] synced %d new credentials from authenticator", added)
	}
	return added, nil
}

func (pm *PasskeyManager) persistLocked() error {
	if pm.store == nil {
		return nil
	}
	return pm.store.SaveCredentials(context.Background(), pm.snapshotCredentialsLocked())
}

func decodeCDPCredential(cred *webauthn.Credential) (*PasskeyCredential, error) {
	credID, err := base64.StdEncoding.DecodeString(cred.CredentialID)
	if err != nil {
		return nil, fmt.Errorf("decode credential id: %w", err)
	}

	privKeyBytes, err := base64.StdEncoding.DecodeString(cred.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("decode private key: %w", err)
	}

	parsedKey, err := x509.ParsePKCS8PrivateKey(privKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}

	privateKey, ok := parsedKey.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("unexpected private key type %T", parsedKey)
	}

	userHandle, err := base64.StdEncoding.DecodeString(cred.UserHandle)
	if err != nil {
		return nil, fmt.Errorf("decode user handle: %w", err)
	}

	return &PasskeyCredential{
		CredentialID: credID,
		PrivateKey:   privateKey,
		RPID:         cred.RpID,
		UserHandle:   userHandle,
		SignCount:    cred.SignCount,
	}, nil
}

func (pm *PasskeyManager) removeCredentialLocked(credentialID string) bool {
	removed := false
	for rpID, creds := range pm.credentials {
		filtered := creds[:0]
		for _, cred := range creds {
			if base64.StdEncoding.EncodeToString(cred.CredentialID) == credentialID {
				removed = true
				continue
			}
			filtered = append(filtered, cred)
		}

		if len(filtered) == 0 {
			delete(pm.credentials, rpID)
			delete(pm.privateKeys, rpID)
			continue
		}

		pm.credentials[rpID] = filtered
		pm.privateKeys[rpID] = filtered[len(filtered)-1].PrivateKey
	}

	return removed
}

func (pm *PasskeyManager) credentialIDs() map[string]struct{} {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	ids := make(map[string]struct{})
	for _, creds := range pm.credentials {
		for _, cred := range creds {
			ids[base64.StdEncoding.EncodeToString(cred.CredentialID)] = struct{}{}
		}
	}
	return ids
}

func (pm *PasskeyManager) hasNewCredentialSince(before map[string]struct{}) bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	for _, creds := range pm.credentials {
		for _, cred := range creds {
			if _, ok := before[base64.StdEncoding.EncodeToString(cred.CredentialID)]; !ok {
				return true
			}
		}
	}
	return false
}

func (pm *PasskeyManager) snapshotCredentialsLocked() []*PasskeyCredential {
	credentials := make([]*PasskeyCredential, 0)
	for _, creds := range pm.credentials {
		for _, cred := range creds {
			credentials = append(credentials, &PasskeyCredential{
				CredentialID: append([]byte(nil), cred.CredentialID...),
				PrivateKey:   cred.PrivateKey,
				RPID:         cred.RPID,
				UserHandle:   append([]byte(nil), cred.UserHandle...),
				SignCount:    cred.SignCount,
			})
		}
	}
	return credentials
}
