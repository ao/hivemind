package carotate

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/store"
)

// Status is GET /v1/ca: the CAs, the rotation in progress, and the nodes.
type Status struct {
	// Current is the CA in ca.crt, and Next the one in ca-next.crt while
	// a rotation runs.
	Current *CAInfo `json:"current"`
	Next    *CAInfo `json:"next,omitempty"`
	// Listener is the fingerprint of the CA of the server's certificate,
	// which join tokens name.
	Listener string        `json:"listener"`
	Rotation *RotationInfo `json:"rotation,omitempty"`
	// Nodes is every node but the server's own, while a rotation runs.
	Nodes []NodeStatus `json:"nodes,omitempty"`
	// Ended is set in the reply of the step that ended a rotation, and
	// says how it ended.
	Ended *Ended `json:"ended,omitempty"`
}

// Ended is how a rotation ended, in the reply of the step that ended it.
type Ended struct {
	// Back is true when the cluster is back on the CA it had before the
	// rotation started: an abort in phase trust, or a finish after an
	// abort turned the rotation round. Then the backups taken during the
	// rotation hold the dropped CA's key, and those from before it are
	// good; otherwise every backup from before the end holds the dropped
	// key.
	Back bool `json:"back"`
	// Kept is the fingerprint of the CA the cluster has now, in ca.crt,
	// and Dropped that of the CA whose key was deleted.
	Kept    string `json:"kept"`
	Dropped string `json:"dropped"`
	// WentWithout is, after finish --force, the nodes it went ahead
	// without: those the server had not seen present a certificate from
	// the CA it kept. A node whose certificate is from the other CA is cut
	// off; one that renewed unseen, as after a restore, comes back.
	WentWithout []Waiting `json:"went_without,omitempty"`
}

// cutOffRecheck is how long after a forced finish the server looks again
// at the nodes it went ahead without: an agent that cannot reach the server
// backs off to one pass every 30 s.
const cutOffRecheck = 30 * time.Second

// recheckForced logs, once cutOffRecheck has passed since a forced finish,
// which of the nodes it went ahead without have reached the server since,
// and which have not. A request after the finish passed the node
// listener, which accepts the kept CA alone, so a node with a lease from
// after it is not cut off. A node removed since is left out. Run calls it.
func (m *Manager) recheckForced(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.forced) == 0 || m.now().Sub(m.forcedAt) < cutOffRecheck {
		return
	}
	nodes, err := m.st.ListNodes(ctx)
	if err != nil {
		return
	}
	lease := map[string]time.Time{}
	for _, n := range nodes {
		lease[n.ID] = n.LastLease
	}
	var back, out []string
	for _, w := range m.forced {
		l, ok := lease[w.Node]
		switch {
		case !ok:
		case l.After(m.forcedAt):
			back = append(back, w.Node)
		default:
			out = append(out, w.Node)
		}
	}
	at := m.forcedAt.UTC().Format(time.RFC3339)
	m.forced, m.forcedAt = nil, time.Time{}
	if len(back) > 0 {
		m.log.Info("of the nodes the forced finish went ahead without, these have reached the server since, so their certificates "+
			"are from the cluster CA, and they need nothing", "nodes", strings.Join(back, ","), "finished", at)
	}
	if len(out) > 0 {
		m.log.Warn("of the nodes the forced finish went ahead without, these have not reached the server in the 30 s since: "+
			"one whose certificate is from the CA the rotation dropped is cut off; remove it with hivemind node remove <id>, "+
			"and join the machine again with an empty data directory. One that is down is listed too: once it is up, "+
			"its own log says whether it was cut off", "nodes", strings.Join(out, ","), "finished", at)
	}
}

// CAInfo describes one CA.
type CAInfo struct {
	Fingerprint string    `json:"fingerprint"`
	Name        string    `json:"name"`
	NotBefore   time.Time `json:"not_before"`
	NotAfter    time.Time `json:"not_after"`
}

// RotationInfo is the rotation in progress.
type RotationInfo struct {
	// Phase is "trust" or "renew".
	Phase string `json:"phase"`
	// Toward is "next", or "current" after an abort in the renew phase.
	Toward string `json:"toward"`
	// From and To are the fingerprints of the CA the rotation comes from
	// and the one it goes to.
	From      string    `json:"from"`
	To        string    `json:"to"`
	StartedAt time.Time `json:"started_at"`
	// ListenerMoved is true once the server's certificate is from the CA
	// the rotation goes to.
	ListenerMoved bool `json:"listener_moved"`
	// NextStep is the step to run next: "renew" or "finish".
	NextStep string `json:"next_step"`
	// Waiting are the nodes NextStep waits for, with why. Empty: run it.
	Waiting []Waiting `json:"waiting"`
}

// NodeStatus is one node in a rotation.
type NodeStatus struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Version   string    `json:"version,omitempty"`
	Live      bool      `json:"live"`
	LastLease time.Time `json:"last_lease"`
	// Seen is true once the node has made a request since the rotation
	// started or turned round.
	Seen bool `json:"seen"`
	// Trusts is true once it said it trusts the CA the rotation goes to.
	Trusts bool `json:"trusts"`
	// Renewed is true once it presented a certificate from that CA.
	Renewed bool `json:"renewed"`
	// OldBuild is true when its requests say nothing of what it trusts: a
	// build without rotation.
	OldBuild bool `json:"old_build"`
}

// view lists every node but the server's own, with what the rotation has
// seen of it.
func (m *Manager) view(ctx context.Context, s *state) ([]NodeStatus, error) {
	nodes, err := m.st.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	m.nmu.Lock()
	marks := make(map[string]store.CARotationNode, len(m.nodes))
	for k, v := range m.nodes {
		marks[k] = v
	}
	m.nmu.Unlock()
	now := m.now()
	out := make([]NodeStatus, 0, len(nodes))
	for _, n := range nodes {
		if n.ID == m.localNode {
			continue
		}
		mk, seen := marks[n.ID]
		out = append(out, NodeStatus{ID: n.ID, Name: n.Name, Version: n.Version, Live: live(n, now), LastLease: n.LastLease.UTC(),
			Seen: seen, Trusts: mk.Trusts || mk.Renewed, Renewed: mk.Renewed, OldBuild: mk.OldBuild})
	}
	return out, nil
}

// waitingForRenew lists the live nodes that do not trust the CA the
// rotation goes to.
func waitingForRenew(view []NodeStatus, rot *store.CARotation) []Waiting {
	var w []Waiting
	for _, n := range view {
		if !n.Live || n.Trusts {
			continue
		}
		w = append(w, Waiting{Node: n.ID, Name: n.Name, Reason: notTrusting(n, rot)})
	}
	return w
}

// waitingForFinish lists the nodes that have not renewed under the CA the
// rotation goes to.
func waitingForFinish(view []NodeStatus, rot *store.CARotation) []Waiting {
	var w []Waiting
	for _, n := range view {
		if n.Renewed {
			continue
		}
		reason := "has not renewed its certificate under the new CA yet"
		if back(rot) {
			reason = "has not renewed its certificate back under the old CA yet"
		}
		switch {
		case !n.Live:
			reason = "is down: its last lease was at " + n.LastLease.Format(time.RFC3339)
		case !n.Trusts:
			reason = notTrusting(n, rot)
		}
		w = append(w, Waiting{Node: n.ID, Name: n.Name, Reason: reason})
	}
	return w
}

// notTrusting says why a node does not trust the CA the rotation goes to,
// by the rotation's direction. After an abort turned the rotation round,
// the marks start again, and the acceptance run read "has not made a
// request since the rotation started" of nodes that had made hundreds.
func notTrusting(n NodeStatus, rot *store.CARotation) string {
	switch {
	case n.OldBuild:
		v := n.Version
		if v == "" {
			v = "older than 2.2"
		}
		return "is a build without CA rotation (" + v + "): upgrade it, or remove it"
	case !n.Seen && back(rot):
		return "has not made a request since the rotation turned round"
	case !n.Seen:
		return "has not made a request since the rotation started"
	case back(rot):
		return "does not trust the old CA, which the rotation goes back to, yet"
	default:
		return "does not trust the new CA yet"
	}
}

// back reports whether an abort has turned the rotation round, towards
// the CA in ca.crt.
func back(rot *store.CARotation) bool { return rot != nil && rot.Toward == store.CACurrent }

// Status describes the CAs and the rotation.
func (m *Manager) Status(ctx context.Context) (Status, error) {
	s := m.cur.Load()
	st := Status{Current: caInfo(s.current), Next: caInfo(s.next), Listener: pki.FingerprintHex(s.listener.Fingerprint())}
	if !s.rotating() || m.st == nil {
		return st, nil
	}
	view, err := m.view(ctx, s)
	if err != nil {
		return Status{}, err
	}
	st.Nodes = view
	ri := &RotationInfo{Phase: s.rot.Phase, Toward: s.rot.Toward, From: pki.FingerprintHex(s.from.Fingerprint()),
		To: pki.FingerprintHex(s.to.Fingerprint()), StartedAt: s.rot.StartedAt.UTC(), ListenerMoved: s.rot.Listener == s.rot.Toward}
	if s.rot.Phase == store.CAPhaseTrust {
		ri.NextStep, ri.Waiting = "renew", waitingForRenew(view, s.rot)
	} else {
		ri.NextStep, ri.Waiting = "finish", waitingForFinish(view, s.rot)
	}
	if ri.Waiting == nil {
		ri.Waiting = []Waiting{}
	}
	st.Rotation = ri
	return st, nil
}

// Start makes the new CA and starts a rotation in phase trust.
func (m *Manager) Start(ctx context.Context) (Status, error) {
	if m.st == nil {
		return Status{}, ErrFixed
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.cur.Load()
	if s.rot != nil {
		return Status{}, ErrInProgress
	}
	if err := m.removeOrphan(); err != nil {
		return Status{}, err
	}
	next, err := pki.CreateNextCA(m.dir)
	if err != nil {
		return Status{}, fmt.Errorf("making the new CA: %w", err)
	}
	m.nmu.Lock()
	rot, err := m.st.StartCARotation(ctx, s.current.Fingerprint(), next.Fingerprint())
	if err == nil {
		var cross []byte
		if cross, err = crossFor(s.current, next, &rot); err == nil {
			m.nodes = map[string]store.CARotationNode{}
			m.setState(derive(s.current, next, &rot, cross))
		}
	}
	m.nmu.Unlock()
	if err != nil {
		// The files of a start the store did not record are removed here,
		// or by the next Open.
		_ = m.removeOrphan()
		return Status{}, err
	}
	m.log.Info("CA rotation started: a new CA is made, and every node is to trust both; then run hivemind ca rotate renew",
		"from", short(s.current.Fingerprint()), "to", short(next.Fingerprint()))
	return m.Status(ctx)
}

// Renew moves the rotation to phase renew, in which every node renews
// under the CA the rotation goes to. It refuses while a live node does not
// trust that CA, unless force.
func (m *Manager) Renew(ctx context.Context, force bool) (Status, error) {
	if m.st == nil {
		return Status{}, ErrFixed
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.cur.Load()
	switch {
	case !s.rotating():
		return Status{}, ErrNoRotation
	case s.rot.Phase == store.CAPhaseRenew:
		return Status{}, fmt.Errorf("%w: it is in phase renew already; hivemind ca rotate finish ends it", ErrWrongPhase)
	}
	view, err := m.view(ctx, s)
	if err != nil {
		return Status{}, err
	}
	if w := waitingForRenew(view, s.rot); len(w) > 0 {
		if !force {
			return Status{}, &RefusedError{Step: "renew", Waiting: w}
		}
		m.log.Warn("CA rotation renew forced, with live nodes that do not trust the new CA", "nodes", nodeIDs(w))
	}
	rot := *s.rot
	rot.Phase = store.CAPhaseRenew
	m.nmu.Lock()
	err = m.st.SetCARotation(ctx, rot, false)
	if err == nil {
		m.setState(derive(s.current, s.next, &rot, crossOf(s)))
	}
	m.nmu.Unlock()
	if err != nil {
		return Status{}, err
	}
	m.log.Info("CA rotation in phase renew: every node renews under the new CA, and the server's certificate moves to it once every node trusts it",
		"to", short(s.to.Fingerprint()))
	select {
	case m.kick <- struct{}{}:
	default:
	}
	return m.Status(ctx)
}

// Finish ends a rotation in phase renew: the server's certificate is from
// the CA the rotation goes to, and that CA alone is trusted and kept. It
// refuses while a node has not renewed under it, unless force, which cuts
// those nodes off.
func (m *Manager) Finish(ctx context.Context, force bool) (Status, error) {
	if m.st == nil {
		return Status{}, ErrFixed
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.cur.Load()
	switch {
	case !s.rotating():
		return Status{}, ErrNoRotation
	case s.rot.Phase == store.CAPhaseTrust:
		return Status{}, fmt.Errorf("%w: it is in phase trust, and no node has renewed: run hivemind ca rotate renew first", ErrWrongPhase)
	}
	view, err := m.view(ctx, s)
	if err != nil {
		return Status{}, err
	}
	w := waitingForFinish(view, s.rot)
	if len(w) > 0 {
		if !force {
			return Status{}, &RefusedError{Step: "finish", Waiting: w}
		}
		// The server knows only what it has seen: after a restore of a
		// backup taken before the nodes renewed, its marks are stale, and in
		// the fourth acceptance run both workers it called cut off came back
		// by themselves 0.5 s later. recheckForced says which did.
		m.log.Warn("CA rotation finish forced, without nodes this server has not seen present a certificate from the CA the rotation goes to: "+
			"a node whose certificate is from the other CA is cut off, and must be removed with hivemind node remove <id> and joined again; "+
			"one that renewed while this server did not see it, as after a restore of a backup, comes back by itself. "+
			"In 30 s this server logs which of them have reached it", "nodes", nodeIDs(w))
	}
	if s.rot.Listener != s.rot.Toward {
		if err := m.setListenerLocked(ctx, s); err != nil {
			return Status{}, err
		}
		s = m.cur.Load()
	}
	ended := &Ended{Back: s.rot.Toward == store.CACurrent, Kept: pki.FingerprintHex(s.to.Fingerprint()),
		Dropped: pki.FingerprintHex(s.from.Fingerprint()), WentWithout: w}
	if err := m.endLocked(ctx, s); err != nil {
		return Status{}, err
	}
	m.forced, m.forcedAt = w, m.now()
	if ended.Back {
		m.log.Info("CA rotation finished, back on the CA the cluster had before it: the cluster trusts that CA alone, and the new CA's key "+
			"is deleted. Backups taken during the rotation hold that key, which the cluster no longer uses: delete them. "+
			"Backups from before the rotation started are still good", "ca", short(s.to.Fingerprint()), "dropped", short(s.from.Fingerprint()))
	} else {
		m.log.Info("CA rotation finished: the cluster trusts the new CA alone, and the old CA's key is deleted. Backups taken before now "+
			"hold the old CA's key, which the cluster no longer uses: take a new backup, and delete the old ones",
			"ca", short(s.to.Fingerprint()), "dropped", short(s.from.Fingerprint()))
	}
	return m.statusEnded(ctx, ended)
}

// statusEnded is Status, with how the rotation ended.
func (m *Manager) statusEnded(ctx context.Context, e *Ended) (Status, error) {
	st, err := m.Status(ctx)
	if err != nil {
		return Status{}, err
	}
	st.Ended = e
	return st, nil
}

// endLocked records that the rotation is ending, so the node listener
// accepts the CA it goes to only, then does the file step and deletes the
// rotation. The caller holds mu.
func (m *Manager) endLocked(ctx context.Context, s *state) error {
	rot := *s.rot
	rot.Phase = store.CAPhaseEnding
	m.nmu.Lock()
	err := m.st.SetCARotation(ctx, rot, false)
	if err == nil {
		m.setState(derive(s.current, s.next, &rot, nil))
	}
	m.nmu.Unlock()
	if err != nil {
		return err
	}
	if err := m.completeEnding(ctx, rot); err != nil {
		return fmt.Errorf("ending the CA rotation (serve finishes it at its next start): %w", err)
	}
	cur, err := pki.LoadCA(m.dir)
	if err != nil {
		return err
	}
	m.nmu.Lock()
	m.nodes = map[string]store.CARotationNode{}
	m.setState(derive(cur, nil, nil, nil))
	m.nmu.Unlock()
	return nil
}

// Abort undoes a rotation that has not finished. In phase trust, when no
// node has presented a certificate from the new CA, the rotation ends at
// once and the new CA is deleted. Otherwise it turns round: it goes back
// to the CA it came from, in phase renew, and finish ends it.
func (m *Manager) Abort(ctx context.Context) (Status, error) {
	if m.st == nil {
		return Status{}, ErrFixed
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.cur.Load()
	if !s.rotating() {
		return Status{}, ErrNoRotation
	}
	if s.rot.Toward == store.CACurrent {
		return Status{}, fmt.Errorf("%w: the rotation is going back to the old CA already; finish it with hivemind ca rotate finish, then start a new one",
			ErrWrongPhase)
	}
	view, err := m.view(ctx, s)
	if err != nil {
		return Status{}, err
	}
	renewed := slices.ContainsFunc(view, func(n NodeStatus) bool { return n.Renewed })
	if s.rot.Phase == store.CAPhaseTrust && !renewed && s.rot.Listener == store.CACurrent {
		rot := *s.rot
		rot.Toward = store.CACurrent
		if err := m.endLocked(ctx, &state{current: s.current, next: s.next, rot: &rot}); err != nil {
			return Status{}, err
		}
		m.log.Info("CA rotation aborted: no node had a certificate from the new CA, which is deleted; the nodes stop trusting it at their next request. "+
			"Backups taken during the rotation hold its key, which the cluster no longer uses: delete them. Backups from before the rotation "+
			"started are still good", "ca", short(s.current.Fingerprint()), "dropped", short(s.next.Fingerprint()))
		return m.statusEnded(ctx, &Ended{Back: true, Kept: pki.FingerprintHex(s.current.Fingerprint()), Dropped: pki.FingerprintHex(s.next.Fingerprint())})
	}
	rot := *s.rot
	rot.Toward, rot.Phase = store.CACurrent, store.CAPhaseRenew
	m.nmu.Lock()
	err = m.st.SetCARotation(ctx, rot, true)
	if err == nil {
		var cross []byte
		if cross, err = crossFor(s.current, s.next, &rot); err == nil {
			m.nodes = map[string]store.CARotationNode{}
			m.setState(derive(s.current, s.next, &rot, cross))
		}
	}
	m.nmu.Unlock()
	if err != nil {
		return Status{}, err
	}
	m.log.Info("CA rotation turned round: the nodes renew back under the old CA, and the server's certificate goes back to it once " +
		"every node trusts it; run hivemind ca rotate finish once hivemind ca status lists no node, to delete the new CA")
	select {
	case m.kick <- struct{}{}:
	default:
	}
	return m.Status(ctx)
}

func nodeIDs(w []Waiting) string {
	ids := make([]string, len(w))
	for i, x := range w {
		ids[i] = x.Node
	}
	return strings.Join(ids, ",")
}

// IsRefusal reports whether err is a step's refusal for the state of the
// rotation or its nodes, which the API answers with 409.
func IsRefusal(err error) bool {
	var r *RefusedError
	return errors.As(err, &r) || errors.Is(err, ErrInProgress) || errors.Is(err, ErrNoRotation) || errors.Is(err, ErrWrongPhase)
}
