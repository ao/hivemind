package store

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
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

// The cluster record starts at epoch 0, from init. BumpEpochFile raises it
// by one in a closed store file, without migrating it, and RaiseEpoch to at
// least a given epoch. A store without a record, or from before epochs, has
// none to raise.
func TestClusterEpoch(t *testing.T) {
	ctx := ctxT(t)
	path := filepath.Join(t.TempDir(), "c.db")
	s := openT(t, path)
	if _, err := s.RaiseEpoch(ctx, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("RaiseEpoch before init: %v, want ErrNotFound", err)
	}
	s.Close()
	if _, err := BumpEpochFile(ctx, path); !errors.Is(err, ErrNotFound) {
		t.Fatalf("BumpEpochFile before init: %v, want ErrNotFound", err)
	}
	s = openT(t, path)
	if err := s.InitCluster(ctx, Cluster{AdminTokenHash: sha256.Sum256([]byte("t")), NodeID: "node-a", Epoch: 7}); err != nil {
		t.Fatal(err)
	}
	if c, err := s.GetCluster(ctx); err != nil || c.Epoch != 0 {
		t.Fatalf("after init: %+v, %v; want epoch 0", c, err)
	}
	s.Close()
	for want := int64(1); want <= 2; want++ {
		if got, err := BumpEpochFile(ctx, path); err != nil || got != want {
			t.Fatalf("BumpEpochFile: %d, %v; want %d", got, err, want)
		}
	}
	s = openT(t, path)
	if c, err := s.GetCluster(ctx); err != nil || c.Epoch != 2 || c.NodeID != "node-a" {
		t.Fatalf("reopened: %+v, %v; want epoch 2", c, err)
	}
	for _, c := range []struct{ to, want int64 }{{5, 5}, {3, 5}, {5, 5}} {
		if got, err := s.RaiseEpoch(ctx, c.to); err != nil || got != c.want {
			t.Fatalf("RaiseEpoch(%d): %d, %v; want %d", c.to, got, err, c.want)
		}
	}

	// A store from before epochs: BumpEpochFile leaves it as it is.
	old := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", old)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, db, migrations[:EpochSchemaVersion-1]); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO cluster (id, admin_token_hash, node_id, created_at) VALUES (1, zeroblob(32), 'node-1', 100)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := BumpEpochFile(ctx, old); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "schema version 15 has no epoch") {
		t.Fatalf("BumpEpochFile on a version 15 store: %v", err)
	}
	if info, err := InspectFile(ctx, old); err != nil || info.SchemaVersion != EpochSchemaVersion-1 {
		t.Fatalf("the version 15 store after BumpEpochFile: %+v, %v", info, err)
	}
}
