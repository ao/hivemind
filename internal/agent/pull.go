package agent

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ao/hivemind/internal/runtime"
	"github.com/ao/hivemind/internal/spec"
	"github.com/ao/hivemind/internal/store"
)

// Pull backoff. A failed pull of an image waits pullBackoff(n) before the
// next one, where n is the failures so far: 10 s, 20 s, 40 s and so on, up
// to 5 minutes, as Kubernetes backs off an image pull. The wait is per image
// and credential, so a registry login or logout, which changes the
// credential, is tried at the next pass. The state is in memory only: a
// restarted agent pulls at once.
const (
	pullBase = 10 * time.Second
	pullCap  = 5 * time.Minute
)

func pullBackoff(failures int) time.Duration {
	d := pullBase
	for i := 1; i < failures; i++ {
		if d >= pullCap/2 {
			return pullCap
		}
		d *= 2
	}
	return min(d, pullCap)
}

// pullKey is an image and the credential sent for it: the SHA-256 of the
// credential, or zero for none. The agent keeps the digest only.
type pullKey struct {
	image string
	cred  [sha256.Size]byte
}

// pullState is the failures of one pullKey, and the pull in flight.
type pullState struct {
	failures int
	next     time.Time // no pull before this
	err      error     // the last failure
	seen     bool      // a replica needed the pull in this pass
	// done is set while a pull is in flight, and receives its result once;
	// cancel stops it. started is when it began.
	done    chan error
	cancel  context.CancelFunc
	started time.Time
}

// pullError is a pull that failed, and when the next one is due, or one in
// flight, with err nil and since set. Waiting is true when this pass made no
// pull, because the last one failed less than its backoff ago or one is
// still in flight; the pass does not count that as an error.
type pullError struct {
	err     error
	next    time.Time
	waiting bool
	image   string
	since   time.Time
}

func (e *pullError) Error() string {
	if e.err == nil {
		return fmt.Sprintf("pulling %s, started %s; the registry has not answered yet", e.image, e.since.UTC().Format("15:04:05 UTC"))
	}
	return fmt.Sprintf("%v; next pull after %s", e.err, e.next.UTC().Format("15:04:05 UTC"))
}

func (e *pullError) Unwrap() error { return e.err }

// pullFailure is a failed pull in words that name the image. For a pull the
// registry refused, they name the registry and the command that stores a
// credential for it, and hold nothing the registry said.
type pullFailure struct {
	msg string
	err error
}

func (e *pullFailure) Error() string { return e.msg }
func (e *pullFailure) Unwrap() error { return e.err }

// registryAuth is the credential to send with a pull of image for as. One
// whose host is not the image's registry is not sent: the server only sends
// a matching one, and a credential must never go to another registry.
func registryAuth(as store.Assignment, image string, log *slog.Logger) (*runtime.RegistryAuth, string) {
	host, err := spec.RegistryHost(image)
	if err != nil {
		return nil, ""
	}
	if as.Auth == nil {
		return nil, host
	}
	if as.Auth.Host != host {
		log.Warn("not sending a registry credential for another registry", "image_registry", host, "login_registry", as.Auth.Host)
		return nil, host
	}
	return &runtime.RegistryAuth{Username: as.Auth.Username, Password: as.Auth.Password, ServerAddress: host}, host
}

// pullWait is how long a pass waits for a pull it has just started. A pull
// that ends sooner is used in the same pass, as if it had been made in line;
// one that takes longer goes on in its own goroutine, the replica is
// reported as pulling, and the pass moves on to the other replicas. When the
// pull ends it triggers a pass. So a registry that is slow, or never
// answers, holds up only the replicas of its image.
const pullWait = 2 * time.Second

// pull fetches image for as, unless a pull of it with the same credential
// failed less than its backoff ago, or is in flight. It returns nil once the
// image is there, or a *pullError.
func (a *Agent) pull(ctx context.Context, as store.Assignment, image string, log *slog.Logger) error {
	auth, host := registryAuth(as, image, log)
	key := pullKey{image: image}
	if auth != nil {
		key.cred = sha256.Sum256([]byte(auth.Username + "\x00" + auth.Password))
	}
	ps := a.pulls[key]
	if ps == nil {
		ps = &pullState{}
		a.pulls[key] = ps
	}
	ps.seen = true
	if ps.done != nil {
		select {
		case err := <-ps.done:
			return a.pulled(ctx, key, ps, image, host, auth != nil, err)
		default:
			return &pullError{image: image, since: ps.started, waiting: true}
		}
	}
	now := a.cfg.Clock()
	if now.Before(ps.next) {
		return &pullError{err: ps.err, next: ps.next, waiting: true}
	}
	log.Info("pulling image", "image", image, "logged_in", auth != nil)
	pctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	ps.done, ps.cancel, ps.started = done, cancel, now
	go func() {
		done <- a.cfg.Runtime.Pull(pctx, image, auth)
		a.Trigger()
	}()
	t := time.NewTimer(pullWait)
	defer t.Stop()
	select {
	case err := <-done:
		return a.pulled(ctx, key, ps, image, host, auth != nil, err)
	case <-t.C:
		log.Info("the pull goes on in the background", "image", image)
		return &pullError{image: image, since: now, waiting: true}
	}
}

// pulled records the end of the pull of key: nil, or a *pullError with the
// failure and the next pull's time.
func (a *Agent) pulled(ctx context.Context, key pullKey, ps *pullState, image, host string, sent bool, err error) error {
	ps.cancel()
	ps.done, ps.cancel = nil, nil
	if err == nil {
		delete(a.pulls, key)
		return nil
	}
	if ctx.Err() != nil {
		return err
	}
	if errors.Is(err, runtime.ErrUnauthorized) {
		err = refused(image, host, sent, err)
	} else {
		// The runtime's error starts with its operation, "pull: ".
		msg := err.Error()
		var re *runtime.Error
		if errors.As(err, &re) && re.Op == "pull" {
			msg = strings.TrimPrefix(msg, "pull: ")
		}
		err = &pullFailure{msg: fmt.Sprintf("pulling %s: %s", image, msg), err: err}
	}
	ps.failures++
	ps.err = err
	ps.next = a.cfg.Clock().Add(pullBackoff(ps.failures))
	a.log.Warn("pull failed", "image", image, "failures", ps.failures, "next", ps.next.UTC(), "err", err)
	return &pullError{err: err, next: ps.next}
}

// refused words a refused pull for the replica's status. The runtime's
// message for a refusal is its own, fixed text, with the reason in
// parentheses; nothing the registry said is in it.
func refused(image, host string, sent bool, err error) error {
	login := "hivemind registry login " + host
	var msg string
	switch ambiguous := errors.Is(err, runtime.ErrNotFound); {
	case ambiguous && sent:
		msg = fmt.Sprintf("pulling %s: %s has no such image, or the stored credential cannot pull it; check the image name, or run: %s", image, host, login)
	case ambiguous:
		msg = fmt.Sprintf("pulling %s: %s has no such image, or it is private; check the image name, or run: %s", image, host, login)
	case sent:
		reason := "refused"
		var re *runtime.Error
		if errors.As(err, &re) {
			if _, r, ok := strings.Cut(re.Message, "("); ok {
				reason = strings.TrimSuffix(r, ")")
			}
		}
		msg = fmt.Sprintf("pulling %s: %s refused the stored credential (%s); run: %s", image, host, reason, login)
	default:
		msg = fmt.Sprintf("pulling %s: %s wants a credential, and none is stored for it; run: %s", image, host, login)
	}
	return &pullFailure{msg: msg, err: err}
}

// forgetPulls drops the state of pulls no replica needed in this pass, and
// cancels such a pull in flight: its spec was fixed, or its app deleted. It
// returns when the next pull that waits out a backoff is due, or zero. A pull
// in flight needs no wake: it triggers a pass when it ends.
func (a *Agent) forgetPulls() time.Time {
	var next time.Time
	for k, ps := range a.pulls {
		if !ps.seen {
			if ps.cancel != nil {
				ps.cancel()
				a.log.Info("cancelled a pull no replica needs any more", "image", k.image)
			}
			delete(a.pulls, k)
			continue
		}
		ps.seen = false
		if ps.done == nil && (next.IsZero() || ps.next.Before(next)) {
			next = ps.next
		}
	}
	return next
}

// cancelPulls stops every pull in flight, and forgets them.
func (a *Agent) cancelPulls() {
	for k, ps := range a.pulls {
		if ps.cancel != nil {
			ps.cancel()
			delete(a.pulls, k)
		}
	}
}
