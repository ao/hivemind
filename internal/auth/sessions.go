package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Sessions holds UI sessions in memory with an absolute expiry. A restart
// ends every session, and users log in again.
//
// The map is keyed by the SHA-256 digest of the session ID, so memory holds
// no value that works as a cookie. Session IDs are 256 bits from crypto/rand,
// so a lookup by key does not need a constant-time compare: no key can be
// guessed.
type Sessions struct {
	mu  sync.Mutex
	ttl time.Duration
	now func() time.Time
	m   map[[sha256.Size]byte]session
}

type session struct {
	csrf    string
	expires time.Time
}

// NewSessions returns an empty session store whose sessions last ttl.
func NewSessions(ttl time.Duration) (*Sessions, error) {
	if ttl <= 0 {
		return nil, errors.New("auth: session TTL must be positive")
	}
	return &Sessions{ttl: ttl, now: time.Now, m: make(map[[sha256.Size]byte]session)}, nil
}

// TTL returns how long a session lasts.
func (s *Sessions) TTL() time.Duration { return s.ttl }

// Create starts a session. It returns the session ID, which goes in the
// cookie, and the CSRF token, which the UI sends back on unsafe requests.
// Expired sessions are removed here.
func (s *Sessions) Create() (id, csrf string, err error) {
	if id, err = randomToken(); err != nil {
		return "", "", err
	}
	if csrf, err = randomToken(); err != nil {
		return "", "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, v := range s.m {
		if !now.Before(v.expires) {
			delete(s.m, k)
		}
	}
	s.m[sessionKey(id)] = session{csrf: csrf, expires: now.Add(s.ttl)}
	return id, csrf, nil
}

// Lookup returns the CSRF token of a live session. An expired session is
// removed and reported as missing.
func (s *Sessions) Lookup(id string) (csrf string, ok bool) {
	k := sessionKey(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, found := s.m[k]
	if !found {
		return "", false
	}
	if !s.now().Before(sess.expires) {
		delete(s.m, k)
		return "", false
	}
	return sess.csrf, true
}

// Delete ends a session. Deleting an unknown session does nothing.
func (s *Sessions) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, sessionKey(id))
}

func sessionKey(id string) [sha256.Size]byte {
	return sha256.Sum256([]byte(id))
}

// randomToken returns 32 bytes from crypto/rand, unpadded base64url.
func randomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("auth: read random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
