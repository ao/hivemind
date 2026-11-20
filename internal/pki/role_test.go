package pki

import (
	"testing"
	"time"
)

// TestServerRole: a certificate issued with Server carries the role beside
// its node ID, which still reads as before; one without has no role.
func TestServerRole(t *testing.T) {
	ca := newCA(t)
	key, err := NewNodeKey()
	if err != nil {
		t.Fatal(err)
	}
	csr, err := NewCSR(key)
	if err != nil {
		t.Fatal(err)
	}
	der, err := ca.IssueNodeCert(csr, NodeCertOptions{NodeID: "node-s1", Advertise: "10.0.0.5", Server: true})
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewTLSCertificate(der, key, ca)
	if err != nil {
		t.Fatal(err)
	}
	if !HasServerRole(server.Leaf) {
		t.Fatal("a server's certificate has no server role")
	}
	if id, err := NodeIDFromCert(server.Leaf); err != nil || id != "node-s1" {
		t.Fatalf("node ID of a server's certificate: %q, %v", id, err)
	}
	worker := nodeCert(t, ca, "node-w1", "", time.Time{}, 0)
	if HasServerRole(worker.Leaf) {
		t.Fatal("a worker's certificate has the server role")
	}
}
