package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/spec"
)

// big returns an app whose stored spec is about kib KiB, in its command.
func big(name string, replicas, kib int) spec.App {
	args := make([]string, 0, kib/4)
	for i := 0; i < kib/4; i++ {
		args = append(args, strings.Repeat("a", 4<<10-16))
	}
	return spec.App{Name: name, Image: "nginx:alpine", Replicas: replicas, Command: args}
}

func placedCount(t *testing.T, s *Store, app string) (placed int, reason string) {
	t.Helper()
	reps, err := s.Replicas(ctxT(t), app)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range reps {
		if r.NodeID != "" {
			placed++
		} else if reason == "" {
			reason = r.PendingReason
		}
	}
	return placed, reason
}

// Placement puts no replica on a node that it would take past
// MaxNodeAssignmentBytes: 100 replicas of a 250 KiB spec are 25 MiB, so
// the node takes those that fit, and the rest wait with a reason that says
// why.
func TestPlacementKeepsANodesAssignmentsWithinTheBound(t *testing.T) {
	s := newT(t)
	ctx := ctxT(t)
	if _, err := s.PutApp(ctx, big("web", 100, 250)); err != nil {
		t.Fatal(err)
	}
	placed, reason := placedCount(t, s, "web")
	if placed < 55 || placed > 63 {
		t.Fatalf("%d replicas placed, want the 15 MiB's worth, about 60", placed)
	}
	if want := "every eligible node's assignments would be more than the 15 MiB a node reads"; !strings.Contains(reason, want) {
		t.Fatalf("reason %q, want %q", reason, want)
	}
	sizes, err := s.AssignmentBytes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if b := sizes[node1]; b > MaxNodeAssignmentBytes || b < MaxNodeAssignmentBytes-(520<<10) {
		t.Fatalf("node %s: %d bytes of assignments, want just under %d", node1, b, MaxNodeAssignmentBytes)
	}
	// A second node takes the rest.
	if err := s.UpsertNode(ctx, Node{ID: "node-2", Name: "two", CPU: 4, Memory: 8 << 30}); err != nil {
		t.Fatal(err)
	}
	if placed, reason := placedCount(t, s, "web"); placed != 100 {
		t.Fatalf("%d placed with two nodes, want 100: %s", placed, reason)
	}
}

// A put whose larger spec would take a node past the bound in place, as a
// rollout moves the replicas the node has, is refused, and changes nothing;
// a smaller spec is taken.
func TestPutRefusesASpecThatGrowsANodePastTheBound(t *testing.T) {
	s := newT(t)
	ctx := ctxT(t)
	a, err := s.PutApp(ctx, big("web", 70, 200))
	if err != nil {
		t.Fatal(err)
	}
	if placed, _ := placedCount(t, s, "web"); placed != 70 {
		t.Fatalf("%d placed, want 70 (14 MiB)", placed)
	}
	_, err = s.PutApp(ctx, big("web", 70, 248))
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "app web's spec would make the assignments of node "+node1) ||
		!strings.Contains(err.Error(), "more than the 15 MiB a node reads; use fewer replicas on that node, a smaller spec, or more nodes") {
		t.Fatalf("put of a spec that makes them 17 MiB: %v", err)
	}
	if got, err := s.GetApp(ctx, "web"); err != nil || got.Generation != a.Generation {
		t.Fatalf("web after the refused put: %+v, %v", got, err)
	}
	if _, err := s.PutApp(ctx, big("web", 70, 100)); err != nil {
		t.Fatalf("a smaller spec: %v", err)
	}
}

// A registry login whose credential, repeated on every running replica of an
// image from that registry, would take a node past the bound is refused; a
// short one is taken.
func TestRegistryLoginRefusesACredentialThatGrowsANodePastTheBound(t *testing.T) {
	s := newT(t)
	ctx := ctxT(t)
	for _, name := range []string{"a", "b"} {
		if _, err := s.PutApp(ctx, spec.App{Name: name, Image: "reg.example.com/app:1", Replicas: 100}); err != nil {
			t.Fatal(err)
		}
	}
	// 16 KiB of < is 96 KiB once escaped, on 200 replicas: about 19 MiB.
	_, err := s.PutRegistry(ctx, "reg.example.com", "u", strings.Repeat("<", MaxRegistryPasswordLen))
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "the credential for reg.example.com would make the assignments of node") {
		t.Fatalf("a long credential: %v", err)
	}
	if regs, _ := s.ListRegistries(ctx); len(regs) != 0 {
		t.Fatalf("registries after the refused login: %+v", regs)
	}
	if _, err := s.PutRegistry(ctx, "reg.example.com", "u", "token"); err != nil {
		t.Fatalf("a short credential: %v", err)
	}
}

// jsonLen is at least the length Go's encoder gives.
func TestJSONLenBoundsTheEncoder(t *testing.T) {
	for _, s := range []string{"", "plain", `"\`, "<>&", "\x00\x1f\n", "ü€😀", "\xff\xfe", "  ", strings.Repeat("a<", 100)} {
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if got := jsonLen(s); got < len(b) {
			t.Errorf("jsonLen(%q) = %d, the encoder wrote %d", s, got, len(b))
		}
	}
}

// A node already past the bound, as one could be before it, takes a put that
// does not grow it: a rollout to a spec of the same length, and a put that
// lowers the replica count. A longer spec is still refused.
func TestPutThatDoesNotGrowANodePastTheBoundIsTaken(t *testing.T) {
	// FirstNode ignores the bound, as placement before it did.
	s := newT(t, WithPlacement(FirstNode))
	ctx := ctxT(t)
	a := big("web", 90, 200)
	a.Env = map[string]string{"V": "1"}
	if _, err := s.PutApp(ctx, a); err != nil {
		t.Fatal(err)
	}
	sizes, err := s.AssignmentBytes(ctx)
	if err != nil || sizes[node1] <= MaxNodeAssignmentBytes {
		t.Fatalf("setup: node %s has %d bytes, want more than %d: %v", node1, sizes[node1], MaxNodeAssignmentBytes, err)
	}
	a.Env = map[string]string{"V": "2"}
	if _, err := s.PutApp(ctx, a); err != nil {
		t.Fatalf("a rollout to a spec of the same length: %v", err)
	}
	a.Replicas = 80
	if _, err := s.PutApp(ctx, a); err != nil {
		t.Fatalf("a put that lowers the replica count: %v", err)
	}
	a.Env = map[string]string{"V": "33"}
	if _, err := s.PutApp(ctx, a); !errors.Is(err, ErrConflict) {
		t.Fatalf("a rollout to a longer spec: %v", err)
	}
}

// The bytes a put's check makes for after the put, from those before it, are
// those read after it, with each of the app's copies on a node at the longer
// of its stored spec and the new one: through a series of puts that grow,
// scale down, scale up again replicas still marked for removal, and change
// the image, of an app beside another with a credential for its registry,
// while the put's placement also places the other app's waiting replica.
func TestPutBytesAfterIsWhatIsReadAfterThePut(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	s := newT(t, WithClock(clock.Now), WithMinReady(20*time.Second))
	ctx := ctxT(t)
	renew := func(ids ...string) {
		t.Helper()
		for range 2 {
			clock.Advance(10 * time.Second)
			for _, id := range ids {
				if err := s.RenewLease(ctx, id); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if err := s.UpsertNode(ctx, Node{ID: "node-2", Name: "two", CPU: 4, Memory: 8 << 30}); err != nil {
		t.Fatal(err)
	}
	renew(node1, "node-2")
	if _, err := s.PutRegistry(ctx, "reg.example.com", "u", "token"); err != nil {
		t.Fatal(err)
	}
	// Three replicas of a host port on two nodes: one waits.
	other := spec.App{Name: "other", Image: "reg.example.com/o:1", Replicas: 3, Ports: []spec.Port{{Host: 9000, Container: 80}}}
	if _, err := s.PutApp(ctx, other); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutApp(ctx, spec.App{Name: "web", Image: "reg.example.com/w:1", Replicas: 4}); err != nil {
		t.Fatal(err)
	}
	// A third node, ready once its wait is over; nothing places meanwhile.
	if err := s.UpsertNode(ctx, Node{ID: "node-3", Name: "three", CPU: 4, Memory: 8 << 30}); err != nil {
		t.Fatal(err)
	}
	renew(node1, "node-2", "node-3")
	if n, _ := placedCount(t, s, "other"); n != 2 {
		t.Fatalf("setup: %d replicas of other placed, want 2", n)
	}

	steps := []spec.App{
		// Longer, and more replicas; other's third replica goes to node-3.
		{Name: "web", Image: "reg.example.com/w:1", Replicas: 6, Env: map[string]string{"A": "a longer value"}},
		// Fewer: four replicas are marked for removal.
		{Name: "web", Image: "reg.example.com/w:1", Replicas: 2, Env: map[string]string{"A": "a much, much longer value"}},
		// Three of them wanted again, with their credential.
		{Name: "web", Image: "reg.example.com/w:1", Replicas: 5, Env: map[string]string{"A": "a much, much, much longer value"}},
		// Another image, from no registry with a credential.
		{Name: "web", Image: "nginx:alpine", Replicas: 5, Env: map[string]string{"A": "a much, much, much, much longer value"}},
		// Shorter, with replicas still marked for removal.
		{Name: "web", Image: "nginx:alpine", Replicas: 3},
	}
	for i, a := range steps {
		c, err := cleanSpec(a)
		if err != nil {
			t.Fatal(err)
		}
		js, _ := json.Marshal(c)
		var made map[string]int64
		s.bytesChecked = func(after map[string]int64) { made = after }
		if _, err := s.PutApp(ctx, a); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		s.bytesChecked = nil
		if made == nil {
			t.Fatalf("step %d: the check did not run", i)
		}
		placed, err := placedOn(ctx, s.rdb)
		if err != nil {
			t.Fatal(err)
		}
		creds, err := registryBytes(ctx, s.rdb)
		if err != nil {
			t.Fatal(err)
		}
		read := map[string]int64{}
		for _, x := range placed {
			n := x.specLen
			if x.App == "web" {
				n = max(n, len(js))
			}
			read[x.NodeID] += assignmentBytes(x, n, credFor(creds, x.Desired, x.Spec))
		}
		for _, n := range []string{node1, "node-2", "node-3"} {
			if made[n] != read[n] {
				t.Errorf("step %d, node %s: the check made %d bytes, %d are read after the put", i, n, made[n], read[n])
			}
		}
	}
	if n, _ := placedCount(t, s, "other"); n != 3 {
		t.Fatalf("%d replicas of other placed, want 3: the puts' placement placed none of them", n)
	}
}
