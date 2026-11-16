package store

import (
	"context"
	"database/sql"
	"slices"
	"testing"
	"time"
)

// countInserts adds a trigger that counts the rows inserted into
// container_status, and returns a function that reads the count.
func countInserts(t *testing.T, s *Store) func() int {
	t.Helper()
	if _, err := s.db.Exec(`CREATE TABLE test_inserts (n INTEGER NOT NULL);
		INSERT INTO test_inserts VALUES (0);
		CREATE TRIGGER test_count_inserts AFTER INSERT ON container_status BEGIN UPDATE test_inserts SET n = n + 1; END`); err != nil {
		t.Fatal(err)
	}
	return func() int {
		t.Helper()
		var n int
		if err := s.db.QueryRow(`SELECT n FROM test_inserts`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
}

// The scale run: every node's report deleted and inserted all of its rows
// every 2 s, one INSERT each, and that was 30% of the server's CPU. A report
// the same as the node's last one renews the lease and stamps the rows with
// the time, and inserts nothing. A change, in any field, is written.
func TestUnchangedReportRenewsWithoutRewriting(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	s := newT(t, WithClock(clock.Now))
	ctx := ctxT(t)
	inserts := countInserts(t, s)
	mustPut(t, s, web(3))
	var rep []Status
	for i := range 3 {
		c := container("web", i, 1)
		c.ContainerID = string(rune('a' + i))
		rep = append(rep, c)
	}
	report := func(sts []Status) {
		t.Helper()
		if err := s.ReportStatus(ctx, node1, sts); err != nil {
			t.Fatal(err)
		}
		checkInvariants(t, s)
	}
	report(rep)
	if n := inserts(); n != 3 {
		t.Fatalf("%d rows inserted by the first report, want 3", n)
	}

	clock.Advance(2 * time.Second)
	report(rep)
	reversed := slices.Clone(rep)
	slices.Reverse(reversed)
	report(reversed)
	if n := inserts(); n != 3 {
		t.Fatalf("%d rows inserted after two unchanged reports, want 3", n)
	}
	n, err := s.GetNode(ctx, node1)
	if err != nil || !n.LastLease.Equal(clock.Now()) {
		t.Fatalf("lease after an unchanged report: %v, %v; want %v", n.LastLease, err, clock.Now())
	}
	sts, err := s.StatusForNode(ctx, node1)
	if err != nil || len(sts) != 3 {
		t.Fatalf("status: %+v, %v", sts, err)
	}
	for _, x := range sts {
		if !x.UpdatedAt.Equal(clock.Now()) {
			t.Fatalf("%s: updated %v after an unchanged report, want %v", x.Name, x.UpdatedAt, clock.Now())
		}
	}

	changed := slices.Clone(rep)
	changed[1].Ready, changed[1].Health = false, "unhealthy"
	report(changed)
	if n := inserts(); n != 6 {
		t.Fatalf("%d rows inserted after a change, want 6", n)
	}
	sts, _ = s.StatusForNode(ctx, node1)
	if len(sts) != 3 || sts[1].Ready || sts[1].Health != "unhealthy" {
		t.Fatalf("status after the change: %+v", sts)
	}

	// Rows that went behind the store's back are written again.
	if _, err := s.db.Exec(`DELETE FROM container_status WHERE node_id = ?`, node1); err != nil {
		t.Fatal(err)
	}
	report(changed)
	if sts, _ := s.StatusForNode(ctx, node1); len(sts) != 3 || inserts() != 9 {
		t.Fatalf("after the rows were deleted: %d rows, %d inserted; want 3 and 9", len(sts), inserts())
	}

	// After a report that failed, the stored rows are not known, so the
	// next report is written even when it is the same.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.ReportStatus(cancelled, node1, changed); err == nil {
		t.Fatal("a report with a cancelled context succeeded")
	}
	report(changed)
	if n := inserts(); n != 12 {
		t.Fatalf("%d rows inserted by the report after a failed one, want 12", n)
	}
}

// An unchanged report still deletes the replicas marked for removal that the
// node has no container for, as every report does.
func TestUnchangedReportStillPurges(t *testing.T) {
	s := newT(t)
	ctx := ctxT(t)
	mustPut(t, s, web(2))
	c := container("web", 0, 1)
	c.ContainerID = "a"
	if err := s.ReportStatus(ctx, node1, []Status{c}); err != nil {
		t.Fatal(err)
	}
	// Replica 1 is on the node, with no container yet, when it is scaled away.
	if _, err := s.Scale(ctx, "web", 1); err != nil {
		t.Fatal(err)
	}
	if reps := replicasOf(t, s, "web"); len(reps) != 2 || reps[1].Desired != Removed {
		t.Fatalf("after the scale: %+v", reps)
	}
	if err := s.ReportStatus(ctx, node1, []Status{c}); err != nil {
		t.Fatal(err)
	}
	if reps := replicasOf(t, s, "web"); len(reps) != 1 {
		t.Fatalf("after an unchanged report: %+v; want replica 1 deleted", reps)
	}
	checkInvariants(t, s)
}

// The scale run: with 20 deploys in flight, a GET /v1/status took 510 ms,
// because every read waited for the one connection behind the writes. Reads
// outside a transaction use their own read-only connections: one returns
// while a write transaction is open, sees each commit as soon as it is made,
// and cannot write.
func TestReadsDoNotWaitForAWrite(t *testing.T) {
	s := newT(t)
	ctx := ctxT(t)
	mustPut(t, s, web(1))
	hold, inTx, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- s.tx(ctx, func(*sql.Tx) error {
			close(inTx)
			<-hold
			return nil
		})
	}()
	<-inTx
	rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if apps, err := s.ListApps(rctx); err != nil || len(apps) != 1 {
		t.Fatalf("ListApps while a write transaction is open: %d apps, %v", len(apps), err)
	}
	if _, err := s.AssignmentsForNode(rctx, node1); err != nil {
		t.Fatalf("AssignmentsForNode while a write transaction is open: %v", err)
	}
	if _, err := s.ListNodes(rctx); err != nil {
		t.Fatalf("ListNodes while a write transaction is open: %v", err)
	}
	if _, err := s.ListStatus(rctx, ""); err != nil {
		t.Fatalf("ListStatus while a write transaction is open: %v", err)
	}
	close(hold)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	mustPut(t, s, web(2))
	if a, err := s.GetApp(ctx, "web"); err != nil || a.Spec.Replicas != 2 {
		t.Fatalf("a read after a commit: %+v, %v", a.Spec, err)
	}
	if reps := replicasOf(t, s, "web"); len(reps) != 2 {
		t.Fatalf("replicas after the commit: %+v", reps)
	}
	if _, err := s.rdb.ExecContext(ctx, `DELETE FROM apps`); err == nil {
		t.Fatal("a write through the read-only pool succeeded")
	}
	if apps, _ := s.ListApps(ctx); len(apps) != 1 {
		t.Fatalf("apps after the refused write: %d", len(apps))
	}
}
