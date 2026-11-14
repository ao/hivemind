# Roadmap

Every task in [PLAN.md](docs/PLAN.md) has an implementation on this branch. These are what is left, in suggested order. Each one was checked against the code on this branch.

1. Draining a node: stop placing new replicas on it, and move its replicas off while it still runs, before removal. Nomad has `node eligibility` and `node drain` for this. Today `node remove --force` on a running node moves its replicas at once, and their old containers keep running on it until they are removed by hand.
2. Rolling updates of an app that publishes a host port. Two replicas of one app can't publish the same host port on one node, so on a single machine such an app has one replica and its update has a gap. Closing it needs a per-node proxy that owns the host port, which the current scope rules out.
3. Host ports at deploy time. `deploy` accepts a port that another app already publishes, and the replicas wait as `pending`. Decide whether to refuse the deploy instead.
4. The fault suite: a flap scenario with partitions longer than the 15 s lease, so every flap counts as a loss, and a kill of the whole server machine rather than the `serve` process. `scripts/integration-cluster.sh` now destroys the server machine and restores it from a backup, but not under the fault suite's partitions.
5. Cluster CA rotation. The CA lasts ten years from `init`, node certificates end with it, and the server warns in its last year. Rotating it needs a period in which nodes trust the old and the new CA, then a renewal of every node certificate under the new one. Until then, a cluster near its CA's end must be rebuilt with `init` and every machine joined again. See [security](docs/security.md#certificate-renewal).
6. Restoring a server at a new address. Workers keep the server's address in `node.json`, and the server's certificate names it. A restore at another address needs a way to tell the workers, and today they must join again.
7. A web UI, if one is wanted. The API has what a browser needs for Task 6: login and logout at `/v1/session`, a session cookie, and CSRF checks on unsafe requests. There are no pages.
8. Small fixes: `serve` and `init` accept `--server` and `--token` and ignore them, and `logs` accepts `--json` and ignores it.
