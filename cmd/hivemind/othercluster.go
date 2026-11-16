package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ao/hivemind/internal/auth"
	"github.com/ao/hivemind/internal/config"
)

// rootDataDir is the data directory of a hivemind that root set up, as
// docs/systemd.md does with sudo hivemind init. A variable so that tests
// can move it.
var rootDataDir = "/var/lib/hivemind"

// othersFilesError is the reason a CLI has no token when hivemind on this
// machine was set up by another user, such as root: path, or a directory on
// the way to it, exists and belongs to that user, and this user may not read
// it. Nothing is read to find out: the open is refused, and the owner comes
// from the file's metadata. The error says to run the command as that user,
// with sudo, rather than to run init, which would make a second cluster.
// It is nil for root, and when path can be read, does not exist, or belongs
// to this user.
func othersFilesError(path, cmdline string) error {
	uid := os.Getuid()
	if uid == 0 || path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err == nil {
		f.Close()
		return nil
	}
	if !errors.Is(err, fs.ErrPermission) {
		return nil
	}
	// The nearest of path and its parents that this user can stat is the
	// one that keeps it out.
	for p := path; ; p = filepath.Dir(p) {
		fi, err := os.Stat(p)
		if err == nil {
			owner, ok := fileOwner(fi)
			if !ok || owner == uid {
				return nil
			}
			who, sudo := "root", "sudo "
			if owner != 0 {
				name, known := userName(owner)
				who, sudo = "user "+name, "sudo -u "+name+" "
				if !known {
					sudo = "sudo -u '#" + name + "' "
				}
			}
			return fmt.Errorf("hivemind on this machine was set up by %s: this user cannot read %s, so this CLI has no API token for its cluster; "+
				"run %s%s, or give the token with --token or %s", who, path, sudo, cmdline, envToken)
		}
		if !errors.Is(err, fs.ErrPermission) || filepath.Dir(p) == p {
			return nil
		}
	}
}

// othersClusterError looks for the files of a hivemind another user set up on
// this machine, in the data directory this user's CLI would use and in
// root's (see othersFilesError).
func othersClusterError(cmdline string) error {
	dirs := []string{config.DataDir()}
	if rootDataDir != dirs[0] {
		dirs = append(dirs, rootDataDir)
	}
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		for _, p := range []string{dir, filepath.Join(dir, auth.TokenFileName), filepath.Join(dir, workerStateFile)} {
			if err := othersFilesError(p, cmdline); err != nil {
				return err
			}
		}
	}
	return nil
}

// userName is the name of the user with ID uid, or the ID and false.
func userName(uid int) (string, bool) {
	if u, err := user.LookupId(strconv.Itoa(uid)); err == nil && u.Username != "" {
		return u.Username, true
	}
	return strconv.Itoa(uid), false
}

// commandLine is cmd as the user would type it again: its path and
// arguments, with ... in place of its flags, whose values are not repeated
// because one may be a secret, and of any words after --.
func commandLine(cmd *cobra.Command) string {
	if cmd == nil {
		return "hivemind ..."
	}
	parts := []string{cmd.CommandPath()}
	args, more := cmd.Flags().Args(), cmd.Flags().NFlag() > 0
	if n := cmd.ArgsLenAtDash(); n >= 0 {
		args, more = args[:n], true
	}
	for _, a := range args {
		if a == "" || strings.ContainsAny(a, " \t\n'\"\\$`;&|<>()*?[]{}~#!") {
			a = strconv.Quote(a)
		}
		parts = append(parts, a)
	}
	if more {
		parts = append(parts, "...")
	}
	return strings.Join(parts, " ")
}
