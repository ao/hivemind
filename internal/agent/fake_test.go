package agent

import (
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/ao/hivemind/internal/runtime"
	"github.com/ao/hivemind/internal/spec"
)

// fakeRuntime is an in-memory runtime.Runtime. Its rules follow the Docker
// client: names are unique, Create needs the image, Remove without force
// refuses a running container.
type fakeRuntime struct {
	mu         sync.Mutex
	next       int
	nextPort   int                       // ephemeral host ports are 32768, 32769, ...
	containers map[string]*fakeContainer // by ID
	images     map[string]bool
	calls      map[string]int
	// fail, when set, is asked before each operation; a non-nil error is
	// returned instead of doing it.
	fail func(op string, arg string) error
	// now gives the time for StartedAt and FinishedAt. Tests set it to a fake clock.
	now func() time.Time
	// execFn, when set, decides the result of Exec on a running container.
	execFn func(ctx context.Context, id string, cmd []string) (int, error)
	// private maps an image to the credential a pull of it needs.
	private   map[string]runtime.RegistryAuth
	pullAuths []*runtime.RegistryAuth
	// pullFn, when set, runs before each pull, without the lock, so it may
	// block until ctx ends; a non-nil error is the pull's.
	pullFn func(ctx context.Context, image string) error
	// stopFn, when set, runs before each stop, without the lock, with the
	// container's name, so it may block as a container that ignores
	// SIGTERM does; a non-nil error is the stop's.
	stopFn func(ctx context.Context, name string) error
	// ports, when set, makes Start refuse a container whose fixed host port
	// a running container has published, as the engine does.
	ports bool
}

type fakeContainer struct {
	runtime.Container
	cfg runtime.ContainerConfig
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{containers: map[string]*fakeContainer{}, images: map[string]bool{}, calls: map[string]int{},
		now: time.Now}
}

func (f *fakeRuntime) begin(op, arg string) error {
	f.calls[op]++
	if f.fail != nil {
		return f.fail(op, arg)
	}
	return nil
}

func rtErr(op string, sentinel error) error { return &runtime.Error{Op: op, Err: sentinel} }

func (f *fakeRuntime) Info(context.Context) (runtime.Info, error) {
	return runtime.Info{NCPU: 2, MemTotal: 1 << 30}, nil
}

func (f *fakeRuntime) Pull(ctx context.Context, image string, auth *runtime.RegistryAuth) error {
	f.mu.Lock()
	fn := f.pullFn
	f.mu.Unlock()
	if fn != nil {
		if err := fn(ctx, image); err != nil {
			return err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pullAuths = append(f.pullAuths, auth)
	if err := f.begin("pull", image); err != nil {
		return err
	}
	ref, err := spec.ParseImage(image)
	if err != nil {
		return rtErr("pull", runtime.ErrInvalid)
	}
	// A private image needs its credential, as a registry with htpasswd
	// does; the messages are the Docker client's.
	if want, ok := f.private[ref.String()]; ok {
		switch {
		case auth == nil:
			return &runtime.Error{Op: "pull", Err: runtime.ErrUnauthorized, Message: "the registry refused the pull (no credential)"}
		case *auth != want:
			return &runtime.Error{Op: "pull", Err: runtime.ErrUnauthorized, Message: "the registry refused the pull (unauthorized)"}
		}
	}
	f.images[ref.String()] = true
	return nil
}

// makePrivate makes a pull of image need the credential want.
func (f *fakeRuntime) makePrivate(image string, want runtime.RegistryAuth) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ref, err := spec.ParseImage(image)
	if err != nil {
		panic(err)
	}
	if f.private == nil {
		f.private = map[string]runtime.RegistryAuth{}
	}
	f.private[ref.String()] = want
}

// removeImage deletes an image, as docker rmi -f does; containers made
// from it keep running.
func (f *fakeRuntime) removeImage(image string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ref, _ := spec.ParseImage(image)
	delete(f.images, ref.String())
}

// sentAuths returns the credential of each pull so far, nil for none, and
// forgets them.
func (f *fakeRuntime) sentAuths() []*runtime.RegistryAuth {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.pullAuths
	f.pullAuths = nil
	return out
}

// add puts a container in place directly, as if made out of band.
func (f *fakeRuntime) add(name, state string, labels map[string]string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	id := fmt.Sprintf("%064x", f.next)
	f.containers[id] = &fakeContainer{Container: runtime.Container{ID: id, Name: name, State: state, Labels: labels}}
	return id
}

func (f *fakeRuntime) Create(_ context.Context, cfg runtime.ContainerConfig) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("create", cfg.Name); err != nil {
		return "", err
	}
	cfg, err := runtime.Prepare(cfg)
	if err != nil {
		return "", rtErr("create", runtime.ErrInvalid)
	}
	if !f.images[cfg.Image] {
		return "", rtErr("create", runtime.ErrNotFound)
	}
	for _, c := range f.containers {
		if c.Name == cfg.Name {
			return "", rtErr("create", runtime.ErrConflict)
		}
	}
	f.next++
	id := fmt.Sprintf("%064x", f.next)
	f.containers[id] = &fakeContainer{
		Container: runtime.Container{ID: id, Name: cfg.Name, Image: cfg.Image, Labels: cfg.Labels(), State: "created"},
		cfg:       cfg,
	}
	return id, nil
}

func (f *fakeRuntime) find(ref string) *fakeContainer {
	if c, ok := f.containers[ref]; ok {
		return c
	}
	for _, c := range f.containers {
		if c.Name == ref {
			return c
		}
	}
	return nil
}

func (f *fakeRuntime) Start(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("start", id); err != nil {
		return err
	}
	c := f.find(id)
	if c == nil {
		return rtErr("start", runtime.ErrNotFound)
	}
	if f.ports {
		for _, p := range c.cfg.Ports {
			for _, o := range f.containers {
				for _, q := range o.Ports {
					if o != c && o.State == "running" && !p.Ephemeral && p.HostPort == q.HostPort && p.Protocol == q.Protocol {
						return &runtime.Error{Op: "start", Err: runtime.ErrConflict, Message: "port is already allocated"}
					}
				}
			}
		}
	}
	c.State = "running"
	c.StartedAt = f.now()
	c.FinishedAt = time.Time{}
	c.IPAddress = "127.0.0.1"
	// Like Docker, each start publishes the ports again, and an ephemeral
	// binding gets a new port.
	c.Ports = nil
	for _, p := range c.cfg.Ports {
		hp := p.HostPort
		if p.Ephemeral {
			f.nextPort++
			hp = 32767 + f.nextPort
		}
		c.Ports = append(c.Ports, runtime.PortBinding{HostIP: p.HostIP, HostPort: hp, ContainerPort: p.ContainerPort, Protocol: p.Protocol})
	}
	return nil
}

// exit makes a running container exit with code, as if its process ended.
func (f *fakeRuntime) exit(id string, code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.find(id); c != nil && c.State == "running" {
		c.State, c.ExitCode, c.FinishedAt, c.Ports = "exited", code, f.now(), nil
	}
}

func (f *fakeRuntime) Stop(ctx context.Context, id string, _ time.Duration) error {
	f.mu.Lock()
	fn, name := f.stopFn, ""
	if c := f.find(id); c != nil {
		name = c.Name
	}
	f.mu.Unlock()
	if fn != nil {
		if err := fn(ctx, name); err != nil {
			return err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("stop", id); err != nil {
		return err
	}
	c := f.find(id)
	if c == nil {
		return rtErr("stop", runtime.ErrNotFound)
	}
	if c.State == "running" {
		c.State, c.FinishedAt, c.Ports = "exited", f.now(), nil
	}
	return nil
}

func (f *fakeRuntime) Remove(_ context.Context, id string, force bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("remove", id); err != nil {
		return err
	}
	c := f.find(id)
	if c == nil {
		return rtErr("remove", runtime.ErrNotFound)
	}
	if c.State == "running" && !force {
		return rtErr("remove", runtime.ErrConflict)
	}
	delete(f.containers, c.ID)
	return nil
}

func (f *fakeRuntime) Inspect(_ context.Context, id string) (runtime.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("inspect", id); err != nil {
		return runtime.Container{}, err
	}
	c := f.find(id)
	if c == nil {
		return runtime.Container{}, rtErr("inspect", runtime.ErrNotFound)
	}
	out := c.Container
	out.Labels = maps.Clone(c.Labels)
	out.Ports = slices.Clone(c.Ports)
	return out, nil
}

func (f *fakeRuntime) List(_ context.Context, labels map[string]string) ([]runtime.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("list", ""); err != nil {
		return nil, err
	}
	var out []runtime.Container
	for _, c := range f.containers {
		match := true
		for k, v := range labels {
			got, ok := c.Labels[k]
			if !ok || (v != "" && got != v) {
				match = false
			}
		}
		if match {
			x := c.Container
			x.Labels = maps.Clone(c.Labels)
			x.Ports = slices.Clone(c.Ports)
			out = append(out, x)
		}
	}
	return out, nil
}

func (f *fakeRuntime) Exec(ctx context.Context, id string, cmd []string) (int, error) {
	f.mu.Lock()
	if err := f.begin("exec", id); err != nil {
		f.mu.Unlock()
		return 0, err
	}
	c := f.find(id)
	if c == nil || c.State != "running" {
		f.mu.Unlock()
		return 0, rtErr("exec", runtime.ErrConflict)
	}
	fn := f.execFn
	f.mu.Unlock()
	// The hook runs without the lock, so it may block until ctx ends.
	if fn == nil {
		return 0, nil
	}
	return fn(ctx, id, cmd)
}

func (f *fakeRuntime) setExec(fn func(ctx context.Context, id string, cmd []string) (int, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execFn = fn
}

func (f *fakeRuntime) Logs(context.Context, string, runtime.LogOptions, io.Writer, io.Writer) error {
	return nil
}

// snapshot returns the containers by name.
func (f *fakeRuntime) snapshot() map[string]fakeContainer {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]fakeContainer{}
	for _, c := range f.containers {
		out[c.Name] = *c
	}
	return out
}

// count returns how many times op was called, and resets the counters when
// reset is true.
func (f *fakeRuntime) counts(reset bool) map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := maps.Clone(f.calls)
	if reset {
		f.calls = map[string]int{}
	}
	return out
}

func (f *fakeRuntime) setStop(fn func(ctx context.Context, name string) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopFn = fn
}

func (f *fakeRuntime) setFail(fn func(op, arg string) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = fn
}
