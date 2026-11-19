package nodeapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ao/hivemind/internal/store"
)

// LocalAddress returns the local IP address of the client's connection to
// the server: the address the server sees this node at, which is the node's
// private address for routing. It makes a GET of the node's status, which
// changes nothing, and reads the address from the connection it used.
func (c *Client) LocalAddress(ctx context.Context, nodeID string) (string, error) {
	var mu sync.Mutex
	var local net.Addr
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		mu.Lock()
		local = info.Conn.LocalAddr()
		mu.Unlock()
	}}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodGet, c.base+StatusPath(nodeID), nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("calling the server: %w", err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, MaxStatusBytes))
	resp.Body.Close()
	mu.Lock()
	defer mu.Unlock()
	tcp, ok := local.(*net.TCPAddr)
	if !ok {
		return "", errors.New("the connection to the server has no TCP local address")
	}
	ip, ok := netip.AddrFromSlice(tcp.IP)
	if !ok {
		return "", fmt.Errorf("local address %s is not an IP address", tcp)
	}
	return ip.Unmap().String(), nil
}

// Routes returns the routing table, with path routes and their protocols:
// the router of this build reads both. An older server ignores the
// parameters it does not know and sends routes without protocols, or whole
// host routes only, which this build reads as well.
func (c *Client) Routes(ctx context.Context, nodeID string) ([]Route, error) {
	var t RouteTable
	q := "?" + RoutesPathsParam + "=1&" + RoutesProtocolsParam + "=1"
	if err := c.do(ctx, http.MethodGet, RoutesPath(nodeID)+q, nil, &t); err != nil {
		return nil, err
	}
	if err := t.Validate(); err != nil {
		return nil, fmt.Errorf("server sent a routing table that does not validate: %w", err)
	}
	return t.Routes, nil
}

// RouteReaderMaxAge is how long the server remembers a router's last read
// of the routing table. Routers read it every second, so one that has not
// read it for this long has stopped, or was upgraded and reads paths now.
const RouteReaderMaxAge = time.Minute

// RouteReaders keeps, on the server, how each worker's router last read the
// routing table: with or without paths and protocols, and its version. A
// router of a build before path routes reads it without either, and answers
// 503 for the hosts that have path routes, and for those whose route is h2c
// (see HostTableFromStore); one of a build with paths and before protocols
// answers 503 for the h2c routes (RouteTableFromStore). The server warns
// about such routers in its log, in GET /v1/routes and in the status of the
// apps whose routes they hold back. It is safe for concurrent use; a nil one
// keeps nothing.
type RouteReaders struct {
	mu sync.Mutex
	m  map[string]routeRead
}

// RouteRead is how a router read the table once.
type RouteRead struct {
	// Version is the version its node sent with the read, or "" for none.
	Version string
	// Paths and Protocols say whether it asked for path routes and for
	// route protocols.
	Paths, Protocols bool
	// Held are the hosts it was given with no endpoints because they have
	// path routes, and H2C the routes, as host and path, given none
	// because they are h2c routes.
	Held, H2C []string
}

type routeRead struct {
	RouteRead
	at time.Time
}

// OldRouter is a router that read the routing table without paths or
// without protocols.
type OldRouter struct {
	Node string `json:"node"`
	// Version is the version its node sent with the read, or "" for none.
	Version string `json:"version"`
	// Paths and Protocols say whether it reads path routes and route
	// protocols. A router that reads no paths reads no protocols either.
	Paths     bool `json:"paths"`
	Protocols bool `json:"protocols"`
	// Hosts are the hosts it was given with no endpoints because they have
	// path routes, and answers 503 for. Empty while no host has one, and
	// for a router that reads paths.
	Hosts []string `json:"hosts"`
	// H2C are the routes, as host and path (www.example.com/api), that it
	// was given with no endpoints because they are h2c routes, and answers
	// 503 for. Empty while there are none.
	H2C []string `json:"h2c"`
}

// NewRouteReaders returns an empty holder.
func NewRouteReaders() *RouteReaders { return &RouteReaders{m: map[string]routeRead{}} }

// Seen records node's router's read r at at. It reports whether the read
// differs from the node's last one, in version, in what it asked for or in
// what it was held back from, so the caller logs a change once.
func (rr *RouteReaders) Seen(node string, r RouteRead, at time.Time) bool {
	if rr == nil {
		return false
	}
	rr.mu.Lock()
	defer rr.mu.Unlock()
	last, ok := rr.m[node]
	rr.m[node] = routeRead{RouteRead: r, at: at}
	return !ok || last.Version != r.Version || last.Paths != r.Paths || last.Protocols != r.Protocols ||
		!slices.Equal(last.Held, r.Held) || !slices.Equal(last.H2C, r.H2C)
}

// Old returns the routers that read the table without paths or without
// protocols within RouteReaderMaxAge before now, sorted by node, and
// forgets the reads older than that.
func (rr *RouteReaders) Old(now time.Time) []OldRouter {
	if rr == nil {
		return nil
	}
	rr.mu.Lock()
	defer rr.mu.Unlock()
	var out []OldRouter
	for node, r := range rr.m {
		if now.Sub(r.at) > RouteReaderMaxAge {
			delete(rr.m, node)
			continue
		}
		if !r.Paths || !r.Protocols {
			out = append(out, OldRouter{Node: node, Version: r.Version, Paths: r.Paths, Protocols: r.Protocols,
				Hosts: orEmpty(r.Held), H2C: orEmpty(r.H2C)})
		}
	}
	slices.SortFunc(out, func(a, b OldRouter) int { return strings.Compare(a.Node, b.Node) })
	return out
}

// Reading returns the nodes whose router read the routing table within
// RouteReaderMaxAge before now, whatever it asked for: the workers that run
// a router, for hivemind nodes. A router reads the table every second, so
// one that stopped, or whose node is cut off, drops out within that time.
func (rr *RouteReaders) Reading(now time.Time) map[string]bool {
	if rr == nil {
		return nil
	}
	rr.mu.Lock()
	defer rr.mu.Unlock()
	out := map[string]bool{}
	for node, r := range rr.m {
		if now.Sub(r.at) <= RouteReaderMaxAge {
			out[node] = true
		}
	}
	return out
}

// orEmpty is a copy of s, or an empty list for none.
func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return slices.Clone(s)
}

// TLS returns the node's view of the shared TLS storage on the server. It
// is what a worker's router keeps its certificates and ACME state in.
func (c *Client) TLS(nodeID string) *TLSClient { return &TLSClient{c: c, node: nodeID} }

// TLSClient calls the TLS storage routes for one node. Missing keys come back
// as errors that match fs.ErrNotExist, as certmagic expects.
type TLSClient struct {
	c    *Client
	node string
}

func (t *TLSClient) call(ctx context.Context, op string, req TLSRequest) (TLSReply, error) {
	if err := req.Validate(op); err != nil {
		return TLSReply{}, err
	}
	b, err := json.Marshal(req)
	if err != nil {
		return TLSReply{}, err
	}
	if len(b) > MaxTLSRequestBytes {
		return TLSReply{}, fmt.Errorf("%w: TLS storage request is %d bytes, more than %d", ErrInvalid, len(b), MaxTLSRequestBytes)
	}
	var rep TLSReply
	if err := t.c.do(ctx, http.MethodPost, TLSPath(t.node, op), b, &rep); err != nil {
		return TLSReply{}, err
	}
	if err := rep.Validate(); err != nil {
		return TLSReply{}, fmt.Errorf("server sent a TLS storage reply that does not validate: %w", err)
	}
	return rep, nil
}

func notExist(key string) error {
	return fmt.Errorf("TLS storage key %q: %w", truncate(key), fs.ErrNotExist)
}

// Load returns the value at key.
func (t *TLSClient) Load(ctx context.Context, key string) ([]byte, error) {
	rep, err := t.call(ctx, TLSLoad, TLSRequest{Key: key})
	if err != nil {
		return nil, err
	}
	if !rep.Found {
		return nil, notExist(key)
	}
	if rep.Value == nil {
		rep.Value = []byte{}
	}
	return rep.Value, nil
}

// Store puts value at key.
func (t *TLSClient) Store(ctx context.Context, key string, value []byte) error {
	_, err := t.call(ctx, TLSStore, TLSRequest{Key: key, Value: value})
	return err
}

// Delete deletes key and every key under it.
func (t *TLSClient) Delete(ctx context.Context, key string) error {
	_, err := t.call(ctx, TLSDelete, TLSRequest{Key: key})
	return err
}

// Exists reports whether key holds a value or prefixes others.
func (t *TLSClient) Exists(ctx context.Context, key string) (bool, error) {
	rep, err := t.call(ctx, TLSExists, TLSRequest{Key: key})
	return rep.Exists, err
}

// List returns the keys under prefix.
func (t *TLSClient) List(ctx context.Context, prefix string, recursive bool) ([]string, error) {
	rep, err := t.call(ctx, TLSList, TLSRequest{Key: prefix, Recursive: recursive})
	if err != nil {
		return nil, err
	}
	if !rep.Found {
		return nil, notExist(prefix)
	}
	if rep.Keys == nil {
		rep.Keys = []string{}
	}
	return rep.Keys, nil
}

// Stat describes key.
func (t *TLSClient) Stat(ctx context.Context, key string) (store.TLSKeyInfo, error) {
	rep, err := t.call(ctx, TLSStat, TLSRequest{Key: key})
	if err != nil {
		return store.TLSKeyInfo{}, err
	}
	if !rep.Found || rep.Info == nil {
		return store.TLSKeyInfo{}, notExist(key)
	}
	return store.TLSKeyInfo{Key: rep.Info.Key, Modified: rep.Info.Modified, Size: rep.Info.Size, Terminal: rep.Info.Terminal}, nil
}

// TryLock takes the lock name for owner until ttl from now, if it is free
// or stale, and reports whether it did.
func (t *TLSClient) TryLock(ctx context.Context, name, owner string, ttl time.Duration) (bool, error) {
	rep, err := t.call(ctx, TLSLock, TLSRequest{Name: name, Owner: owner, TTLMillis: ttl.Milliseconds()})
	return rep.Acquired, err
}

// RenewLock moves the expiry of a lock owner holds to ttl from now. A lock
// owner does not hold is an error that matches store.ErrNotFound.
func (t *TLSClient) RenewLock(ctx context.Context, name, owner string, ttl time.Duration) error {
	_, err := t.call(ctx, TLSRenew, TLSRequest{Name: name, Owner: owner, TTLMillis: ttl.Milliseconds()})
	return notHeld(err)
}

// Unlock releases a lock owner holds. A lock owner does not hold is an error
// that matches store.ErrNotFound.
func (t *TLSClient) Unlock(ctx context.Context, name, owner string) error {
	_, err := t.call(ctx, TLSUnlock, TLSRequest{Name: name, Owner: owner})
	return notHeld(err)
}

// notHeld turns the server's 409 for a lock that is not held into an error
// that matches store.ErrNotFound, as the store's own error does.
func notHeld(err error) error {
	var re *RefusedError
	if errors.As(err, &re) && re.Code == http.StatusConflict {
		return fmt.Errorf("%w: %w", store.ErrNotFound, err)
	}
	return err
}

// DecodeTLSRequest is the server's decoder for a TLS storage request:
// Decode with the TLS limit, then Validate for op.
func DecodeTLSRequest(r io.Reader, op string) (TLSRequest, error) {
	var req TLSRequest
	if err := Decode(r, MaxTLSRequestBytes, &req); err != nil {
		return TLSRequest{}, err
	}
	if err := req.Validate(op); err != nil {
		return TLSRequest{}, err
	}
	return req, nil
}

// EncodeTLSInfo converts a store key description to the wire form.
func EncodeTLSInfo(i store.TLSKeyInfo) *TLSKeyInfo {
	return &TLSKeyInfo{Key: i.Key, Modified: i.Modified, Size: i.Size, Terminal: i.Terminal}
}
