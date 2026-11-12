# Plan

This plan follows from [ASSESSMENT.md](ASSESSMENT.md). The current binary has no working path from "deploy" to "a container runs", so the work starts by building one narrow path end to end and growing it, rather than repairing every existing package. Code that isn't on that path is removed (decision 3 below); it stays in git history.

Each task is one pull request with its tests. A task is done when its acceptance tests pass in CI.

## Owner decisions

Answered by the owner on 2026-11-13:

1. Licence: GPL-3.0. `LICENSE` stays; the README's "MIT" line is fixed.
2. Scope: a single binary that runs containers on 1 to 10 Linux machines on one network, using Docker Engine, with one control-plane node, host-port networking, and no overlay network, built-in DNS or multi-tenancy. Docker Swarm, Nomad and k3s cover this ground and more; hivemind's case is that it is less to learn and run, and the README says so.
3. Removed, because none of it works and none of it is on the core path (about 12,500 lines of Go, plus their docs):
   - `internal/security` (RBAC, secrets, network policy, IPAM, tenants, scanning, storage encryption, ACL and QoS);
   - `internal/resilience` (circuit breaker, Redis storage, bulkhead, retry, fallback);
   - the service-discovery DNS server and proxy;
   - tenants;
   - zero-downtime, canary and A/B deployment endpoints;
   - volume backup and snapshot;
   - the docs for all of these.

   Rolling updates are kept and built properly (task 12).
4. SWIM: memberlist is removed in v2. Liveness runs over the authenticated node API. Revisit if the control plane becomes highly available.
5. Version: v2.0.0. The API, CLI, default runtime and listen addresses all change, and auth becomes mandatory.
6. Platforms: the node (`hivemind serve`) is supported on Linux amd64 and arm64. The darwin and windows builds are the client CLI. All six are built and smoke-tested in CI.
7. Container image: none for v2.0.0. The release is six static binaries and `SHA256SUMS`.

## Design

Roles. `hivemind init` creates a cluster on one machine, which becomes the server. Every node, the server included, runs an agent. The server stores the desired state and decides placement. Agents run containers and report back. If the server is down, running containers keep running and nothing new is scheduled.

Runtime: Docker Engine first. Docker Engine is reached through its HTTP API (v1.44, Docker Engine 25 or newer) on the local unix socket, with a small client in this repo. The official client module moved from `github.com/docker/docker` to `github.com/moby/moby/client` in 2025 and is still pre-1.0. The dozen endpoints hivemind needs are easier to pin, test and fuzz directly. Docker was chosen over containerd for these reasons:
- Docker provides port publishing, container networking, log storage and image-name normalisation. With containerd, hivemind would have to add CNI plugins and port mapping, collect logs and supervise processes itself, none of which exists today.
- The people this is for (a few machines) mostly have Docker installed already.
- Integration tests can run real nodes as Docker-in-Docker containers, locally and on GitHub Actions.

The runtime stays behind an interface, so containerd can be added later as a second implementation.

State.
- The server keeps apps, replicas, assignments, nodes and tokens in SQLite through `modernc.org/sqlite`, which is pure Go. That makes every binary static and lets it cross-compile.
- On each node, Docker labels (`hivemind.app`, `hivemind.replica`, `hivemind.generation`) are the record of what hivemind started. After a restart, the agent rebuilds its view from Docker rather than from memory.

Reconciliation. The server turns each app spec into replica assignments. Each agent fetches its assignments, compares them with the labelled containers it finds, starts what is missing, stops what is extra, and reports container state. The server reschedules replicas from lost nodes. Each assignment carries a generation number. A node that comes back after a partition stops any container whose generation the server has replaced.

Networking. Container ports are published on the node's host ports. A replica that publishes host port N can't share a node with another replica that publishes N, and the scheduler enforces this. There is no overlay network.

Volumes. Volumes are Docker named volumes on one node. An app with a volume is pinned to the node where its volume lives and is never moved automatically.

Security defaults.
- The API and UI listen on `127.0.0.1:4483`.
- `init` generates a random admin token (32 bytes from `crypto/rand`). It is printed once and written to `<data-dir>/admin.token` with mode 0600; the server stores only its hash.
- Every route except `GET /healthz` requires the token. The UI asks for the token once and sets an `HttpOnly`, `SameSite=Strict` session cookie, and form posts carry a CSRF token.
- No cluster listener runs unless `--advertise <addr>` is given. When it is, node traffic goes over TLS on port 4484 with mutual certificates from a cluster CA that `init` creates. A node joins with a join token, which is single-use and expires after 24 hours by default. With that token it gets a signed certificate.
- A non-loopback API address requires TLS.

## Tasks

### 1. It installs and runs

#### Task 1: Remove dead code (needs decision 3)
- Delete the packages and docs listed in decision 3, the `test/fixtures` mocks that only they use, and the tests that only test removed code.
- Acceptance: `go build ./...`, `go vet ./...` and `go test -race ./...` pass. `go mod tidy` drops the Redis, zap, x/crypto and memberlist modules (memberlist only if decision 4 says so).

#### Task 2: Static binaries with embedded assets
- Replace go-sqlite3 with `modernc.org/sqlite`. Embed `templates/` and `assets/` with `embed`. Set `go 1.26` in go.mod, build with `CGO_ENABLED=0`, and add `-trimpath`.
- Acceptance:
  - CI builds linux/amd64, linux/arm64, darwin/amd64, darwin/arm64, windows/amd64 and windows/arm64.
  - `hivemind version` and `hivemind --help` run on GitHub runners for linux amd64 and arm64, macOS arm64 and Windows amd64, started from an empty directory.
  - A test in a `scratch` container, with no libc and the working directory `/`, starts `hivemind serve` and gets 200 from `/healthz`.

#### Task 3: Docker runtime
- Implement pull (with name normalisation), create (env, command, published ports, named volumes, labels, restart policy off, because the agent supervises), start, stop with timeout, remove, inspect, list by label, logs with follow and tail, and the daemon's CPU and memory (`/info`).
- Acceptance:
  - Integration tests against a real `dockerd` (Docker-in-Docker) cover each operation.
  - An HTTP request reaches a published nginx port.
  - Logs return what the container printed.
  - Errors: a missing image, the daemon stopped mid-call, a port already in use, and a container removed out-of-band.

#### Task 4: Single-node reconcile
- Apps in the SQLite store, the agent loop on the same node, and status reported back.
- Acceptance (integration, one node):
  - Deploying 2 replicas of nginx gives 2 running containers within 30 s.
  - `docker rm -f` on one makes the agent replace it.
  - Deleting the app removes its containers.
  - Restarting `hivemind serve` neither restarts nor duplicates running containers.

#### Task 5: CLI
- Commands: `init`, `serve`, `join`, `deploy` (flags and `-f app.yaml`), `status`, `logs [-f]`, `scale`, `delete`, `version`. The client reads `--server`/`HIVEMIND_SERVER` and `--token`/`HIVEMIND_TOKEN`, or the config `init` wrote. Output is a table by default, or `--json`.
- Acceptance:
  - A test drives the real binary through every command against a running one-node cluster and checks exit codes and output.
  - A wrong token gives exit code 1 and the message `unauthorized`.
  - No server gives exit code 1 and a clear error.

### 2. Safe by default

#### Task 6: API and UI authentication
- Acceptance:
  - A test enumerates every registered route and checks that each returns 401 without a token, except `/healthz`.
  - The token comparison is constant-time.
  - UI login, then logout.
  - A form post without the CSRF token gets 403.
  - The token file is mode 0600, and `init` refuses to overwrite an existing cluster.

#### Task 7: Node-to-node mutual TLS
- A cluster CA (ECDSA P-256) is created at `init`. A join token gives a node a signed certificate with the node ID in it. The server checks that ID on every request.
- Acceptance:
  - A node without a certificate, with a certificate from another CA, or presenting another node's ID is rejected.
  - A join token can't be used twice or after it expires.
  - A test checks that plaintext and TLS-without-client-certificate connections to the cluster port fail.

#### Task 8: Listen addresses
- Defaults: the API on `127.0.0.1:4483`, and no cluster listener without `--advertise`. A non-loopback API address requires TLS.
- Acceptance:
  - An integration test reads the node's listening sockets, from `/proc/net/tcp*`, for each mode.
  - Starting with `--api-addr 0.0.0.0:4483` and no TLS fails with an explanation.

### 3. Correct orchestration

#### Task 9: Multi-node scheduling
- Placement spreads replicas across nodes, respects requested CPU and memory against each node's real totals, and prevents host-port conflicts.
- Acceptance:
  - Unit tests on the placement function, including table tests and a property test that no node ever exceeds capacity.
  - Integration on 3 nodes: 3 replicas land on 3 different nodes, and a 4th replica that needs a used host port stays pending with a reason.

#### Task 10: Node loss and rescheduling
- Agents renew a lease every 5 s. A node is lost after 15 s without renewal, and its replicas are rescheduled. Volume-pinned apps are reported as unavailable rather than moved. When a lost node returns, it stops any container whose generation was replaced.
- Acceptance (fault suite, 3 nodes):
  - After `docker kill` of a worker, its replicas run elsewhere within 30 s.
  - When the worker comes back, there are no duplicate replicas.

#### Task 11: Health checks and restarts
- HTTP, TCP and command probes, with interval, timeout and threshold. The agent restarts failed containers with exponential backoff capped at 5 minutes. Crash loops are reported in status.
- Acceptance:
  - A container that exits is restarted, and its restart count goes up.
  - A container that fails its HTTP probe is restarted.
  - A container that exits immediately backs off rather than spinning, and `status` shows it.

#### Task 12: Rolling updates
- Changing an app's image or spec replaces replicas one at a time (max 1 unavailable, max 1 extra). The update waits for each new replica to become healthy and stops if one does not.
- Acceptance:
  - While v1 is updated to v2, a client polling the app through the host ports sees no gap longer than one replica.
  - An update to an image that fails its health check stops after the first replica, the old replicas keep running, and `hivemind status` says why.

#### Task 13: State that survives restarts
- Acceptance:
  - Restarting the server keeps apps and assignments, and running containers are not restarted.
  - Restarting an agent picks up its containers from their labels.
  - `kill -9` of the server during a deploy leaves the store consistent: SQLite in WAL mode, with every state change in one transaction.

#### Task 14: Fault suite
- Run in Docker on 3 and 5 nodes:
  - kill a worker;
  - kill the server;
  - partition a worker from the server, then heal the partition (`iptables` inside the node container);
  - slow a node with `tc netem` at 2 s added latency;
  - flap a node 10 times in 2 minutes.
- Acceptance:
  - After each scenario the cluster converges to the desired replica count with no duplicates.
  - No replica is rescheduled while a node is only slow and still renewing its lease.
  - A flapping node stays out of placement until it has been healthy for 30 s.

### 4. Operable

#### Task 15: Config file and environment
- A YAML config file (`--config`, default `<data-dir>/hivemind.yaml`). Every flag has a `HIVEMIND_*` variable. Precedence: flag, then env, then file, then default. Acceptance: table tests for precedence, and unknown keys in the file are errors.

#### Task 16: Metrics
- `/metrics` in Prometheus text format, behind the same auth as the API. Optionally, `--metrics-addr` serves it on a separate loopback address. It reports apps, desired and running replicas, node leases, reconcile duration and errors, and HTTP requests. Acceptance: a test scrapes it and checks that the values match the cluster state.

#### Task 17: Logs
- `log/slog`, text by default and `--log-format json`. Gin, if it is kept, runs in release mode. Acceptance: tests check that no token, join token or certificate key ever appears in log output, including at debug level.

#### Task 18: Upgrade path
- v1 data directories are refused with a message: v1 never ran containers, so there is nothing to migrate. From v2.0.0 on, the store has a schema version and forward migrations. A test opens a store from the previous release. `docs/upgrading.md` says what changed from v1.

### 5. Docs that match the code

#### Task 19: Rewrite the docs
- README: what it is, the scope from decision 2, install, a quick start that stays on loopback, and its limits.
- `docs/quick-start.md`, `docs/concepts.md`, and `docs/cli.md` generated from the cobra commands.
- API reference.
- Delete the docs for removed features and for features that don't exist.
- Rewrite ROADMAP.md as a short list.
- Fix the installation guide, the badges (Go version from go.mod) and the CHANGELOG.

Acceptance: a CI job runs the quick-start commands from the README against a real node and fails if any of them breaks.

#### Task 20: CI and release workflow
- `test.yml` runs these jobs:
  - `go vet`, `staticcheck`, and `go test -race ./...`;
  - the integration tests (one node and three nodes in Docker-in-Docker);
  - the fault suite;
  - every fuzz target for 60 s each;
  - cross-compiled builds with smoke tests on Linux amd64 and arm64, macOS and Windows runners.
- `release.yml` runs only on a pushed `v*` tag, never on every push to main. It builds the six binaries, `SHA256SUMS` and the release notes from CHANGELOG.md, and creates a draft release for the owner to publish.
- Fuzz targets: decoding and validation of an app spec from the API, decoding of node-API messages (lease renewal, status reports, assignment lists), join-token parsing, and image-reference parsing.
