package ha

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/raft"

	"github.com/ao/hivemind/internal/store"
)

// fsm is the store as Raft's state machine. An entry is a batch of the
// store's write statements (store.ApplyEntry); a snapshot is the store's
// own (store.SnapshotTo), and restoring one loads it into the open store
// (store.LoadSnapshot).
type fsm struct {
	st  *store.Store
	dir string // for snapshot files, in the Raft directory
	log *slog.Logger
	// fatal is called once, with the error, when an entry cannot be
	// applied. From then on nothing more is applied: a later entry would
	// move the store's applied index past the one that failed.
	fatal  func(error)
	broken atomic.Pointer[error]
	once   sync.Once
}

var _ raft.FSM = (*fsm)(nil)

// applyRetry is how long the FSM waits before it tries a failed entry once
// more.
const applyRetry = time.Second

func (f *fsm) Apply(l *raft.Log) any {
	if l.Type != raft.LogCommand {
		return nil
	}
	if p := f.broken.Load(); p != nil {
		return *p
	}
	ctx := context.Background()
	err := f.st.ApplyEntry(ctx, l.Index, l.Data)
	if err != nil && !errors.Is(err, store.ErrOtherSchema) {
		f.log.Warn("applying a log entry failed; trying once more", "index", l.Index, "err", err)
		time.Sleep(applyRetry)
		err = f.st.ApplyEntry(ctx, l.Index, l.Data)
	}
	if err != nil {
		err = fmt.Errorf("applying log entry %d: %w", l.Index, err)
		f.broken.Store(&err)
		f.once.Do(func() {
			if f.fatal != nil {
				f.fatal(err)
			}
		})
		return err
	}
	return nil
}

func (f *fsm) Snapshot() (raft.FSMSnapshot, error) {
	if p := f.broken.Load(); p != nil {
		return nil, *p
	}
	path, err := tempPath(f.dir, "snapshot-*.db")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := f.st.SnapshotTo(ctx, path); err != nil {
		return nil, err
	}
	return &fsmSnapshot{path: path, snapDir: snapshotDir(f.dir)}, nil
}

func (f *fsm) Restore(rc io.ReadCloser) error {
	_ = tightenModes(snapshotDir(f.dir))
	defer rc.Close()
	path, err := tempPath(f.dir, "restore-*.db")
	if err != nil {
		return err
	}
	defer os.Remove(path)
	if err := copyToFile(path, rc); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	return f.st.LoadSnapshot(ctx, path)
}

// tempPath returns a path in dir that no file has, from pattern.
func tempPath(dir, pattern string) (string, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	path := f.Name()
	f.Close()
	return path, os.Remove(path)
}

func copyToFile(path string, r io.Reader) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

type fsmSnapshot struct{ path, snapDir string }

func (s *fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	f, err := os.Open(s.path)
	if err != nil {
		_ = sink.Cancel()
		return err
	}
	defer f.Close()
	if _, err := io.Copy(sink, f); err != nil {
		_ = sink.Cancel()
		return err
	}
	if err := sink.Close(); err != nil {
		return err
	}
	return tightenModes(s.snapDir)
}

func (s *fsmSnapshot) Release() { _ = os.Remove(s.path) }

// catchUpStore makes the store at least as new as the latest snapshot
// before raft starts. Raft starts from its latest snapshot without
// restoring it into the store (NoSnapshotRestoreOnStart), since the store
// on disk is usually newer; but a server stopped between raft saving a
// snapshot it received and the store loading it has an older store, which
// the entries after the snapshot would be applied to. Such a store loads
// the snapshot here.
func catchUpStore(ctx context.Context, st *store.Store, snaps raft.SnapshotStore, dir string, log *slog.Logger) error {
	list, err := snaps.List()
	if err != nil || len(list) == 0 {
		return err
	}
	latest := list[0]
	applied, err := st.AppliedIndex(ctx)
	if err != nil {
		return err
	}
	if applied >= latest.Index {
		return nil
	}
	_, rc, err := snaps.Open(latest.ID)
	if err != nil {
		return err
	}
	defer rc.Close()
	path, err := tempPath(dir, "catchup-*.db")
	if err != nil {
		return err
	}
	defer os.Remove(path)
	if err := copyToFile(path, rc); err != nil {
		return err
	}
	if err := st.LoadSnapshot(ctx, path); err != nil {
		return fmt.Errorf("loading the Raft snapshot at index %d: %w", latest.Index, err)
	}
	log.Info("the store was older than the latest Raft snapshot, and loaded it", "applied", applied, "snapshot", latest.Index)
	return st.RaiseAppliedIndex(ctx, latest.Index)
}

// snapshotDir is where raft keeps its snapshots.
func snapshotDir(raftDir string) string { return filepath.Join(raftDir, "snapshots") }

// tightenModes makes every directory under dir mode 0700 and every file
// 0600. Raft makes its snapshot directories 0755 and their files with the
// process's umask; they hold a copy of the store, so they get the store's
// mode. The data directory, 0700, keeps other users out of them anyway.
func tightenModes(dir string) error {
	return filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		mode := os.FileMode(0o600)
		if d.IsDir() {
			mode = 0o700
		}
		if err := os.Chmod(p, mode); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	})
}
