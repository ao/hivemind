package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/pki"
)

// A worker whose data directory is damaged refuses to start, and the error
// names the file, the node, and the two ways out: a backup, or a new join
// after the node is removed on the server.
func TestDamagedDataDirectorySaysWhatToDo(t *testing.T) {
	const id = "node-abc123"
	cases := []struct {
		name   string
		damage func(t *testing.T, dir string)
		want   string
	}{
		{"node.key missing", func(t *testing.T, dir string) { rm(t, dir, workerKeyFile) }, "node.key is missing"},
		{"node.key garbage", func(t *testing.T, dir string) { put(t, dir, workerKeyFile, "not a key", 0o600) }, "node.key is not a PEM EC private key"},
		{"node.json truncated", func(t *testing.T, dir string) { put(t, dir, workerStateFile, `{"node_id": "node-ab`, 0o600) }, "node.json is not valid"},
		{"node.json empty", func(t *testing.T, dir string) { put(t, dir, workerStateFile, "", 0o600) }, "node.json is not valid"},
		{"ca.crt replaced", func(t *testing.T, dir string) { put(t, dir, pki.CACertFile, string(newCA(t).CertPEM), 0o644) }, "node.crt does not chain to the cluster CA in ca.crt"},
		{"ca.crt missing", func(t *testing.T, dir string) { rm(t, dir, pki.CACertFile) }, "reading the cluster CA"},
		{"node.crt missing", func(t *testing.T, dir string) { rm(t, dir, workerCertFile) }, "reading the node's certificate"},
		{"node.key of another node", func(t *testing.T, dir string) {
			other := filepath.Join(t.TempDir(), "other")
			saveJoined(t, other, newCA(t), id)
			b, err := os.ReadFile(filepath.Join(other, workerKeyFile))
			if err != nil {
				t.Fatal(err)
			}
			put(t, dir, workerKeyFile, string(b), 0o600)
		}, "do not belong together"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "worker")
			saveJoined(t, dir, newCA(t), id)
			c.damage(t, dir)
			_, err := loadWorkerState(dir)
			if err == nil {
				t.Fatal("the damaged directory loaded")
			}
			msg := err.Error()
			for _, w := range []string{c.want, "data directory " + dir + " is damaged", "backup",
				"hivemind node remove " + id + " --force", "empty --data-dir and a new join token"} {
				if !strings.Contains(msg, w) {
					t.Errorf("message does not say %q:\n%s", w, msg)
				}
			}
		})
	}
}

// A certificate that is not valid yet means the clock is behind: the error
// says so, rather than calling the directory damaged.
func TestClockBehindTheCertificateSaysSo(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "worker")
	ca := newCA(t)
	key, err := pki.NewNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	csr, err := pki.NewCSR(key)
	if err != nil {
		t.Fatal(err)
	}
	// Issued by a server whose clock is 3 hours ahead of this machine's.
	der, err := ca.IssueNodeCert(csr, pki.NodeCertOptions{NodeID: "node-abc123", Now: time.Now().Add(3 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	st := workerState{NodeID: "node-abc123", Server: "10.0.0.5:4484", Joined: time.Now()}
	if err := saveWorkerState(dir, st, key, pemEncodeCert(der), ca.CertPEM); err != nil {
		t.Fatal(err)
	}
	_, err = loadWorkerState(dir)
	if err == nil || !strings.Contains(err.Error(), "this machine's clock reads") || !strings.Contains(err.Error(), "NTP") ||
		strings.Contains(err.Error(), "damaged") {
		t.Fatalf("certificate from the future: %v", err)
	}
}

func rm(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.Remove(filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
}

func put(t *testing.T, dir, name, data string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(data), mode); err != nil {
		t.Fatal(err)
	}
}
