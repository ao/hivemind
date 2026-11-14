package nodeapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/ao/hivemind/internal/spec"
)

// Log relay. A worker has no listener, so the server cannot ask it for a log.
// Instead the worker holds a long poll open on LogRequestsPath. When a user asks
// the API for the log of a replica on that worker, the server answers the poll
// with a LogRequest. The worker then POSTs the log to LogStreamPath, as a
// stream of frames, and the server copies it to the user's response. The
// server checks that the node in the certificate is the node it queued the
// request for.

// Log relay limits and timeouts.
const (
	// LogPollTimeout is how long the server holds a poll open before it answers
	// 204 with no request. It is below the node listener's write timeout.
	LogPollTimeout = 25 * time.Second
	// LogPickupTimeout is how long the API waits for a worker to take a request
	// and start its stream.
	LogPickupTimeout = 10 * time.Second
	// MaxLogStreamsPerNode caps the log requests a node has queued or open.
	MaxLogStreamsPerNode = 16
	// MaxLogPollsPerNode caps the polls a node can hold open at once.
	MaxLogPollsPerNode = 4
	// MaxLogTail is the largest tail a LogRequest may carry.
	MaxLogTail = 1_000_000
	// MaxLogRequestBytes caps the poll reply.
	MaxLogRequestBytes = 4 << 10
	// MaxFramePayload caps the payload of one output frame. A longer write is
	// split.
	MaxFramePayload = 64 << 10
	// MaxFrameMessage caps the payload of a ready or error frame.
	MaxFrameMessage = 4 << 10
	// LogStreamContentType is the media type of a log stream body.
	LogStreamContentType = "application/vnd.hivemind.log-frames"
)

// ErrLogRequestGone means the server no longer waits for the log stream: it
// timed out, or the user went away.
var ErrLogRequestGone = errors.New("the server no longer waits for this log stream")

// LogRequestsPath is the route a node long-polls for log requests.
func LogRequestsPath(id string) string { return Prefix + "/nodes/" + id + "/logs/requests" }

// LogStreamPath is the route a node POSTs one request's log stream to.
func LogStreamPath(id, req string) string { return Prefix + "/nodes/" + id + "/logs/streams/" + req }

// LogRequest asks a node for the log of one replica's container.
type LogRequest struct {
	ID      string `json:"id"`
	App     string `json:"app"`
	Replica int    `json:"replica"`
	Tail    int    `json:"tail"`
	Follow  bool   `json:"follow"`
}

var logRequestIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ValidLogRequestID reports whether id has the form the server issues: 32
// lowercase hex digits.
func ValidLogRequestID(id string) bool { return logRequestIDRE.MatchString(id) }

// Validate checks a log request.
func (q LogRequest) Validate() error {
	switch {
	case !ValidLogRequestID(q.ID):
		return fmt.Errorf("%w: log request ID is malformed", ErrInvalid)
	case !spec.ValidName(q.App):
		return fmt.Errorf("%w: log request app %q is malformed", ErrInvalid, truncate(q.App))
	case q.Replica < 0 || q.Replica > maxReplicaIndex:
		return fmt.Errorf("%w: log request replica %d is out of range", ErrInvalid, q.Replica)
	case q.Tail < 0 || q.Tail > MaxLogTail:
		return fmt.Errorf("%w: log request tail %d is out of range", ErrInvalid, q.Tail)
	}
	return nil
}

// Frame types of a log stream. Each frame is an 8-byte header and a payload:
// one byte of type, three zero bytes, and the payload length as a big-endian
// uint32. This is the layout of Docker's multiplexed log stream, with more
// types.
//
// A stream starts with FrameReady once the node has found the container, or
// with one error frame and nothing else. After FrameReady come stdout and
// stderr frames, in the order the engine wrote them, and at most one
// FrameFailed if the log read failed part way.
const (
	FrameStdout      byte = 1
	FrameStderr      byte = 2
	FrameReady       byte = 3
	FrameNotFound    byte = 4 // the replica has no container on the node
	FrameUnavailable byte = 5 // the node's container engine cannot be reached
	FrameFailed      byte = 6 // any other failure; the payload says what
)

// FrameWriter writes frames to one writer. It is safe for concurrent use, so
// stdout and stderr can be written from separate goroutines.
type FrameWriter struct {
	mu sync.Mutex
	w  io.Writer
}

// NewFrameWriter returns a FrameWriter on w.
func NewFrameWriter(w io.Writer) *FrameWriter { return &FrameWriter{w: w} }

// WriteFrame writes p as frames of type t, split at MaxFramePayload. A ready
// or error frame is cut at MaxFrameMessage.
func (f *FrameWriter) WriteFrame(t byte, p []byte) error {
	if t != FrameStdout && t != FrameStderr && len(p) > MaxFrameMessage {
		p = p[:MaxFrameMessage]
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for first := true; first || len(p) > 0; first = false {
		n := min(len(p), MaxFramePayload)
		var hdr [8]byte
		hdr[0] = t
		binary.BigEndian.PutUint32(hdr[4:], uint32(n))
		if _, err := f.w.Write(hdr[:]); err != nil {
			return err
		}
		if n > 0 {
			if _, err := f.w.Write(p[:n]); err != nil {
				return err
			}
		}
		p = p[n:]
	}
	return nil
}

// Output returns a writer that sends each write as frames of type t, which is
// FrameStdout or FrameStderr.
func (f *FrameWriter) Output(t byte) io.Writer { return frameOutput{f: f, t: t} }

type frameOutput struct {
	f *FrameWriter
	t byte
}

func (o frameOutput) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := o.f.WriteFrame(o.t, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// FrameReader reads a log stream frame by frame.
type FrameReader struct {
	r    io.Reader
	left int64
}

// NewFrameReader returns a FrameReader on r.
func NewFrameReader(r io.Reader) *FrameReader { return &FrameReader{r: r} }

// ErrBadFrame means a log stream is not in the frame format.
var ErrBadFrame = errors.New("malformed log stream frame")

// Next reads the next frame header and returns its type and payload length.
// The payload of the previous frame is skipped if it was not read. A stream
// that ends on a frame boundary returns io.EOF.
func (f *FrameReader) Next() (byte, int64, error) {
	if f.left > 0 {
		if _, err := io.CopyN(io.Discard, f.r, f.left); err != nil {
			return 0, 0, unexpected(err)
		}
		f.left = 0
	}
	var hdr [8]byte
	if _, err := io.ReadFull(f.r, hdr[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return 0, 0, io.EOF
		}
		return 0, 0, err
	}
	t, n := hdr[0], int64(binary.BigEndian.Uint32(hdr[4:]))
	switch {
	case hdr[1] != 0 || hdr[2] != 0 || hdr[3] != 0:
		return 0, 0, ErrBadFrame
	case t == FrameStdout || t == FrameStderr:
		if n > MaxFramePayload {
			return 0, 0, ErrBadFrame
		}
	case t >= FrameReady && t <= FrameFailed:
		if n > MaxFrameMessage {
			return 0, 0, ErrBadFrame
		}
	default:
		return 0, 0, ErrBadFrame
	}
	f.left = n
	return t, n, nil
}

// Copy copies the current frame's payload to w.
func (f *FrameReader) Copy(w io.Writer) error {
	n := f.left
	f.left = 0
	if _, err := io.CopyN(w, f.r, n); err != nil {
		return unexpected(err)
	}
	return nil
}

// Message returns the current frame's payload as text. Ready and error frames
// are at most MaxFrameMessage bytes.
func (f *FrameReader) Message() (string, error) {
	var b bytes.Buffer
	if err := f.Copy(&b); err != nil {
		return "", err
	}
	return b.String(), nil
}

func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// NextLogRequest waits for the server to hand this node a log request. It
// returns nil, nil when the server's poll time passes with none.
func (c *Client) NextLogRequest(ctx context.Context, nodeID string) (*LogRequest, error) {
	ctx, cancel := context.WithTimeout(ctx, LogPollTimeout+15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+LogRequestsPath(nodeID), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.stream.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling the server: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNoContent:
		return nil, nil
	case resp.StatusCode != http.StatusOK:
		return nil, statusError(resp)
	}
	var q LogRequest
	if err := Decode(resp.Body, MaxLogRequestBytes, &q); err != nil {
		return nil, fmt.Errorf("reading the server's reply: %w", err)
	}
	if err := q.Validate(); err != nil {
		return nil, fmt.Errorf("server sent a log request that does not validate: %w", err)
	}
	return &q, nil
}

// SendLogs streams body, a log stream in frames, to the server for request id.
// It returns when the server has read the stream to its end, or stopped
// reading it because the user went away. The request has no timeout: a
// followed log runs until one side ends it, so cancel ctx to stop it.
func (c *Client) SendLogs(ctx context.Context, nodeID, id string, body io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+LogStreamPath(nodeID, id), body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", LogStreamContentType)
	resp, err := c.stream.Do(req)
	if err != nil {
		return fmt.Errorf("sending the log stream: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil
	}
	if resp.StatusCode == http.StatusNotFound {
		var e ErrorBody
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&e)
		return fmt.Errorf("%w: %s", ErrLogRequestGone, e.Error)
	}
	return statusError(resp)
}
