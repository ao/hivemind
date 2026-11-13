package pki

import (
	"bytes"
	"crypto/rand"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestTokens(t *testing.T, at time.Time) (*Tokens, *MemoryStore) {
	t.Helper()
	store := NewMemoryStore()
	tk := NewTokens(store)
	tk.now = func() time.Time { return at }
	return tk, store
}

func randomFingerprint(t *testing.T) [32]byte {
	t.Helper()
	var fp [32]byte
	if _, err := rand.Read(fp[:]); err != nil {
		t.Fatal(err)
	}
	return fp
}

func TestJoinTokenFormatRoundTrip(t *testing.T) {
	t0 := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	tk, _ := newTestTokens(t, t0)
	fp := randomFingerprint(t)
	s, expires, err := tk.Issue(fp, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := t0.Add(DefaultTokenTTL); !expires.Equal(want) {
		t.Fatalf("default expiry = %s, want %s", expires, want)
	}
	if !strings.HasPrefix(s, tokenPrefix) {
		t.Fatalf("token %q lacks prefix", s)
	}
	tok, err := ParseJoinToken(s)
	if err != nil {
		t.Fatal(err)
	}
	if tok.String() != s {
		t.Fatalf("String() = %q, want %q", tok.String(), s)
	}
	if tok.Fingerprint != fp {
		t.Fatal("fingerprint did not survive the round trip")
	}
}

func TestJoinTokenSecretsAreRandom(t *testing.T) {
	tk, _ := newTestTokens(t, time.Now())
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		s, _, err := tk.Issue([32]byte{}, 0)
		if err != nil {
			t.Fatal(err)
		}
		tok, err := ParseJoinToken(s)
		if err != nil {
			t.Fatal(err)
		}
		// Secret is a [32]byte, so len is a constant: compare it to zero instead.
		if tok.Secret == ([32]byte{}) || seen[s] {
			t.Fatalf("secret repeated or short: %q", s)
		}
		seen[s] = true
	}
}

func TestJoinTokenRedeemsOnce(t *testing.T) {
	tk, _ := newTestTokens(t, time.Now())
	fp := randomFingerprint(t)
	s, _, err := tk.Issue(fp, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := tk.Redeem(s)
	if err != nil || got != fp {
		t.Fatalf("first redeem = %x, %v; want the fingerprint", got, err)
	}
	if _, err := tk.Redeem(s); !errors.Is(err, ErrTokenUsed) {
		t.Fatalf("second redeem err = %v, want ErrTokenUsed", err)
	}
}

func TestJoinTokenExpires(t *testing.T) {
	t0 := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	tk, _ := newTestTokens(t, t0)
	s, _, err := tk.Issue(randomFingerprint(t), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	tk.now = func() time.Time { return t0.Add(time.Hour) } // expiry is exclusive
	if _, err := tk.Redeem(s); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("redeem at expiry err = %v, want ErrTokenExpired", err)
	}

	tk2, _ := newTestTokens(t, t0)
	s2, _, err := tk2.Issue(randomFingerprint(t), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	tk2.now = func() time.Time { return t0.Add(time.Hour - time.Second) }
	if _, err := tk2.Redeem(s2); err != nil {
		t.Fatalf("redeem a second before expiry: %v", err)
	}
}

func TestJoinTokenWrongOrMalformedFails(t *testing.T) {
	tk, _ := newTestTokens(t, time.Now())
	fp := randomFingerprint(t)
	s, _, err := tk.Issue(fp, 0)
	if err != nil {
		t.Fatal(err)
	}

	// A well-formed token that was never issued.
	var stranger JoinToken
	stranger.Fingerprint = fp
	if _, err := rand.Read(stranger.Secret[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := tk.Redeem(stranger.String()); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("unissued token err = %v, want ErrInvalidToken", err)
	}

	// The issued token with its secret changed.
	tok, err := ParseJoinToken(s)
	if err != nil {
		t.Fatal(err)
	}
	tok.Secret[0] ^= 0x01
	if _, err := tk.Redeem(tok.String()); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("altered token err = %v, want ErrInvalidToken", err)
	}

	for _, bad := range []string{
		"", "x", tokenPrefix, tokenPrefix + ".", "hivemind-join-v2." + strings.TrimPrefix(s, tokenPrefix),
		strings.ToUpper(s), s + ".extra", strings.Replace(s, ".", "", 1),
	} {
		if _, err := tk.Redeem(bad); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("malformed %q err = %v, want ErrInvalidToken", bad, err)
		}
	}
	// The real token still redeems: the failures above didn't use it up.
	if _, err := tk.Redeem(s); err != nil {
		t.Fatalf("valid token failed after bad attempts: %v", err)
	}
}

func TestJoinTokenConcurrentConsumptionLetsExactlyOneWin(t *testing.T) {
	tk, _ := newTestTokens(t, time.Now())
	s, _, err := tk.Issue(randomFingerprint(t), 0)
	if err != nil {
		t.Fatal(err)
	}
	const callers = 64
	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		mu      sync.Mutex
		winners int
	)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := tk.Redeem(s); err == nil {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if winners != 1 {
		t.Fatalf("%d of %d concurrent redeems succeeded, want exactly 1", winners, callers)
	}
}

func TestStoreHoldsOnlyTheHash(t *testing.T) {
	tk, store := newTestTokens(t, time.Now())
	s, _, err := tk.Issue(randomFingerprint(t), 0)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := ParseJoinToken(s)
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.tokens) != 1 {
		t.Fatalf("store has %d entries, want 1", len(store.tokens))
	}
	for k := range store.tokens {
		if k != tok.hash() {
			t.Fatal("store key is not the SHA-256 of the secret")
		}
		if bytes.Equal(k[:], tok.Secret[:]) {
			t.Fatal("store holds the raw secret")
		}
	}
}

func TestFingerprintMismatchIsDetected(t *testing.T) {
	tk, _ := newTestTokens(t, time.Now())
	fp := randomFingerprint(t)
	s, _, err := tk.Issue(fp, 0)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := ParseJoinToken(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := tok.CheckFingerprint(fp); err != nil {
		t.Fatalf("matching fingerprint: %v", err)
	}
	other := randomFingerprint(t)
	if err := tok.CheckFingerprint(other); !errors.Is(err, ErrFingerprintMismatch) {
		t.Fatalf("mismatch err = %v, want ErrFingerprintMismatch", err)
	}
}

func TestJoinTokenTTLBounds(t *testing.T) {
	tk, _ := newTestTokens(t, time.Now())
	if _, _, err := tk.Issue([32]byte{}, -time.Second); err == nil {
		t.Fatal("negative TTL accepted")
	}
	if _, _, err := tk.Issue([32]byte{}, MaxTokenTTL+time.Second); err == nil {
		t.Fatal("TTL above the maximum accepted")
	}
	if _, _, err := tk.Issue([32]byte{}, MaxTokenTTL); err != nil {
		t.Fatalf("maximum TTL refused: %v", err)
	}
}

func TestMemoryStoreRejectsDuplicateHash(t *testing.T) {
	s := NewMemoryStore()
	var h [32]byte
	h[0] = 1
	if err := s.Add(h, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(h, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("duplicate hash was stored twice")
	}
}
