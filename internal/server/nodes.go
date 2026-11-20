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
	// gone 15 s without renewal. In the start grace after serve starts (2
	// minutes by default, --start-grace), a node that was up before the
	// start and has not reported to this process yet is "waiting": it is
	// not lost, and its replicas stay where they are. When the grace ends
	// with no report from any node that was waiting, they all stay
	// "waiting" (StartHeld); once one of them reports, the others stay
	// "waiting" for the start grace again, from that report.
	State string `json:"state"`
	// StartGraceLeftSeconds is, on a waiting node, how long the start
	// grace has left, or, after a hold has ended, the grace that followed
	// it. It is absent once the grace has ended, and while it holds.
	StartGraceLeftSeconds float64 `json:"start_grace_left_seconds,omitempty"`
	// StartHeld is true on a waiting node once the start grace has ended
	// with no report since the start from any node that was waiting: the
	// server keeps them waiting, and places none of their replicas, until
	// one of them reports or is removed (store.StartWait).
	StartHeld bool `json:"start_held,omitempty"`
	// StartHoldEnded is, on a waiting node, when the hold ended: the first
	// report, after the grace, of a node that was waiting. The node then
	// waits for the start grace again, from that moment.
	StartHoldEnded *time.Time `json:"start_hold_ended,omitempty"`
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
	// CertNotRenewing is true when the certificate is past the time its node
	// renews it, two thirds of the way through its lifetime, by more than a
	// day (pki.RenewOverdue): the node is not renewing it, as a 2.0 worker
	// does not.
	CertNotRenewing bool `json:"cert_not_renewing,omitempty"`
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
	// Version is the hivemind version the node last reported, such as
	// v2.2.0. It is absent for a node that reports none: one older than 2.2.
	Version string `json:"version,omitempty"`
	// Advertise is, on the server's node, the address the workers dial for
	// the server: serve's --advertise, an IP address or a DNS name. It is
	// absent on the workers, and without --advertise.
	Advertise string `json:"advertise,omitempty"`
	// Dials is, on a worker, the server address it dials, host:port, as it
	// registered it from its node.json. It is absent on the server's node,
	// and for a worker of an earlier build, which does not send it.
	Dials string `json:"dials,omitempty"`
	// Router is true for a node that runs a router: the server's node when
	// serve has --route-addr, and a worker whose router read the routing
	// table from the server in the last minute (it reads it every second).
	// It is false for a node that runs none, and for one whose router the
	// server has not heard from in that time, such as a lost node's.
	Router bool `json:"router"`
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
	// Lost counts the replicas that were placed again because the node was
	// lost during the drain, and LostAt is when it was first lost. LostAt is
	// absent when the node was not lost.
	Lost   int        `json:"lost"`
	LostAt *time.Time `json:"lost_at,omitempty"`
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
		Moved: d.Moved, Stopped: d.Stopped, Lost: d.Lost, Remaining: d.Remaining, OldContainers: d.OldContainers,
		Moving: []DrainMoveView{}, Blocked: []DrainBlockedView{}}
	if !d.FinishedAt.IsZero() {
		t := d.FinishedAt.UTC()
		v.FinishedAt = &t
	}
	if !d.LostAt.IsZero() {
		t := d.LostAt.UTC()
		v.LostAt = &t
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
	wait, _ := s.cfg.Store.StartWait(now)
	// The workers' routers read the routing table through the cluster
	// listener every second; the server's own runs in this process.
	routing := s.cfg.RouteReaders.Reading(now)
	// In a cluster of several, every server's node is a server's.
	servers := map[string]store.Server{}
	if list, err := s.cfg.Store.ListServers(r.Context()); err == nil {
		for _, sv := range list {
			servers[sv.ID] = sv
		}
	}
	out := make([]NodeView, 0, len(nodes))
	for _, n := range nodes {
		age := max(now.Sub(n.LastLease), 0)
		state := "ready"
		waiting := false
		switch {
		case age <= scheduler.DefaultLeaseTimeout:
		case s.cfg.Store.Waiting(n, now):
			// The node was up before serve started, and the start grace
			// has not ended, or holds: the sweep does not count it lost
			// (see store.Waiting), and the server waits for its first
			// report.
			state, waiting = "waiting", true
		default:
			state = "lost"
		}
		v := NodeView{ID: n.ID, Name: n.Name, CPU: n.CPU, Memory: n.Memory, LastLease: n.LastLease.UTC(),
			LeaseAgeSeconds: age.Seconds(), State: state, Server: n.ID == s.cfg.NodeID || servers[n.ID].ID != "", Replicas: count[n.ID],
			Eligible: n.Eligible, Address: n.Address, Version: n.Version, Dials: n.Dials, Router: routing[n.ID]}
		if n.ID == s.cfg.NodeID {
			v.Router = s.cfg.RouterReport != nil
		}
		if waiting {
			v.StartHeld = wait.Held
			if !wait.HoldEnded.IsZero() {
				t := wait.HoldEnded.UTC()
				v.StartHoldEnded = &t
			}
			if left := wait.WaitEnds.Sub(now); !wait.WaitEnds.IsZero() && left > 0 {
				v.StartGraceLeftSeconds = left.Seconds()
			}
		}
		if v.Server {
			v.Advertise = s.cfg.Advertise
			if sv, ok := servers[n.ID]; ok {
				v.Advertise = sv.Advertise
			}
		}
		if !n.CertNotAfter.IsZero() {
			t := n.CertNotAfter.UTC()
			v.CertNotAfter = &t
			v.CertNotRenewing = pki.RenewOverdue(n.CertIssuedAt, n.CertNotAfter, now)
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
