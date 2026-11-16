package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/runtime"
	"github.com/ao/hivemind/internal/spec"
	"github.com/ao/hivemind/internal/store"
)

// AppView is the API's view of one app.
type AppView struct {
	Name       string    `json:"name"`
	Generation int       `json:"generation"`
	Replicas   int       `json:"replicas"`
	Spec       spec.App  `json:"spec"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	// Origin names what deployed the app, such as "compose:shop". It is
	// absent for an app deployed without one.
	Origin string `json:"origin,omitempty"`
}

// PutResult is the reply to a create or update.
type PutResult struct {
	App     AppView `json:"app"`
	Created bool    `json:"created"`
}

// ScaleRequest is the body of POST /v1/apps/{name}/scale.
type ScaleRequest struct {
	Replicas int `json:"replicas"`
}

// stateUnknown is a replica's state when its node has not reported to this
// server process yet. The container ID is the one last reported.
const stateUnknown = "unknown"

// staleReport reports whether a container report was made before this server
// process started.
func (s *Server) staleReport(updated time.Time) bool {
	return !s.cfg.Started.IsZero() && updated.Before(s.cfg.Started)
}

// ReplicaStatus is one replica and the state of its container.
type ReplicaStatus struct {
	Index         int    `json:"index"`
	NodeID        string `json:"node_id"`
	Generation    int    `json:"generation"`
	Desired       string `json:"desired"`
	PendingReason string `json:"pending_reason,omitempty"`
	State         string `json:"state"`
	ContainerID   string `json:"container_id,omitempty"`
	Error         string `json:"error,omitempty"`
	// Restarts counts the agent's restarts of this container after it exited or
	// failed its health check.
	Restarts int `json:"restarts"`
	// Health is "", "starting", "healthy" or "unhealthy"; "" means no check.
	Health string `json:"health,omitempty"`
	Ready  bool   `json:"ready"`
	// CrashLooping is set while a restart waits out its backoff, and
	// NextRestart says when it is due.
	CrashLooping bool       `json:"crash_looping,omitempty"`
	NextRestart  *time.Time `json:"next_restart,omitempty"`
}

// AppStatus is one app's replicas.
type AppStatus struct {
	Name       string `json:"name"`
	Image      string `json:"image"`
	Generation int    `json:"generation"`
	Desired    int    `json:"desired"`
	// Running counts the replicas that should run whose container is
	// running, at the replica's own generation: during a rollout an old copy
	// counts until it is replaced. Ready counts those of them that are ready:
	// passing their health check, or with none, and not crash-looping.
	Running  int             `json:"running"`
	Ready    int             `json:"ready"`
	Replicas []ReplicaStatus `json:"replicas"`
	// Rollout is set while a template change is rolling out, or after one stopped.
	Rollout *RolloutStatus `json:"rollout,omitempty"`
}

// RolloutStatus is an unfinished rollout of an app to a new generation.
type RolloutStatus struct {
	Target int    `json:"target"`
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
	// Moving is the replica being moved, or -1.
	Moving int `json:"moving"`
}

// ErrorBody is every error reply.
type ErrorBody struct {
	Error string `json:"error"`
}

func view(a store.App) AppView {
	return AppView{Name: a.Spec.Name, Generation: a.Generation, Replicas: a.Spec.Replicas, Spec: a.Spec,
		CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt, Origin: a.Origin}
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) listApps(w http.ResponseWriter, r *http.Request) {
	apps, err := s.cfg.Store.ListApps(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]AppView, 0, len(apps))
	for _, a := range apps {
		out = append(out, view(a))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getApp(w http.ResponseWriter, r *http.Request) {
	name, ok := pathName(w, r)
	if !ok {
		return
	}
	a, err := s.cfg.Store.GetApp(r.Context(), name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view(a))
}

// postApp creates or updates the app named in the body.
func (s *Server) postApp(w http.ResponseWriter, r *http.Request) {
	a, err := DecodeApp(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		writeDecodeError(w, err)
		return
	}
	s.put(w, r, a)
}

// putApp creates or updates the app named in the path. A name in the body
// must match it.
func (s *Server) putApp(w http.ResponseWriter, r *http.Request) {
	name, ok := pathName(w, r)
	if !ok {
		return
	}
	a, err := decodeAppRaw(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		writeDecodeError(w, err)
		return
	}
	if a.Name == "" {
		a.Name = name
	}
	if a.Name != name {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("the body names app %q but the path names %q", a.Name, name))
		return
	}
	if err := normalizeValidate(&a); err != nil {
		writeDecodeError(w, err)
		return
	}
	s.put(w, r, a)
}

// put stores the app. The query may name an origin, origin=compose:shop,
// which refuses with 409 an app that exists with another origin or with none,
// unless it also has replace=true. See store.PutAppWith.
func (s *Server) put(w http.ResponseWriter, r *http.Request, a spec.App) {
	q := r.URL.Query()
	opts := store.PutOptions{Origin: q.Get("origin")}
	switch q.Get("replace") {
	case "", "false":
	case "true":
		opts.Replace = true
	default:
		writeError(w, http.StatusBadRequest, "the replace parameter must be true or false")
		return
	}
	_, err := s.cfg.Store.GetApp(r.Context(), a.Name)
	created := errors.Is(err, store.ErrNotFound)
	if err != nil && !created {
		s.fail(w, r, err)
		return
	}
	got, err := s.cfg.Store.PutAppWith(r.Context(), a, opts)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.cfg.Trigger()
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	writeJSON(w, code, PutResult{App: view(got), Created: created})
}

func (s *Server) scaleApp(w http.ResponseWriter, r *http.Request) {
	name, ok := pathName(w, r)
	if !ok {
		return
	}
	var req ScaleRequest
	if err := decodeStrict(http.MaxBytesReader(w, r.Body, MaxBodyBytes), &req); err != nil {
		writeDecodeError(w, err)
		return
	}
	if req.Replicas < 1 || req.Replicas > spec.MaxReplicas {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("replicas %d: must be between 1 and %d", req.Replicas, spec.MaxReplicas))
		return
	}
	a, err := s.cfg.Store.Scale(r.Context(), name, req.Replicas)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.cfg.Trigger()
	writeJSON(w, http.StatusOK, view(a))
}

func (s *Server) deleteApp(w http.ResponseWriter, r *http.Request) {
	name, ok := pathName(w, r)
	if !ok {
		return
	}
	if err := s.cfg.Store.DeleteApp(r.Context(), name); err != nil {
		s.fail(w, r, err)
		return
	}
	s.cfg.Trigger()
	writeJSON(w, http.StatusAccepted, map[string]string{"name": name, "status": "deleting"})
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	out, err := s.statusOf(r, "")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) appStatus(w http.ResponseWriter, r *http.Request) {
	name, ok := pathName(w, r)
	if !ok {
		return
	}
	if _, err := s.cfg.Store.GetApp(r.Context(), name); err != nil {
		s.fail(w, r, err)
		return
	}
	out, err := s.statusOf(r, name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(out) == 0 {
		s.fail(w, r, fmt.Errorf("app %q: %w", name, store.ErrNotFound))
		return
	}
	writeJSON(w, http.StatusOK, out[0])
}

// statusOf joins the desired replicas with what the nodes reported. A
// replica's state comes from the container at its generation; with none, it
// is "pending".
func (s *Server) statusOf(r *http.Request, app string) ([]AppStatus, error) {
	ctx := r.Context()
	var apps []store.App
	if app == "" {
		var err error
		if apps, err = s.cfg.Store.ListApps(ctx); err != nil {
			return nil, err
		}
	} else {
		a, err := s.cfg.Store.GetApp(ctx, app)
		if err != nil {
			return nil, err
		}
		apps = []store.App{a}
	}
	reps, err := s.cfg.Store.Replicas(ctx, app)
	if err != nil {
		return nil, err
	}
	sts, err := s.cfg.Store.ListStatus(ctx, app)
	if err != nil {
		return nil, err
	}
	// A container report is matched by node as well as by replica and
	// generation. A lost node's last report must not describe a replica that
	// has moved, or that is pending on another node.
	type key struct {
		node, app string
		idx, gen  int
	}
	byKey := map[key]store.Status{}
	for _, st := range sts {
		byKey[key{st.NodeID, st.App, st.Replica, st.Generation}] = st
	}
	out := make([]AppStatus, 0, len(apps))
	for _, a := range apps {
		as := AppStatus{Name: a.Spec.Name, Image: a.Spec.Image, Generation: a.Generation, Desired: a.Spec.Replicas,
			Replicas: []ReplicaStatus{}}
		for _, rp := range reps {
			if rp.App != a.Spec.Name {
				continue
			}
			rs := ReplicaStatus{Index: rp.Index, NodeID: rp.NodeID, Generation: rp.Generation, Desired: string(rp.Desired),
				PendingReason: rp.PendingReason, State: "pending"}
			if rp.NodeID != "" {
				if st, ok := byKey[key{rp.NodeID, rp.App, rp.Index, rp.Generation}]; ok {
					rs.State, rs.ContainerID, rs.Error = st.State, st.ContainerID, st.Error
					rs.Restarts, rs.Health, rs.Ready, rs.CrashLooping = st.RestartCount, st.Health, st.Ready, st.CrashLooping
					if !st.NextRestart.IsZero() {
						nr := st.NextRestart.UTC()
						rs.NextRestart = &nr
					}
					// A report made to an earlier server process says what
					// was true then. After a restore on a new machine the
					// server's own containers are gone, so it is not shown
					// as the state until the node reports to this process.
					if s.staleReport(st.UpdatedAt) {
						rs.State, rs.Ready, rs.Health, rs.Error = stateUnknown, false, "", ""
						rs.CrashLooping, rs.NextRestart = false, nil
					}
				}
			}
			if rp.Desired == store.Running && rs.State == "running" {
				as.Running++
				if rs.Ready && !rs.CrashLooping {
					as.Ready++
				}
			}
			as.Replicas = append(as.Replicas, rs)
		}
		ro, err := s.cfg.Store.GetRollout(ctx, a.Spec.Name)
		switch {
		case errors.Is(err, store.ErrNoRollout):
		case err != nil:
			return nil, err
		case ro.State != store.RolloutDone:
			as.Rollout = &RolloutStatus{Target: ro.Target, State: string(ro.State), Reason: ro.Reason, Moving: ro.Moving}
		}
		out = append(out, as)
	}
	return out, nil
}

// appLogs streams one replica's container log. The query takes replica
// (default 0), tail (lines from the end, 0 for all) and follow (true to keep
// the stream open). A replica placed on another node is read through that
// node's worker (see remoteLogs). Any other replica, the server's own and one
// with no node, is read from the local runtime.
func (s *Server) appLogs(w http.ResponseWriter, r *http.Request) {
	name, ok := pathName(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	replica, err := queryInt(q.Get("replica"), 0, spec.MaxReplicas-1)
	if err != nil {
		writeError(w, http.StatusBadRequest, "replica: "+err.Error())
		return
	}
	tail, err := queryInt(q.Get("tail"), 0, maxTail)
	if err != nil {
		writeError(w, http.StatusBadRequest, "tail: "+err.Error())
		return
	}
	follow := false
	if v := q.Get("follow"); v != "" {
		if follow, err = strconv.ParseBool(v); err != nil {
			writeError(w, http.StatusBadRequest, "follow: want true or false")
			return
		}
	}
	if _, err := s.cfg.Store.GetApp(r.Context(), name); err != nil {
		s.fail(w, r, err)
		return
	}
	node, err := s.replicaNode(r, name, replica)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if node != "" && node != s.cfg.NodeID {
		s.remoteLogs(w, r, node, nodeapi.LogRequest{App: name, Replica: replica, Tail: tail, Follow: follow})
		return
	}
	cs, err := s.cfg.Runtime.List(r.Context(), map[string]string{
		runtime.LabelApp: name, runtime.LabelReplica: strconv.Itoa(replica)})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(cs) == 0 {
		writeError(w, http.StatusNotFound, fmt.Sprintf("app %q replica %d has no container on this node", name, replica))
		return
	}
	c := runtime.PickContainer(cs)

	ctx, done := s.logContext(w, r, follow)
	defer done()
	fw := startLog(w)
	err = s.cfg.Runtime.Logs(ctx, c.ID, runtime.LogOptions{Follow: follow, Tail: tail}, fw, fw)
	if err != nil && ctx.Err() == nil {
		// The status line is sent, so the error can only be logged.
		s.log.Warn("log stream ended with an error", "app", name, "replica", replica, "err", err)
	}
}

// logContext is the context of a log response. A followed stream outlives
// WriteTimeout, so its write deadline is cleared, and CloseStreams ends it at
// shutdown.
func (s *Server) logContext(w http.ResponseWriter, r *http.Request, follow bool) (context.Context, func()) {
	if !follow {
		return r.Context(), func() {}
	}
	ctx, cancel := mergeCancel(r.Context(), s.streams)
	if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil {
		s.log.Warn("clear write deadline", "err", err)
	}
	return ctx, cancel
}

// startLog sends the headers of a log response, and returns the writer for
// its body. The headers are flushed at once, so a client of a followed log
// that is quiet knows the stream has started.
func startLog(w http.ResponseWriter) *flushWriter {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.Flush()
	return &flushWriter{w: w, rc: rc}
}

// flushWriter flushes after every write, so a followed log reaches the client
// line by line. The runtime may write stdout and stderr from separate calls;
// the mutex keeps them from interleaving inside one write.
type flushWriter struct {
	mu sync.Mutex
	w  io.Writer
	rc *http.ResponseController
}

func (f *flushWriter) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, err := f.w.Write(p)
	if err == nil {
		_ = f.rc.Flush()
	}
	return n, err
}

func queryInt(v string, lo, hi int) (int, error) {
	if v == "" {
		return lo, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		return 0, fmt.Errorf("want an integer from %d to %d", lo, hi)
	}
	return n, nil
}

func pathName(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("name")
	if !spec.ValidName(name) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("app name %q: use lowercase letters, digits and dashes", name))
		return "", false
	}
	return name, true
}

// fail maps a store or runtime error to a status. Unexpected errors are
// logged and the client gets a generic message.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, runtime.ErrUnavailable):
		s.log.Error("container engine unavailable", "method", r.Method, "path", r.URL.Path, "err", err)
		writeError(w, http.StatusServiceUnavailable, "the container engine is unavailable")
	case r.Context().Err() != nil:
		// The client went away; there is no one to answer.
	default:
		s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// requestError is a decoding or validation failure, reported to the client
// with its status.
type requestError struct {
	code int
	msg  string
}

func (e *requestError) Error() string { return e.msg }

func writeDecodeError(w http.ResponseWriter, err error) {
	var re *requestError
	if errors.As(err, &re) {
		writeError(w, re.code, re.msg)
		return
	}
	writeError(w, http.StatusBadRequest, err.Error())
}

// DecodeApp reads one app spec from a request body, normalises it and
// validates it. Unknown fields, trailing data and bodies over MaxBodyBytes
// are errors. The returned error carries the HTTP status to reply with.
func DecodeApp(body io.Reader) (spec.App, error) {
	a, err := decodeAppRaw(body)
	if err != nil {
		return spec.App{}, err
	}
	if err := normalizeValidate(&a); err != nil {
		return spec.App{}, err
	}
	return a, nil
}

func decodeAppRaw(body io.Reader) (spec.App, error) {
	var a spec.App
	if err := decodeStrict(body, &a); err != nil {
		return spec.App{}, err
	}
	return a, nil
}

func normalizeValidate(a *spec.App) error {
	a.Normalize()
	if err := a.Validate(); err != nil {
		return &requestError{http.StatusBadRequest, "invalid app spec: " + err.Error()}
	}
	return nil
}

// decodeStrict decodes exactly one JSON value into v.
func decodeStrict(body io.Reader, v any) error {
	// Read through a limit as well, so DecodeApp is bounded when it is called
	// without http.MaxBytesReader.
	data, err := io.ReadAll(io.LimitReader(body, MaxBodyBytes+1))
	if err != nil {
		var mb *http.MaxBytesError
		if errors.As(err, &mb) {
			return &requestError{http.StatusRequestEntityTooLarge, fmt.Sprintf("request body is larger than %d bytes", MaxBodyBytes)}
		}
		return &requestError{http.StatusBadRequest, "reading request body failed"}
	}
	if len(data) > MaxBodyBytes {
		return &requestError{http.StatusRequestEntityTooLarge, fmt.Sprintf("request body is larger than %d bytes", MaxBodyBytes)}
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return &requestError{http.StatusBadRequest, "request body is empty"}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return &requestError{http.StatusBadRequest, "invalid JSON: " + err.Error()}
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return &requestError{http.StatusBadRequest, "invalid JSON: data after the first value"}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		code, b = http.StatusInternalServerError, []byte(`{"error":"internal error"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write(append(b, '\n'))
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, ErrorBody{Error: msg})
}
