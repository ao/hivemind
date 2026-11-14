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
	// CertNotAfter is when the node's certificate expires, as the server
	// recorded it when it issued the certificate or saw the node present
	// it. It is absent when there is no record, such as for a server
	// without --advertise.
	CertNotAfter *time.Time `json:"cert_not_after,omitempty"`
	// Eligible is false for a node that takes no new replicas: one made
	// ineligible, or drained.
	Eligible bool `json:"eligible"`
	// Drain is the node's drain, in progress or finished. It is absent when
	// the node has none.
	Drain *DrainView `json:"drain,omitempty"`
	// Address is the node's private address, where routed replicas publish
	// their ports. It is absent for a node that reported none, which takes
	// no routed replicas.
	Address string `json:"address,omitempty"`
}

// EligibilityRequest is the body of POST /v1/nodes/{id}/eligibility.
type EligibilityRequest struct {
	Eligible *bool `json:"eligible"`
}

// EligibilityReply is the reply to POST /v1/nodes/{id}/eligibility.
type EligibilityReply struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Eligible bool   `json:"eligible"`
	// DrainCancelled is true when the node was draining: making it eligible
	// ends the drain.
	DrainCancelled bool `json:"drain_cancelled"`
}

// DrainRequest is the body of POST /v1/nodes/{id}/drain. Deadline is a Go
// duration such as "10m". Empty keeps the deadline of a drain in progress,
// and gives a new drain store.DefaultDrainDeadline.
type DrainRequest struct {
	Deadline string `json:"deadline"`
}

// DrainView is a node's drain, in GET and POST /v1/nodes/{id}/drain and in
// GET /v1/nodes.
type DrainView struct {
	NodeID string `json:"node_id"`
	// State is "draining" or "done".
	State      string     `json:"state"`
	StartedAt  time.Time  `json:"started_at"`
	Deadline   time.Time  `json:"deadline"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	// Moved counts the replicas moved to another node, and Stopped those
	// that lost the node at the deadline and were placed again.
	Moved   int `json:"moved"`
	Stopped int `json:"stopped"`
	// Remaining counts the replicas still on the node that should run.
	Remaining int `json:"remaining"`
	// OldContainers counts the containers the node still reports that no
	// replica on it should run, which its agent has yet to remove.
	OldContainers int `json:"old_containers"`
	// Moving is the moves in flight: a copy of the replica starting on To.
	Moving []DrainMoveView `json:"moving"`
	// Blocked is the replicas that did not move at the drain's last step,
	// and why.
	Blocked []DrainBlockedView `json:"blocked"`
}

// DrainMoveView is one move in flight.
type DrainMoveView struct {
	App   string    `json:"app"`
	Index int       `json:"index"`
	To    string    `json:"to"`
	Since time.Time `json:"since"`
}

// DrainBlockedView is one replica that stays on the drained node.
type DrainBlockedView struct {
	App    string `json:"app"`
	Index  int    `json:"index"`
	Reason string `json:"reason"`
}

func drainView(d store.Drain) DrainView {
	v := DrainView{NodeID: d.NodeID, State: string(d.State), StartedAt: d.StartedAt.UTC(), Deadline: d.Deadline.UTC(),
		Moved: d.Moved, Stopped: d.Stopped, Remaining: d.Remaining, OldContainers: d.OldContainers,
		Moving: []DrainMoveView{}, Blocked: []DrainBlockedView{}}
	if !d.FinishedAt.IsZero() {
		t := d.FinishedAt.UTC()
		v.FinishedAt = &t
	}
	for _, m := range d.Moving {
		v.Moving = append(v.Moving, DrainMoveView{App: m.App, Index: m.Index, To: m.To, Since: m.Since.UTC()})
	}
	for _, b := range d.Blocked {
		v.Blocked = append(v.Blocked, DrainBlockedView{App: b.App, Index: b.Index, Reason: b.Reason})
	}
	return v
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
	ds, err := s.cfg.Store.ListDrains(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	drains := map[string]DrainView{}
	for _, d := range ds {
		drains[d.NodeID] = drainView(d)
	}
	now := time.Now()
	out := make([]NodeView, 0, len(nodes))
	for _, n := range nodes {
		age := max(now.Sub(n.LastLease), 0)
		state := "ready"
		if age > scheduler.DefaultLeaseTimeout {
			state = "lost"
		}
		v := NodeView{ID: n.ID, Name: n.Name, CPU: n.CPU, Memory: n.Memory, LastLease: n.LastLease.UTC(),
			LeaseAgeSeconds: age.Seconds(), State: state, Server: n.ID == s.cfg.NodeID, Replicas: count[n.ID],
			Eligible: n.Eligible, Address: n.Address}
		if !n.CertNotAfter.IsZero() {
			t := n.CertNotAfter.UTC()
			v.CertNotAfter = &t
		}
		if d, ok := drains[n.ID]; ok {
			v.Drain = &d
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

// pathNodeID returns the {id} of the path, or answers 400 when it is not a
// node ID.
func pathNodeID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if err := pki.ValidateNodeID(id); err != nil {
		writeError(w, http.StatusBadRequest, "node ID "+strconv.Quote(id)+" is malformed")
		return "", false
	}
	return id, true
}

// setEligibility makes a node eligible or ineligible for new replicas. The
// server's own node may be made ineligible too: it keeps serving the API,
// and its agent keeps the containers it runs.
func (s *Server) setEligibility(w http.ResponseWriter, r *http.Request) {
	id, ok := pathNodeID(w, r)
	if !ok {
		return
	}
	var req EligibilityRequest
	if err := decodeStrict(http.MaxBytesReader(w, r.Body, MaxBodyBytes), &req); err != nil {
		writeDecodeError(w, err)
		return
	}
	if req.Eligible == nil {
		writeError(w, http.StatusBadRequest, `the body needs "eligible": true or false`)
		return
	}
	el, err := s.cfg.Store.SetNodeEligibility(r.Context(), id, *req.Eligible)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("node eligibility set", "node", el.ID, "eligible", el.Eligible, "drain_cancelled", el.DrainCancelled)
	s.cfg.Trigger()
	writeJSON(w, http.StatusOK, EligibilityReply{ID: el.ID, Name: el.Name, Eligible: el.Eligible, DrainCancelled: el.DrainCancelled})
}

// postDrain starts a drain of a node, or changes the deadline of the drain in
// progress. The server's own node may be drained: its replicas move to
// workers, and the server keeps running.
func (s *Server) postDrain(w http.ResponseWriter, r *http.Request) {
	id, ok := pathNodeID(w, r)
	if !ok {
		return
	}
	var req DrainRequest
	if err := decodeStrict(http.MaxBytesReader(w, r.Body, MaxBodyBytes), &req); err != nil {
		writeDecodeError(w, err)
		return
	}
	var deadline time.Duration
	if req.Deadline != "" {
		d, err := time.ParseDuration(req.Deadline)
		if err != nil {
			writeError(w, http.StatusBadRequest, "deadline must be a duration such as 10m: "+err.Error())
			return
		}
		if d <= 0 || d > store.MaxDrainDeadline {
			writeError(w, http.StatusBadRequest, "deadline must be above 0 and at most "+store.MaxDrainDeadline.String())
			return
		}
		deadline = d
	}
	d, err := s.cfg.Store.DrainNode(r.Context(), id, deadline)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.Info("node drain started", "node", d.NodeID, "deadline", d.Deadline.UTC().Format(time.RFC3339),
		"remaining", d.Remaining, "moving", len(d.Moving), "blocked", len(d.Blocked))
	s.cfg.Trigger()
	writeJSON(w, http.StatusOK, drainView(d))
}

// getDrain returns a node's drain, in progress or finished.
func (s *Server) getDrain(w http.ResponseWriter, r *http.Request) {
	id, ok := pathNodeID(w, r)
	if !ok {
		return
	}
	d, err := s.cfg.Store.GetDrain(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, drainView(d))
}

// removeNode takes a node out of the cluster for good. ?force=true removes a
// node that still renews its lease.
func (s *Server) removeNode(w http.ResponseWriter, r *http.Request) {
	id, ok := pathNodeID(w, r)
	if !ok {
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
