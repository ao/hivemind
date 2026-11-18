package backup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/store"
)

func names(m Manifest) []string {
	var out []string
	for _, f := range m.Files {
		out = append(out, f.Name)
	}
	return out
}

// startRotation does what hivemind ca rotate start does to the data
// directory and the store.
func (s *server) startRotation(t *testing.T) *pki.CA {
	t.Helper()
	next, err := pki.CreateNextCA(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.st.StartCARotation(context.Background(), s.ca.Fingerprint(), next.Fingerprint()); err != nil {
		t.Fatal(err)
	}
	return next
}

// A backup during a rotation holds the new CA too, and restores a server in
// the same phase, with both keys.
func TestBackupDuringARotation(t *testing.T) {
	s := newServer(t)
	next := s.startRotation(t)
	var b bytes.Buffer
	m, err := Write(context.Background(), &b, Options{DataDir: s.dir, Snapshot: s.st.SnapshotTo})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(m); !slices.Contains(got, pki.NextCAKeyFile) || !slices.Contains(got, pki.NextCACertFile) {
		t.Fatalf("a backup during a rotation has %v", got)
	}
	dir := filepath.Join(t.TempDir(), "r")
	res, err := Restore(context.Background(), bytes.NewReader(b.Bytes()), dir)
	if err != nil {
		t.Fatal(err)
	}
	if res.CARotation == nil || res.CARotation.Phase != store.CAPhaseTrust || res.CARotation.NextFP != next.Fingerprint() {
		t.Fatalf("restored rotation: %+v", res.CARotation)
	}
	if ca, err := pki.LoadNextCA(dir); err != nil || ca.Fingerprint() != next.Fingerprint() {
		t.Fatalf("the restored new CA: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(dir, pki.NextCAKeyFile)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the restored new CA key: %v %v", fi, err)
	}
}

// Outside a rotation, a backup has the files it always had, and leaves out
// the new CA files of a start that never recorded its rotation.
func TestBackupLeavesOutAnOrphanNextCA(t *testing.T) {
	s := newServer(t)
	if _, err := pki.CreateNextCA(s.dir); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	m, err := Write(context.Background(), &b, Options{DataDir: s.dir, Snapshot: s.st.SnapshotTo})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(m); slices.Contains(got, pki.NextCAKeyFile) || len(got) != 4 {
		t.Fatalf("a backup outside a rotation has %v", got)
	}
}

// A step of a rotation between the store's copy and the reads of the CA
// files leaves them out of step: backup fails, and says to run it again.
func TestBackupRefusesCAFilesOutOfStepWithTheStore(t *testing.T) {
	s := newServer(t)
	next := s.startRotation(t)
	// As if finish ran after the store was copied: ca.crt is the new CA,
	// and the copy still records the rotation.
	snap := func(ctx context.Context, dest string) error {
		if err := s.st.SnapshotTo(ctx, dest); err != nil {
			return err
		}
		if err := os.Rename(filepath.Join(s.dir, pki.NextCAKeyFile), filepath.Join(s.dir, pki.CAKeyFile)); err != nil {
			return err
		}
		return os.Rename(filepath.Join(s.dir, pki.NextCACertFile), filepath.Join(s.dir, pki.CACertFile))
	}
	var b bytes.Buffer
	_, err := Write(context.Background(), &b, Options{DataDir: s.dir, Snapshot: snap})
	if err == nil || !strings.Contains(err.Error(), "run it again") {
		t.Fatalf("a backup out of step: %v", err)
	}
	if ca, _ := pki.LoadCA(s.dir); ca.Fingerprint() != next.Fingerprint() {
		t.Fatal("test setup: ca.crt is not the new CA")
	}

	// And a rotation whose end had begun is refused.
	s2 := newServer(t)
	s2.startRotation(t)
	rot, _ := s2.st.GetCARotation(context.Background())
	rot.Phase = store.CAPhaseEnding
	if err := s2.st.SetCARotation(context.Background(), rot, false); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(context.Background(), &b, Options{DataDir: s2.dir, Snapshot: s2.st.SnapshotTo}); err == nil {
		t.Fatal("a backup of a rotation that was ending")
	}
}

// Restore checks the same: a store that records no rotation, with a new
// CA's files beside it, is refused.
func TestRestoreRefusesANextCAWithoutARotation(t *testing.T) {
	s := newServer(t)
	ctx := context.Background()
	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := s.st.SnapshotTo(ctx, snap); err != nil { // before the rotation
		t.Fatal(err)
	}
	info, err := store.InspectFile(ctx, snap)
	if err != nil {
		t.Fatal(err)
	}
	s.startRotation(t)
	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	files := [][2]string{{StoreName, read(snap)}}
	for _, name := range []string{pki.CAKeyFile, pki.CACertFile, pki.NextCAKeyFile, pki.NextCACertFile} {
		files = append(files, [2]string{name, read(filepath.Join(s.dir, name))})
	}
	m := Manifest{Format: Format, Version: Version, SchemaVersion: info.SchemaVersion, NodeID: info.NodeID}
	for _, f := range files {
		m.Files = append(m.Files, fileOf(f[0], f[1]))
	}
	_, err = Restore(ctx, bytes.NewReader(build(t, m, files...)), filepath.Join(t.TempDir(), "bad"))
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "records no CA rotation") {
		t.Fatalf("a store with no rotation and a new CA's files: %v", err)
	}
}
