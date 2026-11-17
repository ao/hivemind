package store

import (
	"crypto/sha256"
	"errors"
	"path/filepath"
	"testing"
)

func TestClusterRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.db")
	s := openT(t, path)
	ctx := ctxT(t)

	if _, err := s.GetCluster(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty store: %v, want ErrNotFound", err)
	}
	if err := s.InitCluster(ctx, Cluster{NodeID: "n"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("zero hash: %v, want ErrInvalid", err)
	}
	want := Cluster{AdminTokenHash: sha256.Sum256([]byte("t")), NodeID: "node-a"}
	if err := s.InitCluster(ctx, want); err != nil {
		t.Fatal(err)
	}
	other := Cluster{AdminTokenHash: sha256.Sum256([]byte("u")), NodeID: "node-b"}
	if err := s.InitCluster(ctx, other); !errors.Is(err, ErrClusterExists) {
		t.Fatalf("second init: %v, want ErrClusterExists", err)
	}
	s.Close()

	s = openT(t, path)
	got, err := s.GetCluster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.AdminTokenHash != want.AdminTokenHash || got.NodeID != want.NodeID || got.CreatedAt.IsZero() {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// The cluster record starts at epoch 0, from init, and BumpEpoch raises it
// by one at each call, for good. A store without a record has no epoch to
// raise.
func TestClusterEpoch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.db")
	s := openT(t, path)
	ctx := ctxT(t)
	if _, err := s.BumpEpoch(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("BumpEpoch before init: %v, want ErrNotFound", err)
	}
	if err := s.InitCluster(ctx, Cluster{AdminTokenHash: sha256.Sum256([]byte("t")), NodeID: "node-a", Epoch: 7}); err != nil {
		t.Fatal(err)
	}
	if c, err := s.GetCluster(ctx); err != nil || c.Epoch != 0 {
		t.Fatalf("after init: %+v, %v; want epoch 0", c, err)
	}
	for want := int64(1); want <= 2; want++ {
		if got, err := s.BumpEpoch(ctx); err != nil || got != want {
			t.Fatalf("BumpEpoch: %d, %v; want %d", got, err, want)
		}
	}
	s.Close()
	s = openT(t, path)
	if c, err := s.GetCluster(ctx); err != nil || c.Epoch != 2 || c.NodeID != "node-a" {
		t.Fatalf("reopened: %+v, %v; want epoch 2", c, err)
	}
}
