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
	"strconv"
	"time"

	"github.com/ao/hivemind/internal/spec"
)

// Labels that record what hivemind started. The agent rebuilds its view of a
// node from these labels after a restart.
const (
	LabelApp        = "hivemind.app"
	LabelReplica    = "hivemind.replica"
	LabelGeneration = "hivemind.generation"
	// LabelRouted is "true" on the container of a routed app, and "false"
	// on every other. The agent keeps a routed container running for a
	// short drain after it leaves the routing table, before it stops it. It
	// is set either way because Docker merges the image's labels into the
	// container's, and a label the container sets wins: an image with
	// LABEL hivemind.routed=true would otherwise give its app the drain.
	LabelRouted = "hivemind.routed"
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
	// ErrUnauthorized means the registry refused a pull: it wants a
	// credential and got none, or refused the one sent. Docker Hub answers
	// a private repository and a missing one alike ("pull access denied"),
	// so such an error matches ErrNotFound as well.
	ErrUnauthorized = errors.New("the registry refused the pull")
)

// RegistryAuth is the credential a pull sends to the image's registry.
type RegistryAuth struct {
	Username string
	Password string
	// ServerAddress is the registry's host, and port if it has one,
	// without a scheme.
	ServerAddress string
}

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
	// LogDriver is the engine's default log driver, such as "json-file".
	LogDriver string
}

// PortBinding publishes a container port on the host.
type PortBinding struct {
	// HostIP is the address to publish on. Empty means 127.0.0.1.
	HostIP        string
	HostPort      int
	ContainerPort int
	// Protocol is "tcp" or "udp". Empty means tcp.
	Protocol string
	// Ephemeral asks the engine for a free host port. HostPort must then be
	// 0; Container.Ports reports the port the engine chose. Routed apps
	// publish their route ports this way.
	Ephemeral bool
}

// VolumeMount mounts one of the app's volumes into the container. Name is
// the name in the app spec; the engine's volume is VolumeName(App, Name).
type VolumeMount struct {
	Name string
	Path string
}

// LabelVolume is set on each volume hivemind creates, with the volume's name
// in the spec. LabelApp is set on it too.
const LabelVolume = "hivemind.volume"

// VolumeName is the engine's name for an app's volume:
// "hivemind-<app>_<volume>". Scoping the name to the app keeps two apps that
// use the same volume name on one node from sharing data.
//
// App and volume names are lowercase letters, digits and dashes, at most 32
// characters (spec.ValidName). The underscore can't occur in either, so no
// two pairs give the same name: app "a-b" with volume "c" is hivemind-a-b_c,
// and app "a" with volume "b-c" is hivemind-a_b-c. The result starts with a
// letter, uses only characters Docker allows in a volume name
// ([a-zA-Z0-9][a-zA-Z0-9_.-]+), and is at most 74 characters.
func VolumeName(app, volume string) string {
	return "hivemind-" + app + "_" + volume
}

// VolumeLabels returns the labels for an app's volume.
func VolumeLabels(app, volume string) map[string]string {
	return map[string]string{LabelApp: app, LabelVolume: volume}
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
	// CPU is the limit in CPUs, such as 0.5. Zero means no limit.
	CPU float64
	// Memory is the limit in bytes. Zero means no limit. Swap is not allowed
	// on top of it.
	Memory int64
	// App, Replica and Generation become the hivemind.* labels.
	App        string
	Replica    int
	Generation int
	// Routed sets LabelRouted.
	Routed bool
}

// Labels returns the hivemind labels for the replica. Each one is set,
// so that none comes from the image.
func (c ContainerConfig) Labels() map[string]string {
	return map[string]string{
		LabelApp:        c.App,
		LabelReplica:    fmt.Sprint(c.Replica),
		LabelGeneration: fmt.Sprint(c.Generation),
		LabelRouted:     strconv.FormatBool(c.Routed),
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
	// IPAddress is the container's address on its Docker network. Empty when
	// it has none, as for a container that was never started.
	IPAddress string
	// StartedAt and FinishedAt are zero when the container has not started
	// or has not stopped. Inspect fills them; List does not.
	StartedAt  time.Time
	FinishedAt time.Time
	// Ports are the host ports the engine has published for the container,
	// with the port it chose for an ephemeral binding. A container that is
	// not running has none. List and Inspect fill them.
	Ports []PortBinding
}

// PublishedPort returns the host port that publishes containerPort/proto on
// hostIP, or 0 when there is none.
func (c Container) PublishedPort(hostIP string, containerPort int, proto string) int {
	for _, p := range c.Ports {
		if p.ContainerPort == containerPort && p.Protocol == proto && p.HostIP == hostIP && p.HostPort > 0 {
			return p.HostPort
		}
	}
	return 0
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
	// so "nginx" is pulled as docker.io/library/nginx:latest. A non-nil auth
	// is sent to the image's registry; nil pulls without a credential. A
	// refusal by the registry matches ErrUnauthorized. No error contains
	// the password.
	Pull(ctx context.Context, image string, auth *RegistryAuth) error
	// Create makes a container without starting it, and returns its ID. The
	// restart policy is always "no": the agent is the supervisor. Each volume
	// is mounted as VolumeName(cfg.App, v.Name), created with VolumeLabels
	// if it does not exist yet.
	Create(ctx context.Context, cfg ContainerConfig) (string, error)
	// Start starts a created or stopped container. Starting a running
	// container is not an error.
	Start(ctx context.Context, id string) error
	// Stop sends SIGTERM and waits up to timeout before SIGKILL. Stopping a
	// stopped container is not an error.
	Stop(ctx context.Context, id string, timeout time.Duration) error
	// Remove deletes a container and its anonymous volumes, the ones its
	// image declares. Named volumes are kept: no Runtime method deletes one.
	Remove(ctx context.Context, id string, force bool) error
	// Inspect returns one container.
	Inspect(ctx context.Context, id string) (Container, error)
	// List returns the containers whose labels match every key and value in
	// labels. A key with an empty value matches any value. Stopped containers
	// are included.
	List(ctx context.Context, labels map[string]string) ([]Container, error)
	// Exec runs cmd in a running container and returns its exit code. It
	// returns ctx.Err() when ctx ends first; the process is then left to the
	// engine, which ends it with the container. A non-running container is
	// an error wrapping ErrConflict.
	Exec(ctx context.Context, id string, cmd []string) (int, error)
	// Logs writes the container's stdout to stdout and its stderr to stderr.
	// Each may be the same writer. With opts.Follow it returns when the
	// container exits, or with ctx.Err() when ctx is cancelled.
	Logs(ctx context.Context, id string, opts LogOptions, stdout, stderr io.Writer) error
}

// containerNameRE is the engine's rule for container names.
var containerNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

// Prepare returns a copy of cfg with defaults filled in and every field
// checked. It applies the spec rules for env, ports, volumes, resources and
// commands, so
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

	a := spec.App{Name: cfg.App, Image: cfg.Image, Replicas: 1, Command: cfg.Command, Env: cfg.Env,
		Resources: spec.Resources{CPU: cfg.CPU, Memory: spec.ByteSize(cfg.Memory)}}
	// Fixed host ports follow the spec's rules. An ephemeral binding has no
	// host port, so its address and protocol are checked on their own: a
	// spec port with host port 1 stands in for it.
	var fixed, ephemeral []int // indexes into cfg.Ports
	for i, p := range cfg.Ports {
		if p.Ephemeral {
			if p.HostPort != 0 {
				return cfg, fmt.Errorf("%w: ports[%d]: an ephemeral binding has no host port, got %d", ErrInvalid, i, p.HostPort)
			}
			ephemeral = append(ephemeral, i)
			continue
		}
		fixed = append(fixed, i)
		a.Ports = append(a.Ports, spec.Port{Bind: p.HostIP, Host: p.HostPort, Container: p.ContainerPort, Protocol: p.Protocol})
	}
	for _, v := range cfg.Volumes {
		a.Volumes = append(a.Volumes, spec.Volume{Name: v.Name, Path: v.Path})
	}
	a.Normalize()
	if err := a.Validate(); err != nil {
		return cfg, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	eph := make([]spec.Port, 0, len(ephemeral))
	for _, i := range ephemeral {
		p := cfg.Ports[i]
		one := spec.App{Name: "x", Image: "x", Ports: []spec.Port{{Bind: p.HostIP, Host: 1, Container: p.ContainerPort, Protocol: p.Protocol}}}
		one.Normalize()
		if err := one.Validate(); err != nil {
			return cfg, fmt.Errorf("%w: ports[%d]: %v", ErrInvalid, i, err)
		}
		eph = append(eph, one.Ports[0])
	}

	// Copy the slices before writing defaults back, so the caller's are untouched.
	cfg.Ports = slices.Clone(cfg.Ports)
	for j, i := range fixed {
		cfg.Ports[i].HostIP = a.Ports[j].Bind
		cfg.Ports[i].Protocol = a.Ports[j].Protocol
	}
	for j, i := range ephemeral {
		cfg.Ports[i].HostIP = eph[j].Bind
		cfg.Ports[i].Protocol = eph[j].Protocol
	}
	cfg.Volumes = slices.Clone(cfg.Volumes)
	cfg.Command = slices.Clone(cfg.Command)
	return cfg, nil
}
