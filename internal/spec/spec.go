// Package spec defines an application as users describe it, and validates it.
package spec

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"path"
	"regexp"
	"slices"
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
	Routes    []Route           `json:"routes,omitempty" yaml:"routes,omitempty"`
}

// Route sends HTTPS requests for Host, or for the paths under Path on
// Host, through the routers, to Port in the app's containers. Host is an
// exact DNS name: no wildcard and no IP address. A host and path belong to
// one app at a time; one host can have routes with different paths in
// different apps.
type Route struct {
	Host string `json:"host" yaml:"host"`
	// Path is a path prefix: the route takes the requests whose path is
	// Path or starts with Path and "/". Empty, or "/", which Normalize
	// makes empty, is the whole host: every path no other route takes.
	// ValidatePath says what a path may hold.
	Path string `json:"path,omitempty" yaml:"path,omitempty"`
	// Port is the container port, as in Port.Container.
	Port int `json:"port" yaml:"port"`
	// Protocol is what the routers speak to the replicas: "http", the
	// default, for HTTP/1.1, or "h2c", for HTTP/2 without TLS, with prior
	// knowledge, which gRPC needs. Normalize makes "http" empty, so a spec
	// that writes it has the template of one that does not.
	Protocol string `json:"protocol,omitempty" yaml:"protocol,omitempty"`
}

// The protocols a route can speak to its replicas. RouteHTTP is stored as
// "" (see Route.Protocol).
const (
	RouteHTTP = "http"
	RouteH2C  = "h2c"
)

// ProtocolName is the route's protocol as people write it: "http" or "h2c".
func (r Route) ProtocolName() string {
	if r.Protocol == "" {
		return RouteHTTP
	}
	return r.Protocol
}

// ValidateRouteProtocol checks a normalised route protocol: "" (HTTP/1.1)
// or RouteH2C.
func ValidateRouteProtocol(p string) error {
	if p == "" || p == RouteH2C {
		return nil
	}
	return fmt.Errorf("protocol %q: use http or h2c", truncHost(p))
}

// NormalizeRouteProtocol returns a route protocol in the form Validate
// expects: lower case, and "" for http.
func NormalizeRouteProtocol(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	if p == RouteHTTP {
		return ""
	}
	return p
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

// Limits on routes. MaxRoutes is the most routes one app may have, and
// MaxRoutePathLen the longest path a route may have.
const (
	MaxRoutes       = 16
	maxHostLen      = 253
	MaxRoutePathLen = 256
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
	for i := range a.Routes {
		a.Routes[i].Host = NormalizeHost(a.Routes[i].Host)
		a.Routes[i].Path = NormalizePath(a.Routes[i].Path)
		a.Routes[i].Protocol = NormalizeRouteProtocol(a.Routes[i].Protocol)
	}
	if len(a.Routes) == 0 {
		a.Routes = nil
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
		switch {
		case IsHostPath(v.Name):
			add("volumes[%d]: name %q is a path on the host, and bind mounts are not supported: hivemind mounts named volumes only; give the volume a name", i, v.Name)
		case !ValidName(v.Name):
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

	if len(a.Routes) > MaxRoutes {
		add("routes: at most %d", MaxRoutes)
	}
	routes := map[RouteKey]bool{}
	for i, r := range a.Routes {
		if err := ValidateHost(r.Host); err != nil {
			add("routes[%d]: %v", i, err)
		}
		if err := ValidatePath(r.Path); err != nil {
			add("routes[%d]: %v", i, err)
		}
		if !validPort(r.Port) {
			add("routes[%d]: port must be a container port between 1 and 65535", i)
		}
		if err := ValidateRouteProtocol(r.Protocol); err != nil {
			add("routes[%d]: %v", i, err)
		}
		switch k := r.Key(); {
		case routes[k] && r.Path == "":
			add("routes[%d]: host %q listed twice", i, r.Host)
		case routes[k]:
			add("routes[%d]: host %q with path %q listed twice", i, r.Host, r.Path)
		default:
			routes[k] = true
		}
	}
	return errors.Join(errs...)
}

// RouteKey is what a route is known by: its host and path. Two routes with
// one key cannot both exist, in one app or in two.
type RouteKey struct {
	Host, Path string
}

// Key returns the route's key.
func (r Route) Key() RouteKey { return RouteKey{r.Host, r.Path} }

// String is the key as people read it: the host, then the path, if any, as
// in www.example.com/api.
func (k RouteKey) String() string { return k.Host + k.Path }

// NormalizePath returns a route path in the form Validate expects: "" for
// the whole host, which may be written as "" or "/", and otherwise the path
// without one trailing slash, so "/api/" is "/api", which takes /api/ too.
// Nothing else is changed: a path that needs more is refused.
func NormalizePath(p string) string {
	if p == "/" {
		return ""
	}
	if len(p) > 1 && strings.HasSuffix(p, "/") {
		return p[:len(p)-1]
	}
	return p
}

// ValidatePath checks that p, normalised, can be a route's path: "" for the
// whole host, or a prefix of at most MaxRoutePathLen bytes that starts with
// "/" and has one or more segments, separated by "/". Each segment has one
// or more of the letters, digits and the characters - . _ ~ ! $ & ' ( ) * +
// , = : @, which RFC 3986 allows in a path segment as they are, and is not
// "." or "..". Prefixes are compared as they are, so case matters.
//
// The router matches a prefix on the request's decoded path, so a prefix is
// written decoded too, and "%" is refused rather than read: "/a%2Fb" would
// be one segment to some apps and two to others. "?" and "#" end a path, a
// "." or ".." segment means another path, and an empty one ("//") is
// cleaned away from requests before they are matched. ";" is refused
// because servlet containers read what follows it in a segment as
// parameters, and strip it before they route. Spaces, control characters,
// backslashes, the characters RFC 3986 does not allow in a path (" < > [ ]
// ^ ` { | }) and anything outside ASCII are refused: a letter outside ASCII
// has more than one spelling in Unicode, which would make two prefixes of
// one name.
func ValidatePath(p string) error {
	switch {
	case p == "":
		return nil
	case len(p) > MaxRoutePathLen:
		return fmt.Errorf("path is longer than %d characters", MaxRoutePathLen)
	case p[0] != '/':
		return fmt.Errorf("path %q must start with /", truncHost(p))
	}
	for seg := range strings.SplitSeq(p[1:], "/") {
		switch seg {
		case "":
			return fmt.Errorf("path %q has an empty segment (//); write each / once", truncHost(p))
		case ".", "..":
			return fmt.Errorf("path %q has a %s segment; write the path it means", truncHost(p), seg)
		}
		for i := 0; i < len(seg); i++ {
			if !pathChar(seg[i]) {
				return fmt.Errorf("path %q has %s, which a route path may not hold: use letters, digits and - . _ ~ ! $ & ' ( ) * + , = : @ between the slashes", truncHost(p), quoteChar(seg[i]))
			}
		}
	}
	return nil
}

// pathChar reports whether c may be in a segment of a route path: an
// unreserved character or a sub-delimiter of RFC 3986, but ";", or ":" or
// "@".
func pathChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte("-._~!$&'()*+,=:@", c) >= 0
}

// quoteChar names a byte for a message: a printable character quoted, any
// other byte in hex.
func quoteChar(c byte) string {
	switch {
	case c == ' ':
		return "a space"
	case c > ' ' && c < 0x7f:
		return strconv.QuoteRune(rune(c))
	}
	return fmt.Sprintf("byte 0x%02x", c)
}

// NormalizeHost returns a route host in the form Validate expects: trimmed,
// lowercase, and without one trailing dot.
func NormalizeHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	return strings.TrimSuffix(h, ".")
}

// ValidateHost checks that h, normalised, can be a route host: a DNS name of
// at least two labels, each of 1 to 63 letters, digits and dashes, not
// starting or ending with a dash, at most 253 characters in all. Wildcards,
// IP addresses, underscores and a numeric top-level label are refused: a
// certificate authority issues for none of them over HTTP-01.
func ValidateHost(h string) error {
	switch {
	case h == "":
		return errors.New("host is required")
	case len(h) > maxHostLen:
		return fmt.Errorf("host is longer than %d characters", maxHostLen)
	case strings.Contains(h, "*"):
		return fmt.Errorf("host %q: wildcards are not supported; list each name", truncHost(h))
	}
	if _, err := netip.ParseAddr(strings.Trim(h, "[]")); err == nil {
		return fmt.Errorf("host %q is an IP address; routes take DNS names", h)
	}
	labels := strings.Split(h, ".")
	if len(labels) < 2 {
		return fmt.Errorf("host %q needs at least two labels, such as www.example.com", truncHost(h))
	}
	for _, l := range labels {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return fmt.Errorf("host %q: each label is 1 to 63 characters and does not start or end with a dash", truncHost(h))
		}
		for _, c := range l {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return fmt.Errorf("host %q: use lowercase letters, digits, dashes and dots", truncHost(h))
			}
		}
	}
	if tld := labels[len(labels)-1]; strings.Trim(tld, "0123456789") == "" {
		return fmt.Errorf("host %q: the last label must not be all digits", truncHost(h))
	}
	return nil
}

// RouteHosts returns the distinct hosts the app routes, in spec order.
func (a App) RouteHosts() []string {
	out := make([]string, 0, len(a.Routes))
	for _, r := range a.Routes {
		if !slices.Contains(out, r.Host) {
			out = append(out, r.Host)
		}
	}
	return out
}

// HasPathRoutes reports whether a route of the app has a path.
func (a App) HasPathRoutes() bool {
	return slices.ContainsFunc(a.Routes, func(r Route) bool { return r.Path != "" })
}

// RoutePorts returns the distinct container ports the app's routes name,
// sorted.
func (a App) RoutePorts() []int {
	seen := map[int]bool{}
	var out []int
	for _, r := range a.Routes {
		if !seen[r.Port] {
			seen[r.Port] = true
			out = append(out, r.Port)
		}
	}
	sort.Ints(out)
	return out
}

func truncHost(h string) string {
	if len(h) > 64 {
		return h[:64] + "..."
	}
	return h
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

// IsHostPath reports whether a volume's name is written as a path on the
// host, as a bind mount's source is in docker run -v and Compose: it starts
// with /, . or ~, or has a / in it.
func IsHostPath(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "~") || strings.Contains(name, "/")
}

func validMountPath(p string) bool {
	return len(p) <= maxPathLen && strings.HasPrefix(p, "/") && p != "/" &&
		path.Clean(p) == p && !strings.ContainsAny(p, "\x00:")
}
