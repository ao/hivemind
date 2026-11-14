# Roadmap

Every task in [PLAN.md](docs/PLAN.md) has an implementation on this branch. These are what is left, in suggested order. Each one was checked against the code on this branch.

1. Rolling updates of an app that publishes a host port. Two replicas of one app can't publish the same host port on one node, so on a single machine such an app has one replica and its update has a gap. This is closed for routed apps only: an app with `routes` and no host ports publishes ephemeral ports behind the router, and its update starts each new copy beside the old one (see [routing](docs/routing.md#rolling-updates)). An app that publishes a fixed host port still has the gap on one machine.
2. Host ports at deploy time. `deploy` accepts a port that another app already publishes, and the replicas wait as `pending`. Decide whether to refuse the deploy instead.
3. The fault suite: a kill of the whole server machine rather than the `serve` process. `scripts/integration-cluster.sh` now destroys the server machine and restores it from a backup, but not under the fault suite's partitions.
4. Cluster CA rotation. The CA lasts ten years from `init`, node certificates end with it, and the server warns in its last year. Rotating it needs a period in which nodes trust the old and the new CA, then a renewal of every node certificate under the new one. Until then, a cluster near its CA's end must be rebuilt with `init` and every machine joined again. See [security](docs/security.md#certificate-renewal).
5. Restoring a server at a new address. Workers keep the server's address in `node.json`, and the server's certificate names it. A restore at another address needs a way to tell the workers, and today they must join again.
6. A web UI, if one is wanted. The API has what a browser needs for Task 6: login and logout at `/v1/session`, a session cookie, and CSRF checks on unsafe requests. There are no pages.
7. Router metrics from workers. A worker has no metrics listener, so only the server's router counters are in `/metrics`. The workers could send theirs with their status reports.
8. Routing beyond exact hosts: path prefixes, HTTP/2 or gRPC to the replicas, and TCP routes. Each is refused today.
