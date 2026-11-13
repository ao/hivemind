# Roadmap

The tasks come from [PLAN.md](docs/PLAN.md). These are the tasks not finished yet, in suggested order. Each one was checked against the code on this branch.

1. Task 14, the fault suite. `scripts/integration-cluster.sh` covers a worker that is killed and comes back, and host-port placement on three nodes. Still to do: a killed server, a partition that heals, a node slowed by 2 s of added latency, a node flapping 10 times in 2 minutes, and five nodes.
2. Node certificate revocation and renewal. A node certificate lasts 365 days and cannot be revoked. There is no command to remove a node. See [security](docs/security.md#no-revocation-yet).
3. Task 6, the web UI: login, logout and CSRF forms. The session code in `internal/auth` exists, and no route uses it.
4. The CLI's `--ca-cert`: let the CLI trust a private CA for an API served with TLS.
5. Logs of replicas on workers. `GET /v1/apps/{name}/logs` reads only the server's containers.
6. Host ports at deploy time. `deploy` accepts a port that another app already publishes, and the replicas wait as `pending`. Decide whether to refuse the deploy instead.
7. Small fixes: `serve` and `init` accept `--server` and `--token` and ignore them, and `logs` accepts `--json` and ignores it.
