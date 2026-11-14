package cluster

import (
	"errors"
	"net/http"
	"time"

	"github.com/ao/hivemind/internal/logrelay"
	"github.com/ao/hivemind/internal/nodeapi"
)

// The log relay routes. The worker dials both, so it needs no listener of its
// own. The node ID comes from the client certificate, checked against the
// path by a.node, and the relay checks that a stream is for a request queued
// for that node.

// logRequests is the long poll: it answers 200 with the next log request for
// the node, or 204 when the poll time passes with none.
func (a *nodeAPI) logRequests(w http.ResponseWriter, r *http.Request) {
	id, ok := a.node(w, r)
	if !ok {
		return
	}
	q, err := a.logs.Next(r.Context(), id)
	switch {
	case errors.Is(err, logrelay.ErrBusy):
		writeError(w, http.StatusTooManyRequests, "this node already holds the most log polls the server allows")
	case errors.Is(err, logrelay.ErrClosed):
		writeError(w, http.StatusServiceUnavailable, "the server is shutting down")
	case err != nil:
		// The node went away.
	case q == nil:
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	default:
		writeJSON(w, http.StatusOK, q)
	}
}

// logStream takes the body of a POST as the log stream for one request, and
// holds the request open until the API side is done with it.
func (a *nodeAPI) logStream(w http.ResponseWriter, r *http.Request) {
	id, ok := a.node(w, r)
	if !ok {
		return
	}
	reqID := r.PathValue("req")
	if !nodeapi.ValidLogRequestID(reqID) {
		writeError(w, http.StatusBadRequest, "log request ID in the path is malformed")
		return
	}
	claim, err := a.logs.Claim(id, reqID, r.Body)
	switch {
	case errors.Is(err, logrelay.ErrWrongNode):
		a.log.Warn("log stream for another node's request refused", "node", id, "remote", r.RemoteAddr)
		writeError(w, http.StatusForbidden, "this log request is for another node")
		return
	case errors.Is(err, logrelay.ErrClaimed):
		writeError(w, http.StatusConflict, "this log request already has a stream")
		return
	case errors.Is(err, logrelay.ErrClosed):
		writeError(w, http.StatusServiceUnavailable, "the server is shutting down")
		return
	case err != nil:
		writeError(w, http.StatusNotFound, "no such log request; the user may have gone, or it timed out")
		return
	}
	// The stream lasts as long as the user reads it, past the listener's read
	// and write timeouts. The claim is checked first, so only a stream the
	// server asked for gets this.
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Time{}); err != nil {
		a.log.Warn("clear read deadline", "err", err)
	}
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		a.log.Warn("clear write deadline", "err", err)
	}
	// abort makes the next read of the body fail, so the API side stops at
	// once even when the worker sends nothing. The connection is then closed,
	// because the body is unread, and the worker's POST ends.
	claim.Wait(r.Context(), func() { _ = rc.SetReadDeadline(time.Now()) })
	w.WriteHeader(http.StatusNoContent)
}
