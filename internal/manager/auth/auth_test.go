package auth

import (
	"crypto/ed25519"
	"testing"
)

func TestNewIdentity(t *testing.T) {
	id, err := NewIdentity()
	if err != nil {
		t.Fatalf("NewIdentity: %v", err)
	}
	if id.MachineID == "" {
		t.Error("machine_id empty")
	}
	if len(id.SecretPub) != ed25519.PublicKeySize {
		t.Errorf("pub key size = %d, want %d", len(id.SecretPub), ed25519.PublicKeySize)
	}
	if len(id.SecretPriv) != ed25519.PrivateKeySize {
		t.Errorf("priv key size = %d, want %d", len(id.SecretPriv), ed25519.PrivateKeySize)
	}
	// hex 往返一致。
	if id.PubHex() == "" || id.PrivHex() == "" {
		t.Error("hex encoding empty")
	}
}

func TestIdentityUniqueness(t *testing.T) {
	a, _ := NewIdentity()
	b, _ := NewIdentity()
	if a.MachineID == b.MachineID {
		t.Error("two machine_id must differ")
	}
	if string(a.SecretPriv) == string(b.SecretPriv) {
		t.Error("two keypairs must differ")
	}
}

func TestVerifySecret(t *testing.T) {
	id, _ := NewIdentity()
	msg := []byte("hello")
	sig := ed25519.Sign(id.SecretPriv, msg)
	if !VerifySecret(id.SecretPub, msg, sig) {
		t.Error("valid signature rejected")
	}
	if VerifySecret(id.SecretPub, msg, []byte("bad")) {
		t.Error("invalid signature accepted")
	}
	if VerifySecret(id.SecretPub, []byte("tampered"), sig) {
		t.Error("tampered message accepted")
	}
}
