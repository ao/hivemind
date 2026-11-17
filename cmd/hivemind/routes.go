package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/ao/hivemind/internal/server"
)

func routesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "routes",
		Short: "Show the routing table",
		Long: `Show the routing table that every router serves: the host and path of each
route, the app and container port it sends requests to, and the address:port
of each replica that takes its traffic now.

PATH "/" is the whole host. Any other path is a prefix: a request goes to the
route whose path is the longest prefix of its own on a segment boundary, so
/api takes /api and /api/x but not /apix, and to the whole host route when no
prefix matches. ENDPOINTS "-" means no replica of that route is ready, and the
routers answer 503 for it.

A router of a build before path routes cannot tell paths apart, so it is given
each host that has a path route with no replicas, and answers 503 for it. The
command names such routers on standard error; upgrade their nodes.`,
		Args: cobra.NoArgs,
	}
	cf := addClientFlags(cmd)
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		c, err := cf.client()
		if err != nil {
			return err
		}
		var list server.RouteList
		if err := c.call(cmd.Context(), "GET", "/v1/routes", nil, &list); err != nil {
			return olderServer(err, "routing table", "2.2")
		}
		if cf.json {
			return printJSON(cmd.OutOrStdout(), list)
		}
		printRoutes(cmd.OutOrStdout(), list.Routes)
		printOldRouters(cmd.ErrOrStderr(), list)
		return nil
	}
	return cmd
}

func printRoutes(out io.Writer, routes []server.RouteView) {
	if len(routes) == 0 {
		fmt.Fprintln(out, "No routes.")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "HOST\tPATH\tAPP\tPORT\tENDPOINTS")
	for _, r := range routes {
		// A server from before path routes sends no path: its routes are
		// whole hosts.
		eps := strings.Join(r.Endpoints, ",")
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", termSafe(r.Host), termSafe(firstNonEmpty(r.Path, "/")), termSafe(r.App),
			strconv.Itoa(r.Port), termSafe(firstNonEmpty(eps, "-")))
	}
	tw.Flush()
}

// printOldRouters names the routers that cannot read path routes: a
// warning for each that answers 503 for some host now, and a note for the
// others, which would once a host had a path route.
func printOldRouters(out io.Writer, list server.RouteList) {
	for _, o := range list.OldRouters {
		version := firstNonEmpty(o.Version, "no version")
		if len(o.Hosts) > 0 {
			fmt.Fprintf(out, "warning: the router on node %s (%s) is of a build before path routes, and answers 503 for %s; upgrade the node\n",
				termSafe(o.Node), termSafe(version), termSafe(strings.Join(o.Hosts, ", ")))
			continue
		}
		fmt.Fprintf(out, "note: the router on node %s (%s) is of a build before path routes; it would answer 503 for a host with a path route; upgrade the node first\n",
			termSafe(o.Node), termSafe(version))
	}
}
