package nodeapi

import (
	"encoding/pem"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ao/hivemind/internal/pki"
)

// joinReplyFor builds the reply a server would send for node id, signed by ca.
func joinReplyFor(t *testing.T, ca *pki.CA, id string) JoinReply {
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
	return JoinReply{NodeID: id, CertPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		CAPEM: string(ca.CertPEM)}
}

// The worker accepts a reply only when its CA has the token's fingerprint, its
// certificate chains to that CA, and the certificate names the reply's node.
func TestCheckJoinReply(t *testing.T) {
	ca, err := pki.CreateCA(filepath.Join(t.TempDir(), "ca"))
	if err != nil {
		t.Fatal(err)
	}
	fp := ca.Fingerprint()
	good := joinReplyFor(t, ca, "node-abc123")
	if err := checkJoinReply(good, fp); err != nil {
		t.Fatalf("good reply: %v", err)
	}

	other, err := pki.CreateCA(filepath.Join(t.TempDir(), "ca"))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		reply JoinReply
		fp    [32]byte
		want  string
	}{
		"CA is not the token's": {joinReplyFor(t, other, "node-abc123"), fp, "fingerprint"},
		"node ID differs":       {JoinReply{NodeID: "node-other", CertPEM: good.CertPEM, CAPEM: good.CAPEM}, fp, "names"},
		"no CA":                 {JoinReply{NodeID: "node-abc123", CertPEM: good.CertPEM}, fp, "CA certificate"},
		"no certificate":        {JoinReply{NodeID: "node-abc123", CAPEM: good.CAPEM}, fp, "node certificate"},
		"certificate from another CA": {
			JoinReply{NodeID: "node-abc123", CertPEM: joinReplyFor(t, other, "node-abc123").CertPEM, CAPEM: good.CAPEM},
			fp, "chain",
		},
	}
	for name, tc := range cases {
		err := checkJoinReply(tc.reply, tc.fp)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want an error mentioning %q", name, err, tc.want)
		}
	}
}
