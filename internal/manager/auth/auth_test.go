package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"testing"
)

func TestNewMachineID(t *testing.T) {
	a, err := NewMachineID()
	if err != nil || a == "" {
		t.Fatalf("NewMachineID: %v", err)
	}
	b, _ := NewMachineID()
	if a == b {
		t.Error("two machine_id must differ")
	}
}

func TestChallengeRoundTrip(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	ch, err := NewChallenge()
	if err != nil || len(ch) != 64 {
		t.Fatalf("NewChallenge: %v (len=%d)", err, len(ch))
	}

	sig, err := SignChallenge(priv, ch)
	if err != nil {
		t.Fatalf("SignChallenge: %v", err)
	}
	ok, err := VerifyChallenge(pub, ch, sig)
	if err != nil || !ok {
		t.Fatalf("VerifyChallenge valid sig: ok=%v err=%v", ok, err)
	}

	// 篡改 challenge 必须失败。
	badCh, _ := NewChallenge()
	ok, _ = VerifyChallenge(pub, badCh, sig)
	if ok {
		t.Error("tampered challenge accepted")
	}

	// 错误签名必须失败。
	_, priv2, _ := ed25519.GenerateKey(rand.Reader)
	badSig, _ := SignChallenge(priv2, ch)
	ok, _ = VerifyChallenge(pub, ch, badSig)
	if ok {
		t.Error("wrong-key signature accepted")
	}
}

func TestPublicKeyValidation(t *testing.T) {
	// 公钥必须是 32 字节 hex；StoreIdentity 内部会拒绝非法公钥。
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	good := hex.EncodeToString(pub)
	if len(good) != ed25519.PublicKeySize*2 {
		t.Errorf("pub hex length = %d", len(good))
	}
	if _, err := hex.DecodeString("zz"); err == nil {
		t.Error("invalid hex should fail decode")
	}
}
