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

func serversCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "servers",
		Short: "List the servers of the cluster",
		Long: `List the servers of the cluster (docs/ha.md): ID, the host name of each
server's node, the address it advertises with its node listener's port,
its Raft role, and when the leader last heard from it.

ROLE is leader, follower, or nonvoter for a server that has joined and
is catching up with the log: it becomes a voter once it has. A cluster of
one lists its one server, and says so.

LAST CONTACT is how long ago the server that answered last heard from each
one: the leader hears from every follower at each heartbeat. "-" is never,
or another follower, which a follower cannot tell. When no server leads, as
while a majority is down, the server asked answers from its own copy of the
configuration, and a line after the table says there is no leader and why.`,
		Args: cobra.NoArgs,
	}
	cf := addClientFlags(cmd)
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		c, err := cf.client()
		if err != nil {
			return err
		}
		var rep server.ServersReply
		if err := c.call(cmd.Context(), "GET", "/v1/servers", nil, &rep); err != nil {
			return olderServer(err, "servers command", "2.3.0")
		}
		if cf.json {
			return printJSON(cmd.OutOrStdout(), rep)
		}
		printServers(cmd.OutOrStdout(), rep, time.Now())
		return nil
	}
	return cmd
}

func printServers(out io.Writer, rep server.ServersReply, now time.Time) {
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tADDRESS\tROLE\tLAST CONTACT")
	for _, s := range rep.Servers {
		addr := s.Advertise
		if s.NodePort != 0 {
			addr = fmt.Sprintf("%s:%d", s.Advertise, s.NodePort)
		}
		contact := "-"
		if s.LastContact != nil {
			contact = max(now.Sub(*s.LastContact), 0).Round(100*time.Millisecond).String() + " ago"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", termSafe(s.ID), termSafe(firstNonEmpty(s.Name, "-")), termSafe(firstNonEmpty(addr, "-")),
			termSafe(s.Role), contact)
	}
	tw.Flush()
	switch {
	case rep.Leader == "":
		fmt.Fprintf(out, "\nNo leader, as %s sees it: %s\n", termSafe(rep.AnsweredBy), termSafe(firstNonEmpty(rep.Note, "no server leads")))
	case rep.Note != "":
		fmt.Fprintf(out, "\n%s.\n", termSafe(rep.Note))
	}
}

func serverCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Change the servers of the cluster",
		Args:  cobra.NoArgs,
	}
	cmd.AddCommand(serverRemoveCmd())
	return cmd
}

func serverRemoveCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "remove <id>",
		Short: "Remove a server from the cluster",
		Long: `Remove a server from the cluster's Raft configuration and its list of
servers (docs/ha.md#membership). The removed server stops serving; its
containers keep running, and its node stays a node like any other: move its
replicas with hivemind node drain and remove it with hivemind node remove.
Its certificate is refused on the server port from then on. To make the
machine a server again, give it an empty data directory and a new server
join token.

The last voting server cannot be removed. A removal that would leave fewer
servers that the leader heard from in the last 10 s than a majority of
those that stay is refused, unless --force: a cluster of three with one
server down that loses another has no majority.`,
		Args: cobra.ExactArgs(1),
	}
	cf := addClientFlags(cmd)
	cmd.Flags().BoolVar(&force, "force", false, "remove the server even if the servers that stay cannot reach a majority")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		c, err := cf.client()
		if err != nil {
			return err
		}
		path := "/v1/servers/" + url.PathEscape(args[0])
		if force {
			path += "?force=true"
		}
		var rep server.ServerRemoval
		if err := c.call(cmd.Context(), "DELETE", path, nil, &rep); err != nil {
			return olderServer(err, "server remove", "2.3.0")
		}
		if cf.json {
			return printJSON(cmd.OutOrStdout(), rep)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Removed server %s. %s\n", termSafe(rep.ID), termSafe(rep.Next))
		return nil
	}
	return cmd
}
