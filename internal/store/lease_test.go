package store

import (
	"errors"
	"testing"
	"time"
)

// The leases and report stamps the leader of a cluster of several keeps in
// memory, and writes in one entry (docs/ha.md#fewer-raft-entries).

// storedLease reads node id's lease as s stores it, without what a leader
// keeps.
func storedLease(t *testing.T, s *Store, id string) time.Time {
	t.Helper()
	nodes, err := listNodes(ctxT(t), s.rdb)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.ID == id {
			return n.LastLease
		}
	}
	t.Fatalf("no node %s", id)
	return time.Time{}
}

// storedStamps reads the stamps of node id's container rows as s stores
// them.
func storedStamps(t *testing.T, s *Store, id string) []time.Time {
	t.Helper()
	st, err := queryStatus(ctxT(t), s.rdb, `node_id = ? ORDER BY name`, id)
	if err != nil {
		t.Fatal(err)
	}
	var out []time.Time
	for _, x := range st {
		out = append(out, x.UpdatedAt)
	}
	return out
}

// TestRenewalsKeptAndWrittenTogether: on the leader, a renewal and an
// unchanged report from a node whose stored lease is recent append no
// entry; the leader's own views show them; WriteKept writes the leases and
// stamps of every node in one entry, which every server applies.
func TestRenewalsKeptAndWrittenTogether(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	leader, follower, l := replicatedPair(t, clock)
	ctx := ctxT(t)
	for _, id := range []string{node1, "node-2"} {
		if err := leader.UpsertNode(ctx, Node{ID: id, Name: id, CPU: 4, Memory: 8 << 30}); err != nil {
			t.Fatal(err)
		}
	}
	registered := clock.Now()
	st := []Status{{Name: "web-0", ContainerID: "c0", App: "web", Replica: 0, Generation: 1, State: "running", Ready: true}}
	if err := leader.ReportStatus(ctx, node1, st); err != nil {
		t.Fatal(err)
	}
	firstReport := clock.Now()
	entries := len(l.entries)

	clock.Advance(2 * time.Second)
	renewed := clock.Now()
	if err := leader.RenewLease(ctx, "node-2"); err != nil {
		t.Fatal(err)
	}
	if err := leader.ReportStatus(ctx, node1, st); err != nil {
		t.Fatal(err)
	}
	if len(l.entries) != entries {
		t.Fatalf("a renewal and an unchanged report appended %d entries, want none", len(l.entries)-entries)
	}
	if leader.Kept() != 2 {
		t.Fatalf("the leader keeps %d leases, want 2", leader.Kept())
	}
	if got := storedLease(t, follower, "node-2"); !got.Equal(registered) {
		t.Fatalf("the follower's stored lease of node-2 moved before the write: %v", got)
	}
	n2, err := leader.GetNode(ctx, "node-2")
	if err != nil || !n2.LastLease.Equal(renewed) {
		t.Fatalf("the leader's view of node-2's lease: %v %v, want %v", n2.LastLease, err, renewed)
	}
	if ls, err := leader.StatusForNode(ctx, node1); err != nil || len(ls) != 1 || !ls[0].UpdatedAt.Equal(renewed) {
		t.Fatalf("the leader's view of node-1's rows: %+v %v", ls, err)
	}

	n, err := leader.WriteKept(ctx)
	if err != nil || n != 2 {
		t.Fatalf("WriteKept: %d %v", n, err)
	}
	if len(l.entries) != entries+1 {
		t.Fatalf("WriteKept appended %d entries, want 1", len(l.entries)-entries)
	}
	for _, s := range []*Store{leader, follower} {
		for _, id := range []string{node1, "node-2"} {
			if got := storedLease(t, s, id); !got.Equal(renewed) {
				t.Fatalf("stored lease of %s after the write: %v, want %v", id, got, renewed)
			}
		}
		if got := storedStamps(t, s, node1); len(got) != 1 || !got[0].Equal(renewed) {
			t.Fatalf("stored stamps of node-1 after the write: %v, want %v (first report %v)", got, renewed, firstReport)
		}
	}
	if leader.Kept() != 0 {
		t.Fatal("the leader still keeps leases after writing them")
	}
	if n, err := leader.WriteKept(ctx); err != nil || n != 0 || len(l.entries) != entries+1 {
		t.Fatalf("WriteKept with nothing kept: %d %v, %d entries", n, err, len(l.entries)-entries)
	}
	compareDumps(t, "after the write", dumpTables(t, leader), dumpTables(t, follower))
}

// TestRenewalsWrittenAtOnce: a renewal of a node whose stored lease is
// older than KeepWithin, or has lapsed, and a report that changes
// anything, are written at once; a follower keeps nothing; a node that is
// not registered is refused as before.
func TestRenewalsWrittenAtOnce(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	leader, follower, l := replicatedPair(t, clock)
	ctx := ctxT(t)
	if err := leader.UpsertNode(ctx, Node{ID: node1, Name: "one", CPU: 4, Memory: 8 << 30}); err != nil {
		t.Fatal(err)
	}
	entries := len(l.entries)
	clock.Advance(KeepWithin + time.Second)
	if err := leader.RenewLease(ctx, node1); err != nil {
		t.Fatal(err)
	}
	if len(l.entries) != entries+1 || leader.Kept() != 0 {
		t.Fatalf("a renewal of a lease %s old: %d entries, %d kept", KeepWithin+time.Second, len(l.entries)-entries, leader.Kept())
	}
	if got := storedLease(t, follower, node1); !got.Equal(clock.Now()) {
		t.Fatalf("the follower's stored lease: %v, want %v", got, clock.Now())
	}

	// A report that changes something is written.
	entries = len(l.entries)
	clock.Advance(time.Second)
	if err := leader.ReportStatus(ctx, node1, []Status{{Name: "web-0", ContainerID: "c0", App: "web", State: "running"}}); err != nil {
		t.Fatal(err)
	}
	if len(l.entries) != entries+1 {
		t.Fatalf("a report that changed the rows appended %d entries", len(l.entries)-entries)
	}

	// A lapsed lease comes back at once, and the node's readiness starts
	// again.
	clock.Advance(20 * time.Second)
	entries = len(l.entries)
	if err := leader.RenewLease(ctx, node1); err != nil {
		t.Fatal(err)
	}
	if len(l.entries) != entries+1 {
		t.Fatal("a lapsed lease was not written at once")
	}
	if n, _ := follower.GetNode(ctx, node1); !n.ReadySince.Equal(clock.Now()) {
		t.Fatalf("ready_since after the lapse: %v, want %v", n.ReadySince, clock.Now())
	}

	// A follower writes nothing and keeps nothing.
	clock.Advance(time.Second)
	if err := follower.RenewLease(ctx, node1); !errors.Is(err, ErrNotLeader) || follower.Kept() != 0 {
		t.Fatalf("a follower's renewal: %v, %d kept", err, follower.Kept())
	}
	if err := leader.RenewLease(ctx, "node-x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a renewal of a node that is not registered: %v", err)
	}

	// The leadership changes: what the old leader kept is dropped, and the
	// next leader starts from the stored leases.
	if err := leader.RenewLease(ctx, node1); err != nil || leader.Kept() != 1 {
		t.Fatalf("a kept renewal: %v, %d kept", err, leader.Kept())
	}
	leader.TookLeadership()
	if leader.Kept() != 0 {
		t.Fatal("the leases kept survived a change of leadership")
	}
}

// TestNodeThatStopsIsLostOnTime: the write after a node's last renewal
// stores that renewal, so the stored lease of a node that stops is its
// last renewal, from which the lease's 15 s count, as before.
func TestNodeThatStopsIsLostOnTime(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	leader, follower, _ := replicatedPair(t, clock)
	ctx := ctxT(t)
	if err := leader.UpsertNode(ctx, Node{ID: node1, Name: "one", CPU: 4, Memory: 8 << 30}); err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Second)
	last := clock.Now()
	if err := leader.RenewLease(ctx, node1); err != nil {
		t.Fatal(err)
	}
	clock.Advance(WriteKeptEvery)
	if _, err := leader.WriteKept(ctx); err != nil {
		t.Fatal(err)
	}
	for _, s := range []*Store{leader, follower} {
		if got := storedLease(t, s, node1); !got.Equal(last) {
			t.Fatalf("the stored lease of a node that stopped: %v, want its last renewal %v", got, last)
		}
	}
}

// TestClusterOfOneKeepsNothing: a store that does not replicate writes
// every renewal.
func TestClusterOfOneKeepsNothing(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	s := newT(t, WithClock(clock.Now))
	clock.Advance(2 * time.Second)
	if err := s.RenewLease(ctxT(t), node1); err != nil {
		t.Fatal(err)
	}
	if s.Kept() != 0 || !storedLease(t, s, node1).Equal(clock.Now()) {
		t.Fatalf("a cluster of one kept %d leases; stored %v", s.Kept(), storedLease(t, s, node1))
	}
}
