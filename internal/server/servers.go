package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// The servers of the cluster (docs/ha.md#membership).

// noLeaderHeader marks a 503 that says no server leads (ha.HeaderNoLeader).
const noLeaderHeader = "Hivemind-No-Leader"

// ServerEntry is one server in GET /v1/servers.
type ServerEntry struct {
	ID string `json:"id"`
	// Name is the host name of the server's node, as it registered.
	Name string `json:"name,omitempty"`
	// Advertise and NodePort are where workers reach its node listener;
	// RaftAddr is its server port.
	Advertise string `json:"advertise"`
	NodePort  int    `json:"node_port,omitempty"`
	RaftAddr  string `json:"raft_addr,omitempty"`
	// Role is leader, follower, or nonvoter for a server that is catching
	// up and does not vote yet.
	Role string `json:"role"`
	// LastContact is when the server that answered last heard from this
	// one; none for one it has not heard from, or cannot tell.
	LastContact *time.Time `json:"last_contact,omitempty"`
	// Version is the server's release (docs/ha.md, rolling upgrades):
	// from its answer to the leader's build request, or, when
	// VersionFrom is "node", the version its node last sent, for a build
	// that does not answer it. SchemaOldest and SchemaNewest are the schema
	// versions its build runs, when it said.
	Version      string `json:"version,omitempty"`
	VersionFrom  string `json:"version_from,omitempty"`
	SchemaOldest int    `json:"schema_oldest,omitempty"`
	SchemaNewest int    `json:"schema_newest,omitempty"`
}

// ServersReply is the reply to GET /v1/servers.
type ServersReply struct {
	// Leader is the leader's ID, or "" when there is none.
	Leader string `json:"leader"`
	// AnsweredBy is the server that answered.
	AnsweredBy string        `json:"answered_by"`
	Servers    []ServerEntry `json:"servers"`
	// Note says what the list cannot, such as that there is no leader.
	Note string `json:"note,omitempty"`
	// Schema is the store's schema version on the server that answered.
	// SchemaNext is the next version, when the leader's build knows one,
	// and SchemaWaits names the servers that do not run it yet, with why:
	// the leader moves the store to it once every server does.
	Schema      int    `json:"schema,omitempty"`
	SchemaNext  int    `json:"schema_next,omitempty"`
	SchemaWaits string `json:"schema_waits,omitempty"`
}

// ServerRemoval is the reply to DELETE /v1/servers/{id}.
type ServerRemoval struct {
	ID string `json:"id"`
	// Next says what to do with the server's node.
	Next string `json:"next"`
}

func (s *Server) listServers(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Servers == nil {
		writeError(w, http.StatusServiceUnavailable, "this server cannot list the servers")
		return
	}
	rep, err := s.cfg.Servers(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (s *Server) removeServer(w http.ResponseWriter, r *http.Request) {
	if s.cfg.RemoveServer == nil {
		writeError(w, http.StatusServiceUnavailable, "this server cannot remove servers")
		return
	}
	force, err := boolParam(r, "force")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	next, err := s.cfg.RemoveServer(ctx, r.PathValue("id"), force)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ServerRemoval{ID: r.PathValue("id"), Next: next})
}

// boolParam reads a true or false query parameter; absent is false.
func boolParam(r *http.Request, name string) (bool, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false", name)
	}
	return b, nil
}
