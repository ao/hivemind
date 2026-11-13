# Roadmap

The tasks come from [PLAN.md](docs/PLAN.md). These are the tasks not finished yet, in the order I would do them.

1. Ports: refuse a host-port clash at deploy time. Today the second app is accepted, and its replica stays `pending` with the engine's error until the port is free. The scheduler has the check, and `serve` does not use the scheduler yet.
2. Resources: apply `cpu` and `memory` to containers, and check them against the node.
3. Volumes: decide whether volume names are scoped to an app. Today two apps that use one volume name share its data.
4. Task 9, multi-node scheduling: use `scheduler.Place` in the store, with spread, capacity and host-port checks, on three nodes. Rolling updates already move replicas to a second node as a surge when one is registered (task 12 is in `rollout.go`); that path cannot run until `join` does.
5. Task 7, node-to-node mutual TLS: the cluster CA, join tokens and the node listener on port 4484, behind `--advertise`.
6. Task 5 and task 10: `hivemind join`, and rescheduling after a node is lost (`scheduler.LostNodes` exists).
7. Task 6, the web UI: login, logout and CSRF forms. The session code exists and no route uses it.
8. Task 14, the fault suite: node kills, server kills, partitions, slow nodes and flapping, on three and five nodes.
9. Task 18, upgrades: refuse a v1 data directory with a message, and test the store against a database from a previous release.
10. Task 20, CI: run the integration tests in Docker-in-Docker and the quick-start check, run the fuzz targets for 60 seconds each, and add staticcheck. The release workflow exists and makes a draft release on a tag.
11. Task 2, the scratch smoke test: start `hivemind serve` in the scratch image and get 200 from `/healthz`. It runs `version` and `--help` today.
12. Small fixes: `serve` and `init` accept `--server` and `--token` and ignore them, and `logs` accepts `--json` and ignores it.
