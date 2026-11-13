package cluster

import (
	"sync"
	"time"
)

// Defaults for NewFailLimiter in Listen: a source address may fail DefaultFailLimit
// joins in each DefaultFailWindow. A failed join is one that was refused.
const (
	DefaultFailLimit  = 5
	DefaultFailWindow = 10 * time.Minute

	// maxTracked bounds the map. Past it, a source that is not tracked
	// shares one overflow entry, so memory stays bounded and the limit still
	// applies to a flood from many addresses.
	maxTracked = 4096
	overflow   = "*overflow*"
)

// FailLimiter counts failed joins per source address. It is safe for
// concurrent use.
//
// A token is 256 random bits, so no guessing will find one; the limit is there
// so a flood of bad requests can't keep the CA busy, and so repeated failures
// from one address are visible in the log.
type FailLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	now    func() time.Time
	fails  map[string]*failCount
}

type failCount struct {
	start time.Time
	n     int
}

// NewFailLimiter allows limit failures per address in each window. Once an
// address has reached the limit, its joins are refused until the window ends.
func NewFailLimiter(limit int, window time.Duration) *FailLimiter {
	return &FailLimiter{limit: limit, window: window, now: time.Now, fails: map[string]*failCount{}}
}

// Allow reports whether addr may try to join now.
func (l *FailLimiter) Allow(addr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	c := l.lookup(addr)
	if c == nil {
		return true
	}
	if now.Sub(c.start) >= l.window {
		return true
	}
	return c.n < l.limit
}

// Fail records a refused join from addr.
func (l *FailLimiter) Fail(addr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	key := l.key(addr)
	c := l.fails[key]
	if c == nil || now.Sub(c.start) >= l.window {
		c = &failCount{start: now}
		l.fails[key] = c
	}
	c.n++
}

// RetryAfter is how long addr must wait before a join is allowed again. It is
// zero when addr may join now.
func (l *FailLimiter) RetryAfter(addr string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.lookup(addr)
	if c == nil || c.n < l.limit {
		return 0
	}
	return max(0, l.window-l.now().Sub(c.start))
}

// lookup returns the count for addr, or nil. Caller holds mu.
func (l *FailLimiter) lookup(addr string) *failCount {
	return l.fails[l.key(addr)]
}

// key is the map key for addr. Caller holds mu. When the map is full, an
// untracked address uses the overflow entry, after expired entries are dropped.
func (l *FailLimiter) key(addr string) string {
	if _, ok := l.fails[addr]; ok || len(l.fails) < maxTracked {
		return addr
	}
	l.prune(l.now())
	if len(l.fails) < maxTracked {
		return addr
	}
	return overflow
}

// prune drops entries whose window has passed. Caller holds mu.
func (l *FailLimiter) prune(now time.Time) {
	for k, c := range l.fails {
		if now.Sub(c.start) >= l.window {
			delete(l.fails, k)
		}
	}
}
