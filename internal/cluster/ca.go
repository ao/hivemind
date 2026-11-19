package cluster

import (
	"errors"
	"net/http"

	"github.com/ao/hivemind/internal/carotate"
	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/pki"
)

// withCADigest sets, on every reply, the digest of the CA set the workers
// follow (nodeapi.CAHeader). A worker that has not applied that set reads
// it from the ca route.
func withCADigest(rot *carotate.Manager, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, digest := rot.Bundle()
		w.Header().Set(nodeapi.CAHeader, digest)
		next.ServeHTTP(w, r)
	})
}

// checkCA refuses, with 403, a request whose client certificate is from a
// CA the node listener does not accept now. The TLS handshake checked the
// certificate against the CAs of its moment; a connection opened before a
// rotation dropped a CA would otherwise keep working with it. It then
// records what the request shows of the node for the rotation in
// progress, and refuses a certificate from the CA the rotation leaves for
// a node that has presented one from the CA it goes to.
func (a *nodeAPI) checkCA(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		root := pki.VerifiedRoot(r.TLS)
		if root == nil || !a.rot.Accepts(pki.Fingerprint(root)) {
			id, _ := pki.RequestNodeID(r)
			a.log.Warn("node request with a certificate from a CA the cluster no longer trusts refused", "node", id, "remote", r.RemoteAddr)
			writeError(w, http.StatusForbidden, nodeapi.CAUntrustedMessage)
			return
		}
		id, err := pki.RequestNodeID(r)
		if err != nil {
			writeError(w, http.StatusForbidden, "the client certificate names no node")
			return
		}
		trusts, sent := nodeapi.ParseCATrust(r.Header.Get(nodeapi.CATrustHeader))
		if err := a.rot.Observe(r.Context(), id, trusts, sent, pki.Fingerprint(root)); errors.Is(err, carotate.ErrSuperseded) {
			a.log.Warn("node request with a certificate from the CA the rotation leaves refused: the node has presented one from the CA it goes to, "+
				"so this one is not the node's own, or is from a connection opened before it renewed", "node", id, "remote", r.RemoteAddr)
			writeError(w, http.StatusForbidden, nodeapi.CASupersededMessage)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// caSet returns the CA set the node is to trust (nodeapi.CABundle).
func (a *nodeAPI) caSet(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.node(w, r); !ok {
		return
	}
	b, _ := a.rot.Bundle()
	writeJSON(w, http.StatusOK, b)
}
