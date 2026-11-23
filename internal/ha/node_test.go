package ha

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/spec"
	"github.com/ao/hivemind/internal/store"
)

var testTiming = Timing{Heartbeat: 300 * time.Millisecond, Election: 300 * time.Millisecond, LeaderLease: 150 * time.Millisecond,
	Commit: 20 * time.Millisecond}

// testServer is one server of a test cluster on loopback.
type testServer struct {
	id   string
	dir  string
	port int
	st   *store.Store
	node *Node
	cert *pki.CertHolder
	// build, if set, is what the server answers the build request with.
	build func() BuildInfo
	// logs, if set, takes the server's log at info.
	logs *lockedBuffer
}

type testCluster struct {
	t       *testing.T
	ca      *pki.CA
	trust   *pki.TrustHolder
	servers []*testServer
	leading sync.Map // id -> bool, from OnLeader
}

func newTestCluster(t *testing.T, n int, opts ...store.Option) *testCluster {
	t.Helper()
	ca, err := pki.CreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := &testCluster{t: t, ca: ca, trust: pki.NewTrustHolder(pki.MustTrust(ca.Cert))}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("node-s%d", i)
		dir := filepath.Join(t.TempDir(), id)
		st, err := store.Open(context.Background(), filepath.Join(dir, "hivemind.db"), append([]store.Option{store.WithMinReady(0)}, opts...)...)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		c.servers = append(c.servers, &testServer{id: id, dir: dir, st: st, cert: pki.NewCertHolder(c.issue(id, true))})
	}
	return c
}

// issue makes a certificate from the cluster CA for id, a server's if
// server is set.
func (c *testCluster) issue(id string, server bool) tls.Certificate {
	c.t.Helper()
	key, err := pki.NewNodeKey()
	if err != nil {
		c.t.Fatal(err)
	}
	csr, err := pki.NewCSR(key)
	if err != nil {
		c.t.Fatal(err)
	}
	der, err := c.ca.IssueNodeCert(csr, pki.NodeCertOptions{NodeID: id, Advertise: "127.0.0.1", Server: server})
	if err != nil {
		c.t.Fatal(err)
	}
	cert, err := pki.NewTLSCertificate(der, key, c.ca)
	if err != nil {
		c.t.Fatal(err)
	}
	return cert
}

func (c *testCluster) config(s *testServer, seeds ...string) Config {
	return Config{DataDir: s.dir, Store: s.st, ID: s.id, Advertise: "127.0.0.1", Bind: "127.0.0.1", Port: s.port,
		Cert: s.cert, Trust: c.trust, Seeds: seeds, Timing: testTiming,
		Logger:   s.logger(),
		OnLeader: func(ctx context.Context) { c.leading.Store(s.id, true); <-ctx.Done(); c.leading.Store(s.id, false) },
		Fatal:    func(err error) { c.t.Errorf("%s: fatal: %v", s.id, err) }, Build: s.build}
}

// start forms the cluster: server 0 turns Raft on in its cluster of one,
// and the others join as non-voters, and are voters once they catch up.
func (c *testCluster) start() {
	c.t.Helper()
	s0 := c.servers[0]
	ctx := context.Background()
	if err := s0.st.InitCluster(ctx, store.Cluster{AdminTokenHash: sha256.Sum256([]byte("t")), NodeID: s0.id}); err != nil {
		c.t.Fatal(err)
	}
	if err := s0.st.UpsertNode(ctx, store.Node{ID: s0.id, Name: "s0", CPU: 4, Memory: 8 << 30}); err != nil {
		c.t.Fatal(err)
	}
	if _, err := s0.st.PutApp(ctx, spec.App{Name: "before", Image: "busybox", Replicas: 1}); err != nil {
		c.t.Fatal(err)
	}
	n0, err := Enable(ctx, c.config(s0))
	if err != nil {
		c.t.Fatal(err)
	}
	s0.node, s0.port = n0, portOf(c.t, n0.Addr())
	c.t.Cleanup(func() { c.closeAll() })
	for _, s := range c.servers[1:] {
		n, err := Open(c.config(s, s0.id))
		if err != nil {
			c.t.Fatal(err)
		}
		s.node, s.port = n, portOf(c.t, n.Addr())
		if err := n0.AddNonvoter(s.id, n.Addr()); err != nil {
			c.t.Fatal(err)
		}
	}
	c.waitFor(30*time.Second, "every server a voter", func() bool {
		for _, s := range c.servers {
			if s.node == nil || !s.node.IsVoter() || !n0.IsVoterID(s.id) {
				return false
			}
		}
		return true
	})
}

func (c *testCluster) closeAll() {
	for _, s := range c.servers {
		if s.node != nil {
			s.node.Close()
			s.node = nil
		}
	}
}

func portOf(t *testing.T, addr string) int {
	t.Helper()
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	fmt.Sscanf(p, "%d", &n)
	return n
}

func (c *testCluster) waitFor(d time.Duration, what string, ok func() bool) {
	c.t.Helper()
	end := time.Now().Add(d)
	for !ok() {
		if time.Now().After(end) {
			c.t.Fatalf("timed out after %s waiting for %s", d, what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// leader waits for a server among the live ones that can write.
func (c *testCluster) leader() *testServer {
	c.t.Helper()
	var l *testServer
	c.waitFor(20*time.Second, "a leader", func() bool {
		for _, s := range c.servers {
			if s.node != nil && s.node.IsLeader() {
				l = s
				return true
			}
		}
		return false
	})
	return l
}

func appNames(t *testing.T, st *store.Store) string {
	t.Helper()
	apps, err := st.ListApps(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, a := range apps {
		names = append(names, fmt.Sprintf("%s:%d", a.Spec.Name, a.Spec.Replicas))
	}
	return strings.Join(names, ",")
}

// converged waits until every live server has the same apps as want.
func (c *testCluster) converged(want string) {
	c.t.Helper()
	c.waitFor(20*time.Second, "the servers to agree on "+want, func() bool {
		for _, s := range c.servers {
			if s.node != nil && appNames(c.t, s.st) != want {
				return false
			}
		}
		return true
	})
}

// TestClusterFormsAndReplicates: a cluster of one turns Raft on with its
// store as the first snapshot; two servers join, get the store, and become
// voters; writes on the leader reach both; a follower refuses writes.
func TestClusterFormsAndReplicates(t *testing.T) {
	c := newTestCluster(t, 3)
	c.start()
	l := c.leader()
	if l != c.servers[0] {
		t.Fatalf("the leader is %s, want the server that bootstrapped", l.id)
	}
	c.converged("before:1")
	if _, err := l.st.PutApp(context.Background(), spec.App{Name: "web", Image: "nginx", Replicas: 3}); err != nil {
		t.Fatal(err)
	}
	c.converged("before:1,web:3")
	f := c.servers[1]
	_, err := f.st.PutApp(context.Background(), spec.App{Name: "x", Image: "nginx", Replicas: 1})
	var nl *NotLeaderError
	if !errors.As(err, &nl) || !errors.Is(err, store.ErrNotLeader) || nl.Leader != l.id {
		t.Fatalf("a follower's write: %v", err)
	}
	if cl, err := f.st.GetCluster(context.Background()); err != nil || cl.NodeID != c.servers[0].id {
		t.Fatalf("a joined server's cluster record: %+v %v", cl, err)
	}
	ms := l.node.Members()
	if len(ms) != 3 {
		t.Fatalf("members: %+v", ms)
	}
	for _, m := range ms {
		if !m.Voter || m.LastContact.IsZero() || time.Since(m.LastContact) > 5*time.Second {
			t.Fatalf("member %+v: want a voter heard from lately", m)
		}
	}
	if v, ok := c.leading.Load(l.id); !ok || v != true {
		t.Fatal("OnLeader did not run on the leader")
	}
	for _, s := range c.servers {
		for _, p := range []string{Dir(s.dir), filepath.Join(Dir(s.dir), logFile)} {
			fi, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode().Perm()&0o077 != 0 {
				t.Fatalf("%s is mode %v", p, fi.Mode().Perm())
			}
		}
	}
}

// TestLeaderLossAndReturn: the leader stops; the other two elect a leader
// and take writes; every write acknowledged before the stop is there; the
// old leader comes back as a follower and catches up. Then two of three
// are down: no writes, and the error says why.
func TestLeaderLossAndReturn(t *testing.T) {
	c := newTestCluster(t, 3)
	c.start()
	old := c.leader()
	ctx := context.Background()
	var acked []string
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("app%02d", i)
		if _, err := old.st.PutApp(ctx, spec.App{Name: name, Image: "nginx", Replicas: 1}); err == nil {
			acked = append(acked, name)
		}
	}
	stopped := time.Now()
	old.node.Close()
	old.node = nil
	nl := c.leader()
	t.Logf("new leader %s %s after the old one stopped", nl.id, time.Since(stopped).Round(time.Millisecond))
	if nl == old {
		t.Fatal("the stopped server leads")
	}
	if v, ok := c.leading.Load(old.id); !ok || v != false {
		t.Fatal("OnLeader's context did not end on the old leader")
	}
	for _, name := range acked {
		if _, err := nl.st.GetApp(ctx, name); err != nil {
			t.Fatalf("acknowledged write %s is missing on the new leader: %v", name, err)
		}
	}
	if _, err := nl.st.PutApp(ctx, spec.App{Name: "after", Image: "nginx", Replicas: 2}); err != nil {
		t.Fatal(err)
	}
	// The old leader comes back on its address, as a follower, and catches
	// up from the log.
	n, err := Open(c.config(old))
	if err != nil {
		t.Fatal(err)
	}
	old.node = n
	want := appNames(t, nl.st)
	c.converged(want)
	leaders := 0
	for _, s := range c.servers {
		if s.node.IsLeader() {
			leaders++
		}
	}
	if leaders > 1 {
		t.Fatalf("%d servers lead", leaders)
	}

	// Two of three down, the leader among them: the last one cannot
	// write, and says why.
	var last *testServer
	for _, s := range c.servers {
		if last == nil && s != nl {
			last = s
			continue
		}
		s.node.Close()
		s.node = nil
	}
	c.waitFor(10*time.Second, "no leader", func() bool { return !last.node.HasLeader() })
	_, err = last.st.PutApp(ctx, spec.App{Name: "nope", Image: "nginx", Replicas: 1})
	var e *NotLeaderError
	if !errors.As(err, &e) || e.Leader != "" || e.Servers != 3 || !strings.Contains(err.Error(), "a write needs 2") {
		t.Fatalf("a write with two of three down: %v", err)
	}
	// Reads go on.
	if got := appNames(t, last.st); got != want {
		t.Fatalf("reads with two of three down: %s, want %s", got, want)
	}
}

// TestForwarding: a follower passes API, node and join requests to the
// leader with the caller's address, whether it came over TLS, and the
// worker's verified certificate; a caller's own forwarding headers do not
// get through; a peer without the server role cannot reach the port.
func TestForwarding(t *testing.T) {
	c := newTestCluster(t, 3)
	c.start()
	l := c.leader()
	type seen struct {
		Kind, Remote, Peer, Forged string
		TLS                        bool
	}
	echo := func(kind string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s := seen{Kind: kind, Remote: r.RemoteAddr, TLS: r.TLS != nil, Forged: r.Header.Get(HeaderRemote) + r.Header.Get(HeaderChain)}
			if id, err := pki.RequestNodeID(r); err == nil {
				s.Peer = id
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(s)
		})
	}
	for _, s := range c.servers {
		s.node.SetHandlers(Handlers{API: echo(KindAPI), Node: echo(KindNode), Join: echo(KindJoin)})
	}
	f := c.servers[1]
	if f == l {
		f = c.servers[2]
	}
	worker := c.issue("node-w1", false)
	call := func(kind string, r *http.Request) seen {
		t.Helper()
		rec := httptest.NewRecorder()
		f.node.Forward(kind, rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", kind, rec.Code, rec.Body.String())
		}
		var s seen
		if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	r := httptest.NewRequest("GET", "/v1/apps", nil)
	r.RemoteAddr = "10.9.8.7:5555"
	r.Header.Set(HeaderRemote, "1.2.3.4:1")
	r.Header.Set(HeaderChain, "forged")
	got := call(KindAPI, r)
	if got.Kind != KindAPI || got.Remote != "10.9.8.7:5555" || got.TLS || got.Forged != "" {
		t.Fatalf("an API request through the follower: %+v", got)
	}
	r = httptest.NewRequest("GET", "/v1/apps", nil)
	r.RemoteAddr = "10.9.8.7:5556"
	r.TLS = &tls.ConnectionState{}
	if got := call(KindAPI, r); !got.TLS {
		t.Fatalf("an API request over TLS through the follower: %+v", got)
	}
	r = httptest.NewRequest("GET", "/node/v1/nodes/node-w1/assignments", nil)
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{worker.Leaf}}
	r.TLS.PeerCertificates = append(r.TLS.PeerCertificates, c.ca.Cert)
	if got := call(KindNode, r); got.Peer != "node-w1" || got.Kind != KindNode {
		t.Fatalf("a node request through the follower: %+v", got)
	}
	// A chain from another CA is refused by the leader.
	other := newTestCluster(t, 1)
	stranger := other.issue("node-w2", false)
	r = httptest.NewRequest("GET", "/node/v1/nodes/node-w2/assignments", nil)
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{stranger.Leaf, other.ca.Cert}}
	rec := httptest.NewRecorder()
	f.node.Forward(KindNode, rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a node request with another CA's certificate: %d %s", rec.Code, rec.Body.String())
	}
	if got := call(KindJoin, httptest.NewRequest("POST", "/join", strings.NewReader("{}"))); got.Kind != KindJoin {
		t.Fatalf("a join through the follower: %+v", got)
	}

	// A worker's certificate cannot reach the server port, nor can the
	// leader's own certificate pass a forged chain without the role.
	for _, cert := range []tls.Certificate{worker, stranger} {
		cfg := &tls.Config{Certificates: []tls.Certificate{cert}, RootCAs: c.trust.Load().Pool(), ServerName: "127.0.0.1",
			NextProtos: []string{ProtoForward}, MinVersion: tls.VersionTLS13}
		conn, err := tls.Dial("tcp", l.node.BoundAddr(), cfg)
		if err == nil {
			// TLS 1.3 reports a refused client certificate at the first read.
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			_, err = conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))
			if err == nil {
				_, err = conn.Read(make([]byte, 1))
			}
			conn.Close()
		}
		if err == nil {
			t.Fatalf("a certificate without the server role reached the server port")
		}
	}
}

// testLogger discards, or writes to stderr with HA_TEST_LOG set.
func testLogger(id string) *slog.Logger {
	if os.Getenv("HA_TEST_LOG") == "" {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})).With("server", id)
}

// TestRemoveServer: a follower removed from the configuration is gone from
// the leader's; the two left take writes; and the removed server's
// certificate, still valid and of the server role, is refused on the
// leader's server port.
func TestRemoveServer(t *testing.T) {
	c := newTestCluster(t, 3)
	c.start()
	l := c.leader()
	var f *testServer
	for _, s := range c.servers {
		if s != l {
			f = s
			break
		}
	}
	if err := l.node.RemoveServer(f.id); err != nil {
		t.Fatal(err)
	}
	c.waitFor(10*time.Second, "two servers in the configuration", func() bool { return len(l.node.Members()) == 2 })
	if l.node.IsVoterID(f.id) {
		t.Fatalf("the removed server is still a voter: %+v", l.node.Members())
	}
	if _, err := l.st.PutApp(context.Background(), spec.App{Name: "after", Image: "nginx", Replicas: 1}); err != nil {
		t.Fatalf("a write with two servers left: %v", err)
	}
	cfg := &tls.Config{Certificates: []tls.Certificate{*f.cert.Load()}, RootCAs: c.trust.Load().Pool(), ServerName: "127.0.0.1",
		NextProtos: []string{ProtoForward}, MinVersion: tls.VersionTLS13}
	conn, err := tls.Dial("tcp", l.node.BoundAddr(), cfg)
	if err == nil {
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err = conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err == nil {
			_, err = conn.Read(make([]byte, 1))
		}
		conn.Close()
	}
	if err == nil {
		t.Fatal("the removed server's certificate reached the leader's server port")
	}
}

// TestHandover: a leader that hands the leadership on before it stops
// leaves the cluster without a leader that writes for less than raft's
// heartbeat timeout, which a leader that just stops costs at the least
// (the eighth soak's second finding); writes go on through the new
// leader. A follower, or a server that is the only voter, has nothing to
// hand over.
func TestHandover(t *testing.T) {
	c := newTestCluster(t, 3)
	for _, s := range c.servers {
		s.logs = &lockedBuffer{}
	}
	c.start()
	old := c.leader()
	var f *testServer
	for _, s := range c.servers {
		if s != old {
			f = s
			break
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.node.Handover(ctx); err != nil || !old.node.IsLeader() {
		t.Fatalf("a follower's handover: %v; the leader still leads: %v", err, old.node.IsLeader())
	}
	if _, err := old.st.PutApp(ctx, spec.App{Name: "before", Image: "nginx", Replicas: 2}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := old.node.Handover(ctx); err != nil {
		t.Fatal(err)
	}
	if old.node.IsLeader() {
		t.Fatal("the server that handed the leadership on still leads")
	}
	var nl *testServer
	for nl == nil {
		for _, s := range c.servers {
			if s != old && s.node.IsLeader() {
				nl = s
			}
		}
		if time.Since(start) > 5*time.Second {
			t.Fatal("no new leader 5 s after the handover")
		}
		time.Sleep(2 * time.Millisecond)
	}
	gap := time.Since(start)
	c.waitFor(5*time.Second, "the handover's line", func() bool { return strings.Contains(old.logs.String(), "handed the leadership on") })
	for _, l := range strings.Split(old.logs.String(), "\n") {
		if strings.Contains(l, "handed the leadership on") && !strings.Contains(l, " to="+nl.id+" ") {
			t.Fatalf("the handover names no new leader: %s", l)
		}
		if strings.Contains(l, newerTermMsg) && !strings.Contains(l, "level=INFO") {
			t.Fatalf("a planned handover: %s", l)
		}
	}
	t.Logf("%s leads, and writes, %s after the handover began", nl.id, gap.Round(time.Millisecond))
	if gap >= testTiming.Heartbeat {
		t.Fatalf("a leader that writes %s after the handover began, no sooner than after a stop without one (%s)", gap, testTiming.Heartbeat)
	}
	old.node.Close()
	old.node = nil
	if _, err := nl.st.PutApp(ctx, spec.App{Name: "after", Image: "nginx", Replicas: 1}); err != nil {
		t.Fatal(err)
	}
	c.converged("after:1,before:2")

	one := newTestCluster(t, 1)
	one.start()
	if err := one.leader().node.Handover(ctx); err != nil || !one.servers[0].node.IsLeader() {
		t.Fatalf("the only voter's handover: %v; it leads: %v", err, one.servers[0].node.IsLeader())
	}
}

// logger is the server's logger: into logs at info when it is set, else
// testLogger's.
func (s *testServer) logger() *slog.Logger {
	if s.logs != nil {
		return slog.New(slog.NewTextHandler(s.logs, nil))
	}
	return testLogger(s.id)
}
