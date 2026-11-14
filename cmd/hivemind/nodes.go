package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/ao/hivemind/internal/server"
	"github.com/ao/hivemind/internal/store"
)

func nodesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "nodes",
		Short: "List the nodes in the cluster",
		Long: `List the nodes: ID, name, CPU and memory as each node's Docker daemon reports
them, the age of its lease, whether it is ready or lost, and how many replicas
are placed on it. A node is lost when its lease is more than 15 s old.

ELIGIBILITY is "eligible" for a node that takes new replicas, "ineligible" for
one that does not (hivemind node eligibility), "draining" during a drain, and
"drained" after one, until the node is made eligible again or removed.

CERT EXPIRES is when the node's certificate expires, as the server recorded it
when it issued the certificate. A worker renews its certificate once two thirds
of its lifetime have passed, so a date less than four months away means the
node has not renewed. "-" means the server has no record yet.`,
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
	fmt.Fprintln(tw, "ID\tNAME\tROLE\tCPU\tMEMORY\tLEASE AGE\tSTATE\tELIGIBILITY\tREPLICAS\tCERT EXPIRES")
	now := time.Now()
	for _, n := range nodes {
		role := "worker"
		if n.Server {
			role = "server"
		}
		age := (time.Duration(n.LeaseAgeSeconds * float64(time.Second))).Round(time.Second)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\t%d\t%s\n", n.ID, firstNonEmpty(n.Name, "-"), role, n.CPU, bytesIEC(n.Memory),
			age, n.State, eligibility(n), n.Replicas, certExpiry(n.CertNotAfter, now))
	}
	tw.Flush()
}

// eligibility is the ELIGIBILITY column of hivemind nodes.
func eligibility(n server.NodeView) string {
	switch {
	case n.Drain != nil && n.Drain.State == string(store.DrainDraining):
		return "draining"
	case n.Drain != nil:
		return "drained"
	case !n.Eligible:
		return "ineligible"
	}
	return "eligible"
}

// certExpiry prints a certificate's expiry as a UTC date and the time left,
// such as "2027-11-14 (in 364d)", or "-" without a record.
func certExpiry(t *time.Time, now time.Time) string {
	if t == nil {
		return "-"
	}
	left := t.Sub(now)
	var rel string
	switch {
	case left <= 0:
		rel = "expired"
	case left >= 48*time.Hour:
		rel = fmt.Sprintf("in %dd", int(left.Hours()/24))
	case left >= time.Minute:
		rel = "in " + strings.TrimSuffix(left.Round(time.Minute).String(), "0s")
	default:
		rel = "in " + left.Round(time.Second).String()
	}
	return t.UTC().Format("2006-01-02") + " (" + rel + ")"
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
	cmd.AddCommand(nodeRemoveCmd(), nodeEligibilityCmd(), nodeDrainCmd())
	return cmd
}

func nodePath(id, sub string) string { return "/v1/nodes/" + url.PathEscape(id) + "/" + sub }

func nodeEligibilityCmd() *cobra.Command {
	var enable, disable bool
	cmd := &cobra.Command{
		Use:   "eligibility <id> --enable|--disable",
		Short: "Let a node take new replicas, or stop it taking them",
		Long: `Make a node eligible or ineligible for new replicas.

--disable stops the node from taking new replicas. It keeps the replicas it
runs, and a rolling update does not place a new copy on it either. Use it to
look at a node without new work arriving, or before draining several nodes, so
that replicas drained from one do not move to another that is about to be
drained. The server keeps the setting, so it holds across restarts of the
server and of the node.

--enable lets the node take new replicas again. Replicas that wait for a node
may be placed on it at once; replicas that run elsewhere stay where they are.
On a draining node, --enable cancels the drain: a copy that was starting on
another node is removed, and its replica stays on this one.

The server's own node can be made ineligible. It keeps serving the API and
running its containers.

This follows Nomad's node eligibility command:
https://developer.hashicorp.com/nomad/docs/commands/node/eligibility`,
		Args: cobra.ExactArgs(1),
	}
	cf := addClientFlags(cmd)
	cmd.Flags().BoolVar(&enable, "enable", false, "let the node take new replicas")
	cmd.Flags().BoolVar(&disable, "disable", false, "stop the node from taking new replicas")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if enable == disable {
			return errors.New("give one of --enable and --disable")
		}
		c, err := cf.client()
		if err != nil {
			return err
		}
		var reply server.EligibilityReply
		if err := c.call(cmd.Context(), "POST", nodePath(args[0], "eligibility"), server.EligibilityRequest{Eligible: &enable}, &reply); err != nil {
			return err
		}
		if cf.json {
			return printJSON(cmd.OutOrStdout(), reply)
		}
		out := cmd.OutOrStdout()
		name := firstNonEmpty(reply.Name, "-")
		switch {
		case !reply.Eligible:
			fmt.Fprintf(out, "%s (%s): ineligible; it keeps its replicas and takes no new ones\n", reply.ID, name)
		case reply.DrainCancelled:
			fmt.Fprintf(out, "%s (%s): eligible; its drain was cancelled, and the replicas still on it stay\n", reply.ID, name)
		default:
			fmt.Fprintf(out, "%s (%s): eligible\n", reply.ID, name)
		}
		return nil
	}
	return cmd
}

// drainPoll is how often node drain reads the drain while it waits.
var drainPoll = 2 * time.Second

func nodeDrainCmd() *cobra.Command {
	var deadline time.Duration
	var noWait bool
	cmd := &cobra.Command{
		Use:   "drain <id>",
		Short: "Move a node's replicas to other nodes",
		Long: `Drain a node: make it ineligible for new replicas, then move each of its
replicas to another node the way a rolling update moves one. A copy of the
replica starts on another eligible node, and the replica moves there once the
copy is ready and passes its health check. The node's agent then stops the old
container. Each app has one replica moving at a time, so an app keeps its
replica count running while another node has room for it.

A replica that cannot move stays on the node, and the drain says why: it has a
volume, whose data is on this node; its host port is in use on every other
eligible node; no other node has room for it; or its app has a rollout in
progress, which moves it instead. A replica whose copy crash-loops, or is not
ready within the rollout deadline, stays until the drain's deadline.

At --deadline the drain ends. A copy that is still starting takes its replica
at once. Every other replica left on the node, except one with a volume, is
stopped there and placed again, as when a node is lost; it waits as pending if
no node can take it. A replica with a volume keeps running on the node.

The command waits, and prints progress, until the drain is done. Ctrl-C stops
the waiting, not the drain. --no-wait returns at once. drain on a node that is
already draining waits for that drain, and --deadline sets a new deadline from
now. A drain carries on across a restart of the server.

A drained node stays in the cluster, ineligible, until "hivemind node
eligibility --enable", which also cancels a drain in progress, or "hivemind
node remove", which needs no --force once nothing runs on the node.

The server's own node can be drained. Its replicas move to workers, and the
server keeps serving the API and placing replicas.

This follows Nomad's node drain command:
https://developer.hashicorp.com/nomad/docs/commands/node/drain`,
		Args: cobra.ExactArgs(1),
	}
	cf := addClientFlags(cmd)
	cmd.Flags().DurationVar(&deadline, "deadline", store.DefaultDrainDeadline, "how long replicas may take to move; at the deadline the rest are stopped and placed again")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "start the drain and return without waiting for it")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if deadline <= 0 || deadline > store.MaxDrainDeadline {
			return fmt.Errorf("--deadline must be above 0 and at most %s", store.MaxDrainDeadline)
		}
		c, err := cf.client()
		if err != nil {
			return err
		}
		var req server.DrainRequest
		if cmd.Flags().Changed("deadline") {
			req.Deadline = deadline.String()
		}
		ctx, out := cmd.Context(), cmd.OutOrStdout()
		path := nodePath(args[0], "drain")
		var d server.DrainView
		if err := c.call(ctx, "POST", path, req, &d); err != nil {
			return err
		}
		p := &drainPrinter{out: out, seen: map[string]bool{}, quiet: cf.json}
		p.start(d)
		if noWait {
			if cf.json {
				return printJSON(out, d)
			}
			fmt.Fprintf(out, "Not waiting. hivemind nodes shows the drain, and hivemind node drain %s waits for it.\n", d.NodeID)
			return nil
		}
		for d.State == string(store.DrainDraining) {
			select {
			case <-ctx.Done():
				fmt.Fprintf(cmd.ErrOrStderr(), "Stopped waiting. The drain of %s carries on; hivemind nodes shows it.\n", d.NodeID)
				return nil
			case <-time.After(drainPoll):
			}
			var next server.DrainView
			if err := c.call(ctx, "GET", path, nil, &next); err != nil {
				var ae *apiError
				if errors.As(err, &ae) && ae.status == http.StatusNotFound {
					return fmt.Errorf("the drain of %s ended before it finished: %s", d.NodeID, ae.msg)
				}
				if ctx.Err() != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "Stopped waiting. The drain of %s carries on; hivemind nodes shows it.\n", d.NodeID)
					return nil
				}
				return err
			}
			d = next
			p.update(d)
		}
		if cf.json {
			return printJSON(out, d)
		}
		p.finish(d)
		return nil
	}
	return cmd
}

// drainPrinter prints a drain's progress: each move as it starts, each
// reason a replica stays, once each, and the count of moved replicas when it
// changes.
type drainPrinter struct {
	out   io.Writer
	seen  map[string]bool
	moved int
	quiet bool
}

func (p *drainPrinter) line(format string, args ...any) {
	if p.quiet {
		return
	}
	s := fmt.Sprintf(format, args...)
	if p.seen[s] {
		return
	}
	p.seen[s] = true
	fmt.Fprintln(p.out, s)
}

func (p *drainPrinter) start(d server.DrainView) {
	p.line("%s: draining %d %s; deadline %s (in %s)", d.NodeID, d.Remaining, plural(d.Remaining, "replica", "replicas"),
		d.Deadline.UTC().Format(time.RFC3339), max(time.Until(d.Deadline), 0).Round(time.Second))
	p.update(d)
}

func (p *drainPrinter) update(d server.DrainView) {
	for _, m := range d.Moving {
		p.line("  moving %s/%d to %s", m.App, m.Index, m.To)
	}
	if d.Moved > p.moved {
		p.moved = d.Moved
		p.line("  %d moved, %d left on the node", d.Moved, d.Remaining)
	}
	for _, b := range d.Blocked {
		p.line("  %s/%d stays: %s", b.App, b.Index, b.Reason)
	}
}

func (p *drainPrinter) finish(d server.DrainView) {
	fmt.Fprintf(p.out, "%s: drained; %d moved, %d stopped at the deadline and placed again, %d left on the node\n",
		d.NodeID, d.Moved, d.Stopped, d.Remaining)
	if d.Remaining > 0 {
		for _, b := range d.Blocked {
			fmt.Fprintf(p.out, "  %s/%d: %s\n", b.App, b.Index, b.Reason)
		}
	}
	if d.OldContainers > 0 {
		fmt.Fprintf(p.out, "  the node's agent is stopping %d old %s\n", d.OldContainers, plural(d.OldContainers, "container", "containers"))
	}
}

func nodeRemoveCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "remove <id>",
		Short: "Remove a node from the cluster for good",
		Long: `Remove a node from the cluster for good. Drain it, or stop hivemind on it,
first.

The server's own node cannot be removed. A node that still renews its lease,
because hivemind runs on it, is refused unless --force is given, or it is
ineligible and nothing runs on it, as after hivemind node drain.

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
