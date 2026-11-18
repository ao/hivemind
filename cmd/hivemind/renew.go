package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/pki"
)

// A renewal stages the new key and certificate under these names, then
// renames them over node.key and node.crt. recoverRenewal finishes or drops
// a renewal that a crash interrupted.
const (
	workerKeyNext  = "node.key.next"
	workerCertNext = "node.crt.next"
)

// certExpiredError is what a worker reports when its certificate has
// expired. The server refuses an expired certificate, and renewal needs a
// valid one, so the node cannot recover by itself.
func certExpiredError(id string, notAfter time.Time) error {
	return fmt.Errorf("the certificate of node %s expired at %s, so the server refuses it and it cannot be renewed; "+
		"on the server, run `hivemind node remove %s`, then join this machine again with an empty --data-dir and a new join token. "+
		"If this machine's clock is wrong (it reads %s), set it right (NTP) instead, and start the worker again",
		id, notAfter.UTC().Format(time.RFC3339), id, time.Now().UTC().Format(time.RFC3339))
}

// certRenewer is the part of the node API client a renewal uses.
type certRenewer interface {
	Renew(ctx context.Context, nodeID string, csr []byte) ([]byte, error)
	ResetConnections()
}

// renewer keeps a worker's certificate current. It renews once two thirds
// of the certificate's lifetime have passed (pki.RenewDue), with a new key,
// over the node API, and authenticates with the current certificate. The new
// pair is written to the data directory and stored in holder, which the
// client's TLS config reads at each handshake, so the worker uses it without
// a restart. During a rotation of the cluster CA it also renews when the
// server's CA set says to renew under a CA its certificate is not from.
type renewer struct {
	dir    string
	nodeID string
	// trust gives the CAs the worker trusts now; a renewed certificate
	// must chain to one of them.
	trust  func() *pki.Trust
	holder *pki.CertHolder
	client certRenewer
	log    *slog.Logger
	now    func() time.Time
	// missing logs, once, that the server has no renewal route (it is older
	// than 2.1). Nil logs each failure as any other.
	missing *nodeapi.MissingRoute
	// rotation says when the CA set wants a renewal now. Nil: never.
	rotation rotationRenewals
}

// rotationRenewals is the part of the trust keeper the renewer uses.
type rotationRenewals interface {
	// renewUnder is the CA to renew under now, when the certificate leaf
	// is from another one, or nil.
	renewUnder(leaf *x509.Certificate) *[32]byte
	// renewNow receives a value when renewUnder may have changed.
	renewNow() <-chan struct{}
	// renewed is told after each renewal.
	renewed()
}

// renewIfDue renews when the certificate is due, or when the rotation asks
// for a certificate from another CA. It reports whether it renewed. An
// expired certificate is certExpiredError.
func (r *renewer) renewIfDue(ctx context.Context) (bool, error) {
	leaf := r.holder.Leaf()
	now := r.now()
	if now.After(leaf.NotAfter) {
		return false, certExpiredError(r.nodeID, leaf.NotAfter)
	}
	var under *[32]byte
	if r.rotation != nil {
		under = r.rotation.renewUnder(leaf)
	}
	if under == nil && !pki.RenewDue(leaf, now) {
		return false, nil
	}
	if under != nil {
		r.log.Info("renewing the node certificate under the cluster's new CA, as the CA rotation's renew phase asks",
			"ca", pki.FingerprintHex(*under)[:16])
	}
	return true, r.renew(ctx)
}

// renew gets a certificate for a new key and switches to it.
func (r *renewer) renew(ctx context.Context) error {
	key, err := pki.NewNodeKey()
	if err != nil {
		return err
	}
	csr, err := pki.NewCSR(key)
	if err != nil {
		return err
	}
	certPEM, err := r.client.Renew(ctx, r.nodeID, csr)
	if err != nil {
		return err
	}
	cert, err := checkRenewedCert(r.trust(), r.nodeID, key, certPEM, r.now())
	if err != nil {
		return fmt.Errorf("the server's renewed certificate: %w", err)
	}
	if err := writeRenewed(r.dir, key, certPEM); err != nil {
		return fmt.Errorf("saving the renewed certificate: %w", err)
	}
	r.holder.Store(cert)
	// Open connections keep the old certificate, busy ones included, such
	// as the log poll's. Move every later request to new connections, which
	// handshake with the new one.
	r.client.ResetConnections()
	if r.rotation != nil {
		r.rotation.renewed()
	}
	return nil
}

// run checks the certificate at start and then every
// pki.RenewCheckInterval, and when the rotation asks, until ctx ends. A
// failure is logged and tried again at the next check.
func (r *renewer) run(ctx context.Context) {
	var now <-chan struct{}
	if r.rotation != nil {
		now = r.rotation.renewNow()
	}
	for {
		old := r.holder.Leaf()
		renewed, err := r.renewIfDue(ctx)
		next := pki.RenewCheckInterval(r.holder.Leaf())
		switch {
		case err != nil && ctx.Err() != nil:
			return
		case errors.Is(err, nodeapi.ErrNoRoute) && r.missing != nil:
			// The server cannot renew; it says so once, with the date that
			// matters, and the check is tried again on schedule.
			_ = r.missing.Check(err, "not_after", old.NotAfter.UTC())
		case err != nil:
			if r.rotation != nil && r.rotation.renewUnder(old) != nil {
				// A rotation is waiting on this renewal: try again soon.
				next = min(next, caSetRetry)
			}
			r.log.Error("renewing the node certificate failed; trying again at the next check", "err", err,
				"not_after", old.NotAfter.UTC(), "next_check", next)
		case renewed:
			r.log.Info("node certificate renewed", "node", r.nodeID, "old_not_after", old.NotAfter.UTC(),
				"not_after", r.holder.Leaf().NotAfter.UTC())
		}
		t := time.NewTimer(next)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-now:
			t.Stop()
		case <-t.C:
		}
	}
}

// checkRenewedCert checks what the server sent: one PEM certificate that
// chains to a cluster CA the worker trusts, for client auth, is valid at
// now, names this node, and is for key. It returns the TLS pair.
func checkRenewedCert(trust *pki.Trust, nodeID string, key *ecdsa.PrivateKey, certPEM []byte, now time.Time) (tls.Certificate, error) {
	b, rest := pem.Decode(certPEM)
	if b == nil || b.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return tls.Certificate{}, errors.New("not one PEM certificate")
	}
	leaf, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		return tls.Certificate{}, err
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: trust.Pool(), CurrentTime: now,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		var ie x509.CertificateInvalidError
		if errors.As(err, &ie) && ie.Reason == x509.Expired {
			// The server issued it a moment ago, so the clocks disagree.
			return tls.Certificate{}, fmt.Errorf("is valid from %s to %s, and this machine's clock reads %s: "+
				"this clock or the server's is wrong; set them right (NTP), and the renewal is tried again at the next check",
				leaf.NotBefore.UTC().Format(time.RFC3339), leaf.NotAfter.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
		}
		return tls.Certificate{}, fmt.Errorf("does not chain to the cluster CA: %w", err)
	}
	if id, err := pki.NodeIDFromCert(leaf); err != nil || id != nodeID {
		return tls.Certificate{}, fmt.Errorf("names node %q, not %q", id, nodeID)
	}
	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) {
		return tls.Certificate{}, errors.New("is not for the new key")
	}
	return pki.NewTLSCertificate(b.Bytes, key, &pki.CA{Cert: trust.Issuer(leaf)})
}

// writeRenewed replaces node.key and node.crt. Each new file is written in
// full, with mode 0600, and synced under a staging name, then renamed over
// the old one, so neither file is ever seen half written. The two renames
// are not one step: a crash between them leaves the new key with the old
// certificate, and recoverRenewal, which runs before the worker loads its
// files, finishes the job.
func writeRenewed(dir string, key *ecdsa.PrivateKey, certPEM []byte) error {
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	keyNext, certNext := filepath.Join(dir, workerKeyNext), filepath.Join(dir, workerCertNext)
	// Stale staging files are from a renewal that never got to its renames.
	_ = os.Remove(keyNext)
	_ = os.Remove(certNext)
	if err := writeSynced(keyNext, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})); err != nil {
		return err
	}
	if err := writeSynced(certNext, certPEM); err != nil {
		_ = os.Remove(keyNext)
		return err
	}
	if err := syncDir(dir); err != nil {
		return err
	}
	if err := os.Rename(keyNext, filepath.Join(dir, workerKeyFile)); err != nil {
		return err
	}
	if err := os.Rename(certNext, filepath.Join(dir, workerCertFile)); err != nil {
		return err
	}
	return syncDir(dir)
}

// recoverRenewal finishes or drops a renewal that a crash interrupted.
//
//   - node.key.next is there: no rename happened, node.key and node.crt are
//     the old pair. The staging files are removed.
//   - only node.crt.next is there: node.key is already the new key. The
//     certificate is renamed into place.
func recoverRenewal(dir string) error {
	keyNext, certNext := filepath.Join(dir, workerKeyNext), filepath.Join(dir, workerCertNext)
	_, kerr := os.Lstat(keyNext)
	_, cerr := os.Lstat(certNext)
	switch {
	case kerr == nil:
		if err := os.Remove(keyNext); err != nil {
			return err
		}
		if cerr == nil {
			if err := os.Remove(certNext); err != nil {
				return err
			}
		}
		return syncDir(dir)
	case !errors.Is(kerr, fs.ErrNotExist):
		return kerr
	case cerr == nil:
		if err := os.Rename(certNext, filepath.Join(dir, workerCertFile)); err != nil {
			return err
		}
		return syncDir(dir)
	case !errors.Is(cerr, fs.ErrNotExist):
		return cerr
	}
	return nil
}

// writeSynced creates path with mode 0600, writes data and syncs it. path
// must not exist.
func writeSynced(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	werr := f.Chmod(0o600)
	if werr == nil {
		_, werr = f.Write(data)
	}
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(path)
	}
	return werr
}

// syncDir syncs a directory, so renames and new names in it are on disk.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	serr := d.Sync()
	if cerr := d.Close(); serr == nil {
		serr = cerr
	}
	return serr
}

// The worker passes its node API client as the renewer's client.
var _ certRenewer = (*nodeapi.Client)(nil)
