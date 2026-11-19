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
		Rotation: &carotate.RotationInfo{Phase: "renew", Toward: "next", From: cur.Fingerprint, To: next.Fingerprint, NextStep: "finish", StartedAt: time.Now(),
			Waiting: []carotate.Waiting{{Node: "node-old", Name: "box\x1b[2J", Reason: "is a build without CA rotation (v2.1.0): upgrade it, or remove it"}}},
		Nodes: []carotate.NodeStatus{
			{ID: "node-a", Name: "a", Version: "v2.2.0", Live: true, Seen: true, Trusts: true, Renewed: true},
			{ID: "node-old", Name: "box", Version: "v2.1.0", Live: true, Seen: true, OldBuild: true},
			{ID: "node-down", Name: "down"},
		}})
	s := out.String()
	for _, want := range []string{"New CA (ca-next.crt): hivemind cluster CA bbbbbbbb", "The server's certificate is from the new CA",
		"Rotation: phase renew, towards the new CA, ca-next.crt bbbbbbbb, started", "TRUSTS TARGET", "Next: hivemind ca rotate finish, which waits for:",
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
	if !strings.Contains(a, "live") || !strings.Contains(a, "ca-next.crt bbbbbbbb") || !strings.Contains(down, "lost") || !strings.Contains(down, " -") {
		t.Fatalf("node rows:\n%s", s)
	}
}

// After an abort turned a rotation round, ca status names the CA it goes
// back to as its target, by file and fingerprint, and the CA of each
// node's certificate the same way. In the fourth acceptance run the
// columns, named new and old, said the nodes trusted the new CA and held
// its certificates when they had renewed back under the old one.
func TestPrintCAStatusTurnedRound(t *testing.T) {
	cur := &carotate.CAInfo{Fingerprint: strings.Repeat("a", 64), Name: "hivemind cluster CA", NotAfter: time.Date(2036, 1, 2, 0, 0, 0, 0, time.UTC)}
	next := &carotate.CAInfo{Fingerprint: strings.Repeat("b", 64), Name: "hivemind cluster CA bbbbbbbb", NotAfter: time.Date(2036, 11, 1, 0, 0, 0, 0, time.UTC)}
	var out bytes.Buffer
	printCAStatus(&out, carotate.Status{Current: cur, Next: next, Listener: next.Fingerprint,
		Rotation: &carotate.RotationInfo{Phase: "renew", Toward: "current", From: next.Fingerprint, To: cur.Fingerprint, NextStep: "finish",
			StartedAt: time.Now(), Waiting: []carotate.Waiting{{Node: "node-b", Reason: "has not renewed its certificate back under the old CA yet"}}},
		Nodes: []carotate.NodeStatus{
			{ID: "node-a", Name: "a", Version: "v2.2.0", Live: true, Seen: true, Trusts: true, Renewed: true},
			{ID: "node-b", Name: "b", Version: "v2.2.0", Live: true, Seen: true, Trusts: true},
		}})
	s := out.String()
	for _, want := range []string{"Rotation: phase renew, back towards the old CA, ca.crt aaaaaaaa, after an abort, started",
		"TRUSTS TARGET", "node-b has not renewed its certificate back under the old CA yet"} {
		if !strings.Contains(s, want) {
			t.Fatalf("no %q in:\n%s", want, s)
		}
	}
	for _, l := range strings.Split(s, "\n") {
		switch {
		case strings.HasPrefix(l, "node-a "):
			if !strings.Contains(l, "yes") || !strings.HasSuffix(strings.TrimSpace(l), "ca.crt aaaaaaaa") {
				t.Errorf("node-a, renewed back under the old CA: %q", l)
			}
		case strings.HasPrefix(l, "node-b "):
			if !strings.HasSuffix(strings.TrimSpace(l), "ca-next.crt bbbbbbbb") {
				t.Errorf("node-b, still on the new CA: %q", l)
			}
		}
	}
	if strings.Contains(s, "TRUSTS NEW") || strings.Contains(s, "new CA,") {
		t.Errorf("the turned rotation reads as towards the new CA:\n%s", s)
	}
	if got := rotationDirection(&carotate.RotationInfo{Toward: "current"}); got != ", back towards the old CA in ca.crt after an abort" {
		t.Errorf("hivemind status's direction: %q", got)
	}
	if got := rotationDirection(&carotate.RotationInfo{Toward: "next"}); got != "" {
		t.Errorf("hivemind status's direction towards the new CA: %q", got)
	}
}
