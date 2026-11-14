package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/cluster"
	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/store"
)

// fakeRenewServer stands in for the server's renew route. It issues a
// certificate for the ID it is told to, which a test can point elsewhere.
type fakeRenewServer struct {
	ca     *pki.CA
	issue  string // node ID to issue for; empty means the one asked for
	calls  int
	closed int
	err    error
}

func (f *fakeRenewServer) Renew(_ context.Context, nodeID string, csr []byte) ([]byte, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	id := nodeID
	if f.issue != "" {
		id = f.issue
	}
	der, err := f.ca.IssueNodeCert(csr, pki.NodeCertOptions{NodeID: id})
	if err != nil {
		return nil, err
	}
	return pemEncodeCert(der), nil
}

func (f *fakeRenewServer) CloseIdleConnections() { f.closed++ }

// joinedWithLifetime writes a worker's files for id, with a certificate that
// was issued at issued and lasts lifetime.
func joinedWithLifetime(t *testing.T, dir string, ca *pki.CA, id string, issued time.Time, lifetime time.Duration) {
	t.Helper()
	key, err := pki.NewNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	csr, _ := pki.NewCSR(key)
	der, err := ca.IssueNodeCert(csr, pki.NodeCertOptions{NodeID: id, Now: issued, Lifetime: lifetime})
	if err != nil {
		t.Fatal(err)
	}
	st := workerState{NodeID: id, Server: "10.0.0.5:4484", Joined: issued}
	if err := saveWorkerState(dir, st, key, pemEncodeCert(der), ca.CertPEM); err != nil {
		t.Fatal(err)
	}
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestRenewer(t *testing.T, dir string, srv certRenewer, now *time.Time) *renewer {
	t.Helper()
	w, err := loadWorkerState(dir)
	if err != nil {
		t.Fatal(err)
	}
	return &renewer{dir: dir, nodeID: w.state.NodeID, ca: w.ca, holder: w.holder, client: srv, log: quietLog(),
		now: func() time.Time { return *now }}
}

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The worker renews once two thirds of the lifetime have passed, not
// before. The renewed pair is for the same node ID and a new key, it is
// written with mode 0600 and loads again, and the client config presents it
// from then on.
func TestRenewerRenewsAtThresholdNotBefore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "worker")
	ca := newCA(t)
	t0 := time.Now().Add(-200 * time.Hour).Truncate(time.Second)
	joinedWithLifetime(t, dir, ca, "node-abc123", t0, 300*time.Hour)
	srv := &fakeRenewServer{ca: ca}
	now := t0.Add(200*time.Hour - time.Minute)
	r := newTestRenewer(t, dir, srv, &now)
	oldLeaf := r.holder.Leaf()
	oldKey := readFile(t, filepath.Join(dir, workerKeyFile))
	ctx := context.Background()

	if renewed, err := r.renewIfDue(ctx); err != nil || renewed || srv.calls != 0 {
		t.Fatalf("a minute before the threshold: renewed %v, err %v, calls %d", renewed, err, srv.calls)
	}
	now = t0.Add(200 * time.Hour)
	if renewed, err := r.renewIfDue(ctx); err != nil || !renewed || srv.calls != 1 {
		t.Fatalf("at the threshold: renewed %v, err %v, calls %d", renewed, err, srv.calls)
	}
	newLeaf := r.holder.Leaf()
	if newLeaf.SerialNumber.Cmp(oldLeaf.SerialNumber) == 0 {
		t.Fatal("the holder still has the old certificate")
	}
	if id, err := pki.NodeIDFromCert(newLeaf); err != nil || id != "node-abc123" {
		t.Fatalf("renewed certificate names %q, %v", id, err)
	}
	if srv.closed != 1 {
		t.Fatalf("idle connections closed %d times, want 1", srv.closed)
	}
	if bytes.Equal(oldKey, readFile(t, filepath.Join(dir, workerKeyFile))) {
		t.Fatal("the key was not rotated")
	}
	for _, name := range []string{workerKeyFile, workerCertFile} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s: mode %v, %v; want 0600", name, info.Mode().Perm(), err)
		}
	}
	for _, name := range []string{workerKeyNext, workerCertNext} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("staging file %s left behind: %v", name, err)
		}
	}
	pair, err := tls.LoadX509KeyPair(filepath.Join(dir, workerCertFile), filepath.Join(dir, workerKeyFile))
	if err != nil {
		t.Fatalf("the files on disk are not a matching pair: %v", err)
	}
	if !bytes.Equal(pair.Certificate[0], newLeaf.Raw) {
		t.Fatal("node.crt is not the certificate in use")
	}
	w, err := loadWorkerState(dir)
	if err != nil {
		t.Fatalf("loading the renewed files: %v", err)
	}
	if w.holder.Leaf().SerialNumber.Cmp(newLeaf.SerialNumber) != 0 {
		t.Fatal("a restart would load another certificate")
	}
	got, err := r.holder.GetClientCertificate(nil)
	if err != nil || !bytes.Equal(got.Certificate[0], newLeaf.Raw) {
		t.Fatal("the client config does not present the renewed certificate")
	}
}

// A reply for another node ID, or for another key, is refused, and nothing
// changes.
func TestRenewerRefusesAWrongCertificate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "worker")
	ca := newCA(t)
	t0 := time.Now().Add(-250 * time.Hour)
	joinedWithLifetime(t, dir, ca, "node-abc123", t0, 300*time.Hour)
	now := time.Now()
	srv := &fakeRenewServer{ca: ca, issue: "node-other"}
	r := newTestRenewer(t, dir, srv, &now)
	before := readFile(t, filepath.Join(dir, workerCertFile))
	serial := r.holder.Leaf().SerialNumber
	if _, err := r.renewIfDue(context.Background()); err == nil || !strings.Contains(err.Error(), "node-other") {
		t.Fatalf("a certificate for another node: %v", err)
	}
	if !bytes.Equal(before, readFile(t, filepath.Join(dir, workerCertFile))) || r.holder.Leaf().SerialNumber.Cmp(serial) != 0 {
		t.Fatal("a refused certificate changed the worker's certificate")
	}

	// A certificate from another CA.
	srv2 := &fakeRenewServer{ca: newCA(t)}
	r.client = srv2
	if _, err := r.renewIfDue(context.Background()); err == nil || !strings.Contains(err.Error(), "CA") {
		t.Fatalf("a certificate from another CA: %v", err)
	}
	// A failed call changes nothing either.
	r.client = &fakeRenewServer{ca: ca, err: nodeapi.ErrNodeRemoved}
	if _, err := r.renewIfDue(context.Background()); !errors.Is(err, nodeapi.ErrNodeRemoved) {
		t.Fatalf("a refused call: %v", err)
	}
	if !bytes.Equal(before, readFile(t, filepath.Join(dir, workerCertFile))) {
		t.Fatal("a refused call changed node.crt")
	}
}

// A worker whose certificate has expired says what to do: remove the node
// and join again. At start-up and from the renewal loop.
func TestExpiredWorkerCertSaysRemoveAndRejoin(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "worker")
	ca := newCA(t)
	joinedWithLifetime(t, dir, ca, "node-abc123", time.Now().Add(-2*time.Hour), time.Hour)
	_, err := loadWorkerState(dir)
	if err == nil || !strings.Contains(err.Error(), "expired") || !strings.Contains(err.Error(), "hivemind node remove node-abc123") ||
		!strings.Contains(err.Error(), "empty --data-dir") {
		t.Fatalf("loading an expired certificate: %v", err)
	}

	dir2 := filepath.Join(t.TempDir(), "worker")
	joinedWithLifetime(t, dir2, ca, "node-abc123", time.Now(), time.Hour)
	srv := &fakeRenewServer{ca: ca}
	now := time.Now().Add(2 * time.Hour)
	r := newTestRenewer(t, dir2, srv, &now)
	if _, err := r.renewIfDue(context.Background()); err == nil || !strings.Contains(err.Error(), "hivemind node remove") {
		t.Fatalf("renewal with an expired certificate: %v", err)
	}
	if srv.calls != 0 {
		t.Fatal("the worker called the server with an expired certificate")
	}
}

// A renewal that a crash interrupted is finished or dropped when the worker
// loads its files.
func TestRecoverInterruptedRenewal(t *testing.T) {
	ca := newCA(t)

	// Crash before the renames: both staging files, and the old pair in place.
	dir := filepath.Join(t.TempDir(), "worker")
	joinedWithLifetime(t, dir, ca, "node-abc123", time.Now(), 0)
	oldCert := readFile(t, filepath.Join(dir, workerCertFile))
	_, der := issueForNode(t, ca, "node-abc123")
	if err := writeSynced(filepath.Join(dir, workerKeyNext), []byte("staged key")); err != nil {
		t.Fatal(err)
	}
	if err := writeSynced(filepath.Join(dir, workerCertNext), pemEncodeCert(der)); err != nil {
		t.Fatal(err)
	}
	w, err := loadWorkerState(dir)
	if err != nil {
		t.Fatalf("load after a crash before the renames: %v", err)
	}
	if !bytes.Equal(w.holder.Leaf().Raw, mustLeaf(t, oldCert).Raw) {
		t.Fatal("the old pair was not kept")
	}
	for _, name := range []string{workerKeyNext, workerCertNext} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s was not removed: %v", name, err)
		}
	}

	// Crash between the renames: the new key in place, its certificate staged.
	dir = filepath.Join(t.TempDir(), "worker")
	joinedWithLifetime(t, dir, ca, "node-abc123", time.Now(), 0)
	key, der := issueForNode(t, ca, "node-abc123")
	if err := writeRenewed(dir, key, pemEncodeCert(der)); err != nil {
		t.Fatal(err)
	}
	// Undo the second rename, as if the crash came before it.
	if err := os.Rename(filepath.Join(dir, workerCertFile), filepath.Join(dir, workerCertNext)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, workerCertFile), oldCert, 0o600); err != nil {
		t.Fatal(err)
	}
	w, err = loadWorkerState(dir)
	if err != nil {
		t.Fatalf("load after a crash between the renames: %v", err)
	}
	if !bytes.Equal(w.holder.Leaf().Raw, der) {
		t.Fatal("the staged certificate was not put in place")
	}
}

func mustLeaf(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	blk, _ := pem.Decode(certPEM)
	if blk == nil {
		t.Fatal("no PEM certificate")
	}
	leaf, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return leaf
}

// The worker renews over the real node API: the server issues for the same
// ID, records the expiry, and the worker keeps working on the new pair.
func TestWorkerRenewsOverNodeAPI(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	ca, err := pki.CreateCA(filepath.Join(base, "server"))
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, filepath.Join(base, "server", "hivemind.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	tokens := pki.NewTokens(st.JoinTokens())
	c, err := cluster.Listen(cluster.Config{Store: st, CA: ca, Tokens: tokens, Advertise: "127.0.0.1",
		LocalNodeID: "node-server", Logger: quietLog()})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = c.Serve() }()
	defer c.Shutdown()

	tok, _, err := tokens.Issue(ca.Fingerprint(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := pki.NewNodeKey()
	csr, _ := pki.NewCSR(key)
	reply, err := nodeapi.Join(ctx, c.JoinAddr(), tok, csr)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "worker")
	if err := saveWorkerState(dir, workerState{NodeID: reply.NodeID, Server: c.NodeAddr(), Joined: time.Now()},
		key, []byte(reply.CertPEM), []byte(reply.CAPEM)); err != nil {
		t.Fatal(err)
	}
	w, err := loadWorkerState(dir)
	if err != nil {
		t.Fatal(err)
	}
	client, err := nodeapi.NewClient("https://"+c.NodeAddr(), w.tlsCfg)
	if err != nil {
		t.Fatal(err)
	}
	reg := nodeapi.Register{Name: "w", CPU: 1, Memory: 1 << 30}
	if err := client.Register(ctx, reply.NodeID, reg); err != nil {
		t.Fatal(err)
	}
	old := w.holder.Leaf()

	// 300 days on, by the worker's clock: past two thirds of 365 days.
	time.Sleep(1100 * time.Millisecond)
	later := time.Now().Add(300 * 24 * time.Hour)
	r := &renewer{dir: dir, nodeID: reply.NodeID, ca: w.ca, holder: w.holder, client: client, log: quietLog(),
		now: func() time.Time { return later }}
	if renewed, err := r.renewIfDue(ctx); err != nil || !renewed {
		t.Fatalf("renewal over the node API: %v, %v", renewed, err)
	}
	renewed := w.holder.Leaf()
	if renewed.SerialNumber.Cmp(old.SerialNumber) == 0 || !renewed.NotAfter.After(old.NotAfter) {
		t.Fatal("the worker did not switch to a new certificate")
	}
	if id, _ := pki.NodeIDFromCert(renewed); id != reply.NodeID {
		t.Fatalf("renewed certificate names %q, want %q", id, reply.NodeID)
	}
	rec, err := st.NodeCertExpiry(ctx, reply.NodeID)
	if err != nil || !rec.Equal(renewed.NotAfter) {
		t.Fatalf("server's record %s, %v; want %s", rec, err, renewed.NotAfter)
	}
	// New handshakes present the new certificate, and the worker keeps working.
	if err := client.Register(ctx, reply.NodeID, reg); err != nil {
		t.Fatalf("a call after the renewal: %v", err)
	}
	if _, err := client.AssignmentsForNode(ctx, reply.NodeID); err != nil {
		t.Fatalf("assignments after the renewal: %v", err)
	}
}
