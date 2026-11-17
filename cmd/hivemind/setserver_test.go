package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	stdlog "log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/cluster"
	"github.com/ao/hivemind/internal/config"
	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/store"
)

// testCluster is a server's cluster listeners on loopback, with their store.
type testCluster struct {
	ca *pki.CA
	st *store.Store
	cl *cluster.Cluster
}

// newTestCluster starts the cluster listeners of a server with CA ca (a new
// one when nil), advertising advertise.
func newTestCluster(t *testing.T, ca *pki.CA, advertise string) *testCluster {
	t.Helper()
	return newTestClusterAt(t, ca, advertise, 0)
}

// newTestClusterAt is newTestCluster for a server whose store is at epoch.
func newTestClusterAt(t *testing.T, ca *pki.CA, advertise string, epoch int64) *testCluster {
	t.Helper()
	dir := t.TempDir()
	if ca == nil {
		var err error
		if ca, err = pki.CreateCA(filepath.Join(dir, "ca")); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(context.Background(), filepath.Join(dir, "hivemind.db"), store.WithMinReady(0))
	if err != nil {
		t.Fatal(err)
	}
	cl, err := cluster.Listen(cluster.Config{Store: st, CA: ca, Tokens: pki.NewTokens(st.JoinTokens()),
		Advertise: advertise, Bind: "127.0.0.1", Port: 0, LocalNodeID: "node-server", Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Epoch: epoch})
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	go func() { _ = cl.Serve() }()
	t.Cleanup(func() {
		_ = cl.Shutdown()
		st.Close()
	})
	return &testCluster{ca: ca, st: st, cl: cl}
}

// workerFiles reads every file in a worker's data directory, to show that a
// refused set-server changed none of them.
func workerFiles(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.Name() == lockFileName {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = b
	}
	return out
}

func sameFiles(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if !bytes.Equal(v, b[k]) {
			return false
		}
	}
	return true
}

// set-server takes a server only once it has checked it from the worker:
// the right server is taken, and every other is refused with what was
// found, with every file of the worker left as it was. Each server here is a
// real TLS listener on loopback.
func TestSetServerChecksTheServer(t *testing.T) {
	ctx := context.Background()
	right := newTestCluster(t, nil, "127.0.0.1")
	wdir := filepath.Join(t.TempDir(), "worker")
	saveJoined(t, wdir, right.ca, "node-w1") // joined 10.0.0.5:4484
	before := workerFiles(t, wdir)

	refused := func(name, addr, want string) {
		t.Helper()
		var out bytes.Buffer
		start := time.Now()
		err := runSetServer(ctx, &out, wdir, addr)
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "node.json is unchanged") {
			t.Errorf("%s (%s): %v; want an error with %q", name, addr, err, want)
		}
		if d := time.Since(start); d > 10*time.Second {
			t.Errorf("%s took %s", name, d)
		}
		if after := workerFiles(t, wdir); !sameFiles(before, after) {
			t.Errorf("%s changed the worker's files", name)
		}
	}

	// Another cluster's server, as a man in the middle with its own CA
	// would be, at the address.
	other := newTestCluster(t, nil, "127.0.0.1")
	refused("another cluster's server", other.cl.NodeAddr(), fmt.Sprintf("presents a certificate from the CA with fingerprint %x, not from this worker's cluster CA, fingerprint %x: it is another cluster's server", other.ca.Fingerprint(), right.ca.Fingerprint()))

	// The right server, under a name its certificate does not carry.
	_, port, _ := net.SplitHostPort(right.cl.NodeAddr())
	refused("a name the certificate lacks", net.JoinHostPort("localhost", port), "has a certificate from this cluster's CA, but it names 127.0.0.1, not localhost")
	// Another cluster's server under a name its certificate lacks fails
	// the name check first; it is still another CA's.
	_, otherPort, _ := net.SplitHostPort(other.cl.NodeAddr())
	refused("another cluster's server under another name", net.JoinHostPort("localhost", otherPort),
		fmt.Sprintf("not from this worker's cluster CA, fingerprint %x: it is another cluster's server", right.ca.Fingerprint()))

	// A server with a certificate from this cluster's CA, for the address,
	// that does not take this cluster's node certificates.
	refused("a server that refuses the worker", refusingServer(t, right.ca), "refused this node's certificate in the TLS handshake (remote error: tls: ")

	// Nothing listens.
	refused("an address where nothing listens", closedPort(t), "cannot reach a server at")

	// Something that is not a TLS server.
	plain := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(plain.Close)
	refused("a plain HTTP server", strings.TrimPrefix(plain.URL, "http://"), "did not answer as a hivemind server")

	// The join port of the right server, which has the right certificate
	// but no node API, and the default join port, refused before dialling.
	refused("the join port", right.cl.JoinAddr(), "it is the join port")
	refused("the default join port", "10.255.255.1:4485", "is the default join port, but set-server takes the server's cluster port, 10.255.255.1:4484")
	// A URL, or an address with a path, is refused before anything is
	// dialled, with the address to give.
	refused("a URL", "https://10.255.255.1", "https://10.255.255.1 is a URL: give the address without https://: 10.255.255.1")
	refused("a URL with a port and a path", "https://10.255.255.1:4484/node/v1", "give the address without https://: 10.255.255.1:4484")
	refused("an address with a path", "10.255.255.1/x", "10.255.255.1/x has a path: give the address without it: 10.255.255.1")

	// The right server, while the worker runs.
	release, err := lockDataDir(wdir)
	if err != nil {
		t.Fatal(err)
	}
	err = runSetServer(ctx, io.Discard, wdir, right.cl.NodeAddr())
	release()
	if err == nil || !strings.Contains(err.Error(), "another hivemind (pid "+strconv.Itoa(os.Getpid())+") runs on the data directory") ||
		!strings.Contains(err.Error(), "stop it (systemctl stop hivemind), run set-server, then start it again") {
		t.Errorf("set-server beside the running worker: %v", err)
	}
	if after := workerFiles(t, wdir); !sameFiles(before, after) {
		t.Error("set-server beside the running worker changed its files")
	}

	// The right server. Only the server address in node.json changes.
	var out bytes.Buffer
	if err := runSetServer(ctx, &out, wdir, right.cl.NodeAddr()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "now names "+right.cl.NodeAddr()+" (it named 10.0.0.5:4484)") {
		t.Errorf("set-server said: %s", out.String())
	}
	after := workerFiles(t, wdir)
	for name := range before {
		if name != workerStateFile && !bytes.Equal(before[name], after[name]) {
			t.Errorf("set-server changed %s", name)
		}
	}
	if _, ok := after[workerStateNext]; ok || len(after) != len(before) {
		t.Errorf("set-server left files behind: %v", keys(after))
	}
	w, err := loadWorkerState(wdir)
	if err != nil {
		t.Fatal(err)
	}
	old, _ := readWorkerStateBytes(before[workerStateFile])
	if w.state.Server != right.cl.NodeAddr() || w.state.NodeID != "node-w1" || !w.state.Joined.Equal(old.Joined) {
		t.Errorf("node.json after set-server: %+v, before: %+v", w.state, old)
	}
	if m := mode(t, filepath.Join(wdir, workerStateFile)); m != 0o600 {
		t.Errorf("node.json has mode %o", m)
	}
	// Again: nothing to change.
	out.Reset()
	if err := runSetServer(ctx, &out, wdir, right.cl.NodeAddr()); err != nil || !strings.Contains(out.String(), "nothing was changed") {
		t.Errorf("set-server to the server node.json names: %v, %s", err, out.String())
	}

	// The worker runs against the new address with its own certificate.
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- runWorker(rctx, &config.Config{DataDir: wdir, ReconcileInterval: 50 * time.Millisecond},
			slog.New(slog.NewTextHandler(io.Discard, nil)), newMemRuntime())
	}()
	waitUntil(t, "the worker registering with the new server", func() bool {
		_, err := right.st.GetNode(ctx, "node-w1")
		return err == nil
	})
	cancel()
	<-done

	// A removed node is refused by the right server, for good.
	if _, err := right.st.RemoveNode(ctx, "node-w1", true); err != nil {
		t.Fatal(err)
	}
	before = workerFiles(t, wdir)
	refused("the right server, after the node was removed", right.cl.NodeAddr(), "says node node-w1 was removed from the cluster")
}

// A server that advertises a name: set-server takes the name, which the
// certificate carries, and the address the server binds.
func TestSetServerToANamedServer(t *testing.T) {
	srv := newTestCluster(t, nil, "localhost")
	wdir := filepath.Join(t.TempDir(), "worker")
	saveJoined(t, wdir, srv.ca, "node-w1")
	_, port, _ := net.SplitHostPort(srv.cl.NodeAddr())
	for _, addr := range []string{net.JoinHostPort("localhost", port), srv.cl.NodeAddr()} {
		if err := runSetServer(context.Background(), io.Discard, wdir, addr); err != nil {
			t.Fatalf("set-server %s: %v", addr, err)
		}
		w, err := loadWorkerState(wdir)
		if err != nil || w.state.Server != addr {
			t.Fatalf("after set-server %s: %+v, %v", addr, w, err)
		}
	}
}

// set-server runs on a worker: a directory without one, or with a server,
// is refused and not created.
func TestSetServerNeedsAWorker(t *testing.T) {
	isolate(t)
	missing := filepath.Join(t.TempDir(), "none")
	_, _, err := run(t, "", "node", "set-server", "10.0.0.9", "--data-dir", missing)
	if err == nil || !strings.Contains(err.Error(), "this machine has not joined a cluster") {
		t.Errorf("set-server without a worker: %v", err)
	}
	if _, err := os.Stat(missing); err == nil {
		t.Error("set-server created the data directory")
	}
	server := t.TempDir()
	if _, _, err := run(t, "", "init", "--data-dir", server); err != nil {
		t.Fatal(err)
	}
	_, _, err = run(t, "", "node", "set-server", "10.0.0.9", "--data-dir", server)
	if err == nil || !strings.Contains(err.Error(), "holds a server, not a worker") {
		t.Errorf("set-server on the server: %v", err)
	}
}

// refusingServer is a TLS server on loopback with a certificate from ca for
// 127.0.0.1, that takes client certificates only from another CA. It
// returns its address.
func refusingServer(t *testing.T, ca *pki.CA) string {
	t.Helper()
	key, err := pki.NewNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	csr, err := pki.NewCSR(key)
	if err != nil {
		t.Fatal(err)
	}
	der, err := ca.IssueNodeCert(csr, pki.NodeCertOptions{NodeID: "node-mitm", Advertise: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	cert, err := pki.NewTLSCertificate(der, key, ca)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(newCA(t).Cert)
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool, MinVersion: tls.VersionTLS13}
	srv.Config.ErrorLog = stdlog.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "https://")
}

// closedPort is a loopback address where nothing listens.
func closedPort(t *testing.T) string {
	t.Helper()
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(unusedPort(t)))
}

func readWorkerStateBytes(b []byte) (workerState, error) {
	dir, err := os.MkdirTemp("", "nodejson")
	if err != nil {
		return workerState{}, err
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, workerStateFile), b, 0o600); err != nil {
		return workerState{}, err
	}
	return readWorkerState(dir)
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
