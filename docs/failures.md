# When things go wrong

What hivemind does when the machine under it fails, and what it tells you. Each case is a scenario of `scripts/edge-suite.sh`, which runs it against real Docker daemons (docker:dind) and checks the result, or a unit test where noted. The fault suite (`scripts/fault-suite.sh`) covers lost workers, a killed server, partitions and slow links.

## Docker restarts under the agent

When dockerd stops, the agent keeps running. Each pass fails at the container list, and every replica on the node shows as `unknown`, with `the Docker daemon on this node is not answering, so the state of its containers is not known: ...`. The agent changes nothing and tries again every reconcile interval (2 s). It does not restart early to catch up with a restart or a pull that fell due during the outage.

Containers have no restart policy. After `kill -9` of dockerd they keep running in their shims until dockerd starts again, which then stops them; a clean stop of dockerd stops them at once. When the daemon is back, the agent starts the same containers again, with their volumes, and the restart counts as one restart of each.

## Image pulls that fail

A replica whose image cannot be pulled stays `pending`, and its status names the image and Docker's reason: no such image, no such tag, a registry name that does not resolve, a refused connection, `429 Too Many Requests`. Pulls of an image back off from the end of the last one: 10 s, then 20 s, 40 s and so on, up to 5 minutes. A registry login or logout changes the credential, and is tried at the next pass. The backoff is in memory, so a restarted agent pulls at once.

A pull that takes more than 2 seconds goes on in the background. The replica's status says `pulling <image>, started <time>; the registry has not answered yet`, and the other replicas on the node are supervised as usual. Docker gives up on a registry that does not answer after 30 seconds. A pull that no replica needs any more, because the spec was fixed or the app deleted, is cancelled.

## A full disk on a node

With Docker's data root full, pulls and container creates fail with `no space left on device` in the replica's status. A replica whose container cannot be created or started waits 2 s before the next try, then 4 s, doubling up to a minute; pulls keep their own backoff. Containers that run are not touched. Once there is space, the replicas start at their next try.

## A full disk on the server

A write to the API answers `507 Insufficient Storage` with `the server's disk is full, so the change was not saved: ...`, and nothing of it is stored. Reads work. Status reports, lease renewals and the sweep fail and are logged at each attempt; serve keeps running, and the containers on every node keep running. Workers get the same message for their reports. Once there is space, writes work again. 507 rather than 500 or 503: the server is up and could not store the change, and the request will fail again until someone frees space.

## kill -9 of serve

Every change to the store is one SQLite transaction in WAL mode with `synchronous=FULL`, so a killed server leaves the old state or the new one. The edge suite kills serve 20 times under a client that writes as fast as the server answers, and checks after each kill that `PRAGMA integrity_check` and the store's invariants pass, that every write the API acknowledged is there, and that the replicas converge with none lost or doubled.

## Crash loops

The agent restarts a container that exits, whatever its code, and one that fails its health check, after 1 s, doubling to 5 minutes ([concepts](concepts.md#health-checks-and-restarts)). While it waits, the replica is crash-looping, and status says why: `exited with code 3`, `killed for going over its memory limit of 8Mi (out of memory)`, or `failed its health check 2 times in a row: ...`. `hivemind_container_restarts_total` counts the restarts. Other apps on the node are not touched.

## logs -f across a server restart

`hivemind logs -f` does not reconnect. When the server shuts down, the stream ends with `the server is shutting down, so it ended the log stream; run the command again to go on following the log`, and when the connection breaks, as when serve is killed, with `the connection to the server broke during the log ...`. Both exit 1. The end of the log, when the container stops, exits 0. A server machine that goes away without closing the connection is found by TCP keepalives in about 30 seconds.

## The server down under a worker

A worker keeps its containers while the server is down, and its passes back off: 2 s, 4 s, 8 s, 16 s, then one every 30 s. It logs the start of the outage, then a line a minute with the count of failed passes, and one line when the server answers:

```
level=WARN msg="cannot reach the server; passes back off, up to one every 30s, until it answers" component=agent err="fetching assignments: calling the server: ... connect: connection refused"
level=WARN msg="the server is still unreachable" component=agent for=1m0s failed_passes=6 err=...
level=INFO msg="the server answers again" component=agent after=2m30s failed_passes=8
```

The log poll's retries are logged at debug level while the server does not answer. The lease heartbeat keeps its 5 s timer. In the edge suite's `server-down` case (serve stopped for 150 s under one worker, two Docker-in-Docker nodes on the c8g.4xlarge build host), the worker made 8 passes in the 150 s and logged the start line and two `still unreachable` lines for them, plus one log-poll warning for the server's refusal as it shut down and one line when it answered again. Before, it made a pass and logged a warning every 2 s, and the log poll a warning every 30 s. It used 2 CPU ticks (20 ms) in the 150 s, and reached the server 2 s after serve started again. The first reply the worker gets from the server after an outage, to any call (the lease renewal every 5 s, the router's table read every second, the log poll), ends the backed-off wait, and a pass runs at once, so the worker reports its containers within seconds of reaching the server rather than at the end of a wait of up to 30 s; until then, `hivemind status` shows its replicas as `unknown`.

## A damaged worker data directory

A worker refuses to start when a file in its data directory is missing or does not fit the others: `node.key` gone, `node.json` cut short or empty, `ca.crt` from another cluster, `node.crt` gone, or a key that is not the certificate's. The error names the file and the node, and the two ways out: restore the directory from a backup of the machine, or run `hivemind node remove <id> --force` on the server and join again with an empty `--data-dir` and a new token. A key file readable by others is refused with the `chmod` that fixes it.

## Clock skew

The server measures leases with its own clock, so a worker's clock does not move them. Certificates are another matter: they start 5 minutes before they are issued. A worker whose clock is behind the server's by more than that takes a certificate the server issued since as not valid yet: a join fails, a renewal is refused and tried again at the next check, and a restarted worker refuses its own certificate. A worker that is ahead works until its clock passes its certificate's expiry. In each case the error gives the clock's reading and says to set it right. A worker also compares its clock with the `Date` of the server's replies, and logs a warning, once an hour, while they are more than a minute apart.

A clock that is set back does not delay restarts. The agent measures a restart's backoff from the exit time Docker recorded for the container, and an exit time later than the agent's clock, which is what a clock set back leaves, counts as now: the wait is never longer than the backoff, and the agent logs `the clock was set back`. Before this, on three Amazon Linux 2023 machines that powered off with their clocks 245 days ahead, every replica showed `crash-looping: exited with code 0, next restart in 5879h59m51s` after the next boot, and nothing ran until the containers were started by hand.

The edge suite cannot skew one container's clock: libfaketime does not reach a static Go binary, and a time namespace does not shift the real-time clock. These cases are unit tests (`cmd/hivemind/clock_test.go`, `datadir_test.go`, `internal/nodeapi/clock_test.go`).

## Spec limits

The API refuses a spec past any of its limits with 400 and a message that names the field, and a body over 1 MiB with 413. The limits are in [api.md](api.md#limits) and [concepts](concepts.md). The edge suite deploys the largest spec the API takes (64 command arguments of up to 4 KiB, 128 environment variables in 64 KiB, 32 ports, 16 volumes, 16 routes, a 64-argument exec health check) and 100 replicas of one app, on one node.
