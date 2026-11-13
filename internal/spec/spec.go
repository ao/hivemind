// Package spec defines an application as users describe it, and validates it.
package spec

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// App is the desired state of one application.
type App struct {
	Name      string            `json:"name" yaml:"name"`
	Image     string            `json:"image" yaml:"image"`
	Replicas  int               `json:"replicas" yaml:"replicas"`
	Command   []string          `json:"command,omitempty" yaml:"command,omitempty"`
	Env       map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	Ports     []Port            `json:"ports,omitempty" yaml:"ports,omitempty"`
	Volumes   []Volume          `json:"volumes,omitempty" yaml:"volumes,omitempty"`
	Resources Resources         `json:"resources,omitempty" yaml:"resources,omitempty"`
	Health    *Health           `json:"health,omitempty" yaml:"health,omitempty"`
}

// Port publishes a container port on the node.
type Port struct {
	// Bind is the host address to publish on. Empty means 127.0.0.1.
	Bind      string `json:"bind,omitempty" yaml:"bind,omitempty"`
	Host      int    `json:"host" yaml:"host"`
	Container int    `json:"container" yaml:"container"`
	Protocol  string `json:"protocol,omitempty" yaml:"protocol,omitempty"`
}

// Volume mounts a named volume, local to one node, into the container.
type Volume struct {
	Name string `json:"name" yaml:"name"`
	Path string `json:"path" yaml:"path"`
}

// Resources are what one replica reserves on its node, and its limits: CPU
// in CPUs and Memory in bytes. Zero means no reservation and no limit.
type Resources struct {
	CPU    float64  `json:"cpu,omitempty" yaml:"cpu,omitempty"`
	Memory ByteSize `json:"memory,omitempty" yaml:"memory,omitempty"`
}

// Health is a readiness and liveness check. Exactly one of HTTP, TCP and Exec is set.
type Health struct {
	HTTP        *HTTPCheck `json:"http,omitempty" yaml:"http,omitempty"`
	TCP         *TCPCheck  `json:"tcp,omitempty" yaml:"tcp,omitempty"`
	Exec        []string   `json:"exec,omitempty" yaml:"exec,omitempty"`
	Interval    Duration   `json:"interval,omitempty" yaml:"interval,omitempty"`
	Timeout     Duration   `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	Retries     int        `json:"retries,omitempty" yaml:"retries,omitempty"`
	StartPeriod Duration   `json:"start_period,omitempty" yaml:"start_period,omitempty"`
}

// HTTPCheck passes when GET http://<container>:<port><path> returns 2xx or 3xx.
type HTTPCheck struct {
	Path string `json:"path,omitempty" yaml:"path,omitempty"`
	Port int    `json:"port" yaml:"port"`
}

// TCPCheck passes when a TCP connection to the container port succeeds.
type TCPCheck struct {
	Port int `json:"port" yaml:"port"`
}

// Limits on what one spec may contain.
const (
	MaxReplicas  = 100
	maxNameLen   = 32
	maxImageLen  = 255
	maxArgs      = 64
	maxArgLen    = 4096
	maxEnv       = 128
	maxEnvBytes  = 64 << 10
	maxPorts     = 32
	maxVolumes   = 16
	maxCPU       = 1024
	maxMemory    = 1 << 40
	maxPathLen   = 1024
	DefaultBind  = "127.0.0.1"
	defaultProto = "tcp"
)

// The smallest limits a container engine accepts. A nonzero resources.cpu or
// resources.memory below these is rejected here, rather than by every create
// on the node.
const (
	// MinCPU is 0.01 CPUs. Docker turns a CPU limit into a CFS quota per
	// 100ms period, and the kernel's smallest quota is 1ms.
	MinCPU = 0.01
	// MinMemory is 6Mi, Docker's minimum memory limit (linuxMinMemory in the
	// daemon; "the minimum allowed value is 6m" in the docker run docs).
	MinMemory = 6 << 20
)

var (
	nameRE   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	envKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// ValidName reports whether s can name an app or a volume.
func ValidName(s string) bool {
	return len(s) <= maxNameLen && nameRE.MatchString(s)
}

// Normalize fills in defaults. Call it before Validate.
func (a *App) Normalize() {
	a.Name = strings.TrimSpace(a.Name)
	a.Image = strings.TrimSpace(a.Image)
	if a.Replicas == 0 {
		a.Replicas = 1
	}
	for i := range a.Ports {
		p := &a.Ports[i]
		if p.Bind == "" {
			p.Bind = DefaultBind
		}
		p.Protocol = strings.ToLower(p.Protocol)
		if p.Protocol == "" {
			p.Protocol = defaultProto
		}
	}
	for i := range a.Volumes {
		a.Volumes[i].Path = strings.TrimSpace(a.Volumes[i].Path)
	}
	if h := a.Health; h != nil {
		if h.Interval == 0 {
			h.Interval = Duration(10 * time.Second)
		}
		if h.Timeout == 0 {
			h.Timeout = Duration(2 * time.Second)
		}
		if h.Retries == 0 {
			h.Retries = 3
		}
		if h.HTTP != nil && h.HTTP.Path == "" {
			h.HTTP.Path = "/"
		}
	}
	if len(a.Env) == 0 {
		a.Env = nil
	}
}

// Validate returns every problem with the spec, joined, or nil.
func (a *App) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if !ValidName(a.Name) {
		add("name %q: use 1 to %d lowercase letters, digits and dashes, starting and ending with a letter or digit", a.Name, maxNameLen)
	}
	if _, err := ParseImage(a.Image); err != nil {
		add("image: %v", err)
	}
	if a.Replicas < 1 || a.Replicas > MaxReplicas {
		add("replicas %d: must be between 1 and %d", a.Replicas, MaxReplicas)
	}
	if len(a.Command) > maxArgs {
		add("command: at most %d arguments", maxArgs)
	}
	for i, arg := range a.Command {
		if len(arg) > maxArgLen || strings.ContainsRune(arg, 0) {
			add("command[%d]: at most %d bytes and no NUL", i, maxArgLen)
		}
	}
	if len(a.Env) > maxEnv {
		add("env: at most %d variables", maxEnv)
	}
	envBytes := 0
	for k, v := range a.Env {
		envBytes += len(k) + len(v)
		if !envKeyRE.MatchString(k) {
			add("env %q: names are letters, digits and underscores, not starting with a digit", k)
		}
		if strings.ContainsRune(v, 0) {
			add("env %q: value contains NUL", k)
		}
	}
	if envBytes > maxEnvBytes {
		add("env: at most %d bytes in total", maxEnvBytes)
	}

	if len(a.Ports) > maxPorts {
		add("ports: at most %d", maxPorts)
	}
	containerPorts := map[int]bool{}
	seen := map[string]bool{}
	for i, p := range a.Ports {
		if !validPort(p.Host) || !validPort(p.Container) {
			add("ports[%d]: host and container ports must be between 1 and 65535", i)
		}
		if p.Protocol != "tcp" && p.Protocol != "udp" {
			add("ports[%d]: protocol must be tcp or udp", i)
		}
		if !validBind(p.Bind) {
			add("ports[%d]: bind %q is not an IP address", i, p.Bind)
		}
		k := p.Key()
		if seen[k] {
			add("ports[%d]: host port %s listed twice", i, k)
		}
		seen[k] = true
		containerPorts[p.Container] = true
	}

	if len(a.Volumes) > maxVolumes {
		add("volumes: at most %d", maxVolumes)
	}
	if len(a.Volumes) > 0 && a.Replicas > 1 {
		add("volumes: an app with volumes can have only 1 replica, because volumes live on one node")
	}
	paths := map[string]bool{}
	names := map[string]bool{}
	for i, v := range a.Volumes {
		if !ValidName(v.Name) {
			add("volumes[%d]: name %q: use lowercase letters, digits and dashes", i, v.Name)
		}
		if names[v.Name] {
			add("volumes[%d]: volume %q listed twice", i, v.Name)
		}
		names[v.Name] = true
		if !validMountPath(v.Path) {
			add("volumes[%d]: path %q must be an absolute, clean path other than /", i, v.Path)
		}
		if paths[v.Path] {
			add("volumes[%d]: path %q listed twice", i, v.Path)
		}
		paths[v.Path] = true
	}

	if a.Resources.CPU < 0 || a.Resources.CPU > maxCPU || a.Resources.CPU != a.Resources.CPU {
		add("resources.cpu: must be between 0 and %d", maxCPU)
	} else if a.Resources.CPU > 0 && a.Resources.CPU < MinCPU {
		add("resources.cpu: must be 0 (no limit) or at least %g", MinCPU)
	}
	if a.Resources.Memory < 0 || a.Resources.Memory > maxMemory {
		add("resources.memory: must be between 0 and 1Ti")
	} else if a.Resources.Memory > 0 && a.Resources.Memory < MinMemory {
		add("resources.memory: must be 0 (no limit) or at least 6Mi")
	}

	if h := a.Health; h != nil {
		n := 0
		if h.HTTP != nil {
			n++
			if !validPort(h.HTTP.Port) {
				add("health.http.port: must be between 1 and 65535")
			}
			if !strings.HasPrefix(h.HTTP.Path, "/") || len(h.HTTP.Path) > maxPathLen || strings.ContainsAny(h.HTTP.Path, " \t\r\n#") {
				add("health.http.path %q: must start with / and contain no spaces", h.HTTP.Path)
			}
		}
		if h.TCP != nil {
			n++
			if !validPort(h.TCP.Port) {
				add("health.tcp.port: must be between 1 and 65535")
			}
		}
		if len(h.Exec) > 0 {
			n++
			if len(h.Exec) > maxArgs {
				add("health.exec: at most %d arguments", maxArgs)
			}
			for _, arg := range h.Exec {
				if len(arg) > maxArgLen || strings.ContainsRune(arg, 0) {
					add("health.exec: arguments are at most %d bytes and contain no NUL", maxArgLen)
					break
				}
			}
		}
		if n != 1 {
			add("health: set exactly one of http, tcp and exec")
		}
		if h.Interval.D() < time.Second || h.Interval.D() > time.Hour {
			add("health.interval: must be between 1s and 1h")
		}
		if h.Timeout.D() < 100*time.Millisecond || h.Timeout.D() > h.Interval.D() {
			add("health.timeout: must be at least 100ms and no longer than the interval")
		}
		if h.Retries < 1 || h.Retries > 10 {
			add("health.retries: must be between 1 and 10")
		}
		if h.StartPeriod < 0 || h.StartPeriod.D() > time.Hour {
			add("health.start_period: must be between 0 and 1h")
		}
	}
	return errors.Join(errs...)
}

// Key identifies a published host port, for conflict checks: "8080/tcp".
func (p Port) Key() string { return strconv.Itoa(p.Host) + "/" + p.Protocol }

// Template returns the parts of the spec that define a replica, as canonical JSON.
// Two specs with equal templates run identical containers; replicas are not part of it.
func (a App) Template() string {
	t := a
	t.Replicas = 0
	t.Name = ""
	b, _ := json.Marshal(t) // map keys are sorted by encoding/json
	return string(b)
}

// HostPorts returns the keys of the host ports the app publishes, sorted.
func (a App) HostPorts() []string {
	out := make([]string, 0, len(a.Ports))
	for _, p := range a.Ports {
		out = append(out, p.Key())
	}
	sort.Strings(out)
	return out
}

func validPort(p int) bool { return p >= 1 && p <= 65535 }

func validBind(s string) bool {
	a, err := netip.ParseAddr(s)
	return err == nil && a.Zone() == ""
}

func validMountPath(p string) bool {
	return len(p) <= maxPathLen && strings.HasPrefix(p, "/") && p != "/" &&
		path.Clean(p) == p && !strings.ContainsAny(p, "\x00:")
}
