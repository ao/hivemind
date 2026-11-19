package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/ao/hivemind/internal/carotate"
	"github.com/ao/hivemind/internal/server"
	"github.com/ao/hivemind/internal/spec"
)

// maxSpecFile caps an app file read by deploy -f, the same as the API's body limit.
const maxSpecFile = server.MaxBodyBytes

func deployCmd() *cobra.Command {
	var (
		file, name, image   string
		replicas            int
		ports, env, volumes []string
	)
	cmd := &cobra.Command{
		Use:   "deploy [-- command [arg...]]",
		Short: "Create or update an app",
		Long: `Create or update an app, either from flags or from a YAML or JSON file:

  hivemind deploy --name web --image nginx:alpine --replicas 2 --port 8080:80 --env MODE=prod
  hivemind deploy --name db --image postgres:17 --volume data:/var/lib/postgresql/data
  hivemind deploy --name sleeper --image busybox:stable -- sleep 86400
  hivemind deploy -f app.yaml

--port is [bind:]host:container[/udp]. A port is published on 127.0.0.1 unless
it names another bind address. --volume is name:/path, a named volume on the
replica's node mounted at that path; an app with a volume has one replica. A
path on the host in place of the name, a bind mount, is not supported.
The words after -- are the container's command, in place of the image's. A
health check, resources and routes need a file. A route has a host, a
container port and, optionally, a path prefix (path: /api), so several apps
can share a host under different paths, and a protocol: http (HTTP/1.1), the
default, or h2c (protocol: h2c), HTTP/2 without TLS, for a gRPC server or
another that speaks it. hivemind routes shows them all. -f -
reads the spec from standard input. A key the spec does not have is refused
with its line, and the key it is closest to.

deploy returns once the server has stored the spec; the image is pulled and
the containers started after that. A replica that no node can take, such as
one whose host port another app publishes on every node, waits as pending,
and deploy prints a warning on standard error for it, with the reason
hivemind status shows and the app that holds the port; the exit status is
unchanged. --wait waits until every replica that should run is running and
ready at the new generation, and has stayed so for 3 seconds, printing why a
replica waits each time the reason changes. After --timeout (default 5m) it
prints the app's status and exits with status 1; the spec stays deployed.
--wait exits with status 1 at once, with the reason, when a replica has no
node for a reason that does not change by itself: a host port that a
running replica holds on every node that could take it, a bind address no
node has, or more CPU or memory than any node has.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 && cmd.ArgsLenAtDash() != 0 {
				return fmt.Errorf("unexpected argument %q: put the container's command after --, as in hivemind deploy --name x --image busybox -- sleep 60", args[0])
			}
			return nil
		},
	}
	cf := addClientFlags(cmd)
	fl := cmd.Flags()
	fl.StringVarP(&file, "file", "f", "", "app spec file, YAML or JSON (- for standard input)")
	fl.StringVar(&name, "name", "", "app name")
	fl.StringVar(&image, "image", "", "container image")
	fl.IntVar(&replicas, "replicas", 0, "number of replicas (default 1)")
	fl.StringArrayVar(&ports, "port", nil, "published port, [bind:]host:container[/udp] (repeatable)")
	fl.StringArrayVar(&env, "env", nil, "environment variable, KEY=VALUE (repeatable)")
	fl.StringArrayVar(&volumes, "volume", nil, "named volume, name:/path in the container (repeatable)")
	wf := addWaitFlags(cmd)
	cmd.RunE = func(cmd *cobra.Command, command []string) error {
		var a spec.App
		if file != "" {
			for _, f := range []string{"name", "image", "replicas", "port", "env", "volume"} {
				if fl.Changed(f) {
					return fmt.Errorf("use -f or the --%s flag, not both", f)
				}
			}
			if len(command) > 0 {
				return errors.New("use -f or a command after --, not both: the file's command key sets it")
			}
			var err error
			if a, err = readSpecFile(cmd.InOrStdin(), file); err != nil {
				return err
			}
		} else {
			if name == "" || image == "" {
				return errors.New("deploy needs --name and --image, or -f app.yaml")
			}
			a = spec.App{Name: name, Image: image, Replicas: replicas, Command: command}
			for _, p := range ports {
				pp, err := parsePort(p)
				if err != nil {
					return err
				}
				a.Ports = append(a.Ports, pp)
			}
			for _, v := range volumes {
				vn, vp, ok := strings.Cut(v, ":")
				if !ok || vn == "" || vp == "" {
					return fmt.Errorf("--volume %q: want name:/path", v)
				}
				if spec.IsHostPath(vn) {
					return fmt.Errorf("--volume %q: %s is a path on the host, and bind mounts are not supported: hivemind mounts named volumes only; give a name, as in --volume data:%s", v, vn, vp)
				}
				a.Volumes = append(a.Volumes, spec.Volume{Name: vn, Path: vp})
			}
			if len(env) > 0 {
				a.Env = map[string]string{}
			}
			for _, kv := range env {
				k, v, ok := strings.Cut(kv, "=")
				if !ok || k == "" {
					// The argument is not echoed: it may hold a secret.
					return errors.New("--env: want KEY=VALUE")
				}
				a.Env[k] = v
			}
		}
		if a.Name == "" {
			return errors.New("the app spec has no name")
		}
		if err := wf.check(cmd); err != nil {
			return err
		}
		c, err := cf.client()
		if err != nil {
			return err
		}
		var res server.PutResult
		if err := c.call(cmd.Context(), "PUT", appPath(a.Name), a, &res); err != nil {
			return err
		}
		progress := cmd.OutOrStdout()
		if cf.json {
			progress = cmd.ErrOrStderr()
		} else {
			printPut(cmd.OutOrStdout(), res)
		}
		if wf.wait {
			if err := stuckPending(res.App.Name, res.Pending, res.PendingCount); err != nil {
				return err
			}
		}
		warnPending(cmd.ErrOrStderr(), res.App.Name, res.Pending, res.PendingCount)
		if wf.wait {
			if err := waitReady(cmd.Context(), c, progress, res.App.Name, res.App.Generation, wf.timeout); err != nil {
				return err
			}
		}
		if cf.json {
			return printJSON(cmd.OutOrStdout(), res)
		}
		return nil
	}
	return cmd
}

// printPut prints the line for a put: whether it created the app, updated
// it, or found it as it was, and the generation and replica count.
func printPut(out io.Writer, res server.PutResult) {
	verb := "updated"
	switch {
	case res.Created:
		verb = "created"
	case res.Unchanged:
		verb = "unchanged"
	}
	fmt.Fprintf(out, "%s: %s, generation %d, %d %s\n", res.App.Name, verb, res.App.Generation,
		res.App.Replicas, plural(res.App.Replicas, "replica", "replicas"))
}

// stuckPending is the error --wait returns at once, rather than at its
// timeout, when a write left a replica with no node for a reason that does
// not change by itself. It is nil when there is none, and from a server
// older than 2.2, which lists no pending replicas.
func stuckPending(app string, pending []server.PendingReplica, count int) error {
	name := termSafe(app)
	for _, p := range pending {
		if !p.Stuck {
			continue
		}
		more := ""
		if count > 1 {
			more = fmt.Sprintf("; %d replicas have no node, and hivemind status %s shows them", count, name)
		}
		return fmt.Errorf("%s: replica %d has no node, and will not get one by itself: %s; the spec stays deployed%s",
			name, p.Replica, termSafe(p.Reason), more)
	}
	return nil
}

// warnPending prints a warning on w for the replicas a write left with no
// node, as its reply lists them: a line for each reason, naming its
// replicas, and one for those past the reply's list. The write stands.
func warnPending(w io.Writer, app string, pending []server.PendingReplica, count int) {
	name := termSafe(app)
	var reasons []string
	byReason := map[string][]string{}
	for _, p := range pending {
		if _, ok := byReason[p.Reason]; !ok {
			reasons = append(reasons, p.Reason)
		}
		byReason[p.Reason] = append(byReason[p.Reason], strconv.Itoa(p.Replica))
	}
	for _, r := range reasons {
		if idx := byReason[r]; len(idx) == 1 {
			fmt.Fprintf(w, "%s: warning: replica %s has no node: %s\n", name, idx[0], termSafe(r))
		} else {
			fmt.Fprintf(w, "%s: warning: replicas %s have no node: %s\n", name, strings.Join(idx, ", "), termSafe(r))
		}
	}
	if more := count - len(pending); more > 0 {
		fmt.Fprintf(w, "%s: warning: %d more %s no node; hivemind status %s shows them\n",
			name, more, plural(more, "replica has", "replicas have"), name)
	}
}

// readSpecFile decodes one app spec. YAML is a superset of JSON, so one
// decoder reads both. Unknown keys are errors.
func readSpecFile(stdin io.Reader, path string) (spec.App, error) {
	var a spec.App
	var r io.Reader = stdin
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return a, err
		}
		defer f.Close()
		r = f
	}
	b, err := io.ReadAll(io.LimitReader(r, maxSpecFile+1))
	if err != nil {
		return a, err
	}
	if len(b) > maxSpecFile {
		return a, fmt.Errorf("%s is larger than %d bytes", path, maxSpecFile)
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&a); err != nil {
		if errors.Is(err, io.EOF) {
			return a, fmt.Errorf("%s is empty", path)
		}
		return a, fmt.Errorf("%s: %w", path, specKeyErrors(err))
	}
	return a, nil
}

// unknownKeyRE matches the YAML decoder's line for a key the spec does not
// have, which names a Go type: "line 3: field replica not found in type
// spec.App".
var unknownKeyRE = regexp.MustCompile(`^line (\d+): field (.+) not found in type (\S+)$`)

// specKeys maps each struct type of an app spec, by its Go name, to its
// keys, and specPlaces names where it sits in a spec.
var (
	specKeys   = keysOf(reflect.TypeFor[spec.App](), map[string][]string{})
	specPlaces = map[string]string{"spec.App": "the app", "spec.Port": "a port", "spec.Volume": "a volume",
		"spec.Resources": "resources", "spec.Health": "health", "spec.HTTPCheck": "health.http",
		"spec.TCPCheck": "health.tcp", "spec.Route": "a route"}
)

// keysOf adds the YAML keys of t, and of every struct type in it, to out.
func keysOf(t reflect.Type, out map[string][]string) map[string][]string {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || out[t.String()] != nil {
		return out
	}
	out[t.String()] = []string{}
	var keys []string
	for i := range t.NumField() {
		f := t.Field(i)
		if k, _, _ := strings.Cut(f.Tag.Get("yaml"), ","); k != "" && k != "-" {
			keys = append(keys, k)
			keysOf(f.Type, out)
		}
	}
	out[t.String()] = keys
	return out
}

// specKeyErrors rewrites the decoder's errors for unknown keys, which name
// a Go type, as the key, where it is, and the known key it is closest to:
// "line 3: unknown key replica in the app (did you mean replicas?)". Other
// errors pass as they are.
func specKeyErrors(err error) error {
	var te *yaml.TypeError
	if !errors.As(err, &te) {
		return err
	}
	msgs := make([]string, 0, len(te.Errors))
	for _, e := range te.Errors {
		m := unknownKeyRE.FindStringSubmatch(e)
		if m == nil {
			msgs = append(msgs, e)
			continue
		}
		key, typ := m[2], m[3]
		place := specPlaces[typ]
		if place == "" {
			place = strings.TrimPrefix(typ, "spec.")
		}
		msg := fmt.Sprintf("line %s: unknown key %s in %s", m[1], key, place)
		if near := closestKey(key, specKeys[typ]); near != "" {
			msg += " (did you mean " + near + "?)"
		} else if keys := specKeys[typ]; len(keys) > 0 {
			msg += "; its keys are " + strings.Join(keys, ", ")
		}
		msgs = append(msgs, msg)
	}
	return errors.New(strings.Join(msgs, "; "))
}

// closestKey returns the key in keys nearest to s by edit distance, ignoring
// case and with _ read as -, when it is near enough to be a slip: at most 2
// edits, and fewer than half the length of s. Otherwise "".
func closestKey(s string, keys []string) string {
	norm := func(x string) string { return strings.ReplaceAll(strings.ToLower(x), "_", "-") }
	best, bestD := "", 3
	for _, k := range keys {
		if d := editDistance(norm(s), norm(k)); d < bestD && 2*d < len(s) {
			best, bestD = k, d
		}
	}
	return best
}

// editDistance is the Levenshtein distance between a and b, in bytes.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// parsePort reads [bind:]host:container[/proto]. An IPv6 bind goes in
// brackets: [::1]:8080:80.
func parsePort(s string) (spec.Port, error) {
	var p spec.Port
	rest, proto, _ := strings.Cut(s, "/")
	p.Protocol = proto
	i := strings.LastIndex(rest, ":")
	if i < 0 {
		return p, fmt.Errorf("--port %q: want [bind:]host:container[/udp]", s)
	}
	hostPart, ctr := rest[:i], rest[i+1:]
	if j := strings.LastIndex(hostPart, ":"); j >= 0 {
		p.Bind = strings.TrimSuffix(strings.TrimPrefix(hostPart[:j], "["), "]")
		hostPart = hostPart[j+1:]
	}
	var err1, err2 error
	p.Host, err1 = strconv.Atoi(hostPart)
	p.Container, err2 = strconv.Atoi(ctr)
	if err1 != nil || err2 != nil {
		return p, fmt.Errorf("--port %q: ports must be numbers", s)
	}
	return p, nil
}

func statusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status [app]",
		Short: "Show apps, their replicas and container state",
		Long: `Show each app, then each of its replicas and the state of its container.

DESIRED is the replica count the spec asks for. RUNNING counts the replicas
whose container is running, and READY those of them that are ready to take
traffic: passing their health check, or with none, and not crash-looping. A
replica that is running but unhealthy counts in RUNNING and not in READY.
During a rollout, a copy at the old generation counts until it is replaced.

DETAIL shows why a replica is pending or cannot run on its node, or else the
engine's last error for it. A running replica whose health check has not passed
shows the last check's failure, as "its last health check failed: tcp port
50051: ... connection refused"; after a restart for failing its check, it
shows that failure until the new container's own check passes or fails.

A line "<app>: warning: ..." after the first table says what keeps the app from
being served as its spec says, such as a router of a build before path routes,
which answers 503 for the app's host once the host has a path route.

While workers wait for their first report since the server started, after a
restart or a restore, a line after the tables says how many, and that their
replicas show as unknown, counted neither as running nor as ready, until
they report; when the start grace has ended with no report from any of them,
the line says that the server keeps them waiting, and what to do (see
hivemind nodes).`,
		Args: cobra.MaximumNArgs(1),
	}
	cf := addClientFlags(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		c, err := cf.client()
		if err != nil {
			return err
		}
		var got []appStatus
		if len(args) == 1 {
			var one appStatus
			if err := c.call(cmd.Context(), "GET", appPath(args[0], "status"), nil, &one); err != nil {
				return err
			}
			if cf.json {
				return printJSON(cmd.OutOrStdout(), one)
			}
			got = []appStatus{one}
		} else {
			if err := c.call(cmd.Context(), "GET", "/v1/status", nil, &got); err != nil {
				return err
			}
			if cf.json {
				return printJSON(cmd.OutOrStdout(), got)
			}
		}
		apps, readyKnown := statusRows(got)
		printStatus(cmd.OutOrStdout(), apps, readyKnown)
		// Workers waiting for their first report since the server
		// started: their replicas are unknown, and count neither as
		// running nor as ready, though they may serve. A server that
		// cannot list its nodes for this has nothing to add.
		var nodes []nodeRow
		if len(apps) > 0 && c.call(cmd.Context(), "GET", "/v1/nodes", nil, &nodes) == nil {
			if l := waitingLine(nodes); l != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "\n%s\n", l)
			}
		}
		if len(args) == 0 {
			// During a rotation of the cluster CA, the nodes its next step
			// waits for, builds without rotation among them. A server
			// without the route, or without a CA, has nothing to add.
			var ca carotate.Status
			if err := c.call(cmd.Context(), "GET", "/v1/ca", nil, &ca); err == nil && ca.Rotation != nil && len(ca.Rotation.Waiting) > 0 {
				out := cmd.OutOrStdout()
				fmt.Fprintf(out, "\nCluster CA rotation, phase %s: hivemind ca rotate %s waits for:\n", ca.Rotation.Phase, ca.Rotation.NextStep)
				printWaiting(out, ca.Rotation.Waiting)
			}
		}
		return nil
	}
	return cmd
}

// statusRows turns the statuses the CLI read into printStatus's arguments.
func statusRows(got []appStatus) ([]server.AppStatus, bool) {
	apps := make([]server.AppStatus, len(got))
	readyKnown := true
	for i, a := range got {
		apps[i] = a.AppStatus
		if a.Ready == nil {
			readyKnown = false
		} else {
			apps[i].Ready = *a.Ready
		}
	}
	return apps, readyKnown
}

// appStatus is an app's status as the CLI reads it. A server older than 2.2
// sends no ready count, so Ready is nil, and READY prints "-" rather than 0,
// which would say that nothing is ready.
type appStatus struct {
	server.AppStatus
	Ready *int `json:"ready"`
}

// printStatus prints the apps and their replicas. readyKnown is false when
// the server sent no ready counts.
func printStatus(out io.Writer, apps []server.AppStatus, readyKnown bool) {
	if len(apps) == 0 {
		fmt.Fprintln(out, "No apps.")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "APP\tIMAGE\tGENERATION\tDESIRED\tRUNNING\tREADY")
	for _, a := range apps {
		ready := "-"
		if readyKnown {
			ready = strconv.Itoa(a.Ready)
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%s\n", termSafe(a.Name), termSafe(a.Image), a.Generation, a.Desired, a.Running, ready)
	}
	tw.Flush()
	for _, a := range apps {
		if ro := a.Rollout; ro != nil {
			if ro.Reason != "" {
				fmt.Fprintf(out, "%s: %s\n", termSafe(a.Name), termSafe(ro.Reason))
			} else {
				fmt.Fprintf(out, "%s: rolling out generation %d\n", termSafe(a.Name), ro.Target)
			}
		}
		for _, w := range a.Warnings {
			fmt.Fprintf(out, "%s: warning: %s\n", termSafe(a.Name), termSafe(w))
		}
	}
	fmt.Fprintln(out)
	tw = tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "APP\tREPLICA\tNODE\tGENERATION\tSTATE\tHEALTH\tRESTARTS\tCONTAINER\tDETAIL")
	for _, a := range apps {
		for _, r := range a.Replicas {
			state := r.State
			if r.Desired != "running" {
				state += " (removing)"
			}
			// A replica on a node has a reason only when the server knows
			// why it cannot run there, which says more than the engine's
			// error from the last attempt.
			detail := firstNonEmpty(r.PendingReason, r.Error, "-")
			if r.CrashLooping {
				detail = "crash-looping"
				if r.Error != "" {
					detail += ": " + r.Error
				}
				if r.NextRestart != nil {
					detail += ", next restart in " + max(time.Until(*r.NextRestart), 0).Round(time.Second).String()
				}
			}
			fmt.Fprintf(tw, "%s\t%d\t%s\t%d\t%s\t%s\t%d\t%s\t%s\n", termSafe(a.Name), r.Index, termSafe(firstNonEmpty(r.NodeID, "-")), r.Generation,
				termSafe(state), termSafe(firstNonEmpty(r.Health, "-")), r.Restarts, termSafe(firstNonEmpty(shortID(r.ContainerID), "-")), termSafe(detail))
		}
	}
	tw.Flush()
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func logsCmd() *cobra.Command {
	var follow bool
	var tail int
	cmd := &cobra.Command{
		Use:   "logs <app> [replica]",
		Short: "Print a replica's container log",
		Long: `Print the log of one replica's container, replica 0 by default. -f keeps the
stream open until Ctrl-C or until the container exits.

The replica can run on any node. The server reads a worker's replica through
that worker, which must be running and of this version: a worker that does not
answer in 10 seconds is reported by its node ID.

-f does not reconnect. If the server shuts down or restarts, or the connection
to it breaks, the command says so and exits with an error; a server machine
that goes away without closing the connection is found in about 30 seconds.`,
		Args: cobra.RangeArgs(1, 2),
	}
	// The log is the container's own text, so it has no JSON form.
	cf := addConnFlags(cmd)
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new lines")
	cmd.Flags().IntVar(&tail, "tail", 0, "print only the last N lines (0 for all)")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		replica := 0
		if len(args) == 2 {
			n, err := strconv.Atoi(args[1])
			if err != nil || n < 0 {
				return fmt.Errorf("replica %q: want a number from 0", args[1])
			}
			replica = n
		}
		if tail < 0 {
			return errors.New("--tail must not be negative")
		}
		c, err := cf.client()
		if err != nil {
			return err
		}
		q := url.Values{"replica": {strconv.Itoa(replica)}, "tail": {strconv.Itoa(tail)}, "follow": {strconv.FormatBool(follow)}}
		ctx := cmd.Context()
		if !follow {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, requestTimeout)
			defer cancel()
		}
		resp, err := c.send(ctx, "GET", appPath(args[0], "logs")+"?"+q.Encode(), nil)
		if err != nil {
			if follow && errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		defer resp.Body.Close()
		_, err = io.Copy(cmd.OutOrStdout(), resp.Body)
		if follow && cmd.Context().Err() != nil {
			return nil // stopped with Ctrl-C or SIGTERM
		}
		return logEnd(err, resp.Trailer.Get(server.LogEndTrailer), follow)
	}
	return cmd
}

// logEnd is the result of a log command whose body ended with err and the
// server's trailer why (server.LogEndTrailer). A log that ended, because the
// container stopped, is not an error. A server that shut down, a stream that
// broke, or a connection that dropped is: the command does not reconnect.
// A server older than 2.2 sends no trailer, and a body it ended cleanly is
// taken as the end of the log.
func logEnd(err error, why string, follow bool) error {
	again := ""
	if follow {
		again = "; run the command again to go on following the log"
	}
	switch {
	case err != nil:
		return fmt.Errorf("the connection to the server broke during the log (%v): the server may have stopped or restarted%s", err, again)
	case why == server.LogEndShutdown:
		return fmt.Errorf("the server is shutting down, so it ended the log stream%s", again)
	case why == server.LogEndError:
		return fmt.Errorf("the server could not read the rest of the log; its log says why%s", again)
	}
	return nil
}

func scaleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "scale <app> <replicas>",
		Short: "Set an app's replica count",
		Long: `Set an app's replica count. Scaling keeps the generation: new replicas start
from the same spec, and scaling down removes the highest-numbered replicas.

A new replica that no node can take waits as pending, with a warning on
standard error, as deploy prints it. --wait waits until every replica that
should run is running and ready, and has stayed so for 3 seconds, as deploy
--wait does; after --timeout (default 5m) it prints the app's status and
exits with status 1. Like deploy --wait, it exits with status 1 at once when
a replica has no node for a reason that does not change by itself.`,
		Args: cobra.ExactArgs(2),
	}
	cf := addClientFlags(cmd)
	wf := addWaitFlags(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		n, err := strconv.Atoi(args[1])
		if err != nil {
			return fmt.Errorf("replicas %q: want a number", args[1])
		}
		if err := wf.check(cmd); err != nil {
			return err
		}
		c, err := cf.client()
		if err != nil {
			return err
		}
		var v server.ScaleResult
		if err := c.call(cmd.Context(), "POST", appPath(args[0], "scale"), server.ScaleRequest{Replicas: n}, &v); err != nil {
			return err
		}
		progress := cmd.OutOrStdout()
		if cf.json {
			progress = cmd.ErrOrStderr()
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "%s: %d %s\n", v.Name, v.Replicas, plural(v.Replicas, "replica", "replicas"))
		}
		if wf.wait {
			if err := stuckPending(v.Name, v.Pending, v.PendingCount); err != nil {
				return err
			}
		}
		warnPending(cmd.ErrOrStderr(), v.Name, v.Pending, v.PendingCount)
		if wf.wait {
			if err := waitReady(cmd.Context(), c, progress, v.Name, v.Generation, wf.timeout); err != nil {
				return err
			}
		}
		if cf.json {
			return printJSON(cmd.OutOrStdout(), v)
		}
		return nil
	}
	return cmd
}

func deleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <app>",
		Short: "Delete an app and stop its containers",
		Args:  cobra.ExactArgs(1),
	}
	cf := addClientFlags(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		c, err := cf.client()
		if err != nil {
			return err
		}
		var res map[string]string
		if err := c.call(cmd.Context(), "DELETE", appPath(args[0]), nil, &res); err != nil {
			return err
		}
		if cf.json {
			return printJSON(cmd.OutOrStdout(), res)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s: deleting; its containers stop on the next reconcile pass\n", args[0])
		return nil
	}
	return cmd
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
