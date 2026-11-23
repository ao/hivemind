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
	"crypto/x509"
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
	// ErrSeveralServers is a rotation started in a cluster of several
	// servers whose store is below store.CAStoreSchemaVersion: a server
	// runs 2.3.0, or the step to that version has not been written yet
	// (docs/ha.md#the-ca-rotation-with-several-servers).
	ErrSeveralServers = errors.New("the cluster has more than one server, and its store is below schema version 19, which a CA rotation with several servers needs: " +
		"every server must run 2.4.0 or later, and the leader moves the store to 19 once they do (hivemind servers shows both)")
	// ErrServerJoining is a rotation started while a server is catching up
	// with the log as a non-voter.
	ErrServerJoining = errors.New("a server is joining the cluster, and a CA rotation waits until it is a voter (hivemind servers)")
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
	// forced holds the nodes the last forced finish went ahead without,
	// and forcedAt when, until recheckForced has looked at them again.
	// Guarded by mu.
	forced   []Waiting
	forcedAt time.Time

	// changed is kicked when the store's record may have changed under
	// this server, as when it applied another server's entry: Follow
	// looks at the record again.
	changed chan struct{}
	// hooks, set before serve runs anything: serverCert is the server's
	// own certificate now, which a change of the listener CA waits for
	// before the accepted CAs narrow; onState runs after each change of
	// the state; startCheck refuses a start, as while a server joins; and
	// servers lists each server's CA files, for Status.
	serverCert func() *x509.Certificate
	onState    []func()
	startCheck func(ctx context.Context) error
	servers    func(ctx context.Context) []ServerCA
}

// ServerCA is one server's CA files and certificate, as it said in its
// answer to the leader's build request, by fingerprint in hex: "" for a
// file it does not have.
type ServerCA struct {
	ID       string `json:"id"`
	Current  string `json:"current,omitempty"`
	Next     string `json:"next,omitempty"`
	Listener string `json:"listener,omitempty"`
	// Err says why the server's files are not known, as when it did not
	// answer.
	Err string `json:"error,omitempty"`
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
		kick: make(chan struct{}, 1), listener: make(chan struct{}, 1), changed: make(chan struct{}, 1)}
	if m.log == nil {
		m.log = slog.Default()
	}
	if m.now == nil {
		m.now = time.Now
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.followLocked(ctx); err != nil {
		return nil, err
	}
	// A store that takes writes, a cluster of one's, records what a
	// rotation's record or the cluster record lacks; on a server of
	// several, the leader does (Run).
	if err := m.recordGaps(ctx); err != nil && !errors.Is(err, store.ErrNotLeader) {
		return nil, err
	}
	// The record's changes under this server, as when it applies another
	// server's entries, are followed by Follow.
	m.st.OnForeignChange(func() {
		select {
		case m.changed <- struct{}{}:
		default:
		}
	})
	return m, nil
}

// followLocked makes this server's CA files and state those of the store's
// record (docs/ha.md#the-steps-on-every-server): every server, the leader
// included, at Open and each time the record may have changed. The caller
// holds mu.
func (m *Manager) followLocked(ctx context.Context) error {
	var rp *store.CARotation
	rot, err := m.st.GetCARotation(ctx)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return err
	default:
		rp = &rot
	}
	caFP, known, err := m.st.ClusterCA(ctx)
	if err != nil {
		return err
	}
	var s *state
	switch {
	case rp == nil:
		if err := m.settleFiles(caFP, known); err != nil {
			return err
		}
		cur, err := pki.LoadCA(m.dir)
		if err != nil {
			return fmt.Errorf("cluster CA in %s: %w", m.dir, err)
		}
		if known && cur.Fingerprint() != caFP {
			return fmt.Errorf("this server's %s in %s is the CA %s, and the cluster's is %s: it holds another CA than the cluster's, as a server "+
				"that was cut off by hivemind ca rotate finish --force does. Remove it (hivemind server remove, then hivemind node remove) and join "+
				"the machine again with an empty data directory", pki.CACertFile, m.dir, short(cur.Fingerprint()), short(caFP))
		}
		s = derive(cur, nil, nil, nil)
	case rp.Phase == store.CAPhaseEnding:
		if cur := m.cur.Load(); cur == nil || cur.rot == nil || cur.rot.Phase != store.CAPhaseEnding {
			m.log.Info("the CA rotation ends; this server does its file step", "toward", rp.Toward)
		}
		if err := m.endingFiles(*rp); err != nil {
			return err
		}
		cur, err := pki.LoadCA(m.dir)
		if err != nil {
			return fmt.Errorf("the cluster CA after the rotation's end: %w", err)
		}
		// The record's end is the leader's to write, and a cluster of
		// one's: a follower's store refuses it, and follows the leader's.
		if err := m.st.EndCARotationKeeping(ctx, cur.Fingerprint()); err != nil && !errors.Is(err, store.ErrNotLeader) {
			return err
		}
		s = derive(cur, nil, nil, nil)
	default:
		cur, err := pki.LoadCA(m.dir)
		if err != nil {
			return fmt.Errorf("cluster CA in %s: %w", m.dir, err)
		}
		if cur.Fingerprint() != rp.CurrentFP {
			return fmt.Errorf("the CA files in %s are not the CAs of the rotation the store records (%s is %s, the store has %s); "+
				"restore the data directory from a backup", m.dir, pki.CACertFile, short(cur.Fingerprint()), short(rp.CurrentFP))
		}
		next, err := m.nextFiles(ctx, cur, rp)
		if err != nil {
			return err
		}
		cross, err := crossFor(cur, next, rp)
		if err != nil {
			return err
		}
		s = derive(cur, next, rp, cross)
	}
	m.adopt(ctx, s)
	return nil
}

// settleFiles deals with ca-next.key and ca-next.crt when the store records
// no rotation. With the cluster CA recorded, a ca-next.crt that is that CA
// is from a rotation that ended while this server was behind, and is
// renamed over ca.crt; any other is from a rotation that ended back on the
// CA in ca.crt, and is removed. Without it, as in 2.3.0, the files are from
// a start that never recorded its rotation, and are removed.
func (m *Manager) settleFiles(caFP [32]byte, known bool) error {
	present := false
	for _, name := range []string{pki.NextCAKeyFile, pki.NextCACertFile} {
		if _, err := os.Lstat(filepath.Join(m.dir, name)); err == nil {
			present = true
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if !present {
		return nil
	}
	if known {
		next, err := pki.LoadNextCA(m.dir)
		if err == nil && next.Fingerprint() == caFP {
			if cur, err := pki.LoadCA(m.dir); err != nil || cur.Fingerprint() != caFP {
				m.log.Info("the CA rotation ended while this server was behind: its ca-next.key and ca-next.crt are the cluster CA, and replace ca.key and ca.crt",
					"ca", short(caFP))
				return m.renameNext()
			}
		}
		// A server that stopped between renameNext's two renames has the
		// cluster CA's key in ca.key already, and its certificate still in
		// ca-next.crt: the second rename is what is left to do.
		if _, kerr := os.Lstat(filepath.Join(m.dir, pki.NextCAKeyFile)); errors.Is(kerr, fs.ErrNotExist) {
			half, err := pki.LoadCAFiles(m.dir, pki.NextCACertFile, pki.CAKeyFile)
			if err == nil && half.Fingerprint() == caFP {
				m.log.Info("the end of a CA rotation stopped between its two renames: ca-next.crt is the cluster CA, the key already in ca.key, and replaces ca.crt",
					"ca", short(caFP))
				return m.renameNext()
			}
		}
	}
	return m.removeOrphan()
}

// nextFiles returns the new CA of the rotation rp from ca-next.key and
// ca-next.crt, which it writes from the record's sealed copy first when
// they are not there, or do not load as a pair: a write of them that
// stopped between the two files, or before they were synced, leaves one of
// them, or part of one. Files of another CA than the record's are an error.
func (m *Manager) nextFiles(ctx context.Context, cur *pki.CA, rp *store.CARotation) (*pki.CA, error) {
	_, kerr := os.Lstat(filepath.Join(m.dir, pki.NextCAKeyFile))
	_, cerr := os.Lstat(filepath.Join(m.dir, pki.NextCACertFile))
	none := errors.Is(kerr, fs.ErrNotExist) && errors.Is(cerr, fs.ErrNotExist)
	var loadErr error
	if !none {
		_, loadErr = pki.LoadNextCA(m.dir)
	}
	if none || loadErr != nil {
		rc, err := m.st.GetCARotationCA(ctx)
		if err != nil {
			return nil, err
		}
		if rc.Empty() {
			if !none {
				return nil, fmt.Errorf("the store records a CA rotation, and the new CA in %s cannot be read: %w; restore the data directory from a backup", m.dir, loadErr)
			}
			return nil, fmt.Errorf("the store records a CA rotation, and %s has neither the new CA's files nor the record a copy of it; "+
				"restore the data directory from a backup", m.dir)
		}
		keyPEM, err := openNextKey(cur, rp.NextFP, rc.SealedKey)
		if err != nil {
			return nil, err
		}
		next, err := pki.ParseCAPair([]byte(rc.CertPEM), keyPEM)
		if err != nil {
			return nil, fmt.Errorf("the new CA in the CA rotation's record: %w", err)
		}
		if next.Fingerprint() != rp.NextFP {
			return nil, fmt.Errorf("the new CA in the CA rotation's record is %s, and the record names %s", short(next.Fingerprint()), short(rp.NextFP))
		}
		if !none {
			m.log.Warn("the new CA's files do not load, and are written again from the CA rotation's record", "error", loadErr.Error())
			for _, name := range []string{pki.NextCAKeyFile, pki.NextCACertFile} {
				if err := os.Remove(filepath.Join(m.dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return nil, err
				}
			}
		}
		if _, err := pki.WriteNextCAFiles(m.dir, []byte(rc.CertPEM), keyPEM); err != nil {
			return nil, fmt.Errorf("writing the new CA's files from the CA rotation's record: %w", err)
		}
		m.log.Info("wrote this server's copy of the new CA from the CA rotation's record", "ca", short(rp.NextFP))
	}
	next, err := pki.LoadNextCA(m.dir)
	if err != nil {
		return nil, fmt.Errorf("the store records a CA rotation, and the new CA in %s cannot be read: %w; restore the data directory from a backup", m.dir, err)
	}
	if next.Fingerprint() != rp.NextFP {
		return nil, fmt.Errorf("the CA files in %s are not the CAs of the rotation the store records (%s is %s, the store has %s); "+
			"restore the data directory from a backup", m.dir, pki.NextCACertFile, short(next.Fingerprint()), short(rp.NextFP))
	}
	return next, nil
}

// recordGaps records in the store what its records lack and this server's
// files have: the cluster CA's fingerprint, and the new CA of a rotation
// recorded by 2.3.0. A follower's store refuses it with ErrNotLeader. The
// caller holds mu.
func (m *Manager) recordGaps(ctx context.Context) error {
	if m.st.Schema() < store.CAStoreSchemaVersion {
		return nil
	}
	s := m.cur.Load()
	if s.rot == nil {
		if _, known, err := m.st.ClusterCA(ctx); err != nil || known {
			return err
		}
		return m.st.SetClusterCA(ctx, s.current.Fingerprint())
	}
	if s.next == nil || s.rot.Phase == store.CAPhaseEnding {
		return nil
	}
	if _, known, err := m.st.ClusterCA(ctx); err != nil {
		return err
	} else if !known {
		if err := m.st.SetClusterCA(ctx, s.current.Fingerprint()); err != nil {
			return err
		}
	}
	rc, err := m.st.GetCARotationCA(ctx)
	if err != nil || !rc.Empty() {
		return err
	}
	keyPEM, err := s.next.KeyPEM()
	if err != nil {
		return err
	}
	sealed, err := sealNextKey(s.current, s.next.Fingerprint(), keyPEM)
	if err != nil {
		return err
	}
	if err := m.st.FillCARotationCA(ctx, s.next.Fingerprint(), store.CARotationCA{CertPEM: string(s.next.CertPEM), SealedKey: sealed}); err != nil {
		return err
	}
	m.log.Info("recorded the new CA in the CA rotation's record, which a rotation started by 2.3.0 lacks", "ca", short(s.next.Fingerprint()))
	return nil
}

// adopt makes s the state, unless it is the state there is already. When
// the CA of the server's own certificate changes and the CAs it accepts
// narrow at once, as when one look at the record finds both the
// listener's move and the rotation's end, the server's certificate moves
// first: the accepted CAs narrow once it is from the new listener CA, or
// after 5 s. The caller holds mu.
func (m *Manager) adopt(ctx context.Context, s *state) {
	old := m.cur.Load()
	if old == nil {
		// Open's first look.
		nodes, err := m.st.CARotationNodes(ctx)
		if err != nil {
			nodes = map[string]store.CARotationNode{}
		}
		m.nodes = nodes
		s.gen = m.gen.Add(1)
		m.accept = pki.NewTrustHolder(s.accept)
		m.cur.Store(s)
		return
	}
	if sameState(old, s) {
		return
	}
	m.nmu.Lock()
	if pki.Fingerprint(old.listener.Cert) != pki.Fingerprint(s.listener.Cert) && narrower(old, s) {
		mid := *s
		mid.accept = old.accept
		m.setState(&mid)
		m.nmu.Unlock()
		m.awaitServerCert(ctx, s.listener)
		m.nmu.Lock()
	}
	if nodes, err := m.st.CARotationNodes(ctx); err == nil {
		m.nodes = nodes
	}
	m.setState(s)
	m.nmu.Unlock()
}

// sameState reports whether two states are of the same CAs and rotation.
func sameState(a, b *state) bool {
	key := func(s *state) string {
		k := pki.FingerprintHex(s.current.Fingerprint()) + "/" + pki.FingerprintHex(s.listener.Fingerprint())
		if s.next != nil {
			k += "/" + pki.FingerprintHex(s.next.Fingerprint())
		}
		if s.rot != nil {
			k += "/" + s.rot.Phase + "/" + s.rot.Toward + "/" + s.rot.Listener
		}
		return k
	}
	return key(a) == key(b)
}

// narrower reports whether b accepts a CA that a does not, or the other
// way: whether a accepts a CA that b does not.
func narrower(a, b *state) bool {
	for _, fp := range a.accept.Fingerprints() {
		if !b.accept.Contains(fp) {
			return true
		}
	}
	return false
}

// awaitServerCert waits, up to 5 s, until the server's own certificate is
// from ca.
func (m *Manager) awaitServerCert(ctx context.Context, ca *pki.CA) {
	if m.serverCert == nil {
		return
	}
	end := time.Now().Add(5 * time.Second)
	for time.Now().Before(end) && ctx.Err() == nil {
		if c := m.serverCert(); c != nil && c.CheckSignatureFrom(ca.Cert) == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	m.log.Warn("the server's certificate did not move to the listener CA within 5 s; the accepted CAs narrow anyway", "ca", short(ca.Fingerprint()))
}

// SetServerCert gives the Manager the server's own certificate, which a
// change of the listener CA waits for (adopt). serve sets it before it
// runs anything.
func (m *Manager) SetServerCert(f func() *x509.Certificate) { m.serverCert = f }

// OnStateChange adds f to what runs after each change of the CAs or the
// rotation, as the server's own node's client, which takes the accepted
// CAs again. serve adds it before it runs anything. f must not block.
func (m *Manager) OnStateChange(f func()) { m.onState = append(m.onState, f) }

// SetStartCheck makes Start ask f first, which refuses a start by its
// error, as while a server joins the cluster.
func (m *Manager) SetStartCheck(f func(ctx context.Context) error) { m.startCheck = f }

// SetServers gives Status each server's CA files.
func (m *Manager) SetServers(f func(ctx context.Context) []ServerCA) { m.servers = f }

// Follow follows the store's record on this server, every server of a
// cluster of several, until ctx ends: after each change under it, and
// every 10 s.
func (m *Manager) Follow(ctx context.Context) {
	if m.st == nil {
		return
	}
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.changed:
		case <-t.C:
		}
		m.mu.Lock()
		err := m.followLocked(ctx)
		m.mu.Unlock()
		if err != nil && ctx.Err() == nil {
			m.log.Warn("following the CA rotation's record", "err", err)
		}
	}
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
	for _, f := range m.onState {
		f()
	}
}

// Current is the CA in ca.crt.
func (m *Manager) Current() *pki.CA { return m.cur.Load().current }

// Next is the CA in ca-next.crt while a rotation runs, or nil.
func (m *Manager) Next() *pki.CA { return m.cur.Load().next }

// Listener is the CA that signs the server's own certificate, which both
// cluster listeners present. Join tokens name it.
func (m *Manager) Listener() *pki.CA { return m.cur.Load().listener }

// Rotating reports whether a CA rotation is in progress.
func (m *Manager) Rotating() bool { return m.cur.Load().rot != nil }

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
	case seen && old.Trusts && !n.Trusts:
		m.log.Warn("a node that trusted "+towardCA(s.rot)+" sent a request that does not list it", "node", node, "ca", short(toFP),
			"lists", len(trusts))
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
	// A new leader follows the record first, and so finishes a rotation's
	// end that the leader before it left half done, and records what the
	// records lack.
	m.mu.Lock()
	err := m.followLocked(ctx)
	if err == nil {
		err = m.recordGaps(ctx)
	}
	m.mu.Unlock()
	if err != nil && ctx.Err() == nil {
		m.log.Warn("following the CA rotation's record as the leader", "err", err)
	}
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		m.warnExpiry()
		m.recheckForced(ctx)
		if err := m.moveListener(ctx); err != nil && ctx.Err() == nil {
			m.log.Warn("checking whether the server's certificate can move to the CA the rotation goes to", "err", err)
		}
		// The store may have moved to schema version 19 since this server
		// began to lead, as at the end of a rolling upgrade: what 19 adds is
		// recorded then. It writes nothing once it is recorded.
		m.mu.Lock()
		err := m.recordGaps(ctx)
		m.mu.Unlock()
		if err != nil && ctx.Err() == nil && !errors.Is(err, store.ErrNotLeader) {
			m.log.Warn("recording the cluster CA in the store", "err", err)
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

// endingFiles does this server's file step of an ending rotation: towards
// the next CA, ca-next.key and ca-next.crt are renamed over ca.key and
// ca.crt; towards the current CA, they are removed. Each part is done only
// if it is still to do, so a second run after a crash finishes the first.
// The record's end is the caller's.
func (m *Manager) endingFiles(rot store.CARotation) error {
	keep := rot.CurrentFP
	if rot.Toward == store.CANext {
		keep = rot.NextFP
		if err := m.renameNext(); err != nil {
			return err
		}
	} else {
		for _, name := range []string{pki.NextCAKeyFile, pki.NextCACertFile} {
			if err := os.Remove(filepath.Join(m.dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
		if err := syncDir(m.dir); err != nil {
			return err
		}
	}
	ca, err := pki.LoadCA(m.dir)
	if err != nil {
		return fmt.Errorf("the cluster CA after the rotation's end: %w", err)
	}
	if ca.Fingerprint() != keep {
		return fmt.Errorf("the cluster CA in %s is %s after the rotation's end, and the rotation was to keep %s; restore the data directory from a backup",
			m.dir, short(ca.Fingerprint()), short(keep))
	}
	return nil
}

// renameNext renames ca-next.key over ca.key, then ca-next.crt over
// ca.crt, each if it is still there, and syncs the directory.
func (m *Manager) renameNext() error {
	for _, p := range [][2]string{{pki.NextCAKeyFile, pki.CAKeyFile}, {pki.NextCACertFile, pki.CACertFile}} {
		err := os.Rename(filepath.Join(m.dir, p[0]), filepath.Join(m.dir, p[1]))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return syncDir(m.dir)
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
