package nodeapi

import (
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"
)

// MaxClockSkew is how far a node's clock may be from the server's before the
// worker warns. Certificates start 5 minutes before they are issued
// (pki.clockSkew), so a clock that is behind the issuer's by more than that
// refuses a new certificate as not valid yet. A clock that is ahead breaks
// nothing until it passes a certificate's expiry, but every time a node
// reports, such as when a crash-looping container's next restart is due,
// is off by as much.
const MaxClockSkew = time.Minute

// clock keeps the last measured difference between this machine's clock and
// the server's, from the Date header of the server's replies.
type clock struct {
	skew atomic.Int64 // this machine minus the server, in nanoseconds
	seen atomic.Bool
}

// observe measures the skew from one reply. Date has whole seconds, and the
// reply took some time to arrive, so the skew is taken against the middle of
// the request and rounded to the second.
func (c *clock) observe(sent, got time.Time, resp *http.Response) {
	d, err := http.ParseTime(resp.Header.Get("Date"))
	if err != nil {
		return
	}
	mid := sent.Add(got.Sub(sent) / 2)
	c.skew.Store(int64(mid.Sub(d.Add(500 * time.Millisecond)).Round(time.Second)))
	c.seen.Store(true)
}

// ClockSkew returns how far this machine's clock is ahead of the server's,
// negative when it is behind, from the last reply of the server. ok is false
// before the first reply.
func (c *Client) ClockSkew() (skew time.Duration, ok bool) {
	return time.Duration(c.clock.skew.Load()), c.clock.seen.Load()
}

// clockHint adds what to check to an error from a TLS handshake that failed
// because a certificate is not valid at this machine's time: expired, or
// not valid yet, which is what a clock that is wrong by hours looks like.
func clockHint(err error) error {
	var ua x509.UnknownAuthorityError
	if errors.As(err, &ua) {
		// The server's certificate is from a CA this node does not trust.
		return fmt.Errorf("%w; the server's certificate is from a CA this worker does not trust: the address is another cluster's server, "+
			"or this cluster's CA was rotated without this node (hivemind ca rotate finish --force). For the second, run "+
			"`hivemind node remove <id>` on the server, then join this machine again with an empty --data-dir and a new join token", err)
	}
	var ie x509.CertificateInvalidError
	if !errors.As(err, &ie) || ie.Reason != x509.Expired {
		return err
	}
	return fmt.Errorf("%w; this machine's clock reads %s: if it is wrong, set it right (NTP) on this machine and on the server",
		err, time.Now().UTC().Format(time.RFC3339))
}
