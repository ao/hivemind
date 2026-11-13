package cluster

import (
	"fmt"
	"testing"
	"time"
)

// clockedLimiter is a limiter whose clock the test moves.
func clockedLimiter(limit int, window time.Duration) (*FailLimiter, *time.Time) {
	now := time.Unix(1_700_000_000, 0)
	l := NewFailLimiter(limit, window)
	l.now = func() time.Time { return now }
	return l, &now
}

func TestLimiterAllowsUpToTheLimitPerAddress(t *testing.T) {
	l, _ := clockedLimiter(3, time.Minute)
	for range 3 {
		if !l.Allow("10.0.0.1") {
			t.Fatal("refused before the limit")
		}
		l.Fail("10.0.0.1")
	}
	if l.Allow("10.0.0.1") {
		t.Fatal("allowed after the limit")
	}
	if !l.Allow("10.0.0.2") {
		t.Fatal("one address's failures refused another address")
	}
}

func TestLimiterWindowResets(t *testing.T) {
	l, now := clockedLimiter(1, time.Minute)
	l.Fail("10.0.0.1")
	if l.Allow("10.0.0.1") {
		t.Fatal("allowed inside the window after the limit")
	}
	if got := l.RetryAfter("10.0.0.1"); got != time.Minute {
		t.Fatalf("RetryAfter %s, want 1m0s", got)
	}
	*now = now.Add(30 * time.Second)
	if got := l.RetryAfter("10.0.0.1"); got != 30*time.Second {
		t.Fatalf("RetryAfter after 30s %s, want 30s", got)
	}
	*now = now.Add(31 * time.Second)
	if !l.Allow("10.0.0.1") {
		t.Fatal("refused after the window passed")
	}
	if got := l.RetryAfter("10.0.0.1"); got != 0 {
		t.Fatalf("RetryAfter after the window %s, want 0", got)
	}
}

// A zero limit blocks an address after its first failure.
func TestLimiterZeroLimitBlocksAfterOneFailure(t *testing.T) {
	l, _ := clockedLimiter(0, time.Minute)
	if !l.Allow("10.0.0.1") {
		t.Fatal("refused before any failure")
	}
	l.Fail("10.0.0.1")
	if l.Allow("10.0.0.1") {
		t.Fatal("allowed after one failure with a zero limit")
	}
}

// Past maxTracked addresses, new addresses share one overflow entry, so the
// map stays bounded and a flood from many addresses is still limited.
func TestLimiterBoundsItsMapUnderAFlood(t *testing.T) {
	l, _ := clockedLimiter(2, time.Hour)
	for i := range maxTracked {
		l.Fail(fmt.Sprintf("10.1.%d.%d", i/256, i%256))
	}
	for i := range 3 {
		l.Fail(fmt.Sprintf("192.0.2.%d", i))
	}
	if n := len(l.fails); n > maxTracked+1 {
		t.Fatalf("limiter tracks %d addresses, want at most %d", n, maxTracked+1)
	}
	if l.Allow("198.51.100.7") {
		t.Fatal("a new address escaped the overflow limit")
	}
}

// Expired entries are dropped when the map fills, so a quiet period frees
// room for new addresses.
func TestLimiterPrunesExpiredEntriesWhenFull(t *testing.T) {
	l, now := clockedLimiter(1, time.Minute)
	for i := range maxTracked {
		l.Fail(fmt.Sprintf("10.2.%d.%d", i/256, i%256))
	}
	*now = now.Add(2 * time.Minute)
	l.Fail("198.51.100.9")
	if _, ok := l.fails["198.51.100.9"]; !ok {
		t.Fatal("a new address was not tracked after the old entries expired")
	}
	if len(l.fails) > 1 {
		t.Fatalf("%d entries remain after pruning, want 1", len(l.fails))
	}
}
