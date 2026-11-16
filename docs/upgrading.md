# Upgrading

## From 2.0.0

These steps were run on two clusters of three machines (c7g.large, Amazon Linux 2023, Docker 25.0.18), each on 2.0.0 under the [systemd units](systemd.md), with apps that had a volume, a host port, a health check and no port. One cluster was upgraded server first, the other workers first. In both, no container on any node was restarted or moved by the upgrade, every worker stayed `ready`, and the old certificates kept working.

1. Back up the server. 2.0.0 has no `backup` command, so use the new binary's, which only reads the data directory and works while the 2.0.0 server runs: `sudo ./hivemind-linux-arm64 backup /root/hivemind-2.0.0.tar`. This file is the only way back to 2.0.0.
2. Upgrade the server: install the new binary over the old one, as in the [README](../README.md#install), and run `sudo systemctl restart hivemind`. The server migrates the store on that start, from schema version 6, and logs nothing about it; `hivemind backup` prints the schema version.
3. Upgrade each worker the same way: install, then `sudo systemctl restart hivemind`. Upgrade it in place. Do not drain a 2.0.0 worker first: its agent stops containers one after another without renewing its lease, and with a few containers that each take 10 s to stop, the server marks it lost and places its replicas again.

A 2.0.0 worker keeps working with a newer server: `status`, `nodes`, `deploy` and `scale` work. Its replicas' logs get `worker node-... did not answer within 10s`, it takes no routed replicas, and it cannot renew its certificate. Upgrade every 2.0.0 worker within 365 days of its join. Once two thirds of its certificate's life have passed, an upgraded worker renews at its next start or hourly check, using the certificate 2.0.0 issued. A worker whose certificate has expired cannot renew: the server marks it lost and places its replicas again, while its old containers keep running on the machine. To bring it back, run `hivemind node remove <id>` on the server, then join the machine again with an empty data directory and a new join token; the join removes the old containers. `hivemind nodes` shows each node's CERT EXPIRES, and marks a 2.0.0 worker's `not renewing` from day 244, and the server logs a warning for it once a day.

Upgrading the workers first also works. A new worker with a 2.0.0 server logs `polling the server for log requests; retrying ... this node is not registered with the server: 404 Not Found` every 30 s; the node is registered, and the warning stops once the server is upgraded. It registers no address with a 2.0.0 server, and keeps none after the server is upgraded, so restart each worker once more after the server. Until then it takes no routed replicas, and `hivemind nodes --json` shows no `address` for it.

A 2.0.0 binary refuses a migrated store, and leaves its files unchanged:

```
error: opening /var/lib/hivemind/hivemind.db: store schema is newer than this build: the store has schema version 9 and this build knows up to 6; use a newer hivemind
```

To go back to 2.0.0, restore the backup from step 1 into an empty data directory with the new binary's `hivemind restore`, then run 2.0.0 on it. Everything that changed since the backup is lost.

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
| `hivemind --runtime containerd` | Not available. containerd is planned as a second runtime |
| (no init step) | `hivemind init` once, before `serve` |
| `--data-dir`, `HIVEMIND_DATA_DIR` | The same names. The directory layout is new |
| (none) | `hivemind deploy`, `status`, `logs`, `scale`, `delete` |
| (none) | `hivemind serve --advertise` opens the node listeners. `hivemind join-token` and `hivemind join` add a worker |

`join` runs a worker's agent in the foreground. See the [README](../README.md#add-a-machine) for the steps.

### API and web UI

v1 served a web UI and an API on all interfaces, port 4483, with no authentication. Its routes were under `/api/`. The web UI is removed.

v2 serves the API on `127.0.0.1:4483`, and every route except `GET /healthz` needs the admin token from `init`. The routes are under `/v1/`. See [api.md](api.md). Some v1 calls have no direct equivalent: a deploy is a `PUT` to `/v1/apps/{name}`, and a scale is a `POST` to `/v1/apps/{name}/scale`.

### Networking

v1 bound `*:7946` for its membership protocol (memberlist). v2 has no membership protocol. Its API listens on `127.0.0.1:4483`, and nothing else listens unless the server runs with `--advertise`. With `--advertise`, v2 opens two ports on that address only: 4484 for the node API and 4485 for joins. See [security](security.md#listeners).

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
