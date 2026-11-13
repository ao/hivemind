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
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/ao/hivemind/internal/server"
	"github.com/ao/hivemind/internal/spec"
)

// maxSpecFile caps an app file read by deploy -f, the same as the API's body limit.
const maxSpecFile = server.MaxBodyBytes

func deployCmd() *cobra.Command {
	var (
		file, name, image string
		replicas          int
		ports, env        []string
	)
	cmd := &cobra.Command{
		Use:   "deploy",
		Short: "Create or update an app",
		Long: `Create or update an app, either from flags or from a YAML or JSON file:

  hivemind deploy --name web --image nginx:alpine --replicas 2 --port 8080:80 --env MODE=prod
  hivemind deploy -f app.yaml

--port is [bind:]host:container[/udp]. A port is published on 127.0.0.1 unless
it names another bind address. -f - reads the spec from standard input.`,
		Args: cobra.NoArgs,
	}
	cf := addClientFlags(cmd)
	fl := cmd.Flags()
	fl.StringVarP(&file, "file", "f", "", "app spec file, YAML or JSON (- for standard input)")
	fl.StringVar(&name, "name", "", "app name")
	fl.StringVar(&image, "image", "", "container image")
	fl.IntVar(&replicas, "replicas", 0, "number of replicas (default 1)")
	fl.StringArrayVar(&ports, "port", nil, "published port, [bind:]host:container[/udp] (repeatable)")
	fl.StringArrayVar(&env, "env", nil, "environment variable, KEY=VALUE (repeatable)")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		var a spec.App
		if file != "" {
			for _, f := range []string{"name", "image", "replicas", "port", "env"} {
				if fl.Changed(f) {
					return fmt.Errorf("use -f or the --%s flag, not both", f)
				}
			}
			var err error
			if a, err = readSpecFile(cmd.InOrStdin(), file); err != nil {
				return err
			}
		} else {
			if name == "" || image == "" {
				return errors.New("deploy needs --name and --image, or -f app.yaml")
			}
			a = spec.App{Name: name, Image: image, Replicas: replicas}
			for _, p := range ports {
				pp, err := parsePort(p)
				if err != nil {
					return err
				}
				a.Ports = append(a.Ports, pp)
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
		c, err := cf.client()
		if err != nil {
			return err
		}
		var res server.PutResult
		if err := c.call(cmd.Context(), "PUT", appPath(a.Name), a, &res); err != nil {
			return err
		}
		if cf.json {
			return printJSON(cmd.OutOrStdout(), res)
		}
		verb := "updated"
		if res.Created {
			verb = "created"
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s: %s, generation %d, %d %s\n", res.App.Name, verb, res.App.Generation,
			res.App.Replicas, plural(res.App.Replicas, "replica", "replicas"))
		return nil
	}
	return cmd
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
		return a, fmt.Errorf("%s: %w", path, err)
	}
	return a, nil
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
		Args:  cobra.MaximumNArgs(1),
	}
	cf := addClientFlags(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		c, err := cf.client()
		if err != nil {
			return err
		}
		var apps []server.AppStatus
		if len(args) == 1 {
			var one server.AppStatus
			if err := c.call(cmd.Context(), "GET", appPath(args[0], "status"), nil, &one); err != nil {
				return err
			}
			if cf.json {
				return printJSON(cmd.OutOrStdout(), one)
			}
			apps = []server.AppStatus{one}
		} else {
			if err := c.call(cmd.Context(), "GET", "/v1/status", nil, &apps); err != nil {
				return err
			}
			if cf.json {
				return printJSON(cmd.OutOrStdout(), apps)
			}
		}
		printStatus(cmd.OutOrStdout(), apps)
		return nil
	}
	return cmd
}

func printStatus(out io.Writer, apps []server.AppStatus) {
	if len(apps) == 0 {
		fmt.Fprintln(out, "No apps.")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "APP\tIMAGE\tGENERATION\tDESIRED\tRUNNING")
	for _, a := range apps {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\n", a.Name, a.Image, a.Generation, a.Desired, a.Running)
	}
	tw.Flush()
	for _, a := range apps {
		if ro := a.Rollout; ro != nil {
			if ro.Reason != "" {
				fmt.Fprintf(out, "%s: %s\n", a.Name, ro.Reason)
			} else {
				fmt.Fprintf(out, "%s: rolling out generation %d\n", a.Name, ro.Target)
			}
		}
	}
	fmt.Fprintln(out)
	tw = tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "APP\tREPLICA\tNODE\tGENERATION\tSTATE\tCONTAINER\tDETAIL")
	for _, a := range apps {
		for _, r := range a.Replicas {
			state := r.State
			if r.Desired != "running" {
				state += " (removing)"
			}
			detail := firstNonEmpty(r.Error, r.PendingReason, "-")
			fmt.Fprintf(tw, "%s\t%d\t%s\t%d\t%s\t%s\t%s\n", a.Name, r.Index, firstNonEmpty(r.NodeID, "-"), r.Generation,
				state, firstNonEmpty(shortID(r.ContainerID), "-"), detail)
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
		Long:  "Print the log of one replica's container, replica 0 by default. -f keeps the stream open until Ctrl-C.",
		Args:  cobra.RangeArgs(1, 2),
	}
	cf := addClientFlags(cmd)
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
		return err
	}
	return cmd
}

func scaleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "scale <app> <replicas>",
		Short: "Set an app's replica count",
		Args:  cobra.ExactArgs(2),
	}
	cf := addClientFlags(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		n, err := strconv.Atoi(args[1])
		if err != nil {
			return fmt.Errorf("replicas %q: want a number", args[1])
		}
		c, err := cf.client()
		if err != nil {
			return err
		}
		var v server.AppView
		if err := c.call(cmd.Context(), "POST", appPath(args[0], "scale"), server.ScaleRequest{Replicas: n}, &v); err != nil {
			return err
		}
		if cf.json {
			return printJSON(cmd.OutOrStdout(), v)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s: %d %s\n", v.Name, v.Replicas, plural(v.Replicas, "replica", "replicas"))
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
