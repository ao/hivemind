package main

import (
	"fmt"
	"io"
	"net/url"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/ao/hivemind/internal/server"
)

func nodesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "nodes",
		Short: "List the nodes in the cluster",
		Long: `List the nodes: ID, name, CPU and memory as each node's Docker daemon reports
them, the age of its lease, whether it is ready or lost, and how many replicas
are placed on it. A node is lost when its lease is more than 15 s old.`,
		Args: cobra.NoArgs,
	}
	cf := addClientFlags(cmd)
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		c, err := cf.client()
		if err != nil {
			return err
		}
		var nodes []server.NodeView
		if err := c.call(cmd.Context(), "GET", "/v1/nodes", nil, &nodes); err != nil {
			return err
		}
		if cf.json {
			return printJSON(cmd.OutOrStdout(), nodes)
		}
		printNodes(cmd.OutOrStdout(), nodes)
		return nil
	}
	return cmd
}

func printNodes(out io.Writer, nodes []server.NodeView) {
	if len(nodes) == 0 {
		fmt.Fprintln(out, "No nodes.")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tROLE\tCPU\tMEMORY\tLEASE AGE\tSTATE\tREPLICAS")
	for _, n := range nodes {
		role := "worker"
		if n.Server {
			role = "server"
		}
		age := (time.Duration(n.LeaseAgeSeconds * float64(time.Second))).Round(time.Second)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%d\n", n.ID, firstNonEmpty(n.Name, "-"), role, n.CPU, bytesIEC(n.Memory),
			age, n.State, n.Replicas)
	}
	tw.Flush()
}

// bytesIEC prints a byte count in the largest binary unit that keeps a whole
// number before the point, such as 7.6Gi.
func bytesIEC(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	v, i := float64(n), -1
	for v >= unit && i < 4 {
		v /= unit
		i++
	}
	return fmt.Sprintf("%.1f%ci", v, "KMGTP"[i])
}

func nodeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "node",
		Short: "Manage one node",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(nodeRemoveCmd())
	return cmd
}

func nodeRemoveCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "remove <id>",
		Short: "Remove a node from the cluster for good",
		Long: `Remove a node from the cluster for good. Stop hivemind on the node first.

The server's own node cannot be removed. A node that still renews its lease,
because hivemind runs on it, is refused unless --force is given.

On removal, the node's replicas are placed on other nodes. A replica with a
volume is not moved: its data was on the removed node. It waits with no node,
and its reason in status says so, until a deploy changes the app's spec (for
example, a new volume name) or the app is deleted.

The node's ID goes on the server's list of removed nodes. The server refuses
its certificate on the node API from then on, and never issues the ID again.
A removed worker that is still running stops with an error; its containers
keep running until they are removed by hand. To bring the machine back, join
it again with an empty data directory and a new join token.`,
		Args: cobra.ExactArgs(1),
	}
	cf := addClientFlags(cmd)
	cmd.Flags().BoolVar(&force, "force", false, "remove the node even though it still renews its lease")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		c, err := cf.client()
		if err != nil {
			return err
		}
		path := "/v1/nodes/" + url.PathEscape(args[0])
		if force {
			path += "?force=true"
		}
		var reply server.RemoveNodeReply
		if err := c.call(cmd.Context(), "DELETE", path, nil, &reply); err != nil {
			return err
		}
		if cf.json {
			return printJSON(cmd.OutOrStdout(), reply)
		}
		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "%s (%s): removed; %d %s placed again\n", reply.ID, firstNonEmpty(reply.Name, "-"),
			reply.Rescheduled, plural(reply.Rescheduled, "replica", "replicas"))
		if reply.Held > 0 {
			fmt.Fprintf(out, "%d volume %s had data on the node and wait with no node; see hivemind status\n",
				reply.Held, plural(reply.Held, "replica", "replicas"))
		}
		return nil
	}
	return cmd
}
