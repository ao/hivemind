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
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/ao/hivemind/internal/agent"
	"github.com/ao/hivemind/internal/auth"
	"github.com/ao/hivemind/internal/cluster"
	"github.com/ao/hivemind/internal/config"
	"github.com/ao/hivemind/internal/logging"
	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/runtime"
	"github.com/ao/hivemind/internal/runtime/docker"
	"github.com/ao/hivemind/internal/server"
	"github.com/ao/hivemind/internal/store"
	"github.com/ao/hivemind/internal/upgrade"
)

// storeFileName is the SQLite file inside the data directory.
const storeFileName = "hivemind.db"

// shutdownTimeout bounds how long serve waits for requests in flight.
const shutdownTimeout = 10 * time.Second

func initCmd() *cobra.Command {
	var clientConfig string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create a cluster on this machine",
		Long: `Create a cluster on this machine. init creates the data directory (mode 0700),
generates the admin token, writes it to <data-dir>/admin.token (mode 0600) and
stores only its SHA-256 digest in the cluster database. The token is printed
once. init also writes a client config that points the CLI at this server and
at the token file. It refuses to run if a cluster already exists.`,
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
	if err := upgrade.CheckDataDir(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("data directory %s: %w", dir, err)
	}
	tokenPath := filepath.Join(dir, auth.TokenFileName)
	if _, err := os.Lstat(tokenPath); err == nil {
		return fmt.Errorf("a cluster already exists in %s (%s is present); refusing to overwrite it", dir, tokenPath)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
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

	ccPath, err := clientConfigPath(clientConfigFlag)
	if err != nil {
		fmt.Fprintf(out, "No client config written: %v\n", err)
	} else {
		cc := clientConfig{Server: serverURL(cfg), TokenFile: tokenPath}
		switch err := writeClientConfig(ccPath, cc); {
		case err == nil:
			fmt.Fprintf(out, "The CLI on this machine reads the server address and token file from %s.\n", ccPath)
		case errors.Is(err, fs.ErrExist):
			fmt.Fprintf(out, "%s already exists and was left as it is; point it at %s and %s.\n", ccPath, cc.Server, tokenPath)
		default:
			return err
		}
	}
	fmt.Fprintf(out, "\nStart the node with `hivemind serve`. From another shell or machine, use\n\n")
	fmt.Fprintf(out, "    export HIVEMIND_SERVER=%s\n    export HIVEMIND_TOKEN=<the token above>\n", serverURL(cfg))
	return nil
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
	st, err := store.Open(ctx, filepath.Join(cfg.DataDir, storeFileName))
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
		return fmt.Errorf("cannot reach Docker at %s: %w", cfg.DockerHost, err)
	}
	hostname, _ := os.Hostname()
	if err := st.UpsertNode(ctx, store.Node{ID: cl.NodeID, Name: hostname, CPU: info.NCPU, Memory: info.MemTotal}); err != nil {
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
	// After each agent pass, which has just reported status, the rollout
	// controller moves the next replica or checks a deadline. The agent passes
	// on every interval, so deadlines are checked even when nothing reports.
	agentLog := log.With("component", "agent")
	onPass := func(d time.Duration, err error) {
		m.ObserveReconcile(d, err)
		if aerr := st.AdvanceRollouts(context.Background()); aerr != nil {
			agentLog.Warn("advance rollouts", "err", aerr)
		}
	}
	ag, err := agent.New(agent.Config{NodeID: cl.NodeID, Runtime: rt, Source: st,
		Interval: cfg.ReconcileInterval, Logger: agentLog, OnPass: onPass})
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
	api, err := server.New(server.Config{Store: st, Runtime: rt, NodeID: cl.NodeID, Auth: authn,
		Trigger: ag.Trigger, Logger: log.With("component", "api"), Metrics: m, IssueJoinToken: issue})
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", cfg.APIAddr)
	if err != nil {
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
			return err
		}
		mhs = server.NewHTTPServer(api.MetricsHandler(), log)
	}

	// The cluster listeners open only with --advertise, on that address only.
	var nodes *cluster.Cluster
	if cfg.Advertise != "" {
		c, err := cluster.Listen(cluster.Config{Store: st, CA: ca, Tokens: pki.NewTokens(st.JoinTokens()),
			Advertise: cfg.Advertise, Port: cfg.ClusterPort, LocalNodeID: cl.NodeID,
			Logger: log.With("component", "cluster")})
		if err != nil {
			ln.Close()
			if mln != nil {
				mln.Close()
			}
			return err
		}
		nodes = c
	}

	agentCtx, stopAgent := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = ag.Run(agentCtx)
	}()

	serveErr := make(chan error, 3)
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
