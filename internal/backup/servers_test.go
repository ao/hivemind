package backup

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/ao/hivemind/internal/store"
)

// TestRestoreMakesAClusterOfOne: a backup of a server of a cluster of
// several restores as a cluster of one: no servers listed, and an applied
// index of 0 for the Raft log a later server join starts.
func TestRestoreMakesAClusterOfOne(t *testing.T) {
	s := newServer(t)
	ctx := context.Background()
	for _, sv := range []store.Server{{ID: serverNode, Advertise: "10.0.0.5", NodePort: 4484, RaftAddr: "10.0.0.5:4486"},
		{ID: "node-s2", Advertise: "10.0.0.6", NodePort: 4484, RaftAddr: "10.0.0.6:4486"}} {
		if err := s.st.PutServer(ctx, sv); err != nil {
			t.Fatal(err)
		}
	}
	var b bytes.Buffer
	if _, err := Write(ctx, &b, Options{DataDir: s.dir, Snapshot: editSnapshot(s.st, `UPDATE raft_applied SET idx = 4242`)}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "r")
	if _, err := Restore(ctx, &b, dir); err != nil {
		t.Fatal(err)
	}
	r, err := store.Open(ctx, filepath.Join(dir, StoreName))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if n, err := r.CountServers(ctx); err != nil || n != 0 {
		t.Fatalf("the restored store lists %d servers, %v", n, err)
	}
	if idx, err := r.AppliedIndex(ctx); err != nil || idx != 0 {
		t.Fatalf("the restored store's applied index is %d, %v", idx, err)
	}
}
