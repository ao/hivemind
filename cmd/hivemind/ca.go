package main

import (
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/ao/hivemind/internal/carotate"
	"github.com/ao/hivemind/internal/pki"
	"github.com/ao/hivemind/internal/server"
)

func caCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ca",
		Short: "Show the cluster CA, and rotate it",
		Long: `Show the cluster CA, and rotate it in place, without joining any machine
again. The steps run on the server, through its API, with the admin token.
See docs/ca-rotation.md.`,
	}
	cmd.AddCommand(caStatusCmd(), caRotateCmd())
	return cmd
}

func caStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the cluster CA and the rotation in progress",
		Long: `Show the cluster CA: its fingerprint and expiry, and which CA the server's
certificate is from, which join tokens name. During a rotation, also the new
CA, the phase, and for each node whether it is live, whether it trusts the CA
the rotation goes to (its target: the new CA, or after an abort the old one),
which CA its certificate is from, by file and fingerprint, and whether it is a
build without rotation; then the next step and the nodes it waits for.

Compare the fingerprints with the ones the workers log when they add a CA.`,
		Args: cobra.NoArgs,
	}
	cf := addClientFlags(cmd)
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		c, err := cf.client()
		if err != nil {
			return err
		}
		var st carotate.Status
		if err := c.call(cmd.Context(), "GET", "/v1/ca", nil, &st); err != nil {
			return olderServer(err, "CA rotation", "2.2")
		}
		if cf.json {
			return printJSON(cmd.OutOrStdout(), st)
		}
		printCAStatus(cmd.OutOrStdout(), st)
		return nil
	}
	return cmd
}

func caRotateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rotate",
		Short: "Rotate the cluster CA: start, renew, finish, or abort",
		Long: `Rotate the cluster CA in three steps, each run once hivemind ca status says
the one before it is done:

  hivemind ca rotate start    make the new CA; every node learns to trust both
  hivemind ca rotate renew    every node renews its certificate under the new CA
  hivemind ca rotate finish   the old CA is trusted nowhere, and its key is deleted

hivemind ca rotate abort undoes a rotation before finish. Each step is kept in
the server's store, so a server that stops in the middle of one finishes it
when it starts. Take a backup after start and after finish.`,
	}
	cmd.AddCommand(
		caStepCmd("start", "Make a new CA, and have every node trust it beside the old one",
			`Make a new CA beside the old one, in ca-next.crt and ca-next.key, and start a
rotation in phase trust. The server accepts certificates from both CAs, and
every worker of this build adds the new CA to what it trusts at its next
request; a worker of an earlier build cannot, and hivemind ca status names it.
Nothing is issued from the new CA yet. A backup taken now holds both CAs.`, false),
		caStepCmd("renew", "Have every node renew its certificate under the new CA",
			`Move the rotation to phase renew: each node that trusts the new CA renews its
certificate under it at once, and the server's own certificate moves to it
once every node trusts it. renew refuses while a live node does not trust the
new CA, and names each one; --force goes ahead anyway.`, true),
		caStepCmd("finish", "End the rotation: trust the new CA alone and delete the old one",
			`End the rotation: the server's certificate is from the new CA, the server and
every worker trust the new CA alone, ca-next.key and ca-next.crt replace
ca.key and ca.crt, and the old CA's key is deleted. finish refuses while a
node has not renewed, and names each one, with why; --force cuts those nodes
off, and each must join again with an empty data directory and a new join
token. Backups taken before finish hold the old CA's key: take a new one.
After an abort turned the rotation round, finish goes back to the old CA:
the backups taken during the rotation hold the new CA's key, and those from
before start are still good.`, true),
		caStepCmd("abort", "Undo a rotation that has not finished",
			`Undo a rotation that has not finished. In phase trust, the new CA is deleted at
once, and the workers stop trusting it at their next request. In phase renew,
some nodes may hold certificates from the new CA, so the rotation turns round
instead: the nodes renew back under the old CA, and hivemind ca rotate finish
ends it and deletes the new CA. Either way the cluster ends on the CA it had:
backups taken during the rotation hold the new CA's key, and those from
before start are still good.`, false),
	)
	return cmd
}

func caStepCmd(step, short, long string, forceable bool) *cobra.Command {
	var force bool
	cmd := &cobra.Command{Use: step, Short: short, Long: long, Args: cobra.NoArgs}
	cf := addClientFlags(cmd)
	if forceable {
		what := "go ahead while live nodes do not trust the new CA"
		if step == "finish" {
			what = "cut off the nodes that have not renewed"
		}
		cmd.Flags().BoolVar(&force, "force", false, what)
	}
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		c, err := cf.client()
		if err != nil {
			return err
		}
		var st carotate.Status
		if err := c.call(cmd.Context(), "POST", "/v1/ca/rotation/"+step, server.CARotationRequest{Force: force}, &st); err != nil {
			return olderServer(err, "CA rotation", "2.2")
		}
		if cf.json {
			return printJSON(cmd.OutOrStdout(), st)
		}
		out := cmd.OutOrStdout()
		switch {
		case step == "start":
			fmt.Fprintln(out, "Started a rotation of the cluster CA. Take a backup now: one from before this has no key of the new CA.")
		case step == "finish" || (step == "abort" && st.Rotation == nil):
			fmt.Fprintln(out, endedMessage(st.Ended))
		case step == "abort":
			fmt.Fprintln(out, "The rotation turned round: the nodes renew back under the old CA. Run hivemind ca rotate finish once the list below is empty.")
		}
		printCAStatus(out, st)
		return nil
	}
	return cmd
}

// printCAStatus prints what hivemind ca status shows.
func printCAStatus(out io.Writer, st carotate.Status) {
	ca := func(label string, c *carotate.CAInfo) {
		if c == nil {
			return
		}
		fmt.Fprintf(out, "%s: %s, fingerprint %s, valid until %s\n", label, termSafe(c.Name), c.Fingerprint, c.NotAfter.Format(time.DateOnly))
	}
	ca("Cluster CA (ca.crt)", st.Current)
	ca("New CA (ca-next.crt)", st.Next)
	which := "the cluster CA"
	if st.Next != nil && st.Listener == st.Next.Fingerprint {
		which = "the new CA"
	} else if st.Next != nil {
		which = "the cluster CA in ca.crt"
	}
	fmt.Fprintf(out, "The server's certificate is from %s; join tokens name it.\n", which)
	r := st.Rotation
	if r == nil {
		fmt.Fprintln(out, "No rotation is in progress.")
		return
	}
	// The CA the rotation goes to, the target, and the one it leaves, by
	// file and fingerprint: after an abort the rotation goes back to the CA
	// in ca.crt, and in the fourth acceptance run columns named new and old
	// read the wrong way round then.
	target, leaving := caFile(pki.NextCACertFile, r.To), caFile(pki.CACertFile, r.From)
	toward := "towards the new CA, " + target
	if r.Toward == "current" {
		target, leaving = caFile(pki.CACertFile, r.To), caFile(pki.NextCACertFile, r.From)
		toward = "back towards the old CA, " + target + ", after an abort"
	}
	fmt.Fprintf(out, "Rotation: phase %s, %s, started %s.\n", r.Phase, toward, r.StartedAt.Format(time.RFC3339))
	if len(st.Nodes) > 0 {
		fmt.Fprintln(out)
		tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NODE\tNAME\tVERSION\tLEASE\tTRUSTS TARGET\tCERTIFICATE FROM")
		for _, n := range st.Nodes {
			lease := "live"
			if !n.Live {
				lease = "lost"
			}
			trusts, from := "no", leaving
			if n.Trusts {
				trusts = "yes"
			}
			switch {
			case n.Renewed:
				from = target
			case !n.Seen:
				from, trusts = "-", "-"
			}
			if n.OldBuild {
				trusts = "no (old build)"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", n.ID, termSafe(firstNonEmpty(n.Name, "-")), termSafe(firstNonEmpty(n.Version, "-")),
				lease, trusts, from)
		}
		tw.Flush()
		fmt.Fprintln(out)
	}
	if len(r.Waiting) == 0 {
		fmt.Fprintf(out, "Next: hivemind ca rotate %s.\n", r.NextStep)
		return
	}
	fmt.Fprintf(out, "Next: hivemind ca rotate %s, which waits for:\n", r.NextStep)
	printWaiting(out, r.Waiting)
}

// endedMessage says what the end of a rotation means for the backups. An
// abort, and a finish after an abort turned the rotation round, leave the
// cluster on the CA it had before: the backups taken during the rotation
// hold the new CA's key, and those from before it are good. In the fourth
// acceptance run both said to delete every older backup, the good ones
// with the rest.
func endedMessage(e *carotate.Ended) string {
	switch {
	case e == nil:
		return "The rotation has ended. Take a new backup: hivemind ca status shows which CA the cluster has now."
	case e.Back:
		return "The rotation has ended, back on the CA the cluster had before it started (" + caFile(pki.CACertFile, e.Kept) + "). " +
			"Backups taken during the rotation hold the new CA's key, which the cluster no longer uses: delete them. " +
			"Backups from before the rotation started are still good."
	default:
		return "The rotation has ended on the new CA (" + caFile(pki.CACertFile, e.Kept) + "). " +
			"Backups taken before now hold the old CA's key, which the cluster no longer uses: take a new backup, and delete the old ones."
	}
}

// caFile names a CA by its file and the first 8 digits of its fingerprint,
// as "ca-next.crt 83d3dd58".
func caFile(file, fp string) string { return file + " " + fp[:min(len(fp), 8)] }

// rotationDirection is how hivemind status names a rotation's direction:
// "" towards the new CA, as the step names say, and the way back after an
// abort.
func rotationDirection(r *carotate.RotationInfo) string {
	if r.Toward == "current" {
		return ", back towards the old CA in ca.crt after an abort"
	}
	return ""
}

func printWaiting(out io.Writer, w []carotate.Waiting) {
	for _, n := range w {
		name := ""
		if n.Name != "" {
			name = " (" + termSafe(n.Name) + ")"
		}
		fmt.Fprintf(out, "  %s%s %s\n", n.Node, name, termSafe(n.Reason))
	}
}
