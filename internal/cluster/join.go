package cluster

import (
	"encoding/pem"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"

	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/pki"
)

// errJoinRefused is the one message for every token failure. It does not say
// whether the token was malformed, unknown, used or expired.
const errJoinRefused = "the join token is invalid, used or expired"

// joinRoutes returns the handler of the join listener. It serves POST /join
// and nothing else.
func joinRoutes(cfg Config, limiter *FailLimiter, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+nodeapi.JoinPath, func(w http.ResponseWriter, r *http.Request) {
		join(cfg, limiter, log, w, r)
	})
	return mux
}

// join enrolls a node. The token is the credential: the joining node has no
// certificate, so the request is checked by the token alone.
//
// In order: the source address is not over its failure limit; the token parses
// and names this cluster's CA; the CSR yields a certificate; the token is
// redeemed. The token is redeemed last, so a request that fails for another
// reason leaves it usable. The node ID is chosen here, so the joining node
// never picks its own.
func join(cfg Config, limiter *FailLimiter, log *slog.Logger, w http.ResponseWriter, r *http.Request) {
	src := sourceAddr(r)
	if !limiter.Allow(src) {
		w.Header().Set("Retry-After", strconv.Itoa(int(limiter.RetryAfter(src).Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "too many failed joins from this address; try again later")
		log.Warn("join refused by rate limit", "remote", src)
		return
	}
	fail := func(code int, msg, why string) {
		limiter.Fail(src)
		log.Warn("join failed", "remote", src, "reason", why)
		writeError(w, code, msg)
	}

	var req nodeapi.JoinRequest
	if err := nodeapi.Decode(r.Body, nodeapi.MaxJoinBytes, &req); err != nil {
		if errors.Is(err, nodeapi.ErrTooLarge) {
			fail(http.StatusRequestEntityTooLarge, "request body is too large", err.Error())
			return
		}
		fail(http.StatusBadRequest, "invalid join request", err.Error())
		return
	}
	tok, err := pki.ParseJoinToken(req.Token)
	if err != nil {
		fail(http.StatusUnauthorized, errJoinRefused, "malformed token")
		return
	}
	if err := tok.CheckFingerprint(cfg.CA.Fingerprint()); err != nil {
		fail(http.StatusUnauthorized, errJoinRefused, "token names another CA")
		return
	}

	id, err := NewNodeID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		log.Error("node ID", "err", err)
		return
	}
	der, err := cfg.CA.IssueNodeCert(req.CSR, pki.NodeCertOptions{NodeID: id})
	if err != nil {
		fail(http.StatusBadRequest, "the certificate request is invalid", err.Error())
		return
	}
	if _, err := cfg.Tokens.Redeem(req.Token); err != nil {
		fail(http.StatusUnauthorized, errJoinRefused, err.Error())
		return
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	writeJSON(w, http.StatusOK, nodeapi.JoinReply{NodeID: id, CertPEM: string(certPEM), CAPEM: string(cfg.CA.CertPEM)})
	log.Info("node joined", "node", id, "remote", src)
}

// sourceAddr is the host part of the peer's address, which is what the
// failure limit is keyed by.
func sourceAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
