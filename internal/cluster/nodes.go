package cluster

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/store"
)

// nodeAPI serves the node routes. The mTLS listener has already checked that
// the client certificate chains to the cluster CA and names a node. Each
// handler checks that the node in the certificate is the node in the path, so
// a node can read and report for itself only.
type nodeAPI struct {
	st  *store.Store
	log *slog.Logger
}

// nodeRoutes returns the handler of the mTLS listener. It has no join route.
// Every request passes the denylist check first.
func nodeRoutes(cfg Config, log *slog.Logger) http.Handler {
	a := &nodeAPI{st: cfg.Store, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+nodeapi.Prefix+"/nodes/{id}/register", a.register)
	mux.HandleFunc("GET "+nodeapi.Prefix+"/nodes/{id}/assignments", a.assignments)
	mux.HandleFunc("POST "+nodeapi.Prefix+"/nodes/{id}/status", a.postStatus)
	mux.HandleFunc("GET "+nodeapi.Prefix+"/nodes/{id}/status", a.getStatus)
	return a.denyRemoved(mux)
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

// register records the node's name and capacity, and renews its lease.
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
	if err := a.st.UpsertNode(r.Context(), store.Node{ID: id, Name: g.Name, CPU: g.CPU, Memory: g.Memory}); err != nil {
		a.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// assignments returns every replica placed on the node, marked for removal
// included, as the agent expects.
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
	writeJSON(w, http.StatusOK, nodeapi.AssignmentList{Assignments: nodeapi.FromStore(as)})
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
func (a *nodeAPI) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNodeRemoved):
		writeError(w, http.StatusForbidden, nodeapi.RemovedMessage)
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "this node is not registered")
	case errors.Is(err, store.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid request")
	case r.Context().Err() != nil:
		// The node went away; there is no one to answer.
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
