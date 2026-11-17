package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/ao/hivemind/internal/nodeapi"
)

// workerEpochFile holds the highest cluster epoch the worker has seen from a
// server of its cluster, as a decimal number and a newline. A missing file
// is epoch 0: the worker has seen no restore, or joined before epochs.
//
// Two things write it, and both hold the data directory's lock, so they
// never run at once: the worker's node API client, whenever a server
// answers it at a higher epoch, and set-server, once it has taken a server
// at a higher epoch. The renewer, in the worker's process, writes only the
// certificate's files, and set-server only node.json besides this one. A
// write goes to workerEpochNext, is synced, and is renamed over the file,
// so after a crash it holds the old epoch or the new one.
const (
	workerEpochFile = "cluster-epoch"
	workerEpochNext = "cluster-epoch.next"
)

// staleServerRetry is how long a worker sends nothing to a server from
// before a restore, before it asks it again. Tests shorten it.
var staleServerRetry = nodeapi.DefaultStaleRetry

// readWorkerEpoch reads the epoch in dir's workerEpochFile, or 0 when there
// is none.
func readWorkerEpoch(dir string) (int64, error) {
	p := filepath.Join(dir, workerEpochFile)
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	e, ok := nodeapi.ParseEpoch(strings.TrimSpace(string(b)))
	if !ok {
		return 0, fmt.Errorf("%s does not hold an epoch: %q", p, strings.TrimSpace(string(b)))
	}
	return e, nil
}

// saveWorkerEpoch replaces dir's workerEpochFile with epoch.
func saveWorkerEpoch(dir string, epoch int64) error {
	next := filepath.Join(dir, workerEpochNext)
	// A staging file is left only by a write that stopped before its
	// rename; the file is still the old one then.
	if err := os.Remove(next); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := writeSynced(next, []byte(nodeapi.FormatEpoch(epoch)+"\n")); err != nil {
		return err
	}
	if err := os.Rename(next, filepath.Join(dir, workerEpochFile)); err != nil {
		_ = os.Remove(next)
		return err
	}
	return syncDir(dir)
}

// workerEpochGuard is the epoch guard of the worker whose data directory is
// dir: it starts from the epoch saved there, and saves each higher one. A
// file that cannot be read is logged, and the guard starts from 0; the next
// reply from a server at a higher epoch writes the file again.
func workerEpochGuard(dir string, log *slog.Logger) *nodeapi.EpochGuard {
	seen, err := readWorkerEpoch(dir)
	if err != nil {
		log.Warn("reading the cluster epoch this worker has seen; it starts from 0, and takes the epoch of the next server that answers",
			"err", err)
	}
	g := nodeapi.NewEpochGuard(seen)
	g.Save = func(e int64) error { return saveWorkerEpoch(dir, e) }
	g.Log = log
	g.Retry = staleServerRetry
	return g
}

// staleServerError explains, for set-server, why the server at server is
// refused: it is from before a restore.
func staleServerError(stale *nodeapi.StaleServerError, server string) error {
	return fmt.Errorf("the server at %s is from before a restore of this cluster: it is at epoch %d, and this worker has seen epoch %d "+
		"from the restored server. Stop the old server, and give set-server the address of the restored one", server, stale.Epoch, stale.Seen)
}
