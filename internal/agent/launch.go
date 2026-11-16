package agent

import (
	"fmt"
	"time"
)

// Launch backoff. A replica whose container could not be created or started,
// for a reason other than a missing image (pull.go has its own backoff),
// waits launchBackoff(n) before the next try, where n is the failures so
// far: 2 s, 4 s, 8 s and so on, up to a minute. A full disk, a host port
// held by another process, or an engine that refuses the configuration
// fails the same way at every try, and is not retried at every pass. The
// cap is lower than the restart backoff's so that a replica starts soon
// after the cause is fixed. The state is in memory only.
const (
	launchBase = 2 * time.Second
	launchCap  = time.Minute
)

func launchBackoff(failures int) time.Duration {
	d := launchBase
	for i := 1; i < failures; i++ {
		if d >= launchCap/2 {
			return launchCap
		}
		d *= 2
	}
	return min(d, launchCap)
}

// launchState is the failures to create or start one container.
type launchState struct {
	failures int
	next     time.Time // no try before this
	err      error     // the last failure
	seen     bool      // the replica was wanted in this pass
}

func (l *launchState) message() string {
	return fmt.Sprintf("%v; next try after %s", l.err, l.next.UTC().Format("15:04:05 UTC"))
}

// launchFailed records a failure to create or start the container name.
func (a *Agent) launchFailed(name string, err error) *launchState {
	ls := a.launches[name]
	if ls == nil {
		ls = &launchState{}
		a.launches[name] = ls
	}
	ls.seen = true
	ls.failures++
	ls.err = err
	ls.next = a.cfg.Clock().Add(launchBackoff(ls.failures))
	return ls
}

// launchWaiting returns the launch state of name when its backoff has not
// ended, and marks it seen; otherwise nil.
func (a *Agent) launchWaiting(name string) *launchState {
	ls := a.launches[name]
	if ls == nil {
		return nil
	}
	ls.seen = true
	if a.cfg.Clock().Before(ls.next) {
		return ls
	}
	return nil
}

// forgetLaunches drops the state of containers no replica wanted in this
// pass, and returns when the next try is due, or zero.
func (a *Agent) forgetLaunches() time.Time {
	var next time.Time
	for name, ls := range a.launches {
		if !ls.seen {
			delete(a.launches, name)
			continue
		}
		ls.seen = false
		if next.IsZero() || ls.next.Before(next) {
			next = ls.next
		}
	}
	return next
}
