package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"

	"github.com/ao/hivemind/internal/agent"
	"github.com/ao/hivemind/internal/auth"
	"github.com/ao/hivemind/internal/carotate"
	"github.com/ao/hivemind/internal/cluster"
	"github.com/ao/hivemind/internal/config"
	"github.com/ao/hivemind/internal/ha"
	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/router"
	"github.com/ao/hivemind/internal/runtime"
	"github.com/ao/hivemind/internal/runtime/docker"
	"github.com/ao/hivemind/internal/server"
	"github.com/ao/hivemind/internal/store"
	"github.com/ao/hivemind/internal/tlsstore"
)

// A cluster of several servers (docs/ha.md): what serve runs for it.

// serverStateFile is server.json, in the data directory of a server that
// joined a cluster with hivemind join --role server: its node ID and the
// servers it joined. A server that init made has none.
const serverStateFile = "server.json"

// serverState is server.json.
type serverState struct {
	NodeID  string               `json:"node_id"`
	Joined  time.Time            `json:"joined"`
	Servers []nodeapi.ServerInfo `json:"servers"`
}

// errNotJoinedServer means the data directory holds no joined server.
var errNotJoinedServer = errors.New("this machine has not joined a cluster as a server")

func readServerState(dir string) (serverState, error) {
	var st serverState
	raw, err := os.ReadFile(filepath.Join(dir, serverStateFile))
	if errors.Is(err, fs.ErrNotExist) {
		return st, errNotJoinedServer
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return st, fmt.Errorf("%s is not valid: %w", serverStateFile, err)
	}
	if err := pki.ValidateNodeID(st.NodeID); err != nil {
		return st, fmt.Errorf("%s: %w", serverStateFile, err)
	}
	return st, nil
}

// writeServerState writes server.json, mode 0600, with O_EXCL.
func writeServerState(dir string, st serverState) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, serverStateFile), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// startingReplicator is the store's replicator on a server of a cluster of
// several until Raft runs: it refuses every write, so that nothing writes
// the store but the log.
type startingReplicator struct{}

func (startingReplicator) CanWrite() (uint64, error) {
	return 0, fmt.Errorf("%w: this server is starting", store.ErrNotLeader)
}

func (startingReplicator) Replicate(context.Context, uint64, []byte) error {
	return fmt.Errorf("%w: this server is starting", store.ErrNotLeader)
}

// leaderWork is what only the leader runs: the sweep, rollouts and drains,
// and the CA rotation manager's run. A cluster of one runs it from the
// start; a server of several from when it leads, until it stops.
type leaderWork struct {
	run     func(ctx context.Context, started time.Time)
	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	started atomic.Int64
}

// start runs the work from started, unless it runs already; it reports
// whether it started it.
func (l *leaderWork) start(started time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cancel != nil {
		return false
	}
	ctx, cancel := context.WithCancel(context.Background())
	l.cancel, l.done = cancel, make(chan struct{})
	l.started.Store(started.UnixNano())
	go func(done chan struct{}) {
		defer close(done)
		l.run(ctx, started)
	}(l.done)
	return true
}

// stop ends the work, and waits for it.
func (l *leaderWork) stop() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cancel == nil {
		return
	}
	l.cancel()
	<-l.done
	l.cancel, l.done = nil, nil
}

func (l *leaderWork) running() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cancel != nil
}

// since is when the work last started.
func (l *leaderWork) since() time.Time { return time.Unix(0, l.started.Load()) }

// haServe holds what serve runs for a cluster of several servers. Its node
// is nil in a cluster of one, until a server joins.
type haServe struct {
	cfg      *config.Config
	log      *slog.Logger
	st       *store.Store
	rot      *carotate.Manager
	nodes    *cluster.Cluster
	holder   *cluster.HAHolder
	selfID   string
	hostname string
	nodeAddr string
	rt       runtime.Runtime
	seeds    []string
	work     *leaderWork
	// fatal takes an error that stops serve.
	fatal chan error

	node   atomic.Pointer[ha.Node]
	remote atomic.Pointer[nodeapi.Client]
	// mu serialises turning Raft on and the server joins.
	mu sync.Mutex
	// handlers is what forwarded requests reach once this server leads.
	handlers ha.Handlers
	// hadState is true when the data directory had Raft state as serve
	// started.
	hadState bool
	// localReads is the test knob envTestLocalReads.
	localReads bool
	// extras starts what a server of several runs beside the leader's
	// work: its node's log requests and router counters through the node
	// API. Set by serve.
	extras func(c *nodeapi.Client)
	// listed is the last list of servers a node API reply to this server's
	// own node named (nodeapi.ServersHeader), and nodeRemoved is set once
	// the node API refused its node as removed: how a server that was
	// removed while it was down learns it (watchRemoved).
	listed      atomic.Pointer[[]string]
	nodeRemoved atomic.Bool
	// builds is what the leader last learnt of each server's build
	// (schemaLoop), for hivemind servers; nil until it has asked.
	builds atomic.Pointer[[]ha.ServerBuild]
}

// handoverTimeout bounds the handover of the leadership as serve stops.
// Raft's own transfer gives up after its election timeout, 1 s.
const handoverTimeout = 3 * time.Second

// handover passes the leadership to another server, if this one leads a
// cluster of several, before serve stops (ha.Node.Handover). If that
// fails, serve stops as it did before, and the others elect a leader once
// they stop hearing from this one.
func (h *haServe) handover() {
	n := h.node.Load()
	if n == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), handoverTimeout)
	defer cancel()
	if err := n.Handover(ctx); err != nil {
		h.log.Warn("this server leads and stops, and could not hand the leadership on; the others elect a leader once it has stopped", "err", err)
	}
}

// leader reports whether this server writes: always in a cluster of one.
func (h *haServe) leader() bool {
	n := h.node.Load()
	return n == nil || n.IsLeader()
}

func (h *haServe) raftPort() int { return h.cfg.ClusterPort + 2 }

func (h *haServe) haConfig() ha.Config {
	return ha.Config{DataDir: h.cfg.DataDir, Store: h.st, ID: h.selfID, Advertise: h.cfg.Advertise, Bind: h.nodeAddr,
		Port: h.raftPort(), Cert: h.nodes.CertHolder(), Trust: h.rot.Accept(), Seeds: h.seeds,
		Removed: func(id string) bool {
			removed, err := h.st.IsNodeRemoved(context.Background(), id)
			return err == nil && removed
		},
		OnLeader: h.onLeader,
		Build:    h.buildInfo,
		Fatal: func(err error) {
			select {
			case h.fatal <- err:
			default:
			}
		},
		Logger: h.log.With("component", "raft")}
}

// open starts Raft on a server that has been in a cluster of several.
func (h *haServe) open() error {
	n, err := ha.Open(h.haConfig())
	if err != nil {
		return err
	}
	h.started(n)
	return nil
}

// enable turns Raft on in this cluster of one, at its first server join.
func (h *haServe) enable(ctx context.Context) (*ha.Node, error) {
	if n := h.node.Load(); n != nil {
		return n, nil
	}
	if h.cfg.Advertise == "" {
		return nil, fmt.Errorf("%w: this server runs without --advertise", store.ErrConflict)
	}
	h.log.Info("a server joins: this cluster of one turns Raft on, with its store as the log's first snapshot; writes go through the log from now on")
	n, err := ha.Enable(ctx, h.haConfig())
	if err != nil {
		return nil, fmt.Errorf("turning Raft on: %w", err)
	}
	h.started(n)
	return n, nil
}

// started makes n the cluster's Raft node: the listeners pass requests
// to the leader through it, it serves the requests passed to this server,
// and this server's own node reaches the leader through the node API.
func (h *haServe) started(n *ha.Node) {
	n.SetHandlers(h.handlers)
	h.node.Store(n)
	h.holder.Set(n)
	c, err := h.ownClient()
	if err != nil {
		h.log.Error("this server's own node cannot reach the node API", "err", err)
	} else {
		c.OnServers(func(list []string) { h.listed.Store(&list) })
		h.remote.Store(c)
		if h.extras != nil {
			h.extras(c)
		}
	}
	go h.watchRemoved(n)
}

// serverRemovedError is the error serve stops with once this server is removed.
func serverRemovedError(id string) error {
	return &exitError{code: exitRemoved, err: fmt.Errorf("server %s was removed from the cluster's servers (hivemind server remove); "+
		"its containers keep running. To use this machine as a server again, give it an empty --data-dir and a new server join token", id)}
}

// watchRemoved stops serve once this server is removed from the cluster:
// once it has applied the deletion of its own row, as a server removed
// while it runs does; or, for one that was down when it was removed and
// never gets that entry, once the node API refuses its node as removed,
// or the other servers' node API replies leave it out of the list of
// servers while it knows no leader (the fifth acceptance run's third
// finding: such a server ran as a candidate whose vote requests the others
// refused, without end).
func (h *haServe) watchRemoved(n *ha.Node) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	w := removalWatch{seen: h.hadState}
	self := net.JoinHostPort(h.cfg.Advertise, strconv.Itoa(h.cfg.ClusterPort))
	for range t.C {
		servers, err := h.st.ListServers(context.Background())
		var listed []string
		if p := h.listed.Load(); p != nil {
			listed = *p
		}
		unlisted := w.unlisted(listed, self, n.HasLeader())
		if n.Removed() || h.nodeRemoved.Load() || unlisted || (err == nil && w.removed(servers, h.selfID)) {
			if unlisted {
				h.log.Warn("the other servers no longer list this one, and it knows no leader", "listed", strings.Join(listed, ","), "self", self)
			}
			select {
			case h.fatal <- serverRemovedError(h.selfID):
			default:
			}
			return
		}
	}
}

// removalWatch tells a removed server from its store: Raft stops sending a
// follower anything once a change that removes it is in the leader's log,
// so the follower never sees the change, but it has applied the deletion
// of its row, which server remove makes first. A server that has never
// seen its own row, as one that has just joined and is still applying the
// entries that add the servers, is not taken as removed; one that had Raft
// state when serve started has seen it before.
//
// A server that was down when it was removed never applies the deletion.
// Its own node's requests go to the other servers while it knows no
// leader, and their replies name the servers in their stores, without it.
// unlisted takes that as its removal at the second check in a row that
// finds it so, a check every 5 s, so that a list from a server that has
// not yet applied a join, or a moment without a leader, is not.
type removalWatch struct {
	seen bool
	// strikes counts the checks in a row that found this server unlisted.
	strikes int
}

func (w *removalWatch) removed(servers []store.Server, self string) bool {
	if hasServer(servers, self) {
		w.seen = true
		return false
	}
	return w.seen && len(servers) > 0
}

// unlisted reports whether this server, whose node listener is self, is
// taken as removed: the last list of servers a reply named has others but
// not self, and it knows no leader, at this check and the one before.
func (w *removalWatch) unlisted(listed []string, self string, hasLeader bool) bool {
	if hasLeader || len(listed) == 0 || slices.Contains(listed, self) {
		w.strikes = 0
		return false
	}
	w.strikes++
	return w.strikes >= 2
}

// ownClient is the node API client of this server's own node: its server
// certificate, the cluster CA, and every server, this one first.
func (h *haServe) ownClient() (*nodeapi.Client, error) {
	host := h.cfg.Advertise
	tlsCfg, err := pki.ClientTLSConfigTrust(h.rot.Accept().Load(), h.nodes.CertHolder(), host)
	if err != nil {
		return nil, err
	}
	self := net.JoinHostPort(host, strconv.Itoa(h.cfg.ClusterPort))
	list := []string{self}
	if servers, err := h.st.ListServers(context.Background()); err == nil {
		for _, s := range servers {
			if s.ID != h.selfID {
				list = append(list, s.NodeAddr())
			}
		}
	}
	// While this server knows a leader, its own listener passes each
	// request to it: a request starts there, not at the server that
	// answered last, which may have gone since.
	home := func() bool {
		n := h.node.Load()
		return n != nil && n.HasLeader()
	}
	return nodeapi.NewClient("https://"+self, tlsCfg, nodeapi.WithVersion(version), nodeapi.WithServers(list), nodeapi.WithHome(home))
}

// onLeader runs while this server leads a cluster of several. A new leader
// sets the start grace, as serve does at its start, so that the nodes that
// were up wait for their first report to it, and registers its own node.
func (h *haServe) onLeader(ctx context.Context) {
	if !h.work.running() {
		started := time.Now()
		if err := h.st.SetStarted(ctx, started, h.cfg.StartGrace); err != nil && ctx.Err() == nil {
			h.log.Error("setting the start grace as the new leader", "err", err)
		}
		if err := h.upsertOwnNode(ctx); err != nil && ctx.Err() == nil {
			h.log.Warn("registering this server's own node as the new leader", "err", err)
		}
		noteWaiting(ctx, h.st, h.cfg.StartGrace, h.log)
		h.work.start(started)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Raft can make this server the leader before serve has its node.
		for h.node.Load() == nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
		h.schemaLoop(ctx, h.node.Load())
	}()
	<-ctx.Done()
	<-done
	h.builds.Store(nil)
	h.work.stop()
}

// buildInfo is this server's answer to the leader's build request.
func (h *haServe) buildInfo() ha.BuildInfo {
	oldest, newest := h.st.SchemaRange()
	return ha.BuildInfo{Version: nodeapi.CleanVersion(version), SchemaOldest: oldest, SchemaNewest: newest, Schema: h.st.Schema()}
}

// schemaEvery is how often the leader asks every server which build it
// runs (docs/ha.md#the-step).
const schemaEvery = 10 * time.Second

// schemaLoop runs while this server leads: every schemaEvery it asks each
// server of the Raft configuration which build it runs, keeps the answers
// for hivemind servers, and, once every server runs the store's next
// schema version, appends the step to it to the log.
func (h *haServe) schemaLoop(ctx context.Context, n *ha.Node) {
	t := time.NewTimer(time.Second)
	defer t.Stop()
	lastWait := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		t.Reset(schemaEvery)
		builds := n.Builds(ctx)
		if ctx.Err() != nil {
			return
		}
		h.builds.Store(&builds)
		have := h.st.Schema()
		if _, newest := h.st.SchemaRange(); have >= newest {
			continue
		}
		waits := schemaWaits(builds, have+1, h.nodeVersions(ctx))
		if waits != "" {
			if waits != lastWait {
				h.log.Info("the store's schema moves to the next version once every server runs it", "schema", have, "next", have+1,
					"waiting_for", waits)
				lastWait = waits
			}
			continue
		}
		lastWait = ""
		v, err := h.st.StepSchema(ctx)
		if err != nil {
			if ctx.Err() == nil {
				h.log.Warn("moving the store's schema to the next version", "schema", have, "err", err)
			}
			continue
		}
		h.log.Info("every server runs the next schema version, and the store moved to it through the log", "from", have, "to", v)
		// Ask again soon: the servers' answers name the new version, and a
		// build may know a further step.
		t.Reset(time.Second)
	}
}

// schemaWaits names the servers whose builds do not run schema version
// next, with why, or returns "" when every server runs it. versions is
// the version each node last sent, for a build that does not say.
func schemaWaits(builds []ha.ServerBuild, next int, versions map[string]string) string {
	var out []string
	for _, b := range builds {
		switch {
		case b.RunsSchema(next):
			continue
		case !b.Answered:
			out = append(out, b.ID+" (no answer: "+b.Err+")")
		case b.Legacy:
			out = append(out, fmt.Sprintf("%s (%s, which runs schema version %d alone)", b.ID, firstNonEmpty(versions[b.ID], "-"), b.Build.SchemaNewest))
		default:
			out = append(out, fmt.Sprintf("%s (%s, which runs schema versions %d to %d)", b.ID, firstNonEmpty(b.Build.Version, "-"), b.Build.SchemaOldest, b.Build.SchemaNewest))
		}
	}
	if len(builds) == 0 {
		return "the Raft configuration, which this server cannot read"
	}
	return strings.Join(out, ", ")
}

// nodeVersions is the version each node last sent with a request, by node
// ID.
func (h *haServe) nodeVersions(ctx context.Context) map[string]string {
	out := map[string]string{}
	if nodes, err := h.st.ListNodes(ctx); err == nil {
		for _, n := range nodes {
			out[n.ID] = n.Version
		}
	}
	return out
}

// upsertOwnNode registers this server's node in the store, as serve does
// at its start in a cluster of one.
func (h *haServe) upsertOwnNode(ctx context.Context) error {
	info, err := h.rt.Info(ctx)
	if err != nil {
		return err
	}
	return h.st.UpsertNode(ctx, store.Node{ID: h.selfID, Name: h.hostname, CPU: info.NCPU, Memory: info.MemTotal, Address: h.nodeAddr,
		Version: nodeapi.CleanVersion(version)})
}

// register registers this server's node through the node API, as a worker
// does, when it is not the leader.
func (h *haServe) register(ctx context.Context) error {
	c := h.remote.Load()
	if c == nil {
		return errors.New("no node API client")
	}
	info, err := h.rt.Info(ctx)
	if err != nil {
		return err
	}
	return serverSource{h: h}.note(c.Register(ctx, h.selfID, nodeapi.Register{Name: h.hostname, CPU: info.NCPU, Memory: info.MemTotal, Address: h.nodeAddr}))
}

// joinServer admits a server (cluster.ServerJoiner), on the leader.
func (h *haServe) joinServer(ctx context.Context, id string, req nodeapi.JoinServerRequest) (nodeapi.JoinServerReply, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n, err := h.enable(ctx)
	if err != nil {
		return nodeapi.JoinServerReply{}, err
	}
	if !n.IsLeader() {
		return nodeapi.JoinServerReply{}, store.ErrNotLeader
	}
	servers, err := h.st.ListServers(ctx)
	if err != nil {
		return nodeapi.JoinServerReply{}, err
	}
	if len(servers) >= store.MaxServers {
		return nodeapi.JoinServerReply{}, fmt.Errorf("%w: the cluster has %d servers, the most it takes", store.ErrConflict, len(servers))
	}
	if !hasServer(servers, h.selfID) {
		if err := h.st.PutServer(ctx, store.Server{ID: h.selfID, Advertise: h.cfg.Advertise, NodePort: h.cfg.ClusterPort, RaftAddr: n.Addr()}); err != nil {
			return nodeapi.JoinServerReply{}, err
		}
	}
	raftAddr := ha.RaftAddr(req.Advertise, req.NodePort+2)
	for _, s := range servers {
		if s.Advertise == req.Advertise && s.NodePort == req.NodePort {
			return nodeapi.JoinServerReply{}, fmt.Errorf("%w: server %s already advertises %s", store.ErrConflict, s.ID, s.NodeAddr())
		}
	}
	if err := h.st.PutServer(ctx, store.Server{ID: id, Advertise: req.Advertise, NodePort: req.NodePort, RaftAddr: raftAddr}); err != nil {
		return nodeapi.JoinServerReply{}, err
	}
	if err := n.AddNonvoter(id, raftAddr); err != nil {
		_ = h.st.DeleteServer(context.Background(), id)
		return nodeapi.JoinServerReply{}, err
	}
	ca := h.rot.Current()
	keyPEM, err := ca.KeyPEM()
	if err != nil {
		return nodeapi.JoinServerReply{}, err
	}
	reply := nodeapi.JoinServerReply{NodeID: id, CAPEM: string(ca.CertPEM), CAKeyPEM: string(keyPEM)}
	if servers, err = h.st.ListServers(ctx); err != nil {
		return nodeapi.JoinServerReply{}, err
	}
	for _, s := range servers {
		reply.Servers = append(reply.Servers, nodeapi.ServerInfo{ID: s.ID, Advertise: s.Advertise, NodePort: s.NodePort, RaftAddr: s.RaftAddr})
	}
	return reply, nil
}

func hasServer(servers []store.Server, id string) bool {
	for _, s := range servers {
		if s.ID == id {
			return true
		}
	}
	return false
}

// issueServerToken makes a server join token.
func (h *haServe) issueServerToken(ttl time.Duration) (string, time.Time, string, error) {
	if h.rot.Rotating() {
		return "", time.Time{}, "", fmt.Errorf("%w: a CA rotation is in progress, and a server cannot join during one: finish or abort it first", store.ErrConflict)
	}
	count := 1
	if n := h.node.Load(); n != nil {
		count = len(n.Members())
	}
	if count >= store.MaxServers {
		return "", time.Time{}, "", fmt.Errorf("%w: the cluster has %d servers, the most it takes", store.ErrConflict, count)
	}
	warning := ""
	if count+1 == 2 {
		warning = "with this server the cluster has two, and a write needs both: losing either stops writes. Add a third for a cluster that keeps working while one server is down"
	}
	tokens := pki.NewTokens(h.st.ServerJoinTokens())
	tok, exp, err := tokens.Issue(h.rot.Listener().Fingerprint(), ttl)
	return tok, exp, warning, err
}

// servers answers GET /v1/servers.
func (h *haServe) servers(ctx context.Context) (server.ServersReply, error) {
	names, versions := map[string]string{}, map[string]string{}
	if nodes, err := h.st.ListNodes(ctx); err == nil {
		for _, n := range nodes {
			names[n.ID], versions[n.ID] = n.Name, n.Version
		}
	}
	oldest, newest := h.st.SchemaRange()
	n := h.node.Load()
	if n == nil {
		now := time.Now().UTC()
		return server.ServersReply{Leader: h.selfID, AnsweredBy: h.selfID, Note: "a cluster of one server", Schema: h.st.Schema(),
			Servers: []server.ServerEntry{{ID: h.selfID, Name: names[h.selfID], Advertise: h.cfg.Advertise, NodePort: h.cfg.ClusterPort,
				Role: "leader", LastContact: &now, Version: nodeapi.CleanVersion(version), VersionFrom: "build",
				SchemaOldest: oldest, SchemaNewest: newest}}}, nil
	}
	// The leader keeps what it last learnt of each build; a follower that
	// answers, while no server leads, or a leader that has not asked yet,
	// asks now.
	var builds []ha.ServerBuild
	if p := h.builds.Load(); p != nil {
		builds = *p
	} else {
		bctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		builds = n.Builds(bctx)
		cancel()
	}
	byBuild := map[string]ha.ServerBuild{}
	for _, b := range builds {
		byBuild[b.ID] = b
	}
	rows, err := h.st.ListServers(ctx)
	if err != nil {
		return server.ServersReply{}, err
	}
	byID := map[string]store.Server{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	leader, _ := n.Leader()
	rep := server.ServersReply{Leader: leader, AnsweredBy: h.selfID}
	for _, m := range n.Members() {
		e := server.ServerEntry{ID: m.ID, Name: names[m.ID], RaftAddr: m.Addr, Role: "follower"}
		if r, ok := byID[m.ID]; ok {
			e.Advertise, e.NodePort = r.Advertise, r.NodePort
		} else if host, _, err := net.SplitHostPort(m.Addr); err == nil {
			e.Advertise = host
		}
		switch {
		case !m.Voter:
			e.Role = "nonvoter"
		case m.Leader:
			e.Role = "leader"
		}
		if !m.LastContact.IsZero() {
			t := m.LastContact.UTC()
			e.LastContact = &t
		}
		switch b, ok := byBuild[m.ID]; {
		case ok && b.Answered && !b.Legacy:
			e.Version, e.VersionFrom = b.Build.Version, "build"
			e.SchemaOldest, e.SchemaNewest = b.Build.SchemaOldest, b.Build.SchemaNewest
		case versions[m.ID] != "":
			e.Version, e.VersionFrom = versions[m.ID], "node"
		}
		rep.Servers = append(rep.Servers, e)
	}
	if leader == "" {
		rep.Note = "no leader"
		if _, err := n.CanWrite(); err != nil {
			rep.Note = err.Error()
		}
	}
	rep.Schema = h.st.Schema()
	if rep.Schema < newest {
		rep.SchemaNext = rep.Schema + 1
		rep.SchemaWaits = schemaWaits(builds, rep.SchemaNext, versions)
	}
	return rep, nil
}

// removeServer answers DELETE /v1/servers/{id}, on the leader.
func (h *haServe) removeServer(ctx context.Context, id string, force bool) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := h.node.Load()
	if n == nil {
		return "", fmt.Errorf("%w: the cluster has one server, which cannot be removed", store.ErrConflict)
	}
	var target *ha.Member
	voters, staying, heard := 0, 0, 0
	now := time.Now()
	for _, m := range n.Members() {
		if m.ID == id {
			mm := m
			target = &mm
			continue
		}
		if m.Voter {
			staying++
			if now.Sub(m.LastContact) < 10*time.Second {
				heard++
			}
		}
	}
	for _, m := range n.Members() {
		if m.Voter {
			voters++
		}
	}
	if target == nil {
		return "", fmt.Errorf("server %q: %w", id, store.ErrNotFound)
	}
	if target.Voter && voters == 1 {
		return "", fmt.Errorf("%w: %s is the cluster's only voting server", store.ErrConflict, id)
	}
	if target.Voter && heard < staying/2+1 && !force {
		return "", fmt.Errorf("%w: without %s, %d servers vote, and the leader has heard from %d of them in the last 10 s, fewer than the %d a write needs; "+
			"bring them back first, or remove it anyway with --force (force=true in the API)", store.ErrConflict, id, staying, heard, staying/2+1)
	}
	// A removed server's agent stops with its serve, and its containers
	// keep running: the replicas of its node move first, with a drain
	// (the fifth acceptance run's second finding).
	toMove, err := h.st.ReplicasToMove(ctx, id)
	if err != nil {
		return "", err
	}
	if toMove > 0 && !force {
		return "", fmt.Errorf("%w: %w", store.ErrConflict, replicasOnServerError(id, toMove))
	}
	// The row first, while this server still leads: removing the leader
	// itself ends its leadership once the change commits.
	rows, err := h.st.ListServers(ctx)
	if err != nil {
		return "", err
	}
	var row *store.Server
	for i := range rows {
		if rows[i].ID == id {
			row = &rows[i]
		}
	}
	if row != nil {
		if err := h.st.DeleteServer(ctx, id); err != nil {
			return "", err
		}
	}
	if err := n.RemoveServer(id); err != nil {
		if row != nil {
			if perr := h.st.PutServer(context.Background(), *row); perr != nil {
				h.log.Warn("putting the server's row back after a failed removal", "server", id, "err", perr)
			}
		}
		return "", err
	}
	h.log.Info("server removed from the cluster", "server", id, "forced", force, "replicas_on_node", toMove)
	return serverRemovedNext(id, toMove), nil
}

// replicasOnServerError is server remove's refusal of a server whose node
// still has replicas to move.
func replicasOnServerError(id string, n int) error {
	return fmt.Errorf("the node of server %s has %d %s without a volume. A removed server's serve stops with its agent, and its containers "+
		"keep running while the replicas are placed again on other nodes, so they would run twice. Drain the node first: "+
		"hivemind node drain %s, then hivemind server remove %s. Or remove it anyway with --force (force=true in the API)",
		id, n, plural(n, "replica", "replicas"), id, id)
}

// serverRemovedNext is what server remove says to do next.
func serverRemovedNext(id string, toMove int) string {
	if toMove > 0 {
		return fmt.Sprintf("Its node still had %d %s without a volume, which are placed again on other nodes once its lease runs out; "+
			"its containers keep running on the machine until it joins again or they are removed with docker rm. "+
			"Remove the node with hivemind node remove %s", toMove, plural(toMove, "replica", "replicas"), id)
	}
	return fmt.Sprintf("Its node is a node like any other now; remove it with hivemind node remove %s", id)
}

// apiHandler is the API as every server serves it: a follower passes each
// request to the leader, but GET /healthz, and GET /v1/servers while no
// server leads; a request's forwarding headers are dropped first.
func (h *haServe) apiHandler(local http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ha.StripForwardHeaders(r.Header)
		n := h.node.Load()
		if n == nil || n.IsLeader() || r.URL.Path == "/healthz" ||
			(r.Method == http.MethodGet && r.URL.Path == "/v1/servers" && !n.HasLeader()) ||
			(h.localReads && r.Method == http.MethodGet && r.Header.Get(testLocalReadHeader) == "1") {
			local.ServeHTTP(w, r)
			return
		}
		n.Forward(ha.KindAPI, w, r)
	})
}

// serverSource is the agent's Source on a server: the store while this
// server leads, or the cluster has one server; the node API otherwise,
// through this server's own node listener, as a worker does.
type serverSource struct {
	h *haServe
}

func (s serverSource) remote() *workerSource {
	c := s.h.remote.Load()
	if c == nil || s.h.leader() {
		return nil
	}
	return &workerSource{client: c, register: s.h.register}
}

// note records a refusal of this server's node as removed, which tells a
// server that was removed while it was down of its removal (watchRemoved):
// a server's node is removed only once the server is.
func (s serverSource) note(err error) error {
	if errors.Is(err, nodeapi.ErrNodeRemoved) {
		s.h.nodeRemoved.Store(true)
	}
	return err
}

func (s serverSource) AssignmentsForNode(ctx context.Context, id string) ([]store.Assignment, error) {
	if r := s.remote(); r != nil {
		a, err := r.AssignmentsForNode(ctx, id)
		return a, s.note(err)
	}
	return s.h.st.AssignmentsForNode(ctx, id)
}

func (s serverSource) ReportStatus(ctx context.Context, id string, st []store.Status) error {
	if r := s.remote(); r != nil {
		return s.note(r.ReportStatus(ctx, id, st))
	}
	return s.h.st.ReportStatus(ctx, id, st)
}

func (s serverSource) RenewLease(ctx context.Context, id string) error {
	if r := s.remote(); r != nil {
		return s.note(r.RenewLease(ctx, id))
	}
	return s.h.st.RenewLease(ctx, id)
}

func (s serverSource) StatusForNode(ctx context.Context, id string) ([]store.Status, error) {
	if r := s.remote(); r != nil {
		st, err := r.StatusForNode(ctx, id)
		return st, s.note(err)
	}
	return s.h.st.StatusForNode(ctx, id)
}

var (
	_ agent.Source       = serverSource{}
	_ agent.LeaseRenewer = serverSource{}
)

// routes is the router's table on a server: the store's while it leads,
// the leader's through the node API otherwise, so that a follower's router
// keeps its last table, as a worker's does, while no server leads. Each
// read looks again: the router bounds its reads while it has no table, so
// that one stuck on a server that does not answer gives way to the next,
// which reads the store once this server leads, and goes through this
// server's own listener to the leader once it knows one (ownClient).
func (h *haServe) routes(local func(context.Context) (router.Table, error)) func(context.Context) (router.Table, error) {
	var missing *nodeapi.MissingRoute
	return func(ctx context.Context) (router.Table, error) {
		c := h.remote.Load()
		if c == nil || h.leader() {
			return local(ctx)
		}
		if missing == nil {
			missing = nodeapi.NewMissingRoute(c, "routing table", h.log.With("component", "router"))
		}
		return nodeRoutes(c, h.selfID, missing)(ctx)
	}
}

// tlsBackend is the router's TLS storage on a server. While the server
// writes the store itself, as the leader or the one server, it is the
// store. On a follower, writes, deletes and locks go to the leader through
// the node API, as a worker's do, and reads go to the follower's own copy
// of the store first (docs/ha.md#the-routers-tls-storage): its router then
// loads the certificates and keys the store has while no server leads, as
// when it starts during an election. A key the copy lacks is asked of the
// leader, so that a key just stored, whose entry the follower has not
// applied yet, is not taken for missing, which could make certmagic order
// a certificate the storage has; and a challenge token is read from the
// leader only, since its copy could be the previous challenge's.
type tlsBackend struct {
	// remote is the leader's storage through the node API, or nil while
	// this server writes the store itself.
	remote func() tlsstore.Backend
	local  tlsstore.Backend
}

// remoteTLS is the leader's TLS storage for this server's router, or nil
// when this server writes the store itself.
func (h *haServe) remoteTLS() tlsstore.Backend {
	c := h.remote.Load()
	if c == nil || h.leader() {
		return nil
	}
	return c.TLS(h.selfID)
}

func (b tlsBackend) pick() tlsstore.Backend {
	if r := b.remote(); r != nil {
		return r
	}
	return b.local
}

func (b tlsBackend) Load(ctx context.Context, key string) ([]byte, error) {
	r := b.remote()
	if r == nil {
		return b.local.Load(ctx, key)
	}
	if router.IsChallengeKey(key) {
		return r.Load(ctx, key)
	}
	v, err := b.local.Load(ctx, key)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return v, err
	}
	return r.Load(ctx, key)
}
func (b tlsBackend) Store(ctx context.Context, key string, v []byte) error {
	return b.pick().Store(ctx, key, v)
}
func (b tlsBackend) Delete(ctx context.Context, key string) error { return b.pick().Delete(ctx, key) }
func (b tlsBackend) Exists(ctx context.Context, key string) (bool, error) {
	r := b.remote()
	if r == nil {
		return b.local.Exists(ctx, key)
	}
	if ok, err := b.local.Exists(ctx, key); err == nil && ok {
		return true, nil
	}
	return r.Exists(ctx, key)
}

// List asks the leader, whose list is the whole one, and on a follower
// that cannot reach a leader lists its own copy.
func (b tlsBackend) List(ctx context.Context, prefix string, recursive bool) ([]string, error) {
	r := b.remote()
	if r == nil {
		return b.local.List(ctx, prefix, recursive)
	}
	keys, err := r.List(ctx, prefix, recursive)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		if own, lerr := b.local.List(ctx, prefix, recursive); lerr == nil {
			return own, nil
		}
	}
	return keys, err
}
func (b tlsBackend) Stat(ctx context.Context, key string) (store.TLSKeyInfo, error) {
	r := b.remote()
	if r == nil {
		return b.local.Stat(ctx, key)
	}
	i, err := b.local.Stat(ctx, key)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return i, err
	}
	return r.Stat(ctx, key)
}
func (b tlsBackend) TryLock(ctx context.Context, name, owner string, ttl time.Duration) (bool, error) {
	return b.pick().TryLock(ctx, name, owner, ttl)
}
func (b tlsBackend) RenewLock(ctx context.Context, name, owner string, ttl time.Duration) error {
	return b.pick().RenewLock(ctx, name, owner, ttl)
}
func (b tlsBackend) Unlock(ctx context.Context, name, owner string) error {
	return b.pick().Unlock(ctx, name, owner)
}

// followerReporter posts this server's router counters to the leader while
// it does not lead: the leader's /metrics shows its own router's directly.
type followerReporter struct {
	h *haServe
	c *nodeapi.Client
}

func (f followerReporter) ReportRouter(ctx context.Context, id string, r nodeapi.RouterReport) error {
	if f.h.leader() {
		return nil
	}
	return f.c.ReportRouter(ctx, id, r)
}

// waitForStore waits until a joined server has the cluster's store, which
// the leader sends it as a Raft snapshot, and returns the cluster record.
func waitForStore(ctx context.Context, st *store.Store, log *slog.Logger) (store.Cluster, error) {
	logged := false
	for {
		cl, err := st.GetCluster(ctx)
		if err == nil {
			return cl, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return store.Cluster{}, err
		}
		if !logged {
			log.Info("waiting for the leader to send this server the cluster's store")
			logged = true
		}
		select {
		case <-ctx.Done():
			return store.Cluster{}, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// seedIDs are the servers a joined server takes on the server port before
// the leader has sent it the configuration.
func seedIDs(st serverState) []string {
	var out []string
	for _, s := range st.Servers {
		if s.ID != st.NodeID {
			out = append(out, s.ID)
		}
	}
	return out
}

// serverHereError is what init and join say on a machine whose data
// directory holds a joined server.
func serverHereError(dir, doing string) error {
	st, err := readServerState(dir)
	if err != nil {
		return nil
	}
	return fmt.Errorf("%s holds server %s of a cluster (%s), so %s here: run hivemind serve", dir, st.NodeID,
		filepath.Join(dir, serverStateFile), strings.TrimSpace(doing))
}

// runJoinServer is join --role server: it joins this machine as another
// server of the cluster at arg, writes what the reply holds to the data
// directory, and runs the server.
func runJoinServer(cmd *cobra.Command, cfg *config.Config, arg string) error {
	ctx := cmd.Context()
	token, err := joinToken(cmd, "hivemind join-token --role server on a server of the cluster")
	if err != nil {
		return err
	}
	if cfg.Advertise == "" {
		return errors.New("--advertise is required with --role server: the address of this machine that the other servers and the workers reach")
	}
	host, port, err := splitServer(arg)
	if err != nil {
		return err
	}
	if port == defaultClusterPort+1 {
		return fmt.Errorf("%s is the default join port, but join takes the server's cluster port, %s, or no port, and dials the join port above it",
			net.JoinHostPort(host, strconv.Itoa(port)), net.JoinHostPort(host, strconv.Itoa(defaultClusterPort)))
	}
	settings, err := joinSettings(cmd.Flags(), cfg)
	if err != nil {
		return err
	}
	log, err := newLogger(cfg, cmd.ErrOrStderr())
	if err != nil {
		return err
	}
	slog.SetDefault(log)
	dir := cfg.DataDir
	release, err := lockDataDir(dir)
	if err != nil {
		return err
	}
	if err := checkEmptyForServer(dir); err != nil {
		release()
		return err
	}
	// The address this server binds is checked before the token is used.
	if _, err := clusterAddress(ctx, cfg, interfaceAddrs, lookupName, log); err != nil {
		release()
		return err
	}
	joinAddr := net.JoinHostPort(host, strconv.Itoa(port+1))
	reply, err := nodeapi.JoinServer(ctx, joinAddr, nodeapi.JoinServerRequest{Token: token, Advertise: cfg.Advertise,
		NodePort: cfg.ClusterPort, Version: nodeapi.CleanVersion(version), SchemaOldest: store.SchemaOldest, SchemaNewest: store.SchemaVersion})
	if err != nil {
		release()
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		release()
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		release()
		return err
	}
	if _, err := pki.WriteCAFiles(dir, []byte(reply.CAPEM), []byte(reply.CAKeyPEM)); err != nil {
		release()
		return fmt.Errorf("writing the cluster CA: %w", err)
	}
	if err := writeServerState(dir, serverState{NodeID: reply.NodeID, Joined: time.Now().UTC(), Servers: reply.Servers}); err != nil {
		release()
		return err
	}
	log.Info("joined the cluster as a server; it becomes a voter once it has caught up with the log", "node", reply.NodeID,
		"servers", len(reply.Servers))
	if len(settings) > 0 {
		if err := config.WriteFile(cfg.ConfigFile, joinServerConfigHeader, settings); err != nil {
			release()
			return fmt.Errorf("this machine joined the cluster, but writing %s failed: %w; run hivemind serve with the same flags", cfg.ConfigFile, err)
		}
	}
	release()
	rt, err := docker.New(cfg.DockerHost)
	if err != nil {
		return err
	}
	return runServe(ctx, cfg, log, rt)
}

// joinServerConfigHeader is the comment line of the config file a server's
// join writes.
const joinServerConfigHeader = "Written by hivemind join --role server: the settings it was given. serve reads this file."

// checkEmptyForServer refuses a data directory that holds a cluster
// already: a server, a worker, a CA or a store with a cluster record.
func checkEmptyForServer(dir string) error {
	if err := workerHereError(dir, "it cannot join as a server"); err != nil {
		return err
	}
	if err := serverHereError(dir, "it cannot join again"); err != nil {
		return err
	}
	for _, name := range []string{auth.TokenFileName, pki.CAKeyFile, pki.CACertFile, storeFileName, filepath.Join("raft", "raft.db")} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			return fmt.Errorf("%s already holds a cluster (%s is present): join a server with an empty --data-dir", dir, name)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}
