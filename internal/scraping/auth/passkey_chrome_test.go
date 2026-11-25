package auth

import (
	"context"
	"encoding/base64"
	"net"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// TestPasskeyManagerInChrome drives a real headless Chrome through the virtual
// authenticator: register a passkey with navigator.credentials.create, sync it
// back, sign in with navigator.credentials.get, then remove it. It needs a
// Chrome binary and runs only when CHROME_PATH points at one.
func TestPasskeyManagerInChrome(t *testing.T) {
	chromePath := os.Getenv("CHROME_PATH")
	if chromePath == "" {
		t.Skip("CHROME_PATH not set")
	}

	// http://localhost is a secure context, so WebAuthn works without TLS.
	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<!doctype html><title>passkey</title><body>ok</body>"))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	pageURL := "http://localhost:" + strconv.Itoa(ln.Addr().(*net.TCPAddr).Port) + "/"

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(),
		append(chromedp.DefaultExecAllocatorOptions[:],
			chromedp.ExecPath(chromePath),
			chromedp.Flag("no-sandbox", true),
			chromedp.Flag("disable-dev-shm-usage", true),
		)...)
	defer cancelAlloc()
	ctx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, 60*time.Second)
	defer cancelTimeout()

	if err := chromedp.Do(ctx, chromedp.Navigate(pageURL)); err != nil {
		t.Fatalf("navigate: %v", err)
	}

	pm, err := NewPasskeyManager(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	created, err := RegisterPasskey(ctx, pm, func() error {
		_, err := chromedp.Run(ctx, chromedp.Evaluate[bool](`navigator.credentials.create({publicKey: {
			challenge: new Uint8Array(32),
			rp: {id: "localhost", name: "test"},
			user: {id: new Uint8Array([1,2,3,4]), name: "u", displayName: "u"},
			pubKeyCredParams: [{type: "public-key", alg: -7}],
			authenticatorSelection: {residentKey: "required", userVerification: "required"},
		}}).then(c => c !== null)`, chromedp.EvalAwaitPromise))
		return err
	})
	if err != nil || !created {
		t.Fatalf("RegisterPasskey: created=%v err=%v", created, err)
	}
	if !pm.HasCredentials("localhost") {
		t.Fatal("synced credential missing for rpID localhost")
	}

	// A second sync must match the credential instead of adding it again.
	added, err := pm.SyncCredentialsFromAuthenticator(ctx)
	if err != nil || added != 0 {
		t.Fatalf("resync: added=%d err=%v", added, err)
	}

	ok, err := chromedp.Run(ctx, chromedp.Evaluate[bool](`navigator.credentials.get({publicKey: {
		challenge: new Uint8Array(32), rpId: "localhost", userVerification: "required",
	}}).then(c => c !== null)`, chromedp.EvalAwaitPromise))
	if err != nil || !ok {
		t.Fatalf("sign in with the passkey: ok=%v err=%v", ok, err)
	}

	creds, err := pm.GetCredentials(ctx)
	if err != nil || len(creds) != 1 {
		t.Fatalf("GetCredentials: %d %v", len(creds), err)
	}
	if err := pm.RemoveCredential(ctx, base64.StdEncoding.EncodeToString(creds[0].CredentialID)); err != nil {
		t.Fatalf("RemoveCredential: %v", err)
	}
	if pm.HasCredentials("localhost") {
		t.Fatal("credential still present after RemoveCredential")
	}

	// Loading a stored credential into a fresh authenticator must work too.
	if err := pm.Disable(ctx); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	cred, err := GeneratePasskeyCredential("localhost", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := pm.Enable(ctx); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if err := pm.AddCredential(ctx, cred); err != nil {
		t.Fatalf("AddCredential: %v", err)
	}
	creds, err = pm.GetCredentials(ctx)
	if err != nil || len(creds) != 1 || base64.StdEncoding.EncodeToString(creds[0].CredentialID) != base64.StdEncoding.EncodeToString(cred.CredentialID) {
		t.Fatalf("stored credential not in authenticator: %d %v", len(creds), err)
	}
	if err := pm.ClearCredentials(ctx); err != nil {
		t.Fatalf("ClearCredentials: %v", err)
	}
}
