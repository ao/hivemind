// Package runtime is the interface between hivemind and a container engine.
// The Docker Engine implementation is in internal/runtime/docker.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"time"

	"github.com/ao/hivemind/internal/spec"
)

// Labels that record what hivemind started. The agent rebuilds its view of a
// node from these labels after a restart.
const (
	LabelApp        = "hivemind.app"
	LabelReplica    = "hivemind.replica"
	LabelGeneration = "hivemind.generation"
)

// Sentinel errors. Errors returned by a Runtime match these with errors.Is.
var (
	// ErrNotFound means the container, or the image, does not exist.
	ErrNotFound = errors.New("not found")
	// ErrConflict means the request clashes with existing state: a container
	// name in use, or a host port already allocated.
	ErrConflict = errors.New("conflict")
	// ErrUnavailable means the engine could not be reached, or the connection
	// to it broke during the call.
	ErrUnavailable = errors.New("container engine unavailable")
	// ErrInvalid means the configuration was rejected before anything was sent
	// to the engine.
	ErrInvalid = errors.New("invalid configuration")
)

// Error is returned by Runtime methods when an operation fails.
type Error struct {
	// Op is the operation that failed, such as "create" or "stop".
	Op string
	// Err is one of the sentinel errors above, or nil.
	Err error
	// Message is what the engine said, or a short description.
	Message string
	// Cause is the underlying transport error, if any.
	Cause error
}

func (e *Error) Error() string {
	msg := e.Message
	if msg == "" && e.Err != nil {
		msg = e.Err.Error()
	}
	s := e.Op + ": " + msg
	if e.Cause != nil {
		s += ": " + e.Cause.Error()
	}
	return s
}

// Unwrap returns the sentinel and the transport cause, so errors.Is works on both.
func (e *Error) Unwrap() []error {
	var out []error
	if e.Err != nil {
		out = append(out, e.Err)
	}
	if e.Cause != nil {
		out = append(out, e.Cause)
	}
	return out
}

// Info describes the engine's host.
type Info struct {
	NCPU          int
	MemTotal      int64 // bytes
	ServerVersion string
}

// PortBinding publishes a container port on the host.
type PortBinding struct {
	// HostIP is the address to publish on. Empty means 127.0.0.1.
	HostIP        string
	HostPort      int
	ContainerPort int
	// Protocol is "tcp" or "udp". Empty means tcp.
	Protocol string
}

// VolumeMount mounts a named volume into the container.
type VolumeMount struct {
	Name string
	Path string
}

// ContainerConfig describes one replica to create.
type ContainerConfig struct {
	// Name is the container name, unique on the node.
	Name    string
	Image   string
	Command []string
	Env     map[string]string
	Ports   []PortBinding
	Volumes []VolumeMount
	// App, Replica and Generation become the hivemind.* labels.
	App        string
	Replica    int
	Generation int
}

// Labels returns the hivemind labels for the replica.
func (c ContainerConfig) Labels() map[string]string {
	return map[string]string{
		LabelApp:        c.App,
		LabelReplica:    fmt.Sprint(c.Replica),
		LabelGeneration: fmt.Sprint(c.Generation),
	}
}

// Container is the engine's view of one container. List fills only the
// fields the engine reports in its list output, so ExitCode, OOMKilled and
// RestartPolicy are zero there.
type Container struct {
	ID            string
	Name          string
	Image         string
	Labels        map[string]string
	State         string // created, running, paused, restarting, exited, dead or removing
	ExitCode      int
	OOMKilled     bool
	RestartPolicy string
}

// Running reports whether the container is running.
func (c Container) Running() bool { return c.State == "running" }

// LogOptions selects which log lines to return.
type LogOptions struct {
	// Follow keeps the stream open until the container exits or ctx is cancelled.
	Follow bool
	// Tail is the number of lines from the end of the log. Zero means all.
	Tail int
}

// Runtime runs containers on one node. Implementations report failures as
// *Error, so callers can match them with errors.Is against ErrNotFound,
// ErrConflict, ErrUnavailable and ErrInvalid.
type Runtime interface {
	// Info returns the host's CPU count and memory.
	Info(ctx context.Context) (Info, error)
	// Pull fetches an image. The reference is normalised with spec.ParseImage,
	// so "nginx" is pulled as docker.io/library/nginx:latest.
	Pull(ctx context.Context, image string) error
	// Create makes a container without starting it, and returns its ID. The
	// restart policy is always "no": the agent is the supervisor.
	Create(ctx context.Context, cfg ContainerConfig) (string, error)
	// Start starts a created or stopped container. Starting a running
	// container is not an error.
	Start(ctx context.Context, id string) error
	// Stop sends SIGTERM and waits up to timeout before SIGKILL. Stopping a
	// stopped container is not an error.
	Stop(ctx context.Context, id string, timeout time.Duration) error
	// Remove deletes a container. Named volumes are kept.
	Remove(ctx context.Context, id string, force bool) error
	// Inspect returns one container.
	Inspect(ctx context.Context, id string) (Container, error)
	// List returns the containers whose labels match every key and value in
	// labels. A key with an empty value matches any value. Stopped containers
	// are included.
	List(ctx context.Context, labels map[string]string) ([]Container, error)
	// Logs writes the container's stdout to stdout and its stderr to stderr.
	// Each may be the same writer. With opts.Follow it returns when the
	// container exits, or with ctx.Err() when ctx is cancelled.
	Logs(ctx context.Context, id string, opts LogOptions, stdout, stderr io.Writer) error
}

// containerNameRE is the engine's rule for container names.
var containerNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

// Prepare returns a copy of cfg with defaults filled in and every field
// checked. It applies the spec rules for env, ports, volumes and commands, so
// every implementation accepts and rejects the same configurations. Errors
// wrap ErrInvalid.
func Prepare(cfg ContainerConfig) (ContainerConfig, error) {
	if !containerNameRE.MatchString(cfg.Name) {
		return cfg, fmt.Errorf("%w: container name %q: use letters, digits, dots, dashes and underscores, starting with a letter or digit", ErrInvalid, cfg.Name)
	}
	if cfg.Replica < 0 || cfg.Generation < 0 {
		return cfg, fmt.Errorf("%w: replica and generation must not be negative", ErrInvalid)
	}
	img, err := spec.ParseImage(cfg.Image)
	if err != nil {
		return cfg, fmt.Errorf("%w: image: %v", ErrInvalid, err)
	}
	cfg.Image = img.String()

	a := spec.App{Name: cfg.App, Image: cfg.Image, Replicas: 1, Command: cfg.Command, Env: cfg.Env}
	for _, p := range cfg.Ports {
		a.Ports = append(a.Ports, spec.Port{Bind: p.HostIP, Host: p.HostPort, Container: p.ContainerPort, Protocol: p.Protocol})
	}
	for _, v := range cfg.Volumes {
		a.Volumes = append(a.Volumes, spec.Volume{Name: v.Name, Path: v.Path})
	}
	a.Normalize()
	if err := a.Validate(); err != nil {
		return cfg, fmt.Errorf("%w: %v", ErrInvalid, err)
	}

	// Copy the slices before writing defaults back, so the caller's are untouched.
	cfg.Ports = slices.Clone(cfg.Ports)
	for i := range cfg.Ports {
		cfg.Ports[i].HostIP = a.Ports[i].Bind
		cfg.Ports[i].Protocol = a.Ports[i].Protocol
	}
	cfg.Volumes = slices.Clone(cfg.Volumes)
	cfg.Command = slices.Clone(cfg.Command)
	return cfg, nil
}
