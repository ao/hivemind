# Backup and restore

A backup is a copy of the server's state, enough to start the same cluster on another machine: the store, the cluster CA, and the admin token file. Workers keep no state that the server needs, so there is nothing to back up on them. `backup` and `restore` are new in the unreleased 2.1.0; to back up a 2.0.0 server, use the newer binary's `backup`, as [upgrading](upgrading.md#from-200) describes.

A backup holds the cluster CA's private key. Whoever has the file can sign a certificate for any node ID, join machines to the cluster, and read every app's environment values and every registry password from the store. It also holds the admin token in plain text, when the data directory has the token file. Keep a backup as private as the data directory: mode 0600, on a disk only root or the hivemind user can read, and encrypted when it leaves the machine.

## Taking a backup

On the server, as the user that runs `serve` (root, with the [systemd units](systemd.md)):

```sh
sudo install -d -m 0700 /var/backups/hivemind
sudo hivemind backup /var/backups/hivemind/hivemind-$(date +%Y%m%d).tar
```

The directory must exist: `backup` does not create it, and some distributions, such as Amazon Linux 2023, have no `/var/backups`. `backup` reads the data directory (`--data-dir`, or the default) and writes the file with mode 0600. It refuses a file that exists. It writes to a temporary file beside the target, reads it back and checks every file's SHA-256, and only then links it to the name you gave, so the target never holds a partial backup.

It works whether `serve` is running or not. The store is copied with SQLite's `VACUUM INTO`, which reads the database in one read transaction and writes a consistent snapshot ([sqlite.org/lang_vacuum.html](https://sqlite.org/lang_vacuum.html#vacuuminto)). The store runs in WAL mode, where a reader does not block the writer ([sqlite.org/wal.html](https://sqlite.org/wal.html)), so the server keeps working during the copy. `VACUUM INTO` has been in SQLite since 3.27.0; the `modernc.org/sqlite` v1.62.0 that hivemind builds with bundles SQLite 3.54.1. SQLite does not sync the copy to disk, so hivemind does. The copy does not migrate or otherwise change the live store, so a `backup` from a newer hivemind binary is safe beside an older `serve`.

`hivemind backup <file> --api` asks the server for the backup instead, through `POST /v1/backup`, with the admin token. Use it from another machine. The server sends a backup only over TLS, or to a client on loopback, because the file holds the CA key. The CLI checks the archive the same way before it keeps it.

The default reads the data directory rather than calling the API so that the CA key does not cross a network at all, and so that a backup works while `serve` is stopped.

A cron job that keeps a week of daily backups, in the directory made above. cron's `PATH` does not include `/usr/local/bin`, so the job names the binary in full:

```sh
# /etc/cron.d/hivemind-backup
15 3 * * * root umask 077; /usr/local/bin/hivemind backup /var/backups/hivemind/$(date +\%a).tar.new && mv /var/backups/hivemind/$(date +\%a).tar.new /var/backups/hivemind/$(date +\%a).tar
```

## Restoring

```sh
sudo hivemind restore hivemind-20261114.tar --data-dir /var/lib/hivemind
```

As a user other than root, `restore` cannot create `/var/lib/hivemind` (`error: mkdir /var/lib/hivemind: permission denied`). Run it as the user that will run `serve`.

`restore` needs a data directory that is missing or empty, and refuses anything else. It checks the backup before it keeps anything:

- the format and its version. A backup from a newer format is refused;
- the size and SHA-256 of every file, so a truncated or altered file is refused;
- SQLite's integrity check on the store, and its schema version. A store at an older version is migrated forward when `serve` opens it, as any data directory is. A store from a newer hivemind is refused;
- the CA key matches the CA certificate.

The directory gets mode 0700 and every file mode 0600. On any failure, `restore` removes the files it wrote, and the directory if it created it. Like `init`, it writes a client config for the CLI on this machine, unless one exists.

## Replacing a lost server

The workers keep running their containers while the server is gone (see [concepts](concepts.md#server-and-workers)). To bring the cluster back on a new machine:

1. Give the new machine the address the old server advertised. Each worker dials the address in its `node.json`, and checks that the server's certificate names it.
2. Install the same or a newer hivemind, and Docker Engine.
3. Copy the latest backup to the machine, and run `sudo hivemind restore <file> --data-dir /var/lib/hivemind`.
4. Start the server with the same address: `sudo hivemind serve --advertise <address>`, or the [server unit](systemd.md#server), with the same `--cluster-port` as before if you changed it.

The workers reconnect on their next pass, without joining again: the CA is the same, so the new server's certificate chains to the CA each worker trusts, and each worker's certificate chains to the CA the server trusts. The server's own replicas start again on the new machine. Replicas that ran on workers keep running there. `scripts/integration-cluster.sh` tests this with three nodes in Docker: it backs up a running server, destroys its container, data directory and Docker volume, restores the backup on a new container at the same IP, and checks that both workers, which were not restarted, reconnect, and that the apps return to their replica counts with no replica running twice.

What the restored server does not have is everything that changed after the backup:

- Apps deployed, scaled or deleted since the backup are back as they were in the backup. Deploy again what changed.
- A node removed after the backup is not on the restored denylist, so the server accepts its certificate again. Run `hivemind node remove <id>` again for each node you removed since the backup.
- A join token used after the backup is unused again in the restored store, and works until it expires. Join tokens last 24 hours by default, so this matters for a backup less than a day old.
- A node that joined after the backup is not in the store. It registers again by itself on its next call, since its certificate is from the same CA.
- Volumes of replicas that ran on the old server are gone with its disk. Those replicas start with empty volumes. hivemind does not back up volumes.
- Certificate expiries recorded after the backup are refreshed as each node calls in.

A new address is not supported: the workers' `node.json` and the server's certificate both carry the old one. Join each worker again with an empty data directory and a new join token instead.

## The file

A backup is a tar archive. The entries, in order:

| Entry | Holds |
|---|---|
| `manifest.json` | `format` (`hivemind-backup`), `version` (1), `created`, `hivemind_version`, `schema_version`, `node_id`, and the `name`, `size` and `sha256` of every other entry |
| `hivemind.db` | A snapshot of the store, as one SQLite file with no WAL |
| `ca.key` | The cluster CA's private key, PEM |
| `ca.crt` | The cluster CA certificate, PEM |
| `admin.token` | The admin token, when the data directory has the file |

Every entry is a regular file with mode 0600. `restore` refuses any other entry, and any file the manifest does not list. The optional settings file, `hivemind.yaml`, is not in a backup; keep it with your configuration.

## The CA

The cluster CA lasts ten years from `init`. hivemind cannot rotate it. In the CA's last year, node certificates are issued to end when the CA does, and the server logs a warning once a day. When the CA expires, no node certificate verifies, and no worker can reach the server. Before then, create a new cluster with `init` in a new data directory, and join every machine to it. A backup carries the same CA, so restoring one does not extend it.
