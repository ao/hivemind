package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/ao/hivemind/internal/server"
)

// defaultWaitTimeout is how long --wait waits unless --timeout says.
const defaultWaitTimeout = 5 * time.Minute

// waitPoll is how often --wait reads the app's status, and waitSettle how
// long every replica must stay running and ready before it counts. A
// container that exits at once is running for a moment; the agent reports
// the exit on its next pass, 2 seconds later by default. Variables so that
// tests can shorten them.
var (
	waitPoll   = time.Second
	waitSettle = 3 * time.Second
)

// waitFlags are --wait and --timeout.
type waitFlags struct {
	wait    bool
	timeout time.Duration
}

func addWaitFlags(cmd *cobra.Command) *waitFlags {
	w := &waitFlags{}
	cmd.Flags().BoolVar(&w.wait, "wait", false, "wait until every replica that should run is running and ready")
	cmd.Flags().DurationVar(&w.timeout, "timeout", defaultWaitTimeout, "with --wait, how long to wait")
	return w
}

// check refuses --timeout without --wait, and a timeout that is not positive.
func (w *waitFlags) check(cmd *cobra.Command) error {
	if cmd.Flags().Changed("timeout") && !w.wait {
		return errors.New("--timeout is how long --wait waits: give --wait too")
	}
	if w.timeout <= 0 {
		return errors.New("--timeout must be more than 0")
	}
	return nil
}

// waitReplica is a replica's status as --wait reads it. A server older than
// 2.2 sends no ready field, so Ready is nil there.
type waitReplica struct {
	server.ReplicaStatus
	Ready *bool `json:"ready"`
}

// waitApp is an app's status as --wait reads it.
type waitApp struct {
	server.AppStatus
	Replicas []waitReplica `json:"replicas"`
}

// ready reports whether the replica is running and ready to take traffic.
// Without the server's word for it, a running replica that is not
// crash-looping and whose health check, if it has one, has passed is ready.
func (r waitReplica) ready() bool {
	if r.State != "running" || r.CrashLooping {
		return false
	}
	if r.Ready != nil {
		return *r.Ready
	}
	return r.Health == "" || r.Health == "healthy"
}

// waiting says why a replica that should run is not yet running and ready,
// or "" when its status gives no reason.
func (r waitReplica) waiting() string {
	switch {
	case r.CrashLooping:
		if r.Error != "" {
			return "crash-looping: " + r.Error
		}
		return "crash-looping"
	case r.State == "running" && r.Health == "starting" && r.Error != "":
		return "running, not ready: " + r.Error
	case r.State == "running" && r.Health == "starting":
		return "running; its health check has not passed yet"
	case r.State == "running" && r.Health == "unhealthy":
		return firstNonEmpty(r.Error, "running, and failing its health check")
	case r.NodeID == "":
		return "pending, with no node: " + firstNonEmpty(r.PendingReason, "it waits for a node")
	}
	return firstNonEmpty(r.PendingReason, r.Error)
}

// readyCount counts the replicas that should run, at generation gen or a
// later one, that are running and ready. A copy at an older generation does
// not count: during a rollout it is still to be replaced.
func readyCount(a waitApp, gen int) int {
	ready := map[int]bool{}
	for _, r := range a.Replicas {
		if r.Desired == "running" && r.Generation >= gen && r.ready() {
			ready[r.Index] = true
		}
	}
	return len(ready)
}

// waitReady waits until every replica of app that should run is running and
// ready at generation gen or later, and has stayed so for waitSettle. It
// prints to out each reason a replica gives for waiting, and the rollout's
// reason, as they change, and a line at the end. After timeout it prints the
// app's status and fails.
func waitReady(ctx context.Context, c *apiClient, out io.Writer, app string, gen int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	reasons := map[int]string{}
	var rollout string
	var settled time.Time // when every replica was first seen ready, or zero
	for {
		var a waitApp
		if err := c.call(ctx, "GET", appPath(app, "status"), nil, &a); err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("stopped waiting; the change to %s stands, and hivemind status %s shows its replicas", app, app)
			}
			return err
		}
		for _, r := range a.Replicas {
			if r.Desired != "running" || r.Generation < gen || r.ready() {
				continue
			}
			if why := r.waiting(); why != "" && why != reasons[r.Index] {
				fmt.Fprintf(out, "%s: replica %d: %s\n", app, r.Index, oneLine(why))
				reasons[r.Index] = why
			}
		}
		if ro := a.Rollout; ro != nil && ro.Reason != "" && ro.Reason != rollout {
			fmt.Fprintf(out, "%s: %s\n", app, oneLine(ro.Reason))
			rollout = ro.Reason
		}
		n := readyCount(a, gen)
		now := time.Now()
		switch {
		case n < a.Desired:
			settled = time.Time{}
		case settled.IsZero():
			settled = now
		}
		if !settled.IsZero() && now.Sub(settled) >= waitSettle {
			fmt.Fprintf(out, "%s: %d %s running and ready\n", app, a.Desired, plural(a.Desired, "replica", "replicas"))
			return nil
		}
		if now.After(deadline) {
			printWaitStatus(ctx, c, out, app)
			return fmt.Errorf("%s: %d of %d %s running and ready after %s; hivemind status %s shows why",
				app, n, a.Desired, plural(a.Desired, "replica", "replicas"), timeout, app)
		}
		t := time.NewTimer(min(waitPoll, max(time.Until(deadline), 0)+time.Millisecond))
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("stopped waiting; the change to %s stands, and hivemind status %s shows its replicas", app, app)
		case <-t.C:
		}
	}
}

// printWaitStatus prints the app's status, as hivemind status app does, when
// --wait gives up.
func printWaitStatus(ctx context.Context, c *apiClient, out io.Writer, app string) {
	var one appStatus
	if err := c.call(ctx, "GET", appPath(app, "status"), nil, &one); err != nil {
		fmt.Fprintf(out, "reading the status of %s: %v\n", app, err)
		return
	}
	fmt.Fprintln(out)
	apps, readyKnown := statusRows([]appStatus{one})
	printStatus(out, apps, readyKnown)
}

// oneLine makes a server's message one line of printable text.
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}
