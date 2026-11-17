package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"

	"github.com/ao/hivemind/internal/agent"
	"github.com/ao/hivemind/internal/config"
	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/runtime"
	"github.com/ao/hivemind/internal/runtime/docker"
	"github.com/ao/hivemind/internal/store"
)

// A joined worker keeps four files in its data directory. node.json is written
// last, so its presence means the join finished.
const (
	workerStateFile = "node.json"
	workerKeyFile   = "node.key"
	workerCertFile  = "node.crt"
	// defaultClusterPort is the server's cluster port when the address has none.
	defaultClusterPort = 4484
	// registerRetry is how long a worker waits before registering again.
	registerRetry = 5 * time.Second
)

// errNotJoined means the data directory holds no joined node.
var errNotJoined = errors.New("this machine has not joined a cluster")

// workerState is node.json: the node's ID and the server it joined.
type workerState struct {
	NodeID string    `json:"node_id"`
	Server string    `json:"server"` // host:port of the server's cluster port
	Joined time.Time `json:"joined"`
}

// joinedWorker is a worker's state, read from disk and checked.
type joinedWorker struct {
	state  workerState
	tlsCfg *tls.Config
	// ca and holder are for renewal: tlsCfg reads the client certificate
	// from holder at each handshake.
	ca     *pki.CA
	holder *pki.CertHolder
}

func joinCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "join <server>",
		Short: "Join a cluster as a worker, then run its agent",
		Long: `Join a cluster as a worker node and run its agent in the foreground.

  hivemind join 10.0.0.5 --token <token>

<server> is the server's address, host or host:port, where the port is its
cluster port (4484 by default), without a scheme or a path: a URL such as
https://10.0.0.5 is refused. Port 4485, the default join port, is refused
before anything is dialled, since join dials the port above the one it is
given. The worker generates a key and a certificate
request, presents the join token to the server's join port (the cluster port
plus one), and checks the reply against the CA fingerprint in the token. It
then stores its key (mode 0600), its certificate, the cluster CA and its node
ID in the data directory, and starts the agent.

The token is single use and expires; get one with hivemind join-token on the
server. --token is read from the flag only, not from HIVEMIND_TOKEN, which is
the admin token.

A joined machine keeps its state. After a restart, run
"hivemind serve --role worker" with the same data directory. A worker has no
API listener, so --advertise does not apply to it.

The worker registers the local address of its connection to the server as
its private address; its routed replicas publish their ports there.
--route-addr <ip> also runs the HTTPS router on this machine, with
--acme-email, as on serve. It reads the routing table and keeps its
certificates through the server.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cmd.Flags(), config.Options{})
			if err != nil {
				return err
			}
			if cfg.Advertise != "" {
				return errors.New("--advertise does not apply to join: a worker runs no listener")
			}
			if cfg.ClusterAddr != "" {
				return errors.New("--cluster-addr does not apply to join: a worker runs no cluster listener")
			}
			token, _ := cmd.Flags().GetString("token")
			if token == "" {
				return errors.New("--token is required: run hivemind join-token on the server to get one")
			}
			host, port, err := splitServer(args[0])
			if err != nil {
				return err
			}
			// Given the join port itself, join would dial the port above
			// it and reach nothing, after the dial's timeout.
			if port == defaultClusterPort+1 {
				return fmt.Errorf("%s is the default join port, but join takes the server's cluster port, %s, or no port, and dials the join port above it",
					net.JoinHostPort(host, strconv.Itoa(port)), net.JoinHostPort(host, strconv.Itoa(defaultClusterPort)))
			}
			log, err := newLogger(cfg, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			slog.SetDefault(log)
			// The lock covers the join and the agent after it, so a second
			// join or serve on this directory is refused, not run beside.
			release, err := lockDataDir(cfg.DataDir)
			if err != nil {
				return err
			}
			defer release()
			if err := enroll(cmd.Context(), log, cfg.DataDir, host, port, token); err != nil {
				return err
			}
			rt, err := docker.New(cfg.DockerHost)
			if err != nil {
				return err
			}
			return runWorker(cmd.Context(), cfg, log, rt)
		},
	}
	config.Bind(cmd.Flags(), config.Options{})
	cmd.Flags().String("token", "", "join token from hivemind join-token on the server")
	_ = cmd.Flags().SetAnnotation("token", noEnvAnnotation, []string{"true"})
	cmd.Flags().Lookup("advertise").Hidden = true
	cmd.Flags().Lookup("cluster-addr").Hidden = true
	return cmd
}

// runWorkerServe is serve --role worker: it runs an already joined node,
// with the data directory locked.
func runWorkerServe(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	release, err := lockDataDir(cfg.DataDir)
	if err != nil {
		return err
	}
	defer release()
	rt, err := docker.New(cfg.DockerHost)
	if err != nil {
		return err
	}
	return runWorker(ctx, cfg, log, rt)
}

// splitServer returns the host and cluster port of a server address. An
// address without a port uses defaultClusterPort. A URL, with a scheme such
// as https://, or an address with a path is refused with the address to
// give instead.
func splitServer(arg string) (string, int, error) {
	if i := strings.Index(arg, "://"); i >= 0 {
		rest := arg[i+3:]
		if j := strings.IndexAny(rest, "/?#"); j >= 0 {
			rest = rest[:j]
		}
		return "", 0, fmt.Errorf("%s is a URL: give the address without %s://: %s", arg, arg[:i], firstNonEmpty(rest, "<host[:port]>"))
	}
	if i := strings.IndexAny(arg, "/?#"); i >= 0 {
		return "", 0, fmt.Errorf("%s has a path: give the address without it: %s", arg, firstNonEmpty(arg[:i], "<host[:port]>"))
	}
	host, port, err := net.SplitHostPort(arg)
	if err != nil {
		host, port = strings.TrimSuffix(strings.TrimPrefix(arg, "["), "]"), strconv.Itoa(defaultClusterPort)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65534 {
		return "", 0, fmt.Errorf("server port %q must be in 1-65534: the join port is one above it", port)
	}
	if host == "" {
		return "", 0, errors.New("server address is empty")
	}
	return host, n, nil
}

// enroll joins the cluster and saves the result in dir. It refuses to run in a
// directory that is already joined, before it uses the token.
func enroll(ctx context.Context, log *slog.Logger, dir, host string, port int, token string) error {
	if _, err := os.Lstat(filepath.Join(dir, workerStateFile)); err == nil {
		server := net.JoinHostPort(host, strconv.Itoa(port))
		if id, removed := joinedNodeRemoved(ctx, dir, server); removed {
			return fmt.Errorf("%s holds node %s, which the server at %s says was removed from the cluster, so these files cannot be used again: "+
				"move %s aside, or use another --data-dir, and run this join again; the token was not used", dir, id, server, dir)
		}
		return fmt.Errorf("%s already holds a joined node: run hivemind serve --role worker, or use another --data-dir", dir)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	// An epoch file without node.json is a node's that is gone, possibly
	// of another cluster; this node starts from epoch 0.
	if err := os.Remove(filepath.Join(dir, workerEpochFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	key, err := pki.NewNodeKey()
	if err != nil {
		return err
	}
	csr, err := pki.NewCSR(key)
	if err != nil {
		return err
	}
	joinAddr := net.JoinHostPort(host, strconv.Itoa(port+1))
	reply, err := nodeapi.Join(ctx, joinAddr, token, csr)
	if err != nil {
		return err
	}
	st := workerState{NodeID: reply.NodeID, Server: net.JoinHostPort(host, strconv.Itoa(port)), Joined: time.Now().UTC()}
	if err := saveWorkerState(dir, st, key, []byte(reply.CertPEM), []byte(reply.CAPEM)); err != nil {
		return err
	}
	log.Info("joined the cluster", "node", st.NodeID, "server", st.Server)
	return nil
}

// removedProbeTimeout bounds join's question to the server about the node a
// data directory already holds.
const removedProbeTimeout = 5 * time.Second

// joinedNodeRemoved asks the server whether the node joined in dir was
// removed, as after hivemind node remove, when the join is to the server
// that node joined. It reports the node's ID, and true only when the server
// says so with the node's own certificate.
func joinedNodeRemoved(ctx context.Context, dir, server string) (string, bool) {
	w, err := loadWorkerState(dir)
	if err != nil || w.state.Server != server {
		return "", false
	}
	c, err := nodeapi.NewClient("https://"+server, w.tlsCfg, nodeapi.WithVersion(version))
	if err != nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(ctx, removedProbeTimeout)
	defer cancel()
	_, err = c.StatusForNode(ctx, w.state.NodeID)
	return w.state.NodeID, errors.Is(err, nodeapi.ErrNodeRemoved)
}

// saveWorkerState writes the worker's files. Each is created with O_EXCL, so
// nothing is overwritten. On failure, the files written so far are removed.
func saveWorkerState(dir string, st workerState, key *ecdsa.PrivateKey, certPEM, caPEM []byte) (err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("data directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("data directory %s: %w", dir, err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	written := []string{}
	defer func() {
		if err != nil {
			for _, p := range written {
				_ = os.Remove(p)
			}
		}
	}()
	write := func(name string, data []byte, mode fs.FileMode) error {
		p := filepath.Join(dir, name)
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return fmt.Errorf("creating %s: %w", p, err)
		}
		written = append(written, p)
		_, werr := f.Write(data)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		return werr
	}
	if err := write(workerKeyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	if err := write(workerCertFile, certPEM, 0o644); err != nil {
		return err
	}
	if err := write(pki.CACertFile, caPEM, 0o644); err != nil {
		return err
	}
	stateJSON, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return write(workerStateFile, append(stateJSON, '\n'), 0o600)
}

// readWorkerState reads node.json in dir, and checks the node ID and the
// server address in it. A missing node.json is errNotJoined.
func readWorkerState(dir string) (workerState, error) {
	var st workerState
	raw, err := os.ReadFile(filepath.Join(dir, workerStateFile))
	if errors.Is(err, fs.ErrNotExist) {
		return st, fmt.Errorf("%w in %s: run hivemind join <server> --token <token> first", errNotJoined, dir)
	}
	if err != nil {
		return st, err
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&st); err != nil {
		return st, fmt.Errorf("%s is not valid: %w", workerStateFile, err)
	}
	if err := pki.ValidateNodeID(st.NodeID); err != nil {
		return st, fmt.Errorf("%s: %w", workerStateFile, err)
	}
	if _, _, err := net.SplitHostPort(st.Server); err != nil {
		return st, fmt.Errorf("%s: server %q: %w", workerStateFile, st.Server, err)
	}
	return st, nil
}

// workerHereError is what a command that needs a server says on a machine
// whose data directory dir holds a joined worker, or nil when it holds
// none. A worker has no API and no admin token: the commands that use them
// run on the server, or against its API from anywhere.
func workerHereError(dir, doing string) error {
	st, err := readWorkerState(dir)
	if err != nil {
		return nil
	}
	host, _, _ := net.SplitHostPort(st.Server)
	return fmt.Errorf("this machine is worker %s of the cluster at %s (%s), so %s here: run hivemind commands on the server, %s, or point this CLI at its API with --server and --token",
		st.NodeID, st.Server, filepath.Join(dir, workerStateFile), doing, host)
}

// loadWorkerState reads and checks a joined worker's files: the key must be
// private, the certificate must chain to the CA and name the node in
// node.json. A missing node.json is errNotJoined.
func loadWorkerState(dir string) (*joinedWorker, error) {
	st, err := readWorkerState(dir)
	switch {
	case errors.Is(err, errNotJoined):
		return nil, err
	case err != nil:
		// node.json names the node only when it is readable and valid;
		// otherwise the certificate may still say which node this was.
		id := st.NodeID
		if pki.ValidateNodeID(id) != nil {
			id = certNodeID(dir)
		}
		return nil, damaged(dir, id, err)
	}
	host, _, _ := net.SplitHostPort(st.Server)
	if err := recoverRenewal(dir); err != nil {
		return nil, fmt.Errorf("finishing an interrupted certificate renewal: %w", err)
	}

	keyPath := filepath.Join(dir, workerKeyFile)
	info, err := os.Lstat(keyPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, damaged(dir, st.NodeID, fmt.Errorf("%s is missing: without the node's private key, the node cannot prove to the server who it is", workerKeyFile))
	}
	if err != nil {
		return nil, damaged(dir, st.NodeID, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s must be a regular file readable by its owner only: run chmod 600 %s", keyPath, keyPath)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, damaged(dir, st.NodeID, err)
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil || kb.Type != "EC PRIVATE KEY" {
		return nil, damaged(dir, st.NodeID, fmt.Errorf("%s is not a PEM EC private key", workerKeyFile))
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, damaged(dir, st.NodeID, fmt.Errorf("%s: %w", workerKeyFile, err))
	}

	caPEM, err := os.ReadFile(filepath.Join(dir, pki.CACertFile))
	if err != nil {
		return nil, damaged(dir, st.NodeID, fmt.Errorf("reading the cluster CA: %w", err))
	}
	cb, _ := pem.Decode(caPEM)
	if cb == nil || cb.Type != "CERTIFICATE" {
		return nil, damaged(dir, st.NodeID, fmt.Errorf("%s is not a PEM certificate", pki.CACertFile))
	}
	caCert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, damaged(dir, st.NodeID, fmt.Errorf("%s: %w", pki.CACertFile, err))
	}
	ca := &pki.CA{Cert: caCert, CertPEM: caPEM}

	certPEM, err := os.ReadFile(filepath.Join(dir, workerCertFile))
	if err != nil {
		return nil, damaged(dir, st.NodeID, fmt.Errorf("reading the node's certificate: %w", err))
	}
	nb, _ := pem.Decode(certPEM)
	if nb == nil || nb.Type != "CERTIFICATE" {
		return nil, damaged(dir, st.NodeID, fmt.Errorf("%s is not a PEM certificate", workerCertFile))
	}
	leaf, err := x509.ParseCertificate(nb.Bytes)
	if err != nil {
		return nil, damaged(dir, st.NodeID, fmt.Errorf("%s: %w", workerCertFile, err))
	}
	if id, err := pki.NodeIDFromCert(leaf); err == nil && id == st.NodeID && time.Now().After(leaf.NotAfter) {
		return nil, certExpiredError(id, leaf.NotAfter)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		var ie x509.CertificateInvalidError
		if errors.As(err, &ie) && ie.Reason == x509.Expired {
			// The certificate has not expired, as checked above, so the
			// clock is before the start of its validity, or the CA's.
			return nil, fmt.Errorf("%s is valid from %s, the cluster CA from %s, and this machine's clock reads %s: "+
				"set the clock right (NTP), then start the worker again",
				workerCertFile, leaf.NotBefore.UTC().Format(time.RFC3339), caCert.NotBefore.UTC().Format(time.RFC3339),
				time.Now().UTC().Format(time.RFC3339))
		}
		return nil, damaged(dir, st.NodeID, fmt.Errorf("%s does not chain to the cluster CA in %s, so one of them was replaced: %w",
			workerCertFile, pki.CACertFile, err))
	}
	if id, err := pki.NodeIDFromCert(leaf); err != nil || id != st.NodeID {
		return nil, damaged(dir, st.NodeID, fmt.Errorf("%s names node %q, but %s names %q", workerCertFile, id, workerStateFile, st.NodeID))
	}
	// A key that is not the certificate's would load, and then fail every
	// handshake with the server.
	if pub, ok := leaf.PublicKey.(*ecdsa.PublicKey); !ok || !pub.Equal(&key.PublicKey) {
		return nil, damaged(dir, st.NodeID, fmt.Errorf("%s and %s do not belong together: the key is not the certificate's", workerKeyFile, workerCertFile))
	}
	tlsCert, err := pki.NewTLSCertificate(nb.Bytes, key, ca)
	if err != nil {
		return nil, damaged(dir, st.NodeID, err)
	}
	holder := pki.NewCertHolder(tlsCert)
	tlsCfg, err := pki.ClientTLSConfigFrom(ca, holder, host)
	if err != nil {
		return nil, err
	}
	return &joinedWorker{state: st, tlsCfg: tlsCfg, ca: ca, holder: holder}, nil
}

// damaged is the error of a worker data directory that cannot be used, with
// what to do about it. The node's files are written once, at the join, and
// renewal replaces only the certificate; a file that is missing or does not
// fit was changed by something else, and the worker cannot repair it. id is
// the node's ID, or "" when it is not known.
func damaged(dir, id string, err error) error {
	node := "the node (hivemind nodes lists it)"
	if id != "" {
		node = id
	}
	return fmt.Errorf("the data directory %s is damaged: %w. The worker cannot start with it. "+
		"Restore the directory from a backup of this machine, "+
		"or join again: run `hivemind node remove %s --force` on the server, then hivemind join here with an empty --data-dir and a new join token",
		dir, err, node)
}

// certNodeID is the node ID in the certificate in dir, or "".
func certNodeID(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, workerCertFile))
	if err != nil {
		return ""
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return ""
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return ""
	}
	id, err := pki.NodeIDFromCert(c)
	if err != nil {
		return ""
	}
	return id
}

// runWorker runs the agent for the joined node in dir until ctx ends. It
// registers the node first, and again whenever the server says it has no
// record of the node. Its client refuses a server from before a restore
// (workerEpochGuard): the worker then sends it nothing, keeps its
// containers, and asks again now and then.
func runWorker(ctx context.Context, cfg *config.Config, log *slog.Logger, rt runtime.Runtime) error {
	noteRouterSettings(cfg, log)
	w, err := loadWorkerState(cfg.DataDir)
	if err != nil {
		return err
	}
	client, err := nodeapi.NewClient("https://"+w.state.Server, w.tlsCfg, nodeapi.WithVersion(version),
		nodeapi.WithEpochGuard(workerEpochGuard(cfg.DataDir, log)))
	if err != nil {
		return err
	}
	renewCtx, stopRenew := context.WithCancel(ctx)
	defer stopRenew()
	renewLog := log.With("component", "renew")
	go (&renewer{dir: cfg.DataDir, nodeID: w.state.NodeID, ca: w.ca, holder: w.holder, client: client,
		log: renewLog, now: time.Now, missing: nodeapi.NewMissingRoute(client, "certificate renewal", renewLog)}).run(renewCtx)
	hostname, _ := os.Hostname()
	// addr is the node's private address, where its routed replicas publish
	// their ports. It is found before the first registration, and kept. A
	// server older than 2.2 refuses it; addrRefused records that, and the
	// worker registers it again once the server takes it (retryAddress).
	var addr string
	var addrRefused atomic.Bool
	// dials is the server address in node.json, which GET /v1/nodes shows
	// for this node.
	dials := w.state.Server
	if nodeapi.ValidateServerAddress(dials) != nil {
		dials = ""
	}
	register := func(ctx context.Context) error {
		info, err := rt.Info(ctx)
		if err != nil {
			return dockerReachError(cfg.DockerHost, err)
		}
		reg := nodeapi.Register{Name: hostname, CPU: info.NCPU, Memory: info.MemTotal, Address: addr, Server: dials}
		refused, err := registerWith(ctx, client, w.state.NodeID, reg)
		if err != nil {
			return err
		}
		switch was := addrRefused.Swap(refused); {
		case refused && !was:
			log.Warn("the server refused this node's address, as a server older than 2.2 does; this node takes no routed replicas until the server takes it",
				"address", addr)
		case !refused && was:
			log.Info("the server took this node's address; it takes routed replicas now", "address", addr)
		}
		return nil
	}
	for {
		var err error
		if addr, err = workerAddress(ctx, client, w.state.NodeID, log); err == nil {
			err = register(ctx)
		}
		if err == nil {
			break
		}
		if errors.Is(err, nodeapi.ErrNodeRemoved) {
			log.Error("the server says this node was removed from the cluster; stopping", "node", w.state.NodeID)
			return removedError(w.state.NodeID)
		}
		if errors.Is(err, fs.ErrPermission) {
			// A group the user joins takes effect at its next login, so
			// retrying in this process cannot help.
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		// A server from before a restore is logged once, by the client.
		if stale := (*nodeapi.StaleServerError)(nil); !errors.As(err, &stale) {
			log.Warn("registering with the server; retrying", "server", w.state.Server, "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(registerRetry):
		}
	}
	log.Info("worker running", "node", w.state.NodeID, "server", w.state.Server, "address", addr)
	go watchClock(renewCtx, client, log, time.Minute)
	// A removal is final, so the worker stops at the first refusal that says
	// so, rather than retrying for ever. Its containers are left running.
	runCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	agentLog := log.With("component", "agent")
	ag, err := agent.New(agent.Config{NodeID: w.state.NodeID, Runtime: rt,
		Source: workerSource{client: client, register: register, onRemoved: stop,
			lease: nodeapi.NewMissingRoute(client, "lease renewal between status reports", agentLog)},
		Interval: cfg.ReconcileInterval, Logger: agentLog, RouteBind: addr})
	if err != nil {
		return err
	}
	// Any reply from the server, to the lease, the router's table read or
	// the log poll, ends a pass's backoff after an outage: the containers
	// are reported at once, not up to 30 s after the server is back.
	client.OnAnswer(ag.ServerAnswered)
	defer client.OnAnswer(nil)
	// The router reads the routing table and keeps its certificates through
	// the node API, so it needs nothing from this machine but its listeners.
	if cfg.RouteAddr != "" {
		routes := nodeRoutes(client, w.state.NodeID, nodeapi.NewMissingRoute(client, "routing table", log.With("component", "router")))
		rtr, err := newRouter(cfg, log, routes, client.TLS(w.state.NodeID))
		if err != nil {
			return err
		}
		defer func() {
			if err := rtr.Shutdown(); err != nil {
				log.Warn("router shutdown", "err", err)
			}
		}()
		go func() {
			if err := rtr.Serve(); err != nil {
				log.Error("router stopped", "err", err)
				stop(fmt.Errorf("router: %w", err))
			}
		}()
		// A worker has no metrics listener: the server's /metrics shows its
		// router's counters.
		rlog := log.With("component", "router")
		go reportRouter(runCtx, client, w.state.NodeID, rtr.Report, routerReportEvery,
			nodeapi.NewMissingRoute(client, "router metrics", rlog), rlog)
	}
	// After a workers-first upgrade, the server that refused the address is
	// upgraded under the running worker, which then registers it.
	go retryAddress(runCtx, client, addrRefused.Load, register, addressRetry, log)
	// Log requests come over a poll the worker makes, so it needs no listener.
	logsDone := make(chan struct{})
	go func() {
		defer close(logsDone)
		ls := &nodeapi.LogServer{Client: client, NodeID: w.state.NodeID, Runtime: rt, Logger: log.With("component", "logs")}
		if err := ls.Run(runCtx); errors.Is(err, nodeapi.ErrNodeRemoved) {
			stop(err)
		}
	}()
	err = ag.Run(runCtx)
	stop(nil)
	<-logsDone
	cause := context.Cause(runCtx)
	if errors.Is(cause, nodeapi.ErrNodeRemoved) {
		log.Error("the server says this node was removed from the cluster; stopping", "node", w.state.NodeID)
		return removedError(w.state.NodeID)
	}
	if err == nil && ctx.Err() == nil && cause != nil && !errors.Is(cause, context.Canceled) {
		return cause // the router failed
	}
	return err
}

// skewReader is the part of the node API client that measures the clock.
type skewReader interface {
	ClockSkew() (time.Duration, bool)
}

// watchClock checks every period how far this machine's clock is from the
// server's, as the server's replies show it, and warns while it is more than
// nodeapi.MaxClockSkew off: at once, and then once an hour. A worker whose
// clock is hours off otherwise works on until a certificate falls outside
// its validity, and status times from it are off by as much.
func watchClock(ctx context.Context, c skewReader, log *slog.Logger, every time.Duration) {
	var warned time.Time
	check := func() {
		d, ok := c.ClockSkew()
		if !ok || (d <= nodeapi.MaxClockSkew && d >= -nodeapi.MaxClockSkew) {
			warned = time.Time{}
			return
		}
		if !warned.IsZero() && time.Since(warned) < time.Hour {
			return
		}
		warned = time.Now()
		if d > 0 {
			log.Warn(fmt.Sprintf("this machine's clock is %s ahead of the server's; times this node reports are off by as much, "+
				"and once it passes the expiry of the node's certificate the worker stops; run NTP on both machines", d), "skew", d)
			return
		}
		log.Warn(fmt.Sprintf("this machine's clock is %s behind the server's; a certificate the server issues starts 5 minutes "+
			"before its issue time, so this machine takes a renewed one as not valid yet; run NTP on both machines", -d), "skew", d)
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		check()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// registrar is the part of the node API client that registers a node.
type registrar interface {
	Register(ctx context.Context, nodeID string, g nodeapi.Register) error
}

// registerWith registers the node. A server older than 2.2 refuses the
// address and server fields with 400, as it refuses any field it does not
// know, so a refusal of a registration with either is tried once more
// without both: that server places no routed replicas anyway, and does not
// show what a node dials. addressRefused reports that the node had an
// address and is registered without it.
func registerWith(ctx context.Context, c registrar, nodeID string, reg nodeapi.Register) (addressRefused bool, err error) {
	err = c.Register(ctx, nodeID, reg)
	var refused *nodeapi.RefusedError
	if !errors.As(err, &refused) || refused.Code != http.StatusBadRequest || (reg.Address == "" && reg.Server == "") {
		return false, err
	}
	hadAddress := reg.Address != ""
	reg.Address, reg.Server = "", ""
	if err := c.Register(ctx, nodeID, reg); err != nil {
		return false, err
	}
	return hadAddress, nil
}

// addressRetry is how often a worker whose address the server refused
// registers again, when no reply shows the server's version changed first.
const addressRetry = 5 * time.Minute

// retryAddress registers the node again while refused says the server
// refused its address: at once when a reply shows the server's version
// changed, as after the server was upgraded, and every `every` otherwise.
// It runs until ctx ends.
func retryAddress(ctx context.Context, w nodeapi.ServerWatcher, refused func() bool, register func(context.Context) error,
	every time.Duration, log *slog.Logger) {
	for {
		changed := w.ServerChanged()
		t := time.NewTimer(every)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-changed:
		case <-t.C:
		}
		t.Stop()
		if !refused() {
			continue
		}
		if err := register(ctx); err != nil && ctx.Err() == nil {
			log.Info("registering this node's address again; trying later", "err", err)
		}
	}
}

// exitRemoved is the exit status of a worker that the server says was
// removed. It is its own, so a service manager can be told not to start the
// worker again (RestartPreventExitStatus=3 in docs/systemd.md): every start
// after a removal fails the same way.
const exitRemoved = 3

// removedError is what a worker exits with once the server says it was
// removed. The server refuses the node's certificate for good, so retrying
// cannot help. The containers are not touched: the server has already placed
// their replicas elsewhere, and the operator decides when they stop. They
// may have stopped already, as after a power cycle, so the message does not
// say they run.
func removedError(id string) error {
	return &exitError{code: exitRemoved, err: fmt.Errorf("node %s was removed from the cluster, and the server refuses its certificate; "+
		"its containers may still be on this machine (remove them with: docker rm -f $(docker ps -aq --filter label=hivemind.app)); "+
		"to join again, use an empty --data-dir and a new join token", id)}
}

// workerClient is what the worker's agent reads and reports through.
type workerClient interface {
	agent.Source
	StatusForNode(ctx context.Context, nodeID string) ([]store.Status, error)
}

// workerSource is the agent's Source for a worker. A server that has lost
// the node's row answers reports with ErrNotRegistered; the worker registers
// again and reports once more. A server that says the node was removed makes
// the source call onRemoved, which stops the worker.
type workerSource struct {
	client    workerClient
	register  func(context.Context) error
	onRemoved func(error)
	// lease holds off the lease route when the server lacks it, as 2.0
	// does. Nil holds nothing off.
	lease *nodeapi.MissingRoute
}

// check passes err through, and calls onRemoved when it says the node was
// removed.
func (w workerSource) check(err error) error {
	if errors.Is(err, nodeapi.ErrNodeRemoved) && w.onRemoved != nil {
		w.onRemoved(err)
	}
	return err
}

func (w workerSource) AssignmentsForNode(ctx context.Context, id string) ([]store.Assignment, error) {
	as, err := w.client.AssignmentsForNode(ctx, id)
	return as, w.check(err)
}

func (w workerSource) ReportStatus(ctx context.Context, id string, st []store.Status) error {
	err := w.client.ReportStatus(ctx, id, st)
	if !errors.Is(err, nodeapi.ErrNotRegistered) {
		return w.check(err)
	}
	if rerr := w.register(ctx); rerr != nil {
		return w.check(errors.Join(err, rerr))
	}
	return w.check(w.client.ReportStatus(ctx, id, st))
}

// RenewLease makes the worker's agent renew its lease on a timer, apart from
// its passes (agent.LeaseRenewer). A node the server does not know yet gets
// ErrNotRegistered, which the next report handles. A server without the
// route (2.0) is logged once and left alone until it changes: its leases
// come from the reports alone.
func (w workerSource) RenewLease(ctx context.Context, id string) error {
	lr, ok := w.client.(agent.LeaseRenewer)
	if !ok || w.lease.Skip() {
		return nil
	}
	err := w.lease.Check(lr.RenewLease(ctx, id))
	if errors.Is(err, nodeapi.ErrNoRoute) {
		return nil
	}
	return w.check(err)
}

func (w workerSource) StatusForNode(ctx context.Context, id string) ([]store.Status, error) {
	sts, err := w.client.StatusForNode(ctx, id)
	return sts, w.check(err)
}
