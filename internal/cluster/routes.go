package cluster

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"slices"
	"time"

	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/store"
)

// routes returns the routing table. Every node reads the whole table: a
// router on any node serves every routed host.
func (a *nodeAPI) routes(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.node(w, r); !ok {
		return
	}
	rt, err := a.st.RouteTable(r.Context())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, nodeapi.RouteTableFromStore(rt))
}

// tls serves one operation on the shared TLS storage: certmagic's Storage
// and Locker, for the router on the calling node. The node in the
// certificate must be the node in the path, as on every node route, keys are
// confined to certmagic's namespace, and bodies and values are limited.
// A lock is held under the calling node's ID and the owner token it sends,
// so a node can only renew or release its own holds.
//
// Any node can read every key, private keys included: every router serves
// every routed host. docs/security.md says what that means for a
// compromised worker.
func (a *nodeAPI) tls(w http.ResponseWriter, r *http.Request) {
	id, ok := a.node(w, r)
	if !ok {
		return
	}
	op := r.PathValue("op")
	if !slices.Contains(nodeapi.TLSOps, op) {
		writeError(w, http.StatusBadRequest, "unknown TLS storage operation")
		return
	}
	req, err := nodeapi.DecodeTLSRequest(r.Body, op)
	if err != nil {
		writeDecodeError(w, err)
		return
	}
	ctx := r.Context()
	owner := id + "/" + req.Owner
	ttl := time.Duration(req.TTLMillis) * time.Millisecond
	var rep nodeapi.TLSReply
	switch op {
	case nodeapi.TLSLoad:
		v, err := a.st.TLSLoad(ctx, req.Key)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			a.failTLS(w, r, err)
			return
		}
		rep.Found, rep.Value = err == nil, v
	case nodeapi.TLSStore:
		if err := a.st.TLSStore(ctx, req.Key, req.Value); err != nil {
			a.failTLS(w, r, err)
			return
		}
		a.log.Debug("TLS storage write", "node", id, "key", req.Key, "bytes", len(req.Value))
	case nodeapi.TLSDelete:
		if err := a.st.TLSDelete(ctx, req.Key); err != nil {
			a.failTLS(w, r, err)
			return
		}
		a.log.Debug("TLS storage delete", "node", id, "key", req.Key)
	case nodeapi.TLSExists:
		ok, err := a.st.TLSExists(ctx, req.Key)
		if err != nil {
			a.failTLS(w, r, err)
			return
		}
		rep.Found, rep.Exists = true, ok
	case nodeapi.TLSList:
		keys, err := a.st.TLSList(ctx, req.Key, req.Recursive)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			a.failTLS(w, r, err)
			return
		}
		rep.Found, rep.Keys = err == nil, keys
	case nodeapi.TLSStat:
		info, err := a.st.TLSStat(ctx, req.Key)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			a.failTLS(w, r, err)
			return
		}
		if err == nil {
			rep.Found, rep.Info = true, nodeapi.EncodeTLSInfo(info)
		}
	case nodeapi.TLSLock:
		took, err := a.st.TLSTryLock(ctx, req.Name, owner, ttl)
		if err != nil {
			a.failTLS(w, r, err)
			return
		}
		rep.Found, rep.Acquired = true, took
		if took {
			a.log.Debug("TLS lock taken", "node", id, "lock", req.Name)
		}
	case nodeapi.TLSRenew:
		if err := a.st.TLSRenewLock(ctx, req.Name, owner, ttl); err != nil {
			a.failTLS(w, r, err)
			return
		}
	case nodeapi.TLSUnlock:
		if err := a.st.TLSUnlock(ctx, req.Name, owner); err != nil {
			a.failTLS(w, r, err)
			return
		}
		a.log.Debug("TLS lock released", "node", id, "lock", req.Name)
	}
	writeJSON(w, http.StatusOK, rep)
}

// failTLS maps a TLS storage error to a reply. A lock the node does not hold
// is a conflict, not a missing node, so it is 409 rather than 404, and so is
// storage that is full.
func (a *nodeAPI) failTLS(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusConflict, "the lock is not held by this owner")
	case errors.Is(err, store.ErrTLSLockLimit):
		writeError(w, http.StatusConflict, fmt.Sprintf("this node holds %d TLS locks, the most a node may hold; release one first", store.MaxTLSLocksPerHolder))
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "the TLS storage is full")
	default:
		a.fail(w, r, err)
	}
}
