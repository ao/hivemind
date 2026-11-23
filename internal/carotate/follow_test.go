package carotate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/store"
)

// A CA rotation with several servers (docs/ha.md, the CA rotation with
// several servers): the leader runs the steps, and every server follows
// the store's record, which carries the new CA, sealed.

// sharedLog is a Raft log in memory for a few stores: the one that leads
// writes, and every entry is applied to every store, in order.
type sharedLog struct {
	mu     sync.Mutex
	stores []*store.Store
	leader *store.Store
	idx    uint64
}

// member is one store's view of the log.
type member struct {
	l  *sharedLog
	st *store.Store
}

func (m member) CanWrite() (uint64, error) {
	m.l.mu.Lock()
	defer m.l.mu.Unlock()
	if m.l.leader != m.st {
		return 0, fmt.Errorf("%w: test", store.ErrNotLeader)
	}
	return 1, nil
}

func (m member) Replicate(ctx context.Context, _ uint64, data []byte) error {
	m.l.mu.Lock()
	defer m.l.mu.Unlock()
	if m.l.leader != m.st {
		return fmt.Errorf("%w: test", store.ErrNotLeader)
	}
	m.l.idx++
	for _, s := range m.l.stores {
		if err := s.ApplyEntry(ctx, m.l.idx, data); err != nil {
			return err
		}
	}
	return nil
}

// srv is one server of a test cluster: its data directory, store and
// Manager.
type srv struct {
	id  string
	dir string
	st  *store.Store
	m   *Manager
}

// cluster is a few servers with the same CA and one log, the first
// leading.
type cluster struct {
	t       *testing.T
	log     *sharedLog
	servers []*srv
	logs    *logBuf
}

func newCluster(t *testing.T, n int) *cluster {
	t.Helper()
	ctx := context.Background()
	c := &cluster{t: t, log: &sharedLog{}, logs: &logBuf{}}
	var ca *pki.CA
	for i := 0; i < n; i++ {
		s := &srv{id: fmt.Sprintf("node-s%d", i), dir: t.TempDir()}
		if i == 0 {
			var err error
			if ca, err = pki.CreateCA(s.dir); err != nil {
				t.Fatal(err)
			}
		} else {
			key, err := ca.KeyPEM()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pki.WriteCAFiles(s.dir, ca.CertPEM, key); err != nil {
				t.Fatal(err)
			}
		}
		st, err := store.Open(ctx, filepath.Join(s.dir, "hivemind.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		s.st = st
		c.log.stores = append(c.log.stores, st)
		c.servers = append(c.servers, s)
	}
	c.log.leader = c.servers[0].st
	for _, s := range c.servers {
		s.st.SetReplicator(member{l: c.log, st: s.st})
	}
	lead := c.servers[0].st
	if err := lead.InitCluster(ctx, store.Cluster{AdminTokenHash: sha256.Sum256([]byte("t")), NodeID: c.servers[0].id}); err != nil {
		t.Fatal(err)
	}
	ids := []string{"node-w"}
	for _, s := range c.servers {
		ids = append(ids, s.id)
	}
	for _, id := range ids {
		if err := lead.UpsertNode(ctx, store.Node{ID: id, Name: "host-" + id, CPU: 1, Memory: 1 << 30}); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range c.servers {
		s.m = c.open(s)
	}
	return c
}

func (c *cluster) open(s *srv) *Manager {
	c.t.Helper()
	m, err := Open(context.Background(), Options{Dir: s.dir, Store: s.st, LocalNode: s.id, Logger: slog.New(slog.NewTextHandler(c.logs, nil))})
	if err != nil {
		c.t.Fatalf("%s: Open: %v", s.id, err)
	}
	return m
}

// follow has s's Manager follow the record once, as Follow does after a
// change.
func (c *cluster) follow(s *srv) error {
	s.m.mu.Lock()
	defer s.m.mu.Unlock()
	return s.m.followLocked(context.Background())
}

func (c *cluster) followAll() {
	c.t.Helper()
	for _, s := range c.servers {
		if err := c.follow(s); err != nil {
			c.t.Fatalf("%s follows: %v", s.id, err)
		}
	}
}

// files returns the fingerprints of s's ca.crt and ca-next.crt, zero for a
// file it does not have.
func files(t *testing.T, s *srv) (cur, next [32]byte) {
	t.Helper()
	if ca, err := pki.LoadCA(s.dir); err == nil {
		cur = ca.Fingerprint()
	}
	if ca, err := pki.LoadNextCA(s.dir); err == nil {
		next = ca.Fingerprint()
	}
	return cur, next
}

// TestSealedKey: the new CA's key opens with the current CA's key and the
// same fingerprints only.
func TestSealedKey(t *testing.T) {
	cur, err := pki.CreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	other, err := pki.CreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	next, err := pki.NewNextCA()
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := next.KeyPEM()
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := sealNextKey(cur, next.Fingerprint(), keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, keyPEM[30:60]) {
		t.Fatal("the sealed key holds the key's bytes")
	}
	got, err := openNextKey(cur, next.Fingerprint(), sealed)
	if err != nil || !bytes.Equal(got, keyPEM) {
		t.Fatalf("open: %v", err)
	}
	if _, err := openNextKey(other, next.Fingerprint(), sealed); !errors.Is(err, errSealed) {
		t.Fatalf("opened with another CA's key: %v", err)
	}
	if _, err := openNextKey(cur, [32]byte{1}, sealed); !errors.Is(err, errSealed) {
		t.Fatalf("opened for another new CA: %v", err)
	}
	bad := append([]byte{}, sealed...)
	bad[len(bad)-1] ^= 1
	if _, err := openNextKey(cur, next.Fingerprint(), bad); !errors.Is(err, errSealed) {
		t.Fatalf("opened a damaged key: %v", err)
	}
	if _, err := openNextKey(cur, next.Fingerprint(), sealed[:5]); !errors.Is(err, errSealed) {
		t.Fatalf("opened a short key: %v", err)
	}
	// Two seals of one key differ: the nonce is new each time.
	again, _ := sealNextKey(cur, next.Fingerprint(), keyPEM)
	if bytes.Equal(again, sealed) {
		t.Fatal("two seals are the same")
	}
}

// TestRotationOnEveryServer: the leader starts, renews and finishes; the
// follower writes its copy of the new CA from the record, accepts both,
// moves its listener, and does its own file step at the end; both end on
// the new CA, recorded as the cluster's.
func TestRotationOnEveryServer(t *testing.T) {
	c := newCluster(t, 2)
	ctx := context.Background()
	l, f := c.servers[0], c.servers[1]
	old := l.m.Current().Fingerprint()
	if fp, known, _ := f.st.ClusterCA(ctx); !known || fp != old {
		t.Fatalf("the cluster CA recorded at the leader's Open: %v %x", known, fp[:4])
	}
	if _, err := l.m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	next := l.m.Next().Fingerprint()
	rc, err := f.st.GetCARotationCA(ctx)
	if err != nil || rc.Empty() {
		t.Fatalf("the record holds no new CA: %+v %v", rc, err)
	}
	if bytes.Contains(rc.SealedKey, []byte("PRIVATE KEY")) {
		t.Fatal("the record holds the new CA's key unsealed")
	}
	if _, n := files(t, f); n != ([32]byte{}) {
		t.Fatal("the follower has the new CA's files before it followed")
	}
	if err := c.follow(f); err != nil {
		t.Fatal(err)
	}
	if cur, n := files(t, f); cur != old || n != next {
		t.Fatalf("the follower's files after start: %x %x", cur[:4], n[:4])
	}
	if !f.m.Accepts(old) || !f.m.Accepts(next) || !f.m.Rotating() || f.m.Listener().Fingerprint() != old {
		t.Fatal("the follower does not accept both CAs, or its listener moved, in phase trust")
	}
	info, err := os.Stat(filepath.Join(f.dir, pki.NextCAKeyFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the follower's ca-next.key: %v %v", info, err)
	}

	for _, id := range []string{f.id, "node-w"} {
		if err := l.m.Observe(ctx, id, [][32]byte{old, next}, true, old); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := l.m.Renew(ctx, false); err != nil {
		t.Fatal(err)
	}
	if err := l.m.moveListener(ctx); err != nil {
		t.Fatal(err)
	}
	if l.m.Listener().Fingerprint() != next {
		t.Fatal("the leader's listener did not move")
	}
	c.followAll()
	if f.m.Listener().Fingerprint() != next || !f.m.Accepts(old) {
		t.Fatal("the follower's listener did not follow, or it stopped accepting the old CA early")
	}
	for _, id := range []string{f.id, "node-w"} {
		if err := l.m.Observe(ctx, id, [][32]byte{old, next}, true, next); err != nil {
			t.Fatal(err)
		}
	}
	st, err := l.m.Finish(ctx, false)
	if err != nil || st.Ended == nil {
		t.Fatalf("finish: %+v %v", st, err)
	}
	if err := c.follow(f); err != nil {
		t.Fatal(err)
	}
	for _, s := range c.servers {
		if cur, n := files(t, s); cur != next || n != ([32]byte{}) {
			t.Fatalf("%s's files after finish: %x %x", s.id, cur[:4], n[:4])
		}
		if s.m.Rotating() || s.m.Current().Fingerprint() != next || s.m.Accepts(old) {
			t.Fatalf("%s's state after finish", s.id)
		}
		if fp, known, _ := s.st.ClusterCA(ctx); !known || fp != next {
			t.Fatalf("%s's store does not record the new CA as the cluster's", s.id)
		}
	}
	// A restart of the follower finds what it left.
	f.m = c.open(f)
	if f.m.Current().Fingerprint() != next || f.m.Rotating() {
		t.Fatal("the follower after a restart")
	}
}

// TestFollowerThatMissedSteps: a follower that applied the start and then
// both entries of the end before it looked again renames its new CA's
// files by the recorded fingerprint; one that missed the whole rotation
// holds another CA than the cluster's, and says so; one that missed only
// an abort in phase trust removes its new CA's files.
func TestFollowerThatMissedSteps(t *testing.T) {
	c := newCluster(t, 3)
	ctx := context.Background()
	l, f, away := c.servers[0], c.servers[1], c.servers[2]
	if _, err := l.m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	next := l.m.Next().Fingerprint()
	if err := c.follow(f); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{f.id, away.id, "node-w"} {
		if err := l.m.Observe(ctx, id, [][32]byte{l.m.Current().Fingerprint(), next}, true, next); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := l.m.Renew(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := l.m.Finish(ctx, false); err != nil {
		t.Fatal(err)
	}
	if err := c.follow(f); err != nil {
		t.Fatal(err)
	}
	if cur, n := files(t, f); cur != next || n != ([32]byte{}) {
		t.Fatalf("the follower that missed the end: %x %x", cur[:4], n[:4])
	}
	if !strings.Contains(c.logs.String(), "ended while this server was behind") {
		t.Fatalf("no line for the end it caught up with:\n%s", c.logs.String())
	}
	err := c.follow(away)
	if err == nil || !strings.Contains(err.Error(), "holds another CA than the cluster's") {
		t.Fatalf("a server that missed the whole rotation: %v", err)
	}

	// An abort in phase trust that a follower missed.
	c2 := newCluster(t, 2)
	l2, f2 := c2.servers[0], c2.servers[1]
	old2 := l2.m.Current().Fingerprint()
	if _, err := l2.m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c2.follow(f2); err != nil {
		t.Fatal(err)
	}
	if _, err := l2.m.Abort(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c2.follow(f2); err != nil {
		t.Fatal(err)
	}
	if cur, n := files(t, f2); cur != old2 || n != ([32]byte{}) {
		t.Fatalf("the follower after a missed abort: %x %x", cur[:4], n[:4])
	}
}

// TestNextLeaderFinishesTheEnd: a leader that stopped after it recorded the
// end and before it deleted the rotation leaves the end to the next
// leader, which does its file step and records it when it starts to lead.
func TestNextLeaderFinishesTheEnd(t *testing.T) {
	c := newCluster(t, 2)
	ctx := context.Background()
	l, f := c.servers[0], c.servers[1]
	if _, err := l.m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	next := l.m.Next().Fingerprint()
	c.followAll()
	rot, err := l.st.GetCARotation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rot.Phase, rot.Listener = store.CAPhaseEnding, store.CANext
	if err := l.st.SetCARotation(ctx, rot, false); err != nil {
		t.Fatal(err)
	}
	// The leader stops here; the follower leads.
	c.log.mu.Lock()
	c.log.leader = f.st
	c.log.mu.Unlock()
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); f.m.Run(runCtx) }()
	defer func() { cancel(); <-done }()
	for i := 0; i < 200; i++ {
		if _, err := f.st.GetCARotation(ctx); errors.Is(err, store.ErrNotFound) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := f.st.GetCARotation(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the next leader did not end the rotation: %v", err)
	}
	if cur, n := files(t, f); cur != next || n != ([32]byte{}) {
		t.Fatalf("the next leader's files: %x %x", cur[:4], n[:4])
	}
	if fp, known, _ := l.st.ClusterCA(ctx); !known || fp != next {
		t.Fatal("the end the next leader wrote did not reach the old leader's store")
	}
	// The old leader, back, follows.
	l.m = c.open(l)
	if cur, n := files(t, l); cur != next || n != ([32]byte{}) || l.m.Current().Fingerprint() != next {
		t.Fatalf("the old leader after it came back: %x %x", cur[:4], n[:4])
	}
}

// TestStartRefusals: a cluster of several below schema version 19 cannot
// rotate, nor one with a server joining.
func TestStartRefusals(t *testing.T) {
	c := newCluster(t, 1)
	ctx := context.Background()
	l := c.servers[0]
	l.m.SetStartCheck(func(context.Context) error { return fmt.Errorf("%w: node-x is a non-voter", ErrServerJoining) })
	if _, err := l.m.Start(ctx); !errors.Is(err, ErrServerJoining) || !IsRefusal(err) {
		t.Fatalf("start with a server joining: %v", err)
	}

	dir := t.TempDir()
	if _, err := pki.CreateCA(dir); err != nil {
		t.Fatal(err)
	}
	// A store of a cluster of several that is still at 2.3.0's version.
	st, err := store.Open(ctx, filepath.Join(dir, "hivemind.db"), store.WithHeldSchema())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for i, id := range []string{"node-a", "node-b"} {
		addr := fmt.Sprintf("10.0.0.%d", 5+i)
		if err := st.PutServer(ctx, store.Server{ID: id, Advertise: addr, NodePort: 4484, RaftAddr: addr + ":4486"}); err != nil {
			t.Fatal(err)
		}
	}
	m, err := Open(ctx, Options{Dir: dir, Store: st, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(ctx); !errors.Is(err, ErrSeveralServers) || !strings.Contains(err.Error(), "schema version 19") {
		t.Fatalf("start with several servers at schema version %d: %v", st.Schema(), err)
	}
}

// TestNextFilesHalfWritten: a server that stopped while it wrote its copy of
// the new CA, with one file of the pair left or a key cut short, writes the
// pair again from the rotation's record at its next start.
func TestNextFilesHalfWritten(t *testing.T) {
	for _, tc := range []struct {
		name string
		cut  func(t *testing.T, dir string)
	}{
		{"the key alone", func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, pki.NextCACertFile)); err != nil {
				t.Fatal(err)
			}
		}},
		{"the certificate alone", func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, pki.NextCAKeyFile)); err != nil {
				t.Fatal(err)
			}
		}},
		{"a key cut short", func(t *testing.T, dir string) {
			if err := os.Truncate(filepath.Join(dir, pki.NextCAKeyFile), 0); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCluster(t, 2)
			ctx := context.Background()
			l, f := c.servers[0], c.servers[1]
			if _, err := l.m.Start(ctx); err != nil {
				t.Fatal(err)
			}
			next := l.m.Next().Fingerprint()
			if err := c.follow(f); err != nil {
				t.Fatal(err)
			}
			tc.cut(t, f.dir)
			m, err := Open(ctx, Options{Dir: f.dir, Store: f.st, LocalNode: f.id, Logger: slog.New(slog.NewTextHandler(c.logs, nil))})
			if err != nil {
				t.Fatalf("the server's next start: %v", err)
			}
			if _, n := files(t, f); n != next || m.Next() == nil || m.Next().Fingerprint() != next {
				t.Fatalf("ca-next after the start: %x", n[:4])
			}
		})
	}
}

// TestEndBetweenRenames: a server that stopped between the two renames of a
// rotation's end, with the new key in ca.key and the new certificate still
// in ca-next.crt, does the second rename when it follows a store whose
// rotation has ended.
func TestEndBetweenRenames(t *testing.T) {
	c := newCluster(t, 2)
	ctx := context.Background()
	l, f := c.servers[0], c.servers[1]
	if _, err := l.m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	next := l.m.Next().Fingerprint()
	if err := c.follow(f); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{f.id, "node-w"} {
		if err := l.m.Observe(ctx, id, [][32]byte{l.m.Current().Fingerprint(), next}, true, next); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := l.m.Renew(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err := l.m.Finish(ctx, false); err != nil {
		t.Fatal(err)
	}
	// The follower's first rename, and no second.
	if err := os.Rename(filepath.Join(f.dir, pki.NextCAKeyFile), filepath.Join(f.dir, pki.CAKeyFile)); err != nil {
		t.Fatal(err)
	}
	if err := c.follow(f); err != nil {
		t.Fatalf("follows after a stop between the renames: %v", err)
	}
	if cur, n := files(t, f); cur != next || n != ([32]byte{}) {
		t.Fatalf("the files after: %x %x", cur[:4], n[:4])
	}
}

// TestTrustDoubt: a node that trusts the new CA and sends a request that
// does not list it, as one it sent before it took the CA and that came
// late, still trusts it; a second such request trustDoubt or more later,
// with none that lists it between, counts.
func TestTrustDoubt(t *testing.T) {
	c := newCluster(t, 1)
	ctx := context.Background()
	l := c.servers[0]
	if _, err := l.m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	clk := time.Now()
	l.m.now = func() time.Time { return clk }
	cur, next := l.m.Current().Fingerprint(), l.m.Next().Fingerprint()
	trusts := func() bool {
		l.m.nmu.Lock()
		defer l.m.nmu.Unlock()
		return l.m.nodes["node-w"].Trusts
	}
	stored := func() bool {
		marks, err := l.st.CARotationNodes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return marks["node-w"].Trusts
	}
	observe := func(fps ...[32]byte) {
		t.Helper()
		if err := l.m.Observe(ctx, "node-w", fps, true, cur); err != nil {
			t.Fatal(err)
		}
	}
	observe(cur, next)
	if !trusts() || !stored() {
		t.Fatal("the node does not trust the new CA")
	}
	observe(cur)
	if !trusts() || !stored() {
		t.Fatal("one late request without the new CA counted")
	}
	clk = clk.Add(trustDoubt + time.Second)
	observe(cur, next)
	observe(cur)
	clk = clk.Add(time.Second)
	observe(cur)
	if !trusts() {
		t.Fatal("a request that listed the CA did not clear the doubt, or a second without it within trustDoubt counted")
	}
	clk = clk.Add(trustDoubt)
	observe(cur)
	if trusts() || stored() {
		t.Fatal("a second request without the new CA, trustDoubt after the first, did not count")
	}
	if !strings.Contains(c.logs.String(), "sent a request that does not list it") {
		t.Fatalf("no line for it:\n%s", c.logs.String())
	}
}
