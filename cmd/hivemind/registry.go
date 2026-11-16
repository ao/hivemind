package main

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/ao/hivemind/internal/server"
	"github.com/ao/hivemind/internal/spec"
	"github.com/ao/hivemind/internal/store"
)

func registryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "registry",
		Short: "Store credentials for private image registries",
		Long: `Store the username and password, or token, that nodes send when they pull an
image from a private registry: GHCR, ECR, a private Docker Hub repository, or a
registry of your own. See docs/registries.md.`,
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(registryLoginCmd(), registryListCmd(), registryLogoutCmd())
	return cmd
}

func registryPath(host string) string { return "/v1/registries/" + url.PathEscape(host) }

func registryLoginCmd() *cobra.Command {
	var username, passwordFile string
	var passwordStdin bool
	cmd := &cobra.Command{
		Use:   "login <host> --username <name> (--password-stdin | --password-file <file>)",
		Short: "Store the credential that pulls from a registry send",
		Long: `Store the username and password, or token, that every node sends when it
pulls an image from host. A host is a registry as an image reference names it,
such as ghcr.io, registry.example.com:5000 or localhost:5000. Docker Hub is
docker.io; index.docker.io and registry-1.docker.io are the same host.

The password is read from standard input with --password-stdin, or from a file
with --password-file. No flag takes it on the command line, where the process
list would show it and the shell history would keep it. One trailing newline is
removed. --password-stdin refuses a terminal, which would echo the password as
it is typed: pipe it in instead.

The credential is not checked with the registry. A wrong one shows at the next
pull, in hivemind status. Logging in again replaces it. The change applies to
the next pull only: running containers keep running, and a node that has the
image already does not pull it again.

The server stores the password in its database as it is, like environment
values, and sends it to each node that runs an image from host. ECR tokens last
12 hours, so log in again before they expire, for example from cron:

  aws ecr get-login-password | hivemind registry login <account>.dkr.ecr.<region>.amazonaws.com --username AWS --password-stdin`,
		Args: cobra.ExactArgs(1),
	}
	cf := addClientFlags(cmd)
	fl := cmd.Flags()
	fl.StringVar(&username, "username", "", "the username, or AWS for ECR")
	fl.BoolVar(&passwordStdin, "password-stdin", false, "read the password or token from standard input")
	fl.StringVar(&passwordFile, "password-file", "", "read the password or token from this file")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		host, err := spec.ParseRegistryHost(args[0])
		if err != nil {
			return err
		}
		if username == "" {
			return errors.New("--username is required")
		}
		var password string
		switch {
		case passwordStdin && passwordFile != "":
			return errors.New("give --password-stdin or --password-file, not both")
		case passwordStdin:
			if f, ok := cmd.InOrStdin().(*os.File); ok {
				if fi, err := f.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
					return errors.New("--password-stdin reads a pipe or a file, not a terminal, which would echo the password; pipe it in, or use --password-file")
				}
			}
			if password, err = readPassword(cmd.InOrStdin(), "standard input"); err != nil {
				return err
			}
		case passwordFile != "":
			f, err := os.Open(passwordFile)
			if err != nil {
				return fmt.Errorf("--password-file: %w", err)
			}
			password, err = readPassword(f, passwordFile)
			f.Close()
			if err != nil {
				return err
			}
		default:
			return errors.New("give the password with --password-stdin or --password-file; there is no flag that takes it on the command line")
		}
		if err := store.ValidateRegistryCredential(username, password); err != nil {
			return errors.New(strings.TrimPrefix(err.Error(), store.ErrInvalid.Error()+": "))
		}
		c, err := cf.client()
		if err != nil {
			return err
		}
		var reg server.RegistryView
		if err := c.call(cmd.Context(), "PUT", registryPath(host), server.RegistryLogin{Username: username, Password: password}, &reg); err != nil {
			return err
		}
		if cf.json {
			return printJSON(cmd.OutOrStdout(), reg)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s: credential stored for %s, not checked with the registry; the next pull from %s sends it\n",
			reg.Host, reg.Username, reg.Host)
		return nil
	}
	return cmd
}

// readPassword reads a password from r: at most the store's limit, with one
// trailing newline (\n or \r\n) removed, as docker login --password-stdin
// does. Errors never contain the password.
func readPassword(r io.Reader, from string) (string, error) {
	b, err := io.ReadAll(io.LimitReader(r, store.MaxRegistryPasswordLen+3))
	if err != nil {
		return "", fmt.Errorf("reading the password from %s: %w", from, err)
	}
	s := strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
	switch {
	case s == "":
		return "", fmt.Errorf("the password from %s is empty", from)
	case len(s) > store.MaxRegistryPasswordLen:
		return "", fmt.Errorf("the password from %s is longer than %d bytes", from, store.MaxRegistryPasswordLen)
	}
	return s, nil
}

func registryListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the registries that have a stored credential",
		Long: `List each registry that has a stored credential: its host, the username, and
when it was last stored. Passwords are never shown; no API route returns one.`,
		Args: cobra.NoArgs,
	}
	cf := addClientFlags(cmd)
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		c, err := cf.client()
		if err != nil {
			return err
		}
		var list server.RegistryList
		if err := c.call(cmd.Context(), "GET", "/v1/registries", nil, &list); err != nil {
			return err
		}
		if cf.json {
			return printJSON(cmd.OutOrStdout(), list)
		}
		out := cmd.OutOrStdout()
		if len(list.Registries) == 0 {
			fmt.Fprintln(out, "No registries.")
			return nil
		}
		tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "HOST\tUSERNAME\tUPDATED")
		for _, r := range list.Registries {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Host, r.Username, r.UpdatedAt.UTC().Format(time.DateTime)+" UTC")
		}
		return tw.Flush()
	}
	return cmd
}

func registryLogoutCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logout <host>",
		Short: "Remove the stored credential for a registry",
		Long: `Remove the stored credential for a registry. Containers that run an image from
it keep running. The next pull from it sends no credential, so a new replica on
a node that does not have the image yet cannot start.`,
		Args: cobra.ExactArgs(1),
	}
	cf := addConnFlags(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		host, err := spec.ParseRegistryHost(args[0])
		if err != nil {
			return err
		}
		c, err := cf.client()
		if err != nil {
			return err
		}
		if err := c.call(cmd.Context(), "DELETE", registryPath(host), nil, nil); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s: credential removed; running containers keep running\n", host)
		return nil
	}
	return cmd
}
