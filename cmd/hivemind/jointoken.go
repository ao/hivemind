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
	cmd := &cobra.Command{
		Use:   "join-token",
		Short: "Print a token that lets another machine join this cluster",
		Long: `Print a join token. Run "hivemind join <server>:<cluster-port> --token <token>"
on the other machine. The token is single use, and it expires after --ttl
(default 24h, at most 168h). Its text carries the cluster CA's fingerprint, so
the joining machine only trusts this cluster's CA.

The token goes to standard output alone, so scripts can capture it. The
expiry goes to standard error. With --json, both go to standard output as JSON.`,
		Args: cobra.NoArgs,
	}
	cf := addClientFlags(cmd)
	cmd.Flags().StringVar(&ttl, "ttl", "", "how long the token lasts, such as 1h (default "+pki.DefaultTokenTTL.String()+")")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		c, err := cf.client()
		if err != nil {
			return err
		}
		var reply server.JoinTokenReply
		if err := c.call(cmd.Context(), "POST", "/v1/join-tokens", server.JoinTokenRequest{TTL: ttl}, &reply); err != nil {
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
		return nil
	}
	return cmd
}
