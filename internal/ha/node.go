// Package ha runs the servers of a cluster of several as a Raft cluster
// (docs/ha.md): hashicorp/raft with the store as its state machine, the
// log in a SQLite file of its own, and its traffic, with what followers
// pass to the leader, on the server port, over TLS with the servers'
// certificates.
package ha

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/raft"

	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/store"
)

// Timing is Raft's timing. Zero values are raft's defaults, which are for
// a LAN: a heartbeat timeout and an election timeout of 1 s, and a leader
// lease of 500 ms. Tests shorten them.
type Timing struct {
	Heartbeat, Election, LeaderLease, Commit time.Duration
	SnapshotInterval                         time.Duration
	SnapshotThreshold, TrailingLogs          uint64
}

// Config configures Open.
type Config struct {
	// DataDir is the server's data directory. Raft's state is in Dir(DataDir).
	DataDir string
	Store   *store.Store
	// ID is this server's node ID, its Raft server ID.
	ID string
	// Advertise is the address this server advertises, which its
	// certificate names, and Bind the IP address the server port binds.
	// Port is the server port; 0 picks a free one (tests only).
	Advertise, Bind string
	Port            int
	// Cert is this server's certificate, the one its cluster listeners
	// present, and Trust the CAs the cluster accepts.
	Cert  *pki.CertHolder
	Trust *pki.TrustHolder
	// Removed reports a node on the denylist. Nil means none is.
	Removed func(id string) bool
	// Seeds are the servers a joining server takes on the server port
	// before it has a Raft configuration: those the join reply named.
	Seeds []string
	// OnLeader runs while this server leads, once it has applied every
	// entry of the terms before its own; its context ends when it stops
	// leading. Nil runs nothing.
	OnLeader func(ctx context.Context)
	// Fatal is called once when an entry cannot be applied: the server
	// must stop. Nil only logs.
	Fatal  func(error)
	Logger *slog.Logger
	Timing Timing
}

// Dir is the Raft directory of a data directory.
func Dir(dataDir string) string { return filepath.Join(dataDir, "raft") }

// logFile is the log store's file in the Raft directory.
const logFile = "raft.db"

// HasState reports whether the data directory has Raft state: its server
// has been in a cluster of several.
func HasState(dataDir string) bool {
	_, err := os.Stat(filepath.Join(Dir(dataDir), logFile))
	return err == nil
}

// Node is this server's part of the Raft cluster.
type Node struct {
	cfg   Config
	log   *slog.Logger
	raft  *raft.Raft
	trans *observedTransport
	logs  *logStore
	snaps *raft.FileSnapshotStore
	mux   *mux
	tlsm  *tlsMaterial
	// fails limits raft's repeated lines, and knows of a handover (peerlog.go).
	fails *peerFailures
	addr  string // this server's Raft address

	fwdSrv    *http.Server
	fwdClient *http.Client
	handlers  atomic.Pointer[Handlers]

	// ready is the leadership this server holds, once it has applied
	// every earlier entry; 0 while it does not lead. leaderships counts
	// them.
	ready       atomic.Uint64
	readyTerm   atomic.Uint64 // the Raft term of ready
	leaderships atomic.Uint64

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// Open opens the Raft state in cfg.DataDir, which a server has once it has
// been in a cluster of several, binds the server port and starts Raft, and
// makes every write of the store go through the log. A server that joined
// and has no state yet waits for the leader to send it the store. Enable
// is for a cluster of one that turns Raft on.
func Open(cfg Config) (*Node, error) { return openNode(cfg, true) }

func openNode(cfg Config, setReplicator bool) (*Node, error) {
	if cfg.Store == nil || cfg.ID == "" || cfg.Advertise == "" || cfg.Bind == "" || cfg.Cert == nil || cfg.Trust == nil {
		return nil, errors.New("ha: Store, ID, Advertise, Bind, Cert and Trust are required")
	}
	if err := pki.ValidateNodeID(cfg.ID); err != nil {
		return nil, err
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	dir := Dir(cfg.DataDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	n := &Node{cfg: cfg, log: log, stop: make(chan struct{})}
	hl := newHCLogger(log)
	n.fails = hl.fails
	logs, err := openLogStore(filepath.Join(dir, logFile))
	if err != nil {
		return nil, err
	}
	n.logs = logs
	snaps, err := raft.NewFileSnapshotStoreWithLogger(dir, 2, hl.Named("snapshot"))
	if err != nil {
		logs.Close()
		return nil, err
	}
	n.snaps = snaps
	if err := tightenModes(dir); err != nil {
		logs.Close()
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	err = catchUpStore(ctx, cfg.Store, snaps, dir, log)
	cancel()
	if err != nil {
		logs.Close()
		return nil, err
	}
	ln, err := listenServerPort(cfg.Bind, cfg.Port)
	if err != nil {
		logs.Close()
		return nil, fmt.Errorf("the server port on %s: %w", net.JoinHostPort(cfg.Bind, strconv.Itoa(cfg.Port)), err)
	}
	port := cfg.Port
	if port == 0 {
		port = ln.Addr().(*net.TCPAddr).Port
	}
	n.addr = RaftAddr(cfg.Advertise, port)
	n.tlsm = &tlsMaterial{cert: cfg.Cert, trust: cfg.Trust, allow: n.allow}
	n.mux = newMux(ln, n.tlsm, log, serverAddr(n.addr))
	nt := raft.NewNetworkTransportWithLogger(&streamLayer{connQueue: n.mux.raft, m: n.tlsm}, 3, 10*time.Second, hl.Named("transport"))
	n.trans = &observedTransport{NetworkTransport: nt, seen: map[raft.ServerID]time.Time{}, fails: hl.fails}

	rc := raft.DefaultConfig()
	rc.LocalID = raft.ServerID(cfg.ID)
	rc.Logger = hl
	rc.NoSnapshotRestoreOnStart = true
	t := cfg.Timing
	if t.Heartbeat > 0 {
		rc.HeartbeatTimeout = t.Heartbeat
	}
	if t.Election > 0 {
		rc.ElectionTimeout = t.Election
	}
	if t.LeaderLease > 0 {
		rc.LeaderLeaseTimeout = t.LeaderLease
	}
	if t.Commit > 0 {
		rc.CommitTimeout = t.Commit
	}
	if t.SnapshotInterval > 0 {
		rc.SnapshotInterval = t.SnapshotInterval
	}
	if t.SnapshotThreshold > 0 {
		rc.SnapshotThreshold = t.SnapshotThreshold
	}
	if t.TrailingLogs > 0 {
		rc.TrailingLogs = t.TrailingLogs
	}
	f := &fsm{st: cfg.Store, dir: dir, log: log, fatal: func(err error) {
		log.Error("this server cannot apply the cluster's log, and stops: its store would fall behind the others'", "err", err)
		if cfg.Fatal != nil {
			cfg.Fatal(err)
		}
	}}
	cache, err := raft.NewLogCache(512, logs)
	if err != nil {
		n.mux.close()
		logs.Close()
		return nil, err
	}
	r, err := raft.NewRaft(rc, f, cache, logs, snaps, n.trans)
	if err != nil {
		n.mux.close()
		logs.Close()
		return nil, err
	}
	n.raft = r
	n.fwdClient = n.newForwardClient()
	n.fwdSrv = &http.Server{Handler: n.receiver(), ConnContext: connContext, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 64 << 10,
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelDebug)}
	n.wg.Add(4)
	go func() { defer n.wg.Done(); n.mux.serve() }()
	go func() { defer n.wg.Done(); _ = n.fwdSrv.Serve(n.mux.forward) }()
	go func() { defer n.wg.Done(); n.watchLeadership() }()
	go func() { defer n.wg.Done(); n.promoteLoop() }()
	if setReplicator {
		cfg.Store.SetReplicator(n)
	}
	log.Info("raft started", "id", cfg.ID, "addr", n.addr, "bind", ln.Addr().String())
	return n, nil
}

// Addr is this server's Raft address, its advertise address and the
// server port.
func (n *Node) Addr() string { return n.addr }

// BoundAddr is the address the server port is bound to.
func (n *Node) BoundAddr() string { return n.mux.ln.Addr().String() }

// bootstrap starts a Raft cluster with this server as its only voter, and
// hands raft the store as it is as the log's first snapshot (raft.Restore),
// so that a server that joins later gets the store and then the entries
// after it.
func (n *Node) bootstrap(ctx context.Context) error {
	cfg := raft.Configuration{Servers: []raft.Server{{ID: raft.ServerID(n.cfg.ID), Address: raft.ServerAddress(n.addr), Suffrage: raft.Voter}}}
	if err := n.raft.BootstrapCluster(cfg).Error(); err != nil {
		return fmt.Errorf("bootstrapping the Raft cluster: %w", err)
	}
	for n.raft.State() != raft.Leader {
		select {
		case <-ctx.Done():
			return fmt.Errorf("this server did not become the leader of its own Raft cluster: %w", ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	if err := n.raft.Barrier(30 * time.Second).Error(); err != nil {
		return err
	}
	path, err := tempPath(Dir(n.cfg.DataDir), "handover-*.db")
	if err != nil {
		return err
	}
	defer os.Remove(path)
	if err := n.cfg.Store.SnapshotTo(ctx, path); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	meta := &raft.SnapshotMeta{Version: raft.SnapshotVersionMax, Size: fi.Size()}
	if err := n.raft.Restore(meta, f, time.Minute); err != nil {
		return fmt.Errorf("handing the store to Raft: %w", err)
	}
	return nil
}

// Enable turns Raft on in a running cluster of one: it opens Raft in a
// data directory that has no Raft state, bootstraps a cluster with this
// server as its only voter, and hands it the store as the log's first
// snapshot, with no write running meanwhile; every write after goes
// through the log. On an error nothing is left of the Raft state, and the
// store writes as before.
func Enable(ctx context.Context, cfg Config) (*Node, error) {
	if HasState(cfg.DataDir) {
		return nil, errors.New("ha: the data directory has Raft state already")
	}
	if cfg.Store.Replicated() {
		return nil, errors.New("ha: the store replicates already")
	}
	// The log starts at 1, so the store's applied index starts again.
	if err := cfg.Store.ResetAppliedIndex(ctx); err != nil {
		return nil, err
	}
	var n *Node
	err := cfg.Store.EnableReplication(func() (store.Replicator, error) {
		var err error
		if n, err = openNode(cfg, false); err != nil {
			_ = os.RemoveAll(Dir(cfg.DataDir))
			return nil, err
		}
		if err := n.bootstrap(ctx); err != nil {
			n.Close()
			_ = os.RemoveAll(Dir(cfg.DataDir))
			return nil, err
		}
		return n, nil
	})
	if err != nil {
		return nil, err
	}
	return n, nil
}

// watchLeadership follows raft's leadership changes. Once this server
// leads, and has applied every entry before its term (a barrier), it can
// write, and OnLeader runs until it stops leading.
func (n *Node) watchLeadership() {
	var cancel context.CancelFunc
	var done chan struct{}
	stopLeading := func() {
		n.ready.Store(0)
		if cancel != nil {
			cancel()
			<-done
			cancel, done = nil, nil
		}
	}
	defer stopLeading()
	ch := n.raft.LeaderCh()
	leading := n.raft.State() == raft.Leader
	for {
		if leading && cancel == nil {
			if n.becomeLeader() {
				gen := n.ready.Load()
				ctx, c := context.WithCancel(context.Background())
				cancel, done = c, make(chan struct{})
				n.log.Info("this server leads the cluster", "id", n.cfg.ID, "term", n.raft.CurrentTerm(), "leadership", gen)
				go func(d chan struct{}) {
					defer close(d)
					if n.cfg.OnLeader != nil {
						n.cfg.OnLeader(ctx)
					}
				}(done)
			}
		}
		// A leadership lost and won again between two looks is a new term,
		// which needs its own barrier.
		if !leading || (cancel != nil && n.raft.CurrentTerm() != n.readyTerm.Load()) {
			stopLeading()
		}
		select {
		case <-n.stop:
			return
		case l := <-ch:
			if !l && cancel != nil {
				n.log.Info("this server no longer leads the cluster", "id", n.cfg.ID)
			}
			leading = l
		case <-time.After(time.Second):
			// A barrier that failed is tried again while this server leads.
			leading = n.raft.State() == raft.Leader
		}
	}
}

// becomeLeader waits until every entry before this term is applied, and
// marks the leadership ready. It reports false if the leadership ended
// first.
func (n *Node) becomeLeader() bool {
	term := n.raft.CurrentTerm()
	if err := n.raft.Barrier(10 * time.Second).Error(); err != nil {
		n.log.Warn("the new leader could not apply the entries before its term yet", "err", err)
		return false
	}
	if n.raft.State() != raft.Leader || n.raft.CurrentTerm() != term {
		return false
	}
	n.cfg.Store.TookLeadership()
	n.readyTerm.Store(term)
	n.ready.Store(n.leaderships.Add(1))
	return true
}

// CanWrite is the store's question before a transaction that writes
// (store.Replicator).
func (n *Node) CanWrite() (uint64, error) {
	g := n.ready.Load()
	if g == 0 || n.raft.State() != raft.Leader || n.raft.CurrentTerm() != n.readyTerm.Load() {
		return 0, n.notLeader()
	}
	return g, nil
}

// notLeader is the error for a write this server cannot take: it names the
// leader, or says there is none.
func (n *Node) notLeader() error {
	if addr, id := n.raft.LeaderWithID(); id != "" && string(id) != n.cfg.ID {
		return &NotLeaderError{Leader: string(id), Addr: string(addr)}
	}
	return &NotLeaderError{Reachable: n.reachable(), Servers: n.voters()}
}

// NotLeaderError is a write on a server that does not lead.
type NotLeaderError struct {
	// Leader and Addr name the leader, when this server knows one.
	Leader, Addr string
	// Reachable is how many voters, this one included, this server has
	// heard from lately, and Servers how many there are, when there is no
	// leader.
	Reachable, Servers int
}

func (e *NotLeaderError) Error() string {
	if e.Leader != "" {
		return fmt.Sprintf("this server is not the leader; %s at %s is", e.Leader, e.Addr)
	}
	return fmt.Sprintf("no leader: this server reaches %d of the %d servers, and a write needs %d; running containers are not affected",
		e.Reachable, e.Servers, e.Servers/2+1)
}

func (e *NotLeaderError) Unwrap() error { return store.ErrNotLeader }

// Replicate appends a batch to the log and waits until this server has
// applied it (store.Replicator).
func (n *Node) Replicate(ctx context.Context, term uint64, batch []byte) error {
	if g := n.ready.Load(); g == 0 || g != term {
		return n.notLeader()
	}
	f := n.raft.Apply(batch, 10*time.Second)
	errc := make(chan error, 1)
	go func() {
		err := f.Error()
		if err == nil {
			if r, ok := f.Response().(error); ok && r != nil {
				err = r
			}
		}
		errc <- err
	}()
	select {
	case err := <-errc:
		switch {
		case err == nil:
			return nil
		case errors.Is(err, raft.ErrNotLeader), errors.Is(err, raft.ErrLeadershipTransferInProgress),
			errors.Is(err, raft.ErrEnqueueTimeout), errors.Is(err, raft.ErrRaftShutdown):
			return fmt.Errorf("%w (%v)", n.notLeader(), err)
		case errors.Is(err, raft.ErrLeadershipLost), errors.Is(err, raft.ErrAbortedByRestore):
			return fmt.Errorf("%w: %v", store.ErrOutcomeUnknown, err)
		default:
			return err
		}
	case <-ctx.Done():
		return fmt.Errorf("%w: %v", store.ErrOutcomeUnknown, ctx.Err())
	}
}

// IsLeader reports whether this server leads and can write.
func (n *Node) IsLeader() bool {
	_, err := n.CanWrite()
	return err == nil
}

// Leader returns the leader's ID and Raft address, or empty strings when
// this server knows of none.
func (n *Node) Leader() (id, addr string) {
	a, i := n.raft.LeaderWithID()
	return string(i), string(a)
}

// configuration is the latest Raft configuration this server has.
func (n *Node) configuration() []raft.Server {
	f := n.raft.GetConfiguration()
	if err := f.Error(); err != nil {
		return nil
	}
	return f.Configuration().Servers
}

func (n *Node) voters() int {
	c := 0
	for _, s := range n.configuration() {
		if s.Suffrage == raft.Voter {
			c++
		}
	}
	return c
}

// recentContact is how recent a server's last contact must be to count as
// reachable.
const recentContact = 10 * time.Second

// reachable counts the voters this server has heard from lately: itself;
// the followers that answered it, if it leads; the leader, if it follows
// one.
func (n *Node) reachable() int {
	c := 0
	now := time.Now()
	_, leader := n.raft.LeaderWithID()
	leads := n.raft.State() == raft.Leader
	for _, s := range n.configuration() {
		if s.Suffrage != raft.Voter {
			continue
		}
		switch {
		case string(s.ID) == n.cfg.ID:
			c++
		// What a server heard from the followers while it led says
		// nothing once it does not.
		case leads && now.Sub(n.trans.lastSeen(s.ID)) < recentContact:
			c++
		case s.ID == leader && now.Sub(n.raft.LastContact()) < recentContact:
			c++
		}
	}
	return c
}

// allow decides whether a server port peer is taken: not removed, and in
// the Raft configuration, or a seed while this server has none.
func (n *Node) allow(id string) error {
	if n.cfg.Removed != nil && n.cfg.Removed(id) {
		return fmt.Errorf("node %s was removed from the cluster", id)
	}
	if id == n.cfg.ID {
		return nil
	}
	if n.raft != nil {
		for _, s := range n.configuration() {
			if string(s.ID) == id {
				return nil
			}
		}
	}
	if slices.Contains(n.cfg.Seeds, id) {
		return nil
	}
	return fmt.Errorf("server %s is not in this server's Raft configuration", id)
}

// Member is one server of the Raft configuration, as this server sees it.
type Member struct {
	ID, Addr string
	// Voter is false for a server that is catching up (a non-voter).
	Voter bool
	// Leader is true for the leader this server knows.
	Leader bool
	// LastContact is when this server last heard from the member: now for
	// itself, the last answer to its heartbeats for a follower when it
	// leads, and the leader's last message when it follows. Zero for
	// never, or for another follower.
	LastContact time.Time
}

// Members lists the Raft configuration, sorted by ID.
func (n *Node) Members() []Member {
	_, leader := n.raft.LeaderWithID()
	now := time.Now()
	var out []Member
	for _, s := range n.configuration() {
		m := Member{ID: string(s.ID), Addr: string(s.Address), Voter: s.Suffrage == raft.Voter, Leader: s.ID == leader}
		switch {
		case m.ID == n.cfg.ID:
			m.LastContact = now
		case n.raft.State() == raft.Leader:
			m.LastContact = n.trans.lastSeen(s.ID)
		case s.ID == leader:
			m.LastContact = n.raft.LastContact()
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// HasLeader reports whether this server knows a leader.
func (n *Node) HasLeader() bool {
	_, id := n.raft.LeaderWithID()
	return id != ""
}

// membershipTimeout bounds a change of the configuration.
const membershipTimeout = 30 * time.Second

// AddNonvoter adds a server that has joined as a non-voter: it gets the
// log, and does not count for a majority until it is made a voter.
func (n *Node) AddNonvoter(id, addr string) error {
	if !n.IsLeader() {
		return n.notLeader()
	}
	for _, s := range n.configuration() {
		if string(s.ID) == id {
			return nil
		}
	}
	return n.raft.AddNonvoter(raft.ServerID(id), raft.ServerAddress(addr), 0, membershipTimeout).Error()
}

// Promote makes the non-voter id a voter.
func (n *Node) Promote(id string) error {
	if !n.IsLeader() {
		return n.notLeader()
	}
	for _, s := range n.configuration() {
		if string(s.ID) != id {
			continue
		}
		if s.Suffrage == raft.Voter {
			return nil
		}
		return n.raft.AddVoter(s.ID, s.Address, 0, membershipTimeout).Error()
	}
	return fmt.Errorf("server %s is not in the Raft configuration", id)
}

// RemoveServer removes a server from the configuration.
func (n *Node) RemoveServer(id string) error {
	if !n.IsLeader() {
		return n.notLeader()
	}
	return n.raft.RemoveServer(raft.ServerID(id), 0, membershipTimeout).Error()
}

// Removed reports whether this server's raft has shut down because the
// configuration no longer has it.
func (n *Node) Removed() bool {
	if n.raft.State() != raft.Shutdown {
		return false
	}
	for _, s := range n.configuration() {
		if string(s.ID) == n.cfg.ID {
			return false
		}
	}
	return true
}

// CaughtUp reports whether this server has applied every entry the
// leader has told it is committed, and heard from the leader lately.
func (n *Node) CaughtUp() bool {
	if n.raft.State() == raft.Leader {
		return true
	}
	if !n.HasLeader() || time.Since(n.raft.LastContact()) > recentContact {
		return false
	}
	ci := n.raft.CommitIndex()
	return ci > 0 && n.raft.AppliedIndex() >= ci
}

// IsVoter reports whether the configuration has this server as a voter.
func (n *Node) IsVoter() bool { return n.IsVoterID(n.cfg.ID) }

// IsVoterID reports whether the configuration this server has names id
// as a voter.
func (n *Node) IsVoterID(id string) bool {
	for _, s := range n.configuration() {
		if string(s.ID) == id {
			return s.Suffrage == raft.Voter
		}
	}
	return false
}

// Handover passes the leadership to another voter when this server leads
// and the configuration has one, and returns once this server no longer
// leads, the transfer failed, or ctx ended. A server that stops calls it
// first: raft picks the follower that is furthest along, brings it up to
// date and has it start an election at once (TimeoutNow), which the other
// followers grant though they heard from this leader lately. Without it a
// stopping leader led until its Raft stopped, at the end of its stop, and
// the others waited out a heartbeat timeout before they elected one: in
// the eighth soak, 10 s with the API and router closed, then 1.0 to 1.7 s
// with no leader. Writes that come meanwhile are refused as on a server
// that does not lead, and a client may send them again. Returns nil when
// there is nothing to hand over.
func (n *Node) Handover(ctx context.Context) error {
	if n.raft.State() != raft.Leader {
		return nil
	}
	others := 0
	for _, s := range n.configuration() {
		if s.Suffrage == raft.Voter && string(s.ID) != n.cfg.ID {
			others++
		}
	}
	if others == 0 {
		return nil
	}
	start := time.Now()
	n.log.Info("this server leads, and hands the leadership to another server before it stops", "id", n.cfg.ID)
	// The followers' answers with the new leader's term are expected now.
	n.fails.handingOver()
	f := n.raft.LeadershipTransfer()
	errc := make(chan error, 1)
	go func() { errc <- f.Error() }()
	select {
	case err := <-errc:
		if err != nil {
			return fmt.Errorf("handing the leadership on: %w", err)
		}
	case <-ctx.Done():
		return fmt.Errorf("handing the leadership on: %w", ctx.Err())
	}
	took := time.Since(start).Round(time.Millisecond)
	if id := n.newLeader(ctx); id != "" {
		n.log.Info("handed the leadership on", "to", id, "took", took)
	} else {
		n.log.Info("handed the leadership on; no other server has said it leads yet", "took", took)
	}
	return nil
}

// newLeaderWait bounds Handover's wait for the new leader's name.
const newLeaderWait = time.Second

// newLeader waits, up to newLeaderWait or the end of ctx, until this
// server hears from another that leads, and returns its ID, or "". Raft's
// transfer ends once the follower was told to start its election, before
// it has won: the old leader then knows no leader until the new one's
// first message, so Handover logged to="" (ninth soak).
func (n *Node) newLeader(ctx context.Context) string {
	t := time.NewTimer(newLeaderWait)
	defer t.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		if id, _ := n.Leader(); id != "" && id != n.cfg.ID {
			return id
		}
		select {
		case <-ctx.Done():
			return ""
		case <-t.C:
			return ""
		case <-tick.C:
		}
	}
}

// Close stops Raft and the server port.
func (n *Node) Close() error {
	var err error
	n.stopOnce.Do(func() {
		close(n.stop)
		err = n.raft.Shutdown().Error()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = n.fwdSrv.Shutdown(ctx)
		cancel()
		_ = n.mux.close()
		_ = n.trans.Close()
		n.wg.Wait()
		err = errors.Join(err, n.logs.Close())
	})
	return err
}
