package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ao/hivemind/internal/pki"
)

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

// init on a worker refuses before it creates anything, and says to run the
// CLI on the server. So does serve without --role worker.
func TestInitOnAWorker(t *testing.T) {
	home := isolate(t)
	dir := filepath.Join(home, "worker")
	saveJoined(t, dir, newCA(t), "node-abc123")
	before := dirNames(t, dir)

	_, _, err := run(t, "", "init", "--data-dir", dir)
	if err == nil || !strings.Contains(err.Error(), "this machine is worker node-abc123 of the cluster at 10.0.0.5:4484") ||
		!strings.Contains(err.Error(), "run hivemind commands on the server, 10.0.0.5") {
		t.Fatalf("init on a worker: %v", err)
	}
	if after := dirNames(t, dir); !slices.Equal(before, after) {
		t.Fatalf("init on a worker changed its data directory: %v, then %v", before, after)
	}
	_, _, err = run(t, "", "serve", "--data-dir", dir)
	if err == nil || !strings.Contains(err.Error(), "holds worker node-abc123") || !strings.Contains(err.Error(), "serve --role worker") {
		t.Fatalf("serve on a worker: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, storeFileName)); err == nil {
		t.Fatal("serve on a worker created a store")
	}
}

// A data directory with a CA but no token is refused before the store is
// created.
func TestInitOverACALeavesNoStore(t *testing.T) {
	home := isolate(t)
	dir := filepath.Join(home, "d")
	if _, err := pki.CreateCA(dir); err != nil {
		t.Fatal(err)
	}
	_, _, err := run(t, "", "init", "--data-dir", dir)
	if err == nil || !strings.Contains(err.Error(), "a cluster CA already exists") {
		t.Fatalf("init over a CA: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, storeFileName)); err == nil {
		t.Fatal("the refused init created a store")
	}
}

// The CLI on a worker, which has no token, says to run it on the server
// rather than to run init.
func TestCLIOnAWorker(t *testing.T) {
	home := isolate(t)
	dir := filepath.Join(home, "worker")
	saveJoined(t, dir, newCA(t), "node-abc123")
	t.Setenv("HIVEMIND_DATA_DIR", dir)
	for _, args := range [][]string{{"status"}, {"nodes"}, {"deploy", "--name", "web", "--image", "nginx"}} {
		_, _, err := run(t, "", args...)
		if err == nil || !strings.Contains(err.Error(), "this machine is worker node-abc123") ||
			!strings.Contains(err.Error(), "run hivemind commands on the server, 10.0.0.5") || strings.Contains(err.Error(), "hivemind init") {
			t.Errorf("%v on a worker: %v", args, err)
		}
	}
	// With a token, the CLI does not care where it runs.
	if _, _, err := run(t, "", "status", "--server", "http://127.0.0.1:1", "--token", "hm_x"); err == nil ||
		strings.Contains(err.Error(), "worker") {
		t.Errorf("status with a token on a worker: %v", err)
	}
}
