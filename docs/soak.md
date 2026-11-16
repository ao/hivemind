# Soak test on four machines

One server and three workers, each a separate machine, run three small apps. Then the soak test powers off a worker, reboots one, cuts one off from the server, replaces the server with an empty machine, and leaves the cluster alone for four hours. Every number below comes from the harness's log; nothing is estimated. The harness is in [scripts/soak](../scripts/soak), and the last section says how to run it.

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
