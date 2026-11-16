# Scale test: ten machines, 100 apps, 500 replicas

hivemind is meant for one to ten machines. This test ran it at ten: one server and nine workers, 100 apps with 500 replicas between them. It deployed them one after another and 20 at once, left them alone for 30 minutes, powered off three workers at once, rolled 50 apps at once, drained a worker, removed a powered-off one, scaled every portless app to twice its count and back, restarted the server, and restored it from a backup on a new machine. Every time below comes from the harness's log or the machines' journals; the benchmark section is marked as such. The harness is [scripts/soak](../scripts/soak), with `scale.sh` for this test; the last section says how to run it.

## What broke or was slow

In order of damage. The evidence is in the sections below. Each has been fixed since this run, except that a deploy still reads every placed replica (part of 4); [After the fixes](#after-the-fixes) says how, and what was measured.

1. A node removes containers one at a time, and nothing else happens on it meanwhile. A container whose PID 1 ignores SIGTERM, such as `busybox sleep`, takes the full 10 s stop timeout. During the 50-app rolling update, the surge copies of the 30 apps with a health check waited behind those stops on their nodes: one was created 42 s after it was placed, another not at all within its 60 s deadline. All 30 rollouts paused with `replica 3 is not ready after 1m0s` and stayed split between two generations until they were deployed again. Alone, the same 30 rollouts finished in 112.5 s. The same serial removal made deleting the 100 apps take 185 s and scaling back from 933 to 500 replicas take 367 s, although the API calls took about a second.
2. A replica with a volume, held on a lost node, is reported `running` and `ready`, and counted in the app's `running` and `ready`, while its `pending_reason` says the app is down. With three workers powered off, `GET /v1/status` said 500 of 500 running; two of them were on machines that were off. The metrics code counts the same way (read in `internal/server/metrics.go`, not measured).
3. Every write goes through the store's one SQLite connection, with `synchronous=FULL`. With 20 deploys in flight, a deploy took 72.8 ms at the median and 331.7 ms at most, against 8.2 ms and 23.5 ms one after another, and a `GET /v1/status` in that second took 510.6 ms. In the steady state, 46% of the server's CPU samples were in the workers' calls to the node API, most of it `ReportStatus`, which deletes and re-inserts every container row of the node every 2 s, one `INSERT` per container. Compiling SQL statements, which the driver does for each statement run, was 20.5% of the samples.
4. One replica that cannot be placed makes every sweep and every node's report run the whole placement again. In the benchmarks, a sweep at 500 replicas took 5.2 ms instead of 1.4 ms, and a report 5.6 ms instead of 3.1 ms; at 2000 replicas, 15.9 ms and 16.8 ms. A deploy also reads every placed replica, with its spec, before placing its own, so deploying N apps costs O(N²) in total: one more app took 4.8 ms with 500 replicas in the store and 13.7 ms with 2000. Each figure is linear in the replica count, and small at this scope.
5. A removed worker under the documented systemd unit restarts every 5 s: the server refuses it, it exits with status 1, and `Restart=on-failure` starts it again. It restarted 7 times in 35 s, each logged as a warning on the server. Its message also says its containers are still running, when after a power cycle all 58 had exited.

Nothing else failed. No replica ran on two machines in any sample. No request got an error status. No replica moved except by a power-off, the rollouts, the drain and the scaling, and the workers' containers were unchanged across both server restarts and the restore, so no node that was up was marked lost.

## The machines

| | |
|---|---|
| Nodes | 10 EC2 c7g.large in one subnet: 2 vCPU (Graviton3), 3.7 GiB (MemTotal 3897240 kB), 30 GB gp3 |
| OS | Amazon Linux 2023.12.20261112, kernel 6.18.62-127.179.amzn2023.aarch64 |
| Docker | 25.0.18, from the distribution |
| hivemind | commit f80c1aa, `CGO_ENABLED=0 go build -trimpath` for linux/arm64 with Go 1.27.4, symbols kept for `perf` (`hivemind version` prints `dev (f80c1aabca93)`) |
| Roles | hivemind-soak-1 is the server (`serve --advertise <its private address>`), hivemind-soak-2 to 10 are workers; units from [systemd.md](systemd.md), as root |
| Control machine | an EC2 c8g.4xlarge (16 vCPU, Graviton4) in the same VPC, which runs the harness and the benchmarks |

The API listened on 127.0.0.1 only, the cluster ports on the server's private address, and the apps' host ports on 127.0.0.1; routed replicas publish on each node's private address. After the deploy, the harness checked every node's listeners: no hivemind port and no port that `docker-proxy` held was on a wildcard address.

## The load

100 apps, 500 replicas, from `scale.sh specs`:

| Apps | Image | Replicas each | Notes |
|---|---|---|---|
| `web` | nginx:alpine | 3 | 127.0.0.1:8080; the probes curl it |
| `hp-01` to `hp-18` | nginx:alpine | 3 | host ports 127.0.0.1:9001 to 9018 |
| `kv` | redis:7-alpine, `--appendonly yes` | 1 | volume, 127.0.0.1:6379; a value written once is read back by every probe |
| `vol-1` to `vol-9` | redis:7-alpine, `--appendonly yes` | 1 | volume, no port |
| `hc-01` to `hc-30` | nginx:alpine | 5 | HTTP health check on port 80 (defaults: every 10 s, 3 retries) |
| `rt-01` to `rt-10` | nginx:alpine | 3 | routed as `rt-NN.scale.test`, port 80 |
| `cmp1` to `cmp5` | busybox:stable `sleep 86400` (1 to 3), nginx:alpine (4, 5) | 4 | one compose file each, deployed with `hivemind compose deploy` |
| `bb-01` to `bb-16` | busybox:stable, `sleep 86400` | 9 (bb-16: 8) | |
| `ng-01` to `ng-10` | nginx:alpine | 9 | |

So 20 apps publish a host port, 10 have a volume, 30 a health check, 10 are routed, and 5 come from compose files. The 71 apps with no host port and no volume (`hc`, `rt`, `cmp`, `bb`, `ng`) are the portless ones. 163 replicas run `sleep` as PID 1, which ignores SIGTERM.

## Method

The harness runs on the control machine and reaches the nodes over ssh. Each line of its log carries the control machine's UTC time in milliseconds. Every API request is made on the server machine, by curl or the CLI, and its latency is curl's `time_total` there; a compose deploy is timed around the CLI with `date`, so it includes the CLI's start.

- Deploys, scales, deletes and rollouts: `scale.sh` sends the requests from a script on the server, one after another or N at once (`xargs -P`), and meanwhile samples `GET /v1/status` once a second: the replicas desired, running and ready, the apps below their count (`running < desired`), the rollouts in progress and paused. Every third sample after all are running, it counts the hivemind containers on every node with `docker ps -a`. "All running" is the first sample with every replica running; "containers match" is the first count equal to the desired total.
- The probe loop of the soak test ran throughout: every 10 s, `hivemind status --json`, the convergence check of `scripts/lib/converged.awk` against `docker inspect` on every node, and the `web` and `kv` checks; every 60 s, each hivemind process's RSS, threads, CPU time and fds, the store's file sizes, and five requests each to `GET /v1/status`, `/v1/nodes` and `/v1/apps`.
- During the power-offs, the soak test's watcher polled once a second: a new TCP connection to port 22 of each machine, `hivemind nodes` and `hivemind status`, and logged each change.
- Images were pulled on every node before the first deploy (6.0 to 6.2 s per node for the three images), so neither deploy includes a pull. The replaced server machine pulled them again.
- The server was profiled with `perf record -F 99 -g -p <pid>` for the 30 minutes of the steady state (9136 samples), and for 150 s of the first deploy. The benchmarks are Go benchmarks run on the control machine; they are described in their section.

## Results

Times are UTC on 2026-11-16.

### 1. Deploy

| | One after another | 20 at once |
|---|---|---|
| Requests | 95 `PUT /v1/apps/{name}`, 5 `compose deploy` | the same, 20 in flight |
| Request latency, all 100: p50 / p99 / max | 8.2 / 23.2 / 23.5 ms | 72.8 / 304.1 / 331.7 ms |
| `compose deploy` (CLI), min / p50 / max | 8.7 / 11.4 / 14.7 ms | 24.5 / 128.0 / 260.2 ms |
| Every request answered | 1.8 s after the first | 1.0 s |
| First request to all 500 running | 14.1 s | 14.1 s |
| First request to all 500 ready | 26.1 s | 25.1 s |
| `GET /v1/status` in that time, max | 20.1 ms | 510.6 ms |
| Error statuses | none | none |

Each node got exactly 50 replicas. The ten routed hosts had three endpoints each in `GET /v1/routes`. The time from all running to all ready is the health-checked apps waiting for their first check.

Deleting the 100 apps, between the two deploys: the 100 `DELETE` requests took 3.4 ms at the median and 5.4 ms at most, all `202`, and `GET /v1/status` was empty at once. The last of the 500 containers was gone 184.8 s after the first request. Until the `sleep` containers were gone, each node removed about one container every 10 s.

### 2. Steady state

30 minutes, 05:27:09 to 05:57:11, after the second deploy, with no change to the apps. The load on the API was the probe loop's; the watcher and the 1 s sampler were off.

| Probe | Result |
|---|---|
| Status answered, converged, 500 of 500 running, no replica on two machines | 180 of 180 |
| `web` answered on its three nodes | 540 of 540 |
| `kv` value read back and the same as written | 180 of 180 |

| Process, 31 samples a minute apart | RSS first / last / max (MiB) | RSS slope (KiB/h) | Threads | Open fds | CPU, % of one CPU |
|---|---|---|---|---|---|
| Server (hivemind-soak-1) | 27.8 / 27.6 / 29.9 | -14 | 10 | 42 to 43 | 3.89 |
| Each worker, range of the nine | 21.7-22.4 / 21.8-22.6 / 22.5-23.1 | -339 to +678 | 8 to 10 | 10 to 12 | 0.25 to 0.27 |

The store was 404 KiB, with a 4080 to 4088 KiB WAL and a 32 KiB shared-memory file. Thirty minutes is too short to say anything about a slow leak; the RSS slopes are noise at that length. Goroutines were not measured: `/metrics` has no `go_goroutines`, and pprof is not built in.

| Request, in the window | Requests | p50 / p99 / max (ms) | Reply bytes |
|---|---|---|---|
| `GET /v1/status` | 155 | 12.8 / 38.7 / 69.3 | 114600 |
| `GET /v1/nodes` | 155 | 3.7 / 7.1 / 10.6 | 3279 to 3305 |
| `GET /v1/apps` | 155 | 1.3 / 4.7 / 4.8 | 27320 |
| `GET /v1/routes`, 100 in a row after the window | 100 | 1.1 / 5.6 / 5.8 | 1325 |

The status reply is 229 bytes per replica: 114600 bytes for 500, and 205753 bytes for the 933 of section 6.

Where the server's CPU went, from `perf`: `perf report` on the c7g.large did not finish in 5 minutes, and `perf script --no-inline` was cut off at 280 s, so this is the first 1934 samples, about the first 6.4 minutes of the window. Each row counts the samples with that function on the stack.

| Function | Share of samples |
|---|---|
| `net/http.(*conn).serve`, every request | 59.1% |
| node API (`cluster.(*nodeAPI)`), the workers' calls | 45.7% |
| `store.(*Store).ReportStatus` | 30.3% |
| `sqlite.(*conn).ExecContext`; of it, compiling the statement (`prepareV2`) | 26.3%; 20.5% |
| `store.(*Store).AssignmentsForNode` | 9.4% |
| Garbage collection (`gcBgMarkWorker`) | 9.7% |
| Public API middleware; of it, `GET /v1/status` | 7.9%; 6.2% |
| The server's own agent (`agent.(*Agent).Run`) | 7.8% |
| The sweep (`main.sweep`) | 1.6% |

The sweep had 30 samples, about 0.3 s of CPU for some 76 sweeps (one every 5 s), so roughly 4 ms of CPU per sweep. That is an estimate from samples, not a timing. The benchmark below puts the same work at 1.4 ms on the control machine's faster cores.

### 3. Three workers powered off at once

hivemind-soak-8, 9 and 10 were powered off with `sysrq o` at the same time. They ran 150 replicas: 148 without a volume, and `vol-4` and `vol-1`. Times from the watcher (1 s), except where it says so.

| Time | What happened |
|---|---|
| 06:22:47.0 | Power-off requests sent, in parallel |
| 06:22:54.3 to 06:22:56.3 | Port 22 closed (the watcher checks the machines one after another, 1 s each, so the three are within these 2 s) |
| 06:23:07.8 | All three `lost` in `hivemind nodes`, lease 15.5 to 16.9 s old |
| 06:23:08.6 | The 148 replicas have new nodes and are pending there; 84 apps below their count |
| 06:23:14.8 | 458 of 500 running |
| 06:23:17.9 | 500 of 500 running, no app below its count; 451 ready |

So every replica without a volume ran elsewhere at most 10.1 s after the nodes were lost, and at most 23.6 s after the first machine went silent. The status sampler ran every 3 s here, not every second, because it waited on port 22 of the dead machines; the times after 06:23:08 are as late as 3 s.

`vol-1` and `vol-4` stayed on their nodes, as documented, with `pending_reason` "volume is on node ..., which is unavailable". But their state was `running`, `ready` was true, and both apps counted them in `running` and `ready`: finding 2.

The three machines were started again at 06:30:59.3:

| Measure | Seconds after the start request |
|---|---|
| Port 22 open | 14.7 to 15.5 |
| `ready` | 17.6 to 18.6 |
| Each removed its 49, 49 and 50 exited containers (workers' journals) | within 0.6 s of its agent starting |
| `vol-1` and `vol-4` running again on their own nodes, same containers | 20.3 |
| Converged (10 s probe) | 23.6 at the latest |
| Replicas on two machines at any time | none |

### 4. Rolling update of 50 apps at once

At 06:34:04, 50 apps got a new environment value, all 50 `PUT`s at once: the 30 `hc` apps, the 16 `bb` apps and `ng-01` to `ng-04`. Each rollout surges one replica at a time.

| Measure | Result |
|---|---|
| `PUT` latency, p50 / max | 78.9 / 228.4 ms, all `200` |
| Samples, one a second | 819 |
| Apps with fewer replicas running than desired, in any sample | none |
| Apps with fewer replicas ready than desired, in any sample | none |
| The 20 apps without a health check, done | 4 min 10 s to 6 min 22 s after the requests |
| The 30 apps with a health check | all paused at 06:36:13.8, 2 min 10 s in: `rollout to generation 2 stopped: replica 3 is not ready after 1m0s`, with 3 of 5 replicas moved |
| `GET /v1/status` in the window, p50 / p99 / max | 11.5 / 27.6 / 230.7 ms |

The deadline of a move with this health check is 60 s: 3 retries at 10 s, plus a 30 s margin, counted from when the surge is placed. The journals show why the moves missed it. Replica 3 of `hc-01` was placed on hivemind-soak-2 at about 06:35:14, 60 s before the pause; that node's agent was stopping `sleep` containers for the `bb` rollouts, 10 s each, and created the copy at 06:35:55.5, 42 s later. It was not reported ready in the 18 s left, and was removed at 06:36:30.6. Replica 3 of `hc-02` has no "created container" line on any node before the pause.

A paused rollout waits for another deploy, so at 06:48:08 the 30 `hc` apps were deployed again with a new value, alone. All 30 finished at 06:50:01.1, 112.5 s later, with no app below its count and no pause. `PUT` latency: p50 44.6 ms, max 203.8 ms.

### 5. Drain, and removing a powered-off worker

The drain took the worker with the most replicas, hivemind-soak-5 with 64 (after section 3, the three returned workers had almost none). `hivemind node drain <id> --deadline 10m` started at 06:50:22.7 and returned 188.6 s later: 63 moved, none stopped at the deadline, one left, `vol-6`, because its volume is on the node. No app was below its count in any of the 190 samples, and `GET /v1/status` took at most 18.9 ms.

Then hivemind-soak-7, with 58 replicas, `vol-9` one of them, was powered off at 06:54:47.0:

| Measure | Result |
|---|---|
| Port 22 closed | 06:54:55.1 |
| `lost` | 06:55:09.3 (scenario loop, 1 to 3 s) |
| 57 replicas running elsewhere, no app below its count | 06:55:22.4 |
| `DELETE /v1/nodes/{id}` at 06:55:53.5 | `200` in 4.2 ms, `{"rescheduled": 0, "held": 1}` |
| `vol-9` | pending, with "volume was on node ..., which was removed, and no other node has its data; deploy the app with a changed spec ..." |

`rescheduled` is 0 because the replicas had already moved when the node was lost. When the machine was started again, its unit ran, the server refused it, and it exited with the message docs/cli.md describes; systemd restarted it every 5 s (finding 5). Joining it again as docs/cli.md says (stop the unit, `docker rm -f` its containers, empty `/var/lib/hivemind`, `hivemind join` with a new token) took 1.5 s and gave it a new node ID. `vol-9` was then deployed with a new volume name, and ran, with an empty volume, within 15 s.

### 6. Scale every portless app to twice its count, and back

| | To twice (500 to 933) | Back (933 to 500) |
|---|---|---|
| Requests | 71 `POST /v1/apps/{name}/scale`, one after another | the same |
| Latency, p50 / max | 13.0 / 28.8 ms | 2.8 / 10.4 ms |
| Status at the new count, all running | 24.2 s after the first request | 1.1 s |
| All ready | 27.2 s | 1.1 s |
| Containers on the nodes equal to the count | 24.3 s | 367.3 s |
| `GET /v1/status` in the window, p50 / max | 19.6 / 65.7 ms | 14.7 / 35.8 ms |

At 933 replicas, each node ran 92 to 94 containers and had at least 2.3 GiB available. The status reply was 205753 bytes.

Scaling back removes the highest replica indices, which were the ones just added. Placement fills the emptiest node first, so the rejoined hivemind-soak-7, which ran nothing, had got 94 of the 433 new replicas, and each other node 37 to 39. From the nodes' journals: the nine removed their 37 to 39 containers in 80 to 191 s, and hivemind-soak-7 removed 94, 34 of them `sleep` containers at 10 s each, the last 365 s after the first request.

### 7. Restart the server, and restore it on a new machine

`systemctl restart hivemind` on the server, twice, with the 500 replicas running. The first run's harness loop was slow (see below), so its figures come from the probe and the server's journal.

| Measure | Run 1, 07:05:26 | Run 2, 07:09:39 |
|---|---|---|
| `systemctl restart` returned | 0.08 s | 1.15 s |
| API answered after that | 0.03 s | 0.03 s |
| All ten nodes `ready` | - | 0.30 s after the API |
| Replicas running in `status`, first sample after the restart | 56 of 500, 0.05 s after the API | 56 of 500 at 0.27 s, 223 at 1.26 s |
| 500 of 500 running, converged | at most 3.0 s after the API (probe) | 2.26 s after the API |
| Containers replaced or added | none | none |

Until a node reports to the new process, its replicas are shown as `unknown`, as the API documents.

Then the server's data directory was backed up (`hivemind backup`, 425472 bytes: manifest, store, CA key and certificate, admin token), its machine was terminated, and a new, empty one was launched at the same address. The harness installed the binary, ran `hivemind restore` and started the unit as soon as the new machine's user data had installed Docker. The workers were not touched.

| Measure | Seconds |
|---|---|
| Replace request to API down | 7.3 |
| API down to the new machine ready (EC2 launch, boot, Docker installed) | 103.8 |
| Restore | 0.56 |
| Restore to the unit started | 0.55 |
| Unit started to API up | 0.88 |
| API down to API up | 105.8 |
| API up to each worker `ready`, without joining again | 0.3 (six), 1.4 (three) |
| API up to 500 of 500 running and converged | 21.4; the server's 56 replicas were created again on the new machine, which pulled the images first |
| Workers' containers, every 5 s while the server was away, and after | unchanged: same IDs and start times, none added |
| `kv` value | intact |

`vol-3` and `vol-5` ran on the old server, so they started again with empty volumes, as [backup.md](backup.md) says.

## Benchmarks

To see how the costs grow past 500 replicas, the Go benchmarks in [scripts/soak/testdata/scale_bench_test.go](../scripts/soak/testdata/scale_bench_test.go), copied into `internal/server`, ran on the control machine in `golang:1.27`: one goroutine, against a store of N apps of 5 replicas on 10 ready nodes, every replica reported running by its node, a third of the apps with a health check. 50 iterations each, `go test -run '^$' -bench Scale -benchtime 50x ./internal/server`, Go 1.27.4, c8g.4xlarge. Times per operation:

| Operation | 100 replicas | 500 | 1000 | 2000 |
|---|---|---|---|---|
| `GET /v1/status` (reply bytes) | 1.4 ms (22402) | 6.8 ms (112002) | 14.4 ms (224002) | 29.7 ms (448002) |
| `GET /v1/apps` | 0.12 ms | 0.47 ms | 0.92 ms | 1.79 ms |
| `GET /v1/nodes` | 0.49 ms | 1.98 ms | 3.99 ms | 8.02 ms |
| Sweep (`ReconcileNodes`, `AdvanceRollouts`, `AdvanceDrains`) | 0.40 ms | 1.36 ms | 2.59 ms | 5.03 ms |
| Sweep, with one replica no node can take | 3.08 ms | 5.17 ms | 8.86 ms | 15.88 ms |
| One node's assignments and report | 2.46 ms | 3.06 ms | 4.87 ms | 7.99 ms |
| The same, with one replica no node can take | 3.29 ms | 5.64 ms | 9.65 ms | 16.80 ms |
| `PutApp` of one more app of 5 replicas | 2.79 ms | 4.82 ms | 8.49 ms | 13.75 ms |

Every operation grows about linearly with the replica count. The report and the put include an fsync of the store, which is why they do not start near zero. Ten nodes report every 2 s, so at 2000 replicas with one unplaceable replica the reports alone would hold the store's one connection about 8% of the time.

## After the fixes

The findings were fixed after this run. The ten-machine test was not run again; what was run instead is below.

1. Serial removal. An agent now stops and removes extra containers in the background, 8 at a time, and starts the containers it needs without waiting for them. A container that needs a host port, the volumes or the name of one being removed waits for that one only, and its removal skips the queue. A rollout move's deadline counts from when the node first reports a container of the new copy. `scripts/integration-cluster.sh` now rolls 8 apps with an HTTP health check (a 33 s move deadline) at the moment 24 `sleep` containers, 8 per node, are deleted, on three Docker-in-Docker nodes on one c8g.4xlarge: the `sleep` containers were gone in 16 s and the 8 rollouts finished 26 s after the delete, none paused. The same script against the code before the fixes failed there as this run did: each node removed a `sleep` container every 10 s, and 37 s after the delete all 8 rollouts had paused with `replica 0 is not ready after 33s`.
2. A held volume replica on a lost node is `unknown` and not counted, in status and metrics.
3. A report that is the same as the node's last one renews the lease and stamps the rows, without rewriting them; a changed one prepares its `INSERT` once. Reads outside a transaction use a pool of four read-only connections beside the write connection.
4. Placement is skipped when nothing it reads changed since it last placed nothing, and a transaction decodes each app generation's spec once and reads the placed replicas once.
5. A removed worker exits with status 3, and the systemd units have `RestartPreventExitStatus=3`.

The same benchmarks, with two more, on the build host (c8g.4xlarge, 16 vCPU Graviton4, Amazon Linux 2023, `golang:1.27` in Docker, `-benchtime 50x`), before (the code this run used, with fix 2) and after, each the mean of two runs, run in turn on an idle machine:

| Operation | 100 replicas | 500 | 1000 | 2000 |
|---|---|---|---|---|
| Sweep, with one replica no node can take | 2.99 → 0.61 ms | 5.04 → 1.80 ms | 8.65 → 3.31 ms | 15.58 → 6.34 ms |
| Sweep, nothing pending | 0.41 → 0.40 ms | 1.36 → 1.35 ms | 2.56 → 2.54 ms | 5.01 → 4.97 ms |
| One node's assignments and report, report unchanged | 2.46 → 2.23 ms | 3.02 → 1.95 ms | 4.84 → 2.44 ms | 7.92 → 3.41 ms |
| The same, with one replica no node can take | 3.24 → 2.42 ms | 5.57 → 2.52 ms | 9.40 → 3.33 ms | 16.47 → 4.86 ms |
| One node's report, changed each time | 2.34 → 2.24 ms | 2.67 → 2.11 ms | 4.16 → 3.08 ms | 6.75 → 4.48 ms |
| `GET /v1/status` while the ten nodes report back to back, each report changed | 44.8 → 1.52 ms | 318.3 → 7.38 ms | 921.2 → 15.37 ms | 2926.0 → 31.67 ms |
| `GET /v1/status` alone | 1.40 → 1.41 ms | 6.80 → 6.85 ms | 14.17 → 14.32 ms | 29.50 → 29.57 ms |
| `PutApp` of one more app of 5 replicas | 2.72 → 2.77 ms | 4.74 → 4.77 ms | 7.96 → 7.92 ms | 13.29 → 12.89 ms |

`GET /v1/apps` and `/v1/nodes` did not change. A deploy is not faster: a profile of `PutApp` at 2000 replicas put most of its placement time in reading every placed replica's row from SQLite, about 2.5 µs a row, and not in decoding specs, so a deploy of N apps still costs O(N²) in total. Reading the specs apart from the rows, once per app generation, made no difference and was not kept. Keeping the placed replicas in memory across transactions would remove it; that is not done. The floor of about 2.2 ms for a report is the fsync of its commit (`synchronous=FULL`).

## What did not work, and what was not measured

- Goroutines, as above.
- The first run of the server restart and the return of the three workers were sampled by the harness's `collect`, which split the server's reply with a bash pattern removal (`${out#*#nodes}`). That is quadratic in the length of the string and took 24 s for the 115 KB status, so those loops sampled every 23 to 26 s. The figures above for those two come from the probe, the watcher and the journals instead. `collect` now splits with `sed` and takes 0.3 s; the second restart ran with it.
- After the drain, the harness ran `hivemind node drain --no-wait --json` to read the result. On a finished drain that starts a new one, which found nothing to move and finished at once. The result above is the CLI's own output; the harness now reads `GET /v1/nodes/{id}/drain`.
- The rollout loop was written to wait for every rollout to end, which a paused one never does; it was stopped by hand 13 minutes in, and now stops once every rollout left is paused.
- The perf breakdown covers the first 6.4 minutes of the steady state, as above.
- The power-off command's ssh to the machine timed out after 20 s, as it should: the machine is gone. The log has the time the machine stopped answering.

## Running it

On a control machine with ssh access to ten nodes (passwordless sudo, Docker, jq, curl on each), and a linux build of hivemind:

```sh
export SOAK_DIR=$PWD/.scratch/run
# "name private-ip" lines, the server first
printf 'node-1 10.0.0.5\nnode-2 10.0.0.6\n...' >"$SOAK_DIR/nodes"
SETUP_APPS=0 BIN=path/to/hivemind scripts/soak/setup.sh
scripts/soak/scale.sh specs
scripts/soak/scale.sh pull

scripts/soak/scale.sh deploy seq
scripts/soak/scale.sh delete
scripts/soak/scale.sh deploy par 20
scripts/soak/scale.sh kv-write
scripts/soak/scenario.sh probe-start

scripts/soak/scenario.sh mark steady begin
#    ...30 minutes later:
scripts/soak/scenario.sh mark steady end

scripts/soak/scenario.sh watch-start
scripts/soak/scale.sh lose node-8 node-9 node-10 &
#    ...power the three machines off, wait for it to end, then:
scripts/soak/scale.sh return node-8 node-9 node-10 &
#    ...power them on
scripts/soak/scenario.sh watch-stop

scripts/soak/scale.sh rollout 50
scripts/soak/scale.sh drain                 # the worker with the most replicas
scripts/soak/scale.sh lose node-7 &         # ...power it off
scripts/soak/scale.sh remove node-7
scripts/soak/setup.sh rejoin node-7         # after powering it on
scripts/soak/scale.sh scale 2
scripts/soak/scale.sh scale 1
scripts/soak/scale.sh restart-server

scripts/soak/scenario.sh begin lose-server node-1
scripts/soak/scenario.sh server-backup
scripts/soak/scenario.sh server-replace &
#    ...replace the machine with an empty one at the same address

scripts/soak/report.sh "$SOAK_DIR/soak.log"
```

`scale.sh` with no argument lists its commands. Each one logs a `mark` line at its start and end, and `event` lines the first time each condition holds, to the same log as the rest of the harness.
