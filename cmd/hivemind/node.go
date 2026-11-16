package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/ao/hivemind/internal/agent"
	"github.com/ao/hivemind/internal/auth"
	"github.com/ao/hivemind/internal/backup"
	"github.com/ao/hivemind/internal/cluster"
	"github.com/ao/hivemind/internal/config"
	"github.com/ao/hivemind/internal/logging"
	"github.com/ao/hivemind/internal/logrelay"
	"github.com/ao/hivemind/internal/metrics"
	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/router"
	"github.com/ao/hivemind/internal/runtime"
	"github.com/ao/hivemind/internal/runtime/docker"
	"github.com/ao/hivemind/internal/server"
	"github.com/ao/hivemind/internal/store"
	"github.com/ao/hivemind/internal/tlsstore"
	"github.com/ao/hivemind/internal/upgrade"
)

// storeFileName is the SQLite file inside the data directory.
const storeFileName = "hivemind.db"

// shutdownTimeout bounds how long serve waits for requests in flight.
const shutdownTimeout = 10 * time.Second

// sweepInterval is how often the server checks leases, places pending
// replicas, advances rollouts and drains, and prunes expired join tokens.
const sweepInterval = 5 * time.Second

// sweep runs the server's periodic work until ctx ends. A node whose lease
// has lapsed is released: its replicas are placed on other nodes, and volume
// replicas wait for it. Each node is logged once when it is lost. Leases count
// from started at the earliest (see store.ReconcileNodes), so a server that
// was down gives every node a lease timeout to report. Drains count the start
// of their moves the same way (see store.AdvanceDrains).
func sweep(ctx context.Context, st *store.Store, log *slog.Logger, every time.Duration, started time.Time) {
	t := time.NewTicker(every)
	defer t.Stop()
	lost := map[string]bool{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		nodes, err := st.ReconcileNodes(ctx, started)
		if err != nil {
			if ctx.Err() == nil {
				log.Warn("node sweep", "err", err)
			}
		}
		now := make(map[string]bool, len(nodes))
		for _, id := range nodes {
			now[id] = true
			if !lost[id] {
				log.Warn("node lost: its replicas are placed again, and volume replicas wait for it", "node", id)
			}
		}
		lost = now
		if n, err := st.PruneJoinTokens(ctx, time.Now()); err != nil {
			if ctx.Err() == nil {
				log.Warn("pruning join tokens", "err", err)
			}
		} else if n > 0 {
			log.Info("pruned expired join tokens", "count", n)
		}
		if err := st.AdvanceRollouts(ctx); err != nil && ctx.Err() == nil {
			log.Warn("advance rollouts", "err", err)
		}
		advanceDrains(ctx, st, log, started)
	}
}

// advanceDrains takes a step of every drain, and logs each drain that
// finished.
func advanceDrains(ctx context.Context, st *store.Store, log *slog.Logger, started time.Time) {
	done, err := st.AdvanceDrains(ctx, started)
	if err != nil {
		if ctx.Err() == nil {
			log.Warn("advance drains", "err", err)
		}
		return
	}
	for _, d := range done {
		if d.LostAt.IsZero() {
			log.Info("node drained", "node", d.NodeID, "moved", d.Moved, "stopped", d.Stopped, "left", d.Remaining)
		} else {
			log.Warn("node drained; it was lost during the drain, and its replicas without a volume were placed again",
				"node", d.NodeID, "moved", d.Moved, "stopped", d.Stopped, "lost", d.Lost, "left", d.Remaining,
				"lost_at", d.LostAt.UTC())
		}
	}
}

func initCmd() *cobra.Command {
	var clientConfig string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create a cluster on this machine",
		Long: `Create a cluster on this machine. init creates the data directory (mode 0700),
generates the admin token, writes it to <data-dir>/admin.token (mode 0600) and
stores only its SHA-256 digest in the cluster database. The token is printed
once. init also writes a client config that points the CLI at this server and
at the token file. With --api-tls-cert, the client config also names that
certificate as ca-cert, so the CLI on this machine trusts it. init refuses to
run if a cluster already exists.

init takes serve's flags, and writes the ones it is given to
<data-dir>/hivemind.yaml, with mode 0600, so that a plain hivemind serve uses
them; certificate paths are written as absolute paths. A config file that
exists, such as one given with --config, is not changed: init refuses serve
flags then, before it creates anything.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInit(cmd, cmd.Flags(), clientConfig)
		},
	}
	config.Bind(cmd.Flags(), config.Options{})
	cmd.Flags().StringVar(&clientConfig, "client-config", "", "client config file to write (default "+defaultClientConfigHint+"; env HIVEMIND_CLIENT_CONFIG)")
	return cmd
}

func runInit(cmd *cobra.Command, flags *pflag.FlagSet, clientConfigFlag string) error {
	ctx := cmd.Context()
	cfg, err := config.Load(flags, config.Options{})
	if err != nil {
		return err
	}
	dir := cfg.DataDir
	// Everything that makes init refuse is checked before it creates
	// anything, the store included.
	if err := workerHereError(dir, "init would make a second cluster"); err != nil {
		return err
	}
	if err := upgrade.CheckDataDir(dir); err != nil {
		return err
	}
	tokenPath := filepath.Join(dir, auth.TokenFileName)
	if _, err := os.Lstat(tokenPath); err == nil {
		return fmt.Errorf("a cluster already exists in %s (%s is present); refusing to overwrite it", dir, tokenPath)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, name := range []string{pki.CAKeyFile, pki.CACertFile} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
			return fmt.Errorf("a cluster CA already exists in %s (%s is present); refusing to overwrite it", dir, name)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	// The serve flags init was given go into the config file, so that a
	// plain serve, as the quick start and the systemd unit run it, uses
	// them. A file that exists, as one given with --config must, is the
	// user's, and is not changed.
	explicitConfig := flags.Changed("config")
	_, err = os.Lstat(cfg.ConfigFile)
	configExists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	serveSettings, err := config.GivenFlags(flags)
	if err != nil {
		return err
	}
	if len(serveSettings) > 0 && configExists {
		return fmt.Errorf("%s exists, and init writes the serve settings it is given (%s) to it: put them in that file and run init without them, or remove the file",
			cfg.ConfigFile, settingFlags(serveSettings))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("data directory %s: %w", dir, err)
	}

	st, err := store.Open(ctx, filepath.Join(dir, storeFileName))
	if err != nil {
		return err
	}
	defer st.Close()
	if _, err := st.GetCluster(ctx); err == nil {
		return fmt.Errorf("a cluster already exists in %s (the database has a cluster record); refusing to overwrite it", dir)
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	token, err := auth.NewToken()
	if err != nil {
		return err
	}
	nodeID, err := newNodeID()
	if err != nil {
		return err
	}
	// The cluster CA signs node certificates. Its key is mode 0600 and never
	// leaves this data directory.
	if _, err := pki.CreateCA(dir); err != nil {
		if errors.Is(err, pki.ErrCAExists) {
			return fmt.Errorf("a cluster CA already exists in %s; refusing to overwrite it", dir)
		}
		return err
	}
	if err := auth.WriteAdminToken(dir, token); err != nil {
		removeCA(dir)
		return err
	}
	if err := st.InitCluster(ctx, store.Cluster{AdminTokenHash: auth.HashToken(token), NodeID: nodeID}); err != nil {
		_ = os.Remove(tokenPath)
		removeCA(dir)
		if errors.Is(err, store.ErrClusterExists) {
			return fmt.Errorf("a cluster already exists in %s; refusing to overwrite it", dir)
		}
		return err
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Created a cluster in %s (node %s).\n\n", dir, nodeID)
	fmt.Fprintf(out, "Admin token, shown only this once:\n\n    %s\n\n", token)
	fmt.Fprintf(out, "It is also saved in %s (mode 0600). The server keeps only its SHA-256 digest.\n", tokenPath)
	if len(serveSettings) > 0 {
		if err := config.WriteFile(cfg.ConfigFile, "Written by hivemind init: the serve settings it was given. serve reads this file.", serveSettings); err != nil {
			return fmt.Errorf("the cluster is created, but writing %s failed: %w; give serve the same flags", cfg.ConfigFile, err)
		}
		fmt.Fprintf(out, "The serve settings init was given (%s) are in %s, which serve reads.\n", settingFlags(serveSettings), cfg.ConfigFile)
	}

	ccPath, err := clientConfigPath(clientConfigFlag)
	if err != nil {
		fmt.Fprintf(out, "No client config written: %v\n", err)
	} else {
		cc := clientConfig{Server: serverURL(cfg), TokenFile: tokenPath}
		if cfg.APITLSCert != "" {
			// The CLI trusts the API's own certificate file. A certificate
			// in the trust pool is accepted as a root, so this works for a
			// self-signed certificate and for one from a private CA.
			if cc.CACert, err = filepath.Abs(cfg.APITLSCert); err != nil {
				return err
			}
			if note := certNameNote(cc.CACert, cc.Server); note != "" {
				fmt.Fprintln(out, note)
			}
		}
		switch err := writeClientConfig(ccPath, cc); {
		case err == nil:
			fmt.Fprintf(out, "The CLI on this machine reads the server address and token file from %s.\n", ccPath)
		case errors.Is(err, fs.ErrExist):
			fmt.Fprintf(out, "%s already exists and was left as it is; point it at %s and %s.\n", ccPath, cc.Server, tokenPath)
		default:
			return err
		}
	}
	serveLine := "hivemind serve"
	switch {
	case explicitConfig:
		serveLine += " --config " + cfg.ConfigFile
	case flags.Changed("data-dir"):
		serveLine += " --data-dir " + dir
	}
	fmt.Fprintf(out, "\nStart the node with `%s`. From another shell or machine, use\n\n", serveLine)
	fmt.Fprintf(out, "    export HIVEMIND_SERVER=%s\n    export HIVEMIND_TOKEN=<the token above>\n", serverURL(cfg))
	if cfg.APITLSCert != "" {
		fmt.Fprintf(out, "    export %s=<a copy of %s, or the CA that signed it>\n", envCACert, cfg.APITLSCert)
	}
	return nil
}

// settingFlags lists settings as the flags they came from, such as
// "--api-addr, --advertise".
func settingFlags(ss []config.Setting) string {
	names := make([]string, 0, len(ss))
	for _, s := range ss {
		names = append(names, "--"+s.Key)
	}
	return strings.Join(names, ", ")
}

// removeCA deletes the CA files init created, after a failed init, so the
// data directory holds no key without its cluster.
func removeCA(dir string) {
	_ = os.Remove(filepath.Join(dir, pki.CAKeyFile))
	_ = os.Remove(filepath.Join(dir, pki.CACertFile))
}

// loadClusterCA reads the cluster CA from dir. It returns nil, and no error,
// when the directory has no CA.
func loadClusterCA(dir string) (*pki.CA, error) {
	ca, err := pki.LoadCA(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cluster CA in %s: %w", dir, err)
	}
	return ca, nil
}

// joinIssuer returns the function that issues join tokens for ca.
func joinIssuer(ca *pki.CA, st *store.Store) func(time.Duration) (string, time.Time, error) {
	tokens := pki.NewTokens(st.JoinTokens())
	return func(ttl time.Duration) (string, time.Time, error) {
		return tokens.Issue(ca.Fingerprint(), ttl)
	}
}

func newNodeID() (string, error) { return cluster.NewNodeID() }

// serverURL is the URL a client on this machine uses for the API address.
// A wildcard bind is reached on loopback.
func serverURL(cfg *config.Config) string {
	scheme := "http"
	if cfg.APITLSCert != "" {
		scheme = "https"
	}
	host, port, err := net.SplitHostPort(cfg.APIAddr)
	if err != nil {
		return scheme + "://" + cfg.APIAddr
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return scheme + "://" + net.JoinHostPort(host, port)
}

func serveCmd() *cobra.Command {
	var role string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the server and the agent on this node",
		Long: `Run the API server and the agent on this node. The API listens on --api-addr,
127.0.0.1:4483 by default. A non-loopback address needs --api-tls-cert and
--api-tls-key. SIGINT or SIGTERM stops the server; containers keep running.

--advertise <addr> also opens the cluster listeners, on that address only: the
node listener on --cluster-port (4484) and the join listener on the port above
it. Other machines join with hivemind join.

--role worker runs only the agent, for a machine that joined with hivemind
join. It has no API listener. --role server is the default.

--route-addr <ip> runs the HTTPS router on this node, on that address:
--route-http-port (80) for ACME HTTP-01 challenges and redirects, and
--route-https-port (443) for the apps' routes. It may be 0.0.0.0, and it is
the only listener that may: it serves the apps' public traffic, without
authentication. It needs --acme-email; --acme-ca chooses the CA (Let's
Encrypt by default). Without --route-addr there is no router. See
docs/routing.md.

GET /metrics on the API serves Prometheus metrics and needs the admin token.
--metrics-addr also serves it, and nothing else, on a separate loopback
address; that listener needs the token too. Logs go to stderr at
--log-level, as text or with --log-format json. Tokens, cookies, CSRF tokens,
private keys and env values are not logged.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(cmd.Flags(), config.Options{})
			if err != nil {
				return err
			}
			log, err := newLogger(cfg, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			slog.SetDefault(log)
			switch role {
			case "server":
			case "worker":
				if cfg.Advertise != "" {
					return errors.New("--advertise does not apply to a worker, which runs no listener")
				}
				return runWorkerServe(cmd.Context(), cfg, log)
			default:
				return fmt.Errorf("--role %q must be server or worker", role)
			}
			rt, err := docker.New(cfg.DockerHost)
			if err != nil {
				return err
			}
			return runServe(cmd.Context(), cfg, log, rt)
		},
	}
	config.Bind(cmd.Flags(), config.Options{})
	cmd.Flags().StringVar(&role, "role", "server", "server (API, scheduler and agent) or worker (agent only, after hivemind join)")
	return cmd
}

// dockerReachError is the error for a Docker daemon that serve or a worker
// cannot reach at host. A socket this user may not open says what to do:
// join the docker group, or run as root.
func dockerReachError(host string, err error) error {
	if errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("cannot reach Docker at %s: %w; this user may not open the socket: add it to the docker group "+
			"(sudo usermod -aG docker $USER, then log in again), or run hivemind as root", host, err)
	}
	return fmt.Errorf("cannot reach Docker at %s: %w", host, err)
}

// newLogger is the server's logger: the level and format from the config,
// with secrets redacted.
func newLogger(cfg *config.Config, w io.Writer) (*slog.Logger, error) {
	return logging.New(w, cfg.LogLevel, cfg.LogFormat)
}

// listenMetrics opens the --metrics-addr listener. config.Validate already
// requires a loopback host; this checks the address the socket was actually
// bound to as well, because a host name such as localhost is resolved here.
func listenMetrics(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on --metrics-addr %s: %w", addr, err)
	}
	tcp, ok := ln.Addr().(*net.TCPAddr)
	if !ok || !tcp.IP.IsLoopback() {
		ln.Close()
		return nil, fmt.Errorf("--metrics-addr %s bound %s, which is not a loopback address", addr, ln.Addr())
	}
	return ln, nil
}

func runServe(ctx context.Context, cfg *config.Config, log *slog.Logger, rt runtime.Runtime) error {
	// The token prints as [redacted]: config.Secret masks itself, and the
	// redacting handler masks it again.
	log.Debug("resolved config", "config", cfg)
	if err := upgrade.CheckDataDir(cfg.DataDir); err != nil {
		return err
	}
	release, err := lockDataDir(cfg.DataDir)
	if err != nil {
		return err
	}
	defer release()
	if w, err := readWorkerState(cfg.DataDir); err == nil {
		return fmt.Errorf("%s holds worker %s of the cluster at %s: run hivemind serve --role worker", cfg.DataDir, w.NodeID, w.Server)
	}
	st, err := store.Open(ctx, filepath.Join(cfg.DataDir, storeFileName), store.WithMinReady(cfg.MinReady),
		store.WithLogger(log.With("component", "store")))
	if err != nil {
		return err
	}
	defer st.Close()
	cl, err := st.GetCluster(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no cluster in %s: run `hivemind init` first", cfg.DataDir)
	}
	if err != nil {
		return err
	}

	var tlsCfg *tls.Config
	if cfg.APITLSCert != "" {
		cert, err := tls.LoadX509KeyPair(cfg.APITLSCert, cfg.APITLSKey)
		if err != nil {
			return fmt.Errorf("API TLS certificate: %w", err)
		}
		tlsCfg = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
	}

	info, err := rt.Info(ctx)
	if err != nil {
		return dockerReachError(cfg.DockerHost, err)
	}
	// The node's private address, where its routed replicas publish their
	// ports: the cluster listener's address, or loopback without one.
	nodeAddr, err := serverNodeAddress(ctx, cfg.Advertise)
	if err != nil {
		return err
	}
	hostname, _ := os.Hostname()
	if err := st.UpsertNode(ctx, store.Node{ID: cl.NodeID, Name: hostname, CPU: info.NCPU, Memory: info.MemTotal, Address: nodeAddr,
		Version: nodeapi.CleanVersion(version)}); err != nil {
		return err
	}

	sessions, err := auth.NewSessions(12 * time.Hour)
	if err != nil {
		return err
	}
	authn, err := auth.NewAuthenticator(auth.TokenHash(cl.AdminTokenHash), sessions)
	if err != nil {
		return err
	}
	m := server.NewMetrics()
	// After each agent pass, which has just reported status, the rollout and
	// drain controllers move the next replica or check a deadline. The agent
	// passes on every interval, so deadlines are checked even when nothing
	// reports.
	agentLog := log.With("component", "agent")
	started := time.Now()
	onPass := func(d time.Duration, err error) {
		m.ObserveReconcile(d, err)
		if aerr := st.AdvanceRollouts(context.Background()); aerr != nil {
			agentLog.Warn("advance rollouts", "err", aerr)
		}
		advanceDrains(context.Background(), st, agentLog, started)
	}
	ag, err := agent.New(agent.Config{NodeID: cl.NodeID, Runtime: rt, Source: st,
		Interval: cfg.ReconcileInterval, Logger: agentLog, OnPass: onPass, RouteBind: nodeAddr})
	if err != nil {
		return err
	}
	ca, err := loadClusterCA(cfg.DataDir)
	if err != nil {
		return err
	}
	if cfg.Advertise != "" && ca == nil {
		return fmt.Errorf("--advertise is set, but %s has no cluster CA: run hivemind init first", cfg.DataDir)
	}
	var issue func(time.Duration) (string, time.Time, error)
	if ca != nil {
		issue = joinIssuer(ca, st)
	}
	writeBackup := func(ctx context.Context, w io.Writer) error {
		_, err := backup.Write(ctx, w, backup.Options{DataDir: cfg.DataDir, Snapshot: st.SnapshotTo, HivemindVersion: version})
		return err
	}
	// Workers exist only with the cluster listener, and their logs come
	// through it.
	var logs *logrelay.Relay
	if cfg.Advertise != "" {
		logs = logrelay.New(logrelay.Options{})
	}
	// The router opens only with --route-addr. It is the one listener that
	// may bind every address: it serves the apps' public traffic.
	var rtr *router.Router
	var routerMetrics func(*metrics.Writer)
	if cfg.RouteAddr != "" {
		if rtr, err = newRouter(cfg, log, storeRoutes(st), tlsstore.StoreBackend{St: st, NodeID: cl.NodeID}); err != nil {
			return err
		}
		routerMetrics = rtr.WriteMetrics
	}
	closeRouter := func() {
		if rtr != nil {
			_ = rtr.Shutdown()
		}
	}
	api, err := server.New(server.Config{Store: st, Runtime: rt, NodeID: cl.NodeID, Auth: authn,
		Trigger: ag.Trigger, Logger: log.With("component", "api"), Metrics: m, IssueJoinToken: issue,
		Backup: writeBackup, Logs: logs, RouterMetrics: routerMetrics, Started: started, Version: version})
	if err != nil {
		closeRouter()
		return err
	}

	ln, err := net.Listen("tcp", cfg.APIAddr)
	if err != nil {
		closeRouter()
		return fmt.Errorf("listen on %s: %w", cfg.APIAddr, err)
	}
	hs := server.NewHTTPServer(api.Handler(), log)
	hs.TLSConfig = tlsCfg

	// The metrics listener is plain HTTP on loopback and needs the admin
	// token, like /metrics on the API.
	var mln net.Listener
	var mhs *http.Server
	if cfg.MetricsAddr != "" {
		if mln, err = listenMetrics(cfg.MetricsAddr); err != nil {
			ln.Close()
			closeRouter()
			return err
		}
		mhs = server.NewHTTPServer(api.MetricsHandler(), log)
	}

	// The cluster listeners open only with --advertise, on that address only.
	var nodes *cluster.Cluster
	if cfg.Advertise != "" {
		lifetime, err := testCertLifetime()
		if err != nil {
			ln.Close()
			if mln != nil {
				mln.Close()
			}
			closeRouter()
			return err
		}
		if lifetime != 0 {
			log.Warn("test build: node certificates are issued with a short lifetime", "lifetime", lifetime)
		}
		c, err := cluster.Listen(cluster.Config{Store: st, CA: ca, Tokens: pki.NewTokens(st.JoinTokens()),
			Advertise: cfg.Advertise, Port: cfg.ClusterPort, LocalNodeID: cl.NodeID,
			Logger: log.With("component", "cluster"), NodeCertLifetime: lifetime, Logs: logs, Version: version})
		if err == nil {
			if err = checkBoundAddress(c.NodeAddr(), nodeAddr); err != nil {
				_ = c.Shutdown()
			}
		}
		if err != nil {
			ln.Close()
			if mln != nil {
				mln.Close()
			}
			closeRouter()
			return err
		}
		nodes = c
	}

	agentCtx, stopAgent := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = ag.Run(agentCtx)
	}()
	go func() {
		defer wg.Done()
		sweep(agentCtx, st, log.With("component", "sweep"), sweepInterval, started)
	}()

	serveErr := make(chan error, 4)
	if rtr != nil {
		go func() {
			if err := rtr.Serve(); err != nil {
				serveErr <- fmt.Errorf("router: %w", err)
			}
		}()
	}
	if nodes != nil {
		log.Info("cluster listening", "node_addr", nodes.NodeAddr(), "join_addr", nodes.JoinAddr())
		go func() {
			if err := nodes.Serve(); err != nil {
				serveErr <- fmt.Errorf("cluster listener: %w", err)
			}
		}()
	}
	go func() {
		var err error
		if tlsCfg != nil {
			err = hs.ServeTLS(ln, "", "")
		} else {
			err = hs.Serve(ln)
		}
		serveErr <- fmt.Errorf("API server: %w", err)
	}()
	if mhs != nil {
		go func() { serveErr <- fmt.Errorf("metrics server: %w", mhs.Serve(mln)) }()
		log.Info("serving metrics", "addr", mln.Addr().String())
	}
	log.Info("hivemind serving", "addr", ln.Addr().String(), "tls", tlsCfg != nil, "node", cl.NodeID,
		"docker", info.ServerVersion, "version", version)

	var runErr error
	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case runErr = <-serveErr:
	}
	api.CloseStreams()
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := hs.Shutdown(sctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Warn("API shutdown", "err", err)
	}
	if rtr != nil {
		if err := rtr.Shutdown(); err != nil {
			log.Warn("router shutdown", "err", err)
		}
	}
	if nodes != nil {
		if err := nodes.Shutdown(); err != nil {
			log.Warn("cluster shutdown", "err", err)
		}
	}
	if mhs != nil {
		if err := mhs.Shutdown(sctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Warn("metrics shutdown", "err", err)
		}
	}
	stopAgent()
	wg.Wait()
	log.Info("stopped")
	return runErr
}
