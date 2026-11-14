package nodeapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/ao/hivemind/internal/runtime"
)

// Poll backoff after a failed poll.
const (
	logPollMinBackoff = time.Second
	logPollMaxBackoff = 30 * time.Second
)

// LogServer is the worker's side of the log relay. It polls the server for
// log requests and streams each one from the local runtime. It opens no
// listener: every connection is one the worker dials.
type LogServer struct {
	Client  *Client
	NodeID  string
	Runtime runtime.Runtime
	// Logger receives a line when polling fails. Nil means slog.Default().
	Logger *slog.Logger
}

// Run polls until ctx ends, and returns nil then. It returns ErrNodeRemoved
// when the server says the node was removed. It waits for the streams it
// started before it returns.
func (s *LogServer) Run(ctx context.Context) error {
	log := s.Logger
	if log == nil {
		log = slog.Default()
	}
	var wg sync.WaitGroup
	defer wg.Wait()
	backoff := time.Duration(0)
	for ctx.Err() == nil {
		q, err := s.Client.NextLogRequest(ctx, s.NodeID)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, ErrNodeRemoved) {
				return err
			}
			backoff = min(max(2*backoff, logPollMinBackoff), logPollMaxBackoff)
			log.Warn("polling the server for log requests; retrying", "in", backoff, "err", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			continue
		}
		backoff = 0
		if q == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			err := s.serve(ctx, *q)
			// The line is written once the log read on this node has ended.
			if err != nil && ctx.Err() == nil {
				log.Info("log stream ended", "app", q.App, "replica", q.Replica, "follow", q.Follow,
					"duration", time.Since(start).Round(time.Millisecond), "err", err)
				return
			}
			log.Info("log stream ended", "app", q.App, "replica", q.Replica, "follow", q.Follow,
				"duration", time.Since(start).Round(time.Millisecond))
		}()
	}
	return nil
}

// serve streams one request. The stream is cut when the server stops reading,
// which it does when the user goes away, and then the runtime's log read is
// cancelled.
func (s *LogServer) serve(ctx context.Context, q LogRequest) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		pw.CloseWithError(s.produce(ctx, q, NewFrameWriter(pw)))
	}()
	err := s.Client.SendLogs(ctx, s.NodeID, q.ID, pr)
	cancel()
	_ = pr.CloseWithError(io.ErrClosedPipe)
	<-done
	return err
}

// produce writes the frames of one request. It returns nil at the end of the
// log, which closes the stream cleanly.
func (s *LogServer) produce(ctx context.Context, q LogRequest, fw *FrameWriter) error {
	cs, err := s.Runtime.List(ctx, map[string]string{
		runtime.LabelApp: q.App, runtime.LabelReplica: strconv.Itoa(q.Replica)})
	if err != nil {
		t := FrameFailed
		if errors.Is(err, runtime.ErrUnavailable) {
			t = FrameUnavailable
		}
		return fw.WriteFrame(t, []byte(err.Error()))
	}
	if len(cs) == 0 {
		return fw.WriteFrame(FrameNotFound, []byte(fmt.Sprintf("app %q replica %d has no container on node %s", q.App, q.Replica, s.NodeID)))
	}
	c := runtime.PickContainer(cs)
	if err := fw.WriteFrame(FrameReady, nil); err != nil {
		return err
	}
	err = s.Runtime.Logs(ctx, c.ID, runtime.LogOptions{Follow: q.Follow, Tail: q.Tail},
		fw.Output(FrameStdout), fw.Output(FrameStderr))
	if err == nil || ctx.Err() != nil || errors.Is(err, io.ErrClosedPipe) {
		return err
	}
	// The server logs it; the user has the 200 already. The stream then ends
	// cleanly, so the server reads the frame.
	return fw.WriteFrame(FrameFailed, []byte(err.Error()))
}
