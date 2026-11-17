// Package nodeapi holds the messages of the node API, which the server and a
// worker exchange over the cluster listener, and the client that a worker
// uses. The server side lives in internal/cluster.
//
// Every message is JSON. A decoder rejects unknown fields, trailing data and
// bodies over a fixed limit, and Validate checks the fields the store would
// otherwise trust. Routes and their auth are described in internal/cluster.
package nodeapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ao/hivemind/internal/spec"
	"github.com/ao/hivemind/internal/store"
)

// Routes. Node routes are on the mTLS listener and carry the node ID in the
// path. The join route is on the join listener and needs no client
// certificate.
const (
	Prefix   = "/node/v1"
	JoinPath = "/join"
)

// Body limits, in bytes.
const (
	MaxRegisterBytes    = 4 << 10
	MaxJoinBytes        = 16 << 10
	MaxStatusBytes      = 1 << 20
	MaxAssignmentsBytes = 16 << 20
	MaxRoutesBytes      = 8 << 20
	// MaxTLSRequestBytes fits a value of store.MaxTLSValueLen in base64,
	// with room for the key and the JSON around it.
	MaxTLSRequestBytes = 256 << 10
)

// Field limits.
const (
	maxNameLen      = 253
	maxContainers   = 10000
	maxStateLen     = 64
	maxErrorLen     = 4096
	maxCPU          = 1 << 20
	maxMemory       = 1 << 50
	maxGeneration   = 1 << 31
	maxReplicaIndex = 1 << 20
	maxContainerLen = 128
	maxRoutePorts   = 64
	maxRoutes       = 10000
	maxEndpoints    = 1000
	maxTLSKeys      = 10000
	maxTLSOwnerLen  = 64
	maxTLSTTL       = int64(time.Hour / time.Millisecond)
)

// ErrInvalid means a message did not decode or validate. ErrTooLarge means it
// was over the limit given to Decode.
var (
	ErrInvalid  = errors.New("invalid node API message")
	ErrTooLarge = errors.New("node API message is too large")
)

// RegisterPath is the route a node calls once to report its capacity.
func RegisterPath(id string) string { return Prefix + "/nodes/" + id + "/register" }

// AssignmentsPath is the route a node reads its assignments from.
func AssignmentsPath(id string) string { return Prefix + "/nodes/" + id + "/assignments" }

// StatusPath is the route a node reports its containers to. A report renews
// the node's lease. A GET on the same path returns what the node last reported.
func StatusPath(id string) string { return Prefix + "/nodes/" + id + "/status" }

// LeasePath is the route a node renews its lease on, with an empty POST,
// between status reports.
func LeasePath(id string) string { return Prefix + "/nodes/" + id + "/lease" }

// RoutesPath is the route a node's router reads the routing table from.
func RoutesPath(id string) string { return Prefix + "/nodes/" + id + "/routes" }

// TLSPath is the route of one TLS storage operation, such as load or lock.
func TLSPath(id, op string) string { return Prefix + "/nodes/" + id + "/tls/" + op }

// TLS storage operations: the methods of certmagic's Storage and Locker.
const (
	TLSLoad   = "load"
	TLSStore  = "store"
	TLSDelete = "delete"
	TLSExists = "exists"
	TLSList   = "list"
	TLSStat   = "stat"
	TLSLock   = "lock"
	TLSRenew  = "renew"
	TLSUnlock = "unlock"
)

// TLSOps lists every TLS storage operation.
var TLSOps = []string{TLSLoad, TLSStore, TLSDelete, TLSExists, TLSList, TLSStat, TLSLock, TLSRenew, TLSUnlock}

// Register is the body of POST register. Address is the node's private
// address, where its routed containers publish their ports: the local
// address of the worker's connection to the server. A worker older than 2.2
// sends none, and takes no routed replicas.
type Register struct {
	Name    string `json:"name"`
	CPU     int    `json:"cpu"`
	Memory  int64  `json:"memory"`
	Address string `json:"address,omitempty"`
}

// RoutePort is a container port of a routed container and the host port it
// is published on.
type RoutePort struct {
	Port     int `json:"port"`
	HostPort int `json:"host_port"`
}

// Route is one host of the routing table and the address:port of each
// replica that takes its traffic now.
type Route struct {
	Host      string   `json:"host"`
	Endpoints []string `json:"endpoints"`
}

// RouteTable is the reply to GET routes.
type RouteTable struct {
	Routes []Route `json:"routes"`
}

// TLSRequest is the body of every TLS storage operation. Each one reads the
// fields it needs: Key for the storage methods (a prefix for list), Value
// for store, Recursive for list, and Name, Owner and TTLMillis for the
// locks. Owner is a token the node chooses for one hold of a lock; the
// server stores it with the node's ID, so a node can only renew or release
// its own holds.
type TLSRequest struct {
	Key       string `json:"key,omitempty"`
	Value     []byte `json:"value,omitempty"`
	Recursive bool   `json:"recursive,omitempty"`
	Name      string `json:"name,omitempty"`
	Owner     string `json:"owner,omitempty"`
	TTLMillis int64  `json:"ttl_ms,omitempty"`
}

// TLSReply is the reply to a TLS storage operation. Found is false for a
// load, stat or list of a key that does not exist.
type TLSReply struct {
	Found    bool        `json:"found"`
	Value    []byte      `json:"value,omitempty"`
	Exists   bool        `json:"exists,omitempty"`
	Keys     []string    `json:"keys,omitempty"`
	Info     *TLSKeyInfo `json:"info,omitempty"`
	Acquired bool        `json:"acquired,omitempty"`
}

// TLSKeyInfo is the reply to stat.
type TLSKeyInfo struct {
	Key      string    `json:"key"`
	Modified time.Time `json:"modified"`
	Size     int64     `json:"size"`
	Terminal bool      `json:"terminal"`
}

// Assignment is one replica placed on a node, with the spec to run it from.
// RegistryAuth is the credential for the registry of the spec's image, sent
// only to a node that asks for credentials (RegistryAuthParam), and only
// when one is stored and the replica is to run. A 2.1 worker's decoder
// refuses unknown fields, and so the whole list, if any assignment had one;
// it does not ask, and gets none.
type Assignment struct {
	App           string        `json:"app"`
	Index         int           `json:"index"`
	NodeID        string        `json:"node_id"`
	Generation    int           `json:"generation"`
	Desired       string        `json:"desired"`
	PendingReason string        `json:"pending_reason,omitempty"`
	Spec          spec.App      `json:"spec"`
	RegistryAuth  *RegistryAuth `json:"registry_auth,omitempty"`
}

// RegistryAuth is a registry credential in an assignment.
type RegistryAuth struct {
	Host     string `json:"host"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// RegistryAuthParam is the query parameter a node adds to GET assignments,
// with the value "1", to say it takes registry credentials (2.2 and later).
// A server older than 2.2 ignores it.
const RegistryAuthParam = "registry_auth"

// AssignmentList is the reply to GET assignments.
type AssignmentList struct {
	Assignments []Assignment `json:"assignments"`
}

// Container is one container as the node last saw it. It mirrors
// store.Status without the node ID, which the server takes from the path.
type Container struct {
	Name         string      `json:"name"`
	ContainerID  string      `json:"container_id"`
	App          string      `json:"app"`
	Replica      int         `json:"replica"`
	Generation   int         `json:"generation"`
	State        string      `json:"state"`
	Error        string      `json:"error,omitempty"`
	RestartCount int         `json:"restart_count"`
	Health       string      `json:"health,omitempty"`
	Ready        bool        `json:"ready"`
	CrashLooping bool        `json:"crash_looping"`
	NextRestart  time.Time   `json:"next_restart"`
	RoutePorts   []RoutePort `json:"route_ports,omitempty"`
}

// StatusReport is the body of POST status, and the reply to GET status. A
// report replaces everything the node reported before.
type StatusReport struct {
	Containers []Container `json:"containers"`
}

// JoinRequest is the body of POST join. Token is the join token. CSR is a DER
// certificate request for the node's own key; the server takes only the key
// from it.
type JoinRequest struct {
	Token string `json:"token"`
	CSR   []byte `json:"csr"`
}

// JoinReply is the reply to POST join. CertPEM is the node's certificate and
// CAPEM is the cluster CA. The joining node pins the CA by the fingerprint in
// its token before it trusts either.
type JoinReply struct {
	NodeID  string `json:"node_id"`
	CertPEM string `json:"cert_pem"`
	CAPEM   string `json:"ca_pem"`
}

// ErrorBody is every error reply on the node API and the join route.
type ErrorBody struct {
	Error string `json:"error"`
}

// Decode reads exactly one JSON value from r into v. It reads at most limit
// bytes; a longer body is ErrTooLarge. Unknown fields, an empty body and data
// after the value are ErrInvalid.
func Decode(r io.Reader, limit int64, v any) error {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return fmt.Errorf("%w: reading body: %v", ErrInvalid, err)
	}
	if int64(len(data)) > limit {
		return ErrTooLarge
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return fmt.Errorf("%w: body is empty", ErrInvalid)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: data after the JSON value", ErrInvalid)
	}
	return nil
}

// Validate checks a registration.
func (g Register) Validate() error {
	switch {
	case len(g.Name) > maxNameLen || hasControl(g.Name):
		return fmt.Errorf("%w: node name must be at most %d printable characters", ErrInvalid, maxNameLen)
	case g.CPU < 0 || g.CPU > maxCPU:
		return fmt.Errorf("%w: cpu %d is out of range", ErrInvalid, g.CPU)
	case g.Memory < 0 || g.Memory > maxMemory:
		return fmt.Errorf("%w: memory %d is out of range", ErrInvalid, g.Memory)
	}
	if g.Address != "" {
		if err := ValidateNodeAddress(g.Address); err != nil {
			return err
		}
	}
	return nil
}

// ValidateNodeAddress checks a worker's private address: an IP address in
// canonical form, without a zone, that is not loopback, a wildcard or
// multicast. Routers on other nodes must be able to reach it.
func ValidateNodeAddress(s string) error {
	ip, err := netip.ParseAddr(s)
	switch {
	case err != nil || ip.String() != s:
		return fmt.Errorf("%w: node address %q is not an IP address in canonical form", ErrInvalid, truncate(s))
	case ip.Zone() != "":
		return fmt.Errorf("%w: node address %s has a zone", ErrInvalid, s)
	case ip.IsLoopback() || ip.Unmap().IsLoopback():
		return fmt.Errorf("%w: node address %s is loopback, which other nodes cannot reach", ErrInvalid, s)
	case ip.IsUnspecified():
		return fmt.Errorf("%w: node address %s is a wildcard", ErrInvalid, s)
	case ip.IsMulticast():
		return fmt.Errorf("%w: node address %s is multicast", ErrInvalid, s)
	}
	return nil
}

// Validate checks the routing table a server sent.
func (t RouteTable) Validate() error {
	if len(t.Routes) > maxRoutes {
		return fmt.Errorf("%w: more than %d routes", ErrInvalid, maxRoutes)
	}
	seen := make(map[string]bool, len(t.Routes))
	for _, r := range t.Routes {
		if err := spec.ValidateHost(r.Host); err != nil {
			return fmt.Errorf("%w: route: %v", ErrInvalid, err)
		}
		if seen[r.Host] {
			return fmt.Errorf("%w: route %s is listed twice", ErrInvalid, r.Host)
		}
		seen[r.Host] = true
		if len(r.Endpoints) > maxEndpoints {
			return fmt.Errorf("%w: route %s has more than %d endpoints", ErrInvalid, r.Host, maxEndpoints)
		}
		for _, e := range r.Endpoints {
			ap, err := netip.ParseAddrPort(e)
			if err != nil || ap.Port() == 0 || ap.Addr().Zone() != "" || ap.Addr().IsUnspecified() {
				return fmt.Errorf("%w: route %s has endpoint %q, which is not ip:port", ErrInvalid, r.Host, truncate(e))
			}
		}
	}
	return nil
}

// Validate checks a TLS storage request for op. The key rules are the
// store's: keys stay in certmagic's namespace.
func (r TLSRequest) Validate(op string) error {
	wrap := func(err error) error {
		if err == nil {
			return nil
		}
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	switch op {
	case TLSLoad, TLSStore:
		if err := store.ValidTLSKey(r.Key); err != nil {
			return wrap(err)
		}
		if len(r.Value) > store.MaxTLSValueLen {
			return fmt.Errorf("%w: value is more than %d bytes", ErrInvalid, store.MaxTLSValueLen)
		}
	case TLSDelete, TLSExists, TLSList, TLSStat:
		return wrap(store.ValidTLSPrefix(r.Key))
	case TLSLock, TLSRenew, TLSUnlock:
		if err := store.ValidTLSLockName(r.Name); err != nil {
			return wrap(err)
		}
		if r.Owner == "" || len(r.Owner) > maxTLSOwnerLen || strings.IndexFunc(r.Owner, func(c rune) bool {
			return !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_')
		}) >= 0 {
			return fmt.Errorf("%w: lock owner must be 1 to %d letters, digits, dashes and underscores", ErrInvalid, maxTLSOwnerLen)
		}
		if op != TLSUnlock && (r.TTLMillis < 1 || r.TTLMillis > maxTLSTTL) {
			return fmt.Errorf("%w: lock lifetime must be 1 ms to 1 h", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: unknown TLS storage operation %q", ErrInvalid, truncate(op))
	}
	return nil
}

// Validate checks a TLS storage reply.
func (r TLSReply) Validate() error {
	if len(r.Value) > store.MaxTLSValueLen {
		return fmt.Errorf("%w: value is more than %d bytes", ErrInvalid, store.MaxTLSValueLen)
	}
	if len(r.Keys) > maxTLSKeys {
		return fmt.Errorf("%w: more than %d keys", ErrInvalid, maxTLSKeys)
	}
	for _, k := range r.Keys {
		if err := store.ValidTLSKey(k); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
	}
	return nil
}

// Validate checks every assignment in the list.
func (l AssignmentList) Validate() error {
	for _, a := range l.Assignments {
		if !spec.ValidName(a.App) || a.Index < 0 || a.Index > maxReplicaIndex ||
			a.Generation < 1 || a.Generation > maxGeneration || a.Spec.Image == "" {
			return fmt.Errorf("%w: assignment for %q replica %d is malformed", ErrInvalid, a.App, a.Index)
		}
		switch store.Desired(a.Desired) {
		case store.Running, store.Removed:
		default:
			return fmt.Errorf("%w: assignment desired %q is not running or removed", ErrInvalid, a.Desired)
		}
		if a.RegistryAuth != nil {
			if err := a.RegistryAuth.validate(); err != nil {
				return fmt.Errorf("%w: assignment for %q replica %d: %v", ErrInvalid, a.App, a.Index, err)
			}
		}
	}
	return nil
}

// validate checks the form of a credential that came with an assignment: a
// host in canonical form, and a username and password. The size and
// character rules are the server's, which checks them when it stores the
// credential; a worker that refused a credential would refuse every
// assignment on its node with it. The agent sends a credential only for
// the registry of the image, and drops one for another. Errors hold neither
// the username nor the password.
func (r RegistryAuth) validate() error {
	if host, err := spec.ParseRegistryHost(r.Host); err != nil || host != r.Host {
		return errors.New("registry credential has a malformed host")
	}
	if r.Username == "" || r.Password == "" {
		return errors.New("registry credential has no username or no password")
	}
	return nil
}

// Validate checks every container in the report.
func (s StatusReport) Validate() error {
	if len(s.Containers) > maxContainers {
		return fmt.Errorf("%w: more than %d containers in one report", ErrInvalid, maxContainers)
	}
	for _, c := range s.Containers {
		switch {
		case c.Name == "" || len(c.Name) > maxContainerLen || hasControl(c.Name):
			return fmt.Errorf("%w: container name %q is malformed", ErrInvalid, truncate(c.Name))
		case c.Replica < 0 || c.Replica > maxReplicaIndex:
			return fmt.Errorf("%w: container %s has replica %d", ErrInvalid, c.Name, c.Replica)
		case c.Generation < 0 || c.Generation > maxGeneration:
			return fmt.Errorf("%w: container %s has generation %d", ErrInvalid, c.Name, c.Generation)
		case c.RestartCount < 0:
			return fmt.Errorf("%w: container %s has a negative restart count", ErrInvalid, c.Name)
		case len(c.State) > maxStateLen || len(c.Error) > maxErrorLen || len(c.ContainerID) > maxContainerLen:
			return fmt.Errorf("%w: container %s has a field that is too long", ErrInvalid, c.Name)
		}
		switch c.Health {
		case "", "starting", "healthy", "unhealthy":
		default:
			return fmt.Errorf("%w: container %s has health %q", ErrInvalid, c.Name, c.Health)
		}
		if len(c.RoutePorts) > maxRoutePorts {
			return fmt.Errorf("%w: container %s has more than %d route ports", ErrInvalid, c.Name, maxRoutePorts)
		}
		for _, p := range c.RoutePorts {
			if p.Port < 1 || p.Port > 65535 || p.HostPort < 1 || p.HostPort > 65535 {
				return fmt.Errorf("%w: container %s has route port %d on host port %d", ErrInvalid, c.Name, p.Port, p.HostPort)
			}
		}
	}
	return nil
}

// ToStore converts an assignment to the store's form.
func (a Assignment) ToStore() store.Assignment {
	out := store.Assignment{
		Replica: store.Replica{App: a.App, Index: a.Index, NodeID: a.NodeID, Generation: a.Generation,
			Desired: store.Desired(a.Desired), PendingReason: a.PendingReason},
		Spec: a.Spec,
	}
	if r := a.RegistryAuth; r != nil {
		out.Auth = &store.RegistryAuth{Host: r.Host, Username: r.Username, Password: r.Password}
	}
	return out
}

// FromStore converts store assignments to the wire form. With withAuth
// false, which is what a node that did not ask for credentials gets, no
// assignment carries one.
func FromStore(as []store.Assignment, withAuth bool) []Assignment {
	out := make([]Assignment, 0, len(as))
	for _, a := range as {
		w := Assignment{App: a.App, Index: a.Index, NodeID: a.NodeID, Generation: a.Generation,
			Desired: string(a.Desired), PendingReason: a.PendingReason, Spec: a.Spec}
		if withAuth && a.Auth != nil {
			w.RegistryAuth = &RegistryAuth{Host: a.Auth.Host, Username: a.Auth.Username, Password: a.Auth.Password}
		}
		out = append(out, w)
	}
	return out
}

// ToStore converts the report to the store's statuses. The caller sets the
// node ID on each one. Each control character in the report's text, the
// container ID, app, state and error, becomes a space (Printable): a
// worker's error can hold a Docker message with a newline in it, and an
// escape sequence would reach an admin's terminal through hivemind status.
// Refusing the report instead would also stop its lease.
func (s StatusReport) ToStore() []store.Status {
	out := make([]store.Status, 0, len(s.Containers))
	for _, c := range s.Containers {
		var rp []store.RoutePort
		for _, p := range c.RoutePorts {
			rp = append(rp, store.RoutePort{Port: p.Port, HostPort: p.HostPort})
		}
		out = append(out, store.Status{Name: c.Name, ContainerID: Printable(c.ContainerID), App: Printable(c.App), Replica: c.Replica,
			Generation: c.Generation, State: Printable(c.State), Error: Printable(c.Error), RestartCount: c.RestartCount,
			Health: c.Health, Ready: c.Ready, CrashLooping: c.CrashLooping, NextRestart: c.NextRestart, RoutePorts: rp})
	}
	return out
}

// StatusFromStore converts stored statuses to a report.
func StatusFromStore(sts []store.Status) StatusReport {
	out := StatusReport{Containers: make([]Container, 0, len(sts))}
	for _, x := range sts {
		var rp []RoutePort
		for _, p := range x.RoutePorts {
			rp = append(rp, RoutePort{Port: p.Port, HostPort: p.HostPort})
		}
		out.Containers = append(out.Containers, Container{Name: x.Name, ContainerID: x.ContainerID, App: x.App,
			Replica: x.Replica, Generation: x.Generation, State: x.State, Error: x.Error,
			RestartCount: x.RestartCount, Health: x.Health, Ready: x.Ready, CrashLooping: x.CrashLooping,
			NextRestart: x.NextRestart, RoutePorts: rp})
	}
	return out
}

// RouteTableFromStore converts the store's routing table to the wire form.
func RouteTableFromStore(rt []store.Route) RouteTable {
	out := RouteTable{Routes: make([]Route, 0, len(rt))}
	for _, r := range rt {
		eps := r.Endpoints
		if eps == nil {
			eps = []string{}
		}
		out.Routes = append(out.Routes, Route{Host: r.Host, Endpoints: eps})
	}
	return out
}

func hasControl(s string) bool {
	return strings.IndexFunc(s, unicode.IsControl) >= 0
}

// Printable returns s with each control character, C0 and C1 (ESC and CSI
// among them), replaced by a space, and each byte that is not UTF-8 by
// U+FFFD.
func Printable(s string) string {
	if !hasControl(s) && utf8.ValidString(s) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

func truncate(s string) string {
	if len(s) > 32 {
		return s[:32] + "..."
	}
	return s
}
