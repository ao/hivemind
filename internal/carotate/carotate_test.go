package carotate

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/store"
)

const serverNode = "node-server"

type clock struct{ off atomic.Int64 }

func (c *clock) now() time.Time          { return time.Now().Add(time.Duration(c.off.Load())) }
func (c *clock) advance(d time.Duration) { c.off.Add(int64(d)) }

type fixture struct {
	t     *testing.T
	dir   string
	st    *store.Store
	clock *clock
	m     *Manager
	first [32]byte // the CA init made
	logs  *logBuf  // what every Manager opened on the fixture logged
}

func newFixture(t *testing.T, nodes ...string) *fixture {
	t.Helper()
	dir := t.TempDir()
	ca, err := pki.CreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{}
	st, err := store.Open(context.Background(), filepath.Join(dir, "hivemind.db"), store.WithClock(c.now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fx := &fixture{t: t, dir: dir, st: st, clock: c, first: ca.Fingerprint(), logs: &logBuf{}}
	for _, id := range append([]string{serverNode}, nodes...) {
		if err := st.UpsertNode(context.Background(), store.Node{ID: id, Name: "host-" + id, CPU: 1, Memory: 1 << 30}); err != nil {
			t.Fatal(err)
		}
	}
	fx.m = fx.open()
	return fx
}

// open opens a Manager on the fixture's directory and store, as serve does
// at each start.
func (fx *fixture) open() *Manager {
	fx.t.Helper()
	m, err := Open(context.Background(), Options{Dir: fx.dir, Store: fx.st, LocalNode: serverNode,
		Logger: slog.New(slog.NewTextHandler(fx.logs, nil)), Now: fx.clock.now})
	if err != nil {
		fx.t.Fatalf("Open: %v", err)
	}
	return m
}

// restart opens a new Manager, as a serve that stopped and started again.
func (fx *fixture) restart() { fx.m = fx.open() }

func (fx *fixture) status() Status {
	fx.t.Helper()
	s, err := fx.m.Status(context.Background())
	if err != nil {
		fx.t.Fatal(err)
	}
	return s
}

func (fx *fixture) cas() (cur, next [32]byte) {
	s := fx.m.cur.Load()
	cur = s.current.Fingerprint()
	if s.next != nil {
		next = s.next.Fingerprint()
	}
	return cur, next
}

// trust has node say it trusts fps, with a certificate from root.
func (fx *fixture) trust(node string, root [32]byte, fps ...[32]byte) error {
	return fx.m.Observe(context.Background(), node, fps, true, root)
}

func exists(t *testing.T, p string) bool {
	t.Helper()
	_, err := os.Lstat(p)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

func waitingIDs(w []Waiting) string {
	ids := make([]string, len(w))
	for i, x := range w {
		ids[i] = x.Node
	}
	return strings.Join(ids, ",")
}

func (fx *fixture) mustStart() (cur, next [32]byte) {
	fx.t.Helper()
	if _, err := fx.m.Start(context.Background()); err != nil {
		fx.t.Fatal(err)
	}
	return fx.cas()
}

// The whole rotation: start, renew, the server's certificate, finish; and
// what each side trusts and is issued in each phase.
func TestRotationStartToFinish(t *testing.T) {
	fx := newFixture(t, "node-a", "node-b")
	ctx := context.Background()
	old := fx.first
	if b, _ := fx.m.Bundle(); len(b.CAs) != 1 || b.Phase != "" {
		t.Fatalf("bundle before a rotation: %+v", b)
	}
	if _, err := fx.m.Renew(ctx, false); !errors.Is(err, ErrNoRotation) {
		t.Fatalf("renew with no rotation: %v", err)
	}

	cur, next := fx.mustStart()
	if cur != old || next == ([32]byte{}) {
		t.Fatal("start did not keep the old CA and make a new one")
	}
	if !exists(t, filepath.Join(fx.dir, pki.NextCAKeyFile)) || !exists(t, filepath.Join(fx.dir, pki.NextCACertFile)) {
		t.Fatal("no ca-next files after start")
	}
	if _, err := fx.m.Start(ctx); !errors.Is(err, ErrInProgress) {
		t.Fatalf("second start: %v", err)
	}
	// Phase trust: both accepted, the listener and every issuance from the
	// old CA, a bundle with the new CA cross-signed.
	if !fx.m.Accepts(old) || !fx.m.Accepts(next) {
		t.Fatal("phase trust does not accept both CAs")
	}
	if fx.m.Listener().Fingerprint() != old {
		t.Fatal("phase trust: the server's certificate is not from the old CA")
	}
	if got := fx.m.RenewalCA(old, [][32]byte{old, next}); got.Fingerprint() != old {
		t.Fatal("phase trust: a renewal from the new CA")
	}
	b, digest := fx.m.Bundle()
	parsed, err := b.Parse()
	if err != nil || b.Phase != nodeapi.CAPhaseTrust || b.Renew != "" || len(parsed) != 2 || parsed[1].Cross == nil {
		t.Fatalf("bundle in phase trust: %+v %v", b, err)
	}
	if _, err := pki.VerifyCross(parsed[1].Cert, parsed[1].Cross, pki.MustTrust(parsed[0].Cert), time.Now()); err != nil {
		t.Fatalf("the new CA's cross-certificate: %v", err)
	}
	if fx.m.TokenCA(old) == nil || fx.m.TokenCA(next) == nil || fx.m.TokenCA([32]byte{7}) != nil {
		t.Fatal("TokenCA in phase trust")
	}

	// renew waits for the live nodes to trust the new CA.
	_, err = fx.m.Renew(ctx, false)
	var refused *RefusedError
	if !errors.As(err, &refused) || waitingIDs(refused.Waiting) != "node-a,node-b" {
		t.Fatalf("renew before the nodes trust: %v", err)
	}
	if !strings.Contains(err.Error(), "has not made a request") || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("renew's refusal: %v", err)
	}
	if err := fx.trust("node-a", old, old, next); err != nil {
		t.Fatal(err)
	}
	if err := fx.trust("node-b", old, old); err != nil {
		t.Fatal(err)
	}
	_, err = fx.m.Renew(ctx, false)
	if !errors.As(err, &refused) || waitingIDs(refused.Waiting) != "node-b" || !strings.Contains(err.Error(), "does not trust the new CA yet") {
		t.Fatalf("renew with node-b not trusting: %v", err)
	}
	if err := fx.trust("node-b", old, old, next); err != nil {
		t.Fatal(err)
	}
	st, err := fx.m.Renew(ctx, false)
	if err != nil || st.Rotation.Phase != store.CAPhaseRenew || st.Rotation.NextStep != "finish" {
		t.Fatalf("renew: %+v %v", st.Rotation, err)
	}
	b2, digest2 := fx.m.Bundle()
	if digest2 == digest || b2.Renew != pki.FingerprintHex(next) {
		t.Fatalf("bundle in phase renew: %+v", b2)
	}
	// Renewals move to the new CA for a node that trusts it, and never back.
	if got := fx.m.RenewalCA(old, [][32]byte{old, next}); got.Fingerprint() != next {
		t.Fatal("phase renew: a node that trusts the new CA renews under the old")
	}
	if got := fx.m.RenewalCA(old, nil); got.Fingerprint() != old {
		t.Fatal("phase renew: a node that does not trust the new CA renews under it")
	}
	if got := fx.m.RenewalCA(next, nil); got.Fingerprint() != next {
		t.Fatal("phase renew: a certificate from the new CA renews under the old")
	}

	// The listener moves once every node trusts the new CA: it does.
	if err := fx.m.moveListener(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fx.m.ListenerChanged():
	default:
		t.Fatal("no listener change signalled")
	}
	if fx.m.Listener().Fingerprint() != next || !fx.status().Rotation.ListenerMoved {
		t.Fatal("the server's certificate did not move to the new CA")
	}

	// finish waits for every node to renew.
	_, err = fx.m.Finish(ctx, false)
	if !errors.As(err, &refused) || waitingIDs(refused.Waiting) != "node-a,node-b" || !strings.Contains(err.Error(), "has not renewed") {
		t.Fatalf("finish before renewals: %v", err)
	}
	if err := fx.trust("node-a", next, old, next); err != nil {
		t.Fatal(err)
	}
	// Renewed under the new CA, node-a's old certificate is refused.
	if err := fx.trust("node-a", old, old, next); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("node-a with its old certificate after renewing: %v, want ErrSuperseded", err)
	}
	if err := fx.trust("node-b", next, old, next); err != nil {
		t.Fatal(err)
	}
	st, err = fx.m.Finish(ctx, false)
	if err != nil || st.Rotation != nil || st.Next != nil {
		t.Fatalf("finish: %+v %v", st, err)
	}
	if c, n := fx.cas(); c != next || n != ([32]byte{}) {
		t.Fatal("after finish, ca.crt is not the new CA")
	}
	if exists(t, filepath.Join(fx.dir, pki.NextCAKeyFile)) || exists(t, filepath.Join(fx.dir, pki.NextCACertFile)) {
		t.Fatal("ca-next files left after finish")
	}
	if ca, err := pki.LoadCA(fx.dir); err != nil || ca.Fingerprint() != next {
		t.Fatalf("ca.crt and ca.key after finish: %v", err)
	}
	if fx.m.Accepts(old) || !fx.m.Accepts(next) || fx.m.TokenCA(old) != nil {
		t.Fatal("after finish the old CA is still accepted")
	}
	if b, _ := fx.m.Bundle(); len(b.CAs) != 1 || b.Phase != "" {
		t.Fatalf("bundle after finish: %+v", b)
	}
	if _, err := fx.st.GetCARotation(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the store still has the rotation: %v", err)
	}
	// After a restart, the cluster has the new CA alone.
	fx.restart()
	if c, _ := fx.cas(); c != next || fx.m.Listener().Fingerprint() != next {
		t.Fatal("after a restart, not the new CA")
	}
}

// A restart in each phase comes back in the same phase, with what it had
// seen of the nodes.
func TestRestartInEachPhase(t *testing.T) {
	fx := newFixture(t, "node-a")
	ctx := context.Background()
	old, next := fx.mustStart()
	fx.restart()
	if st := fx.status(); st.Rotation == nil || st.Rotation.Phase != store.CAPhaseTrust {
		t.Fatalf("after a restart in phase trust: %+v", st.Rotation)
	}
	if _, n := fx.cas(); n != next {
		t.Fatal("the new CA changed across a restart")
	}
	if err := fx.trust("node-a", old, old, next); err != nil {
		t.Fatal(err)
	}
	fx.restart()
	if st := fx.status(); !st.Nodes[0].Trusts {
		t.Fatal("what the server saw of node-a was lost across a restart")
	}
	if _, err := fx.m.Renew(ctx, false); err != nil {
		t.Fatal(err)
	}
	fx.restart()
	if st := fx.status(); st.Rotation.Phase != store.CAPhaseRenew || st.Rotation.ListenerMoved {
		t.Fatalf("after a restart in phase renew: %+v", st.Rotation)
	}
	if fx.m.Listener().Fingerprint() != old {
		t.Fatal("the listener moved across a restart by itself")
	}
	if err := fx.m.moveListener(ctx); err != nil {
		t.Fatal(err)
	}
	fx.restart()
	if fx.m.Listener().Fingerprint() != next {
		t.Fatal("the moved listener went back across a restart")
	}
	if err := fx.trust("node-a", next, old, next); err != nil {
		t.Fatal(err)
	}
	fx.restart()
	if err := fx.trust("node-a", old, old, next); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("after a restart, node-a's old certificate: %v, want ErrSuperseded", err)
	}
	if st, err := fx.m.Finish(ctx, false); err != nil || st.Ended == nil || st.Ended.Back || st.Ended.Kept != pki.FingerprintHex(next) || st.Ended.Dropped != pki.FingerprintHex(old) {
		t.Fatalf("finish: %+v, %v; want it ended on the new CA", st.Ended, err)
	}
	fx.restart()
	if st := fx.status(); st.Rotation != nil || st.Current.Fingerprint != pki.FingerprintHex(next) {
		t.Fatalf("after a restart after finish: %+v", st)
	}
}

// A start that crashed after it wrote the new CA and before it recorded
// the rotation leaves files that the next Open removes.
func TestCrashInStart(t *testing.T) {
	fx := newFixture(t)
	if _, err := pki.CreateNextCA(fx.dir); err != nil {
		t.Fatal(err)
	}
	fx.restart()
	if exists(t, filepath.Join(fx.dir, pki.NextCAKeyFile)) || exists(t, filepath.Join(fx.dir, pki.NextCACertFile)) {
		t.Fatal("Open left the files of a start that did not record its rotation")
	}
	if st := fx.status(); st.Rotation != nil {
		t.Fatal("a rotation after an incomplete start")
	}
	// And a start finds stale files too, and makes its own.
	if _, err := pki.CreateNextCA(fx.dir); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.m.Start(context.Background()); err != nil {
		t.Fatalf("start over stale files: %v", err)
	}
}

// A finish that crashed at each point of its file step is finished by the
// next Open: after the store recorded the end, after the key's rename, and
// after both renames.
func TestCrashInFinish(t *testing.T) {
	for _, stop := range []string{"recorded", "key renamed", "both renamed"} {
		t.Run(stop, func(t *testing.T) {
			fx := newFixture(t)
			ctx := context.Background()
			_, next := fx.mustStart()
			if _, err := fx.m.Renew(ctx, false); err != nil {
				t.Fatal(err)
			}
			rot, err := fx.st.GetCARotation(ctx)
			if err != nil {
				t.Fatal(err)
			}
			rot.Phase, rot.Listener = store.CAPhaseEnding, store.CANext
			if err := fx.st.SetCARotation(ctx, rot, false); err != nil {
				t.Fatal(err)
			}
			if stop != "recorded" {
				if err := os.Rename(filepath.Join(fx.dir, pki.NextCAKeyFile), filepath.Join(fx.dir, pki.CAKeyFile)); err != nil {
					t.Fatal(err)
				}
			}
			if stop == "both renamed" {
				if err := os.Rename(filepath.Join(fx.dir, pki.NextCACertFile), filepath.Join(fx.dir, pki.CACertFile)); err != nil {
					t.Fatal(err)
				}
			}
			fx.restart()
			if c, n := fx.cas(); c != next || n != ([32]byte{}) {
				t.Fatal("the interrupted finish did not end with the new CA")
			}
			if _, err := fx.st.GetCARotation(ctx); !errors.Is(err, store.ErrNotFound) {
				t.Fatal("the rotation is still recorded")
			}
		})
	}
}

// Abort in phase trust ends the rotation at once and deletes the new CA,
// also when it stopped half way.
func TestAbortInTrust(t *testing.T) {
	fx := newFixture(t, "node-a")
	ctx := context.Background()
	old, next := fx.mustStart()
	if err := fx.trust("node-a", old, old, next); err != nil {
		t.Fatal(err)
	}
	st, err := fx.m.Abort(ctx)
	if err != nil || st.Rotation != nil || st.Next != nil || st.Ended == nil || !st.Ended.Back ||
		st.Ended.Kept != pki.FingerprintHex(old) || st.Ended.Dropped != pki.FingerprintHex(next) {
		t.Fatalf("abort in phase trust: %+v %v", st, err)
	}
	if c, _ := fx.cas(); c != old || fx.m.Accepts(next) || exists(t, filepath.Join(fx.dir, pki.NextCAKeyFile)) {
		t.Fatal("abort in phase trust did not drop the new CA")
	}
	if _, err := fx.m.Abort(ctx); !errors.Is(err, ErrNoRotation) {
		t.Fatalf("abort with no rotation: %v", err)
	}

	// Stopped after the store recorded the end, before the files went.
	_, next = fx.mustStart()
	rot, _ := fx.st.GetCARotation(ctx)
	rot.Phase, rot.Toward = store.CAPhaseEnding, store.CACurrent
	if err := fx.st.SetCARotation(ctx, rot, false); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(fx.dir, pki.NextCAKeyFile)); err != nil {
		t.Fatal(err)
	}
	fx.restart()
	if c, n := fx.cas(); c != old || n != ([32]byte{}) || exists(t, filepath.Join(fx.dir, pki.NextCACertFile)) {
		t.Fatalf("the interrupted abort did not end with the old CA (%x)", next)
	}
}

// Abort in phase renew turns the rotation round: the nodes renew back under
// the old CA, and finish deletes the new one.
func TestAbortInRenew(t *testing.T) {
	fx := newFixture(t, "node-a", "node-b")
	ctx := context.Background()
	old, next := fx.mustStart()
	for _, n := range []string{"node-a", "node-b"} {
		if err := fx.trust(n, old, old, next); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fx.m.Renew(ctx, false); err != nil {
		t.Fatal(err)
	}
	if err := fx.m.moveListener(ctx); err != nil {
		t.Fatal(err)
	}
	if err := fx.trust("node-a", next, old, next); err != nil { // node-a renewed
		t.Fatal(err)
	}
	st, err := fx.m.Abort(ctx)
	if err != nil || st.Rotation == nil || st.Rotation.Toward != store.CACurrent || st.Rotation.Phase != store.CAPhaseRenew {
		t.Fatalf("abort in phase renew: %+v %v", st.Rotation, err)
	}
	if st.Rotation.From != pki.FingerprintHex(next) || st.Rotation.To != pki.FingerprintHex(old) {
		t.Fatal("the turned rotation does not go from the new CA to the old")
	}
	for _, n := range st.Nodes {
		if n.Seen || n.Renewed || n.Trusts {
			t.Fatalf("marks kept across the turn: %+v", n)
		}
	}
	// The reasons say the rotation turned round, not that it started.
	for _, w := range st.Rotation.Waiting {
		if w.Reason != "has not made a request since the rotation turned round" {
			t.Fatalf("right after the turn, %s: %q", w.Node, w.Reason)
		}
	}
	// Both CAs are still accepted, the bundle says renew under the old CA,
	// with the old CA cross-signed by the new one for a node that trusts
	// only the new.
	if !fx.m.Accepts(old) || !fx.m.Accepts(next) {
		t.Fatal("a turned rotation drops a CA")
	}
	b, _ := fx.m.Bundle()
	parsed, err := b.Parse()
	if err != nil || b.Renew != pki.FingerprintHex(old) || pki.Fingerprint(parsed[1].Cert) != old {
		t.Fatalf("bundle of a turned rotation: %+v %v", b, err)
	}
	if _, err := pki.VerifyCross(parsed[1].Cert, parsed[1].Cross, pki.MustTrust(parsed[0].Cert), time.Now()); err != nil {
		t.Fatalf("the old CA cross-signed by the new: %v", err)
	}
	if got := fx.m.RenewalCA(next, [][32]byte{old, next}); got.Fingerprint() != old {
		t.Fatal("a turned rotation renews under the new CA")
	}
	if _, err := fx.m.Abort(ctx); !errors.Is(err, ErrWrongPhase) {
		t.Fatalf("a second abort: %v", err)
	}
	// The listener goes back once every node trusts the old CA.
	for _, n := range []string{"node-a", "node-b"} {
		if err := fx.trust(n, next, old, next); err != nil {
			t.Fatal(err)
		}
	}
	if err := fx.m.moveListener(ctx); err != nil {
		t.Fatal(err)
	}
	if fx.m.Listener().Fingerprint() != old {
		t.Fatal("the listener did not go back to the old CA")
	}
	fx.restart()
	if _, err := fx.m.Finish(ctx, false); err == nil || !strings.Contains(err.Error(), "has not renewed its certificate back under the old CA yet") {
		t.Fatalf("finish before the nodes renewed back: %v", err)
	}
	for _, n := range []string{"node-a", "node-b"} {
		if err := fx.trust(n, old, old, next); err != nil {
			t.Fatal(err)
		}
	}
	if st, err := fx.m.Finish(ctx, false); err != nil || st.Ended == nil || !st.Ended.Back || st.Ended.Kept != pki.FingerprintHex(old) {
		t.Fatalf("finish of the turned rotation: %+v, %v; want it ended back on the old CA", st.Ended, err)
	}
	if c, n := fx.cas(); c != old || n != ([32]byte{}) || exists(t, filepath.Join(fx.dir, pki.NextCAKeyFile)) {
		t.Fatal("the turned rotation did not end with the old CA")
	}
}

// After a restore of a backup from phase trust, a node may present a
// certificate from the new CA: abort then turns round rather than cut it off.
func TestAbortInTrustWithARenewedNodeTurnsRound(t *testing.T) {
	fx := newFixture(t, "node-a")
	old, next := fx.mustStart()
	if err := fx.trust("node-a", next, old, next); err != nil {
		t.Fatal(err)
	}
	st, err := fx.m.Abort(context.Background())
	if err != nil || st.Rotation == nil || st.Rotation.Toward != store.CACurrent {
		t.Fatalf("abort: %+v %v", st.Rotation, err)
	}
	if !fx.m.Accepts(next) {
		t.Fatal("the node with a certificate from the new CA was cut off")
	}
}

// Offline nodes and builds without rotation: renew goes ahead without a
// node that is down, finish names it, and --force cuts it off.
func TestOfflineAndOldBuildNodes(t *testing.T) {
	fx := newFixture(t, "node-a", "node-down", "node-old")
	ctx := context.Background()
	old, next := fx.mustStart()
	fx.clock.advance(time.Hour) // every lease lapses
	for _, n := range []string{serverNode, "node-a", "node-old"} {
		if err := fx.st.RenewLease(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	if err := fx.trust("node-a", old, old, next); err != nil {
		t.Fatal(err)
	}
	if err := fx.m.Observe(ctx, "node-old", nil, false, old); err != nil {
		t.Fatal(err)
	}
	_, err := fx.m.Renew(ctx, false)
	var refused *RefusedError
	if !errors.As(err, &refused) || waitingIDs(refused.Waiting) != "node-old" || !strings.Contains(err.Error(), "build without CA rotation") {
		t.Fatalf("renew with an old build: %v", err)
	}
	if _, err := fx.m.Renew(ctx, true); err != nil {
		t.Fatalf("renew --force: %v", err)
	}
	if err := fx.trust("node-a", next, old, next); err != nil {
		t.Fatal(err)
	}
	// The listener stays: node-down and node-old do not trust the new CA.
	if err := fx.m.moveListener(ctx); err != nil {
		t.Fatal(err)
	}
	if fx.m.Listener().Fingerprint() != old {
		t.Fatal("the listener moved while two nodes do not trust the new CA")
	}
	_, err = fx.m.Finish(ctx, false)
	if !errors.As(err, &refused) || waitingIDs(refused.Waiting) != "node-down,node-old" {
		t.Fatalf("finish: %v", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "is down") || !strings.Contains(msg, "build without CA rotation") ||
		!strings.Contains(msg, "cut them off") {
		t.Fatalf("finish's refusal: %v", msg)
	}
	st, err := fx.m.Finish(ctx, true)
	if err != nil || st.Rotation != nil {
		t.Fatalf("finish --force: %v", err)
	}
	if fx.m.Listener().Fingerprint() != next || fx.m.Accepts(old) {
		t.Fatal("finish --force did not move to the new CA")
	}
}

// A node removed during a rotation drops out of what it waits for.
func TestRemovedNodeIsNotWaitedFor(t *testing.T) {
	fx := newFixture(t, "node-a", "node-gone")
	ctx := context.Background()
	old, next := fx.mustStart()
	if err := fx.trust("node-a", old, old, next); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.st.RemoveNode(ctx, "node-gone", true); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.m.Renew(ctx, false); err != nil {
		t.Fatalf("renew after the other node was removed: %v", err)
	}
}

// CA files that are not the rotation's are refused at Open.
func TestOpenRefusesFilesThatDoNotMatch(t *testing.T) {
	fx := newFixture(t)
	fx.mustStart()
	if err := os.Remove(filepath.Join(fx.dir, pki.NextCAKeyFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(fx.dir, pki.NextCACertFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := pki.CreateNextCA(fx.dir); err != nil {
		t.Fatal(err)
	}
	_, err := Open(context.Background(), Options{Dir: fx.dir, Store: fx.st, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err == nil || !strings.Contains(err.Error(), "not the CAs of the rotation") {
		t.Fatalf("Open with another new CA: %v", err)
	}
}

func TestFixedManager(t *testing.T) {
	ca, err := pki.CreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := Fixed(ca)
	if m.Listener() != ca || !m.Accepts(ca.Fingerprint()) || m.TokenCA(ca.Fingerprint()) != ca {
		t.Fatal("Fixed")
	}
	if _, err := m.Start(context.Background()); !errors.Is(err, ErrFixed) {
		t.Fatalf("Start on Fixed: %v", err)
	}
	if err := m.Observe(context.Background(), "node-a", nil, false, ca.Fingerprint()); err != nil {
		t.Fatal(err)
	}
	if st, err := m.Status(context.Background()); err != nil || st.Rotation != nil {
		t.Fatalf("Status on Fixed: %v", err)
	}
}

// logBuf is a log a Manager writes to while a test reads it.
type logBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// The lines a rotation logs for each node name the CA it goes to by the
// rotation's direction: the new one, or, after an abort turned it round,
// the old one it goes back to, with both fingerprints. In the fifth soak
// they called the old CA "the new cluster CA" after an abort.
func TestRotationLinesNameTheDirection(t *testing.T) {
	fx := newFixture(t, "node-a")
	ctx := context.Background()
	old, next := fx.mustStart()
	if err := fx.trust("node-a", old, old, next); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.m.Renew(ctx, false); err != nil {
		t.Fatal(err)
	}
	if err := fx.trust("node-a", next, old, next); err != nil {
		t.Fatal(err)
	}
	fields := func(to, from [32]byte) string { return "node=node-a ca=" + short(to) + " leaving=" + short(from) }
	forward := fx.logs.String()
	for _, want := range []string{
		`msg="a node trusts the CA the rotation goes to, the new one in ca-next.crt" ` + fields(next, old),
		`msg="a node presents a certificate from the CA the rotation goes to, the new one in ca-next.crt" ` + fields(next, old),
	} {
		if !strings.Contains(forward, want) {
			t.Errorf("towards the new CA, no line %q in:\n%s", want, forward)
		}
	}
	if strings.Contains(forward, "goes back to") {
		t.Errorf("towards the new CA, a line says the rotation goes back:\n%s", forward)
	}

	if _, err := fx.m.Abort(ctx); err != nil {
		t.Fatal(err)
	}
	if err := fx.trust("node-a", next, old, next); err != nil {
		t.Fatal(err)
	}
	if err := fx.trust("node-a", old, old, next); err != nil {
		t.Fatal(err)
	}
	back := strings.TrimPrefix(fx.logs.String(), forward)
	for _, want := range []string{
		`msg="a node trusts the CA the rotation goes back to, the old one in ca.crt, after an abort" ` + fields(old, next),
		`msg="a node presents a certificate from the CA the rotation goes back to, the old one in ca.crt, after an abort" ` + fields(old, next),
	} {
		if !strings.Contains(back, want) {
			t.Errorf("back towards the old CA, no line %q in:\n%s", want, back)
		}
	}
	for _, l := range strings.Split(back, "\n") {
		if strings.Contains(l, "a node ") && strings.Contains(l, "new one") {
			t.Errorf("back towards the old CA, a line names the new CA as the one it goes to: %s", l)
		}
	}
}
