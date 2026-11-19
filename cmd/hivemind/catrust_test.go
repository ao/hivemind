package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/carotate"
	"github.com/ao/hivemind/internal/cluster"
	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/store"
)

// rotServer is a server's store, CA rotation and cluster listeners on
// loopback.
type rotServer struct {
	dir string
	st  *store.Store
	m   *carotate.Manager
	cl  *cluster.Cluster
}

func newRotServer(t *testing.T) *rotServer {
	t.Helper()
	dir := t.TempDir()
	if _, err := pki.CreateCA(dir); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), filepath.Join(dir, "hivemind.db"), store.WithMinReady(0))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := carotate.Open(context.Background(), carotate.Options{Dir: dir, Store: st, LocalNode: "node-server", Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	cl, err := cluster.Listen(cluster.Config{Store: st, Rotation: m, Tokens: pki.NewTokens(st.JoinTokens()),
		Advertise: "127.0.0.1", Port: 0, LocalNodeID: "node-server", Logger: log})
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = cl.Serve() }()
	go m.Run(ctx)
	t.Cleanup(func() {
		cancel()
		_ = cl.Shutdown()
		st.Close()
	})
	return &rotServer{dir: dir, st: st, m: m, cl: cl}
}

// join enrolls a worker in dir, as hivemind join does, with a token for the
// CA of the server's certificate.
func (s *rotServer) join(t *testing.T, dir string) {
	t.Helper()
	tok, _, err := pki.NewTokens(s.st.JoinTokens()).Issue(s.m.Listener().Fingerprint(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := pki.NewNodeKey()
	csr, _ := pki.NewCSR(key)
	reply, err := nodeapi.Join(context.Background(), s.cl.JoinAddr(), tok, csr)
	if err != nil {
		t.Fatal(err)
	}
	st := workerState{NodeID: reply.NodeID, Server: s.cl.NodeAddr(), Joined: time.Now().UTC()}
	if err := saveWorkerState(dir, st, key, []byte(reply.CertPEM), []byte(reply.CAPEM)); err != nil {
		t.Fatal(err)
	}
}

// testWorker is what runWorker builds from a data directory: the trust
// keeper, the node API client and the renewer.
type testWorker struct {
	dir    string
	w      *joinedWorker
	keeper *trustKeeper
	client *nodeapi.Client
	r      *renewer
}

func startTestWorker(t *testing.T, dir string) *testWorker {
	t.Helper()
	w, err := loadWorkerState(dir)
	if err != nil {
		t.Fatal(err)
	}
	host, _, _ := net.SplitHostPort(w.state.Server)
	k := newTrustKeeper(dir, w.state.NodeID, host, w.trust, w.holder, quietLog())
	c, err := nodeapi.NewClient("https://"+w.state.Server, w.tlsCfg, nodeapi.WithVersion(version), nodeapi.WithCATrust(k.header))
	if err != nil {
		t.Fatal(err)
	}
	k.client = c
	c.OnCA(k.seen)
	r := &renewer{dir: dir, nodeID: w.state.NodeID, trust: k.Trust, holder: w.holder, client: c, log: quietLog(), now: time.Now, rotation: k}
	return &testWorker{dir: dir, w: w, keeper: k, client: c, r: r}
}

func (tw *testWorker) register() error {
	return tw.client.Register(context.Background(), tw.w.state.NodeID, nodeapi.Register{Name: "w", CPU: 1, Memory: 1 << 30})
}

// follow does what the keeper's loop does on a reply with a new digest:
// it applies the server's set, and the renewer renews if the set asks.
func (tw *testWorker) follow(t *testing.T) {
	t.Helper()
	if err := tw.register(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-tw.keeper.want:
	default:
		t.Fatal("a reply with a new CA digest did not ask the keeper to read the set")
	}
	if err := tw.keeper.apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.r.renewIfDue(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// diskTrust reads ca.crt.
func diskTrust(t *testing.T, dir string) *pki.Trust {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, pki.CACertFile))
	if err != nil {
		t.Fatal(err)
	}
	tr, err := pki.ParseTrustPEM(b)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func certFrom(tw *testWorker, ca *pki.CA) bool {
	return tw.w.holder.Leaf().CheckSignatureFrom(ca.Cert) == nil
}

// A worker follows a whole rotation over the node API: it adds the new CA
// at start, renews under it in the renew phase, keeps working when the
// server's certificate moves, and drops the old CA at finish; after a
// restart in each phase it loads what it had.
func TestWorkerFollowsARotation(t *testing.T) {
	s := newRotServer(t)
	ctx := context.Background()
	old := s.m.Current()
	dir := filepath.Join(t.TempDir(), "w")
	s.join(t, dir)
	// A second worker joins and then stays off for the whole rotation.
	offDir := filepath.Join(t.TempDir(), "off")
	s.join(t, offDir)
	off := startTestWorker(t, offDir)
	if err := off.register(); err != nil {
		t.Fatal(err)
	}

	tw := startTestWorker(t, dir)
	if err := tw.register(); err != nil {
		t.Fatal(err)
	}
	<-tw.keeper.want // the first reply's digest, which nothing applied yet
	if err := tw.keeper.apply(ctx); err != nil {
		t.Fatal(err)
	}
	if len(tw.keeper.Trust().CAs()) != 1 {
		t.Fatal("outside a rotation the worker trusts more than one CA")
	}

	// start: the worker adds the new CA, on disk too, its own CA first.
	if _, err := s.m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	next, err := pki.LoadNextCA(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	tw.follow(t)
	if tr := diskTrust(t, dir); len(tr.CAs()) != 2 || pki.Fingerprint(tr.CAs()[0]) != old.Fingerprint() || !tr.Contains(next.Fingerprint()) {
		t.Fatalf("ca.crt in phase trust: %x", tr.Fingerprints())
	}
	if !strings.Contains(tw.keeper.header(), pki.FingerprintHex(next.Fingerprint())) {
		t.Fatal("the worker does not say it trusts the new CA")
	}
	if !certFrom(tw, old) {
		t.Fatal("phase trust: the worker renewed")
	}
	// A restart in phase trust loads both CAs.
	tw = startTestWorker(t, dir)
	if len(tw.w.trust.CAs()) != 2 {
		t.Fatal("a restarted worker lost the new CA")
	}
	if err := tw.register(); err != nil {
		t.Fatal(err)
	}

	// renew: the worker renews under the new CA at once.
	if _, err := s.m.Renew(ctx, true); err != nil { // the offline worker does not trust the new CA
		t.Fatal(err)
	}
	tw.follow(t)
	if !certFrom(tw, next) {
		t.Fatal("phase renew: the worker did not renew under the new CA")
	}
	if tr := diskTrust(t, dir); pki.Fingerprint(tr.CAs()[0]) != next.Fingerprint() {
		t.Fatal("after the renewal, ca.crt does not have the new CA first")
	}
	if err := tw.register(); err != nil {
		t.Fatalf("the renewed worker: %v", err)
	}
	// A restart after the renewal loads the new certificate.
	tw = startTestWorker(t, dir)
	if !certFrom(tw, next) {
		t.Fatal("the restarted worker has not the renewed certificate")
	}
	if err := tw.register(); err != nil {
		t.Fatal(err)
	}
	// The offline worker holds the server's certificate back; finish names it.
	if _, err := s.m.Finish(ctx, false); err == nil || !strings.Contains(err.Error(), off.w.state.NodeID) {
		t.Fatalf("finish with the offline worker: %v", err)
	}
	if _, err := s.m.Finish(ctx, true); err != nil {
		t.Fatal(err)
	}
	tw.follow(t)
	if tr := diskTrust(t, dir); len(tr.CAs()) != 1 || pki.Fingerprint(tr.CAs()[0]) != next.Fingerprint() {
		t.Fatalf("ca.crt after finish: %x", tr.Fingerprints())
	}
	tw = startTestWorker(t, dir)
	if err := tw.register(); err != nil {
		t.Fatalf("the worker after finish and a restart: %v", err)
	}

	// The offline worker comes back: it trusts the old CA only, so its
	// handshake fails, and the error says what to do.
	off = startTestWorker(t, offDir)
	err = off.register()
	var ua x509.UnknownAuthorityError
	if !errors.As(err, &ua) || !strings.Contains(err.Error(), "rotated without this node") || !strings.Contains(err.Error(), "node remove") {
		t.Fatalf("the cut-off worker: %v", err)
	}
}

// An abort in the renew phase turns the rotation round: a worker that
// renewed under the new CA renews back under the old, and keeps both until
// finish, then the old alone.
func TestWorkerFollowsAnAbortInRenew(t *testing.T) {
	s := newRotServer(t)
	ctx := context.Background()
	old := s.m.Current()
	dir := filepath.Join(t.TempDir(), "w")
	s.join(t, dir)
	tw := startTestWorker(t, dir)
	wlogs := &syncLog{}
	tw.r.log = slog.New(slog.NewTextHandler(wlogs, nil))
	if err := tw.register(); err != nil {
		t.Fatal(err)
	}
	<-tw.keeper.want
	if _, err := s.m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	tw.follow(t)
	if err := tw.register(); err != nil { // says it trusts both now
		t.Fatal(err)
	}
	if _, err := s.m.Renew(ctx, false); err != nil {
		t.Fatal(err)
	}
	tw.follow(t)
	next, _ := pki.LoadNextCA(s.dir)
	if !certFrom(tw, next) {
		t.Fatal("the worker did not renew under the new CA")
	}
	firstIs := func(when string, want *pki.CA) {
		t.Helper()
		if tr := diskTrust(t, dir); len(tr.CAs()) != 2 || pki.Fingerprint(tr.CAs()[0]) != want.Fingerprint() {
			t.Fatalf("%s: ca.crt %x, want both CAs, %x first", when, tr.Fingerprints(), want.Fingerprint())
		}
	}
	firstIs("after the renewal under the new CA", next)
	// The renewal's line names the CA it goes to and the one it leaves by
	// fingerprint, which reads right both ways; in the fifth soak it
	// called the old CA new after the abort.
	renewLine := func(to, from *pki.CA) string {
		return "renewing the node certificate under the CA the cluster's CA rotation goes to, as its renew phase asks\" ca=" +
			pki.FingerprintHex(to.Fingerprint())[:16] + " from=" + pki.FingerprintHex(from.Fingerprint())[:16]
	}
	if l := wlogs.String(); !strings.Contains(l, renewLine(next, old)) {
		t.Fatalf("the renewal under the new CA logged:\n%s", l)
	}
	if _, err := s.m.Abort(ctx); err != nil {
		t.Fatal(err)
	}
	tw.follow(t)
	if !certFrom(tw, old) || len(tw.keeper.Trust().CAs()) != 2 {
		t.Fatal("after the abort the worker did not renew back under the old CA, keeping both")
	}
	// The CA of its certificate is first again. In the fifth soak the
	// dropped CA stayed first until finish.
	firstIs("after the renewal back under the old CA", old)
	if l := wlogs.String(); !strings.Contains(l, renewLine(old, next)) || strings.Count(l, "renewing the node certificate") != 2 {
		t.Fatalf("the renewal back under the old CA logged:\n%s", l)
	}
	if pki.Fingerprint(tw.keeper.Trust().CAs()[0]) != old.Fingerprint() {
		t.Fatal("the set in memory does not have the old CA first")
	}
	// A worker that starts with ca.crt in the wrong order, as after a crash
	// between a renewal and its reorder, puts it right.
	if err := os.WriteFile(filepath.Join(dir, pki.CACertFile), pki.MustTrust(next.Cert, old.Cert).PEM(), 0o600); err != nil {
		t.Fatal(err)
	}
	tw = startTestWorker(t, dir)
	tw.keeper.renewed()
	firstIs("at a start with the new CA first", old)
	if err := tw.register(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.m.Finish(ctx, false); err != nil {
		t.Fatal(err)
	}
	tw.follow(t)
	if tr := diskTrust(t, dir); len(tr.CAs()) != 1 || pki.Fingerprint(tr.CAs()[0]) != old.Fingerprint() {
		t.Fatalf("ca.crt after the turned rotation ended: %x", tr.Fingerprints())
	}
	if _, err := os.Stat(filepath.Join(s.dir, pki.NextCAKeyFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the new CA's key is still on the server")
	}
}

// fakeCASet serves a CA set, as a server would, with the CA its connection
// was checked against.
type fakeCASet struct {
	b    nodeapi.CABundle
	root *x509.Certificate
	set  int
}

func (f *fakeCASet) CABundle(context.Context, string) (nodeapi.CABundle, *x509.Certificate, error) {
	return f.b, f.root, nil
}
func (f *fakeCASet) SetTLSConfig(*tls.Config) { f.set++ }
func (f *fakeCASet) ResetConnections()        {}

// The worker's checks of a CA set: a new CA only on a cross-certificate
// from a CA it trusts; never without the CA of its own certificate or of
// the set's connection.
func TestTrustKeeperChecksTheSet(t *testing.T) {
	sdir := t.TempDir()
	old, err := pki.CreateCA(sdir)
	if err != nil {
		t.Fatal(err)
	}
	next, err := pki.CreateNextCA(sdir)
	if err != nil {
		t.Fatal(err)
	}
	_, rogue := newTwoCAs(t)
	dir := filepath.Join(t.TempDir(), "w")
	saveJoined(t, dir, old, "node-abc123")
	w, err := loadWorkerState(dir)
	if err != nil {
		t.Fatal(err)
	}
	cross := func(signer, ca *pki.CA) string {
		der, err := signer.CrossSign(ca.Cert)
		if err != nil {
			t.Fatal(err)
		}
		return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	}
	newKeeper := func(f *fakeCASet) *trustKeeper {
		k := newTrustKeeper(dir, "node-abc123", "10.0.0.5", w.trust, w.holder, quietLog())
		k.client = f
		return k
	}
	ctx := context.Background()

	// A new CA cross-signed by a CA the worker does not trust is left out.
	f := &fakeCASet{root: old.Cert, b: nodeapi.CABundle{Phase: nodeapi.CAPhaseTrust,
		CAs: []nodeapi.BundleCA{{CertPEM: string(old.CertPEM)}, {CertPEM: string(next.CertPEM), CrossPEM: cross(rogue, next)}}}}
	k := newKeeper(f)
	if err := k.apply(ctx); err != nil || len(k.Trust().CAs()) != 1 || f.set != 0 {
		t.Fatalf("a cross-certificate from an untrusted CA: %v, %d CAs", err, len(k.Trust().CAs()))
	}
	// A new CA with no cross-certificate is left out.
	f.b.CAs[1].CrossPEM = ""
	if err := k.apply(ctx); err != nil || len(k.Trust().CAs()) != 1 {
		t.Fatalf("a new CA without a cross-certificate: %v", err)
	}
	// A cross-certificate for another CA is refused.
	f.b.CAs[1].CrossPEM = cross(old, rogue)
	if err := k.apply(ctx); err == nil || len(k.Trust().CAs()) != 1 {
		t.Fatalf("a cross-certificate for another CA: %v", err)
	}
	// The right cross-certificate: the CA is added, and ca.crt written.
	f.b.CAs[1].CrossPEM = cross(old, next)
	if err := k.apply(ctx); err != nil || len(k.Trust().CAs()) != 2 || f.set != 1 {
		t.Fatalf("the right cross-certificate: %v", err)
	}
	if tr := diskTrust(t, dir); len(tr.CAs()) != 2 {
		t.Fatal("ca.crt not written")
	}
	// A set that leaves out the CA of its own connection is not applied.
	f2 := &fakeCASet{root: next.Cert, b: nodeapi.CABundle{CAs: []nodeapi.BundleCA{{CertPEM: string(old.CertPEM)}}}}
	k.client = f2
	if err := k.apply(ctx); err == nil || len(k.Trust().CAs()) != 2 {
		t.Fatalf("a set without its connection's CA: %v", err)
	}
	// A set without the CA of the worker's own certificate keeps it.
	f3 := &fakeCASet{root: next.Cert, b: nodeapi.CABundle{CAs: []nodeapi.BundleCA{{CertPEM: string(next.CertPEM)}}}}
	k.client = f3
	if err := k.apply(ctx); err != nil || !k.Trust().Contains(old.Fingerprint()) {
		t.Fatalf("a set without the worker's own CA: %v", err)
	}
	// A server older than rotation has no set: nothing changes.
	k.client = noRouteCASet{}
	if err := k.apply(ctx); err != nil {
		t.Fatal(err)
	}
	// A crash while ca.crt was replaced leaves ca.crt.next, which the
	// next start removes.
	if err := os.WriteFile(filepath.Join(dir, workerTrustNext), []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadWorkerState(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, workerTrustNext)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a stale ca.crt.next was left")
	}
}

type noRouteCASet struct{}

func (noRouteCASet) CABundle(context.Context, string) (nodeapi.CABundle, *x509.Certificate, error) {
	return nodeapi.CABundle{}, nil, nodeapi.ErrNoRoute
}
func (noRouteCASet) SetTLSConfig(*tls.Config) {}
func (noRouteCASet) ResetConnections()        {}

// newTwoCAs makes two unrelated CAs.
func newTwoCAs(t *testing.T) (*pki.CA, *pki.CA) {
	t.Helper()
	return newCA(t), newCA(t)
}
