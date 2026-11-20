package main

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/store"
	"github.com/ao/hivemind/internal/tlsstore"
)

// unreachableTLS is the leader's TLS storage while no server leads: every
// call fails, and is counted.
type unreachableTLS struct {
	mu    sync.Mutex
	calls []string
}

var errNoLeaderTLS = errors.New("no leader: this server reaches 1 of the 3 servers, and a write needs 2")

func (u *unreachableTLS) note(op, key string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls = append(u.calls, op+" "+key)
	return errNoLeaderTLS
}

func (u *unreachableTLS) Load(_ context.Context, key string) ([]byte, error) {
	return nil, u.note("load", key)
}
func (u *unreachableTLS) Store(_ context.Context, key string, _ []byte) error {
	return u.note("store", key)
}
func (u *unreachableTLS) Delete(_ context.Context, key string) error { return u.note("delete", key) }
func (u *unreachableTLS) Exists(_ context.Context, key string) (bool, error) {
	return false, u.note("exists", key)
}
func (u *unreachableTLS) List(_ context.Context, prefix string, _ bool) ([]string, error) {
	return nil, u.note("list", prefix)
}
func (u *unreachableTLS) Stat(_ context.Context, key string) (store.TLSKeyInfo, error) {
	return store.TLSKeyInfo{}, u.note("stat", key)
}
func (u *unreachableTLS) TryLock(_ context.Context, name, _ string, _ time.Duration) (bool, error) {
	return false, u.note("trylock", name)
}
func (u *unreachableTLS) RenewLock(_ context.Context, name, _ string, _ time.Duration) error {
	return u.note("renewlock", name)
}
func (u *unreachableTLS) Unlock(_ context.Context, name, _ string) error {
	return u.note("unlock", name)
}

func (u *unreachableTLS) taken() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := u.calls
	u.calls = nil
	return out
}

// TestFollowerRouterReadsItsOwnCopy: a follower's router reads the
// certificates its own copy of the store has, with no leader to ask (the
// eighth soak's third finding: a server's router that started while no
// server led loaded nothing); asks the leader for a key its copy lacks,
// and for a challenge token; and writes and locks through the leader only.
// While the server writes the store itself, everything is the store.
func TestFollowerRouterReadsItsOwnCopy(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "hivemind.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	local := tlsstore.StoreBackend{St: st, NodeID: "node-f"}
	const cert = "certificates/acme/www.example.com/www.example.com.crt"
	const challenge = "acme/acme-v02.api.letsencrypt.org-directory/challenge_tokens/www.example.com.json"
	for _, k := range []string{cert, challenge} {
		if err := st.TLSStore(ctx, k, []byte("v "+k)); err != nil {
			t.Fatal(err)
		}
	}
	leader := &unreachableTLS{}
	follower := tlsBackend{remote: func() tlsstore.Backend { return leader }, local: local}

	if v, err := follower.Load(ctx, cert); err != nil || string(v) != "v "+cert {
		t.Fatalf("a follower's load of a certificate its copy has, with no leader: %q %v", v, err)
	}
	if ok, err := follower.Exists(ctx, cert); err != nil || !ok {
		t.Fatalf("exists: %v %v", ok, err)
	}
	if i, err := follower.Stat(ctx, cert); err != nil || i.Size == 0 {
		t.Fatalf("stat: %+v %v", i, err)
	}
	if keys, err := follower.List(ctx, "certificates", true); err != nil || !slices.Contains(keys, cert) {
		t.Fatalf("list with no leader: %v %v", keys, err)
	}
	if got := leader.taken(); len(got) != 1 || got[0] != "list certificates" {
		t.Fatalf("the leader was asked %v; want only the list, which it answers first", got)
	}

	if _, err := follower.Load(ctx, "certificates/acme/new.example.com/new.example.com.crt"); !errors.Is(err, errNoLeaderTLS) {
		t.Fatalf("a key the copy lacks: %v, want the leader's answer", err)
	}
	if _, err := follower.Load(ctx, challenge); !errors.Is(err, errNoLeaderTLS) {
		t.Fatalf("a challenge token: %v, want the leader's answer", err)
	}
	if _, err := follower.Exists(ctx, "certificates/acme/new.example.com"); !errors.Is(err, errNoLeaderTLS) {
		t.Fatalf("exists of a key the copy lacks: %v", err)
	}
	if err := follower.Store(ctx, cert, []byte("x")); !errors.Is(err, errNoLeaderTLS) {
		t.Fatalf("store: %v", err)
	}
	if _, err := follower.TryLock(ctx, "issue_cert_www.example.com", "o", time.Minute); !errors.Is(err, errNoLeaderTLS) {
		t.Fatalf("trylock: %v", err)
	}
	if err := follower.Delete(ctx, cert); !errors.Is(err, errNoLeaderTLS) {
		t.Fatalf("delete: %v", err)
	}
	if got := len(leader.taken()); got != 6 {
		t.Fatalf("the leader was asked %d times, want 6", got)
	}
	if v, err := st.TLSLoad(ctx, cert); err != nil || string(v) != "v "+cert {
		t.Fatalf("a follower's write reached its own copy: %q %v", v, err)
	}

	own := tlsBackend{remote: func() tlsstore.Backend { return nil }, local: local}
	if v, err := own.Load(ctx, challenge); err != nil || string(v) != "v "+challenge {
		t.Fatalf("the leader's own load: %q %v", v, err)
	}
	if _, err := own.Load(ctx, "certificates/x/y.crt"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the leader's own load of a missing key: %v", err)
	}
	if err := own.Store(ctx, "certificates/x/y.crt", []byte("y")); err != nil {
		t.Fatal(err)
	}
	if len(leader.taken()) != 0 {
		t.Fatal("a server that writes the store itself asked the leader")
	}
}
