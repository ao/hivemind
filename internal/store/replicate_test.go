package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/spec"
)

// loopLog is a Raft log in memory for one leader and its followers. An
// entry is applied to every follower and then to the leader, as Raft
// applies a committed entry on every server.
type loopLog struct {
	mu        sync.Mutex
	leader    *Store
	followers []*Store
	entries   [][]byte
	notLeader bool
	term      uint64
	// base is the index of the entry before the first in entries.
	base uint64
}

func (l *loopLog) CanWrite() (uint64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.notLeader {
		return 0, fmt.Errorf("%w: test", ErrNotLeader)
	}
	return l.term, nil
}

func (l *loopLog) Replicate(ctx context.Context, term uint64, batch []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.notLeader || term != l.term {
		return fmt.Errorf("%w: test", ErrNotLeader)
	}
	l.entries = append(l.entries, append([]byte{}, batch...))
	idx := l.base + uint64(len(l.entries))
	for _, f := range l.followers {
		if err := f.ApplyEntry(ctx, idx, batch); err != nil {
			return err
		}
	}
	return l.leader.ApplyEntry(ctx, idx, batch)
}

// replicatedPair opens a leader and a follower store with the same clock,
// the leader writing through a loopLog to both.
func replicatedPair(t *testing.T, clock *fakeClock) (*Store, *Store, *loopLog) {
	t.Helper()
	dir := t.TempDir()
	opts := []Option{WithMinReady(0), WithClock(clock.Now)}
	leader := openT(t, filepath.Join(dir, "a", "hivemind.db"), opts...)
	follower := openT(t, filepath.Join(dir, "b", "hivemind.db"), opts...)
	l := &loopLog{leader: leader, followers: []*Store{follower}, term: 1}
	leader.SetReplicator(l)
	follower.SetReplicator(&loopLog{notLeader: true})
	return leader, follower, l
}

// workload changes every table of the store, through the store's own
// methods, as a cluster's server would.
func workload(t *testing.T, s *Store, clock *fakeClock) {
	t.Helper()
	ctx := ctxT(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.InitCluster(ctx, Cluster{AdminTokenHash: sha256.Sum256([]byte("token")), NodeID: "node-s"}))
	for i, id := range []string{"node-s", "node-a", "node-b", "node-c"} {
		must(s.UpsertNode(ctx, Node{ID: id, Name: "m" + id, CPU: 4, Memory: 8 << 30, Address: fmt.Sprintf("10.0.0.%d", i+1),
			Version: "v2.3.0", Dials: "10.0.0.1:4484"}))
	}
	must(s.SetNodeVersion(ctx, "node-a", "v2.2.0"))
	must(s.RecordNodeCertValidity(ctx, "node-a", clock.Now(), clock.Now().Add(365*24*time.Hour)))
	if _, err := s.PutRegistry(ctx, "ghcr.io", "user", "pass word"); err != nil {
		t.Fatal(err)
	}
	web := spec.App{Name: "web", Image: "ghcr.io/x/web:1", Replicas: 3, Env: map[string]string{"A": "1", "SECRET": "s"}}
	if _, err := s.PutApp(ctx, web); err != nil {
		t.Fatal(err)
	}
	api := spec.App{Name: "api", Image: "nginx:alpine", Replicas: 2,
		Routes: []spec.Route{{Host: "www.example.com", Port: 80}, {Host: "www.example.com", Path: "/v1", Port: 80}}}
	if _, _, err := s.PutAppPlaced(ctx, api, PutOptions{Origin: "compose:shop"}); err != nil {
		t.Fatal(err)
	}
	vol := spec.App{Name: "db", Image: "postgres:16", Replicas: 1, Volumes: []spec.Volume{{Name: "data", Path: "/var/lib/postgresql/data"}}}
	if _, err := s.PutApp(ctx, vol); err != nil {
		t.Fatal(err)
	}
	report := func(ids ...string) {
		t.Helper()
		if len(ids) == 0 {
			ids = []string{"node-s", "node-a", "node-b", "node-c"}
		}
		for _, id := range ids {
			as, err := s.AssignmentsForNode(ctx, id)
			must(err)
			var st []Status
			for i, a := range as {
				x := Status{Name: fmt.Sprintf("%s-%d-%d", a.App, a.Index, a.Generation), ContainerID: fmt.Sprintf("c%s%d", id, i),
					App: a.App, Replica: a.Index, Generation: a.Generation, State: "running", Ready: true, Health: "healthy",
					RestartCount: i, NextRestart: clock.Now().Add(time.Duration(i) * time.Second)}
				if a.App == "api" {
					x.RoutePorts = []RoutePort{{Port: 80, HostPort: 30000 + i}}
				}
				if a.App == "db" {
					// Bytes that are not UTF-8, as a container's error text
					// can hold.
					x.Error = "exit \xff\xfe \x00 status"
				}
				st = append(st, x)
			}
			must(s.ReportStatus(ctx, id, st))
		}
	}
	report()
	clock.Advance(2 * time.Second)
	report() // the same report: stamps
	if _, err := s.Scale(ctx, "web", 5); err != nil {
		t.Fatal(err)
	}
	web.Image = "ghcr.io/x/web:2"
	web.Replicas = 5
	if _, err := s.PutApp(ctx, web); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		clock.Advance(3 * time.Second)
		report()
		must(s.AdvanceRollouts(ctx))
	}
	if _, err := s.SetNodeEligibility(ctx, "node-b", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DrainNode(ctx, "node-c", time.Minute); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		clock.Advance(3 * time.Second)
		report()
		if _, err := s.AdvanceDrains(ctx, time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	// TLS storage and its locks.
	must(s.TLSStore(ctx, "certificates/acme/www.example.com/www.example.com.crt", []byte("cert")))
	must(s.TLSStore(ctx, "certificates/acme/www.example.com/www.example.com.key", []byte{}))
	must(s.TLSStore(ctx, "acme/ca/users/a@example.com/a.json", nil))
	if ok, err := s.TLSTryLock(ctx, "issue_cert_www.example.com", "owner-1", time.Minute); err != nil || !ok {
		t.Fatalf("lock: %v %v", ok, err)
	}
	must(s.TLSRenewLock(ctx, "issue_cert_www.example.com", "owner-1", time.Minute))
	if ok, err := s.TLSTryLock(ctx, "other", "owner-2", time.Minute); err != nil || !ok {
		t.Fatalf("lock: %v %v", ok, err)
	}
	must(s.TLSUnlock(ctx, "other", "owner-2"))
	must(s.TLSDelete(ctx, "acme"))
	// Join tokens of both roles.
	must(s.JoinTokens().Add(sha256.Sum256([]byte("w1")), clock.Now().Add(time.Hour)))
	must(s.JoinTokens().Add(sha256.Sum256([]byte("w2")), clock.Now().Add(time.Minute)))
	must(s.ServerJoinTokens().Add(sha256.Sum256([]byte("s1")), clock.Now().Add(time.Hour)))
	must(s.JoinTokens().Consume(sha256.Sum256([]byte("w1")), clock.Now()))
	if err := s.JoinTokens().Consume(sha256.Sum256([]byte("s1")), clock.Now()); !errors.Is(err, pki.ErrInvalidToken) {
		t.Fatalf("a server token redeemed as a worker's: %v", err)
	}
	must(s.ServerJoinTokens().Consume(sha256.Sum256([]byte("s1")), clock.Now()))
	must(s.PutServer(ctx, Server{ID: "node-s", Advertise: "10.0.0.1", NodePort: 4484, RaftAddr: "10.0.0.1:4486"}))
	must(s.PutServer(ctx, Server{ID: "node-a", Advertise: "hm-a.example.internal", NodePort: 4484, RaftAddr: "hm-a.example.internal:4486"}))
	// A CA rotation's marks.
	var cur, next [32]byte
	cur[0], next[0] = 1, 2
	if _, err := s.StartCARotation(ctx, cur, next); err != nil {
		t.Fatal(err)
	}
	must(s.NoteCARotationNode(ctx, CARotationNode{NodeID: "node-a", Trusts: true}))
	// node-b goes quiet and is lost; node-c is removed.
	for i := 0; i < 8; i++ {
		clock.Advance(3 * time.Second)
		for _, id := range []string{"node-s", "node-a", "node-c"} {
			must(s.RenewLease(ctx, id))
		}
		if _, err := s.ReconcileNodes(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.RemoveNode(ctx, "node-c", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveNode(ctx, "node-a", true); !errors.Is(err, ErrIsServer) {
		t.Fatalf("removing a server's node: %v", err)
	}
	must(s.DeleteServer(ctx, "node-a"))
	must(s.DeleteApp(ctx, "db"))
	must(s.DeleteRegistry(ctx, "ghcr.io"))
	clock.Advance(2 * time.Hour)
	if _, err := s.PruneJoinTokens(ctx, clock.Now()); err != nil {
		t.Fatal(err)
	}
	report("node-s", "node-a")
}

// dumpTables returns every row of every table of the store, as text, by
// table. Rows are read in primary key order, so two stores with the same
// rows give the same dump.
func dumpTables(t *testing.T, s *Store) map[string][]string {
	t.Helper()
	ctx := ctxT(t)
	tables, err := tableNames(ctx, s.rdb, "main")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, tb := range tables {
		cols, err := columnNames(ctx, s.rdb, "main", tb)
		if err != nil {
			t.Fatal(err)
		}
		list := make([]string, len(cols))
		for i, c := range cols {
			// hex() shows a value's bytes, quote() its type: an integer, a
			// text and a blob with the same bytes differ.
			list[i] = "quote(" + quoteIdent(c) + ")"
		}
		rows, err := s.rdb.QueryContext(ctx, `SELECT `+strings.Join(list, " || '|' || ")+` FROM `+quoteIdent(tb)+` ORDER BY 1`)
		if err != nil {
			t.Fatal(err)
		}
		var lines []string
		for rows.Next() {
			var line sql.NullString
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			lines = append(lines, line.String)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		out[tb] = lines
	}
	return out
}

func compareDumps(t *testing.T, what string, a, b map[string][]string, skip ...string) {
	t.Helper()
	for tb := range a {
		if _, ok := b[tb]; !ok {
			t.Errorf("%s: table %s is missing", what, tb)
		}
	}
	for tb, rows := range b {
		if strings.Contains(strings.Join(skip, ","), tb) {
			continue
		}
		if !reflect.DeepEqual(a[tb], rows) {
			t.Errorf("%s: table %s differs:\n%s\n----\n%s", what, tb, strings.Join(a[tb], "\n"), strings.Join(rows, "\n"))
		}
	}
}

// TestReplicatedStoresAgree runs a workload that changes every table on a
// leader whose writes go through a log, and checks that a follower that
// applied the same entries has the same rows in every table; that a third
// store that applies the entries afterwards, from a fresh store, has them
// too; and that a store that ran the workload without replication has the
// same rows as the leader, but for the applied index.
func TestReplicatedStoresAgree(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	leader, follower, l := replicatedPair(t, clock)
	workload(t, leader, clock)
	checkInvariants(t, leader)
	checkInvariants(t, follower)
	ld := dumpTables(t, leader)
	if len(ld["apps"]) == 0 || len(ld["container_status"]) == 0 || len(ld["tls_storage"]) == 0 || len(ld["removed_nodes"]) == 0 ||
		len(ld["servers"]) == 0 || len(ld["rollouts"]) == 0 || len(ld["ca_rotation_nodes"]) == 0 || len(ld["app_routes"]) == 0 {
		t.Fatalf("the workload left a table empty that it should fill: %v", ld)
	}
	compareDumps(t, "follower", ld, dumpTables(t, follower))
	if len(l.entries) < 50 {
		t.Fatalf("the workload made %d entries", len(l.entries))
	}

	late := openT(t, filepath.Join(t.TempDir(), "hivemind.db"), WithMinReady(0), WithClock(clock.Now))
	for i, e := range l.entries {
		if err := late.ApplyEntry(ctxT(t), uint64(i+1), e); err != nil {
			t.Fatalf("entry %d: %v", i+1, err)
		}
	}
	// Applied again, as after a restart that replays the log: skipped.
	for i, e := range l.entries {
		if err := late.ApplyEntry(ctxT(t), uint64(i+1), e); err != nil {
			t.Fatalf("entry %d again: %v", i+1, err)
		}
	}
	compareDumps(t, "late store", ld, dumpTables(t, late))

	plainClock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	plain := openT(t, filepath.Join(t.TempDir(), "hivemind.db"), WithMinReady(0), WithClock(plainClock.Now))
	workload(t, plain, plainClock)
	compareDumps(t, "store without replication", ld, dumpTables(t, plain), "raft_applied")
}

// TestFollowerRefusesWrites: a store that is not the leader reads its
// copy, and refuses a write before it changes anything.
func TestFollowerRefusesWrites(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	leader, follower, l := replicatedPair(t, clock)
	ctx := ctxT(t)
	if err := leader.UpsertNode(ctx, Node{ID: node1, Name: "one", CPU: 4, Memory: 8 << 30}); err != nil {
		t.Fatal(err)
	}
	if _, err := leader.PutApp(ctx, web(2)); err != nil {
		t.Fatal(err)
	}
	before := dumpTables(t, follower)
	if _, err := follower.PutApp(ctx, web(3)); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("a follower's put: %v, want ErrNotLeader", err)
	}
	if err := follower.RenewLease(ctx, node1); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("a follower's lease renewal: %v, want ErrNotLeader", err)
	}
	compareDumps(t, "follower after refused writes", before, dumpTables(t, follower))
	// Reads work on the follower, and see what the leader wrote.
	if got := replicasOf(t, follower, "web"); len(got) != 2 {
		t.Fatalf("the follower reads %d replicas, want 2", len(got))
	}
	if as, err := follower.AssignmentsForNode(ctx, node1); err != nil || len(as) != 2 {
		t.Fatalf("the follower's assignments: %v %v", as, err)
	}
	// The leader loses the leadership: its writes are refused too.
	l.mu.Lock()
	l.notLeader = true
	l.mu.Unlock()
	if _, err := leader.Scale(ctx, "web", 4); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("a deposed leader's write: %v, want ErrNotLeader", err)
	}
	if got := replicasOf(t, leader, "web"); len(got) != 2 {
		t.Fatalf("the refused scale left %d replicas", len(got))
	}
}

// TestForeignEntryDropsCaches: an entry another server wrote reaches the
// denylist of removed nodes, the routing table and the report digests of
// a store that applies it.
func TestForeignEntryDropsCaches(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	a, b, la := replicatedPair(t, clock)
	ctx := ctxT(t)
	for _, id := range []string{node1, "node-2"} {
		if err := a.UpsertNode(ctx, Node{ID: id, Name: id, CPU: 4, Memory: 8 << 30, Address: "10.0.0.9"}); err != nil {
			t.Fatal(err)
		}
	}
	routed := spec.App{Name: "api", Image: "nginx:alpine", Replicas: 1, Routes: []spec.Route{{Host: "www.example.com", Port: 80}}}
	if _, err := a.PutApp(ctx, routed); err != nil {
		t.Fatal(err)
	}
	// b reads the table once, so that it is cached.
	if _, err := b.RouteTable(ctx); err != nil {
		t.Fatal(err)
	}
	as, err := a.AssignmentsForNode(ctx, node1)
	if err != nil {
		t.Fatal(err)
	}
	var st []Status
	for _, x := range as {
		st = append(st, Status{Name: "api-0", App: x.App, Replica: x.Index, Generation: x.Generation, State: "running", Ready: true,
			ContainerID: "c1", RoutePorts: []RoutePort{{Port: 80, HostPort: 30001}}})
	}
	if err := a.ReportStatus(ctx, node1, st); err != nil {
		t.Fatal(err)
	}
	rt, err := b.RouteTable(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rt) != 1 || len(rt[0].Endpoints) != 1 {
		t.Fatalf("the follower's routing table after the leader's report: %+v", rt)
	}
	if _, err := a.RemoveNode(ctx, "node-2", true); err != nil {
		t.Fatal(err)
	}
	if removed, _ := b.IsNodeRemoved(ctx, "node-2"); !removed {
		t.Fatal("the follower's denylist does not have the node the leader removed")
	}

	// The roles change: b leads, a follows. b's report digest for node1 is
	// none, so its first report rewrites the rows; a, which had one, drops
	// it once it applies b's entries.
	la.mu.Lock()
	la.notLeader = true
	la.mu.Unlock()
	lb := &loopLog{leader: b, followers: []*Store{a}, term: 2, base: uint64(len(la.entries))}
	b.SetReplicator(lb)
	b.TookLeadership()
	st[0].Ready = false
	if err := b.ReportStatus(ctx, node1, st); err != nil {
		t.Fatal(err)
	}
	compareDumps(t, "after the leader changed", dumpTables(t, b), dumpTables(t, a))
	if rt, err := a.RouteTable(ctx); err != nil || len(rt) != 1 || len(rt[0].Endpoints) != 0 {
		t.Fatalf("the old leader's routing table after the new leader's report: %+v %v", rt, err)
	}
}

// TestSnapshotLoad: a snapshot of one store loaded into another, which had
// other rows, gives it the snapshot's rows, applied index included, and
// entries after it apply on top.
func TestSnapshotLoad(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	leader, _, l := replicatedPair(t, clock)
	workload(t, leader, clock)
	snap := filepath.Join(t.TempDir(), "snap.db")
	if err := leader.SnapshotTo(ctxT(t), snap); err != nil {
		t.Fatal(err)
	}
	other := newT(t)
	mustPut(t, other, spec.App{Name: "stray", Image: "busybox", Replicas: 2})
	if err := other.LoadSnapshot(ctxT(t), snap); err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, other)
	compareDumps(t, "loaded snapshot", dumpTables(t, leader), dumpTables(t, other))
	if idx, err := other.AppliedIndex(ctxT(t)); err != nil || idx != uint64(len(l.entries)) {
		t.Fatalf("applied index after the load: %d %v, want %d", idx, err, len(l.entries))
	}
	if removed, _ := other.IsNodeRemoved(ctxT(t), "node-c"); !removed {
		t.Fatal("the denylist was not read again after the load")
	}
	l.followers = append(l.followers, other)
	if _, err := leader.Scale(ctxT(t), "web", 2); err != nil {
		t.Fatal(err)
	}
	compareDumps(t, "an entry after the snapshot", dumpTables(t, leader), dumpTables(t, other))
}

// TestApplyRefusesAnotherSchema: a batch from another schema version is
// refused, and changes nothing.
func TestApplyRefusesAnotherSchema(t *testing.T) {
	s := newT(t)
	data, err := EncodeBatch(Batch{Schema: SchemaVersion + 1, Origin: "x", Seq: 1,
		Stmts: []Statement{{SQL: `DELETE FROM nodes`}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyEntry(ctxT(t), 1, data); !errors.Is(err, ErrOtherSchema) {
		t.Fatalf("got %v, want ErrOtherSchema", err)
	}
	if n, _ := s.ListNodes(ctxT(t)); len(n) != 1 {
		t.Fatalf("the refused batch changed the nodes: %v", n)
	}
	if idx, _ := s.AppliedIndex(ctxT(t)); idx != 0 {
		t.Fatalf("applied index %d after a refused batch", idx)
	}
}

// TestBatchEncoding: every argument type comes back as it went in, bit for
// bit, and a damaged batch is refused.
func TestBatchEncoding(t *testing.T) {
	when := time.Date(2026, 11, 20, 1, 2, 3, 456789, time.FixedZone("x", 3600))
	in := Batch{Schema: 18, Origin: "abc", Seq: 7, Stmts: []Statement{
		{SQL: "INSERT INTO t VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)", Args: []any{
			nil, int64(math.MinInt64), int64(math.MaxInt64), math.Float64frombits(0x7ff8000000000001), -0.0,
			true, []byte{}, []byte{0, 0xff}, "not \xff utf-8", when}},
		{SQL: "DELETE FROM t"},
	}}
	data, err := EncodeBatch(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := DecodeBatch(data)
	if err != nil {
		t.Fatal(err)
	}
	if out.Schema != in.Schema || out.Origin != in.Origin || out.Seq != in.Seq || len(out.Stmts) != 2 {
		t.Fatalf("got %+v", out)
	}
	for i, a := range in.Stmts[0].Args {
		b := out.Stmts[0].Args[i]
		switch x := a.(type) {
		case float64:
			if math.Float64bits(x) != math.Float64bits(b.(float64)) {
				t.Errorf("arg %d: %v became %v", i, a, b)
			}
		case time.Time:
			if !x.Equal(b.(time.Time)) || x.Format(time.RFC3339Nano) != b.(time.Time).Format(time.RFC3339Nano) {
				t.Errorf("arg %d: %v became %v", i, a, b)
			}
		default:
			if !reflect.DeepEqual(a, b) {
				t.Errorf("arg %d: %#v became %#v", i, a, b)
			}
		}
	}
	if out.Stmts[1].Args != nil || out.Stmts[1].SQL != "DELETE FROM t" {
		t.Fatalf("second statement: %+v", out.Stmts[1])
	}
	for n := 0; n < len(data); n++ {
		if _, err := DecodeBatch(data[:n]); err == nil {
			t.Fatalf("a batch cut to %d of %d bytes decoded", n, len(data))
		}
	}
	if _, err := DecodeBatch(append(append([]byte{}, data...), 0)); err == nil {
		t.Fatal("a batch with a byte after its end decoded")
	}
	if _, err := EncodeBatch(Batch{Stmts: []Statement{{SQL: "x", Args: []any{1}}}}); err == nil {
		t.Fatal("an int, which database/sql would have made an int64, encoded")
	}
}

// FuzzDecodeBatch: no input makes the decoder panic, and what decodes
// encodes back to the same bytes.
func FuzzDecodeBatch(f *testing.F) {
	data, _ := EncodeBatch(Batch{Schema: 18, Origin: "o", Seq: 1, Stmts: []Statement{{SQL: "x", Args: []any{int64(1), "s", []byte("b"), nil, true, 1.5}}}})
	f.Add(data)
	f.Add([]byte("HMB1"))
	f.Fuzz(func(t *testing.T, in []byte) {
		b, err := DecodeBatch(in)
		if err != nil {
			return
		}
		again, err := EncodeBatch(b)
		if err != nil {
			t.Fatal(err)
		}
		b2, err := DecodeBatch(again)
		if err != nil || !reflect.DeepEqual(b, b2) {
			t.Fatalf("round trip changed the batch: %v", err)
		}
	})
}

// TestRecorderRefusesAWriteAsAQuery: a write run through a query while the
// connection records is refused, since it would not be replicated.
func TestRecorderRefusesAWriteAsAQuery(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	leader, _, _ := replicatedPair(t, clock)
	err := leader.tx(ctxT(t), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctxT(t), `DELETE FROM nodes RETURNING id`)
		if err != nil {
			return err
		}
		return rows.Close()
	})
	if err == nil || !strings.Contains(err.Error(), "would not be replicated") {
		t.Fatalf("got %v", err)
	}
	// A read is fine.
	err = leader.tx(ctxT(t), func(tx *sql.Tx) error {
		var n int
		return tx.QueryRowContext(ctxT(t), `SELECT count(*) FROM nodes`).Scan(&n)
	})
	if err != nil {
		t.Fatal(err)
	}
}
