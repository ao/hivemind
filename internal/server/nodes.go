package server

import (
	"net/http"
	"strconv"
	"time"

	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/scheduler"
	"github.com/ao/hivemind/internal/store"
)

// NodeView is one node in GET /v1/nodes.
type NodeView struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	CPU    int    `json:"cpu"`
	Memory int64  `json:"memory"`
	// LastLease is when the node last reported in, and LeaseAgeSeconds is
	// how long ago that was.
	LastLease       time.Time `json:"last_lease"`
	LeaseAgeSeconds float64   `json:"lease_age_seconds"`
	// State is "ready" while the lease is current, and "lost" after it has
	// gone 15 s without renewal.
	State string `json:"state"`
	// Server is true for the node that runs the server, which cannot be
	// removed.
	Server bool `json:"server"`
	// Replicas counts the replicas placed on the node that should run.
	Replicas int `json:"replicas"`
}

// RemoveNodeReply is the reply to DELETE /v1/nodes/{id}.
type RemoveNodeReply struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Rescheduled counts the replicas that lost the node and are placed
	// again.
	Rescheduled int `json:"rescheduled"`
	// Held counts the volume replicas that now wait with no node.
	Held int `json:"held"`
}

func (s *Server) listNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.cfg.Store.ListNodes(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	reps, err := s.cfg.Store.Replicas(r.Context(), "")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	count := map[string]int{}
	for _, rep := range reps {
		if rep.NodeID != "" && rep.Desired == store.Running {
			count[rep.NodeID]++
		}
	}
	now := time.Now()
	out := make([]NodeView, 0, len(nodes))
	for _, n := range nodes {
		age := max(now.Sub(n.LastLease), 0)
		state := "ready"
		if age > scheduler.DefaultLeaseTimeout {
			state = "lost"
		}
		out = append(out, NodeView{ID: n.ID, Name: n.Name, CPU: n.CPU, Memory: n.Memory, LastLease: n.LastLease.UTC(),
			LeaseAgeSeconds: age.Seconds(), State: state, Server: n.ID == s.cfg.NodeID, Replicas: count[n.ID]})
	}
	writeJSON(w, http.StatusOK, out)
}

// removeNode takes a node out of the cluster for good. ?force=true removes a
// node that still renews its lease.
func (s *Server) removeNode(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := pki.ValidateNodeID(id); err != nil {
		writeError(w, http.StatusBadRequest, "node ID "+strconv.Quote(id)+" is malformed")
		return
	}
	if id == s.cfg.NodeID {
		writeError(w, http.StatusConflict, "node "+id+" runs the server, and the server's own node cannot be removed")
		return
	}
	force := false
	if v := r.URL.Query().Get("force"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "force must be true or false")
			return
		}
		force = b
	}
	rm, err := s.cfg.Store.RemoveNode(r.Context(), id, force)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("node removed", "node", rm.ID, "name", rm.Name, "forced", force, "rescheduled", rm.Rescheduled, "held", rm.Held)
	s.cfg.Trigger()
	writeJSON(w, http.StatusOK, RemoveNodeReply{ID: rm.ID, Name: rm.Name, Rescheduled: rm.Rescheduled, Held: rm.Held})
}
