# Upgrading

## From 2.2.0 to 2.3.0

A server upgraded to 2.3.0 changes nothing until a second server joins: it runs no Raft, opens no new listener, and writes go to the store as before. Adding servers is a separate step, after the upgrade ([below](#adding-servers)).

No test or soak has upgraded a 2.2.0 cluster in place. The [eighth soak](soak.md#eighth-soak-three-servers) made its cluster of three servers with this release from the start, and `scripts/integration-mixed.sh` runs 2.0.0 binaries beside this release, not 2.2.0 ones.

1. Back up the server with the 2.2.0 binary: `sudo hivemind backup /root/hivemind-2.2.0.tar`. This file is the only way back to 2.2.0.
2. Upgrade the server: install the new binary over the old one, as in the [README](../README.md#install), and run `sudo systemctl restart hivemind`. The server migrates the store on that start, from schema version 17 to 18, and logs `msg="store schema migrated" from=17 to=18 adds="several servers"`. Version 18 adds the Raft applied index, the server table and a role for join tokens: tables and a column, nothing changed in what is there.
3. Upgrade each worker the same way, one at a time; with routed apps, wait for its routed replicas to be ready before the next ([routed apps](#routed-apps)).

The 2.3.0 CLI works with a 2.2.0 server for the commands 2.2.0 has. `hivemind servers` and `hivemind server remove` say `the server runs v2.2.0, which has no servers command; upgrade the server` (or `server remove`).

### What 2.3.0 changes on disk

In a cluster of one, only `hivemind.db` changes, by the migration. A worker's files stay as 2.2.0 wrote them.

Once a second server joins, every server has `raft/`, with the Raft log in `raft/raft.db` and its snapshots, and each server that joined has `server.json` and the CA files it received ([security](security.md#files)). A worker of 2.3.0 adds `servers`, the list of the servers' node listeners, to its `node.json`. A 2.2.0 binary refuses that file (`node.json is not valid: json: unknown field "servers"`).

### Workers of 2.2.0

A 2.2.0 worker keeps working with 2.3.0 servers: it uses the server it joined, which passes its requests to the leader. It cannot move to another server: when its server is down, it keeps its containers and waits, as with one server. An upgraded worker learns the servers from the next reply of any server and moves to the next one in its list when one does not answer or has no leader, so upgrade the workers before you rely on that.

### Adding servers

Every server runs the same release: a server will not apply a Raft entry from a leader at another schema version, and a joining server must run the leader's version. So upgrade the first server before you add any: a 2.2.0 server refuses `join-token --role server`, which the 2.3.0 CLI prints as `error: invalid JSON: json: unknown field "server"`, and takes no server joins.

1. Open TCP port 4486 between the server machines, and from nowhere else, and ports 4484 and 4485 from every node to every server ([security](security.md#several-servers)). On EC2, that is a security group rule for TCP 4486 from the servers' own group.
2. Finish any CA rotation: a server join is refused while one is in progress, and in 2.3.0 `hivemind ca rotate start` is refused while the cluster has more than one server ([compatibility](ha.md#compatibility)).
3. Join two more servers, one after the other, as in [membership](ha.md#membership): `hivemind join-token --role server` on the first server, then `hivemind join <first server> --role server --token <token> --advertise <its own address>` on each new machine. `hivemind servers` lists them, and shows each as a `nonvoter` until it has caught up with the log.
4. Give the CLI every server: `HIVEMIND_SERVER=https://10.0.0.5:4483,https://10.0.0.6:4483,https://10.0.0.7:4483`. Each server's API listens on `127.0.0.1:4483` unless it is given another `--api-addr`, which needs TLS ([the API](api.md#tls)): a certificate for each server that names the address in its URL, and a `--ca-cert` file for the CLI that holds the CA that signed them, or every server's certificate. `join` takes no API flags, so give them to each server in its unit's `ExecStart` or in its `<data-dir>/hivemind.yaml` (`api-addr:`, `api-tls-cert:`, `api-tls-key:`), and restart it. A joined server has no `admin.token`, so give its CLI the cluster's admin token with `HIVEMIND_TOKEN`. The client config that `init` wrote on the first server still names `http://127.0.0.1:4483`, so once its API has moved, `sudo hivemind status` there fails with `cannot reach hivemind at http://127.0.0.1:4483: connection refused`: set `server:` in `/root/.config/hivemind/config.yaml` to the API's new URL, and add `ca-cert:` with the CA file. On a server that joined, give the CLI `--server` and `--ca-cert` as well as the token.

### Going back to 2.2.0

From a cluster of one, restore the backup from step 1 into an empty data directory with the 2.2.0 binary's `hivemind restore`, run 2.2.0 on it, and install 2.2.0 on the workers again. Everything that changed since the backup is lost. A 2.2.0 binary refuses the migrated store:

```
error: opening /var/lib/hivemind/hivemind.db: store schema is newer than this build: the store has schema version 18 and this build knows up to 17; use a newer hivemind
```

There is no tested way back to 2.2.0 once a second server has joined.

## From 2.0.0 to 2.3.0

A 2.0.0 cluster upgrades to 2.3.0 the way the next section upgrades it to 2.2.0, with the 2.3.0 binary: the server migrates the store from schema version 6 to 18 on its first start, and a 2.0.0 binary then refuses it with schema version 18 in the message. `scripts/integration-mixed.sh` runs that upgrade, of the server and of a 2.0.0 worker, with this release's binary. Then add servers as [above](#adding-servers).

## From 2.0.0 to 2.2.0

These steps were run on two clusters of three machines (c7g.large, Amazon Linux 2023, Docker 25.0.18), each on 2.0.0 under the [systemd units](systemd.md), with apps that had a volume, a host port, a health check and no port. One cluster was upgraded server first, the other workers first. In both, no container on any node was restarted or moved by the upgrade, every worker stayed `ready`, and the old certificates kept working.

1. Back up the server. 2.0.0 has no `backup` command, so use the new binary's, which only reads the data directory and works while the 2.0.0 server runs: `sudo ./hivemind-linux-arm64 backup /root/hivemind-2.0.0.tar`. This file is the only way back to 2.0.0.
2. Upgrade the server: install the new binary over the old one, as in the [README](../README.md#install), and run `sudo systemctl restart hivemind`. The server migrates the store on that start, from schema version 6 to 17, and logs one line for each step, such as `msg="store schema migrated" from=6 to=7 adds="node certificate expiry"`; `hivemind backup` prints the schema version.
3. Upgrade each worker the same way: install, then `sudo systemctl restart hivemind`. Upgrade it in place. Do not drain a 2.0.0 worker first: its agent stops containers one after another without renewing its lease, and with a few containers that each take 10 s to stop, the server marks it lost and places its replicas again.
4. Before you deploy a routed app, open TCP ports 30000 to 32767 between the nodes, and give each node that should run a router its flags ([below](#routers-and-the-firewall)).

The workers can be upgraded first instead ([below](#upgrading-the-workers-first)).

A 2.2.0 CLI works with a 2.0.0 server for the commands 2.0.0 has. `hivemind nodes` shows `-` for what 2.0.0 does not report, such as ELIGIBILITY. `hivemind status` shows `-` under READY, because 2.0.0 sends no ready count. A command whose route 2.0.0 lacks says so: `the server is older than 2.2 and has no node drain; upgrade the server`.

`hivemind nodes` has a VERSION column: each node from 2.2 on reports its version with every request to the server, and the server's node shows the server's. A node that still runs 2.0.0 shows `-`. `hivemind version` prints the server's version.

### What changes on disk

On the server, `serve` migrates `hivemind.db` in place, in one transaction, and adds `hivemind.lock`, which holds its pid while it runs. Nothing else in the data directory changes. It writes no `hivemind.yaml`: in 2.2.0 `init` writes the serve flags it is given to that file, but a 2.0.0 cluster was made without one, so its flags stay in the unit's `ExecStart`.

On a worker, the upgrade adds `hivemind.lock` and changes nothing else at its start: `node.json`, `node.key`, `node.crt` and `ca.crt` stay as `join` wrote them, and the worker keeps its node ID. `node.key` and `node.crt` are replaced when the worker renews its certificate, from day 243, and `node.crt` is mode 0600 after that; `ca.crt` changes only in a [CA rotation](ca-rotation.md). In 2.2.0, `join` writes the worker flags it is given, such as the router's, to `<data-dir>/hivemind.yaml`, and `serve --role worker` reads them from there. A worker that joined under 2.0.0 has no such file, and the upgrade does not write one. [Security](security.md#files) lists every file.

### Routers and the firewall

The upgrade starts no router. To run one on a node, add `--route-addr` and `--acme-email` to its unit's `ExecStart`, after `--advertise` on the server or `--role worker` on a worker, and restart it:

```ini
ExecStart=/usr/local/bin/hivemind serve --role worker --route-addr 0.0.0.0 --acme-email ops@example.com
```

Or write `route-addr: "0.0.0.0"` and `acme-email: "ops@example.com"` to `<data-dir>/hivemind.yaml`; a flag in `ExecStart` overrides the file. `--acme-ca` takes another ACME CA than Let's Encrypt, and `--route-http-port` (80), `--route-https-port` (443) and `--route-max-conns-per-ip` (256) keep their defaults unless given. `hivemind nodes` shows `yes` under ROUTER for each node whose router runs. See [routing](routing.md#example).

A node with `--route-addr` listens on ports 80 and 443 of that address, for the routed apps' clients, and on port 80 for the ACME CA's HTTP-01 requests. A routed replica publishes its route ports on ports 30000 to 32767 of its node's private address, and every node's router must reach them on every other node. A 2.0.0 cluster needed only ports 4484 and 4485 open to the server, so open TCP 30000 to 32767 between all the nodes before you deploy a routed app; on EC2, that is a security group rule for the range from the nodes' own group. Without it, a router reaches only the replicas on its own node: its dials to the other nodes time out after 2 seconds, and a router on a node with no replica of the host answers `502`. Keep the range closed to anything but the nodes ([ports and firewall](routing.md#ports-and-firewall)).

### A 2.0.0 worker beside a 2.2.0 server

A 2.0.0 worker keeps working with a 2.2.0 server until it is upgraded: it keeps its lease, runs its replicas and takes new ones, and `status`, `nodes`, `deploy` and `scale` cover it. It cannot:

- stream logs: `hivemind logs` of its replicas gets `worker node-... runs a hivemind older than 2.1, which cannot stream logs; upgrade it`;
- take routed replicas: it reports no private address, so placement passes it over for an app with routes;
- pull with a credential from `registry login`: it does not ask for one, so a private image fails to pull there ([registries](registries.md#workers-older-than-22));
- follow a CA rotation: `ca rotate renew` refuses until it is given `--force`, `finish` refuses while the worker is in the cluster, and `finish --force` cuts it off ([CA rotation](ca-rotation.md#workers-of-an-earlier-build));
- exit with status 3 when it is removed: it exits with 1, and its unit restarts it until it is disabled ([systemd](systemd.md#what-the-settings-do));
- be drained without risk: see step 3;
- renew its certificate.

Upgrade every 2.0.0 worker within 365 days of its join. Once two thirds of its certificate's life have passed, an upgraded worker renews at its next start or hourly check, using the certificate 2.0.0 issued. A worker whose certificate has expired cannot renew: the server marks it lost and places its replicas again, while its old containers keep running on the machine. To bring it back, run `hivemind node remove <id>` on the server, then join the machine again with an empty data directory and a new join token; the join removes the old containers. `hivemind nodes` shows each node's CERT EXPIRES, and marks a 2.0.0 worker's `not renewing` from day 244, and the server logs a warning for it once a day.

### Upgrading the workers first

Upgrading the workers first also works. A 2.2.0 worker with a 2.0.0 server logs one warning for each feature the server lacks, such as `the server is older than this worker and has no log streaming; upgrade the server`, and the same for lease renewal between status reports, certificate renewal and, with `--route-addr`, the routing table. It does not call those routes again until the server's version changes, or for 10 minutes. The node is registered: do not join it again. A 2.0.0 server refuses the worker's address, so the worker registers without one and takes no routed replicas; `hivemind nodes` shows `-` for its ADDRESS. Once the server is upgraded, the worker sees the server's new version in its next reply and registers its address, without a restart, and logs `the server took this node's address; it takes routed replicas now`.

`scripts/integration-mixed.sh` runs these mixed states in Docker: a 2.0.0 server with a worker of this build and a 2.0.0 worker, then the server upgraded in place, and checks the messages above. It then rotates the cluster CA with the 2.0.0 worker in the cluster, and upgrades that worker in place in the middle of the rotation ([CA rotation](ca-rotation.md#workers-of-an-earlier-build)).

### Going back to 2.0.0

A 2.0.0 binary refuses a migrated store, and leaves its files unchanged:

```
error: opening /var/lib/hivemind/hivemind.db: store schema is newer than this build: the store has schema version 17 and this build knows up to 6; use a newer hivemind
```

To go back to 2.0.0, restore the backup from step 1 into an empty data directory with the new binary's `hivemind restore`, then run 2.0.0 on it. `restore` leaves a 2.0.0 store at its schema version, and writes the cluster's epoch to `restored-epoch` beside it ([backup](backup.md#restoring)), which 2.0.0 ignores. Everything that changed since the backup is lost.

## Routed apps

A node's agent that restarts keeps each container that kept running and that its last report called healthy as healthy and ready, so its routed replicas stay in the routing table ([concepts](concepts.md#health-checks-and-restarts)). The node's own router refuses connections while its service restarts. In the [third soak](soak.md#upgrade) the server and then each worker were restarted on a new build, each once the last one's routed replicas were ready: four restarts in 6.6 s, one refused request on each stream through two of the workers' routers during their own restarts, and no `503`. A build before that (the [second soak](soak.md#upgrade-and-restarts)) dropped a restarting node's routed replicas from the table until each passed its first check again, 4 seconds with a 2-second check interval, and with every node restarted within a second every routed host answered `503` for 3 to 4 seconds. Restart one node at a time, and wait until `hivemind status` shows its routed replicas ready before the next.

## From v1

Version 2 is a new implementation, not an update to v1. The command line, the API, the data and the feature set all differ. Plan for a fresh start on a new machine or data directory.

### Data

v1 data is not migrated. In v1.0.2, `deploy` wrote a record to the database and never started a container (see [ASSESSMENT.md](ASSESSMENT.md)), so there is no container state to carry over.

Use a new data directory for v2: point `--data-dir` or `HIVEMIND_DATA_DIR` at an empty directory, then run `hivemind init`.

`hivemind init` and `hivemind serve` refuse a v1 data directory. A directory counts as v1 when it holds `app.db` or `storage.db` with v1 tables in it. The error names the file and points here. The check only reads the directory, so a refused command leaves it unchanged. v2 never creates those two file names; its store is `hivemind.db`.

The v2 store has a schema version. A store written by a newer build is refused with an error that says so. Each release from v2.0.0 on adds its migrations after the existing ones, and a store from an earlier release is migrated forward on `serve` and `init` with its data kept.

A v2 data directory made by a development build from before the cluster CA was added has no CA. `serve --advertise` refuses it, and `init` will not run again on it because the cluster exists. Use a new data directory.

### Command line

v1 was one command with flags, such as `hivemind --runtime docker`. v2 has subcommands. Each one has its own flags, and the full list is in [cli.md](cli.md).

| v1 | v2 |
|---|---|
| `hivemind --runtime docker` | `hivemind serve`. Docker is the only runtime |
| `hivemind --runtime containerd` | Not available |
| (no init step) | `hivemind init` once, before `serve` |
| `--data-dir`, `HIVEMIND_DATA_DIR` | The same names. The directory layout is new |
| (none) | `hivemind deploy`, `status`, `logs`, `scale`, `delete` |
| (none) | `hivemind serve --advertise` opens the node listeners. `hivemind join-token` and `hivemind join` add a worker |

`join` runs a worker's agent in the foreground. See the [README](../README.md#add-a-machine) for the steps.

### API and web UI

v1 served a web UI and an API on all interfaces, port 4483, with no authentication. Its routes were under `/api/`. The web UI is removed.

v2 serves the API on `127.0.0.1:4483`, and every route except `GET /healthz` needs the admin token from `init`. The routes are under `/v1/`. See [api.md](api.md). Some v1 calls have no direct equivalent: a deploy is a `PUT` to `/v1/apps/{name}`, and a scale is a `POST` to `/v1/apps/{name}/scale`.

### Networking

v1 bound `*:7946` for its membership protocol (memberlist). v2 has no membership protocol. Its API listens on `127.0.0.1:4483`, and nothing else listens unless the server runs with `--advertise`, or a node with `--route-addr`. With `--advertise`, v2 opens two ports on that address only: 4484 for the node API and 4485 for joins. `--route-addr` opens the HTTPS router on ports 80 and 443. See [security](security.md#listeners).

### Removed

These are not in v2, following decision 3 in [PLAN.md](PLAN.md):

- Security: RBAC, secrets, network policy, IPAM, tenants, image scanning, storage encryption, ACLs and QoS.
- Resilience: circuit breaker, Redis storage, bulkhead, retry and fallback.
- Service discovery: the DNS server and the proxy.
- Tenants.
- Zero-downtime, canary and A/B deployment endpoints.
- Volume backup and snapshots.
- The containerd runtime and the memberlist membership.

### Platforms and builds

The node runs on Linux, amd64 and arm64. The darwin and windows builds are the client CLI.

Building v2 needs Go 1.26 or later. Docker Engine 25 or newer is needed at run time.
