// Package auth holds the authentication building blocks for the hivemind API
// and UI: the admin token and its file, the bearer and session checks, CSRF,
// and a router that lists its routes. The package opens no listener. A router
// that serves the API wires these pieces in.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

const (
	// TokenPrefix marks admin tokens, so a leaked token is easy to spot in
	// logs and secret scanners.
	TokenPrefix = "hm_"

	tokenBytes = 32
	// tokenLen is the length of a well-formed token: the prefix plus the
	// 43 unpadded base64url characters that encode 32 bytes.
	tokenLen = len(TokenPrefix) + 43
)

// ErrInvalidToken means a string is not a well-formed admin token. The error
// never includes the string itself.
var ErrInvalidToken = errors.New("auth: malformed admin token")

// TokenHash is the SHA-256 digest of an admin token. The server keeps this
// digest and never the token.
type TokenHash [sha256.Size]byte

// NewToken returns a new admin token: 32 bytes from crypto/rand, encoded as
// unpadded base64url, with the hm_ prefix.
func NewToken() (string, error) {
	var b [tokenBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("auth: read random bytes: %w", err)
	}
	return TokenPrefix + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// ParseToken checks the shape of a token: the prefix, the length, and a
// base64url body that decodes to 32 bytes. It does not check the token
// against any cluster.
func ParseToken(s string) error {
	if len(s) != tokenLen || !strings.HasPrefix(s, TokenPrefix) {
		return ErrInvalidToken
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(s[len(TokenPrefix):])
	if err != nil || len(b) != tokenBytes {
		return ErrInvalidToken
	}
	return nil
}

// HashToken returns the SHA-256 digest of a token.
//
// A slow password hash such as bcrypt or argon2 slows down guessing of
// low-entropy secrets. An admin token is 256 bits from crypto/rand, so no
// guess is feasible and the extra cost buys nothing. SHA-256 is enough, and
// the digest is what the server keeps. A leaked digest does not reveal the
// token.
func HashToken(token string) TokenHash {
	return sha256.Sum256([]byte(token))
}

// VerifyToken reports whether presented is the token that want was made
// from. Both sides are digests of the same length, and the comparison is
// subtle.ConstantTimeCompare, so the time taken does not reveal how many
// leading bytes matched.
func VerifyToken(presented string, want TokenHash) bool {
	got := HashToken(presented)
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}
