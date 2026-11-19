// Package carotate is the server's side of a rotation of the cluster CA,
// as docs/ca-rotation.md describes it: the CA files in the data directory,
// the rotation's state in the store, which CAs the node listener accepts,
// which CA signs each certificate, and the CA set the workers follow.
//
// A Manager belongs to one serve process, which holds the data
// directory's lock, so it is the only writer of the CA files and of the
// rotation in the store. Every step writes the store in one transaction,
// and a step that changes files records that it has begun first, so Open
// finishes it after a crash.
package carotate

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/scheduler"
	"github.com/ao/hivemind/internal/store"
)

// Errors of the steps. A refusal that names nodes is a *RefusedError.
var (
	ErrInProgress = errors.New("a CA rotation is already in progress: hivemind ca status shows it")
	ErrNoRotation = errors.New("no CA rotation is in progress: start one with hivemind ca rotate start")
	ErrWrongPhase = errors.New("the CA rotation is not in the phase this step needs")
	// ErrSuperseded is Observe's refusal of a certificate from the CA a
	// rotation leaves, for a node that has presented one from the CA it
	// goes to.
	ErrSuperseded = errors.New(nodeapi.CASupersededMessage)
	// ErrFixed is a step on a Manager made with Fixed, which has no data
	// directory.
	ErrFixed = errors.New("this server cannot rotate its CA")
)

// RefusedError is a step refused while nodes are not where it needs them.
type RefusedError struct {
	Step    string
	Waiting []Waiting
}

func (e *RefusedError) Error() string {
	msg := fmt.Sprintf("hivemind ca rotate %s waits for %d node(s):", e.Step, len(e.Waiting))
	for i, w := range e.Waiting {
		if i > 0 {
			msg += ";"
		}
		msg += " " + w.Node
		if w.Name != "" {
			msg += " (" + w.Name + ")"
		}
		msg += " " + w.Reason
	}
	cut := "go ahead without them"
	if e.Step == "finish" {
		cut = "cut them off: each must then join again"
	}
	return msg + ". Run it again once hivemind ca status lists none, or with --force to " + cut
}

// Waiting is a node a step waits for, and why.
type Waiting struct {
	Node   string `json:"node"`
	Name   string `json:"name,omitempty"`
	Reason string `json:"reason"`
}

// Manager holds the cluster's CAs and the rotation in progress.
type Manager struct {
	dir       string
	st        *store.Store
	log       *slog.Logger
	now       func() time.Time
	localNode string

	// mu serialises the steps, and a backup against them (Hold).
	mu sync.Mutex
	// nmu guards nodes, and is held across a step's store write so that no
	// note from Observe lands between a reset of the marks and the new
	// state.
	nmu   sync.Mutex
	nodes map[string]store.CARotationNode
	cur   atomic.Pointer[state]
	gen   atomic.Uint64

	accept   *pki.TrustHolder
	kick     chan struct{}
	listener chan struct{}
	caWarned time.Time
}

// state is the CAs and the rotation at one moment. It is not changed once
// made.
type state struct {
	gen     uint64
	current *pki.CA
	next    *pki.CA           // nil without a rotation
	rot     *store.CARotation // nil without a rotation
	// from and to are the CA the rotation comes from and the one it goes
	// to; both nil without a rotation.
	from, to *pki.CA
	listener *pki.CA
	accept   *pki.Trust
	bundle   nodeapi.CABundle
	digest   string
}

func (s *state) rotating() bool { return s.rot != nil && s.rot.Phase != store.CAPhaseEnding }

// Options configures Open.
type Options struct {
	// Dir is the server's data directory, with ca.crt and ca.key.
	Dir   string
	Store *store.Store
	// LocalNode is the server's own node ID, which no step waits for.
	LocalNode string
	// Logger gets one line per step and per change. Nil means
	// slog.Default().
	Logger *slog.Logger
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

// Open loads the CAs from the data directory and the rotation from the
// store, and finishes a step a crash interrupted: a rotation that was
// ending ends, and CA files of a start that never recorded its rotation
// are removed. CA files that do not match the rotation the store records
// are an error: serve must not guess which CA the cluster has.
func Open(ctx context.Context, o Options) (*Manager, error) {
	if o.Dir == "" || o.Store == nil {
		return nil, errors.New("carotate: Dir and Store are required")
	}
	m := &Manager{dir: o.Dir, st: o.Store, log: o.Logger, now: o.Now, localNode: o.LocalNode,
		kick: make(chan struct{}, 1), listener: make(chan struct{}, 1)}
	if m.log == nil {
		m.log = slog.Default()
	}
	if m.now == nil {
		m.now = time.Now
	}
	rot, err := m.st.GetCARotation(ctx)
	switch {
	case errors.Is(err, store.ErrNotFound):
		if err := m.removeOrphan(); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	case rot.Phase == store.CAPhaseEnding:
		m.log.Info("finishing the end of a CA rotation that a stop interrupted", "toward", rot.Toward)
		if err := m.completeEnding(ctx, rot); err != nil {
			return nil, err
		}
	}
	s, err := m.load(ctx)
	if err != nil {
		return nil, err
	}
	m.accept = pki.NewTrustHolder(s.accept)
	m.cur.Store(s)
	nodes, err := m.st.CARotationNodes(ctx)
	if err != nil {
		return nil, err
	}
	m.nodes = nodes
	return m, nil
}

// Fixed returns a Manager for ca alone, with no data directory and no
// rotation. Its steps fail with ErrFixed. Tests, and a cluster listener
// given a CA and no Manager, use it.
func Fixed(ca *pki.CA) *Manager {
	m := &Manager{log: slog.Default(), now: time.Now, kick: make(chan struct{}, 1), listener: make(chan struct{}, 1),
		nodes: map[string]store.CARotationNode{}}
	s := derive(ca, nil, nil, nil)
	m.accept = pki.NewTrustHolder(s.accept)
	m.cur.Store(s)
	return m
}

// load reads the CA files and the rotation, and builds the state.
func (m *Manager) load(ctx context.Context) (*state, error) {
	cur, err := pki.LoadCA(m.dir)
	if err != nil {
		return nil, fmt.Errorf("cluster CA in %s: %w", m.dir, err)
	}
	rot, err := m.st.GetCARotation(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return derive(cur, nil, nil, nil), nil
	}
	if err != nil {
		return nil, err
	}
	next, err := pki.LoadNextCA(m.dir)
	if err != nil {
		return nil, fmt.Errorf("the store records a CA rotation, and the new CA in %s cannot be read: %w; "+
			"restore the data directory from a backup", m.dir, err)
	}
	if cur.Fingerprint() != rot.CurrentFP || next.Fingerprint() != rot.NextFP {
		return nil, fmt.Errorf("the CA files in %s are not the CAs of the rotation the store records "+
			"(%s is %s, the store has %s; %s is %s, the store has %s); restore the data directory from a backup",
			m.dir, pki.CACertFile, short(cur.Fingerprint()), short(rot.CurrentFP), pki.NextCACertFile,
			short(next.Fingerprint()), short(rot.NextFP))
	}
	cross, err := crossFor(cur, next, &rot)
	if err != nil {
		return nil, err
	}
	return derive(cur, next, &rot, cross), nil
}

// crossFor signs the CA the rotation goes to with the one it comes from.
func crossFor(cur, next *pki.CA, rot *store.CARotation) ([]byte, error) {
	from, to := cur, next
	if rot.Toward == store.CACurrent {
		from, to = next, cur
	}
	return from.CrossSign(to.Cert)
}

// derive builds the state of the CAs and the rotation. cross is the
// cross-certificate of the CA the rotation goes to.
func derive(cur, next *pki.CA, rot *store.CARotation, cross []byte) *state {
	s := &state{current: cur, next: next, rot: rot, listener: cur}
	if rot == nil {
		s.accept = pki.MustTrust(cur.Cert)
		s.bundle = nodeapi.CABundle{CAs: []nodeapi.BundleCA{{CertPEM: string(cur.CertPEM)}}}
		s.digest = s.bundle.Digest()
		return s
	}
	s.from, s.to = cur, next
	if rot.Toward == store.CACurrent {
		s.from, s.to = next, cur
	}
	if rot.Listener == store.CANext {
		s.listener = next
	}
	switch rot.Phase {
	case store.CAPhaseEnding:
		s.listener = s.to
		s.accept = pki.MustTrust(s.to.Cert)
		s.bundle = nodeapi.CABundle{CAs: []nodeapi.BundleCA{{CertPEM: string(s.to.CertPEM)}}}
	default:
		s.accept = pki.MustTrust(s.from.Cert, s.to.Cert)
		s.bundle = nodeapi.CABundle{Phase: rot.Phase, CAs: []nodeapi.BundleCA{
			{CertPEM: string(s.from.CertPEM)},
			{CertPEM: string(s.to.CertPEM), CrossPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cross}))},
		}}
		if rot.Phase == store.CAPhaseRenew {
			s.bundle.Renew = pki.FingerprintHex(s.to.Fingerprint())
		}
	}
	s.digest = s.bundle.Digest()
	return s
}

// setState makes s the state, and its accepted CAs the node listener's.
// The caller holds nmu.
func (m *Manager) setState(s *state) {
	old := m.cur.Load()
	s.gen = m.gen.Add(1)
	m.cur.Store(s)
	m.accept.Store(s.accept)
	if old == nil || pki.Fingerprint(old.listener.Cert) != pki.Fingerprint(s.listener.Cert) {
		select {
		case m.listener <- struct{}{}:
		default:
		}
	}
}

// Current is the CA in ca.crt.
func (m *Manager) Current() *pki.CA { return m.cur.Load().current }

// Listener is the CA that signs the server's own certificate, which both
// cluster listeners present. Join tokens name it.
func (m *Manager) Listener() *pki.CA { return m.cur.Load().listener }

// ListenerChanged receives a value when the listener CA has changed, so
// the server's certificate is issued again.
func (m *Manager) ListenerChanged() <-chan struct{} { return m.listener }

// Accept holds the CAs whose client certificates the node listener
// accepts; its TLS config reads it at each handshake.
func (m *Manager) Accept() *pki.TrustHolder { return m.accept }

// Accepts reports whether the node listener accepts client certificates
// from the CA with fingerprint fp now. A connection opened before a step
// keeps what its handshake checked; this is the check of the moment.
func (m *Manager) Accepts(fp [32]byte) bool { return m.accept.Load().Contains(fp) }

// TokenCA returns the CA of this cluster that a join token names, or nil:
// the current CA, or either CA while a rotation runs. The joining node
// pins the CA its token names, and gets a certificate from it.
func (m *Manager) TokenCA(fp [32]byte) *pki.CA {
	s := m.cur.Load()
	for _, ca := range []*pki.CA{s.current, s.next} {
		if ca != nil && ca.Fingerprint() == fp && s.accept.Contains(fp) {
			return ca
		}
	}
	return nil
}

// RenewalCA is the CA that renews the certificate of a node whose current
// certificate is from the CA with fingerprint root, and whose request says
// it trusts the CAs in trusts. A renewal never moves to an older CA than
// the certificate it replaces; in the renew phase it moves to the CA the
// rotation goes to, for a node that trusts it.
func (m *Manager) RenewalCA(root [32]byte, trusts [][32]byte) *pki.CA {
	s := m.cur.Load()
	if s.rot == nil {
		return s.current
	}
	if !s.rotating() {
		return s.to
	}
	toFP := s.to.Fingerprint()
	if root == toFP || (s.rot.Phase == store.CAPhaseRenew && slices.Contains(trusts, toFP)) {
		return s.to
	}
	return s.from
}

// Bundle is the CA set the workers follow, and its digest for
// nodeapi.CAHeader.
func (m *Manager) Bundle() (nodeapi.CABundle, string) {
	s := m.cur.Load()
	return s.bundle, s.digest
}

// Observe records what a request from node shows: the CAs it says it
// trusts (sent false: it said nothing, as a build without rotation) and
// the CA its certificate is from. It returns ErrSuperseded for a
// certificate from the CA the rotation leaves, when the node has presented
// one from the CA it goes to. Outside a rotation it records nothing. A
// store error is logged; the request goes on.
func (m *Manager) Observe(ctx context.Context, node string, trusts [][32]byte, sent bool, root [32]byte) error {
	s := m.cur.Load()
	if !s.rotating() || node == m.localNode {
		return nil
	}
	toFP, fromFP := s.to.Fingerprint(), s.from.Fingerprint()
	m.nmu.Lock()
	defer m.nmu.Unlock()
	if m.cur.Load().gen != s.gen {
		// A step ran meanwhile; this request was made under the old state.
		return nil
	}
	old, seen := m.nodes[node]
	if old.Renewed && root == fromFP {
		return ErrSuperseded
	}
	n := store.CARotationNode{NodeID: node, OldBuild: !sent, Trusts: sent && slices.Contains(trusts, toFP),
		Renewed: old.Renewed || root == toFP}
	if seen && n.Trusts == old.Trusts && n.Renewed == old.Renewed && n.OldBuild == old.OldBuild {
		return nil
	}
	if err := m.st.NoteCARotationNode(ctx, n); err != nil {
		if ctx.Err() == nil {
			m.log.Warn("recording what a node trusts in the CA rotation", "node", node, "err", err)
		}
		return nil
	}
	n.SeenAt = m.now()
	m.nodes[node] = n
	switch {
	case n.OldBuild && (!seen || !old.OldBuild):
		m.log.Warn("a node of a build without CA rotation is in the cluster: it trusts the old CA only, holds back the server's "+
			"certificate and hivemind ca rotate finish, and must be upgraded, removed, or cut off with finish --force", "node", node)
	case n.Trusts && !old.Trusts:
		m.log.Info("a node trusts "+towardCA(s.rot), "node", node, "ca", short(toFP), "leaving", short(fromFP))
	}
	if n.Renewed && !old.Renewed {
		m.log.Info("a node presents a certificate from "+towardCA(s.rot), "node", node, "ca", short(toFP), "leaving", short(fromFP))
	}
	if n.Trusts != old.Trusts || n.Renewed != old.Renewed {
		select {
		case m.kick <- struct{}{}:
		default:
		}
	}
	return nil
}

// Hold runs f with every step held off, so that a backup copies the store
// and the CA files of one moment.
func (m *Manager) Hold(f func() error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return f()
}

// Run moves the server's certificate to the CA the rotation goes to once
// every node trusts it, checking at each change Observe sees and every 10
// seconds, and logs the CA's expiry warning once a day, until ctx ends.
func (m *Manager) Run(ctx context.Context) {
	if m.st == nil {
		return
	}
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		m.warnExpiry()
		if err := m.moveListener(ctx); err != nil && ctx.Err() == nil {
			m.log.Warn("checking whether the server's certificate can move to the CA the rotation goes to", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-m.kick:
		case <-t.C:
		}
	}
}

// moveListener moves the listener CA to the CA the rotation goes to, in
// the renew phase, once every node trusts it.
func (m *Manager) moveListener(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.cur.Load()
	if !s.rotating() || s.rot.Phase != store.CAPhaseRenew || s.rot.Listener == s.rot.Toward {
		return nil
	}
	view, err := m.view(ctx, s)
	if err != nil {
		return err
	}
	for _, n := range view {
		if !n.Trusts && !n.Renewed {
			return nil
		}
	}
	return m.setListenerLocked(ctx, s)
}

// setListenerLocked moves the listener CA to the CA the rotation goes to.
// The caller holds mu.
func (m *Manager) setListenerLocked(ctx context.Context, s *state) error {
	rot := *s.rot
	rot.Listener = rot.Toward
	m.nmu.Lock()
	defer m.nmu.Unlock()
	if err := m.st.SetCARotation(ctx, rot, false); err != nil {
		return err
	}
	m.setState(derive(s.current, s.next, &rot, crossOf(s)))
	m.log.Info("the server's certificate moves to the CA the rotation goes to; join tokens name it from now on",
		"ca", short(s.to.Fingerprint()))
	return nil
}

// crossOf is the state's cross-certificate, as DER.
func crossOf(s *state) []byte {
	if len(s.bundle.CAs) < 2 {
		return nil
	}
	blk, _ := pem.Decode([]byte(s.bundle.CAs[1].CrossPEM))
	if blk == nil {
		return nil
	}
	return blk.Bytes
}

// warnExpiry logs, once a day, that the CA the cluster depends on is in
// its last year.
func (m *Manager) warnExpiry() {
	now := m.now()
	if now.Sub(m.caWarned) < 24*time.Hour {
		return
	}
	s := m.cur.Load()
	ca := s.current
	if s.rotating() {
		ca = s.from
	}
	left := ca.Cert.NotAfter.Sub(now)
	if left >= pki.CAWarnBefore {
		return
	}
	m.caWarned = now
	attrs := []any{"ca", short(ca.Fingerprint()), "ca_not_after", ca.Cert.NotAfter.UTC(), "left", left.Round(time.Hour)}
	if !s.rotating() {
		m.log.Warn("the cluster CA expires within a year; node certificates end with it, and when it expires no node can reach "+
			"the server. Rotate it: hivemind ca rotate start (docs/ca-rotation.md)", attrs...)
		return
	}
	step := "renew"
	if s.rot.Phase == store.CAPhaseRenew {
		step = "finish"
	}
	m.log.Warn("the cluster CA a rotation is replacing expires within a year; finish the rotation before then: hivemind ca status, "+
		"then hivemind ca rotate "+step, append(attrs, "phase", s.rot.Phase)...)
}

// removeOrphan removes the CA files of a start that never recorded its
// rotation.
func (m *Manager) removeOrphan() error {
	removed := false
	for _, name := range []string{pki.NextCAKeyFile, pki.NextCACertFile} {
		err := os.Remove(filepath.Join(m.dir, name))
		switch {
		case err == nil:
			removed = true
		case !errors.Is(err, fs.ErrNotExist):
			return err
		}
	}
	if removed {
		m.log.Warn("removed the new CA files of a CA rotation start that did not complete; no rotation is in progress")
		return syncDir(m.dir)
	}
	return nil
}

// completeEnding does the file step of an ending rotation and deletes the
// rotation: towards the next CA, ca-next.key and ca-next.crt are renamed
// over ca.key and ca.crt; towards the current CA, they are removed. Each
// part is done only if it is still to do, so a second run after a crash
// finishes the first.
func (m *Manager) completeEnding(ctx context.Context, rot store.CARotation) error {
	keep := rot.CurrentFP
	if rot.Toward == store.CANext {
		keep = rot.NextFP
		for _, p := range [][2]string{{pki.NextCAKeyFile, pki.CAKeyFile}, {pki.NextCACertFile, pki.CACertFile}} {
			err := os.Rename(filepath.Join(m.dir, p[0]), filepath.Join(m.dir, p[1]))
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	} else {
		for _, name := range []string{pki.NextCAKeyFile, pki.NextCACertFile} {
			if err := os.Remove(filepath.Join(m.dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	}
	if err := syncDir(m.dir); err != nil {
		return err
	}
	ca, err := pki.LoadCA(m.dir)
	if err != nil {
		return fmt.Errorf("the cluster CA after the rotation's end: %w", err)
	}
	if ca.Fingerprint() != keep {
		return fmt.Errorf("the cluster CA in %s is %s after the rotation's end, and the rotation was to keep %s; restore the data directory from a backup",
			m.dir, short(ca.Fingerprint()), short(keep))
	}
	return m.st.EndCARotation(ctx)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	serr := d.Sync()
	if cerr := d.Close(); serr == nil {
		serr = cerr
	}
	return serr
}

// short is the first 16 hex digits of a fingerprint, for logs.
func short(fp [32]byte) string { return pki.FingerprintHex(fp)[:16] }

// towardCA names the CA a rotation goes to, for log lines: the new one, or,
// once an abort has turned the rotation round, the old one it goes back to.
// In the fifth soak the lines called the CA the rotation went back to "the
// new cluster CA", which an operator checking an abort by its fingerprint
// reads the wrong way round.
func towardCA(rot *store.CARotation) string {
	if rot != nil && rot.Toward == store.CACurrent {
		return "the CA the rotation goes back to, the old one in ca.crt, after an abort"
	}
	return "the CA the rotation goes to, the new one in ca-next.crt"
}

// live reports whether a node's lease is current at now.
func live(n store.Node, now time.Time) bool {
	return now.Sub(n.LastLease) <= scheduler.DefaultLeaseTimeout
}

// caInfo describes a CA for Status.
func caInfo(ca *pki.CA) *CAInfo {
	if ca == nil {
		return nil
	}
	return &CAInfo{Fingerprint: pki.FingerprintHex(ca.Fingerprint()), Name: ca.Cert.Subject.CommonName,
		NotBefore: ca.Cert.NotBefore.UTC(), NotAfter: ca.Cert.NotAfter.UTC()}
}
