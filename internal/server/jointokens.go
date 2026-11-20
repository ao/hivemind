package server

import (
	"net/http"
	"time"

	"github.com/ao/hivemind/internal/pki"
)

// JoinTokenRequest is the body of POST /v1/join-tokens. TTL is a Go duration
// such as "1h". Empty means pki.DefaultTokenTTL, and for a server's token
// ServerTokenTTL. Server asks for a server join token (docs/ha.md), which
// hivemind join --role server takes and a worker's join refuses.
type JoinTokenRequest struct {
	TTL    string `json:"ttl"`
	Server bool   `json:"server,omitempty"`
}

// JoinTokenReply is the reply to POST /v1/join-tokens. The token is shown to
// the operator once; the server keeps only its digest. Warning, for a
// server's token, says what to know before it is used, such as that the
// cluster would have two servers.
type JoinTokenReply struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	Warning   string    `json:"warning,omitempty"`
}

// ServerTokenTTL is how long a server join token lasts by default, and
// MaxServerTokenTTL the longest it may: it buys the cluster CA's key.
const (
	ServerTokenTTL    = time.Hour
	MaxServerTokenTTL = 24 * time.Hour
)

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
	ttl, most := pki.DefaultTokenTTL, pki.MaxTokenTTL
	if req.Server {
		ttl, most = ServerTokenTTL, MaxServerTokenTTL
	}
	if req.TTL != "" {
		d, err := time.ParseDuration(req.TTL)
		if err != nil {
			writeError(w, http.StatusBadRequest, "ttl must be a duration such as 1h: "+err.Error())
			return
		}
		ttl = d
	}
	if ttl <= 0 || ttl > most {
		writeError(w, http.StatusBadRequest, "ttl must be above 0 and at most "+most.String())
		return
	}
	if req.Server {
		if s.cfg.IssueServerJoinToken == nil {
			writeError(w, http.StatusServiceUnavailable, "this server cannot add servers: it runs without --advertise, so it has no cluster listeners")
			return
		}
		token, expires, warning, err := s.cfg.IssueServerJoinToken(ttl)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, JoinTokenReply{Token: token, ExpiresAt: expires, Warning: warning})
		return
	}
	token, expires, err := s.cfg.IssueJoinToken(ttl)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, JoinTokenReply{Token: token, ExpiresAt: expires})
}
