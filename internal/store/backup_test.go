package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/spec"
)

// seedCluster writes a cluster record so InspectFile has a node ID to find.
func seedCluster(t *testing.T, s *Store) {
	t.Helper()
	if err := s.InitCluster(ctxT(t), Cluster{AdminTokenHash: [32]byte{1}, NodeID: node1}); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	s := newT(t)
	seedCluster(t, s)
	mustPut(t, s, web(3))
	ctx := ctxT(t)
	dest := filepath.Join(t.TempDir(), "snap.db")
	if err := s.SnapshotTo(ctx, dest); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dest)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("snapshot mode %v, %v; want 0600", fi.Mode().Perm(), err)
	}
	info, err := InspectFile(ctx, dest)
	if err != nil || info.SchemaVersion != SchemaVersion || info.NodeID != node1 {
		t.Fatalf("InspectFile: %+v, %v", info, err)
	}
	// The snapshot opens as a store, with the same apps and replicas.
	r := openT(t, dest)
	reps, err := r.Replicas(ctx, "web")
	if err != nil || len(reps) != 3 {
		t.Fatalf("replicas in the snapshot: %d, %v", len(reps), err)
	}
	if err := s.SnapshotTo(ctx, dest); err == nil {
		t.Fatal("a snapshot overwrote an existing file")
	}
}

// SnapshotFile reads a store another connection has open, without migrating
// it or changing it, and refuses a path that is missing or not a store.
func TestSnapshotFileBesideAnOpenStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data", "hivemind.db")
	s := openT(t, path, WithMinReady(0))
	seedCluster(t, s)
	mustPut(t, s, web(2))
	ctx := ctxT(t)
	dest := filepath.Join(dir, "snap.db")
	if err := SnapshotFile(ctx, path, dest); err != nil {
		t.Fatal(err)
	}
	if info, err := InspectFile(ctx, dest); err != nil || info.NodeID != node1 {
		t.Fatalf("InspectFile: %+v, %v", info, err)
	}
	if err := SnapshotFile(ctx, filepath.Join(dir, "missing.db"), filepath.Join(dir, "x.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing store: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "missing.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("SnapshotFile created the missing store")
	}
	notStore := filepath.Join(dir, "plain.db")
	db, err := sql.Open("sqlite", notStore)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE t (x)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := SnapshotFile(ctx, notStore, filepath.Join(dir, "y.db")); !errors.Is(err, ErrNotStore) {
		t.Fatalf("a SQLite file that is not a store: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "junk.db"), []byte("not sqlite at all, not even close"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectFile(ctx, filepath.Join(dir, "junk.db")); !errors.Is(err, ErrNotStore) {
		t.Fatalf("a junk file: %v", err)
	}
}

// Snapshots taken while another goroutine writes, through the store and
// from a second connection, are each consistent: every one passes the
// store's invariants (replica rows match each app's count, specs exist for
// each generation, foreign keys hold).
func TestSnapshotDuringConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data", "hivemind.db")
	s := openT(t, path, WithMinReady(0))
	seedCluster(t, s)
	ctx := context.Background()
	if err := s.UpsertNode(ctx, Node{ID: node1, Name: "one", CPU: 64, Memory: 64 << 30}); err != nil {
		t.Fatal(err)
	}

	var stop atomic.Bool
	var writes atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; !stop.Load(); i++ {
			a := spec.App{Name: fmt.Sprintf("app-%d", i%7), Image: "nginx:alpine", Replicas: 1 + i%5,
				Env: map[string]string{"I": fmt.Sprint(i)}}
			if _, err := s.PutApp(ctx, a); err != nil {
				t.Errorf("PutApp: %v", err)
				return
			}
			if i%3 == 0 {
				if err := s.DeleteApp(ctx, a.Name); err != nil {
					t.Errorf("DeleteApp: %v", err)
					return
				}
			}
			writes.Add(1)
		}
	}()

	for i := range 20 {
		time.Sleep(5 * time.Millisecond)
		dest := filepath.Join(dir, fmt.Sprintf("snap-%d.db", i))
		var err error
		if i%2 == 0 {
			err = s.SnapshotTo(ctx, dest)
		} else {
			err = SnapshotFile(ctx, path, dest)
		}
		if err != nil {
			t.Fatal(err)
		}
		db, err := sql.Open("sqlite", dest)
		if err != nil {
			t.Fatal(err)
		}
		if err := invariants(ctx, db, SchemaVersion); err != nil {
			t.Errorf("snapshot %d is not consistent: %v", i, err)
		}
		db.Close()
	}
	stop.Store(true)
	wg.Wait()
	if writes.Load() < 10 {
		t.Fatalf("only %d writes ran beside the snapshots", writes.Load())
	}
}
