package nodeapi

import (
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"
)

// EpochHeader carries the cluster's epoch, which counts the restores of its
// store: 0 from init, and one more than the backup's after each restore. On
// every node API reply the server sends its store's epoch. On every request
// a worker sends the highest epoch it has seen from a server of its
// cluster. A server that sends none is older than epochs, and is taken as
// before.
const EpochHeader = "Hivemind-Epoch"

// maxEpochLen bounds the header's digits: an epoch is an int64.
const maxEpochLen = 19

// ParseEpoch reads an EpochHeader value: a decimal integer of 0 or more,
// without a sign. ok is false for "" and for anything else, which reads as
// no epoch.
func ParseEpoch(v string) (epoch int64, ok bool) {
	if v == "" || len(v) > maxEpochLen {
		return 0, false
	}
	for _, c := range v {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// FormatEpoch is the EpochHeader value of epoch.
func FormatEpoch(epoch int64) string { return strconv.FormatInt(epoch, 10) }

// StaleServerError is what a request to a server from before a restore
// returns: the server's epoch is lower than one the worker has seen from a
// server of its cluster. That server is a copy of the cluster that a
// restore replaced, such as the machine a backup was taken from, back on.
// It counts as unreachable: the agent leaves its containers as they are and
// backs off, as it does when the server is down.
type StaleServerError struct {
	// Server is the host:port the request went to.
	Server string
	// Epoch is the server's epoch, and Seen the highest the worker has
	// seen.
	Epoch, Seen int64
}

func (e *StaleServerError) Error() string {
	return fmt.Sprintf("the server at %s is from before a restore of this cluster: it is at epoch %d, and this worker has seen epoch %d",
		e.Server, e.Epoch, e.Seen)
}

// Unreachable reports true: the server this worker belongs to did not
// answer.
func (e *StaleServerError) Unreachable() bool { return true }

// DefaultStaleRetry is how long a client sends nothing to a server from
// before a restore, after its last reply, before it asks it again: the
// restored server may come back at that address.
const DefaultStaleRetry = 10 * time.Second

// EpochGuard keeps a worker's client off a server from before a restore.
// It holds the highest epoch the worker has seen, which every request
// carries. A reply whose epoch is lower is refused with a
// StaleServerError: the client logs that once, sends that server nothing
// for Retry, and then lets one request through to ask again. A reply with
// a higher epoch raises the highest, which Save writes to the worker's data
// directory. A reply without an epoch, from a server older than epochs, is
// taken as before. A guard is safe for concurrent use; give each client its
// own.
type EpochGuard struct {
	// Save writes a new highest epoch, so that it outlives the process. It
	// is called with the guard's lock held, so saves happen in order and
	// the saved value never goes down. A failure is logged, and the save
	// is tried again on a later reply, a minute later at most often. Nil
	// saves nothing.
	Save func(epoch int64) error
	// Log receives the line when the client refuses a server, and when it
	// takes it again. Nil means slog.Default().
	Log *slog.Logger
	// Retry is how long to send nothing to a server from before a restore.
	// Zero means DefaultStaleRetry.
	Retry time.Duration
	// Now is the clock. Nil means time.Now.
	Now func() time.Time

	mu        sync.Mutex
	highest   int64
	saved     int64
	saveTried time.Time // the last failed save, zero after a good one
	stale     *StaleServerError
	until     time.Time // while stale, no request goes out before it
}

// NewEpochGuard returns a guard that has seen highest, as read from the
// worker's data directory.
func NewEpochGuard(highest int64) *EpochGuard {
	return &EpochGuard{highest: max(highest, 0), saved: max(highest, 0)}
}

// Highest is the highest epoch the guard has seen.
func (g *EpochGuard) Highest() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.highest
}

func (g *EpochGuard) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

func (g *EpochGuard) retry() time.Duration {
	if g.Retry > 0 {
		return g.Retry
	}
	return DefaultStaleRetry
}

func (g *EpochGuard) log() *slog.Logger {
	if g.Log != nil {
		return g.Log
	}
	return slog.Default()
}

// hold returns the refusal while the server was found stale less than Retry
// ago, so the request is not sent. The first request after that is let
// through to ask again, and the others wait for its answer.
func (g *EpochGuard) hold() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stale == nil {
		return nil
	}
	now := g.now()
	if now.Before(g.until) {
		return g.stale
	}
	g.until = now.Add(g.retry())
	return nil
}

// check takes the EpochHeader of a reply from server. It returns a
// StaleServerError when the epoch is lower than the highest seen.
func (g *EpochGuard) check(server, header string) error {
	epoch, ok := ParseEpoch(header)
	g.mu.Lock()
	defer g.mu.Unlock()
	if ok && epoch < g.highest {
		first := g.stale == nil
		g.stale = &StaleServerError{Server: server, Epoch: epoch, Seen: g.highest}
		g.until = g.now().Add(g.retry())
		if first {
			g.log().Error("the server at "+server+" is from before a restore of this cluster: a restored copy of the cluster has been seen "+
				"at a higher epoch. This worker sends the old server nothing, keeps its containers running, and asks again every "+
				g.retry().String()+", as the restored server may come back at this address. Stop the old server; if this worker should dial "+
				"another address, stop the worker and move it with hivemind node set-server <address> (docs/backup.md)",
				"server", server, "server_epoch", epoch, "seen_epoch", g.highest)
		}
		return g.stale
	}
	if g.stale != nil {
		g.stale = nil
		if ok {
			g.log().Info("the server at "+server+" answers at this cluster's epoch now; this worker talks to it again",
				"server", server, "server_epoch", epoch)
		} else {
			g.log().Info("the server at "+server+" answers without an epoch now, as a build older than epochs does; this worker talks to it again",
				"server", server)
		}
	}
	if ok && epoch > g.highest {
		g.highest = epoch
	}
	g.saveLocked()
	return nil
}

// saveLocked saves the highest epoch when it is above the saved one, unless
// a save failed less than a minute ago.
func (g *EpochGuard) saveLocked() {
	if g.Save == nil || g.highest <= g.saved {
		return
	}
	now := g.now()
	if !g.saveTried.IsZero() && now.Sub(g.saveTried) < time.Minute {
		return
	}
	if err := g.Save(g.highest); err != nil {
		if g.saveTried.IsZero() {
			g.log().Warn("saving the cluster's epoch; this worker refuses a server from before the restore until it stops, "+
				"and after a restart only once a server at the new epoch has answered it again", "epoch", g.highest, "err", err)
		}
		g.saveTried = now
		return
	}
	g.saved, g.saveTried = g.highest, time.Time{}
}
