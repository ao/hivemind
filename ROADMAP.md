# Roadmap

Every task in [PLAN.md](docs/PLAN.md) has an implementation on this branch. These are what is left, in suggested order. Each one was checked against the code on this branch.

1. Node certificate renewal. A node certificate lasts 365 days, and a worker whose certificate has expired must join again with an empty data directory. Removing a node is the only revocation. See [security](docs/security.md#revocation-by-removal).
2. Draining a node: stop placing new replicas on it, and move its replicas off while it still runs, before removal. Nomad has `node eligibility` and `node drain` for this. Today `node remove --force` on a running node moves its replicas at once, and their old containers keep running on it until they are removed by hand.
3. Rolling updates of an app that publishes a host port. Two replicas of one app can't publish the same host port on one node, so on a single machine such an app has one replica and its update has a gap. Closing it needs a per-node proxy that owns the host port, which the current scope rules out.
4. Logs of replicas on workers. `GET /v1/apps/{name}/logs` reads only the server's containers.
5. Host ports at deploy time. `deploy` accepts a port that another app already publishes, and the replicas wait as `pending`. Decide whether to refuse the deploy instead.
6. The fault suite: a flap scenario with partitions longer than the 15 s lease, so every flap counts as a loss, and a kill of the whole server machine rather than the `serve` process.
7. A web UI, if one is wanted. The API has what a browser needs for Task 6: login and logout at `/v1/session`, a session cookie, and CSRF checks on unsafe requests. There are no pages.
8. Small fixes: `serve` and `init` accept `--server` and `--token` and ignore them, and `logs` accepts `--json` and ignores it.
