# Soak test on four machines

One server and three workers, each a separate machine, run three small apps. Then the soak test powers off a worker, reboots one, cuts one off from the server, replaces the server with an empty machine, and leaves the cluster alone for four hours. Every number below comes from the harness's log; nothing is estimated. The harness is in [scripts/soak](../scripts/soak), and [Running it](#running-it) says how to run it. A [second soak](#second-soak-routing-a-private-registry-and-clients-outside-the-cluster), on five machines, measures the same faults and more through the HTTPS routers, from clients outside the cluster.

## Kill a machine, watch it recover

hivemind-soak-3, a worker running replica 2 of `web` and replica 3 of `spread`, was powered off with `sysrq o`, which stops the machine at once, like pulling the plug. Times are UTC on 2026-11-14, from the control machine's clock:

| Time | What happened |
|---|---|
| 08:21:03.3 | Last lease renewal from hivemind-soak-3, as the server recorded it |
| 08:21:06.4 | hivemind-soak-3 stops answering on port 22 |
| 08:21:18.6 | `hivemind nodes` shows it `lost` (lease 15.3 s old) |
| 08:21:19.8 | Its two replicas are placed on other nodes |
| 08:21:20.9 | `spread` replica 3 runs on hivemind-soak-2 |
| 08:21:23.1 | `web` replica 2 runs on hivemind-soak-4, after pulling `nginx:alpine` there |
| 08:21:25.9 | Converged: every replica runs where the server says, nothing is left over |
| 08:27:45.8 | Start request for hivemind-soak-3 sent |
| 08:27:59.9 | hivemind-soak-3 answers on port 22 |
| 08:28:01.0 | It is `ready`, and its two old containers are gone (removed on its first pass); converged |

At no point did a replica run on two machines.

## The machines

| | |
|---|---|
| Machines | 4 EC2 c7g.large in one subnet: 2 vCPU (Graviton3, Neoverse V1), 3.7 GiB of memory (MemTotal 3897240 kB), 30 GB gp3 disk |
| OS | Amazon Linux 2023.12.20261112, kernel 6.18.62-127.179.amzn2023.aarch64 |
| Docker | 25.0.18, from the distribution |
| hivemind | Three builds, each with `CGO_ENABLED=0 go build -trimpath ./cmd/hivemind` for linux/arm64 with Go 1.27.4 (`hivemind version` prints `dev`). Run 1: commit 5595943, for scenarios 1 to 4. Run 2: commit a9f906b ("store: count leases from the server's start"), for scenario 4 again and the steady state. Run 3: commit e80a921, which adds "agent: renew the lease on a timer, apart from the passes" (999840a), for the slow-pass reproduction again. The cluster went from one build to the next by installing the new binary and restarting the units, the server first |
| Roles | hivemind-soak-1 is the server (`serve --advertise <its private address>`), hivemind-soak-2 to 4 are workers |
| Service | systemd, with the units in [systemd.md](systemd.md), as root |
| Control machine | a separate EC2 machine in the same VPC that reaches the nodes over ssh, and runs the harness |

Nothing listened on a public address. The hivemind ports 4484 and 4485 were on the server's private address, the API on 127.0.0.1:4483, and the apps' host ports on 127.0.0.1. The harness checked every node's listeners after setup.

The apps:

| App | Image | Replicas | Port | Notes |
|---|---|---|---|---|
| web | nginx:alpine | 3 | 127.0.0.1:8080 | One per node at most, since the port is the same |
| spread | busybox:stable, `sleep 86400` | 6 | none | |
| kv | redis:7-alpine, `--appendonly yes` | 1 | 127.0.0.1:6379 | Volume `data` at `/data`. A value is written at the start and read back by every probe |

## Method

The harness runs on the control machine and touches the nodes only through ssh. It writes every probe and event as one line to a log, with the control machine's UTC time in milliseconds (`date +%s%3N`; the machine's clock was synchronised by chrony). The numbers here are differences between log lines.

- Every 10 s, the probe loop records: whether `hivemind status --json` answers on the server; the fault suite's convergence check (`scripts/lib/converged.awk`) on that status and the containers `docker inspect` reports on every node; how many replicas run of those that should; any (app, replica) with a running container on two nodes; and, on each node that the server assigns a `web` or `kv` replica, a `curl` of 127.0.0.1:8080 or a read of the kv value from 127.0.0.1:6379, run on that node. While the API is down it uses the last assignment it saw.
- Every 60 s: for each node's hivemind process (`systemctl show -p MainPID`), the RSS and thread count from `/proc/<pid>/status`, the CPU time (utime + stime from `/proc/<pid>/stat`), the number of open file descriptors, and, on the server, the sizes of `hivemind.db`, `hivemind.db-wal` and `hivemind.db-shm`; then five `GET /v1/status` requests with curl on the server, timed by curl's `time_total`.
- During the fault scenarios, a watcher polls once a second: a new TCP connection to each machine's port 22, `GET /healthz`, `hivemind nodes` and `hivemind status`, and logs each change. Each scenario also runs its own loop, also once a second, which logs the first time each condition holds: node lost, replicas running elsewhere, old containers gone, converged.

"Converged" is the fault suite's definition: every replica runs on the node the server assigns it, with that container running, or is pending with a placement reason and no node; the server's status names the same containers the engines report; no container is left over; no (app, replica) is on two nodes. A replica that is pending with a reason counts, so converged does not mean that every replica runs. From scenario 4 on, the probes also count the replicas that run.

The resolution of a time is the interval of the loop that saw it: 10 s for the probe loop, and about 1 s for the watcher and the scenario loops. In the loss phase of scenario 1, a scenario-loop poll waited for the dead machine's ssh connection to time out (3 s then, 1 s from scenario 2 on), so its polls were up to 4 s apart. A fault started from the operator's machine (power off, power on, replace) is marked in the log just before the request; the request itself takes a few seconds to reach EC2, so the tables also give the time the machine stopped or started answering on port 22. Times in a node's own journal are from that node's clock, which can be off by a few hundred milliseconds right after a cold boot; the tables use them only where the log has nothing finer, and say so.

## Results

### 1. Power off a worker

Target: hivemind-soak-3, with `web` replica 2 and `spread` replica 3. The fault is `sysrq o` on the machine (the soak machines' `poweroff` command).

| Measure | Seconds |
|---|---|
| Power-off request sent to port 22 closed | 5.5 |
| Last lease to `lost` in `hivemind nodes` | 15.3 |
| Port 22 closed to `lost` | 12.2 |
| `lost` to `spread` replica 3 running on hivemind-soak-2 | 2.2 |
| `lost` to `web` replica 2 running on hivemind-soak-4 (image pulled there first) | 4.4 |
| Port 22 closed to converged | 19.4 (10 s probe loop); 20.4 (scenario loop) |
| Start request sent to port 22 open | 14.0 |
| Start request sent to `ready` | 15.1 |
| `ready` to its two old containers gone | 0.0 (same poll) |
| Start request sent to converged | 15.1 |
| Replicas on two machines at any time | none |

The returning worker's journal shows both old containers removed 15 ms and 26 ms after its agent started.

### 2. Reboot a worker

Target: hivemind-soak-4, with `web` replica 2 and `spread` replicas 0 and 4. The fault is `sudo systemctl reboot`.

| Measure | Seconds after the reboot command |
|---|---|
| Port 22 closed | 0.3 |
| `lost` | 13.5 |
| Port 22 open | 16.7 |
| Its agent starts (worker's journal) | 18.0 |
| The server releases its three replicas (server's journal) | 17.9 |
| `ready` | 18.8 |
| Its three old containers removed (worker's journal) | 18.0 |
| Converged; the three replicas run on hivemind-soak-3 | 20.5 |
| Replicas on two machines at any time | none |

The reboot took 12 s, and the lease runs out after 15 s, so the node was lost for a moment and all three replicas moved, although the machine was back 0.1 s after the server released them.

### 3. Partition a worker

Target: hivemind-soak-2, with `kv` replica 0, `web` replica 1 and `spread` replicas 2 and 3. The fault is `iptables -I OUTPUT -p tcp -d <server> --dport 4484 -j DROP` on the worker for 60 s, then the rule is deleted.

| Measure | Seconds |
|---|---|
| Partition to `lost` | 14.2 |
| Partition to `web` 1 and `spread` 2 and 3 running on hivemind-soak-4 | 15.2 |
| Partition to the first replica seen on two machines | 14.6 |
| Heal to `ready` | 22.3 |
| Heal to no replica on two machines | 22.3 |
| Heal to converged | 22.3 |
| Time with a replica on two machines | 67 |
| `kv` | stayed on hivemind-soak-2 and kept running; `status` showed it waiting for its node; the value was intact after the heal |

The duplicates are the documented case: hivemind cannot stop containers on a machine it cannot reach (see [concepts](concepts.md#node-return-and-generation-fencing)). The 22 s after the heal are not network time. The worker's first pass after the heal removed its three moved containers one after another: the two `spread` containers took 10.2 s each, because `sleep` as PID 1 ignores SIGTERM and Docker waits its 10 s before SIGKILL, and `web` 0.3 s. On that build the lease was renewed only at the end of a pass, so the node stayed `lost` for those 21 s. That led to the finding below.

### 4. Lose the server machine

The server's data directory was backed up with `hivemind backup` (a 130560-byte tar: manifest, store, CA key and certificate, admin token) and copied to the control machine. Then the server's machine was terminated and a new, empty one launched at the same private address. As soon as the new machine answered on port 22 and its user data had installed Docker, the harness installed the binary, ran `hivemind restore`, and started the server's unit. The workers were not touched.

This ran twice: on run 1's build, and again on run 2's, which counts each lease from the server's start so that a restored server gives every node 15 s to report before its first sweep can release it. Both times the server ran `web` replica 0 and all six `spread` replicas; hivemind-soak-2 ran `web` 1 and `kv`, and hivemind-soak-3 ran `web` 2. Times are from the scenario loop (about 1 s resolution).

| Measure | Run 1 (5595943) | Run 2 (a9f906b) |
|---|---|---|
| Replace request sent to API down | 6.0 s | 7.0 s |
| API down to new machine ready (EC2 launch, boot, Docker installed by user data) | 103.9 s | 103.8 s |
| Restore | 0.5 s | 0.6 s |
| Restore done to unit started | 0.6 s | 0.6 s |
| Unit started to API up | 0.8 s | 0.8 s |
| API down to API up | 105.9 s | 105.8 s |
| API up to each worker `ready`, without joining again | 0.2, 0.2, 1.4 s | 1.4, 1.5, 1.5 s |
| API up to converged; the server's seven replicas run again on the new machine | 6.2 s | 6.2 s |
| `web` on the two workers, `curl` once a second while the server was gone | 188 of 188 answered | 184 of 184 answered |
| Containers on the workers, before and after | unchanged: same IDs and start times, none added | unchanged |
| Workers marked lost by the new server | none | none (no "node lost" line in its journal) |
| `kv` value (on hivemind-soak-2) | intact | intact |

In both runs every worker reported within 1.5 s of the API coming up, before the first lease sweep, which runs 5 s after `serve` starts. So the race that run 2's build closes did not happen in run 1 either, and these numbers show no difference between the builds. The soak had no worker that took longer than 5 s to report, so it did not exercise the fix.

Two things were wrong for a few seconds in both runs. Right after the restore, `hivemind status` reported the server's own replicas as running, with the old machine's container IDs, until its agent had created them again (6 s): the restored store holds the states reported before the backup. The convergence check counts them as not converged, since no engine has such containers, but the running count in `status` was 10 of 10. And `hivemind nodes` showed the workers as `lost`, with a lease age of 115 to 117 s, until each reported: two of the three in run 1, all three in run 2. In run 2 the server would not have released them for 15 s, so the node list disagrees with the sweep in that window.

### Upgrades between the runs

Between scenario 4 of run 1 and run 2, every node got run 2's binary (installed beside the running one and renamed over it), then `systemctl restart hivemind`, the server first. The API answered 0.15 s after the server's restart command returned, the cluster had converged 0.5 s after the last worker restarted, and every replica was on the node it was on before. The same step took the cluster to run 3's build at 12:58, after the steady state, with the same figures (0.15 s, 0.5 s, nothing moved).

### 5. Steady state

On run 2's build (a9f906b), 4 h 0 min 31 s, from 08:56:31.8 to 12:57:02.4, with no fault and no change to the apps. Only the probe loop ran; the watcher was off. So the load on hivemind was one `hivemind status` every 10 s, and every 60 s five `GET /v1/status` and one `GET /metrics`, besides the agents' own passes every 2 s.

The placement in the window: hivemind-soak-2 ran `kv`, `web` 1 and `spread` 2 and 5; hivemind-soak-3 `web` 2 and `spread` 1 and 4; hivemind-soak-4 `web` 0 and `spread` 0 and 3; the server ran none, since the second slow-pass reproduction (below) had moved its replicas to the workers.

| Probe | Result |
|---|---|
| Status answered, converged, all 10 replicas running, no replica on two machines | 1443 of 1443 |
| `web` answered on its node | 4329 of 4329 |
| `kv` value read back, and the same as written | 1443 of 1443 |
| `GET /v1/status` on the server, 1205 requests | all 200; p50 1.5 ms, p99 7.5 ms, max 16.8 ms |
| Events: lost nodes, restarts, failed probes | none |

The hivemind process on each node, 241 samples a minute apart:

| Node | RSS first / last / max (MiB) | RSS, hourly mean, hours 1 to 4 (MiB) | RSS slope, least squares (KiB/h) | Threads first / last | Open fds first / last / max | CPU, % of one CPU | Restarts |
|---|---|---|---|---|---|---|---|
| hivemind-soak-1 (server) | 22.1 / 22.7 / 22.8 | 22.3, 22.7, 22.6, 22.4 | +19 | 8 / 9 | 19 / 20 / 21 | 0.30 | 0 |
| hivemind-soak-2 | 18.4 / 18.8 / 19.1 | 18.7, 18.8, 18.8, 18.8 | +37 | 8 / 8 | 9 / 9 / 9 | 0.05 | 0 |
| hivemind-soak-3 | 18.4 / 18.8 / 19.0 | 18.7, 18.8, 18.8, 18.8 | +58 | 8 / 8 | 9 / 9 / 9 | 0.05 | 0 |
| hivemind-soak-4 | 18.2 / 19.0 / 19.0 | 18.5, 18.7, 18.8, 18.8 | +117 | 8 / 8 | 9 / 9 / 9 | 0.04 | 0 |

The workers' RSS rose 0.1 to 0.3 MiB in the first hour and by at most 0.05 MiB in each hour after; the server's ended within 0.6 MiB of where it began, with no upward trend in the hourly means. Four hours show no leak at this resolution; a slower one would need a longer run. The store was 124 KiB (`hivemind.db`), with a 4036 KiB WAL and a 32 KiB shared-memory file, all three the same size at the start and the end. The server's journal got 2894 lines in the window, one per API request at `info`; the workers' got none.

## Finding: a reconcile pass longer than 15 s made a node lost

Fixed in 999840a, which run 3's build has. Before it, an agent renewed its node's lease only when a pass reported, at the end of the pass. A pass removes the containers it no longer wants one at a time, each with `docker stop` and a 10 s grace period, and it pulls missing images inline. A container whose PID 1 ignores SIGTERM takes the full 10 s. So a node with two or more such containers to remove spent 20 s or more in one pass without renewing its lease, and the server marked it lost, although it was up and reachable. Its replicas were then placed elsewhere, and the moves caused more removals, which could lapse the lease again. Since 999840a the agent also renews the lease every 5 s on its own timer.

[scripts/soak/repro-slow-pass.sh](../scripts/soak/repro-slow-pass.sh) reproduces it: it deploys `slowstop` (busybox `sleep`, three replicas per node when every node is eligible), deletes it, and watches the node list for 60 s. It exits 1 if a node was marked lost. It ran once on each build.

Run 1, with `slowstop` placed three per node:

| Seconds after `hivemind delete slowstop` | |
|---|---|
| 13.6 to 15.5 | all four nodes, the server's own node included, show `lost` in `hivemind nodes` |
| 17.9 | one sweep releases all four; every replica of every app is pending with `no node is ready` |
| 30.5 | the server's node has reported again; the six `spread` replicas and `web` 0 are placed on it; `web` 1 and 2 stay pending, since port 8080 is taken there |
| 31.5 to 32.5 | the workers are `ready` again |
| 46.6 to 47.9 | hivemind-soak-3 and 4 are `lost` again, while they remove the `spread` and `web` containers that moved off them |
| 52.4 | both are `ready` again |
| 82.7 | every replica runs again; `web` 2 last, on hivemind-soak-3, once that node had been ready for the 30 s of `--min-ready` |

Run 2. The workers had been ready for 29 s, less than `--min-ready`, so all twelve `slowstop` replicas went to the server:

| Seconds after `hivemind delete slowstop` | |
|---|---|
| 13.7 | the server's own node shows `lost` |
| 15.4 | the sweep releases it; its seven replicas (`spread` 0 to 5, `web` 0) are placed on the workers, and run there 1.7 s later |
| 17 to 184 | those seven replicas also keep running on the server, whose agent is still in the pass: seven duplicates |
| 122.3 | the server's pass has removed the twelfth `slowstop` container; the node is `ready` |
| 137.4 | `lost` again, while its next pass removes its seven old copies, 10 s each for `spread` |
| 184.5 | `ready`; no duplicate is left |
| 186.7 | converged |

Run 3 (e80a921), `slowstop` placed three per node:

| Seconds after `hivemind delete slowstop` | |
|---|---|
| 0 to 61 | no node changed state in `hivemind nodes`; the server's journal has no "node lost" line |
| 10.2, 20.4, 30.6 | each node removed one of its three containers (the four nodes' journals agree within 1 s) |
| 38.4 at the latest | converged again (10 s probe); no other replica moved or ran twice |

The script exited 0.

| | Run 1 (5595943) | Run 2 (a9f906b) | Run 3 (e80a921) |
|---|---|---|---|
| `slowstop` placement | 3 per node | 12 on the server | 3 per node |
| Nodes marked lost | all 4, then 2 of them again | the server, twice | none |
| Other apps' replicas released by a sweep (of 10) | 9; `kv` waited for its node | 7, all the server had | none |
| Replicas on two machines | up to 4, in two probes 10 s apart | 7, for 167 s | none |
| Delete to every replica running and converged | 82.7 s | 186.7 s | 38.4 s at the latest; nothing else had stopped |
| Script exit | 1 | 1 | 0 |

Before the fix, one `hivemind delete` of an app whose containers ignore SIGTERM could make every node, the server's included, lose its replicas, and leave replicas running twice for minutes on machines that were up. It also slowed scenario 3: 20.4 s of the 22.3 s from the heal to `ready` were two 10 s stop timeouts in one pass. Scenario 3 was not run again on run 3's build, so that number has no after figure.

## What did not work, and what was not measured

- Goroutines: `/metrics` has no `go_goroutines`, so the number of goroutines was not measured. The thread count of each process is in the resource table.
- The soak machines' `poweroff` command did not return: its ssh to the machine waits for an answer that never comes, since the machine is gone. The harness's log has the time the machine stopped answering; the command was ended by hand.
- The power-off, start and replace requests were made from the operator's machine through the EC2 API, so their marks in the log are a few seconds before the machine changed state. The tables give the port 22 times as well.
- The steady state ran with no replica on the server (see its section): placed replicas never move, and the second reproduction had moved the server's replicas to the workers. It was left as it was, since rebalancing meant deleting `spread`, which on that build triggers the finding again.

## Running it

On a control machine with ssh access to every node (passwordless sudo, Docker, jq, curl, iptables on each), and a linux build of hivemind:

```sh
export SOAK_DIR=$PWD/.scratch/run
mkdir -p "$SOAK_DIR"
printf 'node-1 10.0.0.5\nnode-2 10.0.0.6\nnode-3 10.0.0.7\nnode-4 10.0.0.8\n' >"$SOAK_DIR/nodes"   # the first is the server
BIN=path/to/hivemind scripts/soak/setup.sh
scripts/soak/scenario.sh probe-start
scripts/soak/scenario.sh watch-start

# 1. power off a worker
scripts/soak/scenario.sh begin kill-worker node-3
scripts/soak/scenario.sh measure kill-worker node-3 loss &
#    ...power the machine off, then:
scripts/soak/scenario.sh measure kill-worker node-3 return &
#    ...power it on, wait for the measure to end, then:
scripts/soak/scenario.sh end kill-worker

# 2. and 3.
scripts/soak/scenario.sh reboot node-4
scripts/soak/scenario.sh partition node-2 60

# 4. lose the server
scripts/soak/scenario.sh begin lose-server node-1
scripts/soak/scenario.sh server-backup
scripts/soak/scenario.sh server-replace &
#    ...replace the machine with an empty one at the same address

# 5. steady state
scripts/soak/scenario.sh watch-stop
scripts/soak/scenario.sh mark steady begin
#    ...hours later:
scripts/soak/scenario.sh mark steady end

scripts/soak/report.sh "$SOAK_DIR/soak.log"
```

`report.sh` prints the durations, the probe results of each window and the resource figures of the steady window. The labels of its durations name the soak's own machines; edit the list at the top of the script for other names.

# Second soak: routing, a private registry and clients outside the cluster

The same four kinds of fault as above, and a drain, a node replaced by a new machine, a rolling update, an upgrade and three hours of steady state, on 2026-11-16, measured through the routers. Every node of the cluster ran a router. A fifth machine played the internet: it ran the ACME CA (Pebble), its DNS, a private registry, and clients that sent a request every 100 ms to each of two routed hosts through each of the four routers, eight streams in all. Times below are UTC.

## Through the routers

Failed requests per stream, and the longest time between two successful requests on a stream. "Other routers" are the six streams through the routers on the machines the fault did not touch; "its router" the two streams through the router on the machine it did.

| Fault | Other routers: failed per stream | Other routers: longest gap | Its router |
|---|---|---|---|
| Power off a worker | 1 (a request on a kept-alive connection to the dead machine, held until the client gave up at 10 s) | 10.1 s | down from the power-off to 2 s after the machine was back: 88 s |
| Reboot a worker | 0 | 2.1 s | down 18.3 s |
| Partition a worker from the server for 60 s | 0 | 0.11 s | 0 failed: it served its last table |
| Lose the server machine, restore it on a new one | 10 in one second on two of the three workers' routers, 0 on the third (`502` while the restored server's first table held only stale endpoints) | 2.1 s | down 107.6 s while the machine was replaced, then the same 10 `502`s |
| Rolling update of `www` | 0 | 0.13 s | - |
| Drain a worker, enable it again | 0 | 0.11 s | 0 |
| Drain and remove a worker | 0 | 0.11 s | down from the removal on |
| Join a new machine in its place | 0 | 0.13 s | 40 refused over 4.1 s while `join` handed over to the unit, then served |
| Upgrade every node in place, all within a second | every router: 30 to 47, of them 29 to 40 `503`s in 3 to 4 s, the rest its own restart | 4.1 s | (every router restarted) |
| Restart the server's service only | 0 | 0.11 s | refused for 1.2 s |
| Restart one worker's service only | 0 | 0.11 s | refused for 0.2 s |

Before a fault the cluster had converged and every stream was answering. "Longest gap" includes the slow requests: a request that a router first sent to a replica on a machine that had gone took 2.0 s, the router's dial timeout, and then succeeded on another replica.

## The machines and the builds

| | |
|---|---|
| Machines | 5 EC2 c7g.large in one subnet, the same type, OS and Docker as the first soak: 2 vCPU (Graviton3), 3.7 GiB (MemTotal 3897240 kB), Amazon Linux 2023.12.20261112, kernel 6.18.62-127.179.amzn2023.aarch64, Docker 25.0.18 |
| Roles | hivemind-soakb-1 server, 2 to 4 workers, 5 the outside machine. Machines 1 (the server loss) and 4 (the node replacement) were replaced by new machines at the same address during the run |
| hivemind | `CGO_ENABLED=0 go build -trimpath ./cmd/hivemind`, Go 1.27.4, linux/arm64. Commit 9597d90 (sha256 7eeba927...5a5ba) for the set-up and every fault scenario. Commit b4b7102 ("status, metrics: a lost node's last report is not the replica's state"; sha256 1ab140dd...52b72) from 07:38: the upgrade, the two restarts, the steady state and the slow-pass check. The cluster went from one to the other with the harness's upgrade step, server first |
| Service | systemd, the units of [systemd.md](systemd.md) with the router's flags added |
| Outside machine | Pebble (ghcr.io/letsencrypt/pebble, revision 5caaab3), pebble-challtestsrv, registry:2, curl 8.21.0 |
| Network | The soak security group allowed TCP 22 from the control machine, and between the soak machines only TCP 80, 443, 4484 to 4485, 5000, 14000 and 32768 to 60999 (the routed containers' ports; see "What did not work"). Nothing from the internet |

## Set-up

Every node ran its router on its own private address: `--route-addr <address> --acme-email ops@soak.test --acme-ca https://pebble:14000/dir`, on ports 80 and 443. The server's unit had them after `serve --advertise <address>`, each worker's after `serve --role worker`, and `join` had them too. Before hivemind, each node got three things an operator would set up: a line in `/etc/hosts` naming the outside machine `pebble` and `registry.soak.test`; Pebble's TLS root in the system trust store (`/etc/pki/ca-trust/source/anchors`, then `update-ca-trust`), which the routers' ACME client uses; and the registry's CA in `/etc/docker/certs.d/registry.soak.test/ca.crt`.

Pebble validated HTTP-01 on port 80, with its validation sleeps off (`PEBBLE_VA_NOSLEEP=1`), no nonce rejection, and no reuse of authorizations (`PEBBLE_AUTHZREUSE=0`), so every order was validated again through a router. Its default profile issued certificates valid for one hour, so that the routers renewed them during the run. pebble-challtestsrv answered `www.soak.test` and `api.soak.test` with the private addresses of all four nodes. The registry was `registry:2` with htpasswd and a TLS certificate from a CA made for the run, on port 443 of the outside machine; it held `registry.soak.test/soak/whoami:1` and `:2` (traefik/whoami v1.10.3 and v1.11.0). `hivemind registry login registry.soak.test` stored the credential on the server, from standard input.

| App | Image | Replicas | Routes or ports | Notes |
|---|---|---|---|---|
| www | registry.soak.test/soak/whoami:1, then :2 | 3 | `www.soak.test`, container port 80 | private image; HTTP health check on `/health` every 2 s |
| api | nginx:alpine | 2 | `api.soak.test`, container port 80 | HTTP health check on `/` every 2 s |
| spread | busybox:stable, `sleep 86400` | 6 | none | |
| kv | redis:7-alpine, `--appendonly yes` | 1 | 127.0.0.1:6379 | volume `data`; a value written at the start and read back by every probe |
| front, cache | nginx:alpine, redis:7-alpine | 1 each | 127.0.0.1:8081 for front | the compose project `shop` ([scripts/soak/compose-shop.yaml](../scripts/soak/compose-shop.yaml)), deployed with `hivemind compose deploy` |

The harness checked every listener after the set-up. hivemind's ports were where they were in the first soak; the routers were on each node's private address, ports 80 and 443, and each routed container on an ephemeral port of its node's private address. On the outside machine, Pebble's ACME port and the registry were on its private address, Pebble's management port and challtestsrv's on 127.0.0.1. Nothing else listened on a wildcard address but sshd.

The first certificates: the deploy was at 06:55:44. `www.soak.test` was issued 6 s later; the other three routers waited for the storage lock and loaded it from the store the same second. The first challenge for `api.soak.test` failed: Pebble's validation reached a router that had not yet read the table with the new host, and it answered `404`. certmagic tried again 60 s later, and that worked. Every router served both hosts 66 s after the deploy.

## Method

The clients ran on the outside machine: for each router and host, one `curl --http1.1 --rate 10/s` process with `Connection: close` and no TLS session reuse, so each request was a new TCP connection and a full TLS handshake to that router, with `--resolve` naming the router's address, 3 s to connect and 10 s in all. Each request was one line with its end time (curl's `%time{}`, the outside machine's clock), its status, curl's exit code and its timings. A request failed if it was not a `200` or curl reported an error. A certificate watcher on the same machine logged, every 30 s, the serial and expiry of the certificate each router served for each host, from a fresh handshake checked against Pebble's root.

The harness on the control machine was the first soak's: the 10 s probe loop, the 1 s watcher during the faults, and each scenario's own 1 s loop, all to one log with the control machine's clock. Both machines' clocks were synchronised by chrony to the Amazon Time Sync Service; at the start chrony put each within 1 microsecond of its reference. A scenario's window for the clients runs from its first log line to 60 s after its last, or to the next scenario's start. [scripts/soak/client-report.sh](../scripts/soak/client-report.sh) computes the tables from the clients' lines and the log.

## Results

### Power off a worker

hivemind-soakb-3, running `www` 0, `api` 1, `cache` 0 and `spread` 3, powered off with `sysrq o` at about 07:04:53 (its last lease renewal; the request went out at 07:04:48.3).

| Measure | Seconds |
|---|---|
| Power-off request to port 22 closed | 6.2 |
| Port 22 closed to `lost` | 13.5 |
| `lost` to the sweep releasing its replicas (server's journal) | 3.2 |
| Released to `www` 0, `cache` 0 and `spread` 3 running on hivemind-soakb-2 | 2.4 |
| Released to `api` 1 running on the server, after pulling nginx there | 5.8 |
| Port 22 closed to converged | 24.1 |
| Start request to port 22 open | 12.6 |
| Start request to `ready`, its four old containers gone | 14.7 |
| Replicas on two machines at any time | none |

Through the routers: each of the six streams through the other three routers lost one request. It started between 07:04:53.3 and 07:04:53.6, as the machine died, and got no reply: the router had sent it on a connection it already had open to the replica on that machine (it keeps them for reuse), and with the machine gone nothing answered or reset it. curl gave up at 10 s. The server's router counted one `502` for each host afterwards (`hivemind_router_requests_total`); a router waits up to 60 s for a replica's reply. Every later request that a router sent to the dead machine's replicas waited out the 2 s dial timeout and then went to another replica, so those requests took 2.0 s, until the replicas left the table when the node was lost. The two streams through hivemind-soakb-3's own router failed from the power-off until 2 s after the machine answered on port 22 again, 88 s, with connect timeouts and then refused connections, and one failed TLS handshake right after the router started: it opens its listeners before it has loaded the certificates from the server.

### Reboot a worker

hivemind-soakb-4, running `www` 1, `front` 0 and `spread` 0 and 4, with `sudo systemctl reboot`.

| Measure | Seconds after the reboot command |
|---|---|
| Port 22 closed | 0.3 |
| `lost` | 13.5 |
| Port 22 open | 16.9 |
| Its four replicas running on hivemind-soakb-3; converged | 18.3 |
| `ready` | 19.0 |

No request through the other routers failed. The `www` streams' slowest requests took 2.0 s, the dials to the rebooting machine. The streams through hivemind-soakb-4's own router failed for 18.3 s. As in the first soak, a reboot takes longer than the 15 s lease, so the node was lost and its replicas moved.

### Partition a worker

hivemind-soakb-2, running `kv`, `api` 0, `cache` 0, `www` 0 and `spread` 2 and 3, with `iptables -I OUTPUT -p tcp -d <server> --dport 4484 -j DROP` for 60 s.

| Measure | Seconds |
|---|---|
| Partition to `lost` | 14.4 |
| Partition to the first replica on two machines | 16.4 |
| Partition to its five replicas without a volume running on hivemind-soakb-4 | 20.2 |
| Heal to `ready` | 1.4 |
| Heal to no replica on two machines; converged | 22.3 |
| `kv` | stayed on hivemind-soakb-2, held with its reason; the value read back intact after the heal |

No request failed on any of the eight streams, and none took longer than 13 ms. The partitioned node's router could not read the table, and served the last one it had read; the replicas in it kept running on every machine, its own included, until the heal. On this build the node is `ready` 1.4 s after the heal; on the first soak's first build that took 22.3 s, because the lease waited for the pass. Convergence still waits for the pass that removes the moved copies: two `spread` containers, 10 s each, since `sleep` ignores SIGTERM.

### Lose the server machine

The server's data directory was backed up with `hivemind backup` (a 200192-byte tar: manifest, store, CA key and certificate, admin token), its machine terminated, and a new, empty one launched at the same address. The harness then installed the binary, set up the hosts file and the two CAs as on every node, ran `hivemind restore` and started the unit. The workers were not touched. The server ran `www` 2, `api` 1 and `spread` 1 and 5.

| Measure | Seconds |
|---|---|
| Replace request to API down | 7.0 |
| API down to the new machine ready (EC2 launch, boot, Docker installed) | 103.7 |
| Machine ready to restored (binary, hosts, CAs, restore) | 1.6 |
| Restored to unit started | 0.5 |
| Unit started to API up | 0.9 |
| API down to API up | 106.6 |
| API up to each worker `ready`, without joining again | 0.2, 1.2, 1.2 |
| API up to converged, the server's four replicas running again on the new machine | 4.2 |
| Containers on the workers before and after | unchanged |

While the server was gone, the workers' routers served their last table. Their only slow requests were those sent to the server's replicas first: 2.0 s each, then another replica. In the second after the restored server started (07:17:01.0 to 07:17:01.9), the streams through the restored server's router and through two of the three workers' routers got ten `502`s each; the third worker's router's streams got none. The restored store's routing table holds the containers each node had reported before the backup, and counts only nodes whose lease is current. Right after the start that is the server's own node alone, whose old containers do not exist on the new machine: every endpoint in the table refused the connection. The workers' replicas came back into the table when the workers reported, within the second.

### Rolling update

`www` from `whoami:1` to `whoami:2`, both from the private registry.

| Measure | Seconds |
|---|---|
| Deploy to the first replica moved | 7.1 |
| Deploy to the rollout done (three replicas, one at a time) | 17.2 |
| Deploy to converged (old copies stopped) | 24.2 |

No request failed on any stream, and the slowest took 29 ms.

### Drain and enable a worker; remove a node and join a new machine

`hivemind node drain` of hivemind-soakb-3 moved its four replicas (`front` 0, `spread` 0 and 4, `www` 2) in 26.1 s, and the cluster had converged when the command returned. `hivemind node eligibility --enable` then made the node eligible again; nothing moved back to it. No request failed on any stream.

hivemind-soakb-4 was drained the same way (five replicas, 26.1 s), then `hivemind node remove` removed it in 0.3 s. Its agent got its refusal and stopped 1.1 s later, with exit status 1, which systemd reports as a failure, and nothing was left running on it. `Restart=on-failure` then starts it again every 5 s, refused each time, until the unit is disabled; [systemd.md](systemd.md) says to disable it on a removed machine. (A second removal at the end of the run, of the new machine, counted eight such restarts in 45 s.) Its machine was then terminated and a new one launched at the same address. The harness installed the binary, set up the hosts file and CAs, and joined it with a new token and the router's flags: 0.8 s from the join to `ready`, as a new node with a new ID. No request through the other routers failed. Through the new machine's router, 40 requests over 4.1 s were refused while the foreground `join`'s router stopped and the unit's started, as [systemd.md](systemd.md) has it; after that it served.

### Upgrade, and restarts

The harness's upgrade step installed b4b7102 on every node and restarted the services, the server first. The API was back 0.7 s after the server's restart, and the three workers restarted within the next 0.2 s. Every stream then got `503`s for 3 to 4 s (29 to 40 each). Each router also refused a request or a few during its own restart, the server's for 0.6 s, and two failed one handshake.

The `503`s come from the agents' restarts, not the server's. An agent that starts does not know yet whether its containers pass their health checks, so the server's routing table drops its routed replicas until each passes its first check again, although the containers never stopped. A `GET /v1/routes` every 100 ms on the server, while the service on hivemind-soakb-3 alone was restarted, showed its two endpoints leave the table for 4.0 s. With every node restarted at once, every host had no endpoint left. Restarting one node's service alone failed nothing through the other routers: the server's alone failed only requests through the server's own router, refused for 1.2 s, and one worker's alone only those through that worker's router, for 0.2 s. A full upgrade one node at a time was not run. [upgrading.md](upgrading.md#routed-apps) now says to restart one node at a time.

### Certificates

Pebble issued 52 certificates for the two hosts over the run, 41 of them in the three hours of steady state, each valid for one hour. certmagic renews a certificate once less than 50 minutes of it are left (five times its 10-minute check interval), so with one-hour certificates each host was renewed every 10 to 20 minutes, and every router that started, in the faults and the upgrade, renewed both hosts at once. Each renewal was done by one router under the storage lock; four times a router renewed a host again 1 to 3 minutes after another had. The others found the new certificate in the store at their next check and logged `certificate expires soon, but is already renewed in storage; reloading stored certificate`; until then they served the previous one, still valid. The watcher's fresh handshakes, every 30 s with every router for both hosts (3560 in all), always got a certificate that verified against Pebble's root, except while a router was down in a fault (32). The least validity left on any certificate a router served was 31 minutes. No request failed across any renewal.

## Steady state

On b4b7102, 3 h 0 min 34 s, from 07:44:20.7 to 10:44:55.0, with no fault and no change to the apps. The clients ran throughout, eight streams of 10 requests a second, and the probe loop; the watcher was off. Placement in the window: hivemind-soakb-1 (the server) ran `www` 2, `api` 1 and `spread` 1 and 5; hivemind-soakb-2 `kv`, `front`, `www` 0 and `spread` 3; hivemind-soakb-3 `api` 0, `cache`, `www` 1 and `spread` 2; hivemind-soakb-4, the machine that joined at 07:29, `spread` 0 and 4, so its router reached every replica over the network.

| Through the routers | |
|---|---|
| Requests, all eight streams | 860918 |
| Succeeded | 860918 (100 %) |
| Longest gap between two successful requests on a stream | 1.13 s (one request whose TCP connection took 1.03 s, a SYN sent again after 1 s); every other stream at most 0.32 s |
| Total time per request (TCP connect, TLS handshake, request and reply), p50 / p90 / p99 / p99.9 / max | 2.4 / 2.9 / 3.6 / 5.5 / 1027.7 ms |
| TLS handshake, p50 / p99 / max | 1.5 / 2.5 / 224.2 ms |
| TLS done to first byte of the reply (the router and the replica), p50 / p99 / max | 0.6 / 1.1 / 11.8 ms |
| Requests over 100 ms | 2 of 860918, both through hivemind-soakb-3's router |

| Probe | Result |
|---|---|
| Status answered, converged, all 14 replicas running, no replica on two machines | 1084 of 1084 |
| `kv` value read back, and the same as written | 1084 of 1084 |
| `GET /v1/status` on the server, 905 requests | all 200; p50 1.7 ms, p99 7.2 ms, max 15.6 ms |
| Events: lost nodes, restarts, failed probes, duplicates | none |

The hivemind process on each node, 181 samples a minute apart:

| Node | RSS first / last / max (MiB) | RSS, hourly mean, hours 1 to 3 (MiB) | RSS slope, least squares (KiB/h) | Threads first / last | Open fds first / last / max | CPU, % of one CPU | Restarts |
|---|---|---|---|---|---|---|---|
| hivemind-soakb-1 (server) | 25.6 / 26.2 / 26.5 | 26.0, 26.1, 26.1 | +109 | 9 / 9 | 31 / 31 / 33 | 2.20 | 0 |
| hivemind-soakb-2 | 22.0 / 22.8 / 23.0 | 22.3, 22.7, 22.8 | +218 | 8 / 9 | 19 / 19 / 21 | 1.73 | 0 |
| hivemind-soakb-3 | 21.8 / 22.6 / 23.1 | 22.1, 22.6, 22.7 | +327 | 8 / 8 | 18 / 19 / 21 | 1.75 | 0 |
| hivemind-soakb-4 | 21.8 / 22.4 / 22.8 | 22.3, 22.4, 22.5 | +120 | 8 / 8 | 18 / 19 / 21 | 1.52 | 0 |

The workers' RSS rose 0.6 to 0.8 MiB over the three hours, most of it in the first two, and by at most 0.1 MiB in the third hour; the server's by 0.6 MiB, and 0.1 MiB after the first hour. Three hours show no leak at this resolution; a slower one would need a longer run. Each process used 1.5 to 2.2 % of one CPU, against 0.05 to 0.30 % in the first soak: each router here did about 20 TLS handshakes and proxied requests a second. The store went from 192 to 208 KiB (`hivemind.db`), with a 4032 to 4036 KiB WAL.

## What did not work, and what was not measured

- The soak security group first allowed only TCP 80, 443, 4484 to 4485, 5000 and 14000 between the machines. A probe from one machine to a listener on another showed the routed containers' range, 32768 to 60999, closed; with it closed a router can reach only the replicas on its own node. The range was opened (from the group itself) before the cluster was set up, and [routing.md](routing.md#ports-and-firewall) now says that the nodes must allow it from each other.
- The clients' timestamps are the outside machine's clock and the harness's the control machine's. Both were synchronised by chrony; differences between them below a few milliseconds are not meaningful.
- curl writes its lines in 4 KiB blocks, so a log read while curl runs ends in a partial line; the report skips such lines and counts them.
- The 1 s watcher ran during the faults and was stopped for the steady state, as in the first soak; the probe loop ran throughout.
- `/metrics` exists only on the server, so the routers' `502` counts are the server's router's; the workers' routers export none.
- One-hour certificates are much shorter than any public CA's. certmagic renews a certificate once less than five times its 10-minute check interval is left, so here every router renewed at its start and each host was renewed every 10 to 20 minutes. That exercises renewal far more often than 90-day or 6-day certificates would, but it is not how a production CA behaves.
- The scenarios ran on 9597d90, the steady state on b4b7102. b4b7102 changes what status and the metrics report for a lost node's replicas, not routing or the agents, so the routing results above should not depend on it; they were not run again on it.

## Running it, with routing

On a control machine with ssh to the four nodes and the outside machine (passwordless sudo, Docker, jq, curl 8.6 or later and openssl on each), gawk on the control machine, and a linux build of hivemind:

```sh
export SOAK_DIR=$PWD/.scratch/run SOAK_ROUTES=1
printf 'node-1 10.0.0.5\nnode-2 10.0.0.6\nnode-3 10.0.0.7\nnode-4 10.0.0.8\n' >"$SOAK_DIR/nodes"   # the first is the server
printf 'outside 10.0.0.9\n' >"$SOAK_DIR/outside"
scripts/soak/outside.sh setup          # Pebble, challtestsrv, registry; the private images
BIN=path/to/hivemind scripts/soak/setup.sh
scripts/soak/outside.sh client-start
scripts/soak/scenario.sh probe-start
scripts/soak/scenario.sh watch-start

# the faults of the first soak; mark each one the operator makes:
scripts/soak/scenario.sh begin kill-worker node-3
scripts/soak/scenario.sh measure kill-worker node-3 loss &
scripts/soak/scenario.sh fault kill-worker node-3 poweroff    # ...then power it off
scripts/soak/scenario.sh rollout 2          # www to whoami:2
scripts/soak/scenario.sh drain node-3       # drain, then eligibility --enable
scripts/soak/scenario.sh remove node-4      # drain, then node remove
scripts/soak/scenario.sh join-new node-4    # after the machine is replaced at the same address

scripts/soak/outside.sh fetch
SOAK_DURATIONS=scripts/soak/durations-routing.txt scripts/soak/report.sh "$SOAK_DIR/soak.log"
scripts/soak/client-report.sh "$SOAK_DIR/soak.log" "$SOAK_DIR/client"
```

`client-report.sh` prints, for each scenario window, each stream's requests, failures and their codes, longest gap and latency; for the steady window, the success rate and the latency over every stream; and each certificate Pebble issued, with when each router first served it.
