package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/carotate"
)

func TestPrintCAStatus(t *testing.T) {
	cur := &carotate.CAInfo{Fingerprint: strings.Repeat("a", 64), Name: "hivemind cluster CA", NotAfter: time.Date(2036, 1, 2, 0, 0, 0, 0, time.UTC)}
	next := &carotate.CAInfo{Fingerprint: strings.Repeat("b", 64), Name: "hivemind cluster CA bbbbbbbb", NotAfter: time.Date(2036, 11, 1, 0, 0, 0, 0, time.UTC)}

	var out bytes.Buffer
	printCAStatus(&out, carotate.Status{Current: cur, Listener: cur.Fingerprint})
	for _, want := range []string{"Cluster CA (ca.crt): hivemind cluster CA, fingerprint " + cur.Fingerprint + ", valid until 2036-01-02",
		"The server's certificate is from the cluster CA", "No rotation is in progress."} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("no %q in:\n%s", want, out.String())
		}
	}

	out.Reset()
	printCAStatus(&out, carotate.Status{Current: cur, Next: next, Listener: next.Fingerprint,
		Rotation: &carotate.RotationInfo{Phase: "renew", Toward: "next", NextStep: "finish", StartedAt: time.Now(),
			Waiting: []carotate.Waiting{{Node: "node-old", Name: "box\x1b[2J", Reason: "is a build without CA rotation (v2.1.0): upgrade it, or remove it"}}},
		Nodes: []carotate.NodeStatus{
			{ID: "node-a", Name: "a", Version: "v2.2.0", Live: true, Seen: true, Trusts: true, Renewed: true},
			{ID: "node-old", Name: "box", Version: "v2.1.0", Live: true, Seen: true, OldBuild: true},
			{ID: "node-down", Name: "down"},
		}})
	s := out.String()
	for _, want := range []string{"New CA (ca-next.crt): hivemind cluster CA bbbbbbbb", "The server's certificate is from the new CA",
		"Rotation: phase renew, towards the new CA", "Next: hivemind ca rotate finish, which waits for:",
		"node-old (box\\x1b[2J) is a build without CA rotation", "no (old build)"} {
		if !strings.Contains(s, want) {
			t.Fatalf("no %q in:\n%s", want, s)
		}
	}
	if strings.Contains(s, "\x1b") {
		t.Fatal("a control character reached the terminal")
	}
	lines := strings.Split(s, "\n")
	var a, down string
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "node-a "):
			a = l
		case strings.HasPrefix(l, "node-down "):
			down = l
		}
	}
	if !strings.Contains(a, "live") || !strings.Contains(a, "new CA") || !strings.Contains(down, "lost") || !strings.Contains(down, " -") {
		t.Fatalf("node rows:\n%s", s)
	}
}
