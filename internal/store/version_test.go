package store

import (
	"errors"
	"strings"
	"testing"
)

// A node's version is stored at registration and by SetNodeVersion, and a
// registration without one clears it, as after a downgrade.
func TestNodeVersion(t *testing.T) {
	s := newT(t)
	ctx := ctxT(t)
	version := func() string {
		t.Helper()
		n, err := s.GetNode(ctx, "node-x")
		if err != nil {
			t.Fatal(err)
		}
		return n.Version
	}
	if err := s.UpsertNode(ctx, Node{ID: "node-x", Name: "x", Version: "v2.2.0"}); err != nil {
		t.Fatal(err)
	}
	if v := version(); v != "v2.2.0" {
		t.Fatalf("version after register: %q", v)
	}
	if err := s.SetNodeVersion(ctx, "node-x", "v2.2.1-3-gabcdef"); err != nil {
		t.Fatal(err)
	}
	if v := version(); v != "v2.2.1-3-gabcdef" {
		t.Fatalf("version after SetNodeVersion: %q", v)
	}
	if err := s.UpsertNode(ctx, Node{ID: "node-x", Name: "x"}); err != nil {
		t.Fatal(err)
	}
	if v := version(); v != "" {
		t.Fatalf("version after a register with none: %q", v)
	}

	for _, bad := range []string{"v2 2", "v2\n", strings.Repeat("v", MaxNodeVersionLen+1), "v2.2.0\x00"} {
		if err := s.SetNodeVersion(ctx, "node-x", bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("SetNodeVersion(%q): %v, want ErrInvalid", bad, err)
		}
		if err := s.UpsertNode(ctx, Node{ID: "node-x", Name: "x", Version: bad}); !errors.Is(err, ErrInvalid) {
			t.Errorf("UpsertNode with version %q: %v, want ErrInvalid", bad, err)
		}
	}
	if err := s.SetNodeVersion(ctx, "node-nope", "v2.2.0"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetNodeVersion of a node that is not registered: %v", err)
	}
}
