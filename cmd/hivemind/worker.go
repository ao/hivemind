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
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
}

func joinCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "join <server>",
		Short: "Join a cluster as a worker, then run its agent",
		Long: `Join a cluster as a worker node and run its agent in the foreground.

  hivemind join 10.0.0.5 --token <token>

<server> is the server's address, host or host:port, where the port is its
cluster port (4484 by default). The worker generates a key and a certificate
request, presents the join token to the server's join port (the cluster port
plus one), and checks the reply against the CA fingerprint in the token. It
then stores its key (mode 0600), its certificate, the cluster CA and its node
ID in the data directory, and starts the agent.

The token is single use and expires; get one with hivemind join-token on the
server. --token is read from the flag only, not from HIVEMIND_TOKEN, which is
the admin token.

A joined machine keeps its state. After a restart, run
"hivemind serve --role worker" with the same data directory. A worker has no
API listener, so --advertise does not apply to it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cmd.Flags(), config.Options{})
			if err != nil {
				return err
			}
			if cfg.Advertise != "" {
				return errors.New("--advertise does not apply to join: a worker runs no listener")
			}
			token, _ := cmd.Flags().GetString("token")
			if token == "" {
				return errors.New("--token is required: run hivemind join-token on the server to get one")
			}
			host, port, err := splitServer(args[0])
			if err != nil {
				return err
			}
			log, err := newLogger(cfg, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			slog.SetDefault(log)
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
	cmd.Flags().Lookup("token").Usage = "join token from hivemind join-token on the server"
	cmd.Flags().Lookup("advertise").Hidden = true
	return cmd
}

// runWorkerServe is serve --role worker: it runs an already joined node.
func runWorkerServe(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	rt, err := docker.New(cfg.DockerHost)
	if err != nil {
		return err
	}
	return runWorker(ctx, cfg, log, rt)
}

// splitServer returns the host and cluster port of a server address. An
// address without a port uses defaultClusterPort.
func splitServer(arg string) (string, int, error) {
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
		return fmt.Errorf("%s already holds a joined node: run hivemind serve --role worker, or use another --data-dir", dir)
	} else if !errors.Is(err, fs.ErrNotExist) {
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

// loadWorkerState reads and checks a joined worker's files: the key must be
// private, the certificate must chain to the CA and name the node in
// node.json. A missing node.json is errNotJoined.
func loadWorkerState(dir string) (*joinedWorker, error) {
	raw, err := os.ReadFile(filepath.Join(dir, workerStateFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w in %s: run hivemind join <server> --token <token> first", errNotJoined, dir)
	}
	if err != nil {
		return nil, err
	}
	var st workerState
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&st); err != nil {
		return nil, fmt.Errorf("%s: %w", workerStateFile, err)
	}
	if err := pki.ValidateNodeID(st.NodeID); err != nil {
		return nil, err
	}
	host, _, err := net.SplitHostPort(st.Server)
	if err != nil {
		return nil, fmt.Errorf("%s: server %q: %w", workerStateFile, st.Server, err)
	}

	keyPath := filepath.Join(dir, workerKeyFile)
	info, err := os.Lstat(keyPath)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s must be a regular file readable by its owner only", keyPath)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil || kb.Type != "EC PRIVATE KEY" {
		return nil, fmt.Errorf("%s is not a PEM EC private key", keyPath)
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", keyPath, err)
	}

	caPEM, err := os.ReadFile(filepath.Join(dir, pki.CACertFile))
	if err != nil {
		return nil, err
	}
	cb, _ := pem.Decode(caPEM)
	if cb == nil || cb.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("%s is not a PEM certificate", pki.CACertFile)
	}
	caCert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, err
	}
	ca := &pki.CA{Cert: caCert, CertPEM: caPEM}

	certPEM, err := os.ReadFile(filepath.Join(dir, workerCertFile))
	if err != nil {
		return nil, err
	}
	nb, _ := pem.Decode(certPEM)
	if nb == nil || nb.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("%s is not a PEM certificate", workerCertFile)
	}
	leaf, err := x509.ParseCertificate(nb.Bytes)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return nil, fmt.Errorf("%s does not chain to the cluster CA: %w", workerCertFile, err)
	}
	if id, err := pki.NodeIDFromCert(leaf); err != nil || id != st.NodeID {
		return nil, fmt.Errorf("%s names node %q, but %s names %q", workerCertFile, id, workerStateFile, st.NodeID)
	}
	tlsCert, err := pki.NewTLSCertificate(nb.Bytes, key, ca)
	if err != nil {
		return nil, err
	}
	tlsCfg, err := pki.ClientTLSConfig(ca, tlsCert, host)
	if err != nil {
		return nil, err
	}
	return &joinedWorker{state: st, tlsCfg: tlsCfg}, nil
}

// runWorker runs the agent for the joined node in dir until ctx ends. It
// registers the node first, and again whenever the server says it has no
// record of the node.
func runWorker(ctx context.Context, cfg *config.Config, log *slog.Logger, rt runtime.Runtime) error {
	w, err := loadWorkerState(cfg.DataDir)
	if err != nil {
		return err
	}
	client, err := nodeapi.NewClient("https://"+w.state.Server, w.tlsCfg)
	if err != nil {
		return err
	}
	hostname, _ := os.Hostname()
	register := func(ctx context.Context) error {
		info, err := rt.Info(ctx)
		if err != nil {
			return fmt.Errorf("reading the Docker host: %w", err)
		}
		return client.Register(ctx, w.state.NodeID, nodeapi.Register{Name: hostname, CPU: info.NCPU, Memory: info.MemTotal})
	}
	for {
		err := register(ctx)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return nil
		}
		log.Warn("registering with the server; retrying", "server", w.state.Server, "err", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(registerRetry):
		}
	}
	log.Info("worker running", "node", w.state.NodeID, "server", w.state.Server)
	ag, err := agent.New(agent.Config{NodeID: w.state.NodeID, Runtime: rt,
		Source: workerSource{client: client, register: register}, Interval: cfg.ReconcileInterval,
		Logger: log.With("component", "agent")})
	if err != nil {
		return err
	}
	return ag.Run(ctx)
}

// workerClient is what the worker's agent reads and reports through.
type workerClient interface {
	agent.Source
	StatusForNode(ctx context.Context, nodeID string) ([]store.Status, error)
}

// workerSource is the agent's Source for a worker. A server that has lost
// the node's row answers reports with ErrNotRegistered; the worker registers
// again and reports once more.
type workerSource struct {
	client   workerClient
	register func(context.Context) error
}

func (w workerSource) AssignmentsForNode(ctx context.Context, id string) ([]store.Assignment, error) {
	return w.client.AssignmentsForNode(ctx, id)
}

func (w workerSource) ReportStatus(ctx context.Context, id string, st []store.Status) error {
	err := w.client.ReportStatus(ctx, id, st)
	if !errors.Is(err, nodeapi.ErrNotRegistered) {
		return err
	}
	if rerr := w.register(ctx); rerr != nil {
		return errors.Join(err, rerr)
	}
	return w.client.ReportStatus(ctx, id, st)
}

func (w workerSource) StatusForNode(ctx context.Context, id string) ([]store.Status, error) {
	return w.client.StatusForNode(ctx, id)
}
