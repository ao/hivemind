// Command hivemind runs and controls a hivemind cluster.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// Set at build time with -ldflags "-X main.version=... -X main.commit=...".
var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := newRoot(os.Stdin, os.Stdout, os.Stderr).ExecuteContext(ctx)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(exitCode(err))
	}
}

// exitError is an error that sets the status the process exits with.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

// exitCode is the status main exits with for err: an exitError's own, or 1.
func exitCode(err error) int {
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	return 1
}

// newRoot builds the command tree. Tests run it in-process with their own
// writers.
func newRoot(stdin io.Reader, stdout, stderr io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:           "hivemind",
		Short:         "Run containers on a small cluster of machines",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetIn(stdin)
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.AddCommand(
		versionCmd(),
		initCmd(),
		serveCmd(),
		joinCmd(),
		joinTokenCmd(),
		deployCmd(),
		composeCmd(),
		statusCmd(),
		logsCmd(),
		scaleCmd(),
		deleteCmd(),
		nodesCmd(),
		nodeCmd(),
		registryCmd(),
		backupCmd(),
		restoreCmd(),
		genDocsCmd(),
	)
	return root
}

// serverVersionTimeout bounds the version command's call to the server.
const serverVersionTimeout = 5 * time.Second

func versionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the version of the CLI and of the server",
		Long: `Print the version of this binary. When the CLI has a server and a token, from
its flags, the environment or the client config, it also asks the server for
its version, which every reply to an authenticated request carries. A server
older than 2.2 reports none. hivemind nodes shows the version of each node.`,
		Args: cobra.NoArgs,
	}
	cf := addConnFlags(cmd)
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "hivemind %s (%s)\n", version, commit)
		c, err := cf.client()
		if err != nil {
			if cf.server == "" && cf.token == "" {
				return nil // no server to ask
			}
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), serverVersionTimeout)
		defer cancel()
		v, err := c.serverVersion(ctx)
		if err != nil {
			return err
		}
		if v == "" {
			fmt.Fprintf(out, "server %s: older than 2.2, which reports no version\n", c.base)
			return nil
		}
		fmt.Fprintf(out, "server %s: hivemind %s\n", c.base, v)
		return nil
	}
	return cmd
}
