package ha

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/http/httputil"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ao/hivemind/internal/pki"
)

// What a follower passes to the leader (docs/ha.md#clients). The follower
// sends the request as it came, over the server port, with what the leader
// cannot see for itself in headers of its own: what kind of request it is,
// the caller's address, whether the caller came over TLS, and, for a node
// request, the worker's certificate chain, which the leader verifies
// again. The leader takes these headers on the server port's forward
// protocol only, from a peer with the server role; the API and the cluster
// listeners delete them from every request they get (StripForwardHeaders).

// Kinds of forwarded request.
const (
	KindAPI     = "api"
	KindNode    = "node"
	KindJoin    = "join"
	kindPromote = "promote"
)

// Forwarding headers.
const (
	headerPrefix = "Hivemind-Forward-"
	HeaderKind   = headerPrefix + "Kind"
	HeaderRemote = headerPrefix + "Remote"
	HeaderTLS    = headerPrefix + "Tls"
	HeaderChain  = headerPrefix + "Chain"
	// HeaderNoLeader marks a 503 that says no server leads, or that the
	// server asked no longer does: a client tries another server.
	HeaderNoLeader = "Hivemind-No-Leader"
)

// StripForwardHeaders deletes every forwarding header from h. The API and
// the cluster listeners call it on every request, so that a caller cannot
// claim another address or another node's certificate.
func StripForwardHeaders(h http.Header) {
	for k := range h {
		if strings.HasPrefix(http.CanonicalHeaderKey(k), headerPrefix) {
			delete(h, k)
		}
	}
}

// Handlers serve what followers pass to this server once it leads.
type Handlers struct {
	API  http.Handler
	Node http.Handler
	Join http.Handler
}

// SetHandlers sets what serves forwarded requests.
func (n *Node) SetHandlers(h Handlers) { n.handlers.Store(&h) }

// maxChainHeader bounds the certificate chain a forwarded node request
// carries.
const maxChainHeader = 16 << 10

// receiver serves the server port's forward protocol.
func (n *Node) receiver() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := peerState(r.Context())
		var peer string
		var err error
		if st != nil {
			peer, err = pki.PeerNodeID(*st)
		}
		if st == nil || err != nil || len(st.PeerCertificates) == 0 || !pki.HasServerRole(st.PeerCertificates[0]) {
			writeJSONError(w, http.StatusForbidden, "only a server of this cluster may use the server port")
			return
		}
		kind := r.Header.Get(HeaderKind)
		if kind == kindPromote {
			n.servePromote(w, r, peer)
			return
		}
		// A request is passed on once at most: a server that does not
		// lead says so, and the follower's client tries again.
		if !n.IsLeader() {
			WriteNotLeader(w, n.notLeader())
			return
		}
		h := n.handlers.Load()
		if h == nil {
			WriteNotLeader(w, errors.New("the leader is starting"))
			return
		}
		out := r.Clone(r.Context())
		if remote := r.Header.Get(HeaderRemote); remote != "" {
			if _, _, err := net.SplitHostPort(remote); err == nil {
				out.RemoteAddr = remote
			}
		}
		viaTLS := r.Header.Get(HeaderTLS) == "1"
		chain := r.Header.Get(HeaderChain)
		StripForwardHeaders(out.Header)
		switch kind {
		case KindAPI:
			out.TLS = nil
			if viaTLS {
				out.TLS = &tls.ConnectionState{Version: tls.VersionTLS13, HandshakeComplete: true}
			}
			h.API.ServeHTTP(w, out)
		case KindNode:
			cs, err := n.verifyChain(chain)
			if err != nil {
				n.log.Warn("a forwarded node request with a certificate this server does not accept", "from", peer, "err", err)
				writeJSONError(w, http.StatusForbidden, "the node's certificate is not accepted")
				return
			}
			out.TLS = cs
			h.Node.ServeHTTP(w, out)
		case KindJoin:
			out.TLS = &tls.ConnectionState{Version: tls.VersionTLS13, HandshakeComplete: true}
			h.Join.ServeHTTP(w, out)
		default:
			writeJSONError(w, http.StatusBadRequest, "unknown forwarded request")
		}
	})
}

// verifyChain checks a forwarded worker's certificate chain against the
// cluster's CAs, for client use, and returns it as the connection state
// the node routes read.
func (n *Node) verifyChain(h string) (*tls.ConnectionState, error) {
	if h == "" || len(h) > maxChainHeader {
		return nil, errors.New("no certificate chain")
	}
	var certs []*x509.Certificate
	for _, part := range strings.Split(h, ",") {
		der, err := base64.StdEncoding.DecodeString(part)
		if err != nil {
			return nil, err
		}
		c, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, err
		}
		certs = append(certs, c)
	}
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	chains, err := certs[0].Verify(x509.VerifyOptions{Roots: n.cfg.Trust.Load().Pool(), Intermediates: inter,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	if err != nil {
		return nil, err
	}
	if _, err := pki.NodeIDFromCert(certs[0]); err != nil {
		return nil, err
	}
	return &tls.ConnectionState{Version: tls.VersionTLS13, HandshakeComplete: true, PeerCertificates: certs, VerifiedChains: chains}, nil
}

func encodeChain(certs []*x509.Certificate) string {
	parts := make([]string, len(certs))
	for i, c := range certs {
		parts[i] = base64.StdEncoding.EncodeToString(c.Raw)
	}
	return strings.Join(parts, ",")
}

// newForwardClient is the client for the server port's forward protocol.
// It speaks HTTP/1.1 over connections it opens itself, one TLS handshake
// each, with this server's certificate.
func (n *Node) newForwardClient() *http.Client {
	tr := &http.Transport{
		DialTLSContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return n.tlsm.dial(ctx, addr, ProtoForward)
		},
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{Transport: tr}
}

// Forward passes r to the leader as a request of kind, and writes the
// leader's answer to w as it comes. Without a leader it answers 503.
func (n *Node) Forward(kind string, w http.ResponseWriter, r *http.Request) {
	id, addr := n.Leader()
	if id == "" || id == n.cfg.ID {
		WriteNotLeader(w, n.notLeader())
		return
	}
	n.forwardTo(kind, id, addr, w, r)
}

// forwardTo is Forward to the leader id at addr.
func (n *Node) forwardTo(kind, id, addr string, w http.ResponseWriter, r *http.Request) {
	// The leader keeps its own time limits; a follower that kept its own
	// would cut a log stream through it.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
	// sent is set once the request is written to the connection to the
	// leader: until then, the leader cannot have acted on it.
	var sent atomic.Bool
	r = r.WithContext(httptrace.WithClientTrace(r.Context(), &httptrace.ClientTrace{WroteRequest: func(i httptrace.WroteRequestInfo) {
		if i.Err == nil {
			sent.Store(true)
		}
	}}))
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "https"
			pr.Out.URL.Host = addr
			pr.Out.Host = addr
			StripForwardHeaders(pr.Out.Header)
			pr.Out.Header.Set(HeaderKind, kind)
			pr.Out.Header.Set(HeaderRemote, pr.In.RemoteAddr)
			if pr.In.TLS != nil {
				pr.Out.Header.Set(HeaderTLS, "1")
				if kind == KindNode {
					pr.Out.Header.Set(HeaderChain, encodeChain(pr.In.TLS.PeerCertificates))
				}
			}
		},
		Transport:     n.fwdClient.Transport,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, out *http.Request, err error) {
			forwardFailed(w, out.Method, id, addr, sent.Load(), err)
		},
	}
	proxy.ServeHTTP(w, r)
}

// forwardFailed answers a request that the leader did not answer. It is
// marked no leader, so that a client sends it to another server, only when
// the leader cannot have acted on it: it did not reach the leader, or it
// is a GET or a HEAD, which change nothing. A write that reached the
// leader may have been applied, and a client that sent it again could
// apply it twice; the answer says so. Before, every such answer was
// marked, writes included.
func forwardFailed(w http.ResponseWriter, method, id, addr string, sent bool, err error) {
	if !sent || method == http.MethodGet || method == http.MethodHead {
		w.Header().Set(HeaderNoLeader, "1")
		w.Header().Set("Retry-After", "1")
		writeJSONError(w, http.StatusServiceUnavailable, fmt.Sprintf("the leader, %s at %s, did not answer this server: %v", id, addr, err))
		return
	}
	writeJSONError(w, http.StatusServiceUnavailable, fmt.Sprintf("the leader, %s at %s, did not answer this server after it was sent the request, "+
		"so the request may or may not have been applied: check, and try again if it was not: %v", id, addr, err))
}

// WriteNotLeader answers 503 for a request this server cannot serve
// without a leader, marked so that a client tries another server.
func WriteNotLeader(w http.ResponseWriter, err error) {
	w.Header().Set(HeaderNoLeader, "1")
	w.Header().Set("Retry-After", "1")
	writeJSONError(w, http.StatusServiceUnavailable, err.Error())
}

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// servePromote makes the peer, a non-voter that has caught up, a voter.
func (n *Node) servePromote(w http.ResponseWriter, r *http.Request, peer string) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if err := n.Promote(peer); err != nil {
		var nl *NotLeaderError
		if errors.As(err, &nl) {
			WriteNotLeader(w, err)
			return
		}
		writeJSONError(w, http.StatusConflict, err.Error())
		return
	}
	n.log.Info("a server that caught up is a voter now", "server", peer)
	w.WriteHeader(http.StatusNoContent)
}

// promoteEvery is how often a non-voter checks whether it has caught up.
const promoteEvery = 2 * time.Second

// promoteLoop asks the leader to make this server a voter once it has
// caught up, and stops once it is one.
func (n *Node) promoteLoop() {
	t := time.NewTicker(promoteEvery)
	defer t.Stop()
	asked := false
	for {
		select {
		case <-n.stop:
			return
		case <-t.C:
		}
		if n.IsVoter() {
			if asked {
				n.log.Info("this server is a voter of the cluster now", "id", n.cfg.ID)
			}
			return
		}
		if !n.CaughtUp() {
			n.log.Debug("this non-voter has not caught up yet", "commit", n.raft.CommitIndex(), "applied", n.raft.AppliedIndex(),
				"last_contact", n.raft.LastContact())
			continue
		}
		_, addr := n.Leader()
		if addr == "" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+addr+"/promote", nil)
		if err != nil {
			cancel()
			continue
		}
		req.Header.Set(HeaderKind, kindPromote)
		resp, err := n.fwdClient.Do(req)
		if err != nil {
			n.log.Debug("asking the leader to make this server a voter", "leader", addr, "err", err)
		}
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if !asked {
				n.log.Info("this server has caught up with the log, and asked the leader to make it a voter", "id", n.cfg.ID, "status", resp.StatusCode)
			}
			asked = true
		}
		cancel()
	}
}
