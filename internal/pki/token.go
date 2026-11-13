package pki

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

const (
	tokenPrefix = "hivemind-join-v1."
	// tokenSecretLen is 256 bits, above the 128-bit minimum.
	tokenSecretLen = 32

	// DefaultTokenTTL is how long a join token lasts unless the caller says otherwise.
	DefaultTokenTTL = 24 * time.Hour
	// MaxTokenTTL caps how long a token may last.
	MaxTokenTTL = 7 * 24 * time.Hour
)

var (
	// ErrInvalidToken covers malformed, unknown and wrong tokens alike, so the
	// error does not say which one it was.
	ErrInvalidToken        = errors.New("pki: invalid join token")
	ErrTokenUsed           = errors.New("pki: join token already used")
	ErrTokenExpired        = errors.New("pki: join token expired")
	ErrFingerprintMismatch = errors.New("pki: CA fingerprint does not match the join token")
)

// JoinToken is the decoded form of a join token. The text form is
// hivemind-join-v1.<CA fingerprint, 64 hex>.<secret, 43 base64url>.
type JoinToken struct {
	// Fingerprint is the CA SubjectPublicKeyInfo SHA-256 the joining node pins.
	Fingerprint [32]byte
	// Secret is the random part. The server stores only its SHA-256.
	Secret [32]byte
}

// String returns the token text.
func (t JoinToken) String() string {
	return tokenPrefix + hex.EncodeToString(t.Fingerprint[:]) + "." +
		base64.RawURLEncoding.EncodeToString(t.Secret[:])
}

// hash is the value the store keeps. The secret is random and 256 bits long,
// so a plain SHA-256 is enough and no slow hash is needed.
func (t JoinToken) hash() [32]byte {
	return sha256.Sum256(t.Secret[:])
}

// CheckFingerprint returns ErrFingerprintMismatch unless the token names observed.
func (t JoinToken) CheckFingerprint(observed [32]byte) error {
	if t.Fingerprint != observed {
		return ErrFingerprintMismatch
	}
	return nil
}

// ParseJoinToken decodes token text. It accepts only the form String produces,
// so ParseJoinToken(s) succeeding means that s == t.String().
func ParseJoinToken(s string) (JoinToken, error) {
	var t JoinToken
	if !strings.HasPrefix(s, tokenPrefix) {
		return t, ErrInvalidToken
	}
	parts := strings.Split(s[len(tokenPrefix):], ".")
	if len(parts) != 2 {
		return t, ErrInvalidToken
	}
	fp, secret := parts[0], parts[1]
	if len(fp) != 2*len(t.Fingerprint) || strings.ToLower(fp) != fp {
		return t, ErrInvalidToken
	}
	fpBytes, err := hex.DecodeString(fp)
	if err != nil {
		return t, ErrInvalidToken
	}
	copy(t.Fingerprint[:], fpBytes)

	// base64 decoders skip newlines even in strict mode, so compare the
	// re-encoded bytes with the input. That rejects any non-canonical form.
	raw, err := base64.RawURLEncoding.DecodeString(secret)
	if err != nil || len(raw) != tokenSecretLen || base64.RawURLEncoding.EncodeToString(raw) != secret {
		return t, ErrInvalidToken
	}
	copy(t.Secret[:], raw)
	return t, nil
}

// TokenStore holds the hashes of issued join tokens. A store must make Consume
// atomic: two concurrent calls for one hash can't both succeed. SQLite will
// implement this later.
type TokenStore interface {
	// Add records a new unused token hash that expires at expires. It fails
	// if the hash is already stored.
	Add(hash [32]byte, expires time.Time) error
	// Consume marks the hash used. It fails with ErrInvalidToken if the hash
	// is unknown, ErrTokenExpired if now is at or past expiry, and ErrTokenUsed
	// if it was already consumed.
	Consume(hash [32]byte, now time.Time) error
}

// MemoryStore is an in-memory TokenStore. It is safe for concurrent use.
type MemoryStore struct {
	mu     sync.Mutex
	tokens map[[32]byte]*memToken
}

type memToken struct {
	expires time.Time
	used    bool
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{tokens: make(map[[32]byte]*memToken)}
}

// Add implements TokenStore.
func (s *MemoryStore) Add(hash [32]byte, expires time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tokens[hash]; ok {
		return errors.New("pki: token hash already stored")
	}
	s.tokens[hash] = &memToken{expires: expires}
	return nil
}

// Consume implements TokenStore. The lock covers the check and the update, so
// one token can be consumed by one caller only.
func (s *MemoryStore) Consume(hash [32]byte, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tokens[hash]
	if !ok {
		return ErrInvalidToken
	}
	if !now.Before(t.expires) {
		return ErrTokenExpired
	}
	if t.used {
		return ErrTokenUsed
	}
	t.used = true
	return nil
}

// Tokens issues and redeems join tokens against a TokenStore.
type Tokens struct {
	store TokenStore
	now   func() time.Time
}

// NewTokens returns a Tokens service backed by store.
func NewTokens(store TokenStore) *Tokens {
	return &Tokens{store: store, now: time.Now}
}

// Issue makes a new join token for the CA with fingerprint fp. ttl of zero
// means DefaultTokenTTL. Negative values, and values above MaxTokenTTL, are
// refused. It returns the token text and its expiry.
func (t *Tokens) Issue(fp [32]byte, ttl time.Duration) (string, time.Time, error) {
	if ttl == 0 {
		ttl = DefaultTokenTTL
	}
	if ttl < 0 || ttl > MaxTokenTTL {
		return "", time.Time{}, fmt.Errorf("pki: token ttl %s out of range", ttl)
	}
	tok := JoinToken{Fingerprint: fp}
	if _, err := io.ReadFull(rand.Reader, tok.Secret[:]); err != nil {
		return "", time.Time{}, fmt.Errorf("pki: token secret: %w", err)
	}
	expires := t.now().Add(ttl)
	if err := t.store.Add(tok.hash(), expires); err != nil {
		return "", time.Time{}, err
	}
	return tok.String(), expires, nil
}

// Redeem consumes a join token and returns the CA fingerprint it names. A token
// redeems once. A malformed, unknown or wrong token gives ErrInvalidToken.
func (t *Tokens) Redeem(s string) ([32]byte, error) {
	tok, err := ParseJoinToken(s)
	if err != nil {
		return [32]byte{}, err
	}
	if err := t.store.Consume(tok.hash(), t.now()); err != nil {
		return [32]byte{}, err
	}
	return tok.Fingerprint, nil
}
