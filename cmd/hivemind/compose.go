package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ao/hivemind/internal/compose"
	"github.com/ao/hivemind/internal/server"
	"github.com/ao/hivemind/internal/store"
)

func composeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "compose",
		Short: "Convert a Docker Compose file to apps, and deploy or delete them",
		Long: `Turn each service in a Docker Compose file into an app:

  hivemind compose convert compose.yaml   print the app specs
  hivemind compose deploy compose.yaml    create or update the apps
  hivemind compose delete compose.yaml    delete the apps

Only part of Compose is read. Any other key is refused, with its path and a
reason, and every refusal is listed in one error. Variables come from the
environment, then from a .env file beside the compose file, and a variable
that is not set and has no default is an error. A file that uses a variable
that starts with HIVEMIND_, such as HIVEMIND_TOKEN, is refused, and so is a
.env that sets one. A port with no address is published on 127.0.0.1, where
Compose would publish it on every interface. Apps do not find each other by
service name. A service's HTTPS routes go under x-hivemind: {routes: [...]},
each with host, port and an optional path, and so does a health check in the
app spec's form, such as health: {tcp: {port: 50051}}, which Compose cannot
give; it is used in place of healthcheck, and the two together are refused.
See docs/compose.md.`,
	}
	cmd.AddCommand(composeConvertCmd(), composeDeployCmd(), composeDeleteCmd())
	return cmd
}

func addPrefixFlag(cmd *cobra.Command) *string {
	return cmd.Flags().String("prefix", "", "name each app PREFIX-<service> instead of <service>")
}

func composeConvertCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "convert <file>",
		Short: "Print the app specs for a compose file",
		Long: `Print the app spec for each service in a compose file, as YAML, one
document per service, sorted by name. Each document is a file that
hivemind deploy -f reads. - reads the compose file from standard input, and
the .env file from the working directory.

Nothing is sent to the server. A note on standard error names each port that
is published on 127.0.0.1 because the file gives it no address.`,
		Args: cobra.ExactArgs(1),
	}
	prefix := addPrefixFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		res, err := convertComposeFile(cmd, args[0], *prefix)
		if err != nil {
			return err
		}
		out, err := compose.Marshal(res.Apps)
		if err != nil {
			return err
		}
		_, err = cmd.OutOrStdout().Write(out)
		return err
	}
	return cmd
}

func composeDeployCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deploy <file>",
		Short: "Create or update the apps in a compose file",
		Long: `Convert a compose file, check every app, and then create or update each one
as hivemind deploy -f does, in name order, printing a line for each, and a
warning on standard error for each replica that no node can take, as deploy
does.

Each app records the compose project that deployed it: the file's top-level
name, or else the name of the file's directory, as Docker Compose takes it
(the working directory for -). deploy updates an app only if the same project
deployed it, so running it again is safe, and it refuses an app of the same
name that hivemind deploy, the API or another project deployed. --replace
updates such an app anyway, and the project takes it over.

Nothing is sent until every service converts and its spec is valid. Then,
without --replace, deploy reads each app from the server before the first
write, and if any of them belongs to something else, it refuses the whole
file, naming every such app, and deploys nothing. If the server refuses an
app after that, the command stops there: the apps before it stay deployed,
and the error names them. The server checks the origin again at each write,
so an app that another deploy took in the meantime is still refused.`,
		Args: cobra.ExactArgs(1),
	}
	cf := addClientFlags(cmd)
	prefix := addPrefixFlag(cmd)
	replace := cmd.Flags().Bool("replace", false, "update an app of the same name that another project, hivemind deploy or the API deployed")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		res, err := convertComposeFile(cmd, args[0], *prefix)
		if err != nil {
			return err
		}
		project, err := composeProject(args[0], res.Name)
		if err != nil {
			return err
		}
		origin := "compose:" + project
		q := url.Values{"origin": {origin}}
		if *replace {
			q.Set("replace", "true")
		}
		c, err := cf.client()
		if err != nil {
			return err
		}
		if !*replace {
			if err := composeOwnerCheck(cmd.Context(), c, appNamesOf(res.Apps), origin, deployOverride); err != nil {
				return err
			}
		}
		results := []server.PutResult{}
		var done []string
		for _, a := range res.Apps {
			var r server.PutResult
			if err := c.call(cmd.Context(), "PUT", appPath(a.Spec.Name)+"?"+q.Encode(), a.Spec, &r); err != nil {
				return partialError("deploying", "deployed", a.Spec.Name, a.Service, done, err)
			}
			done = append(done, a.Spec.Name)
			results = append(results, r)
			if !cf.json {
				printPut(cmd.OutOrStdout(), r)
			}
			warnPending(cmd.ErrOrStderr(), r.App.Name, r.Pending, r.PendingCount)
		}
		if cf.json {
			return printJSON(cmd.OutOrStdout(), results)
		}
		return nil
	}
	return cmd
}

// ownerVerb holds the words of composeOwnerCheck's messages for deploy and
// for delete.
type ownerVerb struct {
	doing    string // "deploying"
	override string // the flag that skips the check, and what it does
	done     string // "deployed"
}

var (
	deployOverride = ownerVerb{"deploying", "hivemind compose deploy --replace (replace=true in the API) updates", "deployed"}
	deleteOverride = ownerVerb{"deleting", "hivemind compose delete --force deletes", "deleted"}
)

// composeOwnerCheck reads each app before the first write, and refuses the
// whole file, so that nothing is deployed or deleted, when any of them
// exists with another origin than origin: another project's, or none, from
// hivemind deploy or the API. The store checks again at each write, which
// refuses an app that changed hands after this check.
func composeOwnerCheck(ctx context.Context, c *apiClient, apps []string, origin string, verb ownerVerb) error {
	var conflicts []string
	project := strings.TrimPrefix(store.OriginText(origin), "from ")
	for _, name := range apps {
		var v server.AppView
		err := c.call(ctx, "GET", appPath(name), nil, &v)
		var ae *apiError
		switch {
		case err == nil:
			if v.Origin != origin {
				conflicts = append(conflicts, fmt.Sprintf("app %s was deployed %s, not from %s", name, store.OriginText(v.Origin), project))
			}
		case errors.As(err, &ae) && ae.status == http.StatusNotFound:
		default:
			return fmt.Errorf("reading app %s before %s: %w; nothing was %s", name, verb.doing, err, verb.done)
		}
	}
	if len(conflicts) == 0 {
		return nil
	}
	return fmt.Errorf("conflict: %s; %s %s anyway; nothing was %s",
		strings.Join(conflicts, "; "), verb.override, plural(len(conflicts), "it", "them"), verb.done)
}

// composeProject is the compose project of the file at path: the file's
// top-level name, or else the name of its directory, or of the working
// directory for standard input.
func composeProject(path, name string) (string, error) {
	if name != "" {
		return name, nil
	}
	dir := "."
	if path != "-" {
		dir = filepath.Dir(path)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if p := compose.ProjectName(abs); p != "" {
		return p, nil
	}
	return "", fmt.Errorf("the directory %s gives no compose project name; set the top-level name in %s", abs, fileName(path))
}

// composeDeleted is one line of compose delete --json.
type composeDeleted struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

func composeDeleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <file>",
		Short: "Delete the apps in a compose file",
		Long: `Delete the app for each service in a compose file, in name order, printing a
line for each. Only the service names and the top-level name are read, so
the file's variables need not be set unless the name uses one, and keys
that convert would refuse are ignored. An app that does not exist is
reported as not found and skipped. Volumes are kept, as hivemind delete
keeps them.

delete deletes only the apps the file's compose project deployed, as
deploy updates only those: it reads each app before the first delete, and
if any of them was deployed by hivemind deploy, the API or another project,
it refuses the whole file, naming every such app, and deletes nothing. The
server checks the project again at each delete. --force deletes such an app
anyway.`,
		Args: cobra.ExactArgs(1),
	}
	cf := addClientFlags(cmd)
	prefix := addPrefixFlag(cmd)
	force := cmd.Flags().Bool("force", false, "delete an app of the same name that another project, hivemind deploy or the API deployed")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		data, err := readComposeData(cmd.InOrStdin(), args[0])
		if err != nil {
			return err
		}
		dotenv, err := readDotEnv(args[0])
		if err != nil {
			return err
		}
		names, err := compose.AppNames(data, compose.Options{Prefix: *prefix, Lookup: os.LookupEnv, DotEnv: dotenv})
		if err != nil {
			return fmt.Errorf("%s: %w", fileName(args[0]), err)
		}
		project, err := composeProject(args[0], names.Name)
		if err != nil {
			return err
		}
		origin := "compose:" + project
		var q url.Values
		if !*force {
			q = url.Values{"origin": {origin}}
		}
		c, err := cf.client()
		if err != nil {
			return err
		}
		if !*force {
			if err := composeOwnerCheck(cmd.Context(), c, names.Apps, origin, deleteOverride); err != nil {
				return err
			}
		}
		results := []composeDeleted{}
		var done []string
		for _, name := range names.Apps {
			var res map[string]string
			path := appPath(name)
			if len(q) > 0 {
				path += "?" + q.Encode()
			}
			err := c.call(cmd.Context(), "DELETE", path, nil, &res)
			var ae *apiError
			switch {
			case err == nil:
				done = append(done, name)
				results = append(results, composeDeleted{name, "deleting"})
				if !cf.json {
					fmt.Fprintf(cmd.OutOrStdout(), "%s: deleting; its containers stop on the next reconcile pass\n", name)
				}
			case errors.As(err, &ae) && ae.status == http.StatusNotFound:
				results = append(results, composeDeleted{name, "not found"})
				if !cf.json {
					fmt.Fprintf(cmd.OutOrStdout(), "%s: not found\n", name)
				}
			default:
				return partialError("deleting", "deleted", name, "", done, err)
			}
		}
		if cf.json {
			return printJSON(cmd.OutOrStdout(), results)
		}
		return nil
	}
	return cmd
}

// partialError reports a request that failed after others succeeded.
func partialError(doing, did, app, service string, done []string, err error) error {
	what := "app " + app
	if service != "" && service != app {
		what += " (service " + service + ")"
	}
	if len(done) == 0 {
		return fmt.Errorf("%s %s: %w", doing, what, err)
	}
	return fmt.Errorf("%s %s: %w; already %s: %s", doing, what, err, did, strings.Join(done, ", "))
}

// convertComposeFile reads and converts a compose file, and prints its notes
// on standard error.
func convertComposeFile(cmd *cobra.Command, path, prefix string) (*compose.Result, error) {
	data, err := readComposeData(cmd.InOrStdin(), path)
	if err != nil {
		return nil, err
	}
	dotenv, err := readDotEnv(path)
	if err != nil {
		return nil, err
	}
	res, err := compose.Convert(data, compose.Options{Prefix: prefix, Lookup: os.LookupEnv, DotEnv: dotenv})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", fileName(path), err)
	}
	for _, n := range res.Notes {
		fmt.Fprintf(cmd.ErrOrStderr(), "note: %s\n", n)
	}
	return res, nil
}

// readComposeData reads the compose file, or standard input for "-", up to
// the same limit as deploy -f.
func readComposeData(stdin io.Reader, path string) ([]byte, error) {
	r := stdin
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		r = f
	}
	return readLimited(r, fileName(path))
}

// readDotEnv reads the .env file beside the compose file, or in the working
// directory when the file is standard input. A missing .env is nil.
func readDotEnv(path string) ([]byte, error) {
	dir := "."
	if path != "-" {
		dir = filepath.Dir(path)
	}
	p := filepath.Join(dir, ".env")
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readLimited(f, p)
}

func readLimited(r io.Reader, name string) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxSpecFile+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if len(b) > maxSpecFile {
		return nil, fmt.Errorf("%s is larger than %d bytes", name, maxSpecFile)
	}
	return b, nil
}

func fileName(path string) string {
	if path == "-" {
		return "standard input"
	}
	return path
}

func appNamesOf(apps []compose.App) []string {
	out := make([]string, 0, len(apps))
	for _, a := range apps {
		out = append(out, a.Spec.Name)
	}
	return out
}
