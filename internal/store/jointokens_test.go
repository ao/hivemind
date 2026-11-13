package store

import (
	"crypto/sha256"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/pki"
)

func hashOf(s string) [32]byte { return sha256.Sum256([]byte(s)) }

func TestJoinTokenUsedOnce(t *testing.T) {
	s := openT(t, filepath.Join(t.TempDir(), "hivemind.db"))
	ts := s.JoinTokens()
	h := hashOf("one")
	t0 := time.Unix(1_700_000_000, 0)
	if err := ts.Add(h, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := ts.Consume(h, t0); err != nil {
		t.Fatalf("first use: %v", err)
	}
	if err := ts.Consume(h, t0.Add(time.Minute)); !errors.Is(err, pki.ErrTokenUsed) {
		t.Fatalf("second use: got %v, want ErrTokenUsed", err)
	}
	if err := ts.Add(h, t0.Add(time.Hour)); err == nil {
		t.Fatal("adding a stored hash again succeeded")
	}
}

func TestJoinTokenExpired(t *testing.T) {
	s := openT(t, filepath.Join(t.TempDir(), "hivemind.db"))
	ts := s.JoinTokens()
	h := hashOf("old")
	t0 := time.Unix(1_700_000_000, 0)
	if err := ts.Add(h, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	// At the expiry instant the token is already dead.
	if err := ts.Consume(h, t0.Add(time.Minute)); !errors.Is(err, pki.ErrTokenExpired) {
		t.Fatalf("at expiry: got %v, want ErrTokenExpired", err)
	}
	if err := ts.Consume(h, t0.Add(2*time.Minute)); !errors.Is(err, pki.ErrTokenExpired) {
		t.Fatalf("after expiry: got %v, want ErrTokenExpired", err)
	}
}

func TestJoinTokenUnknown(t *testing.T) {
	s := openT(t, filepath.Join(t.TempDir(), "hivemind.db"))
	if err := s.JoinTokens().Consume(hashOf("never issued"), time.Now()); !errors.Is(err, pki.ErrInvalidToken) {
		t.Fatalf("got %v, want ErrInvalidToken", err)
	}
}

// Twenty callers race for one token. Exactly one may get it.
func TestJoinTokenConsumeIsAtomic(t *testing.T) {
	s := openT(t, filepath.Join(t.TempDir(), "hivemind.db"))
	ts := s.JoinTokens()
	h := hashOf("raced")
	now := time.Unix(1_700_000_000, 0)
	if err := ts.Add(h, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ts.Consume(h, now) == nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := wins.Load(); n != 1 {
		t.Fatalf("%d callers consumed the token, want 1", n)
	}
}

func TestPruneJoinTokens(t *testing.T) {
	s := openT(t, filepath.Join(t.TempDir(), "hivemind.db"))
	ts := s.JoinTokens()
	t0 := time.Unix(1_700_000_000, 0)
	expired, live, used := hashOf("expired"), hashOf("live"), hashOf("used")
	for _, h := range [][32]byte{expired, live, used} {
		exp := t0.Add(time.Hour)
		if h == expired {
			exp = t0.Add(time.Minute)
		}
		if err := ts.Add(h, exp); err != nil {
			t.Fatal(err)
		}
	}
	if err := ts.Consume(used, t0); err != nil {
		t.Fatal(err)
	}

	n, err := s.PruneJoinTokens(ctxT(t), t0.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("pruned %d tokens, want 1 (the expired one)", n)
	}
	if err := ts.Consume(expired, t0); !errors.Is(err, pki.ErrInvalidToken) {
		t.Fatalf("pruned token: got %v, want ErrInvalidToken", err)
	}
	if err := ts.Consume(live, t0.Add(2*time.Minute)); err != nil {
		t.Fatalf("live token after prune: %v", err)
	}
}

// A token issued before a restart still redeems once after it.
func TestJoinTokenSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hivemind.db")
	h := hashOf("persisted")
	t0 := time.Unix(1_700_000_000, 0)
	s := openT(t, path)
	if err := s.JoinTokens().Add(h, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2 := openT(t, path)
	if err := s2.JoinTokens().Consume(h, t0); err != nil {
		t.Fatalf("after reopen: %v", err)
	}
	if err := s2.JoinTokens().Consume(h, t0); !errors.Is(err, pki.ErrTokenUsed) {
		t.Fatalf("second use after reopen: got %v, want ErrTokenUsed", err)
	}
}
