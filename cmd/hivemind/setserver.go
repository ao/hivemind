package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ao/hivemind/internal/config"
	"github.com/ao/hivemind/internal/nodeapi"
	"github.com/ao/hivemind/internal/pki"
)

// setServerTimeout bounds set-server's check of the new address.
const setServerTimeout = 15 * time.Second

// workerStateNext is the staging name of node.json while set-server
// replaces it.
const workerStateNext = "node.json.next"

func nodeSetServerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set-server <address>",
		Short: "Move this worker to its server's new address, without joining again",
		Long: `Run on a worker, while the worker is stopped, after its server was restored on
a machine with another address (docs/backup.md). <address> is the server's
new address, host or host:port, where the port is its cluster port (4484 by
default), as for hivemind join. For a server that advertises a DNS name,
update the name's record instead: the workers follow it by themselves.

set-server changes the server address in node.json, and nothing else, and
only after it has checked from this machine that <address> is the server of
this worker's own cluster:

  - the server presents a certificate signed by the cluster CA in this data
    directory, that names <address> and a node;
  - the server accepts this node's certificate, and answers a request of the
    node API for this node, as it does not for a removed node.

Another cluster's server, a server whose certificate does not name
<address>, an address where nothing answers, and anything between this
machine and the server that holds another CA are refused with what was
found, and node.json is left as it was. The node keeps its ID, key and
certificate, and its containers.

The worker reads node.json when it starts, so stop it first: set-server
refuses to run beside it. Its containers keep running meanwhile. Then start
it again, with hivemind serve --role worker or its service.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cmd.Flags(), config.Options{})
			if err != nil {
				return err
			}
			return runSetServer(cmd.Context(), cmd.OutOrStdout(), cfg.DataDir, args[0])
		},
	}
	cmd.Flags().String("data-dir", "", "the worker's data directory (env HIVEMIND_DATA_DIR; default /var/lib/hivemind for root, else ~/.local/state/hivemind)")
	return cmd
}

// runSetServer is node set-server: it checks the server at arg for the
// worker in dir, and then points node.json at it.
func runSetServer(ctx context.Context, out io.Writer, dir, arg string) error {
	host, port, err := splitServer(arg)
	if err != nil {
		return err
	}
	if port == defaultClusterPort+1 {
		return fmt.Errorf("%s is the default join port, but set-server takes the server's cluster port, %s, or no port",
			net.JoinHostPort(host, strconv.Itoa(port)), net.JoinHostPort(host, strconv.Itoa(defaultClusterPort)))
	}
	// Before the lock, which would create the directory: a directory that
	// holds no worker is refused as it is.
	if _, err := readWorkerState(dir); errors.Is(err, errNotJoined) {
		if _, serr := os.Stat(filepath.Join(dir, storeFileName)); serr == nil {
			return fmt.Errorf("%s holds a server, not a worker: set-server runs on each worker, and the server takes its address from serve --advertise", dir)
		}
		return fmt.Errorf("%w in %s: set-server moves a worker that has joined; join this machine with hivemind join <server> --token <token>", errNotJoined, dir)
	}
	release, err := lockDataDir(dir)
	if err != nil {
		return fmt.Errorf("%w. The worker reads node.json only when it starts: stop it (systemctl stop hivemind), run set-server, then start it again; its containers keep running", err)
	}
	defer release()
	w, err := loadWorkerState(dir)
	if err != nil {
		return err
	}
	server := net.JoinHostPort(host, strconv.Itoa(port))
	statePath := filepath.Join(dir, workerStateFile)
	if err := checkNewServer(ctx, w, host, server); err != nil {
		return fmt.Errorf("%w. %s is unchanged", err, statePath)
	}
	old := w.state.Server
	if server == old {
		fmt.Fprintf(out, "The server at %s is node %s's server, and %s already names it; nothing was changed.\n", server, w.state.NodeID, statePath)
		return nil
	}
	st := w.state
	st.Server = server
	if err := replaceWorkerState(dir, st); err != nil {
		return fmt.Errorf("writing %s: %w", statePath, err)
	}
	fmt.Fprintf(out, "The server at %s is node %s's server: it has a certificate from this cluster's CA for %s, and it accepts this node's certificate.\n",
		server, w.state.NodeID, host)
	fmt.Fprintf(out, "%s now names %s (it named %s). Start the worker again: hivemind serve --role worker, or systemctl start hivemind.\n",
		statePath, server, old)
	return nil
}

// checkNewServer checks, before set-server trusts it, that the server at
// server, dialled as host, is the server of w's cluster: its certificate
// chains to w's CA and names host, and it answers a node API request with
// w's certificate. The error says which part failed, and what that means.
func checkNewServer(ctx context.Context, w *joinedWorker, host, server string) error {
	tlsCfg, err := pki.ClientTLSConfigFrom(w.ca, w.holder, host)
	if err != nil {
		return err
	}
	c, err := nodeapi.NewClient("https://"+server, tlsCfg, nodeapi.WithVersion(version))
	if err != nil {
		return err
	}
	defer c.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(ctx, setServerTimeout)
	defer cancel()
	_, err = c.StatusForNode(ctx, w.state.NodeID)
	if err == nil || errors.Is(err, nodeapi.ErrNotRegistered) {
		// A reply from the node API: the handshake passed both ways. A
		// server restored from a backup older than the node's join has
		// no record of it, and the worker registers on its first call.
		return nil
	}
	return serverCheckError(err, w, host, server)
}

// serverCheckError explains why the server at server is not taken.
func serverCheckError(err error, w *joinedWorker, host, server string) error {
	var unknownCA x509.UnknownAuthorityError
	var badName x509.HostnameError
	var invalid x509.CertificateInvalidError
	var dnsErr *net.DNSError
	var opErr *net.OpError
	switch {
	case errors.As(err, &unknownCA):
		return otherCAError(err, w, host, server)
	case errors.As(err, &badName) && (badName.Certificate == nil || !signedBy(badName.Certificate, w.ca)):
		// The name is checked before the chain, so a certificate from
		// another CA for another name fails on the name first.
		return otherCAError(err, w, host, server)
	case errors.As(err, &badName):
		names := "no address"
		if badName.Certificate != nil {
			names = certNames(badName.Certificate)
		}
		return fmt.Errorf("the server at %s has a certificate from this cluster's CA, but it names %s, not %s: "+
			"give set-server the address the server advertises, or start the server with --advertise %s", server, names, host, host)
	case errors.As(err, &invalid):
		return fmt.Errorf("the server at %s presents a certificate that this worker cannot accept: %w", server, err)
	case remoteAlert(err) != "":
		return fmt.Errorf("the server at %s refused this node's certificate in the TLS handshake (%s): "+
			"it does not take certificates from this worker's cluster CA, so it is not this cluster's server", server, remoteAlert(err))
	case errors.Is(err, nodeapi.ErrNodeRemoved):
		return fmt.Errorf("the server at %s is this cluster's server, and it says node %s was removed from the cluster, so it refuses this node for good; "+
			"to bring this machine back, join it again with an empty --data-dir and a new join token", server, w.state.NodeID)
	case errors.Is(err, nodeapi.ErrNoRoute):
		return fmt.Errorf("%s answers with this cluster's certificate, but has no node API there: it is the join port, which is the cluster port plus one; "+
			"give the cluster port (%d by default)", server, defaultClusterPort)
	case errors.As(err, &dnsErr):
		return fmt.Errorf("cannot look up %s: %w", host, dnsErr)
	case errors.As(err, &opErr) && opErr.Op == "dial":
		return fmt.Errorf("cannot reach a server at %s: %w", server, opErr.Err)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("the server at %s did not answer within %s", server, setServerTimeout)
	}
	var unreachable *nodeapi.UnreachableError
	if errors.As(err, &unreachable) {
		return fmt.Errorf("the server at %s did not answer as a hivemind server: %w", server, unreachable.Err)
	}
	return fmt.Errorf("the server at %s: %w", server, err)
}

// otherCAError is the refusal of a server whose certificate is not from w's
// cluster CA. Every cluster CA has the same name, so the message gives the
// fingerprints, which tell them apart; a join token carries its cluster's in
// hex.
func otherCAError(err error, w *joinedWorker, host, server string) error {
	theirs := "a CA it did not send"
	var cve *tls.CertificateVerificationError
	if errors.As(err, &cve) && len(cve.UnverifiedCertificates) > 1 {
		for _, c := range cve.UnverifiedCertificates[1:] {
			if c.IsCA {
				theirs = fmt.Sprintf("the CA with fingerprint %x", pki.Fingerprint(c))
				break
			}
		}
	}
	return fmt.Errorf("the server at %s presents a certificate from %s, not from this worker's cluster CA, fingerprint %x: "+
		"it is another cluster's server, or something between this machine and %s holds another CA",
		server, theirs, w.ca.Fingerprint(), host)
}

// signedBy reports whether leaf chains to ca for server authentication,
// whatever names it carries.
func signedBy(leaf *x509.Certificate, ca *pki.CA) bool {
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	_, err := leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	return err == nil
}

// remoteAlert is the TLS alert the server sent, such as "remote error: tls:
// bad certificate", or "". crypto/tls reports one as a *net.OpError whose Op
// is "remote error"; the text is matched too, for a transport that flattens
// the error.
func remoteAlert(err error) string {
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "remote error" {
		return opErr.Error()
	}
	const mark = "remote error: tls: "
	if i := strings.Index(err.Error(), mark); i >= 0 {
		return err.Error()[i:]
	}
	return ""
}

// certNames lists the DNS names and IP addresses a certificate names.
func certNames(c *x509.Certificate) string {
	var names []string
	names = append(names, c.DNSNames...)
	for _, ip := range c.IPAddresses {
		names = append(names, ip.String())
	}
	if len(names) == 0 {
		return "no address"
	}
	return strings.Join(names, ", ")
}

// replaceWorkerState writes st as node.json in dir. It is written in full and
// synced under a staging name, then renamed over node.json, so node.json is
// never seen half written: after a crash it is the old one or the new one.
func replaceWorkerState(dir string, st workerState) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	next := filepath.Join(dir, workerStateNext)
	// A staging file is left only by a set-server that stopped before its
	// rename; node.json is still the old one then.
	if err := os.Remove(next); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := writeSynced(next, append(b, '\n')); err != nil {
		return err
	}
	if err := os.Rename(next, filepath.Join(dir, workerStateFile)); err != nil {
		_ = os.Remove(next)
		return err
	}
	return syncDir(dir)
}
