package auth

import (
	"testing"
)

func TestGenerateTokenIsUniqueAndLong(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		k := generateToken()
		if len(k) != 64 { // 32 bytes, hex encoded
			t.Fatalf("expected 64 hex chars, got %d", len(k))
		}
		if seen[k] {
			t.Fatal("duplicate API key generated")
		}
		seen[k] = true
	}
}

func TestHashAPIKeyDeterministic(t *testing.T) {
	a, b := HashAPIKey("secret"), HashAPIKey("secret")
	if a != b || a == HashAPIKey("other") || len(a) != 64 {
		t.Fatal("HashAPIKey must be deterministic, collision-free for distinct inputs, 64 hex chars")
	}
}

func TestJWTRoundTrip(t *testing.T) {
	tok, err := issueJWT("client-1", "premium", 1000, 60, "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	id, tier, rl, ws, ok := verifyJWT(tok, "s3cret")
	if !ok || id != "client-1" || tier != "premium" || rl != 1000 || ws != 60 {
		t.Fatalf("unexpected claims: %v %v %v %v %v", id, tier, rl, ws, ok)
	}
}

func TestJWTRejectsWrongSecretAndGarbage(t *testing.T) {
	tok, _ := issueJWT("client-1", "free", 100, 60, "s3cret")
	if _, _, _, _, ok := verifyJWT(tok, "wrong"); ok {
		t.Fatal("token verified with wrong secret")
	}
	if _, _, _, _, ok := verifyJWT("not.a.jwt", "s3cret"); ok {
		t.Fatal("garbage token verified")
	}
}
