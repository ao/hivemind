package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The schema versions of a store in a cluster of several servers, and the
// step that moves them through the log (docs/ha.md#rolling-upgrades).

// columnsOf lists the columns of table in the store's main database.
func columnsOf(t *testing.T, s *Store, table string) []string {
	t.Helper()
	cols, err := columnNames(ctxT(t), s.rdb, "main", table)
	if err != nil {
		t.Fatal(err)
	}
	return cols
}

// fileSchema reads the schema version of the store file at path.
func fileSchema(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestHeldSchemaOpen: a store of a cluster of several keeps its version
// when it opens; a new one is made at the build's oldest version; one
// below the build's oldest is refused, as two releases apart, and one
// above its newest as newer. A cluster of one migrates as before.
func TestHeldSchemaOpen(t *testing.T) {
	ctx := ctxT(t)
	path := filepath.Join(t.TempDir(), "hivemind.db")
	// A store of 2.3.0: schema version 18.
	s, err := Open(ctx, path, WithSchemaRange(HASchemaVersion, HASchemaVersion))
	if err != nil {
		t.Fatal(err)
	}
	if s.Schema() != HASchemaVersion {
		t.Fatalf("a 2.3.0 build made its store at %d", s.Schema())
	}
	s.Close()

	s, err = Open(ctx, path, WithHeldSchema())
	if err != nil {
		t.Fatal(err)
	}
	if s.Schema() != HASchemaVersion || fileSchema(t, path) != HASchemaVersion {
		t.Fatalf("held: the store is at %d, the file at %d; want both at %d", s.Schema(), fileSchema(t, path), HASchemaVersion)
	}
	if oldest, newest := s.SchemaRange(); oldest != SchemaOldest || newest != SchemaVersion {
		t.Fatalf("SchemaRange %d-%d, want %d-%d", oldest, newest, SchemaOldest, SchemaVersion)
	}
	if err := s.CheckInvariants(ctx); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// A build that runs 19 and later: two releases apart.
	if _, err := Open(ctx, path, WithHeldSchema(), WithSchemaRange(HASchemaVersion+1, HASchemaVersion+1)); !errors.Is(err, ErrOlderSchema) ||
		!strings.Contains(err.Error(), "release between") {
		t.Fatalf("a store two releases older: %v, want ErrOlderSchema", err)
	}
	if v := fileSchema(t, path); v != HASchemaVersion {
		t.Fatalf("the refused open moved the file to %d", v)
	}

	// A cluster of one migrates at open.
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Schema() != SchemaVersion {
		t.Fatalf("a cluster of one opened at %d, want %d", s.Schema(), SchemaVersion)
	}
	s.Close()

	// A 2.3.0 build refuses it now.
	if _, err := Open(ctx, path, WithHeldSchema(), WithSchemaRange(HASchemaVersion, HASchemaVersion)); !errors.Is(err, ErrNewerSchema) {
		t.Fatalf("a 2.3.0 build on a store at %d: %v, want ErrNewerSchema", SchemaVersion, err)
	}

	// A new store of a server that joins starts at the oldest version.
	fresh := openT(t, filepath.Join(t.TempDir(), "hivemind.db"), WithHeldSchema())
	if fresh.Schema() != SchemaOldest {
		t.Fatalf("a joining server's new store is at %d, want %d", fresh.Schema(), SchemaOldest)
	}
	checkInvariants(t, fresh)
}

// replicatedPairHeld is replicatedPair with the stores of a cluster of
// several, new, so at the build's oldest version; extra options apply to
// the follower.
func replicatedPairHeld(t *testing.T, clock *fakeClock, follower ...Option) (*Store, *Store, *loopLog) {
	t.Helper()
	dir := t.TempDir()
	opts := []Option{WithMinReady(0), WithClock(clock.Now), WithHeldSchema()}
	leader := openT(t, filepath.Join(dir, "a", "hivemind.db"), opts...)
	f := openT(t, filepath.Join(dir, "b", "hivemind.db"), append(opts, follower...)...)
	l := &loopLog{leader: leader, followers: []*Store{f}, term: 1}
	leader.SetReplicator(l)
	f.SetReplicator(&loopLog{notLeader: true})
	return leader, f, l
}

// TestSchemaStepThroughTheLog: a leader at the old version records its
// batches at it; the step moves the leader and the follower to the next
// version in the same transaction as the applied index; the batches after
// it are recorded at the new version, and both stores have the same rows.
func TestSchemaStepThroughTheLog(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	leader, follower, l := replicatedPairHeld(t, clock)
	ctx := ctxT(t)
	if leader.Schema() != HASchemaVersion || follower.Schema() != HASchemaVersion {
		t.Fatalf("the stores start at %d and %d", leader.Schema(), follower.Schema())
	}
	workload(t, leader, clock)
	for i, e := range l.entries {
		b, err := DecodeBatch(e)
		if err != nil {
			t.Fatalf("entry %d: %v", i+1, err)
		}
		if b.Schema != HASchemaVersion {
			t.Fatalf("entry %d was recorded at %d, the build's newest, not the store's %d", i+1, b.Schema, HASchemaVersion)
		}
	}
	compareDumps(t, "follower at 18", dumpTables(t, leader), dumpTables(t, follower))

	before := len(l.entries)
	v, err := leader.StepSchema(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v != CAStoreSchemaVersion || leader.Schema() != CAStoreSchemaVersion || follower.Schema() != CAStoreSchemaVersion {
		t.Fatalf("after the step: StepSchema %d, leader %d, follower %d; want %d", v, leader.Schema(), follower.Schema(), CAStoreSchemaVersion)
	}
	if len(l.entries) != before+1 {
		t.Fatalf("the step appended %d entries", len(l.entries)-before)
	}
	st, err := DecodeStep(l.entries[before])
	if err != nil || st.From != HASchemaVersion || st.To != CAStoreSchemaVersion {
		t.Fatalf("the step's entry: %+v %v", st, err)
	}
	for _, s := range []*Store{leader, follower} {
		if idx, _ := s.AppliedIndex(ctx); idx != uint64(len(l.entries)) {
			t.Fatalf("applied index %d after the step, want %d", idx, len(l.entries))
		}
		cols := strings.Join(columnsOf(t, s, "ca_rotation"), ",")
		if !strings.Contains(cols, "next_cert") || !strings.Contains(strings.Join(columnsOf(t, s, "cluster"), ","), "ca_fp") {
			t.Fatalf("the step did not run migration %d: ca_rotation has %s", CAStoreSchemaVersion, cols)
		}
		checkInvariants(t, s)
	}
	// At the newest version the step does nothing.
	if v, err := leader.StepSchema(ctx); err != nil || v != SchemaVersion || len(l.entries) != before+1 {
		t.Fatalf("a step at the newest version: %d %v, %d entries", v, err, len(l.entries)-before)
	}

	if _, err := leader.Scale(ctx, "web", 3); err != nil {
		t.Fatal(err)
	}
	b, err := DecodeBatch(l.entries[len(l.entries)-1])
	if err != nil || b.Schema != CAStoreSchemaVersion {
		t.Fatalf("a batch after the step: schema %d %v, want %d", b.Schema, err, CAStoreSchemaVersion)
	}
	compareDumps(t, "follower at 19", dumpTables(t, leader), dumpTables(t, follower))

	// A store that applies the whole log afterwards, from new, gets the
	// same rows: the batches at 18, the step, the batches at 19. Applied
	// again, as a restart replays the log, every entry is skipped by its
	// index, those from before the step too.
	late := openT(t, filepath.Join(t.TempDir(), "hivemind.db"), WithMinReady(0), WithClock(clock.Now), WithHeldSchema())
	for round := 0; round < 2; round++ {
		for i, e := range l.entries {
			if err := late.ApplyEntry(ctx, uint64(i+1), e); err != nil {
				t.Fatalf("round %d, entry %d: %v", round, i+1, err)
			}
		}
	}
	compareDumps(t, "late store", dumpTables(t, leader), dumpTables(t, late))
}

// TestSchemaStepRefused: a build that does not know the next version
// refuses the step and changes nothing, and a batch recorded at another
// version than the store's is refused; so is a format the build does not
// know.
func TestSchemaStepRefused(t *testing.T) {
	ctx := ctxT(t)
	old := openT(t, filepath.Join(t.TempDir(), "hivemind.db"), WithHeldSchema(), WithSchemaRange(HASchemaVersion, HASchemaVersion))
	step := EncodeStep(Step{From: HASchemaVersion, To: HASchemaVersion + 1, Origin: "x", Seq: 1})
	if err := old.ApplyEntry(ctx, 1, step); !errors.Is(err, ErrOtherSchema) || !strings.Contains(err.Error(), "upgrade this server") {
		t.Fatalf("a 2.3.0 build given the step: %v, want ErrOtherSchema", err)
	}
	if old.Schema() != HASchemaVersion {
		t.Fatalf("the refused step moved the store to %d", old.Schema())
	}
	if idx, _ := old.AppliedIndex(ctx); idx != 0 {
		t.Fatalf("applied index %d after a refused step", idx)
	}

	s := openT(t, filepath.Join(t.TempDir(), "hivemind.db"), WithHeldSchema())
	// A step from a version the store is not at.
	if err := s.ApplyEntry(ctx, 1, EncodeStep(Step{From: HASchemaVersion - 1, To: HASchemaVersion, Origin: "x", Seq: 1})); !errors.Is(err, ErrOtherSchema) {
		t.Fatalf("a step from another version: %v", err)
	}
	// A batch at the build's newest version, when the store is at the
	// oldest.
	data, err := EncodeBatch(Batch{Schema: SchemaVersion, Origin: "x", Seq: 1, Stmts: []Statement{{SQL: `DELETE FROM nodes`}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyEntry(ctx, 1, data); !errors.Is(err, ErrOtherSchema) {
		t.Fatalf("a batch at %d on a store at %d: %v", SchemaVersion, s.Schema(), err)
	}
	// A format this build does not know.
	unknown := append([]byte("HMB3"), 0)
	if err := s.ApplyEntry(ctx, 1, unknown); !errors.Is(err, ErrOtherSchema) || !strings.Contains(err.Error(), "format 3") {
		t.Fatalf("an entry of format 3: %v", err)
	}
	if _, err := DecodeBatch(step); err == nil {
		t.Fatal("DecodeBatch took a step, as 2.3.0 must not")
	}
	if err := s.ApplyEntry(ctx, 1, []byte("XYZ1")); err == nil {
		t.Fatal("an entry without the header was applied")
	}
}

// TestSnapshotOfANewerSchema: a store at the old version loads a snapshot
// at the next one after migrating itself to it; a snapshot older than the
// store, or newer than the build knows, is refused.
func TestSnapshotOfANewerSchema(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	leader, _, _ := replicatedPairHeld(t, clock)
	ctx := ctxT(t)
	workload(t, leader, clock)
	if _, err := leader.StepSchema(ctx); err != nil {
		t.Fatal(err)
	}
	snap19 := filepath.Join(t.TempDir(), "snap.db")
	if err := leader.SnapshotTo(ctx, snap19); err != nil {
		t.Fatal(err)
	}

	behind := openT(t, filepath.Join(t.TempDir(), "hivemind.db"), WithMinReady(0), WithHeldSchema())
	if behind.Schema() != HASchemaVersion {
		t.Fatalf("the store starts at %d", behind.Schema())
	}
	if err := behind.LoadSnapshot(ctx, snap19); err != nil {
		t.Fatal(err)
	}
	if behind.Schema() != CAStoreSchemaVersion {
		t.Fatalf("after the snapshot the store is at %d, want %d", behind.Schema(), CAStoreSchemaVersion)
	}
	compareDumps(t, "loaded snapshot at 19", dumpTables(t, leader), dumpTables(t, behind))
	checkInvariants(t, behind)

	// A 2.3.0 build refuses the snapshot at 19.
	old := openT(t, filepath.Join(t.TempDir(), "hivemind.db"), WithHeldSchema(), WithSchemaRange(HASchemaVersion, HASchemaVersion))
	if err := old.LoadSnapshot(ctx, snap19); !errors.Is(err, ErrOtherSchema) {
		t.Fatalf("a 2.3.0 build given a snapshot at 19: %v", err)
	}

	// A snapshot at 18 into a store at 19.
	at18 := openT(t, filepath.Join(t.TempDir(), "hivemind.db"), WithHeldSchema())
	snap18 := filepath.Join(t.TempDir(), "snap.db")
	if err := at18.SnapshotTo(ctx, snap18); err != nil {
		t.Fatal(err)
	}
	if err := behind.LoadSnapshot(ctx, snap18); !errors.Is(err, ErrOtherSchema) {
		t.Fatalf("a snapshot older than the store: %v", err)
	}
}

// TestStepEncoding: a step comes back as it went in, and a damaged one is
// refused.
func TestStepEncoding(t *testing.T) {
	in := Step{From: 18, To: 19, Origin: "abc", Seq: 7}
	out, err := DecodeStep(EncodeStep(in))
	if err != nil || out != in {
		t.Fatalf("round trip: %+v %v, want %+v", out, err, in)
	}
	data := EncodeStep(in)
	for i := len(stepMagic); i < len(data); i++ {
		if _, err := DecodeStep(data[:i]); err == nil {
			t.Fatalf("a step cut at %d of %d bytes decoded", i, len(data))
		}
	}
	if _, err := DecodeStep(append(EncodeStep(in), 0)); err == nil {
		t.Fatal("a step with a byte after its end decoded")
	}
	if _, err := DecodeStep(EncodeStep(Step{From: 18, To: 20})); err == nil {
		t.Fatal("a step of two versions decoded")
	}
	if f, err := entryFormat(EncodeStep(in)); err != nil || f != formatStep {
		t.Fatalf("entryFormat of a step: %d %v", f, err)
	}
}
