package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

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
