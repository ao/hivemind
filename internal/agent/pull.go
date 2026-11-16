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

// pullState is the failures of one pullKey.
type pullState struct {
	failures int
	next     time.Time // no pull before this
	err      error     // the last failure
	seen     bool      // a replica needed the pull in this pass
}

// pullError is a failed pull, and when the next one is due. Waiting is true
// when no pull was made in this pass, because the last one failed less than
// its backoff ago; the pass does not count that as an error.
type pullError struct {
	err     error
	next    time.Time
	waiting bool
}

func (e *pullError) Error() string {
	return fmt.Sprintf("%v; next pull after %s", e.err, e.next.UTC().Format("15:04:05 UTC"))
}

func (e *pullError) Unwrap() error { return e.err }

// pullRefused is a pull the registry refused, in words that name the
// registry and the command that stores a credential for it. It holds
// nothing the registry said.
type pullRefused struct {
	msg string
	err error
}

func (e *pullRefused) Error() string { return e.msg }
func (e *pullRefused) Unwrap() error { return e.err }

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

// pull fetches image for as, unless a pull of it with the same credential
// failed less than its backoff ago. It returns nil or a *pullError.
func (a *Agent) pull(ctx context.Context, as store.Assignment, image string, log *slog.Logger) error {
	auth, host := registryAuth(as, image, log)
	key := pullKey{image: image}
	if auth != nil {
		key.cred = sha256.Sum256([]byte(auth.Username + "\x00" + auth.Password))
	}
	now := a.cfg.Clock()
	ps := a.pulls[key]
	if ps != nil {
		ps.seen = true
		if now.Before(ps.next) {
			return &pullError{err: ps.err, next: ps.next, waiting: true}
		}
	}
	log.Info("pulling image", "image", image, "logged_in", auth != nil)
	err := a.cfg.Runtime.Pull(ctx, image, auth)
	if err == nil {
		delete(a.pulls, key)
		return nil
	}
	if ctx.Err() != nil {
		return err
	}
	if errors.Is(err, runtime.ErrUnauthorized) {
		err = refused(image, host, auth != nil, err)
	}
	if ps == nil {
		ps = &pullState{seen: true}
		a.pulls[key] = ps
	}
	ps.failures++
	ps.err = err
	ps.next = now.Add(pullBackoff(ps.failures))
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
	return &pullRefused{msg: msg, err: err}
}

// forgetPulls drops the state of pulls no replica needed in this pass, and
// returns when the next one that waits is due, or zero.
func (a *Agent) forgetPulls() time.Time {
	var next time.Time
	for k, ps := range a.pulls {
		if !ps.seen {
			delete(a.pulls, k)
			continue
		}
		ps.seen = false
		if next.IsZero() || ps.next.Before(next) {
			next = ps.next
		}
	}
	return next
}
