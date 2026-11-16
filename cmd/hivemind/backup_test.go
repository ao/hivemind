package main

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/auth"
	"github.com/ao/hivemind/internal/backup"
	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/server"
	"github.com/ao/hivemind/internal/store"
)

func TestBackupAndRestoreCommands(t *testing.T) {
	home := isolate(t)
	dir := filepath.Join(home, "server")
	if _, _, err := run(t, "", "init", "--data-dir", dir); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(home, "b.tar")
	stdout, stderr, err := run(t, "", "backup", out, "--data-dir", dir)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if !strings.Contains(stdout, "Wrote "+out) || !strings.Contains(stderr, "private key") {
		t.Fatalf("backup output: %q / %q", stdout, stderr)
	}
	if m := mode(t, out); m != 0o600 {
		t.Fatalf("backup file mode %04o, want 0600", m)
	}
	ents, _ := os.ReadDir(home)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".hivemind-backup-") {
			t.Fatalf("temporary file %s left behind", e.Name())
		}
	}
	if _, _, err := run(t, "", "backup", out, "--data-dir", dir); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("backup over an existing file: %v", err)
	}
	if _, _, err := run(t, "", "backup", filepath.Join(home, "x.tar"), "--data-dir", filepath.Join(home, "none")); err == nil {
		t.Fatal("backup of a missing data directory succeeded")
	}

	// Restore on a "new machine": another home, so init's client config is
	// not there.
	home2 := isolate(t)
	target := filepath.Join(home2, "restored")
	stdout, _, err = run(t, "", "restore", out, "--data-dir", target)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !strings.Contains(stdout, "Restored") || !strings.Contains(stdout, "client config") && !strings.Contains(stdout, "reads the server address") {
		t.Fatalf("restore output: %q", stdout)
	}
	if m := mode(t, target); m != 0o700 {
		t.Fatalf("restored data directory mode %04o", m)
	}
	for _, name := range []string{storeFileName, pki.CAKeyFile, pki.CACertFile, auth.TokenFileName} {
		if m := mode(t, filepath.Join(target, name)); m != 0o600 {
			t.Errorf("%s mode %04o, want 0600", name, m)
		}
	}
	origCA, _ := pki.LoadCA(dir)
	newCA, err := pki.LoadCA(target)
	if err != nil || newCA.Fingerprint() != origCA.Fingerprint() {
		t.Fatalf("restored CA: %v", err)
	}
	// The restored client config finds the restored token.
	cc, err := readClientConfig(filepath.Join(home2, ".config", "hivemind", "config.yaml"), true)
	if err != nil || cc.TokenFile != filepath.Join(target, auth.TokenFileName) {
		t.Fatalf("client config %+v, %v", cc, err)
	}

	if _, _, err := run(t, "", "restore", out, "--data-dir", target); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("restore into a non-empty directory: %v", err)
	}
	// A truncated file is refused, and leaves no directory.
	b, _ := os.ReadFile(out)
	cut := filepath.Join(home2, "cut.tar")
	if err := os.WriteFile(cut, b[:len(b)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	t2 := filepath.Join(home2, "t2")
	if _, _, err := run(t, "", "restore", cut, "--data-dir", t2); err == nil {
		t.Fatal("restore of a truncated backup succeeded")
	}
	if _, err := os.Stat(t2); !os.IsNotExist(err) {
		t.Fatalf("a failed restore left %s: %v", t2, err)
	}
}

func TestBackupHelpSaysItHoldsTheCAKey(t *testing.T) {
	isolate(t)
	for _, cmd := range []string{"backup", "restore"} {
		out, _, err := run(t, "", cmd, "--help")
		if err != nil || !strings.Contains(out, "CA's private key") {
			t.Errorf("%s --help does not say the backup holds the CA private key: %v\n%s", cmd, err, out)
		}
	}
}

// backup --api gets the archive from the server, and it restores.
func TestBackupThroughTheAPI(t *testing.T) {
	home := isolate(t)
	dir := filepath.Join(home, "server")
	if _, _, err := run(t, "", "init", "--data-dir", dir); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(dir, storeFileName))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cl, err := st.GetCluster(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := auth.ReadAdminToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	sess, _ := auth.NewSessions(time.Hour)
	authn, _ := auth.NewAuthenticator(auth.TokenHash(cl.AdminTokenHash), sess)
	api, err := server.New(server.Config{Store: st, Runtime: fakeRuntime{}, NodeID: cl.NodeID, Auth: authn,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Backup: func(ctx context.Context, w io.Writer) error {
			_, err := backup.Write(ctx, w, backup.Options{DataDir: dir, Snapshot: st.SnapshotTo})
			return err
		}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(api.Handler()) // 127.0.0.1, so plain HTTP is allowed
	defer ts.Close()

	out := filepath.Join(home, "api.tar")
	if _, _, err := run(t, "", "backup", out, "--api", "--server", ts.URL, "--token", tok); err != nil {
		t.Fatalf("backup --api: %v", err)
	}
	if _, _, err := run(t, "", "backup", filepath.Join(home, "bad.tar"), "--api", "--server", ts.URL, "--token", "hm_wrong"); err == nil ||
		err.Error() != "unauthorized" {
		t.Fatalf("backup --api with a wrong token: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "bad.tar")); !os.IsNotExist(err) {
		t.Fatal("a refused backup left a file")
	}
	if _, _, err := run(t, "", "restore", out, "--data-dir", filepath.Join(home, "r")); err != nil {
		t.Fatalf("restore of the API backup: %v", err)
	}
}

// A backup into a directory that does not exist says so, and names the
// directory, not the temporary file the archive would have gone to first;
// so does one whose directory is a file. Nothing is written.
func TestBackupIntoAMissingDirectory(t *testing.T) {
	home := isolate(t)
	dir := filepath.Join(home, "server")
	if _, _, err := run(t, "", "init", "--data-dir", dir); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(home, "backups", "hivemind")
	for _, args := range [][]string{{"--data-dir", dir}, {"--api", "--server", "http://127.0.0.1:1", "--token", "x"}} {
		_, _, err := run(t, "", append([]string{"backup", filepath.Join(missing, "Mon.tar")}, args...)...)
		want := "the directory " + missing + " does not exist; create it first (mkdir -p " + missing + ")"
		if err == nil || err.Error() != want {
			t.Fatalf("backup %v into a missing directory: %v, want %q", args, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "backups")); !os.IsNotExist(err) {
		t.Fatalf("backup created something: %v", err)
	}
	file := filepath.Join(home, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run(t, "", "backup", filepath.Join(file, "b.tar"), "--data-dir", dir); err == nil || !strings.Contains(err.Error(), file+" is not a directory") {
		t.Fatalf("backup under a file: %v", err)
	}
}
