package server

import (
	"net/http"
	"time"

	"github.com/ao/hivemind/internal/pki"
)

// JoinTokenRequest is the body of POST /v1/join-tokens. TTL is a Go duration
// such as "1h". Empty means pki.DefaultTokenTTL.
type JoinTokenRequest struct {
	TTL string `json:"ttl"`
}

// JoinTokenReply is the reply to POST /v1/join-tokens. The token is shown to
// the operator once; the server keeps only its digest.
type JoinTokenReply struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// postJoinToken issues a join token. It needs the admin token like every
// other route.
func (s *Server) postJoinToken(w http.ResponseWriter, r *http.Request) {
	if s.cfg.IssueJoinToken == nil {
		writeError(w, http.StatusServiceUnavailable, "this server has no cluster CA, so it cannot issue join tokens; run hivemind init")
		return
	}
	var req JoinTokenRequest
	if err := decodeStrict(http.MaxBytesReader(w, r.Body, MaxBodyBytes), &req); err != nil {
		writeDecodeError(w, err)
		return
	}
	ttl := pki.DefaultTokenTTL
	if req.TTL != "" {
		d, err := time.ParseDuration(req.TTL)
		if err != nil {
			writeError(w, http.StatusBadRequest, "ttl must be a duration such as 1h: "+err.Error())
			return
		}
		ttl = d
	}
	if ttl <= 0 || ttl > pki.MaxTokenTTL {
		writeError(w, http.StatusBadRequest, "ttl must be above 0 and at most "+pki.MaxTokenTTL.String())
		return
	}
	token, expires, err := s.cfg.IssueJoinToken(ttl)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, JoinTokenReply{Token: token, ExpiresAt: expires})
}
