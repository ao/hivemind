// Package backup writes and restores a backup of a server's data directory:
// the store and the cluster CA, and the admin token file when it is there.
//
// A backup is a tar file. Its first entry is manifest.json, which names the
// format and its version, the store's schema version, the cluster's node ID,
// and the size and SHA-256 of every other entry. The other entries are the
// files of the data directory, by their names there. Restore checks every
// size and digest, so a truncated or altered file is refused.
//
// The backup holds the cluster CA's private key, which can sign a
// certificate for any node, and the admin token when the data directory has
// one. It is written with mode 0600 and must be kept as secret as the data
// directory.
package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/ao/hivemind/internal/auth"
	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/store"
)

// Format and Version identify the archive. A reader refuses any other format,
// and a version above its own.
const (
	Format  = "hivemind-backup"
	Version = 1

	// ManifestName is the archive's first entry.
	ManifestName = "manifest.json"
	// StoreName is the store file, in the archive and in the data directory.
	StoreName = "hivemind.db"
)

// Size limits for the entries. The store has no fixed limit; the others are
// small PEM or text files.
const (
	maxManifest  = 64 << 10
	maxSmallFile = 64 << 10
	maxStore     = 1 << 40
)

// entry is one file a backup can hold.
type entry struct {
	name     string
	required bool
	max      int64
}

// entries are the data-directory files a backup holds, in archive order.
var entries = []entry{
	{StoreName, true, maxStore},
	{pki.CAKeyFile, true, maxSmallFile},
	{pki.CACertFile, true, maxSmallFile},
	{auth.TokenFileName, false, maxSmallFile},
}

func lookup(name string) (entry, bool) {
	i := slices.IndexFunc(entries, func(e entry) bool { return e.name == name })
	if i < 0 {
		return entry{}, false
	}
	return entries[i], true
}

// ErrInvalid means a file is not a backup this build can read, or it is
// damaged: truncated, altered or incomplete.
var ErrInvalid = errors.New("invalid backup")

// Manifest is the archive's first entry.
type Manifest struct {
	Format          string    `json:"format"`
	Version         int       `json:"version"`
	Created         time.Time `json:"created"`
	HivemindVersion string    `json:"hivemind_version"`
	// SchemaVersion is the store's schema version.
	SchemaVersion int `json:"schema_version"`
	// NodeID is the server's own node, from the store's cluster record.
	NodeID string `json:"node_id"`
	Files  []File `json:"files"`
}

// File is one entry after the manifest.
type File struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Options configures Write.
type Options struct {
	// DataDir is the server's data directory. Its CA files and admin token
	// are read from there.
	DataDir string
	// TempDir is where the store snapshot is written before it goes into
	// the archive. It should be on a disk only hivemind's user can read.
	// Empty means DataDir.
	TempDir string
	// Snapshot writes a consistent copy of the store to dest, which does
	// not exist: store.SnapshotTo, or store.SnapshotFile for a store this
	// process does not have open.
	Snapshot func(ctx context.Context, dest string) error
	// HivemindVersion goes in the manifest.
	HivemindVersion string
	// Now is the clock for the manifest. Nil means time.Now.
	Now func() time.Time
}

// Write writes a backup of the data directory to w. Everything that can
// fail before the archive starts is done first: the store snapshot, and
// reading and checking the CA. An error returned after the first byte means
// w holds a partial archive, which Verify and Restore refuse.
func Write(ctx context.Context, w io.Writer, o Options) (Manifest, error) {
	if o.Snapshot == nil || o.DataDir == "" {
		return Manifest{}, errors.New("backup: DataDir and Snapshot are required")
	}
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	tmpParent := o.TempDir
	if tmpParent == "" {
		tmpParent = o.DataDir
	}
	tmp, err := os.MkdirTemp(tmpParent, ".hivemind-backup-")
	if err != nil {
		return Manifest{}, fmt.Errorf("backup: temporary directory: %w", err)
	}
	defer os.RemoveAll(tmp)
	if err := os.Chmod(tmp, 0o700); err != nil {
		return Manifest{}, err
	}

	snap := filepath.Join(tmp, StoreName)
	if err := o.Snapshot(ctx, snap); err != nil {
		return Manifest{}, err
	}
	info, err := store.InspectFile(ctx, snap)
	if err != nil {
		return Manifest{}, err
	}
	if info.NodeID == "" {
		return Manifest{}, fmt.Errorf("backup: the store in %s has no cluster record; run hivemind init first", o.DataDir)
	}
	// LoadCA refuses a key that others can read, or that does not match
	// the certificate, so a backup never carries a broken CA.
	if _, err := pki.LoadCA(o.DataDir); err != nil {
		return Manifest{}, fmt.Errorf("backup: cluster CA in %s: %w", o.DataDir, err)
	}

	paths := map[string]string{StoreName: snap}
	m := Manifest{Format: Format, Version: Version, Created: now().UTC(), HivemindVersion: o.HivemindVersion,
		SchemaVersion: info.SchemaVersion, NodeID: info.NodeID}
	for _, e := range entries {
		p, ok := paths[e.name]
		if !ok {
			p = filepath.Join(o.DataDir, e.name)
		}
		f, err := digest(p, e.max)
		if errors.Is(err, fs.ErrNotExist) && !e.required {
			continue
		}
		if err != nil {
			return Manifest{}, fmt.Errorf("backup: %w", err)
		}
		f.Name = e.name
		paths[e.name] = p
		m.Files = append(m.Files, f)
	}

	mb, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return Manifest{}, err
	}
	tw := tar.NewWriter(w)
	mod := m.Created
	if err := writeEntry(tw, ManifestName, mod, bytes.NewReader(mb), int64(len(mb))); err != nil {
		return Manifest{}, err
	}
	for _, f := range m.Files {
		if err := copyEntry(tw, paths[f.Name], f, mod); err != nil {
			return Manifest{}, err
		}
	}
	if err := tw.Close(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// digest returns the size and SHA-256 of a regular file of at most max bytes.
func digest(path string, max int64) (File, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return File{}, err
	}
	if !fi.Mode().IsRegular() {
		return File{}, fmt.Errorf("%s is not a regular file", path)
	}
	if fi.Size() > max {
		return File{}, fmt.Errorf("%s is %d bytes, more than %d", path, fi.Size(), max)
	}
	f, err := os.Open(path)
	if err != nil {
		return File{}, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return File{}, err
	}
	return File{Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

func writeEntry(tw *tar.Writer, name string, mod time.Time, r io.Reader, size int64) error {
	hdr := &tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o600, Size: size, ModTime: mod, Format: tar.FormatPAX}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err := io.Copy(tw, r)
	return err
}

// copyEntry writes the file at path, checking that it still has the size and
// digest in the manifest.
func copyEntry(tw *tar.Writer, path string, f File, mod time.Time) error {
	in, err := os.Open(path)
	if err != nil {
		return err
	}
	defer in.Close()
	h := sha256.New()
	if err := writeEntry(tw, f.Name, mod, io.TeeReader(io.LimitReader(in, f.Size), h), f.Size); err != nil {
		return fmt.Errorf("backup: %s: %w", f.Name, err)
	}
	if hex.EncodeToString(h.Sum(nil)) != f.SHA256 {
		return fmt.Errorf("backup: %s changed while the backup was written", path)
	}
	return nil
}

// sink receives the files of an archive as Read checks them.
type sink func(f File, r io.Reader) error

// read reads an archive, checks the manifest and every entry against it, and
// passes each entry to put. The data put sees is not yet checked against its
// digest: read returns an error if it does not match, after put returns.
func read(r io.Reader, put sink) (Manifest, error) {
	tr := tar.NewReader(r)
	hdr, err := tr.Next()
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: no manifest: %v", ErrInvalid, err)
	}
	if hdr.Name != ManifestName || hdr.Typeflag != tar.TypeReg || hdr.Size > maxManifest || hdr.Size < 0 {
		return Manifest{}, fmt.Errorf("%w: the first entry is %q, not %s", ErrInvalid, hdr.Name, ManifestName)
	}
	mb, err := io.ReadAll(io.LimitReader(tr, hdr.Size))
	if err != nil || int64(len(mb)) != hdr.Size {
		return Manifest{}, fmt.Errorf("%w: manifest is truncated", ErrInvalid)
	}
	m, err := parseManifest(mb)
	if err != nil {
		return Manifest{}, err
	}

	want := map[string]File{}
	for _, f := range m.Files {
		want[f.Name] = f
	}
	seen := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Manifest{}, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		f, ok := want[hdr.Name]
		if !ok || seen[hdr.Name] {
			return Manifest{}, fmt.Errorf("%w: unexpected entry %q", ErrInvalid, hdr.Name)
		}
		if hdr.Typeflag != tar.TypeReg || hdr.Size != f.Size {
			return Manifest{}, fmt.Errorf("%w: entry %s is not a %d-byte file", ErrInvalid, hdr.Name, f.Size)
		}
		seen[hdr.Name] = true
		h := sha256.New()
		cr := &countReader{r: io.TeeReader(io.LimitReader(tr, f.Size), h)}
		if err := put(f, cr); err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) {
				return Manifest{}, fmt.Errorf("%w: %s is truncated", ErrInvalid, f.Name)
			}
			return Manifest{}, err
		}
		if _, err := io.Copy(io.Discard, cr); err != nil {
			return Manifest{}, fmt.Errorf("%w: %s: %v", ErrInvalid, f.Name, err)
		}
		if cr.n != f.Size {
			return Manifest{}, fmt.Errorf("%w: %s is truncated", ErrInvalid, f.Name)
		}
		if hex.EncodeToString(h.Sum(nil)) != f.SHA256 {
			return Manifest{}, fmt.Errorf("%w: %s does not match its SHA-256 in the manifest", ErrInvalid, f.Name)
		}
	}
	for _, f := range m.Files {
		if !seen[f.Name] {
			return Manifest{}, fmt.Errorf("%w: %s is missing; the file is truncated", ErrInvalid, f.Name)
		}
	}
	return m, nil
}

type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func parseManifest(b []byte) (Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		// A newer version may add fields; say so if it claims one.
		var probe struct {
			Format  string `json:"format"`
			Version int    `json:"version"`
		}
		if json.Unmarshal(b, &probe) == nil && probe.Format == Format && probe.Version > Version {
			return Manifest{}, newerFormat(probe.Version)
		}
		return Manifest{}, fmt.Errorf("%w: manifest: %v", ErrInvalid, err)
	}
	switch {
	case m.Format != Format:
		return Manifest{}, fmt.Errorf("%w: format %q, not %s", ErrInvalid, m.Format, Format)
	case m.Version > Version:
		return Manifest{}, newerFormat(m.Version)
	case m.Version < 1:
		return Manifest{}, fmt.Errorf("%w: format version %d", ErrInvalid, m.Version)
	case m.SchemaVersion < 1:
		return Manifest{}, fmt.Errorf("%w: schema version %d", ErrInvalid, m.SchemaVersion)
	case pki.ValidateNodeID(m.NodeID) != nil:
		return Manifest{}, fmt.Errorf("%w: node ID %q", ErrInvalid, m.NodeID)
	}
	names := map[string]bool{}
	for _, f := range m.Files {
		e, ok := lookup(f.Name)
		switch {
		case !ok:
			return Manifest{}, fmt.Errorf("%w: manifest names %q, which a backup does not hold", ErrInvalid, f.Name)
		case names[f.Name]:
			return Manifest{}, fmt.Errorf("%w: manifest names %s twice", ErrInvalid, f.Name)
		case f.Size < 0 || f.Size > e.max:
			return Manifest{}, fmt.Errorf("%w: %s has size %d", ErrInvalid, f.Name, f.Size)
		case len(f.SHA256) != 64:
			return Manifest{}, fmt.Errorf("%w: %s has no SHA-256", ErrInvalid, f.Name)
		}
		names[f.Name] = true
	}
	for _, e := range entries {
		if e.required && !names[e.name] {
			return Manifest{}, fmt.Errorf("%w: manifest does not list %s", ErrInvalid, e.name)
		}
	}
	return m, nil
}

func newerFormat(v int) error {
	return fmt.Errorf("%w: format version %d was written by a newer hivemind; this build reads version %d", ErrInvalid, v, Version)
}

// Verify reads a whole archive and checks it: the manifest, and the size and
// digest of every file. It writes nothing.
func Verify(r io.Reader) (Manifest, error) {
	return read(r, func(File, io.Reader) error { return nil })
}

// ErrNotEmpty means the restore target already holds files.
var ErrNotEmpty = errors.New("the data directory is not empty")

// Restore writes the files of the archive into dir, which must be missing or
// empty. dir gets mode 0700 and each file mode 0600. The store must pass
// SQLite's integrity check and have a schema version this build knows; an
// older one is migrated forward when serve opens it, as with any data
// directory. The CA key must match the CA certificate. On any error, the
// files written so far are removed, and so is dir if Restore created it.
func Restore(ctx context.Context, r io.Reader, dir string) (m Manifest, err error) {
	if dir == "" {
		return Manifest{}, errors.New("restore: no data directory")
	}
	created := false
	switch ents, rerr := os.ReadDir(dir); {
	case errors.Is(rerr, fs.ErrNotExist):
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return Manifest{}, err
		}
		created = true
	case rerr != nil:
		return Manifest{}, rerr
	case len(ents) > 0:
		return Manifest{}, fmt.Errorf("%w: %s holds %d entries; restore needs a new or empty directory", ErrNotEmpty, dir, len(ents))
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return Manifest{}, err
	}
	var written []string
	defer func() {
		if err == nil {
			return
		}
		for _, p := range written {
			_ = os.Remove(p)
		}
		if created {
			_ = os.Remove(dir)
		}
	}()

	m, err = read(r, func(f File, src io.Reader) error {
		p := filepath.Join(dir, f.Name)
		out, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		written = append(written, p)
		werr := out.Chmod(0o600)
		if werr == nil {
			_, werr = io.Copy(out, src)
		}
		if werr == nil {
			werr = out.Sync()
		}
		if cerr := out.Close(); werr == nil {
			werr = cerr
		}
		return werr
	})
	if err != nil {
		return Manifest{}, err
	}

	info, err := store.InspectFile(ctx, filepath.Join(dir, StoreName))
	if err != nil {
		return Manifest{}, err
	}
	// Opening the store may have left SQLite's side files; they are part
	// of what is cleaned up on failure.
	for _, side := range []string{"-wal", "-shm", "-journal"} {
		p := filepath.Join(dir, StoreName+side)
		if _, err := os.Lstat(p); err == nil {
			written = append(written, p)
		}
	}
	switch {
	case info.SchemaVersion > store.SchemaVersion:
		return Manifest{}, fmt.Errorf("%w: the store in the backup has schema version %d and this build knows up to %d; restore it with a newer hivemind",
			store.ErrNewerSchema, info.SchemaVersion, store.SchemaVersion)
	case info.SchemaVersion != m.SchemaVersion || info.NodeID != m.NodeID:
		return Manifest{}, fmt.Errorf("%w: the store does not match the manifest (schema %d, node %q)", ErrInvalid, info.SchemaVersion, info.NodeID)
	}
	if _, err := pki.LoadCA(dir); err != nil {
		return Manifest{}, fmt.Errorf("%w: cluster CA: %v", ErrInvalid, err)
	}
	if err := syncDir(dir); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	serr := d.Sync()
	if cerr := d.Close(); serr == nil {
		serr = cerr
	}
	return serr
}
