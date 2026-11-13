//go:build integration

// Rolling-update tests against a real Docker daemon. They run through
// scripts/integration-docker.sh, which starts docker:dind and sets DOCKER_HOST.
//
// The replicas are on one node, so every move is in place. Each replica is
// polled by its container IP, not by a host port: a host port belongs to the
// app, and two replicas of one app can't publish the same host port on one
// node. The test container shares the daemon's network namespace, so it can
// reach the replicas' bridge addresses directly.
package rollout

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ao/hivemind/internal/agent"
	"github.com/ao/hivemind/internal/runtime"
	"github.com/ao/hivemind/internal/runtime/docker"
	"github.com/ao/hivemind/internal/spec"
	"github.com/ao/hivemind/internal/store"
)

const (
	replicas  = 3
	nodeID    = "node-1"
	servePort = 8080
	image     = "busybox:1.36"
)

// serveCommand writes $VERSION to the index page and serves /www on port 8080.
// The image is the same for both versions; the env value makes the template
// change, so a rollout runs.
var serveCommand = []string{"sh", "-c", "mkdir -p /www && echo $VERSION > /www/index.html && exec httpd -f -p 8080 -h /www"}

// webApp is an app whose replicas serve their version on port 8080, with an
// HTTP health check on healthPort.
func webApp(name, version string, healthPort int, startPeriod time.Duration) spec.App {
	return spec.App{
		Name:     name,
		Image:    image,
		Replicas: replicas,
		Command:  serveCommand,
		Env:      map[string]string{"VERSION": version},
		Health: &spec.Health{
			HTTP:        &spec.HTTPCheck{Path: "/", Port: healthPort},
			Interval:    spec.Duration(time.Second),
			Timeout:     spec.Duration(500 * time.Millisecond),
			Retries:     3,
			StartPeriod: spec.Duration(startPeriod),
		},
	}
}

// cluster is one node's store, agent and rollout controller, all in this test.
type cluster struct {
	t   *testing.T
	st  *store.Store
	rt  *docker.Client
	app string
	ctx context.Context
}

func newCluster(t *testing.T) *cluster {
	t.Helper()
	host := getenv(t, "DOCKER_HOST")
	rt, err := docker.New(host)
	if err != nil {
		t.Fatal(err)
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	c := &cluster{t: t, rt: rt, app: "ru-" + hex.EncodeToString(b[:])}

	ctx, cancel := context.WithCancel(context.Background())
	c.ctx = ctx
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "data", "hivemind.db"), store.WithMinReady(0))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	c.st = st
	if err := st.UpsertNode(ctx, store.Node{ID: nodeID, Name: "it", CPU: 2, Memory: 1 << 30}); err != nil {
		cancel()
		t.Fatal(err)
	}
	a, err := agent.New(agent.Config{NodeID: nodeID, Runtime: rt, Source: st, Interval: 200 * time.Millisecond, StopTimeout: 2 * time.Second})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	agentDone := make(chan struct{})
	go func() { defer close(agentDone); _ = a.Run(ctx) }()
	advDone := make(chan struct{})
	go func() {
		defer close(advDone)
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if err := st.AdvanceRollouts(ctx); err != nil && ctx.Err() == nil {
					t.Errorf("AdvanceRollouts: %v", err)
				}
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-agentDone
		<-advDone
		st.Close()
		// Remove every container this test made, by its app label.
		rmCtx, rmCancel := context.WithTimeout(context.Background(), time.Minute)
		defer rmCancel()
		cs, _ := rt.List(rmCtx, map[string]string{runtime.LabelApp: c.app})
		for _, x := range cs {
			_ = rt.Remove(rmCtx, x.ID, true)
		}
	})
	return c
}

func getenv(t *testing.T, k string) string {
	t.Helper()
	v := os.Getenv(k)
	if v == "" {
		t.Fatalf("%s is not set: run scripts/integration-docker.sh", k)
	}
	return v
}

// put deploys the app with the given version and health port.
func (c *cluster) put(version string, healthPort int, startPeriod time.Duration) {
	c.t.Helper()
	if _, err := c.st.PutApp(c.ctx, webApp(c.app, version, healthPort, startPeriod)); err != nil {
		c.t.Fatal(err)
	}
}

// waitUntil polls cond every 200 ms until it holds, or fails the test.
func waitUntil(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// poller samples every replica's HTTP answer about every 25 ms. A replica
// answers with its version, or is "down" when there is no running container
// or the request fails.
type poller struct {
	c        *cluster
	mu       sync.Mutex
	seen     map[int]map[string]bool
	latest   map[int]string
	maxDown  int            // most replicas down in one sample
	skipped  int            // samples the engine could not answer
	why      map[string]int // why each down replica was down, for the log
	errs     []string       // the first few request errors
	noIP     []string       // the first few containers with no IP, for the log
	timeline []string       // each change of a replica's answer, timestamped
	stop     chan struct{}
	done     chan struct{}
	client   *http.Client
}

func (c *cluster) startPoller() *poller {
	p := &poller{c: c, seen: map[int]map[string]bool{}, latest: map[int]string{}, why: map[string]int{}, stop: make(chan struct{}), done: make(chan struct{}),
		client: &http.Client{Timeout: 300 * time.Millisecond, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}}
	for i := range replicas {
		p.seen[i] = map[string]bool{}
	}
	go func() {
		defer close(p.done)
		for {
			select {
			case <-p.stop:
				return
			default:
			}
			p.sample()
			time.Sleep(25 * time.Millisecond)
		}
	}()
	return p
}

// sample asks each replica once, and counts how many were down. A replica
// with no running container is down. If the engine can't say where a
// container is, the sample is skipped: that is unknown, not down.
func (p *poller) sample() {
	ctx, cancel := context.WithTimeout(p.c.ctx, 5*time.Second)
	defer cancel()
	ips := map[int]string{}
	cs, err := p.c.rt.List(ctx, map[string]string{runtime.LabelApp: p.c.app})
	if err != nil {
		p.skip(err)
		return
	}
	for _, x := range cs {
		if !x.Running() {
			continue
		}
		idx, err := strconv.Atoi(x.Labels[runtime.LabelReplica])
		if err != nil {
			continue
		}
		ci, err := p.c.rt.Inspect(ctx, x.ID)
		if err != nil {
			p.skip(err)
			return
		}
		if ci.IPAddress != "" {
			ips[idx] = ci.IPAddress
		} else if len(p.noIP) < 4 {
			p.noIP = append(p.noIP, fmt.Sprintf("replica %d list %q, inspect %q exit %d, started %s, finished %s, ip empty, container %s",
				idx, x.State, ci.State, ci.ExitCode, ci.StartedAt.Format(time.RFC3339Nano), ci.FinishedAt.Format(time.RFC3339Nano), x.ID[:12]))
		}
	}
	down := 0
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range replicas {
		state := "down"
		if ip := ips[i]; ip != "" {
			state = p.get(ip)
		} else {
			p.why["no running container"]++
		}
		if state == "down" {
			down++
		}
		p.seen[i][state] = true
		if p.latest[i] != state && len(p.timeline) < 400 {
			p.timeline = append(p.timeline, fmt.Sprintf("%s r%d %s", time.Now().Format("05.000"), i, state))
		}
		p.latest[i] = state
	}
	if down > p.maxDown {
		p.maxDown = down
	}
}

// skip records a sample that could not be taken.
func (p *poller) skip(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.skipped++
	if p.skipped <= 3 {
		p.c.t.Logf("sample skipped: %v", err)
	}
}

func (p *poller) get(ip string) string {
	resp, err := p.client.Get(fmt.Sprintf("http://%s:%d/", ip, servePort))
	if err != nil {
		// Called with p.mu held, so the error log is safe here.
		p.why["request error"]++
		if len(p.errs) < 5 {
			p.errs = append(p.errs, err.Error())
		}
		return "down"
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	if resp.StatusCode != http.StatusOK {
		return "down"
	}
	return strings.TrimSpace(string(b))
}

// stopPoller ends sampling and returns a snapshot. The snapshot is taken
// after the last sample, so it does not race with one.
func (p *poller) stopPoller() (maxDown int, seen map[int]map[string]bool, latest map[int]string) {
	close(p.stop)
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	p.c.t.Logf("poller timeline:\n%s", strings.Join(p.timeline, "\n"))
	p.c.t.Logf("poller: %d samples skipped, down because: %v, first request errors: %q, no-IP cases: %q", p.skipped, p.why, p.errs, p.noIP)
	seen = map[int]map[string]bool{}
	latest = map[int]string{}
	for i := range replicas {
		seen[i] = map[string]bool{}
		for k, v := range p.seen[i] {
			seen[i][k] = v
		}
		latest[i] = p.latest[i]
	}
	return p.maxDown, seen, latest
}

// reset clears what the poller has counted so far, and keeps the latest
// answer of each replica. Tests call it once the replicas serve their first
// version, so the start-up samples (containers still booting) are not counted
// as rollout downtime.
func (p *poller) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.maxDown, p.skipped = 0, 0
	p.why, p.errs, p.noIP, p.timeline = map[string]int{}, nil, nil, nil
	for i := range replicas {
		p.seen[i] = map[string]bool{}
	}
}

func (p *poller) allLatest(version string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range replicas {
		if p.latest[i] != version {
			return false
		}
	}
	return true
}

// TestRollingUpdateKeepsAtMostOneReplicaDown updates three replicas from v1 to
// v2 on one node. Replicas move one at a time, so no more than one is down in
// any sample, and every replica ends on v2.
func TestRollingUpdateKeepsAtMostOneReplicaDown(t *testing.T) {
	c := newCluster(t)
	c.put("v1", servePort, 5*time.Second)
	p := c.startPoller()
	waitUntil(t, "three replicas serving v1", 3*time.Minute, func() bool { return p.allLatest("v1") })
	p.reset()

	c.put("v2", servePort, 5*time.Second)
	waitUntil(t, "the rollout to finish", 5*time.Minute, func() bool {
		ro, err := c.st.GetRollout(c.ctx, c.app)
		return err == nil && ro.State == store.RolloutDone
	})
	waitUntil(t, "every replica answering v2", time.Minute, func() bool { return p.allLatest("v2") })
	maxDown, seen, latest := p.stopPoller()

	if maxDown > 1 {
		t.Errorf("up to %d replicas down in one sample, want at most 1", maxDown)
	}
	for i := range replicas {
		if !seen[i]["v1"] || !seen[i]["v2"] {
			t.Errorf("replica %d answered %v, want both v1 and v2", i, keys(seen[i]))
		}
		if latest[i] != "v2" {
			t.Errorf("replica %d ends on %q, want v2", i, latest[i])
		}
	}
	t.Logf("most replicas down in one sample: %d; samples per replica: %v", maxDown, seen)
}

// TestBadHealthStopsRolloutAndOldReplicasKeepServing deploys a v2 whose health
// check never passes. The rollout stops at replica 0, which goes back to v1.
// Replicas 1 and 2 are never touched: they never go down and never answer v2.
func TestBadHealthStopsRolloutAndOldReplicasKeepServing(t *testing.T) {
	c := newCluster(t)
	c.put("v1", servePort, 5*time.Second)
	p := c.startPoller()
	waitUntil(t, "three replicas serving v1", 3*time.Minute, func() bool { return p.allLatest("v1") })
	p.reset()

	// v2 serves on 8080 but its health check asks port 9999, which nothing
	// listens on, so it never becomes Ready.
	c.put("v2", 9999, 2*time.Second)
	var ro store.Rollout
	waitUntil(t, "the rollout to pause", 3*time.Minute, func() bool {
		r, err := c.st.GetRollout(c.ctx, c.app)
		ro = r
		return err == nil && ro.State == store.RolloutPaused
	})
	const prefix = "rollout to generation 2 stopped: replica 0 is "
	if !strings.HasPrefix(ro.Reason, prefix) {
		t.Fatalf("reason %q, want prefix %q", ro.Reason, prefix)
	}
	t.Logf("rollout paused: %s", ro.Reason)

	// Replica 0 goes back to v1 and answers v1 again.
	waitUntil(t, "every replica answering v1 again", 3*time.Minute, func() bool { return p.allLatest("v1") })
	_, seen, _ := p.stopPoller()
	for _, i := range []int{1, 2} {
		if seen[i]["down"] || seen[i]["v2"] {
			t.Errorf("replica %d was touched by the stopped rollout: answered %v", i, keys(seen[i]))
		}
	}
	reps, err := c.st.Replicas(c.ctx, c.app)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range reps {
		if r.Generation != 1 {
			t.Errorf("replica %d is at generation %d, want 1", r.Index, r.Generation)
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
