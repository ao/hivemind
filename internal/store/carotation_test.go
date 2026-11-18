package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestCARotationRecord(t *testing.T) {
	s := newT(t)
	ctx := ctxT(t)
	cur, next := [32]byte{1}, [32]byte{2}
	if _, err := s.GetCARotation(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no rotation: %v, want ErrNotFound", err)
	}
	// A note without a rotation leaves nothing.
	if err := s.NoteCARotationNode(ctx, CARotationNode{NodeID: "node-a", Trusts: true}); err != nil {
		t.Fatal(err)
	}
	if ns, err := s.CARotationNodes(ctx); err != nil || len(ns) != 0 {
		t.Fatalf("nodes without a rotation: %v %v", ns, err)
	}
	if _, err := s.StartCARotation(ctx, cur, cur); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a rotation to the same CA: %v", err)
	}
	r, err := s.StartCARotation(ctx, cur, next)
	if err != nil {
		t.Fatal(err)
	}
	if r.Phase != CAPhaseTrust || r.Toward != CANext || r.Listener != CACurrent {
		t.Fatalf("started %+v", r)
	}
	if _, err := s.StartCARotation(ctx, cur, [32]byte{3}); !errors.Is(err, ErrCARotationExists) {
		t.Fatalf("second start: %v, want ErrCARotationExists", err)
	}
	got, err := s.GetCARotation(ctx)
	if err != nil || got.CurrentFP != cur || got.NextFP != next || got.Phase != CAPhaseTrust || got.StartedAt.IsZero() {
		t.Fatalf("GetCARotation: %+v %v", got, err)
	}

	for _, n := range []CARotationNode{{NodeID: "node-a", Trusts: true}, {NodeID: "node-b", OldBuild: true},
		{NodeID: "node-a", Trusts: true, Renewed: true}} {
		if err := s.NoteCARotationNode(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	ns, err := s.CARotationNodes(ctx)
	if err != nil || len(ns) != 2 || !ns["node-a"].Renewed || !ns["node-a"].Trusts || !ns["node-b"].OldBuild || ns["node-b"].Trusts {
		t.Fatalf("nodes: %+v %v", ns, err)
	}

	got.Phase, got.Listener = CAPhaseRenew, CANext
	if err := s.SetCARotation(ctx, got, false); err != nil {
		t.Fatal(err)
	}
	if ns, _ := s.CARotationNodes(ctx); len(ns) != 2 {
		t.Fatal("SetCARotation without reset lost the nodes")
	}
	got.Toward = CACurrent
	if err := s.SetCARotation(ctx, got, true); err != nil {
		t.Fatal(err)
	}
	if ns, _ := s.CARotationNodes(ctx); len(ns) != 0 {
		t.Fatal("SetCARotation with reset kept the nodes")
	}
	again, _ := s.GetCARotation(ctx)
	if again.Phase != CAPhaseRenew || again.Toward != CACurrent || again.Listener != CANext {
		t.Fatalf("after SetCARotation: %+v", again)
	}
	other := got
	other.NextFP = [32]byte{9}
	if err := s.SetCARotation(ctx, other, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetCARotation for other CAs: %v, want ErrNotFound", err)
	}
	bad := got
	bad.Phase = "done"
	if err := s.SetCARotation(ctx, bad, false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad phase: %v", err)
	}

	// The record is in a snapshot, and InspectFile reads it.
	seedCluster(t, s)
	dest := filepath.Join(t.TempDir(), "snap.db")
	if err := s.SnapshotTo(ctx, dest); err != nil {
		t.Fatal(err)
	}
	info, err := InspectFile(ctx, dest)
	if err != nil || info.CARotation == nil || info.CARotation.NextFP != next || info.CARotation.Toward != CACurrent {
		t.Fatalf("InspectFile: %+v %v", info, err)
	}

	if err := s.NoteCARotationNode(ctx, CARotationNode{NodeID: "node-c"}); err != nil {
		t.Fatal(err)
	}
	if err := s.EndCARotation(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetCARotation(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after end: %v", err)
	}
	if ns, _ := s.CARotationNodes(ctx); len(ns) != 0 {
		t.Fatal("EndCARotation kept the nodes")
	}
	if err := s.EndCARotation(ctx); err != nil {
		t.Fatalf("ending no rotation: %v", err)
	}
	// A new rotation starts with no nodes seen.
	if err := s.NoteCARotationNode(ctx, CARotationNode{NodeID: "node-d"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartCARotation(ctx, next, cur); err != nil {
		t.Fatal(err)
	}
	if ns, _ := s.CARotationNodes(ctx); len(ns) != 0 {
		t.Fatalf("a new rotation has nodes: %v", ns)
	}
	if info, err := InspectFile(ctx, dest); err != nil || info.CARotation == nil {
		t.Fatalf("InspectFile of the earlier snapshot: %v", err)
	}
}

func TestInspectFileWithoutRotation(t *testing.T) {
	s := newT(t)
	seedCluster(t, s)
	ctx := ctxT(t)
	dest := filepath.Join(t.TempDir(), "snap.db")
	if err := s.SnapshotTo(ctx, dest); err != nil {
		t.Fatal(err)
	}
	info, err := InspectFile(ctx, dest)
	if err != nil || info.CARotation != nil {
		t.Fatalf("InspectFile: %+v %v", info, err)
	}
}
