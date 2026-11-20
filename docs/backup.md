# Backup and restore

A backup is a copy of the server's state, enough to start the same cluster on another machine: the store, the cluster CA, and the admin token file. Workers keep no state that the server needs, so there is nothing to back up on them. 2.0.0 has no `backup` or `restore`; to back up a 2.0.0 server, use the 2.2.0 binary's `backup`, as [upgrading](upgrading.md#from-200-to-220) describes.

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
- the CA key matches the CA certificate, and the CA files are the ones the store's [CA rotation](ca-rotation.md#backup-and-restore), if any, names. A backup taken during a rotation holds the new CA too, and `restore` says so, with the phase.

Then it raises the cluster's epoch in the restored store to one more than the backup's, and says so (`The cluster is at epoch 1 now, ...`). The workers that reach the restored server refuse a server at a lower epoch, such as the one the backup came from; see [if the old server comes back](#if-the-old-server-comes-back). The store is not migrated for that. A store from before epochs (schema version 16) has none yet: `restore` leaves it as it is, so that the release that wrote it can still serve it, as [going back to 2.0.0](upgrading.md#going-back-to-200) needs, and writes the epoch, 1, to `restored-epoch` beside it. This build's `serve` migrates the store, gives the cluster that epoch, and removes the file.

The directory gets mode 0700 and every file mode 0600. On any failure, `restore` removes the files it wrote, and the directory if it created it. When the backup has the admin token file, it writes a client config for the CLI on this machine, as `init` does, unless one exists. A backup without the file gets none, and `restore` says to use the admin token you kept from `init`.

### A cluster of several servers

In a cluster of several servers ([several servers](ha.md)), take the backup on any server: `hivemind backup` reads that server's own copy of the store and its CA files, which every server has, and `hivemind backup --api` goes to the leader. A server that joined has no `admin.token`, so its backups hold none, and neither does `backup --api` while such a server leads; the admin token works with the restored store all the same. The Raft directory, `raft/`, and `server.json` are not in a backup.

A backup names the cluster's first server, the one `init` made, as its node (`node node-...` in what `backup` prints, `node_id` in the manifest), on whichever server it was taken, and the restored server runs as that node. If the first server has been removed from the cluster, with `hivemind server remove` and `hivemind node remove`, 2.3.0 cannot serve the restored store: `serve` stops with `error: node "node-...": node was removed from the cluster`.

`restore` makes a cluster of one: it empties the store's list of servers, as well as raising the epoch. Start it with `serve`, and join the other servers again with `hivemind join-token --role server` and `hivemind join --role server`, each with an empty data directory; their old copies are at the lower epoch, which the workers refuse. Remove their old nodes with `hivemind node remove`. A data directory whose store lists servers and that has no Raft state, as when `raft/` was deleted, is refused by `serve`, which would otherwise start a second cluster beside the others: restore a backup into an empty directory instead.

While a majority of the servers is up, a lost server needs no restore: remove it with `hivemind server remove <id>`, and join a new machine as a server. A restore is for losing a majority of them for good.

## Replacing a lost server

The workers keep running their containers while the server is gone (see [concepts](concepts.md#server-and-workers)). Each worker dials the address in its `node.json`, the one it joined, and checks that the server's certificate names it. When the server advertises a DNS name, which is the recommended set-up ([concepts](concepts.md#the-servers-address)), the new machine can have any IP address: update the name's record, and the workers follow it. When it advertises an IP address, give the new machine that address, or move each worker to the new one with `hivemind node set-server` ([below](#at-a-new-ip-address)).

Make sure the old server cannot come back first: disable its unit (`systemctl disable hivemind`), or terminate the machine. What happens if it does is under [if the old server comes back](#if-the-old-server-comes-back).

To bring the cluster back on a new machine:

1. Install the same or a newer hivemind, and Docker Engine.
2. Copy the latest backup to the machine, and run `sudo hivemind restore <file> --data-dir /var/lib/hivemind`.
3. Start the server with the address the workers dial, and with the same `--cluster-port` as before if you changed it: `sudo hivemind serve --advertise <address>`, or the [server unit](systemd.md#server).
   - A DNS name whose record still names the old machine resolves to none of this machine's addresses, and `serve` refuses it, and says so. Add this machine's address to bind: `sudo hivemind serve --advertise hivemind.example.internal --cluster-addr 10.0.0.9`. The certificate names both. `serve` logs a warning at its start that the name resolves elsewhere.
   - An IP address must be on this machine: the old one, given to the new machine first, or its own, with the workers moved after. `serve` refuses one that is not, and a `--cluster-addr` that is not, and lists this machine's addresses: `--cluster-addr 172.31.28.99 is not an address of this machine, so the cluster listeners cannot bind it; this machine's addresses are 127.0.0.1, 172.31.28.11, ::1. Give one of them`. It records nothing of a start that fails, so the next start compares its address with the last one that ran.
4. For a DNS name, update its record to the new machine's address. For a new IP address, move each worker ([below](#at-a-new-ip-address)).
5. For a cluster with [routers](routing.md), at a new address: take the old server's address out of the DNS records of every routed host it was in, and add the new machine's if it runs a router (`--route-addr`). The CA validates each HTTP-01 renewal against an address of the record, so while the dead address is listed, renewals can fail against it, and so do the requests that clients send there ([routing](routing.md#dns)). In the fourth soak, with the dead address first, every renewal failed until the records were put right.

The restored server waits for the workers for its start grace, 2 minutes from the start by default. A worker that was up when the old server went down is `waiting` in `hivemind nodes` until it reports: it is not lost, its replicas are not placed again, and the routers keep sending its replicas traffic. Until it reports, `hivemind status` shows its replicas as `unknown`, and counts them neither as running nor as ready, so an app on the workers shows `RUNNING 0` while it serves; a line after its tables says so, as `2 workers are waiting for their first report since the server started (start grace: 1m40s left); their replicas show as unknown until they report.`, and `hivemind nodes` prints the same line. Step 4 has to reach the workers within that time, including the time their resolvers keep the old record, up to its TTL. For a record with a longer TTL, lower the TTL well before you need it, update the record before you start `serve`, or give `serve` a longer grace, such as `--start-grace 10m`. A worker that reports after the grace has ended, when another did in time, has had its replicas placed again on other nodes, and it removes its own copies. The grace has a cost: a worker that died while the server was down has its replicas placed again only when the grace ends, so they stay down that much longer after the restore. A worker that was already lost before the old server went down is lost at once.

When the grace ends and none of the workers that were waiting has reported, the server marks none of them lost: they stay `waiting`, their replicas are not placed again, and `serve` logs, once and then once an hour while it lasts:

```
level=ERROR msg="no worker has reported to this server since it started, and its start grace has ended: the workers that were up before the start stay waiting, not lost, and their replicas are not placed again. An old copy of a cluster that was restored elsewhere sees this, and so does a server cut off from its workers. If the cluster was restored on another machine, stop this server and disable its service; if a worker is really gone, remove it with hivemind node remove <id>, and its replicas are placed again (docs/backup.md#if-the-old-server-comes-back)" waiting=3 nodes=node-95fe5bc3b6e3,node-2c41d07a9e18,node-c8a3f1e05b77 started=2026-11-18T13:21:56Z start_grace_ended=2026-11-18T13:23:56Z
```

`hivemind status` and `hivemind nodes` say it too, in the line after their tables: `2 workers are waiting: none of them has reported since the server started, and its start grace has ended, so the server keeps them waiting and places none of their replicas again; ...`, and `nodes --json` has `"start_held": true` on each. As soon as one of them reports, the hold ends, and `serve` logs that, with the workers that still wait and until when: they get the start grace again, from that report, and `hivemind status` and `hivemind nodes` say so, as `1 worker is waiting for its first report since the server started: another worker's report ended the hold, and it has the start grace again (1m55s left); ...`. Until then they stay `waiting`, none of their replicas is placed again, and the routing table keeps their endpoints; those that have not reported by its end are lost, and their replicas placed again. A network that heals heals for each worker at its own moment, and a worker that cannot reach the server backs off to one pass every 30 s: in the fifth soak, where the others were lost as soon as the first one reported, a worker 0.5 s behind it was lost, and one 12 s behind had its replicas started again on the server's machine ([soak](soak.md#the-start-graces-hold)). What the hold costs: a cluster whose every worker died while the server was down keeps their replicas unplaced, and those apps down, until you remove each dead node with `hivemind node remove <id>`, which places its replicas again. That is rare, and it is the better default: a server that hears from none of its workers is more often the old copy of a cluster restored elsewhere, back on, whose workers refuse it, or a server cut off from its workers by a network fault. Placing every replica on its own machine then starts a second copy of each app, which its router serves to any client that reaches it; in the fourth soak an old server did that with all 17 movable replicas 2 minutes after it came back ([soak](soak.md#fourth-soak-path-routes-the-start-grace-and-the-old-server-back)). For a server cut off, the workers find their replicas placed elsewhere when the network heals, and remove their own copies.

The workers reconnect on their next pass, without joining again: the CA is the same, so the new server's certificate chains to the CA each worker trusts, and each worker's certificate chains to the CA the server trusts. A worker that dials a name looks it up again for each new connection, so it reaches the new machine once its resolver has the new record; a short TTL on the record shortens that wait. The server's certificate and its node's address follow the new `serve` flags: the certificate is issued at each start for `--advertise` and the address the listeners bind, and the node's address, which `hivemind nodes` shows, is that address. Nothing in the restored store keeps the old one, except what you put in an app: a port bound to the old server's address, such as `10.0.0.5:5432:5432`, stays pending with that address in its reason until the app is deployed with the new one. The server's own replicas start again on the new machine. Replicas that ran on workers keep running there. `scripts/integration-cluster.sh` tests this with three nodes in Docker: it backs up a running server, destroys its container, data directory and Docker volume, restores the backup on a new container at the same IP, and checks that both workers, which were not restarted, reconnect, and that the apps return to their replica counts with no replica running twice. `scripts/integration-newaddr.sh` tests a new IP address both ways: a server that advertises a Docker network alias is restored on a container at another IP, serves with `--cluster-addr`, and the alias moves to it 30 s later, past the lease timeout: the workers, never restarted, wait for it rather than being lost, and reconnect once the alias moves, and requests through a worker's router from before the loss, and through the new server's from its start, get no `503`; and a server that advertises its IP is restored at another, and its workers are moved with `hivemind node set-server`. In both, the workers keep their node IDs, certificates and containers, and the apps converge with no replica running twice. `scripts/fault-suite.sh` restores the server by name the same way under two of its faults. With a worker cut off from the server, the backup is the latest one, taken after that worker's lease lapsed and its replicas moved: the restored server shows that worker as lost at once and the others as waiting, the others report once the alias moves, with the containers they ran, and the cut-off worker, once healed, reaches the new server by name without joining again and removes the copies that moved. With 2000 ms of delay on a worker's link, every worker waits and then reports inside the start grace, the slow one too, and no container on any worker changes.

What the restored server does not have is everything that changed after the backup:

- Apps deployed, scaled or deleted since the backup are back as they were in the backup. Deploy again what changed.
- A node removed after the backup is not on the restored denylist, so the server accepts its certificate again. Run `hivemind node remove <id>` again for each node you removed since the backup.
- A join token used after the backup is unused again in the restored store, and works until it expires. Join tokens last 24 hours by default, so this matters for a backup less than a day old.
- A node that joined after the backup is not in the store. It registers again by itself on its next call, since its certificate is from the same CA.
- Volumes of replicas that ran on the old server are gone with its disk. Those replicas start with empty volumes. hivemind does not back up volumes.
- Certificate expiries recorded after the backup are refreshed as each node calls in.

### If the old server comes back

The old machine, back with its data directory, runs its `serve` as a second server of the same cluster, with the same CA, unless `serve` refuses its `--advertise` address: a DNS name, without `--cluster-addr`, whose record now names the new machine. A server with `--cluster-addr`, or one that advertises its IP address, starts.

What keeps the workers off it is the cluster's epoch. `init` starts a cluster at epoch 0, and a store from before epochs is at 0 too. `restore` makes the restored store's epoch one more than the backup's. The server sends its epoch on every node API reply, in a `Hivemind-Epoch` header, and logs it at its start (`cluster listening ... epoch=1`). Each worker keeps the highest epoch it has seen in `cluster-epoch`, in its data directory, and sends it with every request. A worker that has had a reply from the restored server refuses a server at a lower epoch. It sends that server nothing more, leaves its containers as they are, and logs once, at error, for each address that answers so; for a server that is a name, the line names the address it led to:

```
level=ERROR msg="the server at hivemind.example.internal:4484 (10.0.0.5:4484) is from before a restore of this cluster: a restored copy of the cluster has been seen at a higher epoch. This worker sends the old server nothing, keeps its containers running, and asks again every 10s on a new connection, as the restored server may come back at this address. Stop the old server; if this worker should dial another address, stop the worker and move it with hivemind node set-server <address> (docs/backup.md)" server=hivemind.example.internal:4484 address=10.0.0.5:4484 server_epoch=0 seen_epoch=1
```

It asks again every 10 s, since the restored server may come back at that address, and logs `... answers at this cluster's epoch now; this worker talks to it again` when it does. It closes the connection the refused reply came on, and every other connection it has to the server, so each ask is a new connection: a worker that dials a name looks it up again, and reaches the restored server at the first ask after its resolver leads there. The "talks to it again" line is only for an address the worker refused; a reply from another machine behind the name, such as the restored server, is taken without a line, and the refusal of an address is logged again only after the worker has taken that address again. With two machines behind one name, as while a record or a resolver still leads some requests to the old server, the worker refuses the old one and takes the restored one, and logs one line for the old one. The old server, for its part, refuses with `409` every request from a node that has seen a higher epoch, before it reads its store, and logs once:

```
level=ERROR msg="a newer copy of this cluster exists: this server was replaced by a restore, and its store is from before it. It refuses every node that has seen the newer copy, and its own replicas and router run on a stale view of the cluster. Stop this server and disable its service (docs/backup.md)" node=node-95fe5bc3b6e3 node_epoch=1 epoch=0 remote=10.0.0.7:41822
```

[`set-server`](#at-a-new-ip-address) refuses it as well. Stop it and disable its unit when you see either line. What the epoch does not cover:

- A worker that has had no reply from the restored server has not seen its epoch, and takes the old server: one that dials the old IP address and has not been moved yet, or one whose resolver still has the old record when both servers run.
- A server older than this build sends no epoch, and is taken as before.
- The old server itself. It learns it is stale only from a worker that has seen the newer epoch. On its own, with every worker refusing it, its start grace ends with no report: it keeps the workers `waiting`, places none of their replicas, and logs at error that no worker has reported since it started ([above](#replacing-a-lost-server)). Its own node still runs the replicas its store places there, and its router serves them, and the workers' replicas at the addresses it last had, to any client that DNS sends to it. A worker that has not seen the restored server's epoch and reports to it ends that hold; the old server then marks the other workers lost when the start grace has passed again, and starts their replicas on its own node.
- Two restores of the same backup are at the same epoch, and the workers take either. Stop the first before you restore the backup again elsewhere.
- Restoring, on purpose, a backup taken before an earlier restore can give a lower epoch than the workers have seen, as each restore adds one to its backup's. They then refuse the server you mean to use. Stop each worker, delete `cluster-epoch` from its data directory, and start it again.

`scripts/integration-newaddr.sh` starts the old server's container again after each restore, with its data directory and its old store. By name, with the restored server stopped and the alias moved back to the old one, both workers refuse it and log that once, for 20 s their containers stay as they were and the old server has no report from them; past its start grace, 15 s in the test, the old server keeps both waiting, marks neither lost, places none of their replicas again, and logs that no worker has reported since it started; a worker's router serves every request, and once the alias moves to the restored server, with the old one still running, the workers, never restarted, report to it within 30 s, and log no second refusal. By IP, `set-server` to the old server is refused. The fault suite brings each old server back by name after each of its two restores, the second one at epoch 2, and checks the refusals the same way.

### At a new IP address

A worker that joined an IP address keeps dialing it, and the new server's certificate does not name it. Start the server at its own address, `sudo hivemind serve --advertise 10.0.0.9`. It logs a warning that the workers which joined the old address need `hivemind node set-server 10.0.0.9`, with the count of workers in the store, and names in `dial_old_address` each one that registered the old address as the one it dials. `hivemind nodes --json` shows what each worker dials, as `dials`; a worker of an earlier build sends none. Then, on each worker:

```sh
sudo systemctl stop hivemind
sudo hivemind node set-server 10.0.0.9
sudo systemctl start hivemind
```

The worker's containers keep running while it is stopped, and it picks them up again when it starts. `set-server` changes the server address in `node.json`, and `cluster-epoch` when the server is at a higher epoch than the worker has seen: the node keeps its ID, key and certificate, and does not join again. It refuses to run beside the worker, which reads `node.json` only when it starts.

Before it writes `node.json`, `set-server` checks from the worker that the address is its own cluster's server, by the same trust that every connection uses:

- the server's certificate must chain to the cluster CA in the worker's data directory, and name the address given, and a node;
- the server must accept the worker's certificate, and answer a node API request for the worker's node ID. A server that says the node was removed is refused;
- the server must not be from before a restore: its epoch must not be lower than the highest the worker has seen ([above](#if-the-old-server-comes-back)).

Anything else is refused with what was found, and `node.json` is left as it was:

```
error: the server at 10.0.0.9:4484 presents a certificate from the CA with fingerprint 5d1e..., not from this worker's cluster CA, fingerprint 3f9c...: it is another cluster's server, or something between this machine and 10.0.0.9 holds another CA. /var/lib/hivemind/node.json is unchanged
error: the server at 10.0.0.7:4484 has a certificate from this cluster's CA, but it names 10.0.0.9, not 10.0.0.7: give set-server the address the server advertises, or start the server with --advertise 10.0.0.7. /var/lib/hivemind/node.json is unchanged
error: the server at 10.0.0.5:4484 is from before a restore of this cluster: it is at epoch 0, and this worker has seen epoch 1 from the restored server. Stop the old server, and give set-server the address of the restored one. /var/lib/hivemind/node.json is unchanged
error: cannot reach a server at 10.0.0.8:4484: connect: connection refused. /var/lib/hivemind/node.json is unchanged
error: 10.0.0.9:4485 is the default join port, but set-server takes the server's cluster port, 10.0.0.9:4484, or no port. /var/lib/hivemind/node.json is unchanged
error: https://10.0.0.9 is a URL: give the address without https://: 10.0.0.9. /var/lib/hivemind/node.json is unchanged
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
| `ca-next.key`, `ca-next.crt` | Only during a [CA rotation](ca-rotation.md): the new CA's key and certificate, PEM. A build without CA rotation refuses a backup with them |

`backup` copies the store first and reads the CA files after it, and checks that they are the CA files of the rotation the copied store records, or of none. A rotation step that ran in between, from another shell, makes it fail and ask to run it again; `hivemind backup --api` and the steps run one after the other in the server, never at once.

Every entry is a regular file with mode 0600. `restore` refuses any other entry, and any file the manifest does not list. The optional settings file, `hivemind.yaml`, is not in a backup; keep it with your configuration.

## The CA

The cluster CA lasts ten years from `init`. In the CA's last year, node certificates are issued to end when the CA does, and the server logs a warning once a day. When the CA expires, no node certificate verifies, and no worker can reach the server. Before then, rotate it: `hivemind ca rotate start`, `renew` and `finish` ([CA rotation](ca-rotation.md)). A backup carries the CA it was taken with, so restoring one does not extend it.

Take a backup after a rotation's `start`, so that a restore has the new CA's key, and after its `finish`, since backups from before it hold the old CA's key and name the old CA as the cluster's. After an `abort`, and after a `finish` that ends a rotation an abort turned round, the cluster is back on the CA it had: the backups taken during the rotation hold the new CA's key, which it no longer uses, and those from before `start` are still good. What a restore of each brings back is in [CA rotation](ca-rotation.md#backup-and-restore). A restore raises the epoch in any phase of a rotation, as it always does; the rotation does not change the epoch.
