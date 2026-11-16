# Changelog

Notable changes to hivemind, newest first. Versions follow [Semantic Versioning](https://semver.org). A release is cut by pushing a `v*` tag; its notes are the matching section below.

## [2.2.0] - Unreleased

### Added

- `hivemind compose convert`, `deploy` and `delete` read a Docker Compose file and make one app of each service. The CLI does the conversion; the API is unchanged. The keys read are `image`, `command`, `environment`, `ports`, named `volumes`, `deploy` (`replicas`, `resources` and `restart_policy`), `healthcheck` and `restart`, with anchors, merge keys, `x-` extensions and `.env` interpolation. Any other key is refused with its path and a reason, and every refusal is listed in one error. A port with no address is published on `127.0.0.1`, where Compose publishes on every interface, and a note on standard error says so for each such port. An unset variable with no default is an error rather than an empty string. Apps do not find each other by service name. `deploy` validates every app before it sends any, and `--prefix` names the apps `<prefix>-<service>`. See [docs/compose.md](docs/compose.md).
- `hivemind node eligibility <id> --disable|--enable`: an ineligible node takes no new replicas, from placement or from a rollout's surge, and keeps the ones it runs. The server stores the flag, so it holds across restarts. `POST /v1/nodes/{id}/eligibility` is the route.
- `hivemind node drain <id> [--deadline 10m] [--no-wait]`: makes the node ineligible, then moves each replica off it with a rollout surge at the replica's own generation, one replica per app at a time, so an app keeps its replica count running while another node has room. A replica with a volume, or one whose host port is taken on every other eligible node, stays, and the drain says why. At the deadline, what is left is stopped and placed again, as on node loss; replicas with a volume keep running. The command waits and prints each move. A drain carries on across a restart of the server. `POST` and `GET /v1/nodes/{id}/drain` are the routes. See [docs/concepts.md](docs/concepts.md#draining-a-node).
- `hivemind nodes` has an ELIGIBILITY column (`eligible`, `ineligible`, `draining` or `drained`), and `GET /v1/nodes` the fields `eligible` and `drain`. Schema version 8 adds the eligibility column and the drain tables; a 2.1 store is migrated when 2.2 opens it.
- HTTPS routing. An app spec can list `routes`, each an exact host and a container port. `serve` and `join` take `--route-addr`, which runs a router on that node on `--route-http-port` (80) and `--route-https-port` (443), with certificates from an ACME CA (`--acme-ca`, Let's Encrypt by default; `--acme-email` is required) for the routed hosts only, over HTTP-01, through certmagic. Any node can run one, and they share certificates, keys, the ACME account and challenge tokens through the server's database, with a lock so that one router issues each certificate. Requests go round-robin to the Ready replicas on any node; a replica that cannot be dialled is skipped for 3 seconds. Port 80 redirects to https with 308. A host belongs to one app: a deploy that claims another app's host gets `409`. See [docs/routing.md](docs/routing.md).
- Routed apps publish their route ports on ephemeral host ports of the node's private address, so several replicas of one routed app can share a node, and a rolling update of a routed app starts each new copy beside the old one, on the same node if there is no other. The old copy leaves the routing table when the new one is promoted, and runs 5 more seconds before it is stopped. In the integration test, no request of 4,734 failed during a rolling update through three routers.
- A worker registers the local address of its connection to the server as its private address. `GET /v1/nodes` has `address`, and `GET /v1/routes` shows the routing table. `/metrics` has `hivemind_router_requests_total`, `hivemind_router_upstream_errors_total` and `hivemind_router_cert_expiry_timestamp_seconds` for the server's router. Schema version 9 adds the node address, the reported route ports, the route hosts and the TLS storage; `hivemind backup` includes the ACME state.
- `scripts/integration-routing.sh` runs three nodes with routers against Pebble, and CI runs it. Two new fuzz targets cover the node API's routing and TLS storage messages.
- Private registries. `hivemind registry login <host> --username <name> --password-stdin` (or `--password-file`) stores a credential for a registry, `hivemind registry list` shows each host, username and update time, and `hivemind registry logout <host>` removes one. There is no flag that takes the password on the command line. A node sends the credential to its Docker daemon in `X-Registry-Auth` when it pulls an image from that registry. Docker Hub is `docker.io`, and `index.docker.io` and `registry-1.docker.io` are the same host. The routes are `PUT`, `GET` and `DELETE /v1/registries`, and none returns a password. Credentials are stored in plain text in the server's database, like environment values, are in backups, and reach only the nodes that run an image from that registry. A pull the registry refuses leaves the replica `pending` with a status that names the registry and says to run `hivemind registry login <host>`. ECR tokens last 12 hours and hivemind has no credential helper: run the login again from cron. Schema version 10 adds the `registries` table. See [docs/registries.md](docs/registries.md).
- `scripts/integration-docker.sh` runs `registry:2` with htpasswd and TLS from a CA made for the run, and the integration test pulls from it with and without a login, after a logout, and with a wrong password.

### Changed

- After `serve` starts, a replica whose last report was made to an earlier server process is shown as `unknown`, not as the state it had then, until its node reports again; after a restore on a new machine those containers no longer exist. A node that has not reported since the start is `waiting` for 15 s rather than `lost`, as the sweep does not release it before then.
- Routing reverses part of the v2 scope (decision 2 in [PLAN.md](docs/PLAN.md)), which had host-port networking and no proxy. Host ports work as before, and an app without routes is unchanged, but hivemind now runs a reverse proxy when `--route-addr` is given.
- The router is the one listener that may bind `0.0.0.0`. It has no authentication, because it serves the apps' public traffic. `--route-addr` has no default. Every other listener still refuses a wildcard.
- A worker can read every routed host's TLS private key and the ACME account key over the node API, whether or not it runs a router. [docs/security.md](docs/security.md#what-a-compromised-worker-can-do) lists what that means for a compromised worker.
- Workers older than 2.2 take no routed replicas, because they report no address. A rollout that adds routes to an app moves its replicas off such workers, rather than in place. A 2.2 worker whose server is older registers without its address, as that server's decoder refuses the field. A worker's router needs a 2.2 server.

- A failed image pull backs off: 10 seconds, doubling to 5 minutes, per image and credential. Before, every reconcile pass pulled again. A login or logout changes the credential, so it is tried at the next pass.
- Workers older than 2.2 get no registry credentials. Their decoder refuses unknown fields, so a credential in one assignment would stop them reconciling every replica on the node; a 2.2 worker asks for credentials, and only then gets them. A private image placed on an older worker fails to pull there, with Docker's message.
- An image reference on `index.docker.io` is normalised to `docker.io`, as Docker reads it, so `index.docker.io/nginx` is `docker.io/library/nginx:latest`.
- `hivemind node remove` of a node that is ineligible and runs nothing, as after a drain, needs no `--force`, though the node still renews its lease.
- `serve`, `init` and `join` no longer accept `--server`, and `serve` and `init` no longer accept `--token`. They never used them. `logs` no longer accepts `--json`, which it ignored. Passing one of these flags is now an error. `join --token` is unchanged.

### Fixed

- A host port bound to a specific address (not loopback or a wildcard) is placed only on the node that has that address: its reported `address`, or the server's `--advertise` address. Before, such a replica could land on any node, where every pass created a container whose bind failed and removed it again. With no such node it is `pending`, and the reason names the address. The sweep places again a replica already on the wrong node whose container is not running; one with a volume stays with its data, and its reason says how to run it. A rollout to another node's address moves the replicas there, or pauses for a replica with a volume. A server without `--advertise` takes any address, as it has no workers. A routed replica with a volume on a node without an address now pauses its rollout, rather than being placed again away from its data. In `hivemind status`, `DETAIL` shows the server's reason before the engine's last error.
- A node lost during its drain: the replicas the loss placed again were counted nowhere, so the drain reported `0 stopped, 0 left` for a replica that node loss had stopped. The result now counts them (`lost` and `lost_at` in the drain object, "N placed again after the node was lost" from `node drain`), and the command and the server's log say the node was lost. Schema version 11 adds the two columns.
- A node that does not renew its certificate, as a 2.0.0 worker never does, went unnoticed until the certificate expired and the node stopped working. A day after its renewal time (two thirds of its lifetime: day 244 of 365), `hivemind nodes` marks it, as in `2027-11-16 (in 120d, not renewing)`, `GET /v1/nodes` has `cert_not_renewing`, and the server logs a warning for that node once a day. The server records each certificate's issue time from the certificate, where it recorded the time it first saw it.
- `hivemind status` counted an unhealthy, crash-looping replica in `RUNNING`, as its container was running. `RUNNING` still counts running containers, and a new `READY` column, `ready` in the app status, counts those that pass their health check, or have none, and are not crash-looping. The docs said `RUNNING` counted only the current generation; it counts each replica at its own generation, so an old copy counts during a rollout, and the docs now say so.
- `hivemind compose deploy` replaced an unrelated app that had a service's name, without a warning. Each app now records the project that deployed it (the file's top-level `name`, or its directory's name, as Docker Compose takes it), and `compose deploy` refuses an app that `hivemind deploy`, the API or another project deployed, unless `--replace`. Deploying the same file again needs no flag. `PUT /v1/apps/{name}` takes `origin` and `replace` in the query, and `AppView` has `origin`. Apps that `compose deploy` made before this have no origin, so the first `compose deploy` after the upgrade needs `--replace`. Schema version 12 adds the column.
- `hivemind backup` into a directory that does not exist failed with `open /var/backups/.hivemind-backup-723498941.tar: no such file or directory`, naming a temporary file. It now says `the directory /var/backups does not exist; create it first (mkdir -p /var/backups)`, before it reads anything, and says so when the directory is a file.

## [2.1.0] - Unreleased

### Added

- Node certificate renewal. A worker renews its certificate on day 243 of 365, with a new key, over the node API, using its current certificate. The server issues for the node ID in that certificate only, and refuses a removed node or an expired certificate. The new key and certificate are written with mode 0600 and renamed into place, and the worker uses them without a restart. A worker must run 2.1 to renew; a 2.0 worker keeps working against a 2.1 server until its certificate expires.
- The server renews its own cluster listener certificate on the same schedule, without a restart.
- `hivemind nodes` has a CERT EXPIRES column, `GET /v1/nodes` a `cert_not_after` field, and `/metrics` the gauge `hivemind_node_cert_expiry_timestamp_seconds`. Schema version 7 adds the `node_certs` table; a 2.0 store is migrated when 2.1 opens it.
- `hivemind backup <file>`: a backup of the server's store, CA certificate and key, and admin token file, as a tar file with a manifest, a format version and a SHA-256 for each file. It is written with mode 0600, and checked before it is kept. It reads the data directory, with `serve` running or not; `--api` gets it from the new `POST /v1/backup` route instead, which sends it only over TLS or to a loopback client.
- `hivemind restore <file> --data-dir <dir>`: restores a backup into an empty directory, with mode 0700 and files 0600, after checking every digest, the store's integrity and schema version, and the CA. [docs/backup.md](docs/backup.md) has the procedure for replacing a lost server at the same address; the workers reconnect without joining again.
- `hivemind logs` reads replicas on workers, with `--tail` and `-f`. The worker keeps a poll open to the server's node listener, takes the log requests queued for its node, and streams each log back over a request it makes, so it still opens no listener. The server checks the node in the stream's certificate against the request. A worker that does not answer in 10 seconds gets a `504` that names it, a lost node a `503`, and a node with 16 streams open a `429`. Ctrl-C on `logs -f` ends the read on the worker.

### Changed

- Every node API request checks that the client certificate is valid by the server's clock. Before, only the TLS handshake did, so a worker that kept its connection open could keep using a certificate that had expired.
- In the CA's last year, node certificates are issued to end with the CA rather than refused, and the server logs a warning once a day. The CA lasts ten years and cannot be rotated.
- A worker whose certificate has expired fails at start with what to do: remove the node, and join again.
- `node.crt` on a worker is mode 0600 after a renewal; it was 0644.
- `GET /v1/apps/{name}/logs` reads the replica on the node it is placed on, not on the server's node. A replica on a worker no longer gets `404`. Workers of version 2.0.0 do not answer log requests, so their replicas get `504` until they are upgraded.

### Known limits

- The cluster CA cannot be rotated. When it expires, ten years after `init`, every machine must join a new cluster.
- A restored server has the state of the backup: nodes removed since then must be removed again, and join tokens used since then work again until they expire. Volumes on the old server are not in a backup.
- Restoring on a new address is not supported. Workers must join again.

## [2.0.0] - 2026-11-14

Version 2 is a rewrite. It is not compatible with 1.x: the command line, the API, the data directory and the feature set all differ. v1 data is not migrated, so start v2 in a new data directory. See [docs/upgrading.md](docs/upgrading.md).

### Added

- Subcommands: `init`, `serve`, `join`, `join-token`, `deploy`, `status`, `logs`, `scale`, `delete` and `version`.
- Workers. `serve --advertise` opens the node listener (port 4484) and the join listener (port 4485) on that address only. `join-token` issues a single-use token, which lasts 24 hours by default and 168 hours at most. `join` enrolls a machine as a worker and runs its agent in the foreground. `serve --role worker` runs a joined worker after a restart.
- A cluster CA, created by `init`. Node certificates are ECDSA P-256 and last 365 days. A join token carries the CA fingerprint, and a joining machine trusts only that CA.
- Placement across nodes: replicas spread over the nodes, within each node's CPU and memory, and without host-port clashes. A volume pins its replica to its node.
- Node loss: a node whose lease is more than 15 seconds old is lost, and its replicas are placed again. A replica with a volume stays on its node. A returning node removes the containers of replicas that moved away.
- A node takes new replicas 30 seconds after it last became ready (`--min-ready`).
- Rolling updates can surge onto a second eligible node, when that node has the app's host ports free.
- App spec in YAML or JSON, with validation of names, images, ports, volumes, resources and health checks.
- Docker Engine runtime, over the Engine API v1.44: pull, create, start, stop, remove, inspect, list by label, logs and exec.
- An agent on each node that starts, stops and replaces containers to match its assignments. Containers carry the labels `hivemind.app`, `hivemind.replica` and `hivemind.generation`.
- CPU and memory limits. `resources.cpu` becomes the container's `NanoCpus` and `resources.memory` its `Memory`, with `MemorySwap` equal to `Memory` so there is no swap on top. 0 means no limit. The smallest nonzero values are 0.01 CPUs and 6Mi. A container over its memory limit is OOM-killed and restarted with the usual backoff.
- Volumes scoped to their app. Volume `data` of app `web` is the Docker volume `hivemind-web_data`, labelled `hivemind.app` and `hivemind.volume`, so two apps that use the same volume name get separate data. Deleting an app keeps its volumes.
- HTTP, TCP and exec health checks, and restarts with exponential backoff from 1 second to 5 minutes. `status` shows each replica's health, restart count and crash loop.
- Rolling updates: a spec change moves the replicas one at a time. A move waits for its replica to be ready, has a deadline, and pauses the rollout with a reason on failure. Rolling back is deploying the old spec. `status` and the API report an unfinished rollout.
- `init` creates an admin token, stored as a SHA-256 digest. Every API route except `GET /healthz` needs it.
- Prometheus metrics at `/metrics`, and an optional loopback listener for them.
- Logs with `log/slog`, as text or JSON. Tokens, cookies and private keys are redacted.
- Settings from flags, `HIVEMIND_*` variables and a YAML file, in that order of precedence.
- A client config that `init` writes, so the CLI on the same machine needs no token in the environment.
- Command reference in `docs/cli.md`, generated from the command definitions.
- `nodes` lists the nodes, with capacity, lease age, ready or lost, and replica count. `node remove <id>` takes a node out of the cluster: its replicas are placed again, and a replica with a volume waits with no node and a reason. It refuses the server's own node, and a node that still renews its lease unless `--force` is given. The API routes are `GET /v1/nodes` and `DELETE /v1/nodes/{id}`.
- A denylist of removed node IDs, checked on every node API request after the TLS handshake. A removed node's certificate gets 403, the store refuses its ID, and join never issues it again. A running worker that is removed logs it and exits. Schema version 6 adds the table.
- `--ca-cert`, `HIVEMIND_CA_CERT` and a `ca-cert` key in the client config: a PEM file of certificates the CLI trusts for an https API, instead of the system roots. `init --api-tls-cert` records that certificate there.
- `POST /v1/session` and `DELETE /v1/session`: log in with the admin token to get an `HttpOnly`, `SameSite=Strict` session cookie and a CSRF token, and log out. Unsafe cookie requests need the CSRF token. Failed logins are limited to 5 per source address in 10 minutes.
- Release builds for six platforms, and `SHA256SUMS`. The node runs on Linux amd64 and arm64.

### Changed

- The API listens on `127.0.0.1:4483` by default, not on all interfaces.
- Authentication is mandatory.
- The YAML config file refuses a `token` key. Pass the API token with `--token` or `HIVEMIND_TOKEN`, or let the client read the token file that `init` wrote.
- Go 1.26 is required to build. The module declared 1.22 before.
- The SQLite store uses WAL mode, and each change is one transaction.
- `init` and `serve` refuse a v1 data directory and point to docs/upgrading.md.
- `init` creates the cluster CA as well as the admin token.
- Earlier v2 builds named a Docker volume by its spec name alone, so volume `data` was the Docker volume `data`, shared by every app on the node that used that name. It is now `hivemind-<app>_data`, and the first deploy with this build starts with an empty volume. To keep the data: deploy once, so the new volume exists with its labels; run `hivemind delete <app>`, which keeps both volumes; on the app's node, copy the data with `docker run --rm -v data:/from -v hivemind-<app>_data:/to alpine cp -a /from/. /to/`; then deploy again. 1.x is not affected, since v1 data is not migrated.

### Removed

- The web UI, the v1 `/api/` routes, the containerd runtime and the memberlist membership.
- Security (RBAC, secrets, network policy, IPAM, tenants, scanning, storage encryption, ACLs and QoS), resilience (circuit breaker, Redis storage, bulkhead, retry and fallback), service discovery (DNS and proxy), tenants, zero-downtime and canary deployment endpoints, and volume backup and snapshots.

### Known limits

- Node certificates cannot be renewed, and there is no revocation list: removing the node is the only revocation. A certificate lasts 365 days.
- A removed worker's containers keep running until they are removed by hand.
- A replica with a volume is never moved. Its app is down while its node is lost.
- Two apps that publish the same host port are both accepted. The second one's replicas go where the port is free, and any that cannot go stay `pending` with the reason until it is.
- `hivemind logs` cannot read replicas that run on a worker.
- hivemind never deletes a volume. Volumes of deleted apps stay on their node until removed with `docker volume rm`.
- A rolling update on one node replaces each replica in place, so that replica is down while it moves.
- Tests run three nodes in Docker. Larger clusters have not been tested.

## [1.0.1](https://github.com/ao/hivemind/compare/v1.0.0...v1.0.1) (2025-08-07)


### Bug Fixes

* Update development roadmap to reflect current implementation status and feature completion ([e5ab046](https://github.com/ao/hivemind/commit/e5ab046f92846bd72b04ea5de09efd9470c8ee0d))

## 1.0.0 (2025-08-07)


### Features

* Implement Proxy Server with service discovery and metrics tracking ([9ca7b91](https://github.com/ao/hivemind/commit/9ca7b9122add16de63908caedeb2815702f2ea9a))
* Implement TenantQuotaEnforcer for managing tenant resource quotas ([f35b0ee](https://github.com/ao/hivemind/commit/f35b0ee4431820f2c5864dc97138b50f3a70e34c))
* Update implementation status and complete Proxy Server in Service Discovery ([36c007f](https://github.com/ao/hivemind/commit/36c007f3c611af8e74bf99be3dfccee4aafee03f))


### Bug Fixes

* Enhance unit test coverage reporting and add gosec installation step ([695cf82](https://github.com/ao/hivemind/commit/695cf823429963405cfea99c2cde42e670b527d9))
* Improve test workflow by simplifying unit test command and enhancing gosec installation with error handling ([0cb037d](https://github.com/ao/hivemind/commit/0cb037de847ce48b903bfac392150cd0d53b1e01))
* Update coverage report filename in unit tests and Codecov configuration ([8574135](https://github.com/ao/hivemind/commit/8574135b06e2ed4da91a24b0935cd021d0ec3f96))
* Update integration tests to use localhost addresses and allow tests to continue on error ([b7a78d5](https://github.com/ao/hivemind/commit/b7a78d5f04f7246fa7ed813c8a9540460f15d1f1))
* Update test workflow to use Makefile commands and improve test node creation to avoid network timeouts ([dbe424b](https://github.com/ao/hivemind/commit/dbe424bbee7753bf0d7bb1978bd8b5a0e1a53238))


### Code Refactoring

* Organize network policy management and initialization tests ([fd6a02a](https://github.com/ao/hivemind/commit/fd6a02afe9a924d7a989b4b89052d23451af10e3))
