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

// Routes returns the routing table.
func (c *Client) Routes(ctx context.Context, nodeID string) ([]Route, error) {
	var t RouteTable
	if err := c.do(ctx, http.MethodGet, RoutesPath(nodeID), nil, &t); err != nil {
		return nil, err
	}
	if err := t.Validate(); err != nil {
		return nil, fmt.Errorf("server sent a routing table that does not validate: %w", err)
	}
	return t.Routes, nil
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
