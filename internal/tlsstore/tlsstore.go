// Package tlsstore is certmagic's Storage for the routers: the certificates,
// private keys, ACME account and challenge tokens that every router in the
// cluster shares, and the locks around issuing. On the server it is backed
// by the store; on a worker, by the node API, which the server answers from
// the same store.
//
// Sharing storage is what certmagic needs to run in a cluster. Its Storage
// documentation: "Processes running in a cluster should use the same Storage
// value (with the same configuration) in order to share certificates and
// other TLS resources with the cluster." Its HTTP-01 solver writes each
// challenge to this storage ("distributedSolver allows the ACME HTTP-01 and
// TLS-ALPN challenges to be solved by an instance other than the one which
// initiated it ... The only requirement is that the instance which
// initiates the challenge shares the same storage and locker with the
// others in the cluster"), so whichever router the CA's request reaches can
// answer a challenge that another router started.
//
// Locks follow the Locker documentation: Lock "blocking until the lock can be
// obtained or an error is returned. Only one lock for the given name can
// exist at a time. A call to Lock for a name which already exists blocks
// until the named lock is released or becomes stale." A lock here is a row
// with an expiry, which its holder pushes forward while it holds the lock, as
// certmagic's FileStorage rewrites the timestamp in its lock file. A holder
// that dies stops doing so, and the lock becomes stale when it expires.
package tlsstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/caddyserver/certmagic"

	"github.com/ao/hivemind/internal/store"
)

// Defaults for Options.
const (
	// DefaultLockTTL is how long a lock lives without a renewal. A holder
	// that stops renewing, because it crashed or lost the server, loses the
	// lock this long after its last renewal.
	DefaultLockTTL = 30 * time.Second
	// DefaultLockRefresh is how often a holder renews its locks.
	DefaultLockRefresh = 10 * time.Second
	// DefaultLockPoll is how often Lock tries again while the lock is held.
	DefaultLockPoll = time.Second
)

// Backend is the shared storage: the store's TLS methods, directly on the
// server or over the node API on a worker. A missing key is an error that
// matches fs.ErrNotExist.
type Backend interface {
	Load(ctx context.Context, key string) ([]byte, error)
	Store(ctx context.Context, key string, value []byte) error
	Delete(ctx context.Context, key string) error
	Exists(ctx context.Context, key string) (bool, error)
	List(ctx context.Context, prefix string, recursive bool) ([]string, error)
	Stat(ctx context.Context, key string) (store.TLSKeyInfo, error)
	TryLock(ctx context.Context, name, owner string, ttl time.Duration) (bool, error)
	RenewLock(ctx context.Context, name, owner string, ttl time.Duration) error
	Unlock(ctx context.Context, name, owner string) error
}

// StoreBackend is the Backend of the server's router: the store itself.
// Owners are prefixed with the server's node ID, as the node API prefixes a
// worker's owners with its own.
type StoreBackend struct {
	St     *store.Store
	NodeID string
}

// Load implements Backend.
func (b StoreBackend) Load(ctx context.Context, key string) ([]byte, error) {
	return b.St.TLSLoad(ctx, key)
}

// Store implements Backend.
func (b StoreBackend) Store(ctx context.Context, key string, value []byte) error {
	return b.St.TLSStore(ctx, key, value)
}

// Delete implements Backend.
func (b StoreBackend) Delete(ctx context.Context, key string) error { return b.St.TLSDelete(ctx, key) }

// Exists implements Backend.
func (b StoreBackend) Exists(ctx context.Context, key string) (bool, error) {
	return b.St.TLSExists(ctx, key)
}

// List implements Backend.
func (b StoreBackend) List(ctx context.Context, prefix string, recursive bool) ([]string, error) {
	return b.St.TLSList(ctx, prefix, recursive)
}

// Stat implements Backend.
func (b StoreBackend) Stat(ctx context.Context, key string) (store.TLSKeyInfo, error) {
	return b.St.TLSStat(ctx, key)
}

// TryLock implements Backend.
func (b StoreBackend) TryLock(ctx context.Context, name, owner string, ttl time.Duration) (bool, error) {
	return b.St.TLSTryLock(ctx, name, b.NodeID+"/"+owner, ttl)
}

// RenewLock implements Backend.
func (b StoreBackend) RenewLock(ctx context.Context, name, owner string, ttl time.Duration) error {
	return b.St.TLSRenewLock(ctx, name, b.NodeID+"/"+owner, ttl)
}

// Unlock implements Backend.
func (b StoreBackend) Unlock(ctx context.Context, name, owner string) error {
	return b.St.TLSUnlock(ctx, name, b.NodeID+"/"+owner)
}

// Options tune a Storage. The zero value uses the defaults.
type Options struct {
	LockTTL     time.Duration
	LockRefresh time.Duration
	LockPoll    time.Duration
	// Logger receives a line when a lock is lost. Nil means slog.Default().
	Logger *slog.Logger
	// Name is what String returns, for certmagic's logs.
	Name string
}

// Storage is certmagic.Storage and certmagic.TryLocker over a Backend. It is
// safe for concurrent use.
type Storage struct {
	b    Backend
	opts Options
	log  *slog.Logger
	id   string // random, so two processes on one node never share an owner

	mu   sync.Mutex
	seq  uint64
	held map[string]*hold
}

type hold struct {
	owner string
	stop  chan struct{}
	done  chan struct{}
}

var (
	_ certmagic.Storage   = (*Storage)(nil)
	_ certmagic.TryLocker = (*Storage)(nil)
)

// New returns a Storage on b.
func New(b Backend, opts Options) (*Storage, error) {
	if opts.LockTTL <= 0 {
		opts.LockTTL = DefaultLockTTL
	}
	if opts.LockRefresh <= 0 {
		opts.LockRefresh = DefaultLockRefresh
	}
	if opts.LockPoll <= 0 {
		opts.LockPoll = DefaultLockPoll
	}
	if opts.LockRefresh >= opts.LockTTL {
		return nil, errors.New("tlsstore: the lock refresh must be shorter than the lock lifetime")
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	var r [6]byte
	if _, err := rand.Read(r[:]); err != nil {
		return nil, err
	}
	return &Storage{b: b, opts: opts, log: log, id: hex.EncodeToString(r[:]), held: map[string]*hold{}}, nil
}

// String names the storage in certmagic's logs.
func (s *Storage) String() string {
	if s.opts.Name != "" {
		return s.opts.Name
	}
	return "hivemind TLS storage"
}

// Store implements certmagic.Storage.
func (s *Storage) Store(ctx context.Context, key string, value []byte) error {
	return s.b.Store(ctx, key, value)
}

// Load implements certmagic.Storage. A missing key matches fs.ErrNotExist.
func (s *Storage) Load(ctx context.Context, key string) ([]byte, error) { return s.b.Load(ctx, key) }

// Delete implements certmagic.Storage: the key and every key under it. A
// missing key is not an error.
func (s *Storage) Delete(ctx context.Context, key string) error { return s.b.Delete(ctx, key) }

// Exists implements certmagic.Storage. A backend that fails, as a worker's
// does while it cannot reach the server, counts as the key existing:
// certmagic's interface has no error here, and an answer of "missing" sends
// it down the wrong path. Its renewal check, finding a certificate's files
// missing, logs that they are not found "for any configured issuer" and
// obtains a new certificate instead of renewing; that asks the CA for a
// certificate the storage holds whenever the server answers again before
// the order, which counts against the CA's limits on duplicate
// certificates (sixth soak). With "exists", certmagic renews: its storage
// check fails with the backend's own error and the renewal is tried again
// at a later check, or, once the server answers, it loads the stored
// certificate and renews it only if it is still due. An obtain finds the
// files there and orders nothing; its certificate fails to load, and is
// logged, until the server answers.
func (s *Storage) Exists(ctx context.Context, key string) bool {
	ok, err := s.b.Exists(ctx, key)
	if err != nil {
		s.log.Debug("checking a key in the shared TLS storage failed; taking it to exist", "key", key, "err", err)
		return true
	}
	return ok
}

// List implements certmagic.Storage.
func (s *Storage) List(ctx context.Context, path string, recursive bool) ([]string, error) {
	return s.b.List(ctx, path, recursive)
}

// Stat implements certmagic.Storage.
func (s *Storage) Stat(ctx context.Context, key string) (certmagic.KeyInfo, error) {
	i, err := s.b.Stat(ctx, key)
	if err != nil {
		return certmagic.KeyInfo{}, err
	}
	return certmagic.KeyInfo{Key: i.Key, Modified: i.Modified, Size: i.Size, IsTerminal: i.Terminal}, nil
}

// Lock implements certmagic.Locker. It blocks until it takes the lock, the
// lock goes stale and it takes it then, or ctx ends.
func (s *Storage) Lock(ctx context.Context, name string) error {
	for {
		ok, err := s.TryLock(ctx, name)
		if err != nil || ok {
			return err
		}
		t := time.NewTimer(s.opts.LockPoll)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// TryLock implements certmagic.TryLocker: one attempt. A lock this Storage
// already holds is not taken again.
func (s *Storage) TryLock(ctx context.Context, name string) (bool, error) {
	s.mu.Lock()
	if _, mine := s.held[name]; mine {
		s.mu.Unlock()
		return false, nil
	}
	s.seq++
	owner := s.id + "-" + strconv.FormatUint(s.seq, 10)
	s.mu.Unlock()

	ok, err := s.b.TryLock(ctx, name, owner, s.opts.LockTTL)
	if err != nil || !ok {
		return false, err
	}
	h := &hold{owner: owner, stop: make(chan struct{}), done: make(chan struct{})}
	s.mu.Lock()
	s.held[name] = h
	s.mu.Unlock()
	go s.refresh(name, h)
	return true, nil
}

// refresh keeps a held lock from expiring until Unlock stops it. A renewal
// that the server refuses means the lock was lost: it expired, because the
// renewals did not reach the server for a lock lifetime, and someone else
// may hold it now. That is logged, and the refresh stops; the work under
// the lock carries on, which certmagic allows for ("code relying upon
// Locker must be tolerant of some mis-synchronizations").
func (s *Storage) refresh(name string, h *hold) {
	defer close(h.done)
	t := time.NewTicker(s.opts.LockRefresh)
	defer t.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-t.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), s.opts.LockRefresh)
		err := s.b.RenewLock(ctx, name, h.owner, s.opts.LockTTL)
		cancel()
		if err != nil {
			s.log.Warn("renewing a TLS storage lock", "lock", name, "err", err)
			if errors.Is(err, store.ErrNotFound) {
				return
			}
		}
	}
}

// Unlock implements certmagic.Locker.
func (s *Storage) Unlock(ctx context.Context, name string) error {
	s.mu.Lock()
	h, ok := s.held[name]
	delete(s.held, name)
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("TLS storage lock %q is not held here", name)
	}
	close(h.stop)
	<-h.done
	return s.b.Unlock(ctx, name, h.owner)
}

// abandon stops renewing name without unlocking it, as a holder that died
// would. Tests use it.
func (s *Storage) abandon(name string) {
	s.mu.Lock()
	h, ok := s.held[name]
	delete(s.held, name)
	s.mu.Unlock()
	if ok {
		close(h.stop)
		<-h.done
	}
}
