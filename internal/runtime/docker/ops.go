package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ao/hivemind/internal/runtime"
	"github.com/ao/hivemind/internal/spec"
)

// Info returns the daemon's CPU count and memory.
func (c *Client) Info(ctx context.Context) (runtime.Info, error) {
	const op = "info"
	resp, err := c.call(ctx, op, http.MethodGet, "/info", nil, nil)
	if err != nil {
		return runtime.Info{}, err
	}
	defer resp.Body.Close()
	var body struct {
		NCPU          int    `json:"NCPU"`
		MemTotal      int64  `json:"MemTotal"`
		ServerVersion string `json:"ServerVersion"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return runtime.Info{}, c.readError(ctx, op, err)
	}
	return runtime.Info{NCPU: body.NCPU, MemTotal: body.MemTotal, ServerVersion: body.ServerVersion}, nil
}

// Pull fetches an image and reads the progress stream to the end. The engine
// reports pull failures inside the stream, with HTTP 200, so the stream is
// checked for an error object.
func (c *Client) Pull(ctx context.Context, ref string) error {
	const op = "pull"
	img, err := spec.ParseImage(ref)
	if err != nil {
		return fmt.Errorf("%w: image: %v", runtime.ErrInvalid, err)
	}
	q := url.Values{}
	if img.Digest != "" {
		q.Set("fromImage", img.Repository()+"@"+img.Digest)
	} else {
		q.Set("fromImage", img.Repository())
		q.Set("tag", img.Tag)
	}
	resp, err := c.call(ctx, op, http.MethodPost, "/images/create", q, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	dec := json.NewDecoder(resp.Body)
	for {
		var m struct {
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		err := dec.Decode(&m)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return c.readError(ctx, op, err)
		}
		msg := m.Error
		if msg == "" {
			msg = m.ErrorDetail.Message
		}
		if msg != "" {
			e := classify(op, 0, msg)
			if pullNotFound(msg) {
				e.Err = runtime.ErrNotFound
			}
			return e
		}
	}
}

// Create makes a container with the hivemind labels, a restart policy of
// "no", and the requested ports and volumes. Ports default to 127.0.0.1.
func (c *Client) Create(ctx context.Context, cfg runtime.ContainerConfig) (string, error) {
	const op = "create"
	cfg, err := runtime.Prepare(cfg)
	if err != nil {
		return "", err
	}

	env := make([]string, 0, len(cfg.Env))
	for k, v := range cfg.Env {
		env = append(env, k+"="+v)
	}
	slices.Sort(env)

	exposed := map[string]struct{}{}
	bindings := map[string][]portBinding{}
	for _, p := range cfg.Ports {
		key := strconv.Itoa(p.ContainerPort) + "/" + p.Protocol
		exposed[key] = struct{}{}
		bindings[key] = append(bindings[key], portBinding{
			HostIP:   p.HostIP,
			HostPort: strconv.Itoa(p.HostPort),
		})
	}
	mounts := make([]mount, 0, len(cfg.Volumes))
	for _, v := range cfg.Volumes {
		mounts = append(mounts, mount{Type: "volume", Source: v.Name, Target: v.Path})
	}

	body := createRequest{
		Image:        cfg.Image,
		Cmd:          cfg.Command,
		Env:          env,
		Labels:       cfg.Labels(),
		Tty:          false, // multiplexed logs; see StdCopy
		ExposedPorts: exposed,
		HostConfig: hostConfig{
			PortBindings:  bindings,
			Mounts:        mounts,
			RestartPolicy: restartPolicy{Name: "no"},
		},
	}
	resp, err := c.call(ctx, op, http.MethodPost, "/containers/create",
		url.Values{"name": {cfg.Name}}, body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		ID string `json:"Id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", c.readError(ctx, op, err)
	}
	if out.ID == "" {
		return "", &runtime.Error{Op: op, Message: "the daemon returned no container ID"}
	}
	return out.ID, nil
}

// Start starts a container. A container that is already running is not an error.
func (c *Client) Start(ctx context.Context, id string) error {
	const op = "start"
	p, err := containerPath(op, id)
	if err != nil {
		return err
	}
	resp, err := c.call(ctx, op, http.MethodPost, p+"/start", nil, nil, http.StatusNotModified)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// Stop stops a container. The timeout is rounded up to whole seconds, which
// is the engine's resolution. A stopped container is not an error.
func (c *Client) Stop(ctx context.Context, id string, timeout time.Duration) error {
	const op = "stop"
	p, err := containerPath(op, id)
	if err != nil {
		return err
	}
	if timeout < 0 {
		return &runtime.Error{Op: op, Err: runtime.ErrInvalid, Message: "timeout must not be negative"}
	}
	secs := int(math.Ceil(timeout.Seconds()))
	resp, err := c.call(ctx, op, http.MethodPost, p+"/stop",
		url.Values{"t": {strconv.Itoa(secs)}}, nil, http.StatusNotModified)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// Remove deletes a container. Named volumes stay, because the engine is asked
// to remove only anonymous volumes (v=false).
func (c *Client) Remove(ctx context.Context, id string, force bool) error {
	const op = "remove"
	p, err := containerPath(op, id)
	if err != nil {
		return err
	}
	q := url.Values{"v": {"false"}, "force": {strconv.FormatBool(force)}}
	resp, err := c.call(ctx, op, http.MethodDelete, p, q, nil)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// Inspect returns one container.
func (c *Client) Inspect(ctx context.Context, id string) (runtime.Container, error) {
	const op = "inspect"
	p, err := containerPath(op, id)
	if err != nil {
		return runtime.Container{}, err
	}
	resp, err := c.call(ctx, op, http.MethodGet, p+"/json", nil, nil)
	if err != nil {
		return runtime.Container{}, err
	}
	defer resp.Body.Close()
	var b struct {
		ID     string `json:"Id"`
		Name   string `json:"Name"`
		Config struct {
			Image  string            `json:"Image"`
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
		State struct {
			Status    string `json:"Status"`
			ExitCode  int    `json:"ExitCode"`
			OOMKilled bool   `json:"OOMKilled"`
		} `json:"State"`
		HostConfig struct {
			RestartPolicy struct {
				Name string `json:"Name"`
			} `json:"RestartPolicy"`
		} `json:"HostConfig"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		return runtime.Container{}, c.readError(ctx, op, err)
	}
	return runtime.Container{
		ID:            b.ID,
		Name:          strings.TrimPrefix(b.Name, "/"),
		Image:         b.Config.Image,
		Labels:        b.Config.Labels,
		State:         b.State.Status,
		ExitCode:      b.State.ExitCode,
		OOMKilled:     b.State.OOMKilled,
		RestartPolicy: b.HostConfig.RestartPolicy.Name,
	}, nil
}

// List returns every container, stopped ones included, whose labels match.
// The engine's list output has no exit code, restart policy or OOM flag, so
// those fields are zero. Use Inspect for them.
func (c *Client) List(ctx context.Context, labels map[string]string) ([]runtime.Container, error) {
	const op = "list"
	match := make([]string, 0, len(labels))
	for k, v := range labels {
		if v == "" {
			match = append(match, k)
		} else {
			match = append(match, k+"="+v)
		}
	}
	slices.Sort(match)
	filters, err := json.Marshal(map[string][]string{"label": match})
	if err != nil {
		return nil, &runtime.Error{Op: op, Message: "encoding filters", Cause: err}
	}
	resp, err := c.call(ctx, op, http.MethodGet, "/containers/json",
		url.Values{"all": {"1"}, "filters": {string(filters)}}, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var list []struct {
		ID     string            `json:"Id"`
		Names  []string          `json:"Names"`
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
		State  string            `json:"State"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, c.readError(ctx, op, err)
	}
	out := make([]runtime.Container, 0, len(list))
	for _, item := range list {
		name := ""
		if len(item.Names) > 0 {
			name = strings.TrimPrefix(item.Names[0], "/")
		}
		out = append(out, runtime.Container{
			ID:     item.ID,
			Name:   name,
			Image:  item.Image,
			Labels: item.Labels,
			State:  item.State,
		})
	}
	return out, nil
}

// Logs copies the container's log to stdout and stderr. With Follow it blocks
// until the container exits or ctx is cancelled.
func (c *Client) Logs(ctx context.Context, id string, opts runtime.LogOptions, stdout, stderr io.Writer) error {
	const op = "logs"
	p, err := containerPath(op, id)
	if err != nil {
		return err
	}
	if opts.Tail < 0 {
		return &runtime.Error{Op: op, Err: runtime.ErrInvalid, Message: "tail must not be negative"}
	}
	tail := "all"
	if opts.Tail > 0 {
		tail = strconv.Itoa(opts.Tail)
	}
	q := url.Values{
		"stdout": {"1"},
		"stderr": {"1"},
		"follow": {strconv.FormatBool(opts.Follow)},
		"tail":   {tail},
	}
	resp, err := c.call(ctx, op, http.MethodGet, p+"/logs", q, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	src := &readTracker{r: resp.Body}
	err = StdCopy(stdout, stderr, src)
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	// A failed read from the connection, or a frame cut short by the end of
	// the stream, means the daemon went away. A failed write is the caller's.
	if errors.Is(err, io.ErrUnexpectedEOF) || (src.err != nil && src.err != io.EOF) {
		return &runtime.Error{Op: op, Err: runtime.ErrUnavailable, Message: "lost the connection to the Docker daemon during the log stream", Cause: err}
	}
	return &runtime.Error{Op: op, Message: "writing the log output", Cause: err}
}

// readTracker remembers the last error its reader returned, so a failed
// write can be told apart from a failed read.
type readTracker struct {
	r   io.Reader
	err error
}

func (t *readTracker) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if err != nil {
		t.err = err
	}
	return n, err
}

// Request bodies for POST /containers/create. Field names are the engine's.
type createRequest struct {
	Image        string              `json:"Image"`
	Cmd          []string            `json:"Cmd,omitempty"`
	Env          []string            `json:"Env,omitempty"`
	Labels       map[string]string   `json:"Labels"`
	Tty          bool                `json:"Tty"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
	HostConfig   hostConfig          `json:"HostConfig"`
}

type hostConfig struct {
	PortBindings  map[string][]portBinding `json:"PortBindings,omitempty"`
	Mounts        []mount                  `json:"Mounts,omitempty"`
	RestartPolicy restartPolicy            `json:"RestartPolicy"`
}

type portBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

type mount struct {
	Type   string `json:"Type"`
	Source string `json:"Source"`
	Target string `json:"Target"`
}

type restartPolicy struct {
	Name string `json:"Name"`
}
