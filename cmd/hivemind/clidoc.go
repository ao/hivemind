package main

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// cliDocPath is the generated command reference, from the repository root.
const cliDocPath = "docs/cli.md"

// cliDocIntro is written after the title of docs/cli.md.
const cliDocIntro = `This file is generated from the command definitions. Run ` + "`make docs`" + ` after changing a
command, flag or help text. The test ` + "`TestCLIDocIsCurrent`" + ` fails when this file is out of date.

Settings are resolved in this order: a flag, then a ` + "`HIVEMIND_*`" + ` environment variable, then the
YAML config file (` + "`serve`" + ` and ` + "`init`" + ` read ` + "`<data-dir>/hivemind.yaml`" + ` unless ` + "`--config`" + ` is given),
then the default. The client commands read the server address and token from their flags, then from
` + "`HIVEMIND_SERVER`" + ` and ` + "`HIVEMIND_TOKEN`" + `, then from the client config that ` + "`init`" + ` writes.

` + "`init`" + ` and ` + "`serve`" + ` accept the client settings ` + "`--server`" + ` and ` + "`--token`" + `. They do not use them.
` + "`logs`" + ` accepts ` + "`--json`" + ` and ignores it.

Exit status is 0 on success and 1 on any error, including a bad flag or argument. Errors are written to
standard error with an ` + "`error:`" + ` prefix.
`

// envFlags are the flags that can also be set from the environment, as
// HIVEMIND_ followed by the flag name in upper case with underscores.
var envFlags = map[string]bool{
	"config": true, "data-dir": true, "api-addr": true, "advertise": true, "cluster-port": true,
	"api-tls-cert": true, "api-tls-key": true, "docker-host": true, "log-level": true, "log-format": true,
	"metrics-addr": true, "reconcile-interval": true, "server": true, "token": true, "client-config": true,
}

// hostDefaults are flags whose default depends on the user and the machine.
// The reference states the rule in place of a value.
var hostDefaults = map[string]string{
	"data-dir": "The default is /var/lib/hivemind for root, and $XDG_STATE_HOME/hivemind or ~/.local/state/hivemind for other users.",
}

func genDocsCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "gen-docs",
		Short:  "Write the command reference as Markdown",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := io.WriteString(cmd.OutOrStdout(), renderCLIDoc())
			return err
		},
	}
}

// renderCLIDoc returns the text of docs/cli.md. It builds its own command tree,
// so the output does not depend on the state of a running command. Help that
// cobra adds when it runs (help, completion and the help flag) is left out, and
// so are hidden commands.
func renderCLIDoc() string {
	root := newRoot(strings.NewReader(""), io.Discard, io.Discard)
	var b strings.Builder
	b.WriteString("# Command reference\n\n")
	b.WriteString(cliDocIntro)
	b.WriteString("\n")
	var walk func(c *cobra.Command, level int)
	walk = func(c *cobra.Command, level int) {
		writeCommandDoc(&b, c, level)
		for _, sub := range visibleCommands(c) {
			walk(sub, level+1)
		}
	}
	walk(root, 2)
	return b.String()
}

// visibleCommands returns the sub-commands a reader sees, sorted by name.
func visibleCommands(c *cobra.Command) []*cobra.Command {
	var out []*cobra.Command
	for _, s := range c.Commands() {
		if s.Hidden || s.Name() == "help" || s.Name() == "completion" {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

func writeCommandDoc(b *strings.Builder, c *cobra.Command, level int) {
	fmt.Fprintf(b, "%s %s\n\n", strings.Repeat("#", min(level, 6)), c.CommandPath())
	if text := strings.TrimSpace(c.Long); text != "" {
		b.WriteString(text + "\n\n")
	} else if text := strings.TrimSpace(c.Short); text != "" {
		b.WriteString(text + "\n\n")
	}
	fmt.Fprintf(b, "```\n%s\n```\n\n", c.UseLine())

	if subs := visibleCommands(c); len(subs) > 0 {
		b.WriteString("| Command | Description |\n|---|---|\n")
		for _, s := range subs {
			fmt.Fprintf(b, "| `%s` | %s |\n", s.Name(), escapeCell(s.Short))
		}
		b.WriteString("\n")
	}

	var flags []*pflag.Flag
	c.LocalFlags().VisitAll(func(f *pflag.Flag) {
		if !f.Hidden && f.Name != "help" {
			flags = append(flags, f)
		}
	})
	if len(flags) == 0 {
		return
	}
	sort.Slice(flags, func(i, j int) bool { return flags[i].Name < flags[j].Name })
	b.WriteString("| Flag | Type | Default | Environment | Description |\n|---|---|---|---|---|\n")
	for _, f := range flags {
		name := "`--" + f.Name + "`"
		if f.Shorthand != "" {
			name = "`-" + f.Shorthand + "`, " + name
		}
		env := "-"
		if envFlags[f.Name] {
			env = "`HIVEMIND_" + strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_")) + "`"
		}
		desc := f.Usage
		def := "-"
		if note, ok := hostDefaults[f.Name]; ok {
			def = "see description"
			desc = strings.TrimSuffix(desc, ".") + ". " + note
		} else if d := f.DefValue; d != "" && d != "false" && d != "0" && d != "[]" {
			def = "`" + d + "`"
		}
		fmt.Fprintf(b, "| %s | %s | %s | %s | %s |\n", name, flagType(f), def, env, escapeCell(desc))
	}
	b.WriteString("\n")
}

func flagType(f *pflag.Flag) string {
	if f.Value.Type() == "stringArray" {
		return "string, repeatable"
	}
	return f.Value.Type()
}

func escapeCell(s string) string {
	return strings.ReplaceAll(s, "|", `\|`)
}
