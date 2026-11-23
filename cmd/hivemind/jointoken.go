package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/server"
)

func joinTokenCmd() *cobra.Command {
	var ttl string
	var role string
	cmd := &cobra.Command{
		Use:   "join-token",
		Short: "Print a token that lets another machine join this cluster",
		Long: `Print a join token. Run "hivemind join <server>:<cluster-port> --token <token>"
on the other machine. The token is single use, and it expires after --ttl
(default 24h, at most 168h). Its text carries the cluster CA's fingerprint, so
the joining machine only trusts this cluster's CA.

--role server prints a server join token instead, for
"hivemind join <server> --role server --token <token> --advertise <address>"
on a machine that is to be another server of the cluster (docs/ha.md). It
expires after 1h by default, at most 24h: the joining server receives the
cluster CA's private key. A worker's join refuses it, and a server's join
refuses a worker's token, each saying which kind the token is and leaving
it unused. A cluster of two servers needs both for a write,
so the command warns when the token would make two; use three or five.

The token goes to standard output alone, so scripts can capture it. The
expiry, and a warning, go to standard error. With --json, all go to
standard output as JSON.`,
		Args: cobra.NoArgs,
	}
	cf := addClientFlags(cmd)
	cmd.Flags().StringVar(&ttl, "ttl", "", "how long the token lasts, such as 1h (default "+pki.DefaultTokenTTL.String()+
		", "+server.ServerTokenTTL.String()+" with --role server)")
	cmd.Flags().StringVar(&role, "role", "worker", "worker, or server for a token that joins another server of the cluster")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		c, err := cf.client()
		if err != nil {
			return err
		}
		var reply server.JoinTokenReply
		if role != "worker" && role != "server" {
			return fmt.Errorf("--role %q must be worker or server", role)
		}
		if err := c.call(cmd.Context(), "POST", "/v1/join-tokens", server.JoinTokenRequest{TTL: ttl, Server: role == "server"}, &reply); err != nil {
			return err
		}
		if cf.json {
			b, err := json.MarshalIndent(reply, "", "  ")
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(b))
			return nil
		}
		fmt.Fprintln(cmd.OutOrStdout(), reply.Token)
		fmt.Fprintf(cmd.ErrOrStderr(), "expires %s\n", reply.ExpiresAt.Local().Format(time.RFC3339))
		if reply.Warning != "" {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", reply.Warning)
		}
		return nil
	}
	return cmd
}
