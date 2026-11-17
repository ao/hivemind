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

The workers keep running their containers while the server is gone (see [concepts](concepts.md#server-and-workers)). Each worker dials the address in its `node.json`, the one it joined, and checks that the server's certificate names it. When the server advertises a DNS name, which is the recommended set-up ([concepts](concepts.md#the-servers-address)), the new machine can have any IP address: update the name's record, and the workers follow it. When it advertises an IP address, give the new machine that address, or move each worker to the new one with `hivemind node set-server` ([below](#at-a-new-ip-address)). To bring the cluster back on a new machine:

1. Install the same or a newer hivemind, and Docker Engine.
2. Copy the latest backup to the machine, and run `sudo hivemind restore <file> --data-dir /var/lib/hivemind`.
3. Start the server with the address the workers dial, and with the same `--cluster-port` as before if you changed it: `sudo hivemind serve --advertise <address>`, or the [server unit](systemd.md#server).
   - A DNS name whose record still names the old machine resolves to none of this machine's addresses, and `serve` refuses it, and says so. Add this machine's address to bind: `sudo hivemind serve --advertise hivemind.example.internal --cluster-addr 10.0.0.9`. The certificate names both. `serve` logs a warning until the name resolves to that address.
   - An IP address must be on this machine: the old one, given to the new machine first, or its own, with the workers moved after.
4. For a DNS name, update its record to the new machine's address. For a new IP address, move each worker ([below](#at-a-new-ip-address)).

The restored server waits for the workers for its start grace, 2 minutes from the start by default. A worker that was up when the old server went down is `waiting` in `hivemind nodes` until it reports: it is not lost, its replicas are not placed again, and the routers keep sending its replicas traffic. Step 4 has to reach the workers within that time, including the time their resolvers keep the old record, up to its TTL. For a record with a longer TTL, lower the TTL well before you need it, update the record before you start `serve`, or give `serve` a longer grace, such as `--start-grace 10m`. A worker that reports after the grace has ended has had its replicas placed again on other nodes, and it removes its own copies. The grace has a cost: a worker that died while the server was down has its replicas placed again only when the grace ends, so they stay down that much longer after the restore. A worker that was already lost before the old server went down is lost at once.

The workers reconnect on their next pass, without joining again: the CA is the same, so the new server's certificate chains to the CA each worker trusts, and each worker's certificate chains to the CA the server trusts. A worker that dials a name looks it up again for each new connection, so it reaches the new machine once its resolver has the new record; a short TTL on the record shortens that wait. The server's certificate and its node's address follow the new `serve` flags: the certificate is issued at each start for `--advertise` and the address the listeners bind, and the node's address, which `hivemind nodes` shows, is that address. Nothing in the restored store keeps the old one, except what you put in an app: a port bound to the old server's address, such as `10.0.0.5:5432:5432`, stays pending with that address in its reason until the app is deployed with the new one. The server's own replicas start again on the new machine. Replicas that ran on workers keep running there. `scripts/integration-cluster.sh` tests this with three nodes in Docker: it backs up a running server, destroys its container, data directory and Docker volume, restores the backup on a new container at the same IP, and checks that both workers, which were not restarted, reconnect, and that the apps return to their replica counts with no replica running twice. `scripts/integration-newaddr.sh` tests a new IP address both ways: a server that advertises a Docker network alias is restored on a container at another IP, serves with `--cluster-addr`, and the alias moves to it 30 s later, past the lease timeout: the workers, never restarted, wait for it rather than being lost, and reconnect once the alias moves, and requests through a worker's router from before the loss, and through the new server's from its start, get no `503`; and a server that advertises its IP is restored at another, and its workers are moved with `hivemind node set-server`. In both, the workers keep their node IDs, certificates and containers, and the apps converge with no replica running twice.

What the restored server does not have is everything that changed after the backup:

- Apps deployed, scaled or deleted since the backup are back as they were in the backup. Deploy again what changed.
- A node removed after the backup is not on the restored denylist, so the server accepts its certificate again. Run `hivemind node remove <id>` again for each node you removed since the backup.
- A join token used after the backup is unused again in the restored store, and works until it expires. Join tokens last 24 hours by default, so this matters for a backup less than a day old.
- A node that joined after the backup is not in the store. It registers again by itself on its next call, since its certificate is from the same CA.
- Volumes of replicas that ran on the old server are gone with its disk. Those replicas start with empty volumes. hivemind does not back up volumes.
- Certificate expiries recorded after the backup are refreshed as each node calls in.

### At a new IP address

A worker that joined an IP address keeps dialing it, and the new server's certificate does not name it. Start the server at its own address, `sudo hivemind serve --advertise 10.0.0.9`. It logs a warning that the workers which joined the old address need `hivemind node set-server 10.0.0.9`, with the count of workers in the store. Then, on each worker:

```sh
sudo systemctl stop hivemind
sudo hivemind node set-server 10.0.0.9
sudo systemctl start hivemind
```

The worker's containers keep running while it is stopped, and it picks them up again when it starts. `set-server` changes the server address in `node.json` and nothing else: the node keeps its ID, key and certificate, and does not join again. It refuses to run beside the worker, which reads `node.json` only when it starts.

Before it writes `node.json`, `set-server` checks from the worker that the address is its own cluster's server, by the same trust that every connection uses:

- the server's certificate must chain to the cluster CA in the worker's data directory, and name the address given, and a node;
- the server must accept the worker's certificate, and answer a node API request for the worker's node ID. A server that says the node was removed is refused.

Anything else is refused with what was found, and `node.json` is left as it was:

```
error: the server at 10.0.0.9:4484 presents a certificate from the CA with fingerprint 5d1e..., not from this worker's cluster CA, fingerprint 3f9c...: it is another cluster's server, or something between this machine and 10.0.0.9 holds another CA. /var/lib/hivemind/node.json is unchanged
error: the server at 10.0.0.7:4484 has a certificate from this cluster's CA, but it names 10.0.0.9, not 10.0.0.7: give set-server the address the server advertises, or start the server with --advertise 10.0.0.7. /var/lib/hivemind/node.json is unchanged
error: cannot reach a server at 10.0.0.8:4484: connect: connection refused. /var/lib/hivemind/node.json is unchanged
error: 10.0.0.9:4485 is the default join port, but set-server takes the server's cluster port, 10.0.0.9:4484, or no port. /var/lib/hivemind/node.json is unchanged
```

The fingerprints are the SHA-256 of each CA's public key, the hex that a join token carries. A server whose certificate the worker accepts, but which refuses the worker's certificate in the TLS handshake, as a machine with a copy of a server certificate and another CA would, is refused too. A man in the middle without the cluster CA's key cannot pass the first check: the worker takes only a certificate signed by the CA in its own data directory.

To move a cluster that advertises an IP address to a DNS name, without a restore: start the server with `--advertise <name> --cluster-addr <its IP>`. Its certificate then names both, so the workers that dial the IP keep working. Then run `hivemind node set-server <name>` on each worker, as above, at any time.

The other way, from a name to the server's IP address, works the same: a server started with `--advertise <name> --cluster-addr <its IP>` has a certificate that names both, so stop each worker, run `hivemind node set-server <its IP>`, and start it again. Once every worker dials the IP, the server can be started with `--advertise <its IP>` alone. In the third soak ([soak](soak.md), on EC2 c7g.large machines) `set-server` took 0.13 s on each of three workers, in both directions.

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
