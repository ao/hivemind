package auth

import (
	"crypto/sha256"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time { return c.t }

func newTestSessions(t *testing.T, ttl time.Duration) (*Sessions, *fakeClock) {
	t.Helper()
	s, err := NewSessions(ttl)
	if err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{t: time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)}
	s.now = clock.Now
	return s, clock
}

func TestSessionCreateLookupDelete(t *testing.T) {
	s, _ := newTestSessions(t, time.Hour)
	id1, csrf1, err := s.Create()
	if err != nil {
		t.Fatal(err)
	}
	id2, csrf2, err := s.Create()
	if err != nil {
		t.Fatal(err)
	}
	if id1 == id2 || csrf1 == csrf2 {
		t.Fatal("two sessions share an ID or a CSRF token")
	}
	if id1 == csrf1 {
		t.Fatal("session ID and CSRF token are the same value")
	}

	if got, ok := s.Lookup(id1); !ok || got != csrf1 {
		t.Fatalf("Lookup(id1) = (%q, %v), want (%q, true)", got, ok, csrf1)
	}
	if _, ok := s.Lookup("not-a-session"); ok {
		t.Fatal("Lookup of an unknown ID succeeded")
	}
	if _, ok := s.Lookup(""); ok {
		t.Fatal("Lookup of the empty ID succeeded")
	}

	s.Delete(id1)
	if _, ok := s.Lookup(id1); ok {
		t.Fatal("deleted session still found")
	}
	if got, ok := s.Lookup(id2); !ok || got != csrf2 {
		t.Fatal("deleting one session removed another")
	}
	s.Delete(id1) // deleting twice is harmless
}

func TestSessionExpiry(t *testing.T) {
	s, clock := newTestSessions(t, time.Hour)
	id, _, err := s.Create()
	if err != nil {
		t.Fatal(err)
	}
	clock.t = clock.t.Add(time.Hour - time.Second)
	if _, ok := s.Lookup(id); !ok {
		t.Fatal("session expired one second early")
	}
	clock.t = clock.t.Add(time.Second) // now exactly at the expiry instant
	if _, ok := s.Lookup(id); ok {
		t.Fatal("session still live at its expiry instant")
	}
	if _, ok := s.Lookup(id); ok {
		t.Fatal("expired session came back")
	}
}

func TestSessionCreatePrunesExpired(t *testing.T) {
	s, clock := newTestSessions(t, time.Minute)
	for i := 0; i < 5; i++ {
		if _, _, err := s.Create(); err != nil {
			t.Fatal(err)
		}
	}
	clock.t = clock.t.Add(2 * time.Minute)
	if _, _, err := s.Create(); err != nil {
		t.Fatal(err)
	}
	if n := len(s.m); n != 1 {
		t.Fatalf("store holds %d sessions after pruning, want 1", n)
	}
}

// TestSessionKeyedByDigest checks that the store keeps no usable cookie
// value: the map key is the SHA-256 of the ID, never the ID itself.
func TestSessionKeyedByDigest(t *testing.T) {
	s, _ := newTestSessions(t, time.Hour)
	id, _, err := s.Create()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.m[sha256.Sum256([]byte(id))]; !ok {
		t.Fatal("session is not stored under the digest of its ID")
	}
	for k := range s.m {
		if string(k[:]) == id {
			t.Fatal("raw session ID found as a map key")
		}
	}
}

func TestNewSessionsRejectsNonPositiveTTL(t *testing.T) {
	for _, ttl := range []time.Duration{0, -time.Second} {
		if _, err := NewSessions(ttl); err == nil {
			t.Fatalf("NewSessions(%v) accepted a non-positive TTL", ttl)
		}
	}
}

func TestNewAuthenticatorRefusesEmptyInputs(t *testing.T) {
	s, _ := newTestSessions(t, time.Hour)
	if _, err := NewAuthenticator(TokenHash{}, s); err == nil {
		t.Fatal("an all-zero admin hash was accepted")
	}
	tok, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewAuthenticator(HashToken(tok), nil); err == nil {
		t.Fatal("a nil session store was accepted")
	}
}
