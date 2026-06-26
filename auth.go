// Bearer-token validation for realtime connections.
//
// The Expo app authenticates with a self-contained, HMAC-SHA256 signed token
// issued by the WordPress plugin (BMA_Token_Auth in
// blockli-mobile-app/includes/class-bma-token-auth.php). Format:
//
//	bma.<base64url(payload JSON)>.<base64url(HMAC-SHA256(payload-body, secret))>
//
// We validate identity here LOCALLY using the shared signing secret
// (WordPress option `bma_token_secret`, provided via BMA_TOKEN_SECRET) — no
// round-trip to WordPress per connection. This mirrors BMA_Token_Auth::validate()
// exactly so Go and WordPress accept/reject the same tokens.
//
// Authorization (which threads a user may subscribe to) is NOT in the token and
// is handled separately against BuddyBoss.
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// bmaClaims is the decoded token payload. Mirrors the WP plugin's payload.
type bmaClaims struct {
	Iss string `json:"iss"`
	UID int    `json:"uid"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
	Jti string `json:"jti"`
	Pid string `json:"pid,omitempty"`
}

var (
	errTokenFormat  = errors.New("invalid token format")
	errTokenSig     = errors.New("invalid token signature")
	errTokenExpired = errors.New("token expired")
	errTokenPayload = errors.New("invalid token payload")
)

// validateBMAToken verifies a `bma.` token against the shared secret and returns
// the decoded claims. now is injectable for testing; callers pass time.Now().
func validateBMAToken(token, secret string, now time.Time) (*bmaClaims, error) {
	token = strings.TrimSpace(token)
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "bma" {
		return nil, errTokenFormat
	}
	body, sig := parts[1], parts[2]

	// Constant-time signature comparison over the base64url-encoded HMAC,
	// matching the PHP hash_equals() check.
	expected := signBMA(body, secret)
	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return nil, errTokenSig
	}

	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil, errTokenPayload
	}
	var c bmaClaims
	if err := json.Unmarshal(raw, &c); err != nil || c.UID == 0 || c.Exp == 0 {
		return nil, errTokenPayload
	}
	if now.Unix() > c.Exp {
		return nil, errTokenExpired
	}
	return &c, nil
}

// signBMA computes base64url(HMAC-SHA256(body, secret)) with no padding,
// matching BMA_Token_Auth::sign()/base64url_encode().
func signBMA(body, secret string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}
