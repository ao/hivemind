// Package logrelay carries log requests from the API to workers, and their log
// streams back, without a listener on the worker.
//
// The API calls Open with the node a replica runs on. Open queues a request
// and waits. The node's worker holds a long poll open on the node listener,
// which calls Next and gets the request. The worker then POSTs the log, and
// the node listener's handler calls Claim with the request ID, the node ID
// from its client certificate and the request body. Claim refuses a request
// that was queued for another node. Open then returns a Stream that reads the
// body, and the API copies it to the user.
//
// Either side can end a stream. The API calls Cancel when the user goes away;
// the node handler then cuts the body, and the worker sees its POST end. The
// node handler returns only after the API has called Close, because a request
// body must not be read after its handler returns.
package logrelay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/ao/hivemind/internal/nodeapi"
)

// Errors.
var (
	// ErrNoAnswer means no worker took the request within the pickup timeout.
	ErrNoAnswer = errors.New("the worker did not answer")
	// ErrBusy means the node is at its cap of streams, or of polls.
	ErrBusy = errors.New("too many log streams for this node")
	// ErrClosed means the relay is shut down.
	ErrClosed = errors.New("the log relay is shut down")
	// ErrUnknown means no request with that ID waits for a stream: it never
	// existed, timed out, was not polled yet, or already ended.
	ErrUnknown = errors.New("no such log request")
	// ErrWrongNode means the request was queued for another node.
	ErrWrongNode = errors.New("the log request is for another node")
	// ErrClaimed means a stream for the request is already open.
	ErrClaimed = errors.New("the log request already has a stream")
)

// Options sets the relay's limits. A zero field takes the nodeapi default.
type Options struct {
	PickupTimeout     time.Duration
	PollTimeout       time.Duration
	MaxStreamsPerNode int
	MaxPollsPerNode   int
}

// Relay matches API requests with worker streams. It is safe for concurrent
// use.
type Relay struct {
	opts Options

	mu     sync.Mutex
	closed bool
	done   chan struct{}
	nodes  map[string]*node
	reqs   map[string]*request
	// started is when the relay was made.
	started time.Time
}

type node struct {
	queue  []*request
	wake   chan struct{} // closed and replaced when the queue grows
	active int           // requests not yet released, queued or streaming
	polls  int
	polled bool // a poll for the node has reached the relay
}

type state int

const (
	queued state = iota
	picked
	claimed
	abandoned
)

type request struct {
	msg   nodeapi.LogRequest
	node  string
	state state
	body  io.Reader
	// ready is closed when a stream claims the request.
	ready chan struct{}
	// cancel is closed when the API side wants the stream to end.
	cancel     chan struct{}
	cancelOnce sync.Once
	// released is closed when the API side has stopped reading the body.
	released    chan struct{}
	releaseOnce sync.Once
}

// New returns a relay.
func New(o Options) *Relay {
	if o.PickupTimeout <= 0 {
		o.PickupTimeout = nodeapi.LogPickupTimeout
	}
	if o.PollTimeout <= 0 {
		o.PollTimeout = nodeapi.LogPollTimeout
	}
	if o.MaxStreamsPerNode <= 0 {
		o.MaxStreamsPerNode = nodeapi.MaxLogStreamsPerNode
	}
	if o.MaxPollsPerNode <= 0 {
		o.MaxPollsPerNode = nodeapi.MaxLogPollsPerNode
	}
	return &Relay{opts: o, done: make(chan struct{}), nodes: map[string]*node{}, reqs: map[string]*request{},
		started: time.Now()}
}

// Started is when the relay was made, which is when serve started.
func (r *Relay) Started() time.Time { return r.started }

// Polled reports whether a poll for nodeID has reached the relay since it
// was made. A worker that streams logs (2.1 and later) holds a poll open
// nearly all the time, and polls again within half a minute of losing the
// server; a 2.0 worker never polls.
func (r *Relay) Polled(nodeID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.nodes[nodeID]
	return n != nil && n.polled
}

// PickupTimeout is how long Open waits for a worker.
func (r *Relay) PickupTimeout() time.Duration { return r.opts.PickupTimeout }

// MaxStreamsPerNode is the cap on one node's requests.
func (r *Relay) MaxStreamsPerNode() int { return r.opts.MaxStreamsPerNode }

// Close ends every poll and stream, and refuses new ones. It is idempotent.
func (r *Relay) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		r.closed = true
		close(r.done)
	}
}

func (r *Relay) node(id string) *node {
	n := r.nodes[id]
	if n == nil {
		n = &node{wake: make(chan struct{})}
		r.nodes[id] = n
	}
	return n
}

// Open queues a request for nodeID and waits until the node's worker claims
// it, the pickup timeout passes (ErrNoAnswer), ctx ends, or the relay closes.
// q.ID is set by Open. The caller must Close the stream it gets.
func (r *Relay) Open(ctx context.Context, nodeID string, q nodeapi.LogRequest) (*Stream, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	q.ID = hex.EncodeToString(b[:])
	req := &request{msg: q, node: nodeID, ready: make(chan struct{}), cancel: make(chan struct{}),
		released: make(chan struct{})}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, ErrClosed
	}
	n := r.node(nodeID)
	if n.active >= r.opts.MaxStreamsPerNode {
		r.mu.Unlock()
		return nil, ErrBusy
	}
	n.active++
	r.reqs[q.ID] = req
	n.queue = append(n.queue, req)
	close(n.wake)
	n.wake = make(chan struct{})
	r.mu.Unlock()

	t := time.NewTimer(r.opts.PickupTimeout)
	defer t.Stop()
	var err error
	select {
	case <-req.ready:
	case <-t.C:
		err = ErrNoAnswer
	case <-ctx.Done():
		err = ctx.Err()
	case <-r.done:
		err = ErrClosed
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if req.state == claimed {
		// The claim won a race with the timeout or ctx; the caller sees ctx
		// or the closed relay on its first read.
		return &Stream{r: r, req: req}, nil
	}
	req.state = abandoned
	delete(r.reqs, q.ID)
	n.queue = slices.DeleteFunc(n.queue, func(x *request) bool { return x == req })
	n.active--
	return nil, err
}

// Next waits up to the poll timeout for a request queued for nodeID, and
// marks it picked. It returns nil, nil when the time passes with none.
func (r *Relay) Next(ctx context.Context, nodeID string) (*nodeapi.LogRequest, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, ErrClosed
	}
	n := r.node(nodeID)
	n.polled = true
	if n.polls >= r.opts.MaxPollsPerNode {
		r.mu.Unlock()
		return nil, ErrBusy
	}
	n.polls++
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		n.polls--
		r.mu.Unlock()
	}()

	t := time.NewTimer(r.opts.PollTimeout)
	defer t.Stop()
	for {
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			return nil, ErrClosed
		}
		for len(n.queue) > 0 {
			req := n.queue[0]
			n.queue = n.queue[1:]
			if req.state == queued {
				req.state = picked
				msg := req.msg
				r.mu.Unlock()
				return &msg, nil
			}
		}
		wake := n.wake
		r.mu.Unlock()
		select {
		case <-wake:
		case <-t.C:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-r.done:
			return nil, ErrClosed
		}
	}
}

// Claim is a node's stream for one request, once the relay has accepted it.
type Claim struct {
	r   *Relay
	req *request
}

// Claim attaches body to the request id, which must have been picked by a
// poll and queued for nodeID, the node named by the stream's client
// certificate. A request for another node is left as it is, and the claim is
// refused with ErrWrongNode.
func (r *Relay) Claim(nodeID, id string, body io.Reader) (*Claim, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, ErrClosed
	}
	req := r.reqs[id]
	switch {
	case req == nil:
		return nil, ErrUnknown
	case req.node != nodeID:
		return nil, ErrWrongNode
	case req.state == claimed:
		return nil, ErrClaimed
	case req.state != picked:
		return nil, ErrUnknown
	}
	req.state = claimed
	req.body = body
	close(req.ready)
	return &Claim{r: r, req: req}, nil
}

// Wait blocks until the API side ends the stream, ctx ends, or the relay
// closes. In the last two cases it calls abort, which must make reads of the
// body fail, so that the API side stops. It then waits until the API side has
// stopped reading, so the body may be discarded when Wait returns.
func (c *Claim) Wait(ctx context.Context, abort func()) {
	select {
	case <-c.req.cancel:
	case <-ctx.Done():
	case <-c.r.done:
	}
	abort()
	<-c.req.released
	c.r.mu.Lock()
	c.r.nodes[c.req.node].active--
	c.r.mu.Unlock()
}

// Stream is the API side of a claimed request. Read returns the worker's
// frames. Cancel may be called from any goroutine; Close must be called by the
// goroutine that reads, once it has stopped reading.
type Stream struct {
	r   *Relay
	req *request
}

// Read reads the worker's stream.
func (s *Stream) Read(p []byte) (int, error) {
	select {
	case <-s.req.released:
		return 0, io.ErrClosedPipe
	default:
	}
	return s.req.body.Read(p)
}

// Cancel asks the node side to cut the stream. A blocked Read then returns an
// error. It is idempotent.
func (s *Stream) Cancel() { s.req.cancelOnce.Do(func() { close(s.req.cancel) }) }

// Close cancels the stream and releases the body. Call it once reading is
// over. It is idempotent.
func (s *Stream) Close() error {
	s.Cancel()
	s.req.releaseOnce.Do(func() {
		s.r.mu.Lock()
		delete(s.r.reqs, s.req.msg.ID)
		s.r.mu.Unlock()
		close(s.req.released)
	})
	return nil
}

// Request returns the request the stream answers.
func (s *Stream) Request() nodeapi.LogRequest { return s.req.msg }
