package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/ao/hivemind/internal/logrelay"
	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/scheduler"
	"github.com/ao/hivemind/internal/store"
)

// timeNow is the clock for the lost-node check. Tests move it.
var timeNow = time.Now

// replicaNode returns the node the replica is placed on, or "" when it has
// none or the store has no such replica.
func (s *Server) replicaNode(r *http.Request, app string, index int) (string, error) {
	reps, err := s.cfg.Store.Replicas(r.Context(), app)
	if err != nil {
		return "", err
	}
	for _, rp := range reps {
		if rp.Index == index {
			return rp.NodeID, nil
		}
	}
	return "", nil
}

// remoteLogs streams the log of a replica on another node, through that
// node's worker and the log relay. The worker sends stdout and stderr as
// separate frames; both go to the response body, as on the local path.
func (s *Server) remoteLogs(w http.ResponseWriter, r *http.Request, node string, q nodeapi.LogRequest) {
	what := fmt.Sprintf("app %q replica %d", q.App, q.Replica)
	if s.cfg.Logs == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Sprintf(
			"%s runs on node %s, and this server cannot reach workers: it runs without --advertise", what, node))
		return
	}
	n, err := s.cfg.Store.GetNode(r.Context(), node)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("%s is placed on node %s, which is not in the cluster", what, node))
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if age := timeNow().Sub(n.LastLease); age > scheduler.DefaultLeaseTimeout {
		writeError(w, http.StatusServiceUnavailable, fmt.Sprintf(
			"%s runs on node %s, which is lost: it last reported %s ago, so its logs cannot be read",
			what, node, age.Round(time.Second)))
		return
	}

	ctx, done := s.logContext(w, r, q.Follow)
	defer done()
	st, err := s.cfg.Logs.Open(ctx, node, q)
	switch {
	case errors.Is(err, logrelay.ErrBusy):
		writeError(w, http.StatusTooManyRequests, fmt.Sprintf(
			"node %s already has %d log streams open; end one and try again", node, s.cfg.Logs.MaxStreamsPerNode()))
		return
	case errors.Is(err, logrelay.ErrNoAnswer):
		writeError(w, http.StatusGatewayTimeout, s.noAnswer(n))
		return
	case errors.Is(err, logrelay.ErrClosed):
		writeError(w, http.StatusServiceUnavailable, "the server is shutting down")
		return
	case err != nil:
		// The user went away.
		return
	}
	defer st.Close()
	// A user who goes away, or a shutdown, cuts the worker's stream at once.
	stop := context.AfterFunc(ctx, st.Cancel)
	defer stop()

	// The first frame says whether the node found the container. It must come
	// within the pickup time too.
	fr := nodeapi.NewFrameReader(st)
	timer := time.AfterFunc(s.cfg.Logs.PickupTimeout(), st.Cancel)
	typ, _, err := fr.Next()
	if !timer.Stop() {
		// The timer cut the stream, so whatever Next read, it is over.
		err = logrelay.ErrNoAnswer
	}
	if err != nil {
		switch {
		case ctx.Err() != nil:
		case errors.Is(err, logrelay.ErrNoAnswer):
			writeError(w, http.StatusGatewayTimeout, s.noAnswer(n))
		default:
			s.log.Warn("log stream from a worker broke before it started", "node", node, "err", err)
			writeError(w, http.StatusBadGateway, fmt.Sprintf("node %s ended the log stream before it started", node))
		}
		return
	}
	if typ != nodeapi.FrameReady {
		msg, _ := fr.Message()
		switch typ {
		case nodeapi.FrameNotFound:
			writeError(w, http.StatusNotFound, fmt.Sprintf("%s has no container on node %s", what, node))
		case nodeapi.FrameUnavailable:
			s.log.Error("container engine unavailable on a node", "node", node, "err", clean(msg))
			writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("the container engine on node %s is unavailable", node))
		default:
			s.log.Warn("a node could not read a log", "node", node, "type", typ, "err", clean(msg))
			writeError(w, http.StatusBadGateway, fmt.Sprintf("node %s could not read the log: %s", node, clean(msg)))
		}
		return
	}

	fw := startLog(w)
	for {
		typ, _, err := fr.Next()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			if ctx.Err() == nil {
				s.log.Warn("log stream from a node ended with an error", "node", node, "app", q.App,
					"replica", q.Replica, "err", err)
			}
			return
		}
		switch typ {
		case nodeapi.FrameStdout, nodeapi.FrameStderr:
			if err := fr.Copy(fw); err != nil {
				if ctx.Err() == nil {
					s.log.Warn("copying a node's log stream", "node", node, "err", err)
				}
				return
			}
		case nodeapi.FrameFailed, nodeapi.FrameUnavailable:
			msg, _ := fr.Message()
			// The status line is sent, so the error can only be logged.
			s.log.Warn("log stream ended with an error", "node", node, "app", q.App, "replica", q.Replica,
				"err", clean(msg))
			return
		default:
			s.log.Warn("a node sent an unexpected log frame", "node", node, "type", typ)
			return
		}
	}
}

// neverPolledAfter is how long after serve starts a live worker that has
// not polled for log requests is taken to have no log streaming. A worker
// that has it polls again within half a minute of losing the server. Tests
// change it.
var neverPolledAfter = time.Minute

// noAnswer says why worker n did not take a log request. A worker that
// reports no version and has not polled since the server started is older
// than 2.1, which added log streaming, so the message says to upgrade it.
func (s *Server) noAnswer(n store.Node) string {
	d := s.cfg.Logs.PickupTimeout()
	switch {
	case n.Version == "" && !s.cfg.Logs.Polled(n.ID) && timeNow().Sub(s.cfg.Logs.Started()) >= neverPolledAfter:
		return fmt.Sprintf("worker %s runs a hivemind older than 2.1, which cannot stream logs; upgrade it", n.ID)
	case n.Version != "":
		return fmt.Sprintf("worker %s, which runs %s, did not answer within %s; is it running?", n.ID, n.Version, d)
	}
	return fmt.Sprintf("worker %s did not answer within %s; is it running?", n.ID, d)
}

// clean makes a node's message safe to log and to show: no control
// characters, and at most 512 bytes.
func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	if len(s) > 512 {
		s = s[:512] + "..."
	}
	return s
}
