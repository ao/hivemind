# Soak test on four machines

One server and three workers, each a separate machine, run three small apps. Then the soak test powers off a worker, reboots one, cuts one off from the server, replaces the server with an empty machine, and leaves the cluster alone for four hours. Every number below comes from the harness's log; nothing is estimated. The harness is in [scripts/soak](../scripts/soak), and [Running it](#running-it) says how to run it. A [second soak](#second-soak-routing-a-private-registry-and-clients-outside-the-cluster), on five machines, measures the same faults and more through the HTTPS routers, from clients outside the cluster. A [third soak](#third-soak-the-server-restored-at-a-new-address) restores the server on new machines at new addresses, for workers that dial its DNS name and for workers moved with `hivemind node set-server`, and checks the routers' counters in `/metrics`. A [fourth soak](#fourth-soak-path-routes-the-start-grace-and-the-old-server-back) adds two apps that share a host by path, restores the server with its name's record changed inside the start grace, and powers the old server's machine on again.

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
- `/metrics` exists only on the server, so the routers' `502` counts are the server's router's; the workers' routers exported none then (since this soak, workers post theirs to the server).
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

# Third soak: the server restored at a new address

The second soak's set-up on five new machines, on 2026-11-17, with one change: the server advertised a DNS name, `hivemind-server.soak.test`, and the workers joined that name. The server's machine was then lost and its backup restored on a new machine at another private address, three times: for workers that dial the name, whose record was changed to the new address; for workers that dial an IP address, which were moved with `hivemind node set-server`; and for the name again, with the record changed 30 s late. Between them the routers' counters in the server's `/metrics` were checked against the clients, a worker was powered off, one was drained, every node was upgraded to the final build, and the cluster ran for an hour with the clients running. Times are UTC.

## Restoring the server at a new address

| | Workers dial the name | Workers dial an IP address | The name, record changed 30 s late |
|---|---|---|---|
| API down to API up | 98.1 s | 94.6 s | 94.1 s |
| of that, the new machine's launch and boot (launch request to its user data done) | 85.1 s | 88.9 s | 88.1 s |
| New machine ready to API up (binary, hosts file, CAs, `restore`, unit started) | 2.1 s | 3.1 s | 3.1 s |
| The operator's step once the API was up | the name's line in `/etc/hosts` changed on all six machines, done 1.3 s after the API was up | on each worker in turn `systemctl stop`, `set-server <new address>`, `systemctl start`; the three done 1.8 s after the API was up | the name's line changed 31.0 s after the API was up |
| Workers `ready` | 0.9, 1.8 and 5.9 s after the record changed | 1.3, 2.0 and 2.0 s after the API was up | 1.2, 1.2 and 2.1 s after the record changed |
| Converged, every replica running | 30.5 s after the record changed | 5.4 s after the API was up | 20.0 s after the record changed |
| Workers that joined again | none: the same node IDs, certificates and containers | none | none |
| Nodes marked lost | none | none | all three, 15.0 s after `serve` started |
| Replicas on two machines | none | none | up to nine at once, for 35.0 s |
| Where the replicas ended | where they were | where they were | 13 of 14 on the new server |
| Requests through the workers' routers while the server was gone | none failed; one request per `www` stream took 7.5 s, then about every 5 s one took 2.0 s | the same | the same |
| Requests in the seconds after the restore | `503` for 0.7 to 1.7 s on the streams through three of the four routers, 7 to 17 per stream | `503` on two `www` streams, 10 and 11; each worker's router refused requests while its service restarted for `set-server` | through the new server's router, 131 `503`s for `api.soak.test` (no endpoint for 10.1 s at a time) and 32 for `www.soak.test`; none through the workers' routers |

In each case the restored server warned, in its journal, that its address had changed since its last start and what the workers needed: for the name, that they reach it once the name leads to the new address (and, until then, that the name resolved to the old machine); for the IP address, that each worker must be moved with `hivemind node set-server`, with the count of workers. Its certificate named the name and the new address, or the new address alone.

## The machines and the builds

| | |
|---|---|
| Machines | 8 EC2 c7g.large in one subnet, the type, OS and Docker of the earlier soaks: 2 vCPU (Graviton3), 3.7 GiB (MemTotal 3897240 kB), Amazon Linux 2023.12.20261112, kernel 6.18.62-127.179.amzn2023.aarch64, Docker 25.0.18 |
| Roles | hivemind-soakd-1 the first server, 2 to 4 workers, 5 the outside machine. hivemind-soakd-6, 7 and 8 were launched during the run as the new server machines, each at a new private address |
| hivemind | `CGO_ENABLED=0 go build -trimpath ./cmd/hivemind`, Go 1.27.4, linux/arm64. Commit 9983bd5 (sha256 0359f496...145bc42) for the set-up and every scenario up to the drain. Commit f4d8799, the final build (the router hardening; sha256 27c3d55b...80eef7b), from 09:16:55: the upgrade, the steady state and the late-record restore. The cluster went from one to the other with the harness's upgrade step, the server first |
| Service | systemd, the units of [systemd.md](systemd.md) with `--advertise hivemind-server.soak.test --cluster-addr <its address>` on the server and the router's flags on every node |
| Outside machine | Pebble (revision 5caaab3), pebble-challtestsrv, registry:2, curl 8.21.0, as in the second soak |
| Network | The soak security group allowed, between the soak machines only, TCP 80, 443, 4484 to 4485, 5000, 14000 and 32768 to 60999 (checked from one machine to another before the set-up: open; 8000: closed). Nothing from the internet |

## Set-up

As in the second soak, with the server's name: one line in `/etc/hosts` on every machine, the outside one included, `<server address> hivemind-server.soak.test`, stood in for its DNS record. The server ran `serve --advertise hivemind-server.soak.test --cluster-addr <its address>`, and each worker joined `hivemind-server.soak.test`; its `node.json` held `hivemind-server.soak.test:4484`. Every node ran a router; the apps were the second soak's (`www`, private image, 3 replicas, routed; `api`, 2, routed; `spread`, 6; `kv`, a volume, its value checked by every probe; the compose project `shop`). The set-up took 61 s from the binaries' install to every router serving both hosts; the routers served them 3.3 s after the harness began to wait for that.

The agent created every container with the `json-file` log driver, `max-size` `10m` and `max-file` `5` (`docker inspect`); Docker's default driver on the nodes is `json-file`. `hivemind compose convert` refused a file whose `environment` read `${HIVEMIND_TOKEN:-x}`: `variable HIVEMIND_TOKEN: a compose file cannot use the variables that start with HIVEMIND_ ...`.

## Method

The second soak's harness and clients: eight streams on the outside machine, one per router and host, each a new TCP connection and TLS handshake every 100 ms; the 10 s probe loop; the 1 s watcher during the faults; each scenario's own 1 s loop. New in [scripts/soak](../scripts/soak) for this run:

- `scenario.sh server-move NEW NEWIP name|ip` measures a server restored on a new machine at another address. It waits for the old machine to stop answering and for the new one (whose address it reads from a file once the machine is launched), installs the binary, prepares the machine, restores the backup, starts the unit (`--advertise <name> --cluster-addr <new address>`, or `--advertise <new address>`), and, once the API answers, does the operator's step: the name's line on every machine (`scenario.sh dns`), or `set-server` on each worker. For the clients it adds streams to the new router and points the routed hosts' DNS answers (challtestsrv) at the nodes there are now.
- For its first 40 s, the restored server's routing table (`GET /v1/routes`) every 200 ms, logged at each change.
- `scenario.sh set-server-checks` runs `set-server` on a worker beside its running service, then, with the service stopped, to each address it is given, and checks that `node.json` keeps its digest. `outside.sh scratch-start` runs another cluster's server (a `hivemind init` in its own data directory) on the outside machine for it to refuse.
- `scenario.sh metrics-snap` saves the router series of the server's `/metrics`, and `metrics-report.sh` compares two of them with the clients' own logs. `metrics-watch` logs, every 5 s, which nodes have router series.
- The probe loop's resource line also has the number of dangling Docker volumes on each node, and the largest json-file log of a hivemind container and the most such files one container has.
- The upgrade step restarts one node at a time and waits until its routed replicas are ready in `hivemind status`, as [upgrading.md](upgrading.md#routed-apps) says.

## Results

### Lose the server; the workers dial its name

The server, hivemind-soakd-1 at 10.0.0.1, ran `www` 1, `front` and `spread` 0 and 4. It was backed up (a 200192-byte tar), powered off with `sysrq o`, and a new machine, hivemind-soakd-6, was launched; it came up at 10.0.0.6.

| | UTC | Seconds |
|---|---|---|
| Power-off request | 08:35:41.0 | |
| API down (1 s watcher) | 08:35:51.2 | +10.2 |
| Launch request for the new machine | 08:36:02.2 | |
| New machine ready | 08:37:27.3 | launch + 85.1 |
| API up, restored, serving with `--cluster-addr 10.0.0.6` while the name still named 10.0.0.1 | 08:37:29.3 | outage 98.1 |
| The name's line changed on every machine | 08:37:30.6 | API up + 1.3 |
| hivemind-soakd-2, 4 and 3 `ready` | 08:37:31.5, 32.5, 36.5 | record + 0.9, 1.8, 5.9 |
| The server's four replicas running on the new machine | 08:37:34.5 | |
| hivemind-soakd-2's replicas reported | 08:37:31.5 | |
| hivemind-soakd-4's and hivemind-soakd-3's replicas reported | 08:37:58.6, 08:38:00.6 | record + 27.9, 29.9 |
| Converged, every replica running | 08:38:01.1 | record + 30.5 |

Each worker kept its node ID, its certificate and every container, and its `node.json` still named `hivemind-server.soak.test:4484`. The new server's journal had no "node lost" line, and the cluster never had a replica on two machines.

Two workers were `ready` within 2 s of the record's change but reported their containers 26 and 24 s after that, and until then `hivemind status` showed their ten replicas as `unknown`. While a worker cannot reach the server its passes back off, up to one every 30 s ("cannot reach the server; passes back off, up to one every 30s" in its journal); its lease is renewed on its own 5 s timer, which reached the new server at once, but the next pass came only when the backoff ran out. hivemind-soakd-4's last failed pass was at 08:37:28.1 and its next at 08:37:58.1, 30.0 s later; hivemind-soakd-2's backoff happened to end at 08:37:31.5, just after the record changed.

Through the routers: the server's machine stopped at about 08:35:46. On each of the three `www` streams through the workers' routers, one request, at 08:35:53, took 7.5 s: it had gone out on a connection the router kept open to the server's `www` replica; it failed after the 5 s TCP_USER_TIMEOUT and went to another replica. In soak 2 that request failed at the client's 10 s limit. After that, about every 5 s one request on each `www` stream waited the 2.0 s dial timeout for the dead replica before going to another (20 to 22 such requests per stream in the 98 s): a replica whose dial failed is skipped for 3 s, then tried again with the next request. No request through the workers' routers failed while the server was gone. The `api` streams had none of this, as no `api` replica ran on the server. Then, in the seconds after the restore:

| Router | `api` | `www` |
|---|---|---|
| hivemind-soakd-2 | 0 failed | 10 `503`s, 08:37:31.5 to 32.5 |
| hivemind-soakd-3 | 0 | 0 |
| hivemind-soakd-4 | 7 `503`s, 08:37:31.2 to 31.9 | 7 `503`s, the same second |
| hivemind-soakd-6 (the new server; its streams started at 08:37:30.4) | 17 `503`s, 08:37:30.6 to 32.3 | 17 `503`s |

A `503` means that the router's table had no endpoint for the host. Each worker's endpoints came back into the new server's routing table only when that worker reached the new server, although the server means to keep, for 15 s after it starts, the endpoints of the workers that were up before the start. The second restore logged the table and shows it directly (below), and the store explains it: `serve` renews its own node's lease (`UpsertNode`) before it records its start time, so the latest lease "before the start", from which the 15 s are counted, is the server's own, and every worker whose last lease is more than 15 s old is left out. On a plain restart of the server the workers' leases are a few seconds old and the table keeps them: after a restart of the server's service at 08:46:29, the first answer of `GET /v1/routes`, 0.1 s later, had every endpoint. The harness's query of the restored store, before and after that lease, is in the report of this run.

### Move the workers from the name to the IP address

To have an IP-joined cluster for the second restore, each worker was moved to the server's address with `set-server`, the reverse of the move [backup.md](backup.md#at-a-new-ip-address) describes: on each worker, `systemctl stop`, `hivemind node set-server 10.0.0.6`, `systemctl start`, one after another. Each `set-server` checked the server and changed `node.json` from `hivemind-server.soak.test:4484` to `10.0.0.6:4484`; the three took 0.4 s together. The server was then restarted with `--advertise 10.0.0.6`, and its certificate named only that address. Through the routers, each worker's router refused one to five requests (0.1 to 0.5 s) while its service restarted, and the server's router one; no `503`: although every worker's agent restarted within 0.4 s, their routed replicas stayed in the table (in soak 2 they left it for 4 s; the agent now keeps a container its last report called healthy).

### set-server refuses

On hivemind-soakd-3, `hivemind node set-server` beside its running service, then, with the service stopped, to four other addresses. Each was refused, and `node.json` kept its SHA-256 every time. The service was stopped for 0.49 s.

| Address given | What `set-server` said |
|---|---|
| `10.0.0.6`, the right server, with the worker running | `another hivemind (pid 36440) runs on the data directory /var/lib/hivemind; stop it first ...` |
| `10.0.0.2`, another worker, where nothing listens on 4484 | `cannot reach a server at 10.0.0.2:4484: connect: connection refused. /var/lib/hivemind/node.json is unchanged` |
| `10.0.0.5`, another cluster's server (a scratch `init` on the outside machine) | `the server at 10.0.0.5:4484 presents a certificate from the CA with fingerprint 411effb2..., not from this worker's cluster CA, fingerprint d25d4a49...: it is another cluster's server, ... node.json is unchanged` |
| `10.0.0.6:4485`, the right server's join port | `10.0.0.6:4485 is the default join port, but set-server takes the server's cluster port, 10.0.0.6:4484, or no port` |
| `hivemind-server.soak.test`, a name that leads to the right server, which now advertised only its address | `the server at hivemind-server.soak.test:4484 has a certificate from this cluster's CA, but it names 10.0.0.6, not hivemind-server.soak.test: ... node.json is unchanged` |

### Lose the server; the workers dial its IP address

The server, now hivemind-soakd-6 at 10.0.0.6 with `www` 1, `front` and `spread` 0 and 4, was backed up, powered off, and a new machine, hivemind-soakd-7, came up at 10.0.0.7.

| | UTC | Seconds |
|---|---|---|
| Power-off request | 08:49:42.4 | |
| API down (1 s watcher) | 08:49:52.4 | +10.0 |
| New machine ready | 08:51:23.9 | launch + 88.9 |
| API up, serving `--advertise 10.0.0.7` | 08:51:27.0 | outage 94.6 |
| Its routing table: no endpoint for either host | 08:51:27.05 | |
| `set-server 10.0.0.7` and a restart on hivemind-soakd-2, 3 and 4, one after another | 08:51:27.8 to 28.8 | |
| Table: `api` has hivemind-soakd-2's replica | 08:51:28.19 | |
| Table: both workers' `api` and `www` replicas | 08:51:28.85 | |
| Workers `ready` | 08:51:28.3, 29.0, 29.0 | API up + 1.3, 2.0, 2.0 |
| Converged, every replica running | 08:51:32.4 | API up + 5.4 |
| Table: the new server's own `www` replica too | 08:51:34.2 | |

Here the workers reported within 2 s, because `set-server` restarts each agent, which passes at once. The table at 08:51:27.05 shows what caused the `503`s in both restores: no endpoint for any host until a worker reported. Through the routers: the same 7.5 s request and the 2.0 s requests while the server was gone, none failed; then 11 `503`s on the `www` stream through hivemind-soakd-2's router and 10 through the new server's, in the second after the start; and each worker's router refused 1 to 7 requests during its own restart for `set-server`.

### The routers' counters in /metrics

The server's `/metrics` had `hivemind_router_requests_total` and `hivemind_router_cert_expiry_timestamp_seconds` for all four nodes. Two reads 300 s apart (08:54:21 and 08:59:21), with no fault between them, against the clients' own logs of the requests that ended between the two reads:

| Router | Host | Counter increase, code 200 | 200s in the clients' logs | Difference |
|---|---|---|---|---|
| hivemind-soakd-2 | api.soak.test | 2980 | 2982 | -2 |
| hivemind-soakd-2 | www.soak.test | 2978 | 2979 | -1 |
| hivemind-soakd-3 | api.soak.test | 2979 | 2980 | -1 |
| hivemind-soakd-3 | www.soak.test | 2978 | 2979 | -1 |
| hivemind-soakd-4 | api.soak.test | 2980 | 2982 | -2 |
| hivemind-soakd-4 | www.soak.test | 2980 | 2982 | -2 |
| hivemind-soakd-7 (the server, read directly) | api.soak.test | 2980 | 2981 | -1 |
| hivemind-soakd-7 | www.soak.test | 2979 | 2979 | 0 |

The workers' counters reach the server up to 15 s late, but at a steady 10 requests a second the lag moves both ends of the window alike, so it does not show here; the counts agree within 2 requests. The `503`s of the restores were counted too (`code="503"`, 10 and 11 on the two routers where the clients saw 10 and 11). A worker's counters start again at zero when its service restarts.

### Power off a worker

hivemind-soakd-4, with `www` 2 and `spread` 1 and 5, powered off with `sysrq o`.

| Measure | Seconds |
|---|---|
| Power-off request to port 22 closed | 6.5 |
| Port 22 closed to `lost` | 13.8 |
| `lost` to its three replicas running elsewhere, converged | 4.2 |
| Port 22 closed to its router's series gone from the server's `/metrics` (5 s polls) | 119.3 |
| Start request to port 22 open | 13.0 |
| Start request to its old containers removed | 14.7 |
| Start request to `ready` | 15.1 |
| `ready` to its router's series back in `/metrics` | 16.3 |
| Replicas on two machines | none |

Through the other routers no request failed; the one request per `www` stream on a kept connection to the dead machine took 7.5 s, as above.

### Drain a worker; anonymous volumes

`hivemind node drain` of hivemind-soakd-3 moved its five replicas, `cache` among them, in 22.3 s, then `eligibility --enable`; no request failed on any stream. `cache` is redis without a volume in its spec, so Docker gives each of its containers an anonymous volume for the image's `VOLUME /data`. Before the drain hivemind-soakd-3 had that volume; after it, none: the agent removed it with the container. The new `cache` container on hivemind-soakd-4 had its own. No node had a dangling volume in any probe of the run (`docker volume ls -f dangling=true`, every 60 s). The probe loop counted the drain's surge copies, each started before the old copy stopped, as replicas on two machines for two probes; that is how a drain moves a replica, not a fault.

### Upgrade

The upgrade step installed f4d8799 on every node and restarted the server's service, then each worker's once the last one's routed replicas were ready in `hivemind status`. Each was ready at once, so the four restarts took 6.6 s. The API answered 0.1 s after the server's restart. Through the routers: one refused request on each stream through hivemind-soakd-2's and hivemind-soakd-3's routers during their own restart, nothing else; no `503`.

### Certificates, and the one expiry

The first certificates were issued at 08:33:27 and 08:33:28, valid for one hour. They were not renewed in time, and expired at 09:33:26: for about 3 min 35 s, until 09:37:01.5, every request on every stream failed certificate verification (`curl` exit 60, 2116 to 2139 per stream). This was not the cluster's network or a router crash:

- From the first restore on, the routed hosts' DNS answers on the outside machine (pebble-challtestsrv) still listed the dead first server at 10.0.0.1, because the harness's step that updates them failed (a `pkill` in it matched its own shell). The routers tried to renew from 08:42, with 50 minutes left, and again at their checks at 09:01, 09:11 and 09:27; every attempt failed: Pebble's HTTP-01 validation dialled 10.0.0.1 (`no route to host`). The answers were corrected at 09:20:24; Pebble still dialled the old address at 09:27 (not traced), and they were cleared and set again at 09:29:55.
- certmagic checks every 10 minutes from the router's start. After the upgrade at 09:17 the checks came at 09:27, before the second correction, and at 09:36:55, after the expiry. That one renewed both hosts, and the clients' requests through every router succeeded again 5 to 7 s after the issuance.
- No router tried to renew when it started at 09:17, although each had a certificate it had been trying to renew: a router that starts loads its hosts' certificates from the shared storage before it serves TLS (new since soak 2), and certmagic then treats them as managed already and does not check them until its first 10-minute check. In soak 2 every router that started renewed such a certificate at once. Here a renewal at the start would have failed too.

With a one-hour lifetime a few failed checks are enough; with Let's Encrypt's 90-day or 6-day certificates there are hundreds of checks in the renewal window. The certificates of 09:36:55 were renewed again at 09:46:55, with 50 minutes left, as the first ones were first tried at 08:42; in soak 2 hosts were renewed every 10 to 20 minutes. The steady state below began after the expiry, at 09:41:59.

### Lose the server; the name's record changes 30 s late

[concepts.md](concepts.md#the-servers-address) suggests a TTL of a minute or so for the server's name, and the build of this run gave each worker 15 s from a restored server's start to report (since this run, the start grace gives it 2 minutes by default; see the end of this section). After the steady state the cluster was moved back to the name, as [backup.md](backup.md#at-a-new-ip-address) describes (the server restarted with `--advertise hivemind-server.soak.test --cluster-addr <its address>`, then `set-server hivemind-server.soak.test` on each worker, 0.13 s each), and the server was lost once more. This time the harness changed the name's line 30 s after the restored server's API answered, as a record whose old value is still cached would reach the workers late.

| | UTC | Seconds |
|---|---|---|
| Power-off request (hivemind-soakd-7) | 10:43:05.1 | |
| API down (1 s watcher) | 10:43:15.6 | |
| New machine (hivemind-soakd-8) ready | 10:44:46.6 | launch + 88.1 |
| `serve` started (its journal) | 10:44:48.9 | |
| API up | 10:44:49.7 | outage 94.1 |
| Routing table: no endpoint for either host | 10:44:49.8 | |
| Table: the workers' `api` and `www` endpoints, from the store | 10:44:53.7 | |
| Table: `api` has none; `www` only the new server's own | 10:45:03.9 | start + 15.0 |
| All three workers `lost` ("node lost: its replicas are placed again" in the server's journal) | 10:45:03.9 | start + 15.0 |
| The workers' nine replicas without a volume running on the new server too: nine (app, replica) pairs on two machines | 10:45:09.2 | |
| The name's line changed | 10:45:20.7 | API up + 31.0 |
| Workers `ready` | 10:45:21.9, 21.9, 22.8 | record + 1.2, 1.2, 2.1 |
| No replica on two machines; converged | 10:45:40.7 | record + 20.0 |

The workers did not join again and their `node.json` was unchanged, but every replica they ran, except `kv`, which has a volume and waited for its node, was placed on the new server, started there while the workers' copies kept running, and stayed there: when the workers reached the server they removed their copies, one `spread` container after another in 10 s stops; there were replicas on two machines for 35.0 s in all. The cluster ended with 13 of its 14 replicas on one machine. Through the workers' routers no request failed: they could not reach the server and served their last table. Through the new server's router, `api.soak.test` answered `503` 131 times, from 10:44:50.7 to 10:45:13.9 (no endpoint for 3 s after the start, then from the 15 s mark until its own new copies passed their health checks), and `www.soak.test` 32 times in its first 3 s.

The table's first 15 s also show how the start grace fails: it was empty until about 10:44:53, when the server's own agent renewed its node's lease; from then until the 15 s mark it held the workers' endpoints from the store, as meant; in the two earlier restores the workers reported within those first 4 s.

Since this run, `serve` has a start grace, 2 minutes by default (`--start-grace`): a worker that was up before the start is `waiting` until it reports, not lost, and the routing table keeps its endpoints as it last reported them ([concepts](concepts.md#leases-and-node-loss)). `scripts/integration-newaddr.sh` moves the name 30 s after the restored server starts, past the 15 s, and checks that the workers wait and that no replica runs twice. The soak was not run again on that build.

## Steady state

On f4d8799, 1 h 0 min 12 s, from 09:41:59.3 to 10:42:11.5, with no fault and no change to the apps: the eight client streams, the probe loop, no watcher. Placement in the window: the server, hivemind-soakd-7, ran `www` 1, `front` and `spread` 1 and 5; hivemind-soakd-2 `kv`, `api` 0, `www` 2 and `spread` 3; hivemind-soakd-3 `spread` 0 and 4 only, so its router reached every replica over the network; hivemind-soakd-4 `api` 1, `cache`, `www` 0 and `spread` 2.

| Through the routers | |
|---|---|
| Requests, all eight streams | 286854 |
| Succeeded | 286854 (100 %) |
| Longest gap between two successful requests on a stream | 0.12 s |
| Total time per request (TCP connect, TLS handshake, request and reply), p50 / p90 / p99 / p99.9 / max | 2.5 / 3.0 / 3.7 / 5.8 / 21.5 ms |
| TLS handshake, p50 / p99 / max | 1.5 / 2.5 / 10.6 ms |
| TLS done to first byte of the reply, p50 / p99 / max | 0.6 / 1.2 / 19.6 ms |
| Certificates issued in the window | 13, at each 10-minute check of the server's router; twice a second router renewed a host 8 s after the first |
| Certificate checks (a fresh handshake with every router for both hosts every 30 s) | 960, none failed |

| Probe | Result |
|---|---|
| Status answered, converged, all 14 replicas running, no replica on two machines | 360 of 360 |
| `kv` value read back, and the same as written | 360 of 360 |
| `GET /v1/status` on the server, 300 requests | all 200; p50 2.2 ms, p99 8.1 ms, max 8.9 ms |
| Dangling Docker volumes, every node, every 60 s | 0 (and 0 in every sample of the whole run) |
| Container log files | at most 3 files for one container, the largest 10000188 bytes (`api`'s nginx logs every request, about 18 MB an hour); `max-size` `10m` is 10000000 bytes, and Docker rotates after the write that passes it |
| Events: lost nodes, restarts, failed probes | none |

The hivemind process on each node, 60 samples a minute apart:

| Node | RSS first / last / max (MiB) | Threads first / last | Open fds first / last / max | CPU, % of one CPU | Restarts |
|---|---|---|---|---|---|
| hivemind-soakd-7 (server) | 27.5 / 27.5 / 27.8 | 9 / 10 | 35 / 38 / 39 | 2.50 | 0 |
| hivemind-soakd-2 | 22.7 / 23.2 / 23.3 | 8 / 9 | 20 / 20 / 22 | 1.91 | 0 |
| hivemind-soakd-3 | 22.8 / 23.1 / 23.1 | 8 / 8 | 18 / 19 / 19 | 1.58 | 0 |
| hivemind-soakd-4 | 23.1 / 22.7 / 23.3 | 9 / 9 | 19 / 20 / 21 | 1.87 | 0 |

An hour is too short to see a slow leak; the RSS of each process stayed within 0.6 MiB. The store was 208 KiB (`hivemind.db`), with a 4027 to 4040 KiB WAL. The routers' counters over the window, against the clients: the server's router counted 22 and 19 more `200`s than its two streams had, which are Pebble's HTTP-01 validations of the 13 renewals (21 and 18 in its log; Pebble validated through the server's router, the first address in the hosts' DNS answers); the workers' counters differed by -121 (hivemind-soakd-2) and +28 (3 and 4) per stream, 12 s and 3 s of a stream's traffic: each read sees a worker's counters as it last posted them, up to 15 s before, and that lag was not the same at the two reads.

## What did not work, and what was not measured

- Three harness bugs cost measurements. The first restore's 1 s scenario loop started 75 s after the power-off (a tmux command line expanded the log path empty), so that restore's API-down time is the watcher's, not the scenario loop's, and its loop then waited on the background loops it had restarted until it was ended by hand; its post-checks ran separately. The step that adds a new router to the clients killed its own shell, so the certificate watcher stopped from 08:37 to 09:19 and the routed hosts' DNS answers were not updated, which led to the expiry above. All three are fixed in the harness.
- The first steady window, from 09:18:20, was ended at 09:37:25 by the expiry; the steady state above is the second window, after it.
- The name's record was a line in `/etc/hosts` changed on every machine at once; a real DNS record reaches each resolver after its TTL, at different times. The late-record restore stands in for that with one delay for all workers.
- Each new server machine was launched after the old one was powered off, so the outages include 85 to 89 s of EC2 launch and boot; an operator with a machine ready saves that.
- The routers' counters were checked against the clients in a window with a steady rate, where the workers' 15 s posting lag does not show.
- The steady state was one hour, as the brief asked: too short for leak figures like those of the first soak.
- The machines' private addresses are shown renumbered: 10.0.0.N is hivemind-soakd-N.

## Running it, at a new address

As [with routing](#running-it-with-routing), with the server's name set:

```sh
export SOAK_DIR=$PWD/.scratch/run SOAK_ROUTES=1 SERVER_NAME=hivemind-server.soak.test
scripts/soak/outside.sh setup
BIN=path/to/hivemind scripts/soak/setup.sh      # the server advertises the name; workers join it
scripts/soak/outside.sh client-start
scripts/soak/scenario.sh probe-start
scripts/soak/scenario.sh watch-start

# lose the server; restore at a new address and change the name's record
scripts/soak/scenario.sh begin lose-server-name node-1
scripts/soak/scenario.sh server-backup
scripts/soak/scenario.sh server-move - - name &      # ...power node-1 off, launch a new machine, then:
echo "node-6 10.0.0.9" >"$SOAK_DIR/new-server"

# workers that dial an IP address: move them, then serve the address alone
scripts/soak/scenario.sh set-server-all <server address>
echo ip >"$SOAK_DIR/advertise"                       # and install the server unit again, and restart it
scripts/soak/outside.sh scratch-start
scripts/soak/scenario.sh set-server-checks node-3 <another worker> <outside machine> <server>:4485 "$SERVER_NAME"
scripts/soak/scenario.sh begin lose-server-ip node-6
scripts/soak/scenario.sh server-backup
scripts/soak/scenario.sh server-move - - ip &        # ...as above

# the routers' counters against the clients
scripts/soak/scenario.sh metrics-snap m1             # ...minutes later:
scripts/soak/scenario.sh metrics-snap m2
scripts/soak/outside.sh fetch
scripts/soak/metrics-report.sh "$SOAK_DIR/metrics-m1.txt" "$SOAK_DIR/metrics-m2.txt" "$SOAK_DIR/client"

SOAK_DURATIONS=scripts/soak/durations-newaddr.txt scripts/soak/report.sh "$SOAK_DIR/soak.log"
```

`DNS_DELAY=30 scripts/soak/scenario.sh server-move - - name lose-server-late` changes the record 30 s after the restored API answers. Fetch the client logs a minute after the last read for `metrics-report.sh`: curl writes its lines in 4 KiB blocks, and the last seconds are not in the file yet.

# Fourth soak: path routes, the start grace, and the old server back

The third soak's set-up on six new machines, on 2026-11-18, on the build that adds the start grace, the cluster epoch, path routes, the routers' backoff for dead replicas and their renewal at start. Two more apps shared one host by path, `paths.soak.test/` and `paths.soak.test/api`, and the clients checked that every reply came from the app its path belongs to. The server's machine was lost and restored on a new machine with the name's record changed 60 s after the restored API answered, inside the 2-minute start grace. Then the old server's machine was powered on again with its unit enabled, and one worker was sent to it for 30 s. Then rolling updates of the two path apps, a worker powered off, a router restarted with certificates in their renewal window, and an hour of steady state. Times are UTC; the machines' private addresses are shown renumbered, 10.0.0.N for hivemind-soakf-N.

## Restoring the server with the record changed 60 s late

| | Third soak, record 30 s late (f4d8799) | This soak, record 60 s late (b0a2f76) |
|---|---|---|
| API down to API up | 94.1 s | 94.3 s |
| Workers marked lost | all three, 15.0 s after `serve` started | none: `waiting` in `hivemind nodes` until each reported |
| Replicas on two machines | up to nine at once, for 35.0 s | none |
| Where the replicas ended | 13 of 14 on the new server | where they were; the server's own five on the new machine |
| The restored server's routing table at its first answer | no endpoint for either host | every worker's endpoint, none of the dead machine's |
| `503`s through the new server's router | 131 for `api`, 32 for `www` | none |
| `503`s through the workers' routers | none | none |
| Workers `ready` after the record changed | 1.2, 1.2 and 2.1 s | 1.1, 3.2 and 5.8 s |
| Converged after the record changed | 20.0 s | 5.8 s |
| Requests of 1.5 s or more per stream through a worker's router while the server was gone, for an app with a replica on it | about one every 5 s: 20 to 22 in about 95 s | 4 to 7 in about 165 s, the first of them 7.5 s |

The third soak's restore with the record changed on time had `503`s for 0.7 to 1.7 s on three of four routers (7 to 17 per stream), and two workers reported their containers 26 and 24 s after they could reach the new server; here every worker had reported within 5.8 s of the record's change.

## The machines and the build

| | |
|---|---|
| Machines | 6 EC2 c7g.large in one subnet: 2 vCPU (Graviton3), 3.7 GiB (MemTotal 3897240 kB), Amazon Linux 2023.12.20261117, kernel 6.18.62-127.179.amzn2023.aarch64, Docker 25.0.18 |
| Roles | hivemind-soakf-1 the first server, 2 to 4 workers, 5 the outside machine, 6 the new server machine, launched during the run at a new address |
| hivemind | b0a2f76, `CGO_ENABLED=0 go build -trimpath ./cmd/hivemind`, Go 1.27.4, linux/arm64, sha256 2caed610...9601bdb09, for the whole run |
| Service | systemd, the units of [systemd.md](systemd.md) with `--advertise hivemind-server.soak.test --cluster-addr <its address>` on the server and the router's flags on every node; `--start-grace` at its default, 2 minutes |
| Outside machine | Pebble (revision 5caaab3), pebble-challtestsrv, registry:2, curl 8.21.0 |
| Network | The soak security group allowed, between the soak machines only, TCP 80, 443, 4484 to 4485, 5000, 14000 and 32768 to 60999 (checked from one machine to a listener on another before the set-up: open; 8000: closed). Nothing from the internet. Clocks: chrony put every machine within 2 microseconds of the Amazon Time Sync Service at the start |

## Set-up

As in the third soak, every node routing and the server advertising `hivemind-server.soak.test`, plus two apps on one host:

| App | Image | Replicas | Route |
|---|---|---|---|
| proot | registry.soak.test/soak/whoami:1, `WHOAMI_NAME=proot` | 2 | `paths.soak.test`, the whole host |
| papi | the same image, `WHOAMI_NAME=papi` | 2 | `paths.soak.test`, path `/api` |

With the third soak's apps (`www`, 3 replicas; `api`, 2; `spread`, 6; `kv`, with a volume; `front` and `cache` from the compose project) the cluster ran 18 replicas. The set-up took 61 s from the binaries' install to every router serving all three hosts. `hivemind routes` listed `paths.soak.test` twice, `/` for proot and `/api` for papi, each with its two endpoints. Through each of the four routers, a list of paths went to the app it should:

| Path asked | Answered by |
|---|---|
| `/`, `/x`, `/apix`, `/api.json`, `/API/x` | proot |
| `/api/`, `/api/x`, `/%61pi/x` | papi |
| `/api` | papi (whoami's own `/api` handler, which answers JSON) |
| `//api//x` | `301` to `/api/x`, from the replica: the router matched the cleaned path, and passed the request on as sent |
| `/api/../x` | `400`, `bad request: the path has a dot segment (. or .., or %2E)` |
| `/api%2Fx` | `400`, `bad request: the path has an encoded slash (%2F)` |

## Method

The third soak's harness and clients, with 16 streams: one per router for each of `www.soak.test/`, `api.soak.test/`, `paths.soak.test/` and `paths.soak.test/api/x`, each a new TCP connection and TLS handshake every 100 ms. The two path streams read each reply and log the app named in its `Name:` line; a `200` from the other app counts as misrouted. New in [scripts/soak](../scripts/soak):

- `SOAK_PATHS=1` deploys proot and papi, adds the path streams, and `setup.sh check-paths` asks every router for the paths above.
- `scenario.sh old-back OLD OLDIP W` measures the old server's machine coming back: once a second, its API and its own view (the nodes in its store, its routing table, the containers on it), and the restored server's view. The clients send their streams to its router from the time it answers on port 22, as clients whose DNS answer still has its address would. 30 s after its API answers, worker W is sent to it: W's line for the name names the old machine, and W's connections to the restored server are reset (`iptables -j REJECT --reject-with tcp-reset` on W, to the restored server's port 4484), so that W dials the name again. 30 s later the line and the connections are put back. 300 s after its API answered, its unit is disabled and stopped (`systemctl disable --now`).
- `scenario.sh cert-restart T` restarts T's service once a certificate its router serves has 50 minutes or less of its hour left, and logs the serials T serves every 5 s.
- `scenario.sh rollout VERSION APP...` updates several apps at once.
- `client-report.sh` counts misrouted replies and requests that took 1.5 s or more, and has a per-path table for the steady window.
- `outside.sh chall-update` clears each routed host's DNS answers before it sets them: challtestsrv appends to a host's addresses, and Pebble validates against the first one (see [certificates](#certificates-and-the-harnesss-dns-answers)).

## Results

### Lose the server; the name's record changes 60 s late

The server, hivemind-soakf-1 at 10.0.0.1, ran `api` 1, `cache`, `papi` 0, `spread` 3 and `www` 0. It was backed up (a 204288-byte tar), powered off with `sysrq o`, and a new machine, hivemind-soakf-6, came up at 10.0.0.6.

| | UTC | Seconds |
|---|---|---|
| Power-off request | 13:17:21.7 | |
| API down (1 s watcher) | 13:17:32.4 | +10.7 |
| Launch request for the new machine | 13:17:35.6 | |
| New machine ready | 13:19:04.3 | launch + 88.7 |
| API up, serving `--cluster-addr 10.0.0.6` while the name still named 10.0.0.1 | 13:19:06.7 | outage 94.3 |
| Its routing table's first answer: every worker endpoint of every route, none of 10.0.0.1's | 13:19:06.8 | |
| `hivemind nodes`: the three workers `waiting`, lease ages 121 to 122 s; `hivemind status` 0 of 18 replicas running (the workers' are `unknown` until they report) | 13:19:07.8 | |
| The new server's own routed replicas (`api` 1, `papi` 0, `www` 0) in its table, started again on the new machine | 13:19:16.8 | |
| The name's line changed on all six machines | 13:20:07.6 | API up + 60.9 |
| hivemind-soakf-4, 2 and 3 `ready` | 13:20:08.7, 10.8, 13.4 | record + 1.1, 3.2, 5.8 |
| Converged, all 18 running | 13:20:13.4 | record + 5.8 |

`serve` logged, at its start, that `--advertise` resolved to 10.0.0.1 and not to `--cluster-addr`, that its address had changed with three workers that dial the name, `workers that were up before this start wait for their first report ... waiting=3 start_grace=2m0s`, and `cluster listening ... epoch=1`. It logged no `node lost`. Each worker kept its node ID, certificate and containers, and its `node.json` still named `hivemind-server.soak.test:4484`. The server's row in `hivemind nodes` read `10.0.0.6 (hivemind-server.soak.test)`, and `nodes --json` had each worker's `dials` as `hivemind-server.soak.test:4484`.

Through the routers no request failed, on any of the 16 streams: none through the workers' routers, which served their last table, and none through the new server's router from its first request at 13:19:07.9. The workers' routers still had the dead machine's replicas (`api` 1, `papi` 0, `www` 0) in their table for 165 s, until the workers reached the new server. On each stream of those apps through a worker's router, the first request after the power-off went out on a kept connection to the dead machine and took 7.5 s; after that, the dead replica's dials were tried at growing intervals, each costing a request the 2.0 s dial timeout:

| Stream | Requests of 1.5 s or more, by start time |
|---|---|
| `www` through hivemind-soakf-3 | 13:17:27 (7.5 s), 13:17:37, 13:17:46, 13:18:00, 13:18:26, 13:18:58 |
| `api` through hivemind-soakf-3 | 13:17:27 (7.5 s), 13:17:37, 13:17:45, 13:17:59, 13:18:26, 13:18:58, 13:19:30 (1.8 s) |
| `/api/x` through hivemind-soakf-4 | 13:17:27 (7.5 s), 13:17:37, 13:17:45, 13:18:00, 13:18:26 |
| `www` through hivemind-soakf-2 | 13:17:27 (7.5 s), 13:17:37, 13:17:45, 13:18:00 |

The gaps, 10, 8, 14, 26 and 32 s, are the skip of 3 s doubling to 30 s, plus the dial; in the third soak one such request came about every 5 s. The streams of `proot`, which had no replica on the server, had none.

### The old server back

At 13:21:42, four minutes after the restore, the old server's machine, hivemind-soakf-1, was started again, with its data directory from before the restore and its unit enabled. Its `/etc/hosts` line still named itself, and its unit had `--cluster-addr`, so its `serve` started.

| | UTC | Seconds |
|---|---|---|
| Start request | 13:21:42.2 | |
| Port 22 open | 13:21:55.4 | +13.2 |
| Its `serve` started: `cluster listening ... epoch=0`, three workers `waiting`, start grace 2 minutes; its router loaded the three certificates from its own store | 13:21:56.4 | |
| Its router answered the clients with `200`; its table held the workers' endpoints, as they last reported to it | 13:21:56.7 | |
| hivemind-soakf-2 sent to it | 13:22:27.1 | |
| hivemind-soakf-2 refuses it; each side logs its error line, in the same millisecond | 13:22:28.05 | +0.95 |
| hivemind-soakf-2 lost on the restored server, which places its four replicas again; four (app, replica) pairs on two machines | 13:22:41.2 | redirect + 14.1 |
| hivemind-soakf-2's line and connections back to the restored server | 13:22:56.2 | |
| The old server's grace ends: all three workers lost in its store | 13:23:56.4 | its start + 120.0 |
| Its own copies of the 17 replicas without a volume running on it; its table holds only them | 13:24:05 | |
| The old server's unit disabled and stopped | 13:26:57.2 | |
| hivemind-soakf-2 reaches the restored server (`answers at this cluster's epoch now`) | 13:26:58.05 | +0.85 |
| hivemind-soakf-2 `ready` there; converged, no replica on two machines | 13:27:01.1, 13:27:09.1 | stop + 3.9, 11.9 |

The two error lines were the ones [backup.md](backup.md#if-the-old-server-comes-back) shows. On the worker: `the server at hivemind-server.soak.test:4484 is from before a restore of this cluster ... asks again every 10s ...` with `server_epoch=0 seen_epoch=1`; on the old server: `a newer copy of this cluster exists: this server was replaced by a restore ... Stop this server and disable its service`, with the worker's node ID, `node_epoch=1 epoch=0`. The worker kept its containers running, and its router served its last table: no request through it failed. Its `cluster-epoch` file held 1 and its `node.json` was unchanged.

The worker did not go back to the restored server when its name and its connections were put back. For 4 min 5 s it went on refusing the old server, `the server is still unreachable ... from before a restore` once a minute in its journal, although the name named the restored server and nothing blocked it; it reached the restored server 0.85 s after the old server was stopped. Its connection to the old server stayed open: the worker asks again every 10 s on the connection it already has, and the stale reply leaves that connection in the pool, so the name is not looked up again (see the report of this run). While it was away, the restored server had marked it lost, placed its four replicas on other nodes, and the four ran on two machines for 4 min 26 s, until the worker came back and removed its copies.

The restored server kept the cluster: the other two workers stayed `ready` on it throughout, nothing was placed on the old machine, and no request through its router or the workers' routers failed in the window. Through the old server's router, as a client still sent there by DNS would see it:

| From | To | Through the old server's router |
|---|---|---|
| 13:21:56.7 | 13:23:56.4 (its grace) | `200` on every stream, 1186 to 1187 per stream, from the workers' replicas, which its table kept as they had last reported to it, and its own old containers; every path reply from the right app |
| 13:23:56.4 | 13:26:57.2 | `200` from its own copies, which it started once it had marked every worker lost: 1716 to 1797 per stream. `paths.soak.test/` had no endpoint for 8 s while its proot copies started: 80 `503`s |
| 13:26:57.2 | | refused (its unit stopped) |

Nothing on the old server said that it was the old copy until a worker of the restored cluster reached it: from its own view, three workers did not report within its grace and were lost. Its router served certificates from its own store, the ones of 13:15:56, still valid.

Since this run, a worker closes the connection a refused reply came on and opens new ones, so each ask looks the name up again; it keeps its refusal by the address that answered, and logs it once for each; and a server whose start grace ends with no report from any worker that was waiting keeps them all waiting, places none of their replicas, and logs at error that no worker has reported since it started ([backup](backup.md#replacing-a-lost-server)). `scripts/integration-newaddr.sh` checks all three. The soak was not run again on that build.

### Rolling updates of the path apps

| | Deploy to converged | Failed requests, 16 streams | Misrouted | Requests of 1.5 s or more |
|---|---|---|---|---|
| papi to `whoami:2` | 19.3 s | 0 | 0 | 0 |
| proot to `whoami:2` | 21.3 s | 0 | 0 | 0 |
| proot and papi to `whoami:1`, at once | 23.5 s | 0 | 0 | 0 |

Each rollout moved one replica at a time, starting the new generation, here on another node, before stopping the old; the probe loop counted those surge copies as replicas on two machines in two probes of each rollout, as it did for the third soak's drain. The longest gap between two successful requests on any stream was 0.11, 0.13 and 0.15 s in the three windows, and the slowest request took 12, 36 and 54 ms. No reply for a path came from the other app while both apps' replicas were being replaced, also when both were replaced at once.

### Power off a worker

hivemind-soakf-4, with `front`, `papi` 0, `www` 1 and `spread` 0 and 4, powered off with `sysrq o`.

| Measure | Seconds |
|---|---|
| Power-off request to port 22 closed | 6.3 |
| Port 22 closed to `lost` | 13.6 |
| `lost` to its five replicas running elsewhere, converged | 7.6 |
| Start request to port 22 open | 13.1 |
| Start request to its old containers removed, converged | 15.1 |
| Start request to `ready` | 15.2 |
| Replicas on two machines | none |

Through the other routers no request failed. On each stream of `www` and `/api/x` through them, two requests were slow: one of 7.5 s, on a kept connection to the dead machine, and one 2.0 s dial 11 s later, as in the third soak (two per `www` stream there too). The node is lost 14 s after it stops, and its replicas leave the table, so the backoff has no time to show here; it shows in the server's outage above. The streams through hivemind-soakf-4's own router failed from the power-off until 58 s after it, 57 per stream.

### Certificate renewal at a router start

At 13:33:47 all three certificates were in their renewal window, with 42 minutes of their hour left, and none had been renewed (below). hivemind-soakf-3's service was restarted:

| | UTC |
|---|---|
| `systemctl restart hivemind` on hivemind-soakf-3 | 13:33:47.58 |
| Its router: `certificates in their renewal window at start; renewing them now`, the three hosts | 13:33:47.68 |
| Pebble issued the three new certificates | 13:33:50 |
| `certificate renewed successfully`, three times | 13:33:55.01 to 55.02 |
| hivemind-soakf-3 served the new certificates (5 s polls) | between 13:33:52.8 and 13:33:57.9 |

Its streams lost one refused request each during the restart. The other routers loaded the new certificates from the shared storage at their next 10-minute check (`already renewed in storage; reloading`), 2 to 8 minutes later, except for `paths.soak.test`, which hivemind-soakf-2 renewed once more at 13:35:15 (below); the others then loaded that one.

### Certificates, and the harness's DNS answers

Pebble issued the first three certificates at 13:15:57 to 13:16:01. The restored server's router began renewing them at 13:29:06, and hivemind-soakf-4's router at its start, 13:31:43, as the new build does for a certificate already due. Every attempt failed: Pebble's HTTP-01 validation dialled 10.0.0.1, the dead old server (`dial tcp 10.0.0.1:80: connect: no route to host`). The cause was the harness: pebble-challtestsrv's `add-a` adds to the host's addresses rather than replacing them, so after the move each host answered eight addresses, the old server's first, and Pebble validates against the first. In the third soak this is what kept Pebble on the old address after the answers were "corrected" at 09:20, until they were cleared at 09:29, and the certificates expired. `chall-update` now clears each host first; it was run again at 13:33:31, and from then on every validation went to a live router. With a real DNS provider the same thing happens if a routed host's record still lists a dead node first: a CA that validates against one address fails while that node is down.

hivemind-soakf-2's second renewal of `paths.soak.test`, 88 s after hivemind-soakf-3's (Pebble's issuances at 13:33:50 and 13:35:18), came from Pebble's renewal information (ARI). Pebble suggests a window that spans the whole lifetime, from issuance; certmagic picks a random time in it and renews at any check from 10 minutes before that time, so a new certificate is now and then due at once. soakf-2 renewed under the storage lock with the stored certificate's 58 minutes left. That is Pebble's choice of window, not how a public CA sets it.

Every 10-minute check also logged, for each host, certmagic's warning `certificate is in emergency renewal window; expiration imminent`, with 40 minutes or more left. That is expected with one-hour certificates, and was left as it is: certmagic (v0.25.6) counts a certificate as in its emergency window when less than 1/50 of its lifetime, or less than five of its 10-minute checks, 50 minutes, is left, and a one-hour certificate has less than 50 minutes left from 10 minutes after its issuance. A 90-day certificate reaches that window only in its last 43 hours, after about four weeks of failed renewals.

## Steady state

On b0a2f76, 1 h 0 min 29 s, from 13:35:40.8 to 14:36:10.3, with no fault and no change to the apps: the 16 client streams, the probe loop, no watcher. Placement in the window: the server, hivemind-soakf-6, ran `api` 1, `cache`, `papi` 0, `www` 0 and `spread` 3 and 5; hivemind-soakf-2 `front`, `papi` 1, `proot` 1, `www` 1 and `spread` 0 and 4; hivemind-soakf-3 `api` 0, `kv`, `proot` 0, `www` 2 and `spread` 1 and 2; hivemind-soakf-4, back from its power-off, none, so its router reached every replica over the network.

| Through the routers | |
|---|---|
| Requests, all 16 streams | 576765 |
| Succeeded | 576765 (100 %) |
| Misrouted (a `200` for a path from the other app) | 0 |
| Longest gap between two successful requests on a stream | 0.13 s |
| Total time per request (TCP connect, TLS handshake, request and reply), p50 / p90 / p99 / p99.9 / max | 2.5 / 3.1 / 4.0 / 7.2 / 34.2 ms |
| TLS handshake, p50 / p99 / max | 1.5 / 2.7 / 29.0 ms |
| TLS done to first byte of the reply, p50 / p99 / max | 0.6 / 1.3 / 21.8 ms |
| Certificates issued in the window | 18 (5 for `www`, 7 for `api`, 6 for `paths`); some hosts twice within 10 minutes, from Pebble's renewal window (above) |
| Certificate checks (a fresh handshake with every router for each host every 30 s) | 1452, none failed |

Per host and path, over the four routers:

| Host and path | Requests | Succeeded | Misrouted | p50 / p99 / p99.9 / max (ms) |
|---|---|---|---|---|
| `api.soak.test/` | 144201 | 144201 | 0 | 2.4 / 3.9 / 7.2 / 30.0 |
| `www.soak.test/` | 144186 | 144186 | 0 | 2.5 / 4.0 / 7.4 / 30.2 |
| `paths.soak.test/` (proot) | 144187 | 144187 | 0 | 2.5 / 4.0 / 7.2 / 27.4 |
| `paths.soak.test/api/x` (papi) | 144191 | 144191 | 0 | 2.5 / 4.0 / 7.2 / 34.2 |

| Probe | Result |
|---|---|
| Status answered, converged, all 18 replicas running, no replica on two machines | 363 of 363 |
| `kv` value read back, and the same as written | 363 of 363 |
| `GET /v1/status`, `/v1/nodes`, `/v1/apps` on the server, 305 each | all 200; p50 2.1, 1.8 and 1.0 ms; p99 8.9, 7.8 and 6.9 ms; max 15.6, 10.6 and 12.8 ms |
| Dangling Docker volumes, every node, every 60 s | 0 (and 0 in every sample of the whole run) |
| Container log files | at most 2 files for one container, the largest 10000122 bytes (`api`'s nginx on the server) |
| Events: lost nodes, restarts, failed probes | none |

The hivemind process on each node, 61 samples a minute apart:

| Node | RSS first / last / max (MiB) | Threads first / last | Open fds first / last / max | CPU, % of one CPU | Restarts |
|---|---|---|---|---|---|
| hivemind-soakf-6 (server) | 27.2 / 27.3 / 27.8 | 9 / 9 | 41 / 42 / 44 | 4.07 | 0 |
| hivemind-soakf-2 | 22.7 / 23.2 / 23.5 | 8 / 9 | 23 / 23 / 25 | 3.23 | 0 |
| hivemind-soakf-3 | 22.7 / 23.0 / 23.4 | 8 / 9 | 23 / 23 / 25 | 3.53 | 0 |
| hivemind-soakf-4 | 21.6 / 21.9 / 22.2 | 8 / 8 | 24 / 24 / 26 | 2.65 | 0 |

Each process used 2.7 to 4.1 % of one CPU, against 1.6 to 2.5 % in the third soak: each router here did 40 TLS handshakes and proxied requests a second, twice the third soak's. The RSS of each process stayed within 0.8 MiB. The store was 220 KiB (`hivemind.db`) with a 4027 KiB WAL, the same at both ends.

The routers' counters in the server's `/metrics`, read at the window's two ends, against the clients' `200`s through each router for each host (the counters have no path label, so the two path streams count together for `paths.soak.test`):

| Router | `api.soak.test` | `www.soak.test` | `paths.soak.test` |
|---|---|---|---|
| hivemind-soakf-6 (the server, read directly) | +22 | +15 | +18 |
| hivemind-soakf-2 | +6 | +6 | +9 |
| hivemind-soakf-3 | +6 | +6 | +12 |
| hivemind-soakf-4 | +4 | +6 | +12 |

The server's router counted 22, 15 and 18 more `200`s than its streams had. The three hosts had 7, 5 and 6 issuances in the window, and Pebble made three HTTP-01 validations for each, 21, 15 and 18, all through the first address of the hosts' DNS answers, the server's. The workers' differences are 0.4 to 0.6 s of their streams' traffic, the difference between the posting lag at the two reads (a worker posts its counters every 15 s). Apart from the old server's router, no client got a `503` in the whole run.

## What did not work, and what was not measured

- The harness's DNS answers for the routed hosts kept the old server first after the move (above), so no certificate was renewed from 13:29 to 13:33. They were valid until 14:16, and nothing failed.
- One worker was sent to the old server, for 30 s; a resolver change reaches workers at different times, and with both servers answering one name a worker could reach either. After the steady state the same was done again, 14:36:31 to 14:38:41, to look at the worker's sockets: 5, 30 and 60 s after its name led to the restored server again, its one established connection to port 4484 was the same one, to the old server (`ss`); 7.9 s after the old server was stopped it was talking to the restored server. Its replicas were placed again and ran twice in that window too; it is not in the steady state.
- In the old server's 5 minutes its router renewed nothing: no certificate in its store was due when it started, and it was stopped before its first 10-minute check. A longer stay would show whether its renewals fail, as they should: Pebble validates through the cluster's routers, which do not have its challenges.
- The record's change was a line in `/etc/hosts` on every machine at once, as in the third soak.
- One hour of steady state is too short for leak figures.

## Running it, with path routes

As [at a new address](#running-it-at-a-new-address), with `SOAK_PATHS=1`:

```sh
export SOAK_DIR=$PWD/.scratch/run SOAK_ROUTES=1 SOAK_PATHS=1 SERVER_NAME=hivemind-server.soak.test
scripts/soak/outside.sh setup
BIN=path/to/hivemind scripts/soak/setup.sh       # proot and papi too, and the path checks
scripts/soak/outside.sh client-start             # four streams per router
scripts/soak/scenario.sh probe-start
scripts/soak/scenario.sh watch-start

# the server lost; the record changes 60 s after the restored API answers
scripts/soak/scenario.sh begin lose-server-late node-1
scripts/soak/scenario.sh server-backup
DNS_DELAY=60 scripts/soak/scenario.sh server-move - - name lose-server-late &   # ...power node-1 off, launch node-6, then:
echo "node-6 10.0.0.9" >"$SOAK_DIR/new-server"

# the old server's machine back on, one worker sent to it for 30 s
scripts/soak/scenario.sh old-back node-1 10.0.0.5 node-2 &    # ...start node-1's machine; afterwards, stop it

scripts/soak/scenario.sh rollout 2 papi
scripts/soak/scenario.sh rollout 2 proot
scripts/soak/scenario.sh rollout 1 proot papi
scripts/soak/scenario.sh cert-restart node-3

scripts/soak/outside.sh fetch
SOAK_DURATIONS=scripts/soak/durations-soak4.txt scripts/soak/report.sh "$SOAK_DIR/soak.log"
scripts/soak/client-report.sh "$SOAK_DIR/soak.log" "$SOAK_DIR/client"
```

# Fifth soak: rotations of the cluster CA, and the start grace's hold

The fourth soak's set-up on six new machines, on 2026-11-18, on the build that adds the rotation of the cluster CA ([ca-rotation](ca-rotation.md)), a new connection after a refused reply, the refusal kept by the address that answered, and the start grace's hold. Under the 16 client streams the CA was rotated four times: with every node up; with a worker powered off, finished with `--force`, and the worker removed and joined again; aborted in phase renew and finished back to the CA it came from; and with the server lost in phase renew, a backup from that phase restored on a new machine by name, and the rotation finished there. Then the server's machine was stopped and started again with every worker cut off from it, past its start grace, and an hour of steady state followed. Times are UTC; the machines' private addresses are shown renumbered, 10.0.0.N for hivemind-soakg-N. The CA from `init` is named `hivemind cluster CA`; each one a rotation makes has the first 8 hex digits of its fingerprint after that, and is called by them here, as 83d3dd58.

## The four rotations

| | Every node up | A worker powered off | Aborted in phase renew | The server lost in phase renew |
|---|---|---|---|---|
| CAs | init's to 83d3dd58 | 83d3dd58 to 50f80d35 | 50f80d35 to ef678147, then back | 50f80d35 to c63be9fc |
| `start` to every node trusting the new CA (live nodes) | 1.2 s | 1.2 s | 1.2 s | 1.2 s |
| `renew` to every node renewed (live nodes) | 1.8 s | 1.8 s | aborted 1.06 s after `renew` | 1.8 s |
| The server's certificate moves to the new CA | in the `renew` step | in the `finish --force` step | in the `renew` step, and back to 50f80d35 0.96 s after the abort | in the `renew` step; the restored server's from its start |
| `finish` | passed | refused, naming the worker; `--force` passed | passed, 2.7 s after the abort | passed on the restored server |
| `finish` to every worker with that CA alone in `ca.crt` | 1.5 s | 1.4 s | 1.5 s | 1.5 s |
| Failed requests, 16 streams | 0 | 0 through the three routers that stayed up | 0 | 0 through the workers' routers and the new server's |
| Requests of 1.5 s or more | 0 | 2 per stream of an app with a replica on the powered-off worker | 0 | 4 or 5 per stream of an app with a replica on the lost server |
| TLS handshake errors in the server's log | 0 | 7 from the cut-off worker, 18 from the harness's probe on it | 0 | 0 |
| Containers changed | none | the powered-off worker's replicas, placed again when it was lost | none | none on the workers |
| Nodes that joined again | none | the cut-off worker, after `hivemind node remove` | none | none |

Each step's command, run over ssh from the control machine, returned in 54 to 65 ms.

## The machines and the build

| | |
|---|---|
| Machines | 6 EC2 c7g.large in one subnet: 2 vCPU (Graviton3), 3.7 GiB (MemTotal 3897240 kB), Amazon Linux 2023.12.20261117, kernel 6.18.62-127.179.amzn2023.aarch64, Docker 25.0.18 |
| Roles | hivemind-soakg-1 the first server, 2 to 4 workers, 5 the outside machine, 6 the new server machine, launched during the run at a new address |
| hivemind | 6027284, `CGO_ENABLED=0 go build -trimpath ./cmd/hivemind`, Go 1.27.4, linux/arm64, sha256 651ff9b9...2c6b9d8b, for the whole run |
| Service | As in the fourth soak: the units of [systemd.md](systemd.md), `--advertise hivemind-server.soak.test --cluster-addr <its address>` on the server, the router's flags on every node, `--start-grace` at its default, 2 minutes |
| Outside machine | Pebble (revision 5caaab3), pebble-challtestsrv, registry:2, curl 8.21.0 |
| Network | As in the fourth soak, checked again before the set-up: TCP 80, 443, 4484, 4485, 14000, 32768, 40000 and 60999 open from one machine to a listener on another, 8000 closed. Clocks: chrony put every machine within 3.4 microseconds of the Amazon Time Sync Service at the start |

## Set-up

As in the fourth soak, 18 replicas with the path-routed pair. It took 61 s from the binaries' install to every router serving the three hosts, and every path of the fourth soak's table went to the same app through each of the four routers, with the same `301` and `400`s.

## Method

The fourth soak's harness and clients, 16 streams. New in [scripts/soak](../scripts/soak):

- `scenario.sh ca-rotate` runs a rotation as [ca-rotation](ca-rotation.md) says: `hivemind ca rotate start`; `renew` once `hivemind ca status --json` shows every node trusting the new CA (polled every 0.5 s); `finish` once every node has renewed and the server's certificate has moved; then until every worker's `ca.crt` holds the new CA alone.
- During each rotation a watcher logs, at each change, the server's view once a second (the rotation in `ca status --json`, the CA files in its data directory, and the issuer and serial of the certificate its join listener presents, which the node listener presents too) and each worker's every 2 s (the CAs in its `ca.crt`, in order; the issuer, serial and expiry of its `node.crt`; whether the server's certificate verifies against `ca.crt` for the server's name).
- `ca-offline T` waits for T to be powered off and lost, rotates, expects `finish` to refuse and runs `finish --force`, waits for T to be powered on, logs the line T's journal gives for its failed handshakes and what T runs, then runs `hivemind node remove` and joins T again with an empty data directory (`setup.sh rejoin`).
- `ca-abort` runs `renew`, then polls `GET /v1/ca` on the server every 50 ms and aborts once at least one node has renewed and not every one has.
- `ca-restore-begin` rotates to phase renew, waits until every node has renewed and the server's certificate has moved, and takes the backup; `server-move` measures the loss and the restore by name as in the fourth soak, and `ca-finish` finishes the rotation on the restored server.
- `start-hold` waits for the server's machine to stop, drops every worker's packets to the server's port 4484 (iptables), waits for the machine to start again, and logs once a second the server's view of its workers, the containers on its machine and its status line, until 90 s past the end of its start grace; then removes the rules and measures until every worker is ready. With `HEAL_LATE=W` the rule on W comes off `HEAL_LAG` (12) s after the others.
- After each rotation: the containers on every node against those before it (ID and start time), the node IDs in `hivemind nodes` and in each worker's `node.json` against the map, and the TLS handshake errors, x509 lines, refusals and joins in the server's and the workers' journals.

## Results

### A rotation with every node up

| | UTC | Seconds |
|---|---|---|
| `hivemind ca rotate start`: 83d3dd58 | 21:58:10.52 | |
| Each worker adds it (`this worker trusts a new cluster CA, cross-signed by one it trusted`) | 21:58:10.59, 10.86, 11.33 | +0.08, +0.35, +0.81 |
| `renew`; in the same step the server's certificate moves to 83d3dd58, as every node trusts it | 21:58:12.14 | |
| Each worker renews (`renewing the node certificate under the cluster's new CA`) | 21:58:12.32, 12.59, 12.86 | +0.18, +0.46, +0.72 |
| The server sees each new certificate (`a node presents a certificate from the new cluster CA`) | 21:58:13.32, 13.59, 13.86 | 1.0 s after each renewal |
| `finish` | 21:58:14.41 | start + 3.90 |
| Each worker drops init's CA (`this worker no longer trusts a cluster CA`) | 21:58:14.59, 14.86, 15.33 | +0.18, +0.45, +0.91 |

On disk, through the phases, the same on every worker:

| Phase | Worker's `ca.crt`, in order | Worker's `node.crt` from | Server's data directory | Server's certificate from |
|---|---|---|---|---|
| Before | init's | init's | `ca.crt`, `ca.key` | init's |
| trust | init's, 83d3dd58 | init's | and `ca-next.crt` (83d3dd58), `ca-next.key` | init's |
| renew, after the worker renewed | 83d3dd58, init's | 83d3dd58, a new serial, to 2027-11-18 21:58:12 | as in trust | 83d3dd58 |
| After `finish` | 83d3dd58 | 83d3dd58 | `ca.crt` (83d3dd58), `ca.key`; no `ca-next.*` | 83d3dd58 |

The server's certificate verified against every worker's `ca.crt` at every poll. No request on the 16 streams failed or took 1.5 s; the longest gap between two successful requests on a stream was 0.11 s and the slowest request 10 ms. The server logged no TLS handshake error, no refused request, no warning, no error and no join, and the workers' journals had no TLS or x509 line. No container on any node changed (the same IDs and start times before and after), and every node kept its ID and its `node.json`. The server's `hivemind_node_cert_expiry_timestamp_seconds` had the new expiry of every node.

### A rotation with a worker powered off

hivemind-soakg-4 (10.0.0.4), with `api` 1, `cache`, `papi` 0, `spread` 3 and `www` 0, was powered off with `sysrq o` at 22:00:17.7. Its port 22 closed 7.4 s later, it was lost 15.0 s after that, and its five replicas ran elsewhere 6.4 s later.

| | UTC |
|---|---|
| `start`: 50f80d35 | 22:00:48.73 |
| `ca status`: every live node trusts it; hivemind-soakg-4 is `down` | 22:00:49.96 |
| `renew`, without `--force`: passed | 22:00:52.26 |
| The two live workers renewed; the server's certificate stays from 83d3dd58, as hivemind-soakg-4 does not trust 50f80d35 | 22:00:54.05 |
| `finish`: refused | 22:01:01.45 |
| `finish --force`: the server's certificate moves to 50f80d35, and the rotation ends | 22:01:01.50 |
| The two live workers hold 50f80d35 alone | 22:01:02.93 |
| hivemind-soakg-4 started | 22:01:21.47 |
| Its port 22 open | 22:01:35.83 |
| Its first failed handshake | 22:01:37.19 |
| `hivemind node remove` | 22:02:09.07 |
| Joined again with an empty data directory and a new token; converged | 22:02:10.57 |

`finish` named it and why:

```
error: hivemind ca rotate finish waits for 1 node(s): node-3c5d8032c4c8 (...) is down: its last lease was at 2026-11-18T22:00:22Z. Run it again once hivemind ca status lists none, or with --force to cut them off: each must then join again
```

Coming back, the worker logged at each attempt, every 5 s, the line [ca-rotation](ca-rotation.md#nodes-that-are-offline) describes:

```
level=WARN msg="registering with the server; retrying" server=hivemind-server.soak.test:4484 err="calling the server: Get \"https://hivemind-server.soak.test:4484/node/v1/nodes/node-3c5d8032c4c8/status\": tls: failed to verify certificate: x509: certificate signed by unknown authority; the server's certificate is from a CA this worker does not trust: the address is another cluster's server, or this cluster's CA was rotated without this node (hivemind ca rotate finish --force). For the second, run `hivemind node remove <id>` on the server, then join this machine again with an empty --data-dir and a new join token"
```

Its `ca.crt` held 83d3dd58 alone and its `node.crt` was from it. It ran no container: the power-off had stopped them, and its agent started none without the server, so no replica ran twice. The server had it `lost`, and logged each attempt as `http: TLS handshake error from 10.0.0.4:<port>: remote error: tls: bad certificate`, with the address and nothing more. Eighteen more lines from that address, `local error: tls: bad record MAC`, came from the harness's own check on that machine, which aborted its handshakes with the join listener when the check failed; the check no longer aborts. `hivemind node remove` said `removed; 0 replicas placed again`, as they had been placed when it was lost. It joined as a new node, with 50f80d35 alone in `ca.crt` and a certificate from it.

Through the other three routers no request failed. On each stream of `www`, `api` and `papi`, which had a replica on the powered-off worker, two requests took longer: one of 7.5 s on a kept connection to it, then one 2.0 s dial, as in the fourth soak. Through its own router 383 requests per stream failed, from 5 s after the power-off until 112 s after it, when its router served again after the join.

### Aborted in phase renew

| | UTC, the server's clock |
|---|---|
| `start`: ef678147; every node trusts it 1.2 s later | 22:03:05.48 |
| `renew`; the server's certificate moves to ef678147 | 22:03:07.19 |
| The three workers renew under ef678147 | 22:03:07.23, 07.59, 07.86 |
| The server sees hivemind-soakg-4's new certificate | 22:03:08.21 |
| `abort`: the poll before it had 1 of 3 nodes renewed (the other two had renewed, and not yet sent a request with their new certificates) and the server's certificate from ef678147 | 22:03:08.25 |
| The rotation turns round, back towards 50f80d35, in phase renew | 22:03:08.27 |
| The three workers renew back under 50f80d35 | 22:03:08.60, 08.87, 09.23 |
| The server's certificate back to 50f80d35 | 22:03:09.23 |
| The server sees every node's certificate from 50f80d35 | 22:03:10.21 |
| `finish`: passed, ef678147 deleted | 22:03:10.93 |
| Every worker holds 50f80d35 alone | 22:03:12.40 |

No request failed or took 1.5 s, the slowest took 12 ms; there was no TLS error on either side, no container changed and no node joined again. From their renewal back until `finish`, each worker's `ca.crt` had ef678147 first and 50f80d35 second, while its `node.crt` was from 50f80d35, where the docs say the CA of the node's own certificate comes first. In the turned rotation the workers' and the server's lines call 50f80d35 the new CA: `renewing the node certificate under the cluster's new CA ... ca=50f80d3521d8db84`, `a node trusts the new cluster CA ... ca=50f80d3521d8db84`.

### A backup in phase renew, restored on a new machine by name

`start` (c63be9fc), `renew` once every node trusted it, and every node renewed with the server's certificate moved 3.0 s after `start`; then `hivemind backup`, 225792 bytes, with `ca-next.key` and `ca-next.crt` beside `ca.key` and `ca.crt`. The server, hivemind-soakg-1, was powered off, and the backup was restored on hivemind-soakg-6 at 10.0.0.6 as in the fourth soak, with the name's record changed as soon as the restored API answered.

| | UTC | Seconds |
|---|---|---|
| Backup | 22:04:39.4 | |
| Power-off request | 22:04:54.6 | |
| API down (1 s measure) | 22:05:05.7 | +11.0 |
| Launch request for the new machine | 22:05:09.3 | |
| New machine ready | 22:06:41.2 | launch + 91.9 |
| API up | 22:06:44.6 | outage 98.9 |
| The name's record changed on every machine | 22:06:46.0 | |
| hivemind-soakg-3, 2 and 4 ready | 22:06:50.8, 52.4, 52.4 | record + 4.7, 6.4, 6.4 |
| Converged | 22:06:52.4 | record + 6.4 |
| `ca status` on the restored server: phase renew, its certificate from c63be9fc, every node trusting and renewed, nothing to wait for | 22:07:41.9 | |
| `finish`: passed | 22:07:42.2 | |
| Every worker holds c63be9fc alone | 22:07:43.7 | +1.5 |

`restore` said that the backup was taken during a rotation of the cluster CA, in phase renew, that it holds both CAs, and where the docs say which step to run. The restored server issued its own certificate at its start from c63be9fc, the CA the store recorded for it, and the workers, which trusted both CAs and held certificates from c63be9fc, reached it with no step in between. From the backup to the end no container on a worker changed and no node joined again. Through the workers' routers no request failed; each stream of `www` and `papi`, with a replica on the lost server, had 4 or 5 requests of 1.5 s or more, the first of 7.5 s and then the backoff's 2.0 s dials. Through the new server's router no request failed from its first; through the lost server's router 64 to 76 per stream failed until the clients stopped asking it.

### The start grace's hold

The server's machine, hivemind-soakg-6, was stopped (`aws ec2 stop-instances --force`), and once its port 22 had closed, every worker's packets to its port 4484 were dropped. Then the machine was started again.

| | UTC | Seconds |
|---|---|---|
| Stop request | 22:08:11.8 | |
| Port 22 closed; the rules on the three workers | 22:08:17.2 | +5.4 |
| Start request | 22:08:37.8 | |
| Port 22 open | 22:08:53.5 | +15.7 |
| `serve` started: `workers that were up before this start wait for their first report ... waiting=3 start_grace=2m0s`; its agent restarted its own five containers | 22:08:54.3 | |
| API up. `hivemind nodes`: the three workers `waiting`. `hivemind status`: `3 workers are waiting for their first report since the server started (start grace: 1m59s left); ...` | 22:08:55.6 | |
| The grace ends: the error line; `nodes`: still `waiting`, with `start_held`; `status`: `3 workers are waiting: none of them has reported since the server started, and its start grace has ended, so the server keeps them waiting and places none of their replicas again; ...` | 22:10:54.3 | start + 120.0 |
| The rules removed on the three workers | 22:12:25.7 | grace + 91.4 |
| Each worker reaches the server (`the server answers again`) | 22:12:25.9, 26.3, 27.1 | +0.18, +0.63, +1.46 |
| Converged, every worker ready | 22:12:27.7 | +2.0 |
| The sweep: `a worker that was up before this server started has reported to it: the start grace holds the others no longer, and those that have not reported are lost` | 22:12:29.3 | |

The error line was the one [backup](backup.md#replacing-a-lost-server) shows, with `waiting=3`, the three node IDs, `started=2026-11-18T22:08:54Z` and `start_grace_ended=2026-11-18T22:10:54Z`. Through the hold the server ran its own five replicas (`front`, `papi` 1, `spread` 0 and 4, `www` 1) and nothing else; no node was lost, no replica was placed again, and no container on a worker changed. Its router served again from 22:08:54.7, 0.4 s after `serve` started, with the workers' endpoints in its table while they waited. Through the workers' routers no request failed; one request per stream of `www` and `papi` took 2.0 s, a dial to the server's replica while it was down. Through the server's router 138 or 139 requests per stream failed, from the stop until 22:08:54.7.

For the second after the first two workers had reported, `hivemind nodes` showed the third, hivemind-soakg-2, as `lost`, with a lease age of 251 s, until it reported 1.0 s later. Once one waiting worker has reported, the others are no longer waiting, and the sweep, every 5 s, marks them lost and places their replicas at its next pass; here that pass came after all three had reported.

After the steady state the same was done again, with hivemind-soakg-2's rule taken off 12 s after the others' (`HEAL_LATE`), as a worker whose agent is in a longer backoff would come back:

| | UTC |
|---|---|
| `serve` started; the grace ends with the error line and the three workers held, as above | 23:17:15.8, 23:19:15.8 |
| The rules off on hivemind-soakg-3 and 4 | 23:20:45.7 |
| hivemind-soakg-3 reaches the server; its router reads the table at 45.689, its agent is answered at 45.698 | 23:20:45.70 |
| The sweep: the hold ends, and hivemind-soakg-4 and hivemind-soakg-2 are lost (`node lost` for each) | 23:20:45.78 |
| hivemind-soakg-4 reaches the server, 0.52 s after it was marked lost; it ran no replica | 23:20:46.30 |
| hivemind-soakg-2's six replicas without a volume (`api` 1, `papi` 0, `proot` 0, `spread` 1 and 5, `www` 2) started again, all on the server's machine | 23:20:47.9 to 49.5 |
| The rule off on hivemind-soakg-2 | 23:20:57.7 |
| hivemind-soakg-2 reaches the server; its routed containers leave its table and stop after the 5 s drain delay | 23:20:58.0 |
| Its six containers removed; no replica on two machines | 23:21:03.4 to 08.3, 23:21:08.9 |

The six replicas ran on two machines for 19.9 s, and stayed on the server's machine, which then ran 11 of the 18 while hivemind-soakg-2 and 4 ran none. Through hivemind-soakg-3's router, `api.soak.test` and `paths.soak.test/` answered `503` from 23:20:45.70 to 46.51, 9 requests on each stream: the table its router read as it came back had neither hivemind-soakg-2's endpoints, which stopped counting when the hold ended, nor its own, which count again once its agent has reported its containers (45.745); the router read the table again a second later. Both apps had no replica on the server. No other request through a worker's router failed.

## Steady state

On 6027284, 1 h 0 min 40 s, from 22:15:19.0 to 23:15:59.0, after the four rotations and the first hold, with no fault and no change to the apps: the 16 client streams, the probe loop, no watcher. Every node held a certificate from c63be9fc, the fourth rotation's CA, and trusted it alone. Placement in the window: the server, hivemind-soakg-6, ran `front`, `papi` 1, `spread` 0 and 4 and `www` 1; hivemind-soakg-2 `api` 1, `papi` 0, `proot` 0, `spread` 1 and 5 and `www` 2; hivemind-soakg-3 `api` 0, `cache`, `kv`, `proot` 1, `spread` 2 and 3 and `www` 0; hivemind-soakg-4, joined again in the second rotation, none.

| Through the routers | |
|---|---|
| Requests, all 16 streams | 578441 |
| Succeeded | 578441 (100 %) |
| Misrouted (a `200` for a path from the other app) | 0 |
| Requests of 1.5 s or more | 0 |
| Longest gap between two successful requests on a stream | 0.13 s |
| Total time per request (TCP connect, TLS handshake, request and reply), p50 / p90 / p99 / p99.9 / max | 2.5 / 3.1 / 4.0 / 7.6 / 28.6 ms |
| TLS handshake, p50 / p99 / max | 1.5 / 2.7 / 22.5 ms |
| TLS done to first byte of the reply, p50 / p99 / max | 0.6 / 1.3 / 18.7 ms |
| Certificates issued in the window | 17 (6 for `www`, 6 for `api`, 5 for `paths`), each by a router whose node certificate was from c63be9fc, through the server's certificate storage |
| Certificate checks (a fresh handshake with every router for each host every 30 s) | 1452, none failed |

Per host and path, over the four routers:

| Host and path | Requests | Succeeded | Misrouted | p50 / p99 / p99.9 / max (ms) |
|---|---|---|---|---|
| `api.soak.test/` | 144640 | 144640 | 0 | 2.4 / 3.9 / 7.3 / 25.9 |
| `www.soak.test/` | 144600 | 144600 | 0 | 2.5 / 4.0 / 8.0 / 24.3 |
| `paths.soak.test/` (proot) | 144602 | 144602 | 0 | 2.5 / 4.0 / 7.7 / 28.6 |
| `paths.soak.test/api/x` (papi) | 144599 | 144599 | 0 | 2.5 / 4.0 / 7.3 / 26.6 |

| Probe | Result |
|---|---|
| Status answered, converged, all 18 replicas running, no replica on two machines | 364 of 364 |
| `kv` value read back, and the same as written | 364 of 364 |
| `GET /v1/status`, `/v1/nodes`, `/v1/apps` on the server, 305 each | all 200; p50 2.4, 2.2 and 1.0 ms; p99 12.0, 10.0 and 6.0 ms; max 17.3, 13.3 and 12.8 ms |
| Dangling Docker volumes, every node, every 60 s | 0 on three nodes; 1 on hivemind-soakg-4 in every sample since 22:02:25, the anonymous volume of its old `cache` container, which the harness's rejoin removed with `docker rm -f` and without `-v` |
| Container log files | at most 2 files for one container, the largest 10000055 bytes |
| Events: lost nodes, restarts, failed probes | none |

The hivemind process on each node, 61 samples a minute apart:

| Node | RSS first / last / max (MiB) | Threads first / last | Open fds first / last / max | CPU, % of one CPU | Restarts |
|---|---|---|---|---|---|
| hivemind-soakg-6 (server) | 26.8 / 27.1 / 27.3 | 9 / 9 | 40 / 40 / 45 | 4.07 | 0 |
| hivemind-soakg-2 | 23.5 / 23.5 / 23.8 | 8 / 9 | 24 / 24 / 25 | 3.54 | 0 |
| hivemind-soakg-3 | 23.4 / 23.7 / 23.7 | 9 / 9 | 22 / 23 / 24 | 3.07 | 0 |
| hivemind-soakg-4 | 22.1 / 22.1 / 22.2 | 9 / 9 | 22 / 22 / 25 | 2.86 | 0 |

Each process used 2.9 to 4.1 % of one CPU, against 2.7 to 4.1 % in the fourth soak, and its RSS stayed within 0.5 MiB over the hour. The store grew from 212 to 228 KiB (`hivemind.db`), with a 4027 KiB WAL at the end. The routers' counters in the server's `/metrics`, read at the window's two ends, against the clients' `200`s through each router:

| Router | `api.soak.test` | `www.soak.test` | `paths.soak.test` |
|---|---|---|---|
| hivemind-soakg-6 (the server, read directly) | +18 | +18 | +15 |
| hivemind-soakg-2 | -101 | -100 | -202 |
| hivemind-soakg-3 | -100 | -100 | -200 |
| hivemind-soakg-4 | +48 | +49 | +99 |

The server's router counted 18, 18 and 15 more `200`s than its streams had: Pebble's three HTTP-01 validations for each of the 6, 6 and 5 issuances, all through the first address of the hosts' answers, the server's. A worker posts its counters every 15 s: hivemind-soakg-2's and 3's were 10 s further behind the clients at the second read than at the first (100 requests of each stream, at 10 a second), and hivemind-soakg-4's 4.9 s less behind.

## What did not work, and what was not measured

- The first hold ended without a node lost only because the sweep's pass came after the last of the three workers had reported; with one worker 12 s behind the others, its replicas ran twice and moved, and one router answered `503` for 0.8 s (above).
- Each step ran as soon as its condition held, so each rotation took seconds. A rotation left in phase trust or renew for hours, with restarts, renewals and joins in between, was not run.
- The powered-off worker was down before `start`, so it never saw the rotation. A worker that goes down after it trusts the new CA and before it renews was not run.
- The backup was taken in phase renew, after every node had renewed. A backup from phase trust restored after the nodes renewed, for which the docs say to run `renew` again, was not run; nor a restore across `finish`.
- No worker of an earlier build took part; integration-mixed.sh does that in Docker.
- The harness's check of each worker's view aborted its handshakes with the join listener when the check failed, which the server logged as TLS errors (18 lines in the second rotation). It no longer aborts. Its rejoin removed the old containers with `docker rm -f`, which left the anonymous volume of the `cache` container (redis declares `VOLUME /data`) dangling on hivemind-soakg-4; it now removes them with `-v`.
- The record's change was a line in `/etc/hosts` on every machine at once, as in the third and fourth soaks.
- One hour of steady state is too short for leak figures.

## Running it, with CA rotations

As [with path routes](#running-it-with-path-routes):

```sh
export SOAK_DIR=$PWD/.scratch/run SOAK_ROUTES=1 SOAK_PATHS=1 SERVER_NAME=hivemind-server.soak.test
scripts/soak/outside.sh setup
BIN=path/to/hivemind scripts/soak/setup.sh
scripts/soak/outside.sh client-start
scripts/soak/scenario.sh probe-start
scripts/soak/scenario.sh watch-start

scripts/soak/scenario.sh ca-rotate
scripts/soak/scenario.sh ca-offline node-4 &       # power node-4 off, and on, when the log says so
scripts/soak/scenario.sh ca-abort
scripts/soak/scenario.sh ca-restore-begin           # rotates to phase renew and takes the backup
scripts/soak/scenario.sh server-move - - name ca-restore &   # ...power node-1 off, launch node-6, then:
echo "node-6 10.0.0.9" >"$SOAK_DIR/new-server"
scripts/soak/scenario.sh ca-finish ca-restore-finish
scripts/soak/scenario.sh start-hold &               # stop the server's machine, then start it, when the log says so
HEAL_LATE=node-2 scripts/soak/scenario.sh start-hold &

scripts/soak/outside.sh fetch
SOAK_DURATIONS=scripts/soak/durations-soak5.txt scripts/soak/report.sh "$SOAK_DIR/soak.log"
scripts/soak/client-report.sh "$SOAK_DIR/soak.log" "$SOAK_DIR/client"
```

# Sixth soak: gRPC through the routers, a partial heal after the hold

The fifth soak's set-up on five new machines, on 2026-11-19, on fc4ac4b, with a gRPC app on an h2c route beside the path-routed pair, and each worker run by the unit [systemd](systemd.md) gives, `serve --role worker` with no flags. Besides the 16 HTTP streams, the outside machine ran two gRPC clients through each router: a unary call every 200 ms, and a server stream held for 1000 messages, one a second. Under them: the server's machine stopped and started with every worker cut off from it past its start grace, and the workers healed 0, 5 and 20 s apart; a CA rotation, and one aborted in phase renew; rolling updates of the gRPC app and of the path apps; a worker powered off; and an hour of steady state. The first scenario showed the workers' routers sending requests to the host ports the server's replicas had before its machine restarted, which other replicas held after it, so after the steady state the server's machine was restarted once more with the workers cut off, and healed inside the grace, to measure that. Times are UTC; the machines' private addresses are shown renumbered, 10.0.0.N for hivemind-soaki-N.

## The scenarios

| | Failed HTTP requests | Misrouted (a 200 from another app) | Failed unary gRPC calls | Held server streams that ended early |
|---|---|---|---|---|
| The hold, healed 0, 5 and 20 s apart (the workers' routers) | 0 | not measured for `api` (below) | 1424, `502` | 1 of 3, on the server's replica, at the stop |
| A CA rotation | 0 | 0 | 0 | 0 of 4 |
| A rotation aborted in phase renew | 0 | 0 | 0 | 0 of 4 |
| Rolling update of `grpc` | 0 | 0 | 0 | 4 of 4, 13 s after their copy left the table |
| Rolling update of `proot` and `papi` | 0 | 0 | 0 | 0 of 4 |
| A worker powered off (the other three routers) | 0 | 0 | 3, `502`, one per router | 1 of 3, on the powered-off worker's replica |
| Steady state, 1 h 1 min 49 s | 0 of 588984 | 0 | 0 of 73491 | 0 of 14 |
| The server's machine restarted, workers cut off (the workers' routers) | 0 | 716, `papi`'s requests answered by `proot` | 712, `502` | 2 of 3, on the server's replicas |

No node was lost in either hold, no replica ran twice, and no request got a `503` through any router.

## The machines and the build

| | |
|---|---|
| Machines | 5 EC2 c7g.large in one subnet: 2 vCPU (Graviton3), 3.7 GiB (MemTotal 3897240 kB), Amazon Linux 2023.12.20261118, kernel 6.18.62-127.179.amzn2023.aarch64, Docker 25.0.18 |
| Roles | hivemind-soaki-1 the server, 2 to 4 workers, 5 the outside machine |
| hivemind | fc4ac4b, `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=v2.0.0-278-gfc4ac4b -X main.commit=fc4ac4b1815c" ./cmd/hivemind`, Go 1.27.4, linux/arm64, sha256 bc6703b0...67a50c42, for the whole run; `hivemind nodes` showed v2.0.0-278-gfc4ac4b under VERSION |
| The gRPC server and client | `test/h2ctest`, built the same way without `-ldflags`, sha256 39b3bbf6...6b29fffb. The image is the binary alone, made with `docker import` and `ENTRYPOINT ["/h2ctest"]` as [routing](routing.md#tests)'s test makes it, pushed to the private registry as `soak/h2ctest:1` and `soak/h2ctest:2`, which differ in a label |
| Service | The units of [systemd](systemd.md), with `RestartPreventExitStatus=3`: the server's `serve --advertise hivemind-server.soak.test --cluster-addr <its address>` with the router's flags; each worker's `serve --role worker` with no flags |
| Outside machine | Pebble (revision 5caaab3), pebble-challtestsrv, registry:2, curl 8.21.0 with nghttp2 1.59.0 |
| Network | The fifth soak's security group, not checked port by port this time; every port the run used answered. Clocks: chrony put every machine within 1 microsecond of the Amazon Time Sync Service at the start |

## Set-up

As in the fifth soak, with `grpc` as well: 20 replicas. Each worker was joined as [systemd](systemd.md#worker) says, `hivemind join hivemind-server.soak.test --token ... --route-addr <its address> --acme-email ops@soak.test --acme-ca https://pebble:14000/dir`. join wrote the three router flags to `/var/lib/hivemind/hivemind.yaml`, mode 0600, owned by root, and nothing else; then the foreground join was stopped and the unit started. On each worker its router listened on the worker's address, ports 80 and 443, and `hivemind nodes` showed `yes` under ROUTER, 0.2 s after the unit started. The worker that was powered off later ran its router again from the same unit when it came back.

`grpc` runs `h2ctest serve -addr :50051 -name grpc`, 2 replicas, with `V` in its environment naming the version in every reply, on the route `paths.soak.test` `/h2ctest.Echo`, port 50051, `protocol: h2c`, and a TCP health check, so the host has two http routes and an h2c one. `hivemind routes` showed it with `h2c` under PROTOCOL and both endpoints. Through each of the four routers, a unary call, a server stream of five messages, a bidirectional stream, and a unary call over HTTP/1.1, which the router sends on as HTTP/2, reached `grpc` and ended with `grpc-status` 0. It took 61 s from the binaries' install to every router serving the three hosts, and the fourth soak's paths went to the same apps through each router, with the same `301` and `400`s.

## Method

The fifth soak's harness and its 16 HTTP streams. New in [scripts/soak](../scripts/soak):

- Two gRPC clients through each router (`outside.sh`, `SOAK_GRPC=1`), to `https://paths.soak.test/h2ctest.Echo`. One makes a unary call every 200 ms, each a run of `h2ctest call`: a new process, TCP connection and TLS handshake, HTTP/2 by ALPN, and its checks, that the message comes back from one replica over HTTP/2 with `grpc-status` 0. Its time is the whole run of the process. The other holds a server stream: one POST to `Server` with curl over HTTP/2, asking for 1000 messages 1 s apart, each message's arrival taken from curl's trace (`--trace-time`); when a stream ends, the next one starts 1 s later, and curl gives up after 10 s with no data. `client-report.sh` gives both per window: the calls, failures and why, the versions that answered, the longest gap between two successful calls; the streams that ended, those that ended complete (1000 messages, `grpc-status` 0), the others with curl's error, and the longest gap between two messages.
- `start-hold` with `HEAL_LAGS="W:0 W:5 W:20"` takes each worker's rule off at its own lag after the heal begins, and logs the routing table (`GET /v1/routes`) at each change, every 200 ms, through the heal. With `HOLD_NAME` and a negative `HOLD_AFTER` it heals inside the start grace, with no hold.
- After an abort, `ca-abort` logs `hivemind ca status` and `hivemind status` as an operator reads them, and again once every node has renewed back. After each rotation the harness counts the server's lines that name the CA by the rotation's direction, and each worker's renewals with their `ca=` and `from=`.

## Results

### The start grace's hold, healed 0, 5 and 20 s apart

The server, hivemind-soaki-1, ran `api` 0, `grpc` 1, `kv`, `proot` 1 and `spread` 2. Its machine was stopped (`aws ec2 stop-instances --force`), every worker's packets to its port 4484 were dropped once its port 22 had closed, and the machine was started again. Past the end of its grace, the rules came off hivemind-soaki-2 first, hivemind-soaki-3 5 s later and hivemind-soaki-4 20 s later.

| | UTC | Seconds |
|---|---|---|
| Stop request | 04:51:05 | |
| Port 22 closed; the rules on the three workers | 04:51:06.5 | |
| Start request | 04:51:50.7 | |
| Port 22 open | 04:52:06.9 | +16.3 |
| `serve` started, `waiting=3 start_grace=2m0s`; API up | 04:52:07.5, 08.0 | |
| Its five containers started again | 04:52:08.3 to 09.1 | |
| The grace ends: the error line; in `hivemind nodes` the three workers `waiting`, with `start_held` | 04:54:07.5 | start + 120.0 |
| The rule off hivemind-soaki-2; it reaches the server (`the server answers again`) | 04:55:38.0, 38.2 | grace + 90.5 |
| `hivemind nodes`: hivemind-soaki-3 and 4 `waiting`, with `start_hold_ended`. `hivemind status`: `2 workers are waiting for their first report since the server started: another worker's report ended the hold, and they have the start grace again (1m59s left); ...` | 04:55:38.8 | |
| The sweep: `... which ends the start grace's hold: the workers that have not reported yet wait for the start grace again, from that report, until wait_ends ...`, `waiting=2`, `hold_ended=2026-11-19T04:55:38Z`, `wait_ends=2026-11-19T04:57:38Z` | 04:55:42.5 | |
| The rule off hivemind-soaki-3; it reaches the server | 04:55:43.0, 43.4 | +5.0 |
| The rule off hivemind-soaki-4; it reaches the server | 04:55:58.0, 58.1 | +20.0 |
| Every worker ready; converged, 20 of 20 running | 04:55:59.2 | +21.3 |

No node was lost (no `node lost` in the server's journal), no replica was placed again, none ran on two machines, no container on a worker changed, and every node kept its ID. The routing table, read every 200 ms from the start of the heal, did not change through it: the endpoints of the workers that still waited stayed in it. No request got a `503` through any router. Through the workers' routers no HTTP request failed; on each stream of `api` and `proot`, with a replica on the server, two requests took 2.0 s, a dial to it while it was down. Through the server's router 231 or 232 per stream failed, from the stop until it served again at 04:52:07; none after. Of the held gRPC streams, the two through hivemind-soaki-2's and 4's routers were on `grpc` 0, at hivemind-soaki-4, and ran through it all, a message every 1.00 s; the one through hivemind-soaki-3's router was on the server's replica and was reset at the stop.

But from 04:52:34, 26 s after the server's API answered, until each worker reached the server again, about every second unary call through the workers' routers failed with `502 bad gateway`: 468, 454 and 502 through hivemind-soaki-3's, 2's and 4's, the last of each less than 0.5 s before that worker reached the server.

### The server's replicas on new host ports, and the workers' routers' old table

A routed replica's port is published on an ephemeral host port, and Docker picks a new one at each start of the container; the agent reports the new port, and the table follows. A router that cannot read the table serves its last one (`reading the routing table; serving the last one`). When the server's machine restarted, its agent started its three routed containers again, and they came back on each other's ports:

| Replica on hivemind-soaki-1 | Host port before the stop | After the start |
|---|---|---|
| `api` 0 (nginx) | 32770 | 32768 |
| `proot` 1 (whoami) | 32769 | 32770 |
| `grpc` 1 (h2ctest, HTTP/2 only) | 32768 | 32769 |

The workers' routers, cut off, sent each app's requests to its old address once their backoff for those endpoints, which had failed to dial while the machine was down (3 s, doubling to 30 s), let them try again. gRPC calls went to `api`'s nginx: its log on the server has 1424 lines `"PRI * HTTP/2.0" 400` from the workers' addresses between 04:52:34 and 04:55:57, the number of the clients' `502`s, and the workers' routers counted 469 and 455 `502`s in `/metrics` (hivemind-soaki-4's counters started again with its power-off later). `proot`'s requests went to `grpc`, which speaks only HTTP/2: the connection failed before a reply, the router sent each GET to `proot`'s other replica, and none failed. `api`'s requests went to `proot`'s whoami, which answers any path with a `200`. The `api` streams do not check which app answers, so this was not counted; by the share of `api`'s two endpoints, about half of the `api` requests through the workers' routers from 04:52:34 to their reconnection were answered by `proot`.

After the steady state, to count it, a stream that checks the answering app was added for `api` through each router (`api.soak.test/index.html`, `name=-` for nginx's page, `misroute=proot` for whoami's), every worker was cut off, the server's machine stopped and started again, and the rules removed 70 s after its API answered: inside the start grace, so with no hold.

| | UTC |
|---|---|
| Stop request; port 22 closed, the rules on the three workers | 06:08:46.0, 06:08:52.3 |
| Start request; port 22 open; API up | 06:10:13.7, 06:10:28.0, 06:10:30.1 |
| Its eight containers started again | 06:10:30.4 to 32.2 |
| The first misrouted request and the first `502` through a worker's router | 06:10:53.2 |
| The rules removed | 06:11:40.5 |
| The last misrouted request and `502` | 06:11:41.7 |
| Every worker ready; converged | 06:11:42.7 |

| Replica on hivemind-soaki-1 | Before the stop | After the start |
|---|---|---|
| `api` 0 | 32768 | 32768 |
| `grpc` 1 | 32772 | 32769 |
| `papi` 0 | 32771 | 32770 |
| `proot` 1 | 32773 | 32771 |
| `www` 2 | 32774 | 32772 |

Through hivemind-soaki-3's, 2's and 4's routers, 238, 237 and 241 requests for `paths.soak.test/api/x` got a `200` from `proot` instead of `papi`, whose old port was `proot`'s new one, and 237, 235 and 240 gRPC calls got a `502`, `grpc`'s old port being `www`'s new one: all from 06:10:53.2, 23 s after the server's API answered, to 06:11:41.7, as each router read the table again after the heal. `api` kept its port, and none of its requests went elsewhere; `proot`'s and `www`'s old ports were free, so their requests went to their other replicas. No node was lost, and nothing was misrouted before the stop or after the heal.

### A CA rotation, and an abort in phase renew

| | A rotation | Aborted in phase renew |
|---|---|---|
| CAs | init's to ddf77477 | ddf77477 to 685eced1, then back |
| `start` to every node trusting the new CA | 0.68 s | 1.25 s |
| The server's certificate moves to the new CA | in the `renew` step | in the `renew` step, and back within 1.9 s of the abort (1 s watcher) |
| `renew` to every node renewed | 1.79 s | aborted 71 ms after `renew`, with 1 of 3 renewed |
| `abort` to every node renewed back, the server's certificate back | | 2.39 s |
| `finish` to every worker with that CA alone in `ca.crt` | 0.40 s | 1.43 s |
| `start` to `finish` returned | 3.46 s | 4.75 s |
| Failed HTTP requests and gRPC calls; held streams that ended | 0; 0; 0 | 0; 0; 0 |
| TLS handshake errors, warnings, errors and joins in the server's log; TLS or x509 lines in the workers' | 0 | 0 |
| Containers changed; node IDs | none; the same | none; the same |

Each step's command returned in 54 to 65 ms. The slowest HTTP request in the two rotations took 15 ms and the slowest unary call 18 ms, and the held streams gave a message every 1.00 s throughout.

What the fifth soak found wrong in an abort now reads the right way round:

- `ca.crt`: from each worker's renewal back until `finish`, ddf77477 first and 685eced1 second, with `node.crt` from ddf77477: the CA of the node's own certificate first, as [ca-rotation](ca-rotation.md) says. The forward renewals, under 685eced1, came and went between two of the watcher's samples, 2 s apart, so the order in that second was not seen.
- The logs: before the abort, the server's four lines `a node trusts the CA the rotation goes to, the new one in ca-next.crt` and `a node presents a certificate from the CA the rotation goes to, ...`; after it, six lines `... the CA the rotation goes back to, the old one in ca.crt, after an abort`, with `ca=` the old CA and `leaving=` the new one; no line says "the new cluster CA". Each worker logged two renewals, `renewing the node certificate under the CA the cluster's CA rotation goes to, as its renew phase asks` with `ca=685eced1... from=ddf77477...`, then with `ca=ddf77477... from=685eced1...`.
- `hivemind ca status` right after the abort: `Rotation: phase renew, back towards the old CA, ca.crt ddf77477, after an abort`, the columns TRUSTS TARGET and CERTIFICATE FROM, hivemind-soaki-2 with `yes` and `ca-next.crt 685eced1` (it had renewed forward and not back yet), and under `Next: hivemind ca rotate finish, which waits for:` hivemind-soaki-2 `has not renewed its certificate back under the old CA yet` and each of the other two `has not made a request since the rotation turned round`. `hivemind status`: `Cluster CA rotation, phase renew, back towards the old CA in ca.crt after an abort: hivemind ca rotate finish waits for: ...`. Once every node had renewed back, each showed `yes` and `ca.crt ddf77477`, and `Next: hivemind ca rotate finish.`
- `finish` of the aborted rotation said `The rotation has ended, back on the CA the cluster had before it started (ca.crt ddf77477). Backups taken during the rotation hold the new CA's key, which the cluster no longer uses: delete them. Backups from before the rotation started are still good.`, and of the whole one `The rotation has ended on the new CA (ca.crt ddf77477). Backups taken before now hold the old CA's key, which the cluster no longer uses: take a new backup, and delete the old ones.`

### Rolling updates of grpc and of the path apps

`grpc` to version 2 (`h2ctest:2`, `V=2`): deploy at 04:59:42.7, rollout done 04:59:52.9, converged 05:00:02.6.

| | UTC |
|---|---|
| Replica 0's new copy started, on hivemind-soaki-3 | 04:59:43.8 |
| Its old copy, on hivemind-soaki-4, leaves the table (`stopping it after the drain delay`) | 04:59:48.1 |
| Replica 1's new copy started, on hivemind-soaki-4 | 04:59:48.4 |
| Replica 1's old copy, on the server, leaves the table; removed | 04:59:53.5, 58.8 |
| The four held streams, all on replica 0's old copy, reset | 05:00:01.1 |
| Replica 0's old copy removed | 05:00:01.4 |

Through each router 329 unary calls, none failed, 38 to 41 answered by version 1 and the rest by version 2. The held streams ended with `HTTP/2 stream 1 reset by server (error 0x2 INTERNAL_ERROR)` 13.0 s after their copy left the table: the 5 s drain delay, then the 8 s that h2ctest gives open calls after SIGTERM, within the 15 s that [routing](routing.md#streams-and-their-limits) says a stream on the old copy has. Each client's next stream had its first message 1.2 to 1.9 s after the last of the one before. No HTTP request failed.

`proot` and `papi` to `whoami:2` at once: deploy at 05:00:49.1, both done at 05:01:02.4, converged at 05:01:09.6. On the path streams no request failed or reached the wrong app, the slowest took 56 ms, and the gRPC calls and streams on the same host carried on: none failed, no stream ended.

### A worker powered off

hivemind-soaki-4 (10.0.0.4), with `grpc` 1, `proot` 1, `spread` 1 and 5 and `www` 2, was powered off with `sysrq o`; the clients' requests to it stalled from 05:01:43. Its port 22 closed at 05:01:50.4, it was lost 9.6 s later, and its five replicas ran elsewhere 6.4 s after that, four on the server and `spread` 1 on hivemind-soaki-3. Started again, its port 22 opened 15.7 s after the start request, at 05:03:28.0, its old containers were removed 0.2 s later, and it was ready at 05:03:29.2, with its router up from the same flagless unit.

Through the other three routers no HTTP request failed; on each stream of `www` and `proot` two took longer, one of 7.5 s on a kept connection to it, then a 2.0 s dial, as in the fourth and fifth soaks. One unary gRPC call through each of them failed, with a `502` after 5.5 s: the call in flight on that router's connection to `grpc` 1 when the machine died, ended by TCP_USER_TIMEOUT. A gRPC call is a POST, which the router does not send again ([routing](routing.md#protocols)); the clients make one call at a time, so a client with many in flight would lose each call the router had sent to that replica in those 5 s. The held stream through hivemind-soaki-2's router was on `grpc` 1 and was reset, and the next one's first message came 7.2 s after its last; the other two went on. Through hivemind-soaki-4's own router 80 requests per stream failed, until 05:03:28.

## Steady state

On fc4ac4b, 1 h 1 min 49 s, from 05:05:46.2 to 06:07:35.5, after the scenarios above and before the second restart: the 16 HTTP streams, the 8 gRPC clients, the probe loop, no watcher. Every node held a certificate from ddf77477, the first rotation's CA. Placement in the window: the server, hivemind-soaki-1, ran `api` 0, `grpc` 1, `kv`, `papi` 0, `proot` 1, `spread` 2 and 5 and `www` 2; hivemind-soaki-2 `front`, `proot` 0, `spread` 0 and 4 and `www` 1; hivemind-soaki-3 `api` 1, `cache`, `grpc` 0, `papi` 1, `spread` 1 and 3 and `www` 0; hivemind-soaki-4, powered off and on before, none.

| HTTP, through the routers | |
|---|---|
| Requests, all 16 streams | 588984 |
| Succeeded | 588984 (100 %) |
| Misrouted | 0 |
| Requests of 1.5 s or more | 0 |
| Longest gap between two successful requests on a stream | 0.12 s |
| Total time per request (TCP connect, TLS handshake, request and reply), p50 / p90 / p99 / p99.9 / max | 2.6 / 3.1 / 5.1 / 7.8 / 20.0 ms |
| TLS handshake, p50 / p99 / max | 1.6 / 3.0 / 11.5 ms |
| TLS done to first byte of the reply, p50 / p99 / max | 0.6 / 1.6 / 16.7 ms |
| Certificates issued in the window | 22 (8 for `api`, 8 for `paths`, 6 for `www`) |
| Certificate checks (a fresh handshake with every router for each host every 30 s) | 1476, none failed |

| Host and path | Requests | Succeeded | Misrouted | p50 / p99 / p99.9 / max (ms) |
|---|---|---|---|---|
| `api.soak.test/` | 147283 | 147283 | 0 | 2.5 / 5.1 / 7.7 / 20.0 |
| `www.soak.test/` | 147229 | 147229 | 0 | 2.6 / 5.1 / 7.9 / 14.5 |
| `paths.soak.test/` (proot) | 147229 | 147229 | 0 | 2.6 / 5.0 / 7.8 / 18.1 |
| `paths.soak.test/api/x` (papi) | 147243 | 147243 | 0 | 2.6 / 5.1 / 7.8 / 18.8 |

| gRPC, through the routers | |
|---|---|
| Unary calls, all four routers | 73491 |
| Succeeded | 73491 (100 %), all from version 2 |
| Time per call (a new process, TCP connect, TLS handshake and call), p50 / p90 / p99 / p99.9 / max | 5.8 / 8.2 / 11.3 / 15.1 / 24.1 ms |
| Longest gap between two successful calls through a router | 0.22 s |
| Server streams that ended in the window | 14, each complete: 1000 messages and `grpc-status` 0 |
| Messages | 14828 |
| Longest gap between two messages of a router's streams, the next stream's start included | 1.02 s |

| Probe | Result |
|---|---|
| Status answered, converged, all 20 replicas running, no replica on two machines | 371 of 371 |
| `kv` value read back, and the same as written | 371 of 371 |
| `GET /v1/status`, `/v1/nodes`, `/v1/apps` on the server, 310 each | all 200; p50 2.3, 1.8 and 1.1 ms; p99 7.7, 8.8 and 7.9 ms; max 14.4, 13.0 and 12.0 ms |
| Dangling Docker volumes, every node, every 60 s | 0 |
| Container log files | at most 2 files for one container, the largest 10000119 bytes |
| Events: lost nodes, restarts, failed probes | none |

The hivemind process on each node, 62 samples a minute apart:

| Node | RSS first / last / max (MiB) | Threads first / last | Open fds first / last / max | CPU, % of one CPU | Restarts |
|---|---|---|---|---|---|
| hivemind-soaki-1 (server) | 27.8 / 27.7 / 28.0 | 9 / 9 | 43 / 42 / 45 | 5.21 | 0 |
| hivemind-soaki-2 | 24.2 / 24.1 / 24.4 | 9 / 9 | 26 / 26 / 28 | 3.87 | 0 |
| hivemind-soaki-3 | 23.7 / 24.1 / 24.2 | 8 / 9 | 26 / 26 / 27 | 3.83 | 0 |
| hivemind-soaki-4 | 22.0 / 22.3 / 22.4 | 8 / 9 | 27 / 26 / 29 | 3.72 | 0 |

Each process used 3.7 to 5.2 % of one CPU, against 2.9 to 4.1 % in the fifth soak, with the gRPC clients' 20 calls a second and four streams through the routers on top of the same HTTP load, and 8 replicas on the server's machine against 5. RSS stayed within 0.4 MiB over the hour. The store grew from 252 to 256 KiB (`hivemind.db`), with a 4032 KiB WAL at the end. The routers' counters in the server's `/metrics`, read at the window's two ends, against the clients' `200`s through each router, the gRPC calls and streams counted for `paths.soak.test`:

| Router | `api.soak.test` | `paths.soak.test` | `www.soak.test` |
|---|---|---|---|
| hivemind-soaki-1 (the server, read directly) | +24 | +22 | +17 |
| hivemind-soaki-2 | -42 | -104 | -41 |
| hivemind-soaki-3 | -42 | -105 | -42 |
| hivemind-soaki-4 | -42 | -105 | -43 |

The server's router counted 24, 22 and 17 more `200`s than its streams had, against Pebble's three HTTP-01 validations for each of the 8, 8 and 6 issuances, 24, 24 and 18. Each worker's counters were 4.2 s further behind the clients at the second read than at the first: 42 requests of a host with 10 a second, 105 of `paths.soak.test` with its two HTTP streams and the unary calls, 25 a second; a worker posts them every 15 s.

## What did not work, and what was not measured

- When the server's machine restarted while its workers could not reach it, its routed replicas came back on other host ports, and the workers' routers, serving their last table, sent requests to the old ones: gRPC calls to an HTTP/1.1 server, which got `502`, and `papi`'s requests to `proot`, which answered them with a `200`. In the hold this lasted from 26 s after the server's API answered until each worker reconnected, 3 minutes and more; in the second restart, healed inside the grace, 48 s. Fixed after the soak: a routed container now keeps its host ports for its life ([route host ports](routing.md#route-host-ports)).
- The hold's `api` streams did not check which app answered, so how many `api` requests `proot` answered then was not counted; the second restart counted it for `papi` and `api`.
- The gRPC clients make one call at a time per router, so the power-off cost one call per router; with more calls in flight, more would have failed in the 5 s before the router gave up the connection.
- During the hold the workers' routers tried to renew certificates while the server, which holds their storage, was unreachable, and logged `certificate resources not found in storage for any configured issuer (issuer may have changed); obtaining a new certificate instead of renewing`, then at error `job failed ... failed storage check ... storage is probably misconfigured`. Nothing was issued, and after the heal the next attempt found the stored certificate. Fixed after the soak: a failed check of the storage now counts as the key existing, so certmagic takes its renewal path, which fails on the storage check with the real error and asks the CA for nothing.
- The routers renewed every certificate about every 10 minutes, 22 issuances in the steady hour (17 in the fifth soak's), each with the warning `certificate is in emergency renewal window`: Pebble's renewal information gives the whole lifetime of its 1-hour certificates as the renewal window. A property of the test CA's settings.
- A rotation with a worker cut off by `finish --force`, and the server's new warning for such a worker, were not run; nor a compose file's `x-hivemind` health check, nor a failing health check's reason in `hivemind status`.
- The power-off goes through two ssh hops; the harness's fault line is about 5 s before the machine went down, which the clients' first stalled requests (05:01:43) date.
- The harness's held-stream client first ran with a curl `-w` format that began with `@`, which curl reads as a file to load: for two minutes before the first scenario every stream failed at once. It was fixed, those lines dropped, and the clients started again before the first scenario.
- One hour of steady state is too short for leak figures.

## Running it, with gRPC

A build after the sixth soak publishes the routed containers on host ports 30000 to 32767, so the soak security group must allow TCP 30000 to 32767 between the machines; the groups of the second to sixth soaks allowed 32768 to 60999, the range before.

As [with CA rotations](#running-it-with-ca-rotations), with `SOAK_GRPC=1` and a linux build of `test/h2ctest` in the run directory:

```sh
export SOAK_DIR=$PWD/.scratch/run SOAK_ROUTES=1 SOAK_PATHS=1 SOAK_GRPC=1 SERVER_NAME=hivemind-server.soak.test
CGO_ENABLED=0 GOOS=linux go build -trimpath -o "$SOAK_DIR/h2ctest" ./test/h2ctest
scripts/soak/outside.sh setup                 # pushes soak/h2ctest:1 and :2, and the client binary
BIN=path/to/hivemind scripts/soak/setup.sh     # grpc, and a checked call of each kind through every router
scripts/soak/outside.sh client-start           # 16 HTTP streams, and a unary and a held stream per router
scripts/soak/scenario.sh probe-start
scripts/soak/scenario.sh watch-start

HEAL_LAGS="node-2:0 node-3:5 node-4:20" scripts/soak/scenario.sh start-hold &   # stop the server's machine, then start it, when the log says so
scripts/soak/scenario.sh ca-rotate
scripts/soak/scenario.sh ca-abort
scripts/soak/scenario.sh rollout 2 grpc
scripts/soak/scenario.sh rollout 2 proot papi
scripts/soak/scenario.sh begin poweroff-worker node-4
scripts/soak/scenario.sh measure poweroff-worker node-4 loss 300 &    # ...power node-4 off; once it is lost, power it on, then:
scripts/soak/scenario.sh measure poweroff-worker node-4 return 600
scripts/soak/scenario.sh end poweroff-worker
scripts/soak/scenario.sh mark steady begin && scripts/soak/scenario.sh metrics-snap steady-begin
#    ...an hour later:
scripts/soak/scenario.sh metrics-snap steady-end && scripts/soak/scenario.sh mark steady end

# the server's machine restarted while its workers cannot reach it, healed inside the grace
for ip in 10.0.0.5 10.0.0.6 10.0.0.7 10.0.0.8; do STREAMS="api.soak.test/index.html=api" scripts/soak/outside.sh client-add $ip; done
HOLD_NAME=restart-cutoff HOLD_AFTER=-50 HEAL_LAGS="node-2:0 node-3:0 node-4:0" scripts/soak/scenario.sh start-hold &

scripts/soak/outside.sh fetch
SOAK_DURATIONS=scripts/soak/durations-soak6.txt scripts/soak/report.sh "$SOAK_DIR/soak.log"
scripts/soak/client-report.sh "$SOAK_DIR/soak.log" "$SOAK_DIR/client"
scripts/soak/metrics-report.sh "$SOAK_DIR/metrics-steady-begin.txt" "$SOAK_DIR/metrics-steady-end.txt" "$SOAK_DIR/client"
```
