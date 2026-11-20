package router

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/certmagic"
)

// Storage is what the router needs of the shared TLS storage: certmagic's
// Storage, and TryLocker, which certmagic uses when a storage has it, as
// tlsstore.Storage does.
type Storage interface {
	certmagic.Storage
	certmagic.TryLocker
}

// Limits on the storage reads that HTTP-01 challenge requests cause.
const (
	// DefaultChallengeReadInterval is the least time between the starts of
	// two reads of one host's challenge token: at most 4 a second.
	DefaultChallengeReadInterval = 250 * time.Millisecond
	// challengeReadTimeout bounds one read.
	challengeReadTimeout = 5 * time.Second
)

// challengeStorage is the Storage the router gives certmagic. certmagic
// answers GET /.well-known/acme-challenge/<anything> for a routed host from
// the challenge it started itself, if any, and otherwise reads the host's
// challenge token from storage, whatever the token in the path: any client
// can make it read, and on a worker each read is a request to the server's
// node API. Those reads go through reads; every other call goes to the
// Storage as it is.
type challengeStorage struct {
	Storage
	reads *challengeReads
}

func (s challengeStorage) Load(ctx context.Context, key string) ([]byte, error) {
	if isChallengeKey(key) {
		return s.reads.load(ctx, key)
	}
	return s.Storage.Load(ctx, key)
}

// String is the Storage's, which certmagic logs.
func (s challengeStorage) String() string { return fmt.Sprint(s.Storage) }

// isChallengeKey reports whether key is where certmagic's distributed
// HTTP-01 solver keeps a host's challenge: acme/<issuer>/challenge_tokens/
// <host>.json (challengeTokensKey in its solvers.go).
func isChallengeKey(key string) bool {
	return strings.HasPrefix(key, "acme/") && strings.HasSuffix(key, ".json") &&
		path.Base(path.Dir(key)) == "challenge_tokens"
}

// IsChallengeKey is isChallengeKey, for a server's TLS storage, which reads
// challenge tokens from the leader.
func IsChallengeKey(key string) bool { return isChallengeKey(key) }

// challengeReads bounds the reads of challenge tokens. For each key, one
// read runs at a time, and a read starts no sooner than every after the one
// before it started, so one host's challenge requests cause at most one read
// per every, however many clients send them. A caller waits for the first
// read that starts after it arrived, never a result from before, so a token
// that another router stored just before the CA's request is found; callers
// that arrive before that read starts share it. Reads run under the router's
// context, so a caller that goes away does not cancel one that others wait
// for.
type challengeReads struct {
	ctx     context.Context
	backend func(ctx context.Context, key string) ([]byte, error)
	every   time.Duration
	log     *slog.Logger

	mu      sync.Mutex
	slots   map[string]*challengeSlot
	swept   time.Time
	failing bool // the last read failed for another reason than a missing key
}

// challengeSlot is one key's reads: the one running, the one waiting to
// start, which new callers join, and when the last one started.
type challengeSlot struct {
	cur, next *challengeRead
	last      time.Time
}

type challengeRead struct {
	done chan struct{}
	val  []byte
	err  error
}

func newChallengeReads(ctx context.Context, backend func(context.Context, string) ([]byte, error), every time.Duration, log *slog.Logger) *challengeReads {
	return &challengeReads{ctx: ctx, backend: backend, every: every, log: log, slots: map[string]*challengeSlot{}}
}

// load returns the value of key from the next read of it, or ctx's error if
// ctx ends first.
func (c *challengeReads) load(ctx context.Context, key string) ([]byte, error) {
	c.mu.Lock()
	c.sweep()
	s := c.slots[key]
	if s == nil {
		s = &challengeSlot{}
		c.slots[key] = s
	}
	r := s.next
	if r == nil {
		r = &challengeRead{done: make(chan struct{})}
		s.next = r
		go c.run(key, s, r)
	}
	c.mu.Unlock()
	select {
	case <-r.done:
		return r.val, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// run starts r once the read before it has ended and every has passed since
// that one started, reads key, and hands the result to r's callers.
func (c *challengeReads) run(key string, s *challengeSlot, r *challengeRead) {
	defer close(r.done)
	c.mu.Lock()
	cur, start := s.cur, s.last.Add(c.every)
	c.mu.Unlock()
	if cur != nil {
		select {
		case <-cur.done:
		case <-c.ctx.Done():
		}
	}
	if d := time.Until(start); d > 0 && c.ctx.Err() == nil {
		t := time.NewTimer(d)
		select {
		case <-t.C:
		case <-c.ctx.Done():
		}
		t.Stop()
	}
	c.mu.Lock()
	if err := c.ctx.Err(); err != nil {
		if s.next == r {
			s.next = nil
		}
		c.mu.Unlock()
		r.err = err
		return
	}
	s.next, s.cur, s.last = nil, r, time.Now()
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(c.ctx, challengeReadTimeout)
	r.val, r.err = c.backend(ctx, key)
	cancel()

	c.mu.Lock()
	if s.cur == r {
		s.cur = nil
	}
	switch {
	case r.err == nil || isNotExist(r.err):
		if c.failing {
			c.log.Info("reading challenge tokens from the TLS storage again")
		}
		c.failing = false
	case !c.failing:
		c.log.Warn("reading a challenge token from the TLS storage; challenges are not answered until it works", "key", key, "err", r.err)
		c.failing = true
	default:
		c.log.Debug("reading a challenge token from the TLS storage", "key", key, "err", r.err)
	}
	c.mu.Unlock()
	if r.err != nil && !isNotExist(r.err) {
		r.err = unreadableToken{r.err}
	}
}

// sweep forgets, once a minute, the keys that have no read and whose last
// read started more than every ago, such as those of hosts that left the
// table. c.mu is held.
func (c *challengeReads) sweep() {
	now := time.Now()
	if now.Sub(c.swept) < time.Minute {
		return
	}
	c.swept = now
	for k, s := range c.slots {
		if s.cur == nil && s.next == nil && now.Sub(s.last) > c.every {
			delete(c.slots, k)
		}
	}
}

func isNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }

// unreadableToken is a read of a challenge token that failed for another
// reason than a missing key, as when the server is away. certmagic takes it
// as no challenge for the host, which it logs at debug level, and does not
// answer the request; the router has logged the cause once (see run).
// Passed as it is, certmagic would log a warning for every request, so any
// client could flood the log while the server is away.
type unreadableToken struct{ err error }

func (e unreadableToken) Error() string        { return "challenge token not readable: " + e.err.Error() }
func (e unreadableToken) Unwrap() error        { return e.err }
func (e unreadableToken) Is(target error) bool { return target == fs.ErrNotExist }
