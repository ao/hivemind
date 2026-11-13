package agent

import (
	"context"
	"fmt"
	"io"
	"maps"
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
	containers map[string]*fakeContainer // by ID
	images     map[string]bool
	calls      map[string]int
	// fail, when set, is asked before each operation; a non-nil error is
	// returned instead of doing it.
	fail func(op string, arg string) error
}

type fakeContainer struct {
	runtime.Container
	cfg runtime.ContainerConfig
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{containers: map[string]*fakeContainer{}, images: map[string]bool{}, calls: map[string]int{}}
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

func (f *fakeRuntime) Pull(_ context.Context, image string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("pull", image); err != nil {
		return err
	}
	ref, err := spec.ParseImage(image)
	if err != nil {
		return rtErr("pull", runtime.ErrInvalid)
	}
	f.images[ref.String()] = true
	return nil
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
	c.State = "running"
	return nil
}

func (f *fakeRuntime) Stop(_ context.Context, id string, _ time.Duration) error {
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
		c.State = "exited"
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
			out = append(out, x)
		}
	}
	return out, nil
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

func (f *fakeRuntime) setFail(fn func(op, arg string) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = fn
}
