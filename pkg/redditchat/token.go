package redditchat

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// TokenClaims are the parts of Reddit's chat JWT the bridge cares about.
//
// The token is only ever read here, never verified: the bridge isn't the issuer and has no key.
// The claims are used for two things, both of which the server remains the authority on:
// identifying the account before the first request, and warning the user before the token dies.
type TokenClaims struct {
	// AccountID is the Reddit account this token belongs to, e.g. t2_3mbr7.
	AccountID string
	// IssuedAt and ExpiresAt bound the token's usable lifetime. Reddit currently issues chat
	// tokens with a 24 hour lifetime, so this is not a rare event to handle.
	IssuedAt  time.Time
	ExpiresAt time.Time
}

type rawTokenClaims struct {
	AccountID string  `json:"aid"`
	LoginID   string  `json:"lid"`
	IssuedAt  float64 `json:"iat"`
	ExpiresAt float64 `json:"exp"`
}

// ParseToken decodes the claims of a Reddit chat JWT without verifying its signature.
func ParseToken(token string) (*TokenClaims, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("token is not a JWT (expected 3 dot-separated parts, got %d)", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("failed to decode token payload: %w", err)
	}
	var raw rawTokenClaims
	if err = json.Unmarshal(payload, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse token payload: %w", err)
	}
	accountID := raw.AccountID
	if accountID == "" {
		accountID = raw.LoginID
	}
	claims := &TokenClaims{AccountID: accountID}
	if raw.IssuedAt > 0 {
		claims.IssuedAt = floatToTime(raw.IssuedAt)
	}
	if raw.ExpiresAt > 0 {
		claims.ExpiresAt = floatToTime(raw.ExpiresAt)
	}
	return claims, nil
}

func floatToTime(ts float64) time.Time {
	sec := int64(ts)
	return time.Unix(sec, int64((ts-float64(sec))*float64(time.Second)))
}

// Expired reports whether the token is past its expiry time.
func (tc *TokenClaims) Expired() bool {
	return !tc.ExpiresAt.IsZero() && time.Now().After(tc.ExpiresAt)
}

// ExpiresWithin reports whether the token expires in less than the given duration.
func (tc *TokenClaims) ExpiresWithin(d time.Duration) bool {
	return !tc.ExpiresAt.IsZero() && time.Until(tc.ExpiresAt) < d
}
