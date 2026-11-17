package store

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/ao/hivemind/internal/spec"
)

// The size of a node's assignment list.
//
// The node API sends a node its whole assignment list in one reply, and a
// worker reads at most nodeapi.MaxAssignmentsBytes (16 MiB) of it; over that
// it changes nothing on the node. Each assignment holds its replica's whole
// spec, which the spec limits allow to be about 1 MiB, and a running one the
// credential of its image's registry, up to 16 KiB, so many replicas of a
// large spec, or a long credential repeated on each one, could pass it.
//
// MaxNodeAssignmentBytes bounds what one node's list may take, below the
// worker's limit by room for the rest of the reply. Placement puts no
// replica on a node that it would take past the bound
// (scheduler.Options.MaxNodeBytes). A put that would take a node past it in
// place, with a larger spec for the replicas the node has, and a registry
// login that would, with a longer credential, are refused with ErrConflict.
// A node already past it, as one could be before this bound, may still
// shrink.
const MaxNodeAssignmentBytes = 15 << 20

const (
	// rowFixed bounds the bytes of an assignment besides its strings, spec
	// and credential: the keys, quotes, commas and braces, two numbers,
	// and the list's own comma.
	rowFixed = 160
	// reasonRoom is what a new replica's assignment keeps for a reason the
	// server may give it later, such as that its volume's node is away.
	// The server writes those reasons itself, from names and addresses.
	reasonRoom = 512
)

// jsonLen bounds the length of s as a JSON string, quotes included. Go's
// encoder writes a byte as at most six, as it does a < or a control character.
func jsonLen(s string) int {
	n := 2
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"' || c == '\\':
			n += 2
		case c < 0x20 || c == '<' || c == '>' || c == '&' || c >= 0x80:
			n += 6
		default:
			n++
		}
	}
	return n
}

// credBytes bounds what a credential takes in an assignment.
func credBytes(c RegistryAuth) int {
	return len(`,"registry_auth":{"host":,"username":,"password":}`) + jsonLen(c.Host) + jsonLen(c.Username) + jsonLen(c.Password)
}

// assignmentBytes bounds what a takes in its node's list, with a spec of
// specLen bytes, which is the stored spec's length (the node API writes the
// same JSON), and a credential of credLen bytes.
func assignmentBytes(a Assignment, specLen, credLen int) int64 {
	return int64(rowFixed + jsonLen(a.App) + jsonLen(a.NodeID) + jsonLen(string(a.Desired)) +
		jsonLen(a.PendingReason) + specLen + credLen)
}

// registryBytes returns credBytes of each stored credential, by host.
func registryBytes(ctx context.Context, q querier) (map[string]int, error) {
	rows, err := q.QueryContext(ctx, `SELECT host, username, password FROM registries`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var c RegistryAuth
		if err := rows.Scan(&c.Host, &c.Username, &c.Password); err != nil {
			return nil, err
		}
		out[c.Host] = credBytes(c)
	}
	return out, rows.Err()
}

// credFor returns the credential bytes an assignment of sp that is to run
// carries, from creds by host.
func credFor(creds map[string]int, desired Desired, sp spec.App) int {
	if desired != Running || len(creds) == 0 {
		return 0
	}
	host, err := spec.RegistryHost(sp.Image)
	if err != nil {
		return 0
	}
	return creds[host]
}

// nodeBytes returns, for each node, a bound on the bytes of its assignment
// list. Each of app's assignments counts with a spec of at least
// atLeast bytes: a put checks its new spec against the replicas the app
// has, which a rollout may move to it in place.
func nodeBytes(ctx context.Context, q querier, app string, atLeast int) (map[string]int64, error) {
	placed, err := placedOn(ctx, q)
	if err != nil {
		return nil, err
	}
	creds, err := registryBytes(ctx, q)
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, a := range placed {
		n := a.specLen
		if a.App == app {
			n = max(n, atLeast)
		}
		out[a.NodeID] += assignmentBytes(a, n, credFor(creds, a.Desired, a.Spec))
	}
	return out, nil
}

// checkNodeBytes refuses a change that leaves a node's assignments past
// MaxNodeAssignmentBytes, and larger than before it. what names the change
// and fix says what to do, for the message.
func checkNodeBytes(before, after map[string]int64, what, fix string) error {
	var nodes []string
	for n, b := range after {
		if b > MaxNodeAssignmentBytes && b > before[n] {
			nodes = append(nodes, n)
		}
	}
	if len(nodes) == 0 {
		return nil
	}
	slices.Sort(nodes)
	n := nodes[0]
	return fmt.Errorf("%w: %s would make the assignments of node %s %s, more than the %s a node reads; %s",
		ErrConflict, what, n, MiB(after[n]), MiB(MaxNodeAssignmentBytes), fix)
}

// specBytes returns the length of sp as the store writes it.
func specBytes(sp spec.App) int {
	b, err := json.Marshal(sp)
	if err != nil {
		return 0
	}
	return len(b)
}

// MiB writes n bytes in MiB, with one decimal unless it is a whole number.
func MiB(n int64) string {
	if n%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", n>>20)
	}
	return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
}

// registryBytesCached is registryBytes, read on the read connections and
// kept until a registry write (forgetRegistryBytes). Placement has no
// transaction to read it in. A failed read counts no credential, and is
// tried again at the next placement.
func (s *Store) registryBytesCached() map[string]int {
	s.credMu.Lock()
	defer s.credMu.Unlock()
	if s.credSizes == nil {
		m, err := registryBytes(context.Background(), s.rdb)
		if err != nil {
			return nil
		}
		s.credSizes = m
	}
	return s.credSizes
}

// forgetRegistryBytes drops credSizes after a registry write, and the key of
// the last placement that placed nothing, since placement reads credSizes.
func (s *Store) forgetRegistryBytes() {
	s.credMu.Lock()
	s.credSizes = nil
	s.credMu.Unlock()
	s.placeMu.Lock()
	s.placeSet = false
	s.placeMu.Unlock()
}

// AssignmentBytes returns, for each node with an assignment, the bound on
// the bytes of its assignment list that placement and the writes keep
// within MaxNodeAssignmentBytes.
func (s *Store) AssignmentBytes(ctx context.Context) (map[string]int64, error) {
	return nodeBytes(ctx, s.rdb, "", 0)
}
