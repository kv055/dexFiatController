package auth

import (
	"bytes"
	"testing"
)

// The protocol credential carries raw bytes since cdproto v0.157; a round trip
// through it must keep the ID, key, user handle and counter intact.
func TestCDPCredentialRoundTrip(t *testing.T) {
	cred, err := GeneratePasskeyCredential("example.com", []byte("user-handle"))
	if err != nil {
		t.Fatal(err)
	}
	cred.SignCount = 7

	cdpCred, err := encodeCDPCredential(cred, cred.RPID)
	if err != nil {
		t.Fatal(err)
	}
	if !cdpCred.IsResidentCredential || cdpCred.RpID != "example.com" || cdpCred.SignCount != 7 {
		t.Fatalf("unexpected protocol credential: %+v", cdpCred)
	}

	got, err := decodeCDPCredential(cdpCred)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.CredentialID, cred.CredentialID) || !bytes.Equal(got.UserHandle, cred.UserHandle) {
		t.Fatalf("id/user handle changed: %x %x", got.CredentialID, got.UserHandle)
	}
	if got.RPID != cred.RPID || got.SignCount != cred.SignCount {
		t.Fatalf("rpID/signCount changed: %q %d", got.RPID, got.SignCount)
	}
	if !got.PrivateKey.Equal(cred.PrivateKey) {
		t.Fatal("private key changed")
	}
}
