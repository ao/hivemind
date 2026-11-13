package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
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
	"github.com/ao/hivemind/internal/config"
	"github.com/ao/hivemind/internal/runtime/docker"
	"github.com/ao/hivemind/internal/server"
	"github.com/ao/hivemind/internal/store"
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
	if err := auth.WriteAdminToken(dir, token); err != nil {
		return err
	}
	if err := st.InitCluster(ctx, store.Cluster{AdminTokenHash: auth.HashToken(token), NodeID: nodeID}); err != nil {
		_ = os.Remove(tokenPath)
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

func newNodeID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "node-" + hex.EncodeToString(b[:]), nil
}

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
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the server and the agent on this node",
		Long: `Run the API server and the agent on this node. The API listens on --api-addr,
127.0.0.1:4483 by default. A non-loopback address needs --api-tls-cert and
--api-tls-key. SIGINT or SIGTERM stops the server; containers keep running.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(cmd.Flags(), config.Options{})
			if err != nil {
				return err
			}
			return runServe(cmd.Context(), cfg, newLogger(cfg, cmd.ErrOrStderr()))
		},
	}
	config.Bind(cmd.Flags(), config.Options{})
	return cmd
}

func newLogger(cfg *config.Config, w io.Writer) *slog.Logger {
	var level slog.Level
	_ = level.UnmarshalText([]byte(cfg.LogLevel))
	opts := &slog.HandlerOptions{Level: level}
	if cfg.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

func runServe(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
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

	rt, err := docker.New(cfg.DockerHost)
	if err != nil {
		return err
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
	ag, err := agent.New(agent.Config{NodeID: cl.NodeID, Runtime: rt, Source: st,
		Interval: cfg.ReconcileInterval, Logger: log.With("component", "agent")})
	if err != nil {
		return err
	}
	api, err := server.New(server.Config{Store: st, Runtime: rt, NodeID: cl.NodeID, Auth: authn,
		Trigger: ag.Trigger, Logger: log.With("component", "api")})
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", cfg.APIAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.APIAddr, err)
	}
	hs := server.NewHTTPServer(api.Handler(), log)
	hs.TLSConfig = tlsCfg

	agentCtx, stopAgent := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = ag.Run(agentCtx)
	}()

	serveErr := make(chan error, 1)
	go func() {
		if tlsCfg != nil {
			serveErr <- hs.ServeTLS(ln, "", "")
		} else {
			serveErr <- hs.Serve(ln)
		}
	}()
	log.Info("hivemind serving", "addr", ln.Addr().String(), "tls", tlsCfg != nil, "node", cl.NodeID,
		"docker", info.ServerVersion, "version", version)

	var runErr error
	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-serveErr:
		runErr = fmt.Errorf("API server: %w", err)
	}
	api.CloseStreams()
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := hs.Shutdown(sctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Warn("API shutdown", "err", err)
	}
	stopAgent()
	wg.Wait()
	log.Info("stopped")
	return runErr
}

func joinCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "join",
		Short:              "Join another node's cluster (not implemented in this build)",
		DisableFlagParsing: true,
		RunE: func(*cobra.Command, []string) error {
			return errors.New("join is not implemented in this build: it runs single-node clusters only")
		},
	}
}
