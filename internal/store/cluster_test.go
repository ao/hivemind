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
