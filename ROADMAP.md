# Roadmap

The tasks come from [PLAN.md](docs/PLAN.md). These are the tasks not finished yet, in the order I would do them.

1. Task 11, status: show restart counts, health and crash loops in `hivemind status`. They are in `/metrics` only.
2. Ports: refuse or report a host-port clash. Today a second app on the same host port can run without its port published and report `running`. The scheduler has the check, and `serve` does not use the scheduler yet.
3. Resources: apply `cpu` and `memory` to containers, and check them against the node.
4. Volumes: decide whether volume names are scoped to an app. Today two apps that use one volume name share its data.
5. Task 12, rolling updates: replace replicas one at a time, wait for each to be ready, and stop on a failed health check. Under way on another branch.
6. Task 9, multi-node scheduling: use `scheduler.Place` in the store, with spread, capacity and host-port checks, on three nodes.
7. Task 7, node-to-node mutual TLS: the cluster CA, join tokens and the node listener on port 4484, behind `--advertise`.
8. Task 5 and task 10: `hivemind join`, and rescheduling after a node is lost (`scheduler.LostNodes` exists).
9. Task 6, the web UI: login, logout and CSRF forms. The session code exists and no route uses it.
10. Task 14, the fault suite: node kills, server kills, partitions, slow nodes and flapping, on three and five nodes.
11. Task 18, upgrades: refuse a v1 data directory with a message, and test the store against a database from a previous release.
12. Task 20, CI: run the integration tests in Docker-in-Docker and the quick-start check, run the fuzz targets for 60 seconds each, and add staticcheck. The release workflow exists and makes a draft release on a tag.
13. Task 2, the scratch smoke test: start `hivemind serve` in the scratch image and get 200 from `/healthz`. It runs `version` and `--help` today.
14. Small fixes: `serve` and `init` accept `--server` and `--token` and ignore them, and `logs` accepts `--json` and ignores it.
