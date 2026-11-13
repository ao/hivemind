# Changelog

All notable changes to this project will be documented in this file. See [Conventional Commits](https://conventionalcommits.org) for commit guidelines.

## [2.0.0] - Unreleased

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
- HTTP, TCP and exec health checks, and restarts with exponential backoff from 1 second to 5 minutes. `status` shows each replica's health, restart count and crash loop.
- Rolling updates: a spec change moves the replicas one at a time. A move waits for its replica to be ready, has a deadline, and pauses the rollout with a reason on failure. Rolling back is deploying the old spec. `status` and the API report an unfinished rollout.
- `init` creates an admin token, stored as a SHA-256 digest. Every API route except `GET /healthz` needs it.
- Prometheus metrics at `/metrics`, and an optional loopback listener for them.
- Logs with `log/slog`, as text or JSON. Tokens, cookies and private keys are redacted.
- Settings from flags, `HIVEMIND_*` variables and a YAML file, in that order of precedence.
- A client config that `init` writes, so the CLI on the same machine needs no token in the environment.
- Command reference in `docs/cli.md`, generated from the command definitions.
- Release builds for six platforms, and `SHA256SUMS`. The node runs on Linux amd64 and arm64.

### Changed

- The API listens on `127.0.0.1:4483` by default, not on all interfaces.
- Authentication is mandatory.
- Go 1.26 is required to build. The module declared 1.22 before.
- The SQLite store uses WAL mode, and each change is one transaction.
- `init` and `serve` refuse a v1 data directory and point to docs/upgrading.md.
- `init` creates the cluster CA as well as the admin token.

### Removed

- The web UI, the v1 `/api/` routes, the containerd runtime and the memberlist membership.
- Security (RBAC, secrets, network policy, IPAM, tenants, scanning, storage encryption, ACLs and QoS), resilience (circuit breaker, Redis storage, bulkhead, retry and fallback), service discovery (DNS and proxy), tenants, zero-downtime and canary deployment endpoints, and volume backup and snapshots.

### Known limits

- Node certificates cannot be revoked or renewed, and there is no command to remove a node. A certificate lasts 365 days.
- A replica with a volume is never moved. Its app is down while its node is lost.
- Two apps that publish the same host port are both accepted. The second one's replicas go where the port is free, and any that cannot go stay `pending` with the reason until it is.
- `hivemind logs` cannot read replicas that run on a worker.
- `resources` count against a node's capacity during placement, but are not applied to containers.
- Volume names are not scoped to an app.
- The CLI cannot trust a private CA for an API served with TLS.
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
