package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/pki"
)

// workerTrustNext is the staging name of ca.crt while the worker replaces
// it. A stale one, from a crash, is removed at start.
const workerTrustNext = "ca.crt.next"

// caSetRetry is how long the worker waits to read the CA set again after
// a failure, and caSetCheck how often it reads it with no change seen.
const (
	caSetRetry = 30 * time.Second
	caSetCheck = 10 * time.Minute
)

// caSetClient is the part of the node API client the trust keeper uses.
type caSetClient interface {
	CABundle(ctx context.Context, nodeID string) (nodeapi.CABundle, *x509.Certificate, error)
	SetTLSConfig(cfg *tls.Config)
	ResetConnections()
}

// trustKeeper keeps the CAs a worker trusts in step with the server's CA
// set, through a rotation of the cluster CA (docs/ca-rotation.md). The set
// is in ca.crt, one PEM certificate each, the CA of the node's own
// certificate first. Every request says which CAs the worker trusts
// (header); every reply has the digest of the server's set (seen), and a
// digest the keeper has not applied makes it read the set, check it, and
// change ca.crt and the client's TLS config. In the renew phase it asks
// the renewer to renew under the CA the set names.
type trustKeeper struct {
	dir    string
	nodeID string
	host   string // the server's name, which its certificate must have
	holder *pki.CertHolder
	trust  atomic.Pointer[pki.Trust]
	client caSetClient
	log    *slog.Logger
	now    func() time.Time

	applied atomic.Value // string: the digest of the last set applied
	want    chan struct{}
	// mu serialises apply.
	mu sync.Mutex
	// target is the CA the server's set says to renew under now, or nil;
	// renew receives a value when it is set.
	target atomic.Pointer[[32]byte]
	renew  chan struct{}
}

func newTrustKeeper(dir, nodeID, host string, trust *pki.Trust, holder *pki.CertHolder, log *slog.Logger) *trustKeeper {
	k := &trustKeeper{dir: dir, nodeID: nodeID, host: host, holder: holder, log: log, now: time.Now,
		want: make(chan struct{}, 1), renew: make(chan struct{}, 1)}
	k.trust.Store(trust)
	k.applied.Store("")
	return k
}

// Trust is the set the worker trusts now.
func (k *trustKeeper) Trust() *pki.Trust { return k.trust.Load() }

// header is the CATrustHeader value of every request.
func (k *trustKeeper) header() string { return nodeapi.FormatCATrust(k.trust.Load().Fingerprints()) }

// seen takes the CAHeader of a reply. It must not block.
func (k *trustKeeper) seen(digest string) {
	if digest == k.applied.Load().(string) {
		return
	}
	select {
	case k.want <- struct{}{}:
	default:
	}
}

// renewUnder is the CA the server's set says to renew under now, when the
// node's certificate is from another one, or nil.
func (k *trustKeeper) renewUnder(leaf *x509.Certificate) *[32]byte {
	t := k.target.Load()
	if t == nil {
		return nil
	}
	if iss := k.trust.Load().Issuer(leaf); iss != nil && pki.Fingerprint(iss) == *t {
		return nil
	}
	return t
}

// renewNow receives a value when the set asks for a renewal now.
func (k *trustKeeper) renewNow() <-chan struct{} { return k.renew }

// run reads and applies the server's CA set when a reply shows one the
// keeper has not applied, and every caSetCheck, until ctx ends. A failure
// is logged and tried again after caSetRetry.
func (k *trustKeeper) run(ctx context.Context) {
	var failed time.Time
	for {
		t := time.NewTimer(caSetCheck)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-k.want:
		case <-t.C:
		}
		t.Stop()
		if wait := caSetRetry - k.now().Sub(failed); !failed.IsZero() && wait > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
		if err := k.apply(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			failed = k.now()
			k.log.Warn("reading the server's cluster CA set; trying again", "err", err, "retry", caSetRetry)
			select {
			case k.want <- struct{}{}:
			default:
			}
			continue
		}
		failed = time.Time{}
	}
}

// apply reads the server's CA set and makes it the worker's. A CA the
// worker does not trust yet is added only when its cross-certificate is
// signed by one it trusts; one it cannot check that way is left out. The
// CA of the node's own certificate is never removed, nor the CA the
// set's own connection was checked against, and the set is never empty.
func (k *trustKeeper) apply(ctx context.Context) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	var (
		b      nodeapi.CABundle
		parsed []nodeapi.ParsedCA
		next   *pki.Trust
		added  []*x509.Certificate
	)
	cur := k.trust.Load()
	own := cur.Issuer(k.holder.Leaf())
	for attempt := 0; ; attempt++ {
		var root *x509.Certificate
		var err error
		b, root, err = k.client.CABundle(ctx, k.nodeID)
		if errors.Is(err, nodeapi.ErrNoRoute) {
			// A server older than rotation has no set to follow.
			k.applied.Store("none")
			return nil
		}
		if err != nil {
			return err
		}
		if parsed, err = b.Parse(); err != nil {
			return err
		}
		if next, added, err = k.plan(cur, own, parsed); err != nil {
			return err
		}
		if next.Contains(pki.Fingerprint(root)) {
			break
		}
		if attempt == 0 {
			// The connection may be from before the server's certificate
			// moved to another CA: read the set again on a new one.
			k.client.ResetConnections()
			continue
		}
		return fmt.Errorf("the server's CA set leaves out %s, the CA of the certificate it presents; not applied", shortFP(pki.Fingerprint(root)))
	}
	if own != nil && !inSet(parsed, pki.Fingerprint(own)) {
		k.log.Warn("the server's CA set no longer has the CA of this node's certificate, so the server will refuse it; "+
			"this worker keeps trusting that CA", "ca", shortFP(pki.Fingerprint(own)))
	}
	if !next.Same(cur) {
		written, err := k.write(next)
		if err != nil {
			return err
		}
		next = written
		tlsCfg, err := pki.ClientTLSConfigTrust(next, k.holder, k.host)
		if err != nil {
			return err
		}
		k.trust.Store(next)
		k.client.SetTLSConfig(tlsCfg)
		for _, c := range added {
			k.log.Info("this worker trusts a new cluster CA, cross-signed by one it trusted; compare its fingerprint with hivemind ca status on the server",
				"ca", pki.FingerprintHex(pki.Fingerprint(c)), "name", c.Subject.CommonName)
		}
		for _, c := range cur.CAs() {
			if !next.Contains(pki.Fingerprint(c)) {
				k.log.Info("this worker no longer trusts a cluster CA, which the server's set has dropped", "ca", pki.FingerprintHex(pki.Fingerprint(c)))
			}
		}
	}
	k.target.Store(nil)
	if b.Renew != "" {
		fp, err := pki.ParseFingerprintHex(b.Renew)
		if err == nil && next.Contains(fp) {
			k.target.Store(&fp)
			if k.renewUnder(k.holder.Leaf()) != nil {
				select {
				case k.renew <- struct{}{}:
				default:
				}
			}
		}
	}
	k.applied.Store(b.Digest())
	return nil
}

// plan works out the set to trust from the server's: the CA of the node's
// own certificate, every CA of the server's set the worker trusts already,
// and every new one whose cross-certificate checks against a CA it trusts.
// It returns the new CAs too.
func (k *trustKeeper) plan(cur *pki.Trust, own *x509.Certificate, parsed []nodeapi.ParsedCA) (*pki.Trust, []*x509.Certificate, error) {
	now := k.now()
	var keep, added []*x509.Certificate
	if own != nil {
		keep = append(keep, own)
	}
	for _, p := range parsed {
		fp := pki.Fingerprint(p.Cert)
		switch {
		case cur.Contains(fp):
			keep = append(keep, p.Cert)
		case p.Cross == nil:
			k.log.Debug("the server's CA set has a CA this worker does not trust and cannot check; leaving it out", "ca", shortFP(fp))
		default:
			if err := pki.CheckValidAt(p.Cert, now); err != nil {
				return nil, nil, fmt.Errorf("the new CA %s in the server's set: %w", shortFP(fp), err)
			}
			if _, err := pki.VerifyCross(p.Cert, p.Cross, cur, now); err != nil {
				if errors.Is(err, pki.ErrCrossUntrusted) {
					k.log.Debug("the server's CA set has a CA cross-signed by one this worker does not trust; leaving it out", "ca", shortFP(fp))
					continue
				}
				return nil, nil, fmt.Errorf("the new CA %s in the server's set: %w", shortFP(fp), err)
			}
			keep = append(keep, p.Cert)
			added = append(added, p.Cert)
		}
	}
	next, err := pki.NewTrust(keep...)
	if err != nil {
		return nil, nil, fmt.Errorf("the server's CA set: %w", err)
	}
	return next, added, nil
}

func inSet(ps []nodeapi.ParsedCA, fp [32]byte) bool {
	for _, p := range ps {
		if pki.Fingerprint(p.Cert) == fp {
			return true
		}
	}
	return false
}

// ownFirst returns t with the CA of the node's certificate first, the order
// of ca.crt, and the rest in t's order. t itself when it is so already, or
// when no CA of t issued the certificate.
func (k *trustKeeper) ownFirst(t *pki.Trust) *pki.Trust {
	cas := t.CAs()
	own := t.Issuer(k.holder.Leaf())
	if own == nil || pki.Fingerprint(cas[0]) == pki.Fingerprint(own) {
		return t
	}
	ordered := []*x509.Certificate{own}
	for _, c := range cas {
		if pki.Fingerprint(c) != pki.Fingerprint(own) {
			ordered = append(ordered, c)
		}
	}
	return pki.MustTrust(ordered...)
}

// write replaces ca.crt with the set, the CA of the node's certificate
// first: written to ca.crt.next, synced, then renamed, so ca.crt is never
// seen half written. It returns the set as written, which the caller keeps
// as the worker's: renewed reads the order from it. In the fifth soak the
// worker kept the set in the order it had read it, and after an abort in
// the renew phase, a renewal back under the CA that set had first found
// nothing to reorder, and left the dropped CA first in ca.crt until finish.
func (k *trustKeeper) write(t *pki.Trust) (*pki.Trust, error) {
	t = k.ownFirst(t)
	next := filepath.Join(k.dir, workerTrustNext)
	_ = os.Remove(next)
	if err := writeSynced(next, t.PEM()); err != nil {
		return nil, fmt.Errorf("saving the cluster CAs: %w", err)
	}
	if err := os.Rename(next, filepath.Join(k.dir, pki.CACertFile)); err != nil {
		return nil, fmt.Errorf("saving the cluster CAs: %w", err)
	}
	return t, syncDir(k.dir)
}

// renewed puts the CA of the node's new certificate first in ca.crt, so
// that the file reads as it would after a join to that CA. The worker calls
// it after each renewal, and at its start, for a renewal whose reorder a
// crash cut short.
func (k *trustKeeper) renewed() {
	k.mu.Lock()
	defer k.mu.Unlock()
	t := k.trust.Load()
	if k.ownFirst(t) == t {
		return
	}
	written, err := k.write(t)
	if err != nil {
		k.log.Warn("reordering the cluster CAs after a renewal", "err", err)
		return
	}
	k.trust.Store(written)
}

// removeStaleTrust removes a ca.crt.next a crash left.
func removeStaleTrust(dir string) error {
	err := os.Remove(filepath.Join(dir, workerTrustNext))
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func shortFP(fp [32]byte) string { return pki.FingerprintHex(fp)[:16] }
