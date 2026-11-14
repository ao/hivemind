# Upgrading from v1

Version 2 is a new implementation, not an update to v1. The command line, the API, the data and the feature set all differ. Plan for a fresh start on a new machine or data directory.

## Data

v1 data is not migrated. In v1.0.2, `deploy` wrote a record to the database and never started a container (see [ASSESSMENT.md](ASSESSMENT.md)), so there is no container state to carry over.

Use a new data directory for v2: point `--data-dir` or `HIVEMIND_DATA_DIR` at an empty directory, then run `hivemind init`.

`hivemind init` and `hivemind serve` refuse a v1 data directory. A directory counts as v1 when it holds `app.db` or `storage.db` with v1 tables in it. The error names the file and points here. The check only reads the directory, so a refused command leaves it unchanged. v2 never creates those two file names; its store is `hivemind.db`.

The v2 store has a schema version. A store written by a newer build is refused with an error that says so. Each release from v2.0.0 on adds its migrations after the existing ones, and a store from an earlier release is migrated forward on `serve` and `init` with its data kept.

A v2 data directory made by a development build from before the cluster CA was added has no CA. `serve --advertise` refuses it, and `init` will not run again on it because the cluster exists. Use a new data directory.

## Command line

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

## API and web UI

v1 served a web UI and an API on all interfaces, port 4483, with no authentication. Its routes were under `/api/`. The web UI is removed.

v2 serves the API on `127.0.0.1:4483`, and every route except `GET /healthz` needs the admin token from `init`. The routes are under `/v1/`. See [api.md](api.md). Some v1 calls have no direct equivalent: a deploy is a `PUT` to `/v1/apps/{name}`, and a scale is a `POST` to `/v1/apps/{name}/scale`.

## Networking

v1 bound `*:7946` for its membership protocol (memberlist). v2 has no membership protocol. Its API listens on `127.0.0.1:4483`, and nothing else listens unless the server runs with `--advertise`. With `--advertise`, v2 opens two ports on that address only: 4484 for the node API and 4485 for joins. See [security](security.md#listeners).

## Removed

These are not in v2, following decision 3 in [PLAN.md](PLAN.md):

- Security: RBAC, secrets, network policy, IPAM, tenants, image scanning, storage encryption, ACLs and QoS.
- Resilience: circuit breaker, Redis storage, bulkhead, retry and fallback.
- Service discovery: the DNS server and the proxy.
- Tenants.
- Zero-downtime, canary and A/B deployment endpoints.
- Volume backup and snapshots.
- The containerd runtime and the memberlist membership.

## Platforms and builds

The node runs on Linux, amd64 and arm64. The darwin and windows builds are the client CLI.

Building v2 needs Go 1.26 or later. Docker Engine 25 or newer is needed at run time.
