package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/store"
)

func TestSplitServer(t *testing.T) {
	cases := []struct {
		in   string
		host string
		port int
		err  bool
	}{
		{"10.0.0.5", "10.0.0.5", 4484, false},
		{"10.0.0.5:5000", "10.0.0.5", 5000, false},
		{"node-a:4484", "node-a", 4484, false},
		{"[fd00::5]", "fd00::5", 4484, false},
		{"[fd00::5]:4500", "fd00::5", 4500, false},
		{"10.0.0.5:0", "", 0, true},
		{"10.0.0.5:65535", "", 0, true}, // the join port would be 65536
		{":4484", "", 0, true},
		{"10.0.0.5:abc", "", 0, true},
	}
	for _, c := range cases {
		host, port, err := splitServer(c.in)
		if c.err {
			if err == nil {
				t.Errorf("%q: no error, want one", c.in)
			}
			continue
		}
		if err != nil || host != c.host || port != c.port {
			t.Errorf("%q: got %q %d %v, want %q %d", c.in, host, port, err, c.host, c.port)
		}
	}
}

// pemEncodeCert wraps DER certificate bytes as PEM.
func pemEncodeCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// issueForNode returns a key and a certificate for node id, signed by ca.
func issueForNode(t *testing.T, ca *pki.CA, id string) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := pki.NewNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	csr, err := pki.NewCSR(key)
	if err != nil {
		t.Fatal(err)
	}
	der, err := ca.IssueNodeCert(csr, pki.NodeCertOptions{NodeID: id})
	if err != nil {
		t.Fatal(err)
	}
	return key, der
}

func newCA(t *testing.T) *pki.CA {
	t.Helper()
	ca, err := pki.CreateCA(filepath.Join(t.TempDir(), "ca"))
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

// saveJoined writes a worker's files for node id, as enroll does.
func saveJoined(t *testing.T, dir string, ca *pki.CA, id string) {
	t.Helper()
	key, der := issueForNode(t, ca, id)
	st := workerState{NodeID: id, Server: "10.0.0.5:4484", Joined: time.Now()}
	certPEM := pemEncodeCert(der)
	if err := saveWorkerState(dir, st, key, certPEM, ca.CertPEM); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerStateRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "worker")
	ca := newCA(t)
	saveJoined(t, dir, ca, "node-abc123")
	w, err := loadWorkerState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if w.state.NodeID != "node-abc123" || w.state.Server != "10.0.0.5:4484" {
		t.Fatalf("state %+v", w.state)
	}
	if w.tlsCfg == nil || len(w.tlsCfg.Certificates) != 1 {
		t.Fatal("no client certificate in the loaded config")
	}
	for _, name := range []string{workerKeyFile, workerCertFile, pki.CACertFile, workerStateFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The key is written with mode 0600.
func TestWorkerKeyIsPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "worker")
	saveJoined(t, dir, newCA(t), "node-abc123")
	info, err := os.Stat(filepath.Join(dir, workerKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %04o, want 0600", info.Mode().Perm())
	}
}

func TestLoadRefusesAWidePrivateKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "worker")
	saveJoined(t, dir, newCA(t), "node-abc123")
	if err := os.Chmod(filepath.Join(dir, workerKeyFile), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadWorkerState(dir); err == nil || !strings.Contains(err.Error(), "readable by its owner") {
		t.Fatalf("wide key: %v", err)
	}
}

func TestLoadRefusesMismatchedNodeID(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "worker")
	saveJoined(t, dir, newCA(t), "node-abc123")
	// node.json names another node than the certificate does.
	b, err := json.Marshal(workerState{NodeID: "node-other", Server: "10.0.0.5:4484"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, workerStateFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadWorkerState(dir); err == nil || !strings.Contains(err.Error(), "names") {
		t.Fatalf("mismatched node ID: %v", err)
	}
}

func TestLoadRefusesCertificateFromAnotherCA(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "worker")
	ca := newCA(t)
	saveJoined(t, dir, ca, "node-abc123")
	other := newCA(t)
	_, der := issueForNode(t, other, "node-abc123")
	if err := os.WriteFile(filepath.Join(dir, workerCertFile), pemEncodeCert(der), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadWorkerState(dir); err == nil || !strings.Contains(err.Error(), "cluster CA") {
		t.Fatalf("certificate from another CA: %v", err)
	}
}

func TestLoadNotJoined(t *testing.T) {
	_, err := loadWorkerState(t.TempDir())
	if !errors.Is(err, errNotJoined) {
		t.Fatalf("got %v, want errNotJoined", err)
	}
}

// A directory that already holds a joined node is refused before the token is
// used, so a second join never burns a token it cannot use.
func TestEnrollRefusesAJoinedDirectoryBeforeTheNetwork(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "worker")
	saveJoined(t, dir, newCA(t), "node-abc123")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := enroll(context.Background(), log, dir, "127.0.0.1", 1, "hivemind-join-v1.x")
	if err == nil || !strings.Contains(err.Error(), "already holds a joined node") {
		t.Fatalf("enroll in a joined directory: %v", err)
	}
}

// A save never overwrites: the second save fails and leaves the first set.
func TestSaveDoesNotOverwrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "worker")
	ca := newCA(t)
	saveJoined(t, dir, ca, "node-abc123")
	key, der := issueForNode(t, ca, "node-zzz999")
	err := saveWorkerState(dir, workerState{NodeID: "node-zzz999", Server: "x:1"}, key, pemEncodeCert(der), ca.CertPEM)
	if err == nil {
		t.Fatal("second save succeeded over the first")
	}
	w, err := loadWorkerState(dir)
	if err != nil || w.state.NodeID != "node-abc123" {
		t.Fatalf("first state changed: %v %+v", err, w)
	}
}

// After ErrNotRegistered the worker registers again and reports once more.
func TestWorkerSourceRegistersAgainWhenTheServerForgotIt(t *testing.T) {
	c := &fakeWorkerClient{reportErrs: []error{nodeapi.ErrNotRegistered}}
	registers := 0
	src := workerSource{client: c, register: func(context.Context) error { registers++; return nil }}
	if err := src.ReportStatus(context.Background(), "node-a", nil); err != nil {
		t.Fatalf("report after re-register: %v", err)
	}
	if registers != 1 || c.reports != 2 {
		t.Fatalf("registers %d, reports %d; want 1 and 2", registers, c.reports)
	}
}

// Other errors are passed on without a register.
func TestWorkerSourcePassesOtherErrorsThrough(t *testing.T) {
	boom := errors.New("connection refused")
	c := &fakeWorkerClient{reportErrs: []error{boom}}
	registers := 0
	src := workerSource{client: c, register: func(context.Context) error { registers++; return nil }}
	if err := src.ReportStatus(context.Background(), "node-a", nil); !errors.Is(err, boom) {
		t.Fatalf("got %v, want the original error", err)
	}
	if registers != 0 {
		t.Fatalf("registered %d times on an unrelated error", registers)
	}
}

type fakeWorkerClient struct {
	reportErrs []error
	reports    int
}

func (f *fakeWorkerClient) AssignmentsForNode(context.Context, string) ([]store.Assignment, error) {
	return nil, nil
}

func (f *fakeWorkerClient) ReportStatus(context.Context, string, []store.Status) error {
	f.reports++
	if len(f.reportErrs) == 0 {
		return nil
	}
	err := f.reportErrs[0]
	f.reportErrs = f.reportErrs[1:]
	return err
}

func (f *fakeWorkerClient) StatusForNode(context.Context, string) ([]store.Status, error) {
	return nil, nil
}
