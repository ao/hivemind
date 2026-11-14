package cluster

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/store"
)

// certNotes remembers, per node, the latest certificate expiry already
// recorded, so a request with a certificate the store already knows costs no
// write.
type certNotes struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func (n *certNotes) known(id string, notAfter time.Time) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return !notAfter.After(n.seen[id])
}

func (n *certNotes) note(id string, notAfter time.Time) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.seen == nil {
		n.seen = map[string]time.Time{}
	}
	if notAfter.After(n.seen[id]) {
		n.seen[id] = notAfter
	}
}

// requireCurrentCert refuses, with 401, a request whose client certificate is
// not valid at the server's clock. The TLS handshake checks this once, when
// the connection opens; a worker keeps its connection open, and reuses it for
// as long as it keeps calling, so without this check a certificate that
// expired after the handshake would keep working. It also records the
// certificate's expiry when it is newer than the store's record, which covers
// nodes that joined before the server recorded expiries.
func (a *nodeAPI) requireCurrentCert(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			writeError(w, http.StatusUnauthorized, "no client certificate")
			return
		}
		leaf := r.TLS.PeerCertificates[0]
		if err := pki.CheckValidAt(leaf, a.now()); err != nil {
			id, _ := pki.NodeIDFromCert(leaf)
			a.log.Warn("node request with a certificate that is not valid refused", "node", id,
				"remote", r.RemoteAddr, "not_after", leaf.NotAfter.UTC(), "reason", err.Error())
			writeError(w, http.StatusUnauthorized, nodeapi.CertInvalidMessage)
			return
		}
		if id, err := pki.NodeIDFromCert(leaf); err == nil && !a.certs.known(id, leaf.NotAfter) {
			switch err := a.st.RecordNodeCert(r.Context(), id, leaf.NotAfter); {
			case err == nil:
				a.certs.note(id, leaf.NotAfter)
			case errors.Is(err, store.ErrNodeRemoved):
				// denyRemoved answers this request.
			default:
				a.log.Warn("recording a node certificate's expiry", "node", id, "err", err)
			}
		}
		next.ServeHTTP(w, r)
	})
}

// renew issues a new certificate to the node that calls it, for the public
// key in the request. The node ID is the one in the client certificate,
// which the route has checked against the path; nothing in the request can
// name another node. The certificate it presents must be valid now
// (requireCurrentCert), and the node must not be removed (denyRemoved, and
// again in the transaction that records the new certificate). The new
// certificate has the same lifetime as one issued at join.
func (a *nodeAPI) renew(w http.ResponseWriter, r *http.Request) {
	id, ok := a.node(w, r)
	if !ok {
		return
	}
	var req nodeapi.RenewRequest
	if err := nodeapi.Decode(r.Body, nodeapi.MaxRenewBytes, &req); err != nil {
		writeDecodeError(w, err)
		return
	}
	der, err := a.ca.IssueNodeCert(req.CSR, pki.NodeCertOptions{NodeID: id, Lifetime: a.lifetime, Now: a.now()})
	if err != nil {
		if errors.Is(err, pki.ErrInvalidCSR) {
			writeError(w, http.StatusBadRequest, "the certificate request is invalid")
			return
		}
		a.log.Error("renewing a node certificate", "node", id, "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if err := a.st.RecordNodeCert(r.Context(), id, leaf.NotAfter); err != nil {
		a.fail(w, r, err)
		return
	}
	a.certs.note(id, leaf.NotAfter)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	writeJSON(w, http.StatusOK, nodeapi.RenewReply{CertPEM: string(certPEM)})
	a.log.Info("node certificate renewed", "node", id, "not_after", leaf.NotAfter.UTC(), "remote", r.RemoteAddr)
}

// recordIssued records the expiry of a certificate issued at join. The join
// has succeeded by then, so a failure is logged and not returned: the
// record is shown by hivemind nodes, and the node's first request fills it in.
func recordIssued(r *http.Request, cfg Config, log *slog.Logger, id string, der []byte) {
	leaf, err := x509.ParseCertificate(der)
	if err == nil {
		err = cfg.Store.RecordNodeCert(r.Context(), id, leaf.NotAfter)
	}
	if err != nil {
		log.Warn("recording a node certificate's expiry", "node", id, "err", err)
	}
}

// serverCertSource issues the server's own certificate for the advertise
// address and renews it. It is signed by the cluster CA and carries the
// server's node ID.
type serverCertSource struct {
	cfg    Config
	holder *pki.CertHolder
	log    *slog.Logger
	now    func() time.Time
	// caWarned is when the CA expiry was last logged.
	caWarned time.Time
}

func (s *serverCertSource) issue() (tls.Certificate, error) {
	key, err := pki.NewNodeKey()
	if err != nil {
		return tls.Certificate{}, err
	}
	csr, err := pki.NewCSR(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	der, err := s.cfg.CA.IssueNodeCert(csr, pki.NodeCertOptions{NodeID: s.cfg.LocalNodeID, Advertise: s.cfg.Advertise,
		Lifetime: s.cfg.NodeCertLifetime, Now: s.now()})
	if err != nil {
		return tls.Certificate{}, err
	}
	cert, err := pki.NewTLSCertificate(der, key, s.cfg.CA)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := s.cfg.Store.RecordNodeCert(context.Background(), s.cfg.LocalNodeID, cert.Leaf.NotAfter); err != nil {
		s.log.Warn("recording the server certificate's expiry", "err", err)
	}
	return cert, nil
}

// check renews the server certificate when it is due, and warns once a day
// while the CA has less than pki.CAWarnBefore left.
func (s *serverCertSource) check() {
	now := s.now()
	ca := s.cfg.CA.Cert
	if left := ca.NotAfter.Sub(now); left < pki.CAWarnBefore && now.Sub(s.caWarned) >= 24*time.Hour {
		s.caWarned = now
		s.log.Warn("the cluster CA expires soon, and hivemind cannot rotate it; node certificates end with the CA, "+
			"and when it expires no node can reach the server. Before then, create a new cluster with init in a new "+
			"data directory and join every machine to it", "ca_not_after", ca.NotAfter.UTC(), "left", left.Round(time.Hour))
	}
	if !pki.RenewDue(s.holder.Leaf(), now) {
		return
	}
	cert, err := s.issue()
	if err != nil {
		s.log.Error("renewing the server certificate; the current one stays in use", "err", err,
			"not_after", s.holder.Leaf().NotAfter.UTC())
		return
	}
	s.holder.Store(cert)
	s.log.Info("server certificate renewed", "node", s.cfg.LocalNodeID, "not_after", cert.Leaf.NotAfter.UTC())
}

// run calls check every pki.RenewCheckInterval until ctx ends.
func (s *serverCertSource) run(ctx context.Context) {
	for {
		t := time.NewTimer(pki.RenewCheckInterval(s.holder.Leaf()))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		s.check()
	}
}
