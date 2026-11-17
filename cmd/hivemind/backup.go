package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/spf13/cobra"

	"github.com/ao/hivemind/internal/auth"
	"github.com/ao/hivemind/internal/backup"
	"github.com/ao/hivemind/internal/config"
	"github.com/ao/hivemind/internal/store"
)

const backupSecretNote = "The backup holds the cluster CA's private key, which can sign a certificate for any node, " +
	"and the admin token. Keep it as private as the data directory."

func backupCmd() *cobra.Command {
	var useAPI bool
	cmd := &cobra.Command{
		Use:   "backup <file>",
		Short: "Write a backup of the server's state to a file",
		Long: `Write a backup of the server's state to <file>, with mode 0600. The file must
not exist.

The backup holds the cluster CA's private key, which can sign a certificate for
any node, and the admin token. Anyone who has the file can join machines to the
cluster and call the API. Keep it as private as the data directory: mode 0600,
on a disk only you can read, and encrypted if it leaves the machine.

By default backup reads the data directory (--data-dir) on this machine. It
works whether serve is running or not: the store is copied with SQLite's
VACUUM INTO, which reads one consistent snapshot and does not block the
server. It needs read access to the data directory, so run it as the user
that runs serve.

--api asks the server for the backup instead (POST /v1/backup), with the
admin token. The server sends it only over TLS, or to a client on loopback.

The file is a tar archive: manifest.json, with a format version and the
SHA-256 of each file, then hivemind.db, ca.key, ca.crt and admin.token. backup
reads the file back and checks every digest before it keeps it. Restore it
with hivemind restore.`,
		Args: cobra.ExactArgs(1),
	}
	cf := addClientFlags(cmd)
	cmd.Flags().String("data-dir", "", "the server's data directory (env HIVEMIND_DATA_DIR; default /var/lib/hivemind for root, else ~/.local/state/hivemind)")
	cmd.Flags().BoolVar(&useAPI, "api", false, "get the backup from the server's API instead of reading the data directory")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		out := args[0]
		// The archive is written to a temporary file beside out, whose name
		// the user never typed, so a missing directory is reported here.
		dir := filepath.Dir(out)
		if fi, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("the directory %s does not exist; create it first (mkdir -p %s)", dir, dir)
		} else if err != nil {
			return err
		} else if !fi.IsDir() {
			return fmt.Errorf("%s is not a directory, so backup cannot write %s", dir, out)
		}
		if _, err := os.Lstat(out); err == nil {
			return fmt.Errorf("%s already exists; backup does not overwrite a file", out)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		var write func(ctx context.Context, w io.Writer) error
		if useAPI {
			c, err := cf.client()
			if err != nil {
				return err
			}
			write = func(ctx context.Context, w io.Writer) error {
				resp, err := c.send(ctx, "POST", "/v1/backup", nil)
				if err != nil {
					return olderServer(err, "backup through the API", "2.1")
				}
				defer resp.Body.Close()
				if _, err := io.Copy(w, resp.Body); err != nil {
					return fmt.Errorf("reading the backup from %s: %w", c.base, err)
				}
				return nil
			}
		} else {
			cfg, err := config.Load(cmd.Flags(), config.Options{})
			if err != nil {
				return err
			}
			dir := cfg.DataDir
			dbPath := filepath.Join(dir, storeFileName)
			if _, err := os.Stat(dbPath); err != nil {
				return fmt.Errorf("no server store in %s: %w", dir, err)
			}
			write = func(ctx context.Context, w io.Writer) error {
				_, err := backup.Write(ctx, w, backup.Options{DataDir: dir, TempDir: filepath.Dir(out), HivemindVersion: version,
					Snapshot: func(ctx context.Context, dest string) error { return store.SnapshotFile(ctx, dbPath, dest) }})
				return err
			}
		}
		m, size, err := writeBackupFile(cmd.Context(), out, write)
		if err != nil {
			return err
		}
		if cf.json {
			return printJSON(cmd.OutOrStdout(), m)
		}
		names := make([]string, 0, len(m.Files))
		for _, f := range m.Files {
			names = append(names, f.Name)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s (%d bytes, mode 0600): %v, schema version %d, node %s.\n",
			out, size, names, m.SchemaVersion, m.NodeID)
		fmt.Fprintln(cmd.ErrOrStderr(), backupSecretNote)
		return nil
	}
	return cmd
}

// writeBackupFile writes an archive to a temporary file beside out, with
// mode 0600, syncs it, reads it back with backup.Verify, and only then links
// it to out. out is never replaced, and never holds a partial archive.
func writeBackupFile(ctx context.Context, out string, write func(context.Context, io.Writer) error) (backup.Manifest, int64, error) {
	f, err := os.CreateTemp(filepath.Dir(out), ".hivemind-backup-*.tar")
	if err != nil {
		return backup.Manifest{}, 0, err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	werr := f.Chmod(0o600)
	if werr == nil {
		werr = write(ctx, f)
	}
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return backup.Manifest{}, 0, werr
	}
	rf, err := os.Open(tmp)
	if err != nil {
		return backup.Manifest{}, 0, err
	}
	m, err := backup.Verify(rf)
	fi, serr := rf.Stat()
	rf.Close()
	if err != nil {
		return backup.Manifest{}, 0, fmt.Errorf("the backup did not check out, and was not kept: %w", err)
	}
	if serr != nil {
		return backup.Manifest{}, 0, serr
	}
	if err := os.Link(tmp, out); err != nil {
		return backup.Manifest{}, 0, fmt.Errorf("saving the backup as %s: %w", out, err)
	}
	return m, fi.Size(), nil
}

func restoreCmd() *cobra.Command {
	var clientConfigFlag string
	cmd := &cobra.Command{
		Use:   "restore <file>",
		Short: "Restore a server's state from a backup into an empty data directory",
		Long: `Restore a backup made with hivemind backup into --data-dir, which must be
missing or empty. The directory gets mode 0700 and each file mode 0600.

restore checks the archive before it keeps anything: the format version, the
size and SHA-256 of every file, SQLite's integrity check on the store, and
that the CA key matches the CA certificate. A store at an older schema version
is migrated forward when serve opens it; a store from a newer hivemind is
refused. On any failure, the files it wrote are removed.

The backup holds the cluster CA's private key and the admin token. Delete
copies you no longer need.

Like init, restore writes a client config that points the CLI at this server
and at the restored token file, unless one exists.

To replace a lost server, restore and run "hivemind serve --advertise
<address>" with the address the workers dial. The workers reconnect without
joining again, because the CA and their certificates are unchanged. For a
DNS name, the new machine may have any address: add --cluster-addr <its IP>
while the name's record still names the old machine, and update the record.
For an IP address the workers dial, give the new machine that address, or
serve at the new one and run "hivemind node set-server <new address>" on
each worker. See docs/backup.md.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cmd.Flags(), config.Options{})
			if err != nil {
				return err
			}
			f, err := os.Open(args[0])
			if err != nil {
				return err
			}
			defer f.Close()
			m, err := backup.Restore(cmd.Context(), f, cfg.DataDir)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Restored the backup of node %s, made %s, into %s (schema version %d).\n",
				m.NodeID, m.Created.Format("2006-01-02 15:04:05 MST"), cfg.DataDir, m.SchemaVersion)
			hasToken := slices.ContainsFunc(m.Files, func(f backup.File) bool { return f.Name == auth.TokenFileName })
			if hasToken {
				ccPath, err := clientConfigPath(clientConfigFlag)
				if err == nil {
					tokenPath := filepath.Join(cfg.DataDir, auth.TokenFileName)
					if abs, aerr := filepath.Abs(tokenPath); aerr == nil {
						tokenPath = abs
					}
					switch err := writeClientConfig(ccPath, clientConfig{Server: serverURL(cfg), TokenFile: tokenPath}); {
					case err == nil:
						fmt.Fprintf(out, "The CLI on this machine reads the server address and token file from %s.\n", ccPath)
					case errors.Is(err, fs.ErrExist):
						fmt.Fprintf(out, "%s already exists and was left as it is.\n", ccPath)
					default:
						return err
					}
				}
			} else {
				fmt.Fprintln(out, "The backup has no admin token file; use the admin token you kept from init.")
			}
			fmt.Fprintln(out, "Start the server with `hivemind serve`, with --advertise set to the address the workers use. "+
				"At a new IP address, workers that dial the old one need `hivemind node set-server <new address>` on each; see docs/backup.md.")
			return nil
		},
	}
	cmd.Flags().String("data-dir", "", "the new data directory, missing or empty (env HIVEMIND_DATA_DIR; default /var/lib/hivemind for root, else ~/.local/state/hivemind)")
	cmd.Flags().StringVar(&clientConfigFlag, "client-config", "", "client config file to write (default "+defaultClientConfigHint+"; env HIVEMIND_CLIENT_CONFIG)")
	return cmd
}
