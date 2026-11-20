package cluster

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ao/hivemind/internal/carotate"
	"github.com/ao/hivemind/internal/logrelay"
	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/store"
)

// nodeAPI serves the node routes. The mTLS listener has already checked that
// the client certificate chains to the cluster CA and names a node. Each
// handler checks that the node in the certificate is the node in the path, so
// a node can read and report for itself only.
type nodeAPI struct {
	st   *store.Store
	log  *slog.Logger
	logs *logrelay.Relay
	// routers keeps the workers' router reports (routermetrics.go).
	routers *nodeapi.RouterReports
	// readers keeps how each router read the routing table (routes.go).
	readers *nodeapi.RouteReaders
	// rot, lifetime and now issue renewed certificates (renew.go); rot
	// also says which CAs a client certificate may be from now, and holds
	// the CA set the workers follow (ca.go).
	rot      *carotate.Manager
	lifetime time.Duration
	now      func() time.Time
	certs    *certNotes
	// versions is the version this process last stored for each node, so
	// the store is written only when a node's version changes.
	mu       sync.Mutex
	versions map[string]string
}

// nodeRoutes returns the handler of the mTLS listener. It has no join route.
// Every request passes the denylist check first, then the check that the
// client certificate is valid now. Every reply carries the server's version
// and epoch, and a node that has seen a higher epoch is refused first.
func nodeRoutes(cfg Config, log *slog.Logger) http.Handler {
	a := &nodeAPI{st: cfg.Store, log: log, logs: cfg.Logs, routers: cfg.RouterReports, readers: cfg.RouteReaders,
		rot: cfg.Rotation, lifetime: cfg.NodeCertLifetime, now: cfg.now(), certs: &certNotes{}, versions: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+nodeapi.Prefix+"/nodes/{id}/register", a.register)
	mux.HandleFunc("GET "+nodeapi.Prefix+"/nodes/{id}/assignments", a.assignments)
	mux.HandleFunc("POST "+nodeapi.Prefix+"/nodes/{id}/status", a.postStatus)
	mux.HandleFunc("GET "+nodeapi.Prefix+"/nodes/{id}/status", a.getStatus)
	mux.HandleFunc("POST "+nodeapi.Prefix+"/nodes/{id}/lease", a.lease)
	mux.HandleFunc("POST "+nodeapi.Prefix+"/nodes/{id}/renew", a.renew)
	mux.HandleFunc("GET "+nodeapi.Prefix+"/nodes/{id}/logs/requests", a.logRequests)
	mux.HandleFunc("POST "+nodeapi.Prefix+"/nodes/{id}/logs/streams/{req}", a.logStream)
	mux.HandleFunc("GET "+nodeapi.Prefix+"/nodes/{id}/routes", a.routes)
	mux.HandleFunc("POST "+nodeapi.Prefix+"/nodes/{id}/router-metrics", a.routerMetrics)
	mux.HandleFunc("POST "+nodeapi.Prefix+"/nodes/{id}/tls/{op}", a.tls)
	mux.HandleFunc("GET "+nodeapi.Prefix+"/nodes/{id}/ca", a.caSet)
	return withVersion(nodeapi.CleanVersion(cfg.Version), withCADigest(a.rot,
		withEpoch(cfg.epochRef(), log, a.denyRemoved(a.requireCurrentCert(a.checkCA(mux))))))
}

// withVersion sets the server's version on every reply. A worker learns from
// it that the server was upgraded, and that a 404 without the
// not-registered mark is a route the server lacks (see nodeapi).
func withVersion(v string, next http.Handler) http.Handler {
	if v == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(nodeapi.VersionHeader, v)
		next.ServeHTTP(w, r)
	})
}

// staleMessage is the error a server from before a restore answers a node
// with, when the node has seen a higher epoch.
const staleMessage = "this server is from before a restore of the cluster: this node has seen a higher epoch, from the restored server"

// withEpoch sets the server's epoch on every reply, and refuses, with 409
// and before anything reads the store, every request from a node that says
// it has seen a higher epoch. Such a node has been in touch with a server
// restored from a backup of this one, or of a later copy: this server is
// from before that restore, and a newer copy of the cluster runs elsewhere.
// That is logged once, at error, with what to do. A node older than epochs
// sends none, and is served as before.
func withEpoch(epochOf *atomic.Int64, log *slog.Logger, next http.Handler) http.Handler {
	var logged atomic.Bool
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		epoch := epochOf.Load()
		w.Header().Set(nodeapi.EpochHeader, nodeapi.FormatEpoch(epoch))
		seen, ok := nodeapi.ParseEpoch(r.Header.Get(nodeapi.EpochHeader))
		if !ok || seen <= epoch {
			next.ServeHTTP(w, r)
			return
		}
		id, _ := pki.RequestNodeID(r)
		if logged.CompareAndSwap(false, true) {
			log.Error("a newer copy of this cluster exists: this server was replaced by a restore, and its store is from before it. "+
				"It refuses every node that has seen the newer copy, and its own replicas and router run on a stale view of the cluster. "+
				"Stop this server and disable its service (docs/backup.md)",
				"node", id, "node_epoch", seen, "epoch", epoch, "remote", r.RemoteAddr)
		} else {
			log.Debug("request from a node that has seen a newer copy of this cluster refused", "node", id, "node_epoch", seen, "epoch", epoch)
		}
		writeError(w, http.StatusConflict, staleMessage)
	})
}

// nodeVersion is the version the request says the node runs, or "".
func nodeVersion(r *http.Request) string {
	return nodeapi.CleanVersion(r.Header.Get(nodeapi.VersionHeader))
}

// noteVersion stores the version the node sent with a lease or a report,
// when it differs from the one this process last stored for it: once per
// node after the server starts, and after the node is upgraded. A failure is
// logged; the lease has been renewed all the same.
func (a *nodeAPI) noteVersion(r *http.Request, id string) {
	v := nodeVersion(r)
	a.mu.Lock()
	old, ok := a.versions[id]
	a.mu.Unlock()
	if ok && old == v {
		return
	}
	if err := a.st.SetNodeVersion(r.Context(), id, v); err != nil {
		if r.Context().Err() == nil {
			a.log.Warn("recording a node's version", "node", id, "err", err)
		}
		return
	}
	a.rememberVersion(id, v)
}

func (a *nodeAPI) rememberVersion(id, v string) {
	a.mu.Lock()
	a.versions[id] = v
	a.mu.Unlock()
}

// denyRemoved refuses, with 403, every request whose client certificate names
// a node that was removed. There is no certificate revocation list: a removed
// node's certificate still verifies against the CA, so the TLS handshake
// succeeds and this check is what makes the certificate useless. It runs on
// every request, before routing, so no route can forget it. The store refuses
// the ID as well when a node registers or reports, so a request that races a
// removal cannot bring the node back.
func (a *nodeAPI) denyRemoved(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := pki.RequestNodeID(r)
		if err != nil {
			// The listener requires a verified certificate that names a
			// node, so this does not happen; refuse it all the same.
			writeError(w, http.StatusForbidden, "the client certificate names no node")
			return
		}
		removed, err := a.st.IsNodeRemoved(r.Context(), id)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		if removed {
			a.log.Warn("request from a removed node refused", "node", id, "remote", r.RemoteAddr, "path", r.URL.Path)
			writeError(w, http.StatusForbidden, nodeapi.RemovedMessage)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// node returns the node ID in the path once the certificate has been checked
// against it. On failure it has already written the reply.
func (a *nodeAPI) node(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if err := pki.ValidateNodeID(id); err != nil {
		writeError(w, http.StatusBadRequest, "node ID in the path is malformed")
		return "", false
	}
	if err := pki.CheckRequestNodeID(r, id); err != nil {
		a.log.Warn("node request refused", "remote", r.RemoteAddr, "path", r.URL.Path, "reason", err.Error())
		writeError(w, http.StatusForbidden, "the client certificate does not name this node")
		return "", false
	}
	return id, true
}

// register records the node's name, capacity, address and the server address
// it dials, and renews its lease.
func (a *nodeAPI) register(w http.ResponseWriter, r *http.Request) {
	id, ok := a.node(w, r)
	if !ok {
		return
	}
	var g nodeapi.Register
	if err := nodeapi.Decode(r.Body, nodeapi.MaxRegisterBytes, &g); err != nil {
		writeDecodeError(w, err)
		return
	}
	if err := g.Validate(); err != nil {
		writeDecodeError(w, err)
		return
	}
	v := nodeVersion(r)
	if err := a.st.UpsertNode(r.Context(), store.Node{ID: id, Name: g.Name, CPU: g.CPU, Memory: g.Memory, Address: g.Address, Version: v,
		Dials: g.Server}); err != nil {
		a.fail(w, r, err)
		return
	}
	a.rememberVersion(id, v)
	w.WriteHeader(http.StatusNoContent)
}

// lease renews the node's lease. It takes no body: an agent calls it on a
// timer, apart from its passes.
func (a *nodeAPI) lease(w http.ResponseWriter, r *http.Request) {
	id, ok := a.node(w, r)
	if !ok {
		return
	}
	if n, _ := io.Copy(io.Discard, io.LimitReader(r.Body, 1)); n != 0 {
		writeError(w, http.StatusBadRequest, "the lease route takes no body")
		return
	}
	if err := a.st.RenewLease(r.Context(), id); err != nil {
		a.fail(w, r, err)
		return
	}
	a.noteVersion(r, id)
	w.WriteHeader(http.StatusNoContent)
}

// assignments returns every replica placed on the node, marked for removal
// included, as the agent expects. A node that asks for registry credentials
// (2.2 and later) gets the one for each replica's image that is to run, if
// one is stored; a node only ever reads its own assignments, so a credential
// goes only to nodes that run an image from its registry.
func (a *nodeAPI) assignments(w http.ResponseWriter, r *http.Request) {
	id, ok := a.node(w, r)
	if !ok {
		return
	}
	as, err := a.st.AssignmentsForNode(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	withAuth := r.URL.Query().Get(nodeapi.RegistryAuthParam) == "1"
	writeJSON(w, http.StatusOK, nodeapi.AssignmentList{Assignments: nodeapi.FromStore(as, withAuth)})
}

// postStatus replaces what the node reported with the body, and renews its
// lease. A node that never registered gets 404.
func (a *nodeAPI) postStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := a.node(w, r)
	if !ok {
		return
	}
	var rep nodeapi.StatusReport
	if err := nodeapi.Decode(r.Body, nodeapi.MaxStatusBytes, &rep); err != nil {
		writeDecodeError(w, err)
		return
	}
	if err := rep.Validate(); err != nil {
		writeDecodeError(w, err)
		return
	}
	if err := a.st.ReportStatus(r.Context(), id, rep.ToStore()); err != nil {
		a.fail(w, r, err)
		return
	}
	a.noteVersion(r, id)
	w.WriteHeader(http.StatusNoContent)
}

// getStatus returns what the node last reported. An agent reads it at start
// to recover its restart counts.
func (a *nodeAPI) getStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := a.node(w, r)
	if !ok {
		return
	}
	sts, err := a.st.StatusForNode(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, nodeapi.StatusFromStore(sts))
}

// fail maps a store error to a reply. The details of an unexpected error go
// to the log only.
//
// A write lost to a change of leader gets the API's 503 and message
// (internal/server's fail), and a warning: with ErrNotLeader nothing was
// written, and the reply is marked no leader, so that a worker sends it to
// another server; with ErrOutcomeUnknown the write may yet be applied, and
// the agent's next pass sends its report or renewal again. Before, both
// fell to the 500 below and an error line, on the old leader at every
// change of leader (ninth soak), and a worker did not move on from a 500.
func (a *nodeAPI) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotLeader):
		a.log.Warn("a node's request was refused: this server does not lead now", "path", r.URL.Path, "err", err)
		w.Header().Set(nodeapi.NoLeaderHeader, "1")
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, store.ErrOutcomeUnknown):
		a.log.Warn("a node's write may or may not have been applied: the leader changed while it was in the log", "path", r.URL.Path, "err", err)
		writeError(w, http.StatusServiceUnavailable, err.Error()+": check, and try again if it is not")
	case errors.Is(err, store.ErrNodeRemoved):
		writeError(w, http.StatusForbidden, nodeapi.RemovedMessage)
	case errors.Is(err, store.ErrNotFound):
		// The mark tells this 404 from one for a route the server lacks,
		// which a worker newer than the server gets (nodeapi.ErrNoRoute).
		w.Header().Set(nodeapi.ErrorHeader, nodeapi.ErrorNotRegistered)
		writeError(w, http.StatusNotFound, "this node is not registered")
	case errors.Is(err, store.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid request")
	case r.Context().Err() != nil:
		// The node went away; there is no one to answer.
	case store.IsDiskFull(err):
		// The worker logs the message, so it says what is wrong.
		a.log.Error("the disk of the data directory is full; a node's write was refused", "path", r.URL.Path, "err", err)
		writeError(w, http.StatusInsufficientStorage, "the server's disk is full, so it cannot save this node's report")
	default:
		a.log.Error("node request failed", "path", r.URL.Path, "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func writeDecodeError(w http.ResponseWriter, err error) {
	if errors.Is(err, nodeapi.ErrTooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "request body is too large")
		return
	}
	writeError(w, http.StatusBadRequest, "invalid request body")
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
	writeJSON(w, code, nodeapi.ErrorBody{Error: msg})
}
