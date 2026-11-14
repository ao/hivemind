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
	"strings"
	"time"
	"unicode"

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

// Register is the body of POST register.
type Register struct {
	Name   string `json:"name"`
	CPU    int    `json:"cpu"`
	Memory int64  `json:"memory"`
}

// Assignment is one replica placed on a node, with the spec to run it from.
type Assignment struct {
	App           string   `json:"app"`
	Index         int      `json:"index"`
	NodeID        string   `json:"node_id"`
	Generation    int      `json:"generation"`
	Desired       string   `json:"desired"`
	PendingReason string   `json:"pending_reason,omitempty"`
	Spec          spec.App `json:"spec"`
}

// AssignmentList is the reply to GET assignments.
type AssignmentList struct {
	Assignments []Assignment `json:"assignments"`
}

// Container is one container as the node last saw it. It mirrors
// store.Status without the node ID, which the server takes from the path.
type Container struct {
	Name         string    `json:"name"`
	ContainerID  string    `json:"container_id"`
	App          string    `json:"app"`
	Replica      int       `json:"replica"`
	Generation   int       `json:"generation"`
	State        string    `json:"state"`
	Error        string    `json:"error,omitempty"`
	RestartCount int       `json:"restart_count"`
	Health       string    `json:"health,omitempty"`
	Ready        bool      `json:"ready"`
	CrashLooping bool      `json:"crash_looping"`
	NextRestart  time.Time `json:"next_restart"`
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
	}
	return nil
}

// ToStore converts an assignment to the store's form.
func (a Assignment) ToStore() store.Assignment {
	return store.Assignment{
		Replica: store.Replica{App: a.App, Index: a.Index, NodeID: a.NodeID, Generation: a.Generation,
			Desired: store.Desired(a.Desired), PendingReason: a.PendingReason},
		Spec: a.Spec,
	}
}

// FromStore converts store assignments to the wire form.
func FromStore(as []store.Assignment) []Assignment {
	out := make([]Assignment, 0, len(as))
	for _, a := range as {
		out = append(out, Assignment{App: a.App, Index: a.Index, NodeID: a.NodeID, Generation: a.Generation,
			Desired: string(a.Desired), PendingReason: a.PendingReason, Spec: a.Spec})
	}
	return out
}

// ToStore converts the report to the store's statuses. The caller sets the
// node ID on each one.
func (s StatusReport) ToStore() []store.Status {
	out := make([]store.Status, 0, len(s.Containers))
	for _, c := range s.Containers {
		out = append(out, store.Status{Name: c.Name, ContainerID: c.ContainerID, App: c.App, Replica: c.Replica,
			Generation: c.Generation, State: c.State, Error: c.Error, RestartCount: c.RestartCount,
			Health: c.Health, Ready: c.Ready, CrashLooping: c.CrashLooping, NextRestart: c.NextRestart})
	}
	return out
}

// StatusFromStore converts stored statuses to a report.
func StatusFromStore(sts []store.Status) StatusReport {
	out := StatusReport{Containers: make([]Container, 0, len(sts))}
	for _, x := range sts {
		out.Containers = append(out.Containers, Container{Name: x.Name, ContainerID: x.ContainerID, App: x.App,
			Replica: x.Replica, Generation: x.Generation, State: x.State, Error: x.Error,
			RestartCount: x.RestartCount, Health: x.Health, Ready: x.Ready, CrashLooping: x.CrashLooping,
			NextRestart: x.NextRestart})
	}
	return out
}

func hasControl(s string) bool {
	return strings.IndexFunc(s, unicode.IsControl) >= 0
}

func truncate(s string) string {
	if len(s) > 32 {
		return s[:32] + "..."
	}
	return s
}
