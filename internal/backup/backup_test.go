package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/auth"
	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/spec"
	"github.com/ao/hivemind/internal/store"
)

const (
	serverNode = "node-server00001"
	workerNode = "node-worker00001"
	gone       = "node-gone0000001"
)

// server is a data directory as init and some use leave it.
type server struct {
	dir   string
	st    *store.Store
	ca    *pki.CA
	token string
}

func newServer(t *testing.T) *server {
	t.Helper()
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "data")
	st, err := store.Open(ctx, filepath.Join(dir, StoreName), store.WithMinReady(0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ca, err := pki.CreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	token, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.WriteAdminToken(dir, token); err != nil {
		t.Fatal(err)
	}
	if err := st.InitCluster(ctx, store.Cluster{AdminTokenHash: auth.HashToken(token), NodeID: serverNode}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []store.Node{{ID: serverNode, Name: "s", CPU: 8, Memory: 8 << 30}, {ID: workerNode, Name: "w", CPU: 8, Memory: 8 << 30},
		{ID: gone, Name: "g", CPU: 1, Memory: 1 << 30}} {
		if err := st.UpsertNode(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.RemoveNode(ctx, gone, true); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutApp(ctx, spec.App{Name: "web", Image: "nginx:alpine", Replicas: 3, Env: map[string]string{"K": "v"}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := pki.NewTokens(st.JoinTokens()).Issue(ca.Fingerprint(), time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordNodeCert(ctx, workerNode, time.Unix(2_000_000_000, 0)); err != nil {
		t.Fatal(err)
	}
	return &server{dir: dir, st: st, ca: ca, token: token}
}

func (s *server) backup(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if _, err := Write(context.Background(), &b, Options{DataDir: s.dir, Snapshot: s.st.SnapshotTo, HivemindVersion: "test"}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestRoundTrip(t *testing.T) {
	s := newServer(t)
	archive := s.backup(t)
	m, err := Verify(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	if m.Format != Format || m.Version != Version || m.NodeID != serverNode || m.SchemaVersion != store.SchemaVersion || len(m.Files) != 4 {
		t.Fatalf("manifest %+v", m)
	}

	dir := filepath.Join(t.TempDir(), "restored")
	if _, err := Restore(context.Background(), bytes.NewReader(archive), dir); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("data directory mode %v, %v", fi.Mode().Perm(), err)
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		fi, _ := e.Info()
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s has mode %04o, want 0600", e.Name(), fi.Mode().Perm())
		}
	}
	for _, name := range []string{StoreName, pki.CAKeyFile, pki.CACertFile, auth.TokenFileName} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	ctx := context.Background()
	r, err := store.Open(ctx, filepath.Join(dir, StoreName))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	cl, err := r.GetCluster(ctx)
	if err != nil || cl.NodeID != serverNode || cl.AdminTokenHash != [32]byte(auth.HashToken(s.token)) {
		t.Fatalf("cluster record %+v, %v", cl, err)
	}
	reps, err := r.Replicas(ctx, "web")
	if err != nil || len(reps) != 3 {
		t.Fatalf("replicas %v, %v", reps, err)
	}
	nodes, err := r.ListNodes(ctx)
	if err != nil || len(nodes) != 2 || nodes[1].ID != workerNode || !nodes[1].CertNotAfter.Equal(time.Unix(2_000_000_000, 0)) {
		t.Fatalf("nodes %+v, %v", nodes, err)
	}
	if removed, err := r.IsNodeRemoved(ctx, gone); err != nil || !removed {
		t.Fatalf("removed node %s: %v, %v", gone, removed, err)
	}
	var tokens int
	if err := sqlCount(t, filepath.Join(dir, StoreName), `SELECT count(*) FROM join_tokens`, &tokens); err != nil || tokens != 1 {
		t.Fatalf("join tokens %d, %v", tokens, err)
	}
	ca, err := pki.LoadCA(dir)
	if err != nil || ca.Fingerprint() != s.ca.Fingerprint() {
		t.Fatalf("restored CA: %v", err)
	}
	if tok, err := auth.ReadAdminToken(dir); err != nil || tok != s.token {
		t.Fatalf("restored admin token: %v", err)
	}
}

func sqlCount(t *testing.T, path, q string, n *int) error {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	return db.QueryRow(q).Scan(n)
}

// The backup is taken from a store that other goroutines write to, and each
// one restores to a store that opens and holds whole transactions.
func TestBackupDuringConcurrentWrites(t *testing.T) {
	s := newServer(t)
	ctx := context.Background()
	var stop atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; !stop.Load(); i++ {
			if _, err := s.st.Scale(ctx, "web", 1+i%9); err != nil {
				t.Errorf("Scale: %v", err)
				return
			}
		}
	}()
	for i := range 8 {
		archive := s.backup(t)
		dir := filepath.Join(t.TempDir(), fmt.Sprint(i))
		if _, err := Restore(ctx, bytes.NewReader(archive), dir); err != nil {
			t.Fatal(err)
		}
		r, err := store.Open(ctx, filepath.Join(dir, StoreName))
		if err != nil {
			t.Fatal(err)
		}
		a, err := r.GetApp(ctx, "web")
		if err != nil {
			t.Fatal(err)
		}
		reps, err := r.Replicas(ctx, "web")
		if err != nil {
			t.Fatal(err)
		}
		running := 0
		for _, rp := range reps {
			if rp.Desired == store.Running {
				running++
			}
		}
		if running != a.Spec.Replicas {
			t.Errorf("backup %d: app asks for %d replicas, and %d are running", i, a.Spec.Replicas, running)
		}
		r.Close()
	}
	stop.Store(true)
	wg.Wait()
}

func TestRestoreRefusesNonEmptyDir(t *testing.T) {
	archive := newServer(t).backup(t)
	dir := t.TempDir()
	keep := filepath.Join(dir, "something")
	if err := os.WriteFile(keep, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(context.Background(), bytes.NewReader(archive), dir); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("non-empty directory: %v, want ErrNotEmpty", err)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Fatalf("restore changed a non-empty directory: %v", ents)
	}
	// An empty directory that exists is fine, and gets mode 0700.
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.Mkdir(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(context.Background(), bytes.NewReader(archive), empty); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(empty); fi.Mode().Perm() != 0o700 {
		t.Fatalf("mode %04o, want 0700", fi.Mode().Perm())
	}
}

// expectNothingLeft checks that a failed restore left no directory behind.
func expectNothingLeft(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		ents, _ := os.ReadDir(dir)
		t.Fatalf("a failed restore left %s with %v", dir, ents)
	}
}

// Every truncation of the archive that loses data is refused by Verify and
// by Restore, and a failed restore leaves nothing. The last 1024 bytes are
// the tar trailer, two zero blocks, and before them is up to 511 bytes of
// zero padding; Go's tar reader takes a cut there as the end, which is
// harmless, since every file is complete and checked by then.
func TestTruncatedArchiveIsRefused(t *testing.T) {
	archive := newServer(t).backup(t)
	end := len(archive) - 1024 - 511
	step := end / 97
	cuts := []int{end - 1}
	for n := 0; n < end; n += max(step, 1) {
		cuts = append(cuts, n)
	}
	for _, n := range cuts {
		cut := archive[:n]
		if _, err := Verify(bytes.NewReader(cut)); err == nil {
			t.Fatalf("Verify accepted the first %d of %d bytes", n, len(archive))
		}
		dir := filepath.Join(t.TempDir(), "r")
		if _, err := Restore(context.Background(), bytes.NewReader(cut), dir); err == nil {
			t.Fatalf("Restore accepted the first %d of %d bytes", n, len(archive))
		}
		expectNothingLeft(t, dir)
	}
}

// A flipped byte in any file's data fails its digest.
func TestCorruptArchiveIsRefused(t *testing.T) {
	archive := newServer(t).backup(t)
	tr := tar.NewReader(bytes.NewReader(archive))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Name == ManifestName || hdr.Size == 0 {
			continue
		}
		// Find the entry's data in the archive and flip a byte in it.
		data, _ := io.ReadAll(tr)
		at := bytes.Index(archive, data)
		if at < 0 {
			t.Fatalf("data of %s not found", hdr.Name)
		}
		bad := bytes.Clone(archive)
		bad[at+len(data)/2] ^= 0x01
		if _, err := Verify(bytes.NewReader(bad)); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), hdr.Name) {
			t.Errorf("a flipped byte in %s: %v", hdr.Name, err)
		}
		dir := filepath.Join(t.TempDir(), "r")
		if _, err := Restore(context.Background(), bytes.NewReader(bad), dir); err == nil {
			t.Errorf("Restore accepted a flipped byte in %s", hdr.Name)
		}
		expectNothingLeft(t, dir)
	}
	if _, err := Verify(strings.NewReader("this is not a tar file at all")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("not a tar file: %v", err)
	}
}

// build writes an archive with manifest m and the given entries, in order.
func build(t *testing.T, m any, files ...[2]string) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	add := func(name string, data []byte) {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o600, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	mb, _ := json.Marshal(m)
	add(ManifestName, mb)
	for _, f := range files {
		add(f[0], []byte(f[1]))
	}
	tw.Close()
	return b.Bytes()
}

func fileOf(name, data string) File {
	sum := sha256.Sum256([]byte(data))
	return File{Name: name, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
}

func TestManifestChecks(t *testing.T) {
	good := func() Manifest {
		return Manifest{Format: Format, Version: Version, SchemaVersion: 7, NodeID: serverNode,
			Files: []File{fileOf(StoreName, "db"), fileOf(pki.CAKeyFile, "k"), fileOf(pki.CACertFile, "c")}}
	}
	all := [][2]string{{StoreName, "db"}, {pki.CAKeyFile, "k"}, {pki.CACertFile, "c"}}
	cases := []struct {
		name  string
		edit  func(*Manifest)
		files [][2]string
		want  string
	}{
		{"newer format version", func(m *Manifest) { m.Version = Version + 1 }, all, "newer hivemind"},
		{"other format", func(m *Manifest) { m.Format = "tarball" }, all, "format"},
		{"no CA key", func(m *Manifest) { m.Files = m.Files[:1] }, all[:1], "ca.key"},
		{"unknown file", func(m *Manifest) { m.Files = append(m.Files, fileOf("../../etc/passwd", "x")) }, all, "does not hold"},
		{"listed twice", func(m *Manifest) { m.Files = append(m.Files, fileOf(StoreName, "db")) }, all, "twice"},
		{"entry not listed", nil, append(all, [2]string{auth.TokenFileName, "t"}), "unexpected entry"},
		{"entry missing", nil, all[:2], "missing"},
		{"entry twice", nil, append(all, [2]string{StoreName, "db"}), "unexpected entry"},
		{"wrong size", nil, [][2]string{{StoreName, "dbx"}, all[1], all[2]}, "byte file"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := good()
			if c.edit != nil {
				c.edit(&m)
			}
			_, err := Verify(bytes.NewReader(build(t, m, c.files...)))
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want ErrInvalid mentioning %q", err, c.want)
			}
		})
	}
	// A newer version with fields this build does not know still says
	// "newer", not "unknown field".
	newer := map[string]any{"format": Format, "version": Version + 1, "something_new": true}
	if _, err := Verify(bytes.NewReader(build(t, newer))); err == nil || !strings.Contains(err.Error(), "newer hivemind") {
		t.Fatalf("newer manifest with a new field: %v", err)
	}
	// A manifest that is not the first entry.
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: StoreName, Mode: 0o600, Size: 0})
	tw.Close()
	if _, err := Verify(&b); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no manifest first: %v", err)
	}
}

// editSnapshot returns a Snapshot that copies the store and then runs stmts
// on the copy.
func editSnapshot(st *store.Store, stmts ...string) func(context.Context, string) error {
	return func(ctx context.Context, dest string) error {
		if err := st.SnapshotTo(ctx, dest); err != nil {
			return err
		}
		db, err := sql.Open("sqlite", dest)
		if err != nil {
			return err
		}
		defer db.Close()
		for _, q := range stmts {
			if _, err := db.ExecContext(ctx, q); err != nil {
				return err
			}
		}
		return nil
	}
}

// A store from a newer hivemind is refused at restore, and nothing is left.
func TestRestoreRefusesNewerSchema(t *testing.T) {
	s := newServer(t)
	var b bytes.Buffer
	snap := editSnapshot(s.st, fmt.Sprintf(`UPDATE schema_version SET version = %d`, store.SchemaVersion+1))
	if _, err := Write(context.Background(), &b, Options{DataDir: s.dir, Snapshot: snap}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "r")
	if _, err := Restore(context.Background(), &b, dir); !errors.Is(err, store.ErrNewerSchema) {
		t.Fatalf("newer schema: %v, want ErrNewerSchema", err)
	}
	expectNothingLeft(t, dir)
}

// A store at an older schema restores, and is migrated forward on open.
func TestRestoreMigratesOlderSchema(t *testing.T) {
	s := newServer(t)
	var b bytes.Buffer
	snap := editSnapshot(s.st, `DROP TABLE node_certs`, `UPDATE schema_version SET version = 6`)
	if _, err := Write(context.Background(), &b, Options{DataDir: s.dir, Snapshot: snap}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "r")
	m, err := Restore(context.Background(), &b, dir)
	if err != nil || m.SchemaVersion != 6 {
		t.Fatalf("restore of a version 6 store: %+v, %v", m, err)
	}
	r, err := store.Open(context.Background(), filepath.Join(dir, StoreName))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.RecordNodeCert(context.Background(), workerNode, time.Now()); err != nil {
		t.Fatalf("the migrated store has no version 7 table: %v", err)
	}
}

// Write refuses a data directory whose CA key others can read, or that has
// no CA, before it writes a byte.
func TestWriteChecksTheCA(t *testing.T) {
	s := newServer(t)
	if err := os.Chmod(filepath.Join(s.dir, pki.CAKeyFile), 0o644); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if _, err := Write(context.Background(), &b, Options{DataDir: s.dir, Snapshot: s.st.SnapshotTo}); !errors.Is(err, pki.ErrKeyPermissions) {
		t.Fatalf("loose CA key: %v", err)
	}
	if b.Len() != 0 {
		t.Fatal("Write wrote part of an archive before it failed")
	}
	if err := os.Remove(filepath.Join(s.dir, pki.CAKeyFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(context.Background(), &b, Options{DataDir: s.dir, Snapshot: s.st.SnapshotTo}); err == nil {
		t.Fatal("a data directory without a CA key was backed up")
	}
	// No temporary directory is left in the data directory.
	ents, _ := os.ReadDir(s.dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".hivemind-backup-") {
			t.Fatalf("temporary directory %s left behind", e.Name())
		}
	}
}

// A backup without the optional admin token file restores.
func TestBackupWithoutTokenFile(t *testing.T) {
	s := newServer(t)
	if err := os.Remove(filepath.Join(s.dir, auth.TokenFileName)); err != nil {
		t.Fatal(err)
	}
	archive := s.backup(t)
	m, err := Restore(context.Background(), bytes.NewReader(archive), filepath.Join(t.TempDir(), "r"))
	if err != nil || len(m.Files) != 3 {
		t.Fatalf("%+v, %v", m, err)
	}
}

// A restored CA key that does not match the certificate is refused.
func TestRestoreRefusesMismatchedCA(t *testing.T) {
	s := newServer(t)
	other, err := pki.CreateCA(filepath.Join(t.TempDir(), "other"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(s.dir, pki.CACertFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.dir, pki.CACertFile), other.CertPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if _, err := Write(context.Background(), &b, Options{DataDir: s.dir, Snapshot: s.st.SnapshotTo}); err == nil {
		t.Fatal("Write backed up a CA whose key and certificate differ")
	}
}
