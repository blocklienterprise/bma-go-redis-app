package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"
	"time"
)

const testSecret = "test-secret-do-not-use-in-prod"

// mintToken builds a valid `bma.` token the same way the WP plugin does.
func mintToken(t *testing.T, secret string, claims bmaClaims) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := base64.RawURLEncoding.EncodeToString(payload)
	return "bma." + body + "." + signBMA(body, secret)
}

func TestValidateBMAToken_RoundTrip(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tok := mintToken(t, testSecret, bmaClaims{
		Iss: "blockli-mobile", UID: 42, Iat: now.Unix(), Exp: now.Unix() + 3600, Jti: "abc",
	})
	c, err := validateBMAToken(tok, testSecret, now)
	if err != nil {
		t.Fatalf("expected valid, got %v", err)
	}
	if c.UID != 42 {
		t.Fatalf("uid = %d, want 42", c.UID)
	}
}

func TestValidateBMAToken_Rejections(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	good := bmaClaims{Iss: "blockli-mobile", UID: 7, Iat: now.Unix(), Exp: now.Unix() + 3600}

	cases := map[string]string{
		"bad format":   "not-a-token",
		"wrong prefix": "jwt.x.y",
		"empty":        "",
	}
	for name, tok := range cases {
		if _, err := validateBMAToken(tok, testSecret, now); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}

	// Tampered signature (signed with a different secret).
	forged := mintToken(t, "attacker-secret", good)
	if _, err := validateBMAToken(forged, testSecret, now); err != errTokenSig {
		t.Errorf("forged sig: got %v, want errTokenSig", err)
	}

	// Tampered payload (uid changed after signing) must break the signature.
	valid := mintToken(t, testSecret, good)
	parts := []byte(valid)
	parts[len(parts)-1] ^= 0x01 // flip a bit in the signature
	if _, err := validateBMAToken(string(parts), testSecret, now); err == nil {
		t.Errorf("flipped sig byte: expected error, got nil")
	}

	// Expired.
	expired := mintToken(t, testSecret, bmaClaims{Iss: "blockli-mobile", UID: 7, Exp: now.Unix() - 1})
	if _, err := validateBMAToken(expired, testSecret, now); err != errTokenExpired {
		t.Errorf("expired: got %v, want errTokenExpired", err)
	}

	// Missing uid.
	noUID := mintToken(t, testSecret, bmaClaims{Iss: "blockli-mobile", Exp: now.Unix() + 3600})
	if _, err := validateBMAToken(noUID, testSecret, now); err != errTokenPayload {
		t.Errorf("no uid: got %v, want errTokenPayload", err)
	}
}

// TestValidateBMAToken_CrossImpl validates a token minted by the real PHP plugin
// algorithm. The bash harness generates it with the PHP CLI and passes it plus
// the secret via env, proving Go and WordPress agree byte-for-byte. Skipped when
// the env vars are absent (e.g. plain `go test`).
func TestValidateBMAToken_CrossImpl(t *testing.T) {
	tok := os.Getenv("BMA_TEST_TOKEN")
	secret := os.Getenv("BMA_TEST_SECRET")
	if tok == "" || secret == "" {
		t.Skip("BMA_TEST_TOKEN / BMA_TEST_SECRET not set")
	}
	c, err := validateBMAToken(tok, secret, time.Now())
	if err != nil {
		t.Fatalf("PHP-minted token rejected by Go validator: %v", err)
	}
	t.Logf("PHP-minted token accepted: uid=%d iss=%s exp=%d", c.UID, c.Iss, c.Exp)
}
