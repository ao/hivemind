package tlsstore

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"

	"github.com/ao/hivemind/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hivemind.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func newStorage(t *testing.T, st *store.Store, node string, opts Options) *Storage {
	t.Helper()
	opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := New(StoreBackend{St: st, NodeID: node}, opts)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// Every certmagic.Storage method on the store, with certmagic's own keys.
func TestStorageMethods(t *testing.T) {
	var s certmagic.Storage = newStorage(t, newStore(t), "node-a", Options{})
	ctx := ctxT(t)
	keys := certmagic.KeyBuilder{}
	crt := keys.SiteCert("acme-v02.api.letsencrypt.org-directory", "www.example.com")
	priv := keys.SitePrivateKey("acme-v02.api.letsencrypt.org-directory", "www.example.com")
	meta := keys.SiteMeta("acme-v02.api.letsencrypt.org-directory", "www.example.com")
	site := keys.CertsSitePrefix("acme-v02.api.letsencrypt.org-directory", "www.example.com")

	if _, err := s.Load(ctx, crt); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("load of a missing key: %v", err)
	}
	if s.Exists(ctx, crt) {
		t.Fatal("a missing key exists")
	}
	for _, k := range []string{crt, priv, meta} {
		if err := s.Store(ctx, k, []byte(k)); err != nil {
			t.Fatalf("store %s: %v", k, err)
		}
	}
	if v, err := s.Load(ctx, priv); err != nil || string(v) != priv {
		t.Fatalf("load: %q %v", v, err)
	}
	if !s.Exists(ctx, site) || !s.Exists(ctx, crt) {
		t.Fatal("exists of a directory or a value")
	}
	got, err := s.List(ctx, site, false)
	if want := []string{crt, meta, priv}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("list: %v, %v; want %v", got, err, want)
	}
	info, err := s.Stat(ctx, crt)
	if err != nil || !info.IsTerminal || info.Size != int64(len(crt)) || info.Key != crt {
		t.Fatalf("stat: %+v %v", info, err)
	}
	if info, err := s.Stat(ctx, site); err != nil || info.IsTerminal {
		t.Fatalf("stat of a directory: %+v %v", info, err)
	}
	if err := s.Delete(ctx, site); err != nil {
		t.Fatal(err)
	}
	if s.Exists(ctx, crt) || s.Exists(ctx, site) {
		t.Fatal("delete of a directory left keys")
	}
	if err := s.Delete(ctx, site); err != nil {
		t.Fatalf("delete of a missing key: %v", err)
	}
	if err := s.Store(ctx, "hivemind.db", nil); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("a key outside certmagic's namespace: %v", err)
	}
}

// Lock blocks while another holder has the lock, and returns once it is
// released; TryLock does not wait.
func TestLockContention(t *testing.T) {
	st := newStore(t)
	opts := Options{LockPoll: 10 * time.Millisecond}
	a, b := newStorage(t, st, "node-a", opts), newStorage(t, st, "node-b", opts)
	ctx := ctxT(t)
	const name = "issue_cert_www.example.com"

	if err := a.Lock(ctx, name); err != nil {
		t.Fatal(err)
	}
	if ok, err := b.TryLock(ctx, name); ok || err != nil {
		t.Fatalf("B took a held lock: %v %v", ok, err)
	}
	if ok, _ := a.TryLock(ctx, name); ok {
		t.Fatal("A took its own lock twice")
	}
	var got atomic.Bool
	done := make(chan error, 1)
	go func() {
		err := b.Lock(ctx, name)
		got.Store(true)
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	if got.Load() {
		t.Fatal("B's Lock returned while A held the lock")
	}
	if err := a.Unlock(ctx, name); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("B's Lock did not return after A unlocked")
	}
	if err := a.Unlock(ctx, name); err == nil {
		t.Fatal("A unlocked a lock it no longer holds")
	}
	if err := b.Unlock(ctx, name); err != nil {
		t.Fatal(err)
	}

	// A Lock whose context ends returns the context's error.
	if err := a.Lock(ctx, name); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := b.Lock(short, name); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Lock with an ended context: %v", err)
	}
}

// A holder renews its lock, so it outlives its lifetime; a holder that stops
// (as one that died) loses it once it expires, and the next Lock takes it.
func TestLockExpiry(t *testing.T) {
	st := newStore(t)
	opts := Options{LockTTL: 300 * time.Millisecond, LockRefresh: 50 * time.Millisecond, LockPoll: 20 * time.Millisecond}
	a, b := newStorage(t, st, "node-a", opts), newStorage(t, st, "node-b", opts)
	ctx := ctxT(t)
	const name = "register_acme_account_ops@example.com"

	if err := a.Lock(ctx, name); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second) // more than three lifetimes, renewed all along
	if ok, _ := b.TryLock(ctx, name); ok {
		t.Fatal("B took a lock that A kept renewing")
	}
	a.abandon(name)
	start := time.Now()
	if err := b.Lock(ctx, name); err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Fatalf("B waited %s for a stale lock with a 300ms lifetime", waited)
	}
	if err := b.Unlock(ctx, name); err != nil {
		t.Fatal(err)
	}
}

func TestNewChecksTimings(t *testing.T) {
	if _, err := New(StoreBackend{}, Options{LockTTL: time.Second, LockRefresh: time.Second}); err == nil {
		t.Fatal("a refresh as long as the lifetime was accepted")
	}
}

// existsFails is a backend whose Exists fails, as a worker's node API calls
// do while the server cannot be reached.
type existsFails struct{ Backend }

func (existsFails) Exists(context.Context, string) (bool, error) {
	return false, errors.New(`calling the server: Post "https://10.0.0.1:4484/node/v1/tls/store": dial tcp 10.0.0.1:4484: i/o timeout`)
}

// A key whose check fails counts as existing, so certmagic takes the
// renewal path, not the obtain path that orders a certificate the storage
// may well hold (sixth soak). A key the backend answers for is as it says.
func TestExistsWhenTheBackendFails(t *testing.T) {
	st := newStore(t)
	ok := newStorage(t, st, "node-a", Options{})
	ctx := ctxT(t)
	const key = "certificates/acme/www.example.com/www.example.com.crt"
	if ok.Exists(ctx, key) {
		t.Fatal("a missing key exists")
	}
	if err := ok.Store(ctx, key, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if !ok.Exists(ctx, key) {
		t.Fatal("a stored key does not exist")
	}
	failing, err := New(existsFails{StoreBackend{St: st, NodeID: "node-a"}}, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{key, "certificates/acme/missing.example.com/missing.example.com.crt"} {
		if !failing.Exists(ctx, k) {
			t.Errorf("%s: a failed check answers that it does not exist", k)
		}
	}
}
