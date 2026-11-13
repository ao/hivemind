package cluster

import (
	"encoding/pem"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/ratelimit"
	"github.com/ao/hivemind/internal/store"
)

// errJoinRefused is the one message for every token failure. It does not say
// whether the token was malformed, unknown, used or expired.
const errJoinRefused = "the join token is invalid, used or expired"

// joinRoutes returns the handler of the join listener. It serves POST /join
// and nothing else.
func joinRoutes(cfg Config, limiter *ratelimit.FailLimiter, log *slog.Logger) http.Handler {
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
func join(cfg Config, limiter *ratelimit.FailLimiter, log *slog.Logger, w http.ResponseWriter, r *http.Request) {
	src := ratelimit.SourceAddr(r)
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

	id, err := freshNodeID(r, cfg)
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

// newNodeID is NewNodeID. A test replaces it to force a clash.
var newNodeID = NewNodeID

// freshNodeID returns a new node ID that no registered node holds and that is
// not on the denylist of removed nodes. IDs are 48 random bits, so a clash is
// rare. The check makes sure a removed node's ID is never issued again: the
// node that got it would be refused, and the removed node's certificate names
// the same ID.
func freshNodeID(r *http.Request, cfg Config) (string, error) {
	for range 8 {
		id, err := newNodeID()
		if err != nil {
			return "", err
		}
		removed, err := cfg.Store.IsNodeRemoved(r.Context(), id)
		if err != nil {
			return "", err
		}
		if removed {
			continue
		}
		if _, err := cfg.Store.GetNode(r.Context(), id); errors.Is(err, store.ErrNotFound) {
			return id, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", errors.New("no unused node ID after 8 tries")
}
