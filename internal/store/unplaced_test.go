package store

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ao/hivemind/internal/spec"
)

func withPort(a spec.App, bind string, host int) spec.App {
	a.Ports = append(a.Ports, spec.Port{Bind: bind, Host: host, Container: 80})
	return a
}

func putPlaced(t *testing.T, s *Store, a spec.App) []Unplaced {
	t.Helper()
	_, un, err := s.PutAppPlaced(ctxT(t), a, PutOptions{})
	if err != nil {
		t.Fatalf("PutAppPlaced %s: %v", a.Name, err)
	}
	checkInvariants(t, s)
	return un
}

// wantUnplaced checks a write's unplaced replicas, and that each reason
// starts with the pending reason status shows.
func wantUnplaced(t *testing.T, s *Store, app string, got, want []Unplaced) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unplaced replicas of %s:\n%+v\nwant:\n%+v", app, got, want)
	}
	reasons := map[int]string{}
	for _, r := range replicasOf(t, s, app) {
		reasons[r.Index] = r.PendingReason
	}
	for _, u := range got {
		if reasons[u.Index] == "" || !strings.HasPrefix(u.Reason, reasons[u.Index]) {
			t.Errorf("replica %d: reason %q does not start with its pending reason %q", u.Index, u.Reason, reasons[u.Index])
		}
	}
}

const portInUse8080 = "host port 127.0.0.1:8080/tcp is in use on every eligible node"

// A host port that another app publishes on the only node: the put
// succeeds, and says which replica waits, why, and that it lasts. Once the
// app that holds it is deleted, its copy is leaving, and the wait no longer
// lasts.
func TestPutAppPlacedPortHeldByAnotherApp(t *testing.T) {
	s := newT(t)
	api := withPort(spec.App{Name: "api", Image: "nginx:alpine", Replicas: 1}, "", 8080)
	if un := putPlaced(t, s, api); un != nil {
		t.Fatalf("api placed everything, but: %+v", un)
	}
	w := withPort(web(1), "", 8080)
	wantUnplaced(t, s, "web", putPlaced(t, s, w), []Unplaced{{Index: 0, Reason: portInUse8080 + "; held by app api", Lasting: true}})

	if err := s.DeleteApp(ctxT(t), "api"); err != nil {
		t.Fatal(err)
	}
	wantUnplaced(t, s, "web", putPlaced(t, s, w), []Unplaced{{Index: 0, Reason: portInUse8080 + "; held by app api"}})
}

// Another replica of the same app holds the port; a scale lists the new
// replicas it could not place.
func TestPutAppPlacedPortHeldByItsOwnReplica(t *testing.T) {
	s := newT(t)
	w := withPort(web(2), "", 8080)
	wantUnplaced(t, s, "web", putPlaced(t, s, w),
		[]Unplaced{{Index: 1, Reason: portInUse8080 + "; held by another replica of web", Lasting: true}})

	_, un, err := s.ScalePlaced(ctxT(t), "web", 3)
	if err != nil {
		t.Fatal(err)
	}
	held := portInUse8080 + "; held by another replica of web"
	wantUnplaced(t, s, "web", un, []Unplaced{{Index: 1, Reason: held, Lasting: true}, {Index: 2, Reason: held, Lasting: true}})

	if _, un, err = s.ScalePlaced(ctxT(t), "web", 1); err != nil || un != nil {
		t.Fatalf("scale to 1: %+v, %v", un, err)
	}
}

// With two nodes, the replica goes to the node where the port is free, and
// there is nothing to list.
func TestPutAppPlacedEverything(t *testing.T) {
	e := newBindEnv(t, map[string]string{"n1": "10.0.0.1", "n2": "10.0.0.2"})
	if un := putPlaced(t, e.s, withPort(spec.App{Name: "api", Image: "nginx:alpine", Replicas: 1}, "", 8080)); un != nil {
		t.Fatalf("api: %+v", un)
	}
	if un := putPlaced(t, e.s, withPort(web(1), "", 8080)); un != nil {
		t.Fatalf("web: %+v", un)
	}
	if un := putPlaced(t, e.s, web(3)); un != nil {
		t.Fatalf("web without ports: %+v", un)
	}
}

// More memory or CPU than any node has lasts; more than is free does not.
func TestPutAppPlacedResources(t *testing.T) {
	s := newT(t) // one node: 4 CPUs, 8Gi
	big := web(1)
	big.Resources = spec.Resources{Memory: 16 << 30}
	wantUnplaced(t, s, "web", putPlaced(t, s, big), []Unplaced{{Index: 0, Reason: "no node has 16Gi of memory free", Lasting: true}})

	cpu := spec.App{Name: "cpu", Image: "nginx:alpine", Replicas: 1, Resources: spec.Resources{CPU: 6}}
	wantUnplaced(t, s, "cpu", putPlaced(t, s, cpu), []Unplaced{{Index: 0, Reason: "no node has 6 CPUs free", Lasting: true}})

	db := spec.App{Name: "db", Image: "postgres:17", Replicas: 1, Resources: spec.Resources{Memory: 6 << 30}}
	if un := putPlaced(t, s, db); un != nil {
		t.Fatalf("db: %+v", un)
	}
	mid := spec.App{Name: "mid", Image: "nginx:alpine", Replicas: 1, Resources: spec.Resources{Memory: 4 << 30}}
	wantUnplaced(t, s, "mid", putPlaced(t, s, mid), []Unplaced{{Index: 0, Reason: "no node has 4Gi of memory free"}})
}

// A host port bound to an address that no node has lasts.
func TestPutAppPlacedUnknownBindAddress(t *testing.T) {
	e := newBindEnv(t, map[string]string{"n1": "10.0.0.1", "n2": "10.0.0.2"})
	a := withPort(web(1), "10.9.9.9", 8080)
	un := putPlaced(t, e.s, a)
	if len(un) != 1 || !un[0].Lasting || !strings.HasPrefix(un[0].Reason, "no node has address 10.9.9.9, and host port 10.9.9.9:8080/tcp binds address 10.9.9.9") {
		t.Fatalf("unplaced: %+v", un)
	}
	wantUnplaced(t, e.s, "web", un, un)

	// The address one node has: placed there.
	if un := putPlaced(t, e.s, withPort(web(1), "10.0.0.2", 8080)); un != nil {
		t.Fatalf("bound to n2's address: %+v", un)
	}
}

// What can change by itself does not last: no node registered yet, or a
// node that has not been ready for the minimum time.
func TestPutAppPlacedWaitsThatPass(t *testing.T) {
	s := openT(t, filepath.Join(t.TempDir(), "data", "hivemind.db"))
	wantUnplaced(t, s, "web", putPlaced(t, s, web(1)), []Unplaced{{Index: 0, Reason: "no nodes are registered"}})

	if err := s.UpsertNode(ctxT(t), Node{ID: node1, Name: "one", CPU: 4, Memory: 8 << 30}); err != nil {
		t.Fatal(err)
	}
	wantUnplaced(t, s, "web", putPlaced(t, s, web(2)), []Unplaced{
		{Index: 0, Reason: "no node has been ready for 30s"}, {Index: 1, Reason: "no node has been ready for 30s"}})
}
