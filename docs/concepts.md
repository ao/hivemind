# Concepts

This describes the build on this branch. It runs one server, or three or five ([several servers](#several-servers)), and any number of workers, on Linux machines with Docker Engine. Where a limit applies, the section says so. [PLAN.md](PLAN.md) has the full plan.

## Server and workers

`hivemind init` creates a cluster on one machine. That machine is the server. It keeps the desired state in a SQLite database in the data directory, serves the HTTP API on `127.0.0.1:4483` by default, decides where each replica runs, and runs an agent for its own containers.

A worker is another machine that joined with `hivemind join`. It runs an agent and nothing else: no database, no API, and no listener except the router when it is started with `--route-addr`. Each pass, the agent reads its assignments from the server over the node API, compares them with its containers, and reports back. The worker also keeps a request open to the server for log requests, so that `hivemind logs` on the server can read its containers (see [logs from workers](#logs-from-workers)).

`hivemind serve` runs the server and its agent in one process. `hivemind serve --role worker` runs a worker that has already joined. Stopping either one leaves its containers running, but nothing changes them until the agent runs again. If the server is down, workers keep their containers and no new replica is placed.

The agent keeps no state of its own. Its record is the set of containers and their labels, so an agent that restarts picks them up again.

## Several servers

From 2.3.0 a cluster can have several servers, three or five, instead of one ([several servers](ha.md)). They run Raft: one of them is the leader, which runs the sweep, placement, rollouts, drains and the CA rotation manager, and every write goes through the Raft log, so each server has a copy of the store, and a write is acknowledged once a majority has it. The cluster goes on while a majority of the servers is up; with three, while one is down. Each server is also a node, with an agent and its own containers.

Every server serves the API and the node and join listeners. A follower passes each request to the leader, so the CLI can use any server, or a list of them (`HIVEMIND_SERVER=https://10.0.0.5:4483,https://10.0.0.6:4483`). A worker keeps the servers in its `node.json` and uses the first that answers. When the leader is lost, the others elect a new one, in a second or two on a LAN (0.7 to 1.8 s in the integration test), and the new leader gives every node that was up the start grace before it counts as lost, as a server does after a restart ([leases](#leases-and-node-loss)). When a majority is down, there is no leader: nothing is placed or rescheduled, the API answers `503` with the reason, and every container keeps running.

`init` makes a cluster of one, which runs no Raft. `hivemind join-token --role server` and `hivemind join <server> --role server --token <token> --advertise <address>` add a server; `hivemind servers` lists them, and `hivemind server remove <id>` removes one. The servers use a port of their own, 4486, between themselves ([security](security.md#several-servers)).

## Joining a machine

1. On the server, `hivemind join-token` prints a token. The token carries the fingerprint of the cluster CA (the SHA-256 of the CA's public key), 256 random bits, and an expiry.
2. On the worker, `hivemind join <server> --token <token>` generates a key and a certificate request on the worker. It opens a TLS connection to the server's join listener, on the cluster port plus one (4485 by default).
3. The worker completes the handshake only if the server's chain includes the CA whose fingerprint is in the token. The token is sent after that check.
4. The server checks the token, picks the node ID, issues a node certificate for the public key in the request, and marks the token used.
5. The worker checks the reply: the CA must match the fingerprint, and the certificate must chain to that CA and name the node ID in the reply. It then saves its key, certificate, the CA and its node ID in its data directory, and starts the agent in the foreground.

The trust rests on the token:

- The token is the only credential at join. Whoever holds an unused, unexpired token can join a machine. Give each token to one machine, and use the shortest `--ttl` that works.
- A token is single use. The server marks it used in the same transaction that checks it, so two joins with one token cannot both succeed. A join that fails before the token is redeemed leaves it usable.
- A token expires. It lasts 24 hours by default, and `join-token --ttl` allows up to 168 hours. The server removes expired tokens on its sweep.
- The server stores a SHA-256 of each token's secret, not the secret.
- The server picks the node ID (`node-` and 12 hex digits). A joining machine cannot choose its own.
- The join listener asks for no client certificate, so it cannot tell one machine from another. It counts failed joins per source address. After five failures in a 10-minute window, that address gets `429 Too Many Requests` until the window ends. Machines behind one NAT share the count.

## The server's address

The address in `serve --advertise <address>` is the one workers dial. `hivemind join <address>` stores it in the worker's `node.json`, and the worker checks, at every connection, that the server's certificate names it. It can be an IP address or a DNS name. `hivemind nodes` shows it on the server's row after the node's address, as `172.31.19.207 (hivemind.example.internal)`, and `nodes --json` has it as the server's `advertise`, and what each worker dials, from its `node.json`, as its `dials`.

A DNS name is the recommended set-up. A server replaced by a machine with another IP address, as a replaced cloud instance usually is, then needs only the name's record updated: the workers reconnect without joining again, with their certificates ([backup](backup.md#replacing-a-lost-server)). A worker looks the name up at each new connection, so it follows the record as soon as its resolver does; a TTL of a minute or so keeps that short, and inside the 2-minute start grace in which a restored server waits for the workers ([leases](#leases-and-node-loss)). With a longer TTL, lower it well before you need it, or raise `--start-grace` to more than the TTL. With an IP address, the new machine needs the old address, or each worker is moved to the new one with `hivemind node set-server`, which first checks that the new address serves this cluster ([backup](backup.md#at-a-new-ip-address)).

```sh
hivemind serve --advertise hivemind.example.internal
hivemind join hivemind.example.internal --token <token>    # on each worker
```

With a name, `serve` binds the cluster listeners on the address of this machine that the name resolves to, and the server's certificate names both the name and that address. It refuses a name that resolves to none of this machine's addresses, and an IP address, given with `--advertise` or `--cluster-addr`, that is not one of them. `--cluster-addr <ip>` binds that address of this machine instead, whatever the name resolves to, and the certificate names it with the name. That is for a name whose record does not lead here yet, as after a restore, and for one that resolves to an address in front of the machine, such as a NAT's. `serve` logs a warning when the name resolves elsewhere. The listeners never bind a wildcard address.

A restore makes a new copy of the cluster, and the machine it replaced may come back with the old one. The cluster has an epoch for that: 0 from `init`, and one more than the backup's after each `restore`. The server sends its epoch on every node API reply. A worker keeps the highest it has seen in `cluster-epoch`, in its data directory, and refuses a server at a lower epoch, a copy from before a restore: it sends it nothing, keeps its containers running, logs that once at error with what to do, and asks again every 10 s in case the restored server comes back at that address. `hivemind node set-server` refuses such a server too. See [backup](backup.md#if-the-old-server-comes-back) for what it does not cover.

## Node certificates and the node API

A node certificate is ECDSA P-256, signed by the cluster CA, and valid for 365 days. Its URI name is `hivemind://node/<id>`, and its common name is the same ID. The server's own certificate also carries the address it advertises, and the address its listeners bind when that is another, so a worker can check the name it dialed.

A worker renews its certificate once two thirds of its lifetime have passed, on day 243, with a new key, over the node API. The server renews its own certificate on the same schedule. Neither restarts: new connections use the new certificate. A worker that cannot reach the server for the last four months of its certificate's life, such as a machine that was switched off, cannot renew after it expires, and must be removed and join again. `hivemind nodes` shows each node's certificate expiry, and marks it `not renewing` a day after the renewal time has passed, as in `2027-11-16 (in 120d, not renewing)`; the server logs a warning for that node once a day. The details, and what the server checks, are in [security](security.md#certificate-renewal).

The node API runs on the cluster port (`--cluster-port`, 4484 by default) on the `--advertise` address. It opens only when the server runs with `--advertise`. A request is accepted only when the client presents a certificate that chains to the cluster CA and names a node; during a [CA rotation](ca-rotation.md), to either of its two CAs. Each route then checks that the node ID in its path is the node ID in the certificate, and answers 403 if it is not. A node can therefore read its own assignments, report its own containers, and register its own name and capacity. It cannot read or write another node's data.

The node API is internal, and its routes are not a stable interface. See [the API](api.md#node-api-internal).

## Logs from workers

`hivemind logs <app> [replica]` works for a replica on any node. The API looks up the node the replica is placed on. The server reads its own containers' logs from its Docker daemon. For a replica on a worker:

1. The server queues a log request for the replica's node, with a random ID.
2. The worker has a poll open on the node API, which the server answers with the request. A poll with nothing to answer ends after 25 seconds, and the worker opens the next one.
3. The worker finds the replica's container, reads its log from its own Docker daemon, and posts it to the server as one streamed request. Standard output and standard error are separate frames.
4. The server checks that the stream comes from the node the request was queued for, by its certificate, and copies it to the user's response as it arrives.

A worker that does not take the request and start its stream in 10 seconds gets the user a `504` that names the worker and asks whether it is running. A lost node, whose lease is more than 15 s old, is refused at once with `503`, without waiting. With `-f`, the stream lasts until the user stops it or the container exits. Ctrl-C ends the user's request, the server cuts the worker's stream, and the worker stops reading the log. A node can have 16 log streams open at once. The routes and the checks are in [security](security.md#logs-from-workers).

A worker of version 2.0.0 does not poll for log requests, so its replicas get the `504`. Upgrade the worker to read them.

## Leases and node loss

Each pass of a worker's agent reports its containers to the server. That report renews the node's lease. A pass runs every `--reconcile-interval` (2 s by default), and joining and registering also renew the lease. The agent also renews the lease every 5 s on its own, apart from its passes, because a pass reports only at its end, and a pass can be slow when its Docker daemon or the server is. Pulls and container removals do not hold a pass up: a pull goes on in the background after 2 s, and extra containers are stopped and removed in the background, 8 at a time, each with up to 10 s for a process that ignores SIGTERM. A container that needs a host port, the volumes or the name of one being removed starts once that one is gone. Nomad clients heartbeat apart from their work in the same way. The server's own node does the same. The other side of this is that a node whose agent is stuck in a pass still counts as up; the status it last reported stays as it was until the pass ends.

A node is lost when its lease is more than 15 s old. The 15 s is fixed. The server checks leases every 5 s, so a lost node is noticed 15 to 20 s after its last renewal. The server logs one line for each node it marks as lost.

When `serve` starts, every lease it finds is as old as the time it was down. A worker that was up when the server went down is then `waiting` for the start grace, 2 minutes after the start by default (`--start-grace`, at least 15 s): it is not lost, its replicas stay on it, and the routing table keeps them as the worker last reported them, so the routers go on sending them traffic. It is `ready` as soon as it reports. One that has not reported when the grace ends is lost then, and its replicas are placed again. The grace is for a server restored at a new address: the workers reach it only once their resolvers have the name's new record, which can take as long as the record's TTL, or once each is moved with `hivemind node set-server`. What it costs: a worker that died while the server was down has its replicas placed again only when the grace ends, that much later than a worker lost while the server runs. A worker whose lease had already lapsed when the server went down is lost at once. The server takes the time it went down to be the latest lease a worker renewed before the start, and logs how many workers wait.

If the grace ends and none of the workers that were waiting has reported, none of them is lost: they stay `waiting`, and their replicas stay where they were, until one of them reports, which ends this hold and gives the others the start grace again, from that report, before they are lost, or until an operator removes them with `hivemind node remove`. `serve` logs at error, then once an hour, that no worker has reported since it started ([backup](backup.md#replacing-a-lost-server)), and `hivemind status` and `hivemind nodes` say why the workers wait. What this costs: a cluster whose every worker died while the server was down keeps their replicas unplaced, and those apps down, until the dead nodes are removed. Why it is the better default: a server that hears from none of its workers is more often an old copy of a cluster restored elsewhere, whose workers refuse it ([backup](backup.md#if-the-old-server-comes-back)), or a server cut off from its workers, than one whose workers all died at once. Marking them all lost would put every replica on the server's own machine, a second copy of each app beside the copies the workers still run, which the server's router would serve.

Two things follow from the 15 s:

- `--reconcile-interval` is at most 5 s, so a worker can miss two passes before its 15 s lease runs out. A longer value is refused at start-up.
- A worker that cannot reach the server for 15 s is treated as lost, and its replicas are placed again.

## Placement

The scheduler decides where each replica runs. Its input is the nodes, the replicas already placed, and the time. It runs on every deploy, scale, node registration, status report and sweep. A replica without a node is `pending` until one takes it. For each replica, the scheduler applies these rules:

- Only eligible nodes take new replicas. A node is eligible when its lease is current, it has been ready for `--min-ready` (see below), and it has not been made ineligible or drained (see [Draining a node](#draining-a-node)). The server's own node is always ready.
- The CPU and memory the replica asks for must fit in what is left on the node, after the replicas already placed there. A node's totals are the CPU count and memory that its Docker daemon reports. A replica's `resources` are counted here, and the agent also sets them on the container as its limits (see [Resources](#resources)).
- Its host ports must not clash with the ports of replicas already on the node. Two ports clash when the number and protocol match and the bind addresses overlap. A wildcard address (`0.0.0.0` or `::`) overlaps every address. Two different specific addresses do not overlap. The default bind is `127.0.0.1`.
- A replica of an app with `routes` goes only to a node that reported a private address (see [Routing](#routing)). A worker older than 2.2 reports none. Route ports go on host ports that the agent picks, from 30000 to 32767, so they never clash with each other ([routing](routing.md#route-host-ports)).
- A host port bound to a specific address, one that is not loopback or a wildcard, goes only to the node whose address is that address: the address a worker reaches the server from, or the server's `--advertise` address. `hivemind nodes --json` lists each node's `address`. On any other machine the bind would fail. With no such node, the replica stays `pending`, and its reason names the address. A server without `--advertise` has no workers, and its node takes a port bound to any address. A node reports one address, so a port bound to another address of a machine with several does not go to it.
- A node's assignments must stay within 15 MiB. A worker reads all of its assignments in one reply, each with its replica's whole spec and, for one that is to run, the credential of its image's registry, and it reads at most 16 MiB; past that it would change nothing on the node. Replicas with ordinary specs never come near it, but a spec of a few hundred KiB, from a long command or environment, with many replicas, or a long registry token repeated on each, could. A replica that would take a node past the bound goes to another node, or waits with `every eligible node's assignments would be more than the 15 MiB a node reads; use fewer replicas, a smaller spec or more nodes`. A deploy whose larger spec would take a node past it in place, as a rollout moves the replicas the node already has, is refused with `409`, and so is a `registry login` whose credential would.
- Among the nodes that fit, it takes the one with the fewest replicas of the same app. Then the lowest load, which is the larger of the CPU and memory fractions in use. Then the fewest replicas overall, and then the lowest node ID.

Placement never moves a replica that already has a node. Moves come only from node loss, rollouts and drains, with one exception: a replica on a node that does not have the address one of its host ports binds, whose container is not running there, is placed again by the server's sweep, as an earlier version could place it there. A replica with a volume stays where its data is, and its `DETAIL` says how to run it: deploy the app with the port on that node's address, or delete the app and deploy it again, which starts it with a new volume on the node that has the address. A rollout to a spec whose port binds another node's address moves each replica to that node; for a replica with a volume, which cannot move, the rollout pauses with that reason.

A replica that cannot be placed gives its reason in the status `DETAIL` column, and in the `pending_reason` field of the API. Reasons include `no node is ready`, `no node has been ready for 30s`, `every ready node is ineligible for new replicas`, `no node has 1 CPU free`, and `host port 127.0.0.1:8080/tcp is in use on every eligible node`.

Two apps that publish the same host port cannot share a node. `deploy` accepts the second app anyway, so that deploying a replacement and then deleting the app that holds the port works. Its replicas go to the eligible nodes where the port is free, and any replica that has no such node stays `pending` with the host-port reason. It is placed once the first app's containers are gone from a node.

A write places its new replicas before it returns, so `deploy`, `scale` and `compose deploy` know which of them no node took. Each one prints a warning on standard error for those, with the reason `status` shows, followed by the app that holds the port when that is the reason, and exits 0:

```
web: created, generation 1, 1 replica
web: warning: replica 0 has no node: host port 127.0.0.1:8080/tcp is in use on every eligible node; held by app api
```

`deploy --wait` and `scale --wait` exit 1 at once, rather than at their timeout, when a replica has no node for a reason that does not change by itself: a host port that a running replica holds on every node that could take it, a bind address no node has, or more CPU or memory than any node has. It takes deleting or changing an app, or adding a node. A port held by a copy being removed, a node not ready yet, an ineligible node that has room, or CPU and memory that are in use but not more than a node has, may change, and `--wait` waits for them. The spec stays deployed either way.

## Minimum ready time

A node is eligible for new replicas 30 s after it last became ready. It becomes ready when it registers, and again when its lease comes back after a gap of more than 15 s. A node that flaps therefore keeps restarting this clock, and takes no new replicas until its lease has renewed without a gap for 30 s. The setting is `--min-ready` on the server. `0s` turns the wait off. The rule does not move replicas that already run on the node.

## Node loss and rescheduling

When a node is lost, the server releases its running replicas and drops the rollout surges on it. Then each replica is handled by its kind:

- A replica without a volume loses its node and is placed again, like a new replica, on any eligible node, including the server. Its container starts fresh, so anything the app kept in the container's own filesystem is gone.
- A replica with a volume keeps its node. It is not moved, because a volume is a Docker named volume on one node, and a copy elsewhere would start with an empty volume. Its `DETAIL` says `volume is on node <id>, which is unavailable; the app stays down until the node returns`.
- A replica that publishes a host port can stay pending after a loss. If every eligible node already runs a replica that publishes that port, the replica waits, with the host-port reason, until a node has the port free or the lost node returns.

## Node return and generation fencing

When a lost node comes back, it registers again if needed, then asks for its assignments. The server returns only the replicas still placed on that node. Before it starts or supervises anything else, the agent removes every container that matches no assignment, and every container whose generation is older than its assignment. So a replica that moved while the node was away is not started a second time there. Its old container is removed on the first pass after the return.

Fencing does not reach a node that is cut off but still running. Hivemind cannot stop a container on a machine it cannot reach. While the link is down, the old copy of a moved replica keeps running there, and two copies can answer on different machines for that time. A node that was powered off has no copy running, so there is nothing to stop.

## Removing a node

`hivemind nodes` lists the nodes, with each one's lease age and state: `ready` while its lease is current, `lost` after 15 s without renewal, and `waiting` for a worker that has not reported to a server that has just started ([leases](#leases-and-node-loss)). `hivemind node remove <id>` takes a node out of the cluster for good. [Drain](#draining-a-node) the node, or stop hivemind on the machine, first.

- The server's own node cannot be removed.
- A node that still renews its lease is refused, unless `--force` is given, or the node is ineligible and nothing runs on it, as after a drain. This follows `docker node rm`, which refuses a node that is not down. A running node that is removed with replicas on it keeps their containers running beside the copies placed elsewhere, so the refusal keeps that from happening by mistake.
- A drain's move in flight ends with the replica on the node where its copy is starting.
- Each replica on the node that should run loses the node and is placed again, as after a node loss. A replica with a volume is not moved: its data is a Docker volume on the removed node, and a copy elsewhere would start empty. It waits with no node, and its `DETAIL` in `status` says so. It is placed again, with an empty volume, after a deploy that changes the app's spec, such as one with a new volume name. Deleting the app also clears it.
- The node's row, its reported container status, its rollout surges and its replicas marked for removal are deleted. A rollout whose surge was on the node pauses at its deadline.
- The node's ID goes on the server's list of removed nodes, which is kept for good. The server refuses the node's certificate from then on (see [security](security.md#revocation-by-removal)), refuses the ID if the node registers again, and never issues the ID to a joining machine.

A worker that is still running when it is removed gets `403` on its next call to the server, within one reconcile interval. It logs that it was removed and exits with an error, rather than retrying: the removal is final, so a retry could never succeed, and an exit shows up in the service manager and the log. It leaves its containers running. Their replicas already run elsewhere, and the worker does not remove a user's containers on the strength of one reply. Remove them by hand with `docker rm -fv $(docker ps -aq --filter label=hivemind.app)`: `-v` removes their anonymous volumes too, those of images with `VOLUME` ([below](#volumes)), which `-f` alone leaves dangling; the app's named volumes stay. A drained worker has none left. Starting the worker again fails at once. To bring the machine back, run `hivemind join` with an empty data directory and a new join token. It gets a new node ID.

## Draining a node

`hivemind node eligibility <id> --disable` makes a node ineligible. It takes no new replicas, from placement or from a rollout's surge, and it keeps the replicas it runs. `--enable` makes it eligible again. The flag is stored in the server's database, so it holds across restarts of the server and of the node, and across a lapse of the node's lease. `hivemind nodes` shows it in the `ELIGIBILITY` column, and `GET /v1/nodes` as `eligible`.

`hivemind node drain <id>` makes the node ineligible and moves its replicas off while the node still runs. A move is a rollout surge at the replica's own generation: a copy starts on another eligible node, the replica moves there once the copy is ready and passes its health check, and the drained node's agent then stops the old container. Each app has one replica moving at a time, and different apps move at once. So while another node has room, an app keeps its replica count running through the drain.

- A replica with a volume does not move: its data is a Docker volume on this node. It keeps running there.
- A replica whose host ports are in use on every other eligible node, or that no other node has the CPU or memory for, does not move yet. The drain tries it again at each step, every few seconds.
- A replica of an app with a rollout in progress is left to the rollout, which does not surge onto the drained node. The drain moves what the rollout leaves on the node.
- A move whose copy crash-loops, or is not ready within the rollout deadline (the health check's timing plus 30 s, or 2 minutes), is undone, and that replica stays on the node until the drain's deadline.
- At the deadline, `--deadline`, 10 minutes by default, a copy that is still starting takes its replica at once. Every other replica left on the node, except one with a volume, loses the node and is placed again, as on node loss, and the drained node's agent stops its container. A replica that no node can take waits as `pending`.
- The drain is done at the deadline, or before it once only replicas with a volume are left on the node and its agent has removed the old containers.
- If the node is lost during the drain, its replicas without a volume are placed again at once, as on any node loss, moves in flight included. The result counts them as placed again after the node was lost, apart from those moved or stopped at the deadline, and the command and the server's log say when the node was lost.

The command waits and prints each move, each replica that stays and why, and the result. `--no-wait` returns at once. The drain is in the server's database, and the server advances it every few seconds, so it carries on across a restart of the server. A move that was in flight while the server was down gets a whole rollout deadline from the restart. A deadline that passed while the server was down ends the drain at its first step.

A drained node stays in the cluster, ineligible, until `node eligibility --enable` or `node remove`. `--enable` on a draining node cancels the drain: copies still starting elsewhere are removed, and the replicas still on the node stay there. `node remove` needs no `--force` for a node that is ineligible and runs nothing.

The server's own node can be made ineligible, and drained. Its replicas move to workers. The server keeps serving the API and placing replicas, and its agent keeps running whatever is left on its node, such as a replica with a volume. With no eligible node, a new replica waits as `pending` with the reason `every ready node is ineligible for new replicas`.

To drain several nodes, make them all ineligible first, then drain them one at a time, so that a replica drained off one does not move to another that is next.

The commands follow Nomad's [`node eligibility`](https://developer.hashicorp.com/nomad/docs/commands/node/eligibility) and [`node drain`](https://developer.hashicorp.com/nomad/docs/commands/node/drain), and the order for draining several nodes is the one Nomad's drain documentation gives. Some differences: a drain is cancelled with `node eligibility --enable`, not with `node drain -disable`; there is no `-force`, `-no-deadline` or `-ignore-system`; and the deadline defaults to 10 minutes, where Nomad's is one hour.

## Rolling updates

A change to an app's spec, other than its replica count, starts a rollout. The rollout moves the replicas to the new generation, one at a time. The rules come from the store's rollout code (`internal/store/rollout.go`).

- One move runs at a time. A move starts only when the previous one has finished, so at most one replica is being moved and at most one extra copy runs.
- A move has two forms. A surge places the new copy on another eligible node that has the app's host ports free. The copy runs beside the old one, and the replica moves there once the copy is ready. An app with volumes is never surged. In place stops the old container and starts the new one on the same node, so that replica is down while it starts.
- The next move starts only when the replica in flight reports ready at the new generation.
- A move has a deadline: the health check's start period, plus its retries at its interval, plus 30 seconds. An app with no health check gets 2 minutes. The deadline counts from when the node first reports a container of the new copy, so the time a busy node takes to create it, or to pull the image, does not count against the health check; a copy that never appears fails at the deadline from the start of the move. A move that is not ready by then pauses the rollout. A move whose container is crash-looping pauses it too. A drain's moves still count from the start of the move.
- A pause undoes the move in flight. An in-place move goes back to the old generation, and a surge is removed. The other replicas are not touched. The rollout stays paused, with a reason, until you deploy a spec with another template.
- Rolling back is deploying the old spec again. It is a new generation, so it is a rollout like any other.
- Deploying another template during a rollout abandons the move in flight, removes its surge, and starts again from the state the replicas are in.
- A rollout ends when every replica runs the target generation. Its state then drops out of `status`.

On a single node every move is in place: the replica being moved is `pending` while its new container starts, and the others keep running. With a second node that is eligible and has the app's host ports free, the move becomes a surge. A [drain](#draining-a-node) moves replicas off a node with the same surges, at each replica's own generation.

An app with routes and no host ports is the exception: its surge may go on the replica's own node too, since its route ports go on host ports the agent picks for each container, so every move of a routed app is a surge, on one node as on several. Its new copy is in the routing table before the old one leaves it, and the old one keeps running for 5 seconds after that before it is stopped. See [routing](routing.md#rolling-updates).

`hivemind status` prints a line for each app with a rollout in progress or paused, such as `web: rolling out generation 2`, or the pause reason. The API reports the same thing as the `rollout` field of an app's status. See [the API](api.md#objects).

## Apps and replicas

An app is a desired state: a container image, the command, environment, ports, volumes, resources, a health check, and a replica count. Replicas are numbered from 0 to the count minus one. The count is 1 by default and may be at most 100.

`hivemind deploy` takes the name, image, replica count, ports, environment, volumes and command as flags; the command goes after `--`. The rest needs an app file, in YAML or JSON, with the keys of the [app spec](api.md#app-spec):

```yaml
# web.yaml; deploy it with: hivemind deploy -f web.yaml
name: web
image: nginx:alpine
replicas: 1
env:
  NGINX_ENTRYPOINT_QUIET_LOGS: "1"
ports:
  - host: 8080        # published on 127.0.0.1 unless bind names another address
    container: 80
resources:
  cpu: 0.5
  memory: 128Mi
health:
  http:
    path: /
    port: 80
  interval: 5s
  retries: 3
```

```yaml
# ticker.yaml: a command, and a volume on the replica's node
name: ticker
image: busybox:stable
command: ["sh", "-c", "while true; do date >> /data/ticks; sleep 60; done"]
volumes:
  - name: data
    path: /data
```

The same ticker from flags is `hivemind deploy --name ticker --image busybox:stable --volume data:/data -- sh -c 'while true; do date >> /data/ticks; sleep 60; done'`. A key the spec does not have is refused with its line and the nearest key, such as `line 3: unknown key replica in the app (did you mean replicas?)`.

An image from a private registry needs a credential stored with `hivemind registry login <host>` (see [registries](registries.md)). The agent sends it with each pull of an image from that host. `docker login` on the node does not help: the Docker Engine API takes credentials with each pull request. Without one, the replica stays `pending`, and `status` says which registry wants a login. A failed pull is tried again after 10 s, doubling to 5 minutes.

`hivemind compose` makes one app of each service in a Docker Compose file. The CLI does the conversion, so the server stores and runs those apps like any other. See [Compose files](compose.md).

The spec limits are checked on every write. Among them: names are up to 32 lowercase letters, digits and dashes, an image is up to 255 characters, there are at most 128 environment variables and 64 KiB of them in total, at most 32 ports and 16 volumes.

## Generations

Each app has a generation number, which starts at 1. It goes up when anything in the spec changes, except the replica count. Changing the image, the environment, the ports, the health check, the resources or the volumes all count. Deleting an app and deploying it again under the same name gives it a higher generation, so the old containers are replaced.

Each replica has its own generation. A change to the spec sets the app's generation, but the replicas keep their old generation until a rollout moves them, one at a time (see [Rolling updates](#rolling-updates)). The app's `generation` in `status` is the newest one, and each replica row shows the generation its container is at.

A replica's container is named for its generation, `hivemind-<app>-<replica>-g<generation>`. The agent keeps a container only when its generation matches the one the server wants for that replica. A container of an older generation is removed once the replica has moved on.

Scaling does not change the generation. Scaling up starts the new replicas at the current generation. Scaling down marks the high-numbered replicas for removal.

## Labels

Every container hivemind starts carries four labels: `hivemind.app`, `hivemind.replica`, `hivemind.generation`, and `hivemind.routed`, which is `true` for a routed app and `false` for any other. The agent finds its containers by the `hivemind.app` label, and matches them to replicas by the next two. It gives a container with `hivemind.routed=true` the drain after it leaves the routing table. Each is set on every container, because a label the container sets wins over one of the same name in its image.

## Reconcile

The agent runs a pass every 2 seconds (`--reconcile-interval`), and a pass also runs straight after any change through the API. On the server, the agent reads its assignments from the store. On a worker, it reads them over the node API. A pass does the following, in order:

1. Fetch the assignments for this node.
2. List the containers that carry a `hivemind.app` label.
3. Remove the containers that match no assignment, whose generation is old, or that are dead or being removed.
4. For each assignment, supervise the container that is there, or create and start one. A missing image is pulled, with the credential stored for its registry, if any ([registries](registries.md)).
5. Report each replica's state to the server.

A failed replica does not stop the pass. The error goes in the status, and a later pass tries again. A failed pull waits 10 s, then 20 s, 40 s and so on up to 5 minutes, for the same image and credential, counted from the end of the pull. A pull that takes longer than 2 seconds goes on in the background, and triggers a pass when it ends, so a slow registry holds up only the replicas of its image. A container that cannot be created or started waits 2 s, then 4 s, doubling up to a minute. If the assignments cannot be read, or the containers cannot be listed, the pass changes nothing, so containers keep running, and the next pass is at the next interval. If the assignments cannot be read the agent does not report, so its lease ages. While a worker cannot reach the server at all, as when the server is down, its passes back off instead: the next comes after 2 s, then 4 s, doubling up to 30 s, and the first pass that reaches the server brings them back to the interval. The worker logs `cannot reach the server` at the first failed pass, `the server is still unreachable` with the count of failed passes once a minute after that, and `the server answers again` when it is back, rather than a line every pass. The lease heartbeat keeps its 5 s timer throughout, so the node's lease is renewed as soon as the server answers. See [failures](failures.md) for what each failure looks like.

## Health checks and restarts

An app may have one health check: HTTP, TCP or exec.

- HTTP sends `GET` to the container's address on its Docker network, on the container port, and passes on a 2xx or 3xx answer. Redirects are not followed. The path defaults to `/`.
- TCP passes when a connection to the container port opens.
- Exec runs the command inside the container, and passes on exit code 0.

The defaults are an interval of 10 seconds, a timeout of 2 seconds, and 3 retries. A `start_period` makes failures ignored for that long after the container starts. A replica becomes unhealthy after the retries fail in a row.

An app without a health check counts as ready as soon as its container runs. A new container is `starting` until its first check passes, one interval after the container started; a check that changes a replica's state is reported at once. When the agent itself restarts, as in an upgrade of hivemind, a container that kept running and that its last report called healthy stays healthy and ready, and its next check decides as it would have; so its routed replicas stay in the routing table.

The agent restarts a container that exits, whatever its exit code, and a container that becomes unhealthy. The wait before each restart doubles from 1 second and stops at 5 minutes. A container that has been running and healthy for 10 minutes starts over at 1 second. The restart count is kept per container, and the agent reads it back from the last status report when it starts.

The restart policy in the container itself is `no`. The agent is the only thing that restarts a container. A container that exits while the agent is down is not restarted until the agent runs again.

Status reports the state of each replica, its node, its generation, its health, its restart count and its last error. While a restart waits out its backoff, the replica is crash-looping, and status says why the container stopped and when the next restart is: `hivemind status` prints "crash-looping: exited with code 1, next restart in 40s", "crash-looping: killed for going over its memory limit of 64Mi (out of memory), ..." or "crash-looping: failed its health check 3 times in a row: ...".

A replica that runs and has not passed its health check yet says why under `DETAIL`: "its last health check failed: tcp port 50051: dial tcp 172.17.0.5:50051: connect: connection refused", a failure in the start period included; and just after a restart for failing its check, "restarted after it failed its health check 3 times in a row: ...", until the new container's own check passes or fails.

## Ports

A port in the spec is published on a host address. The default address is `127.0.0.1`, so the host port is reachable from the machine only; the other containers on the node can still reach the container port itself (see below). Write `0.0.0.0:8080:80` or another address to publish it elsewhere. The protocol is `tcp` by default, and `udp` is allowed. An IPv6 address goes in brackets, such as `[::1]:8080:80`.

Replicas that publish the same host port cannot share a node. The scheduler enforces this (see [Placement](#placement)). Two more cases:

- A port held by something outside hivemind, such as another program on the machine, makes Docker refuse the bind with `port is already allocated`. The agent removes that container, and the replica stays `pending` with the error in status. Each pass tries again with a new container, so the replica starts, with its port published, once the port is free. A container whose start failed is never started again, because Docker can let a second start succeed without the published port.
- Containers on different machines reach each other only through host ports. There is no overlay network and no DNS. A container gets an address on Docker's default bridge network, and the other containers on the same node can reach any of its ports there, published or not, whatever address the host port is bound to; nothing finds it by name. See [security](security.md#apps-on-one-node).

## Routing

An app can name HTTPS routes: `routes: [{host: www.example.com, port: 80}]`. A node started with `--route-addr` runs a router, which serves each routed host on ports 80 and 443 with a certificate from an ACME CA, and sends each request to a Ready replica of its app on any node. A route's container port is published on a host port of the node's private address, from 30000 to 32767, which the agent picks and reports, and the server builds the routing table from those reports. A route can take only a path prefix of its host, such as `/api`, so several apps can share a host; each host and path belongs to one app ([paths](routing.md#paths)). A route's `protocol` is `http`, HTTP/1.1 to the replicas, or `h2c`, HTTP/2 without TLS, for gRPC servers ([protocols](routing.md#protocols)). This is the one place hivemind proxies traffic, and its one listener that may bind `0.0.0.0`. See [routing](routing.md).

## Volumes

A volume is a Docker named volume on the node where the replica runs, mounted at an absolute path other than `/`. An app with volumes can have only one replica.

The replica stays on the node where it was first placed. If that node is lost, the replica is not moved, and the app is down until the node returns (see [Node loss and rescheduling](#node-loss-and-rescheduling)).

Volume names are scoped to the app. The Docker volume for volume `data` of app `web` is `hivemind-web_data`, so two apps that both use a volume called `data` get separate volumes and never see each other's files. App and volume names can't contain `_`, so no two pairs of names map to the same Docker volume. Docker creates the volume the first time a container mounts it, with the labels `hivemind.app` and `hivemind.volume`, so `docker volume ls --filter label=hivemind.app=web` lists an app's volumes. A volume that already exists is mounted as it is, and keeps its labels.

Deleting an app removes its containers and keeps its volumes. hivemind never deletes a named volume. Deploying an app with the same name and volume name on the same node mounts the same volume and finds the data again. To free the space, remove the volume with `docker volume rm` on the node.

An image can declare volumes of its own, with `VOLUME` in its Dockerfile, as the `postgres` and `redis` images do. Docker gives each container an anonymous volume for each of them, which is not one of the app's volumes: it lasts across restarts of the container, but each new container gets an empty one, so after a rollout or a move the data is not there. hivemind removes a container's anonymous volumes with the container, as `docker rm -v` does. Put data that must last in a volume of the app's spec, mounted at the same path; a named volume mounted there takes the place of the anonymous one.

## Resources

`resources.cpu` and `resources.memory` do two things. Placement counts them against each node's CPU and memory, so a node is not given more replicas than its totals allow. The agent applies them to each container as limits.

- `cpu` is a number of CPUs, such as `0.5`. It becomes Docker's `NanoCpus` (CPUs times 10^9), the same limit as `docker run --cpus`. A container over its limit is throttled, not killed.
- `memory` becomes Docker's `Memory`. `MemorySwap` is set to the same value, so the container gets no swap on top of its limit. Left unset, Docker would let it use as much swap as its memory limit again. A container that goes over the limit is killed by the kernel's OOM killer. It exits with code 137, and the agent restarts it with the same backoff as any other exit, so `status` shows its restart count going up.
- 0, or leaving a field out, means no limit and no reservation.
- The smallest nonzero values are `0.01` CPUs and `6Mi` of memory, the smallest Docker accepts. Smaller values are rejected when the spec is validated.

Changing either value changes the app's template, so it is a rolling update, like a change to the image or the environment.

On a kernel without swap accounting, Docker ignores `MemorySwap` and logs a warning. The memory limit still holds, but the container can use swap beyond it if the host has swap.

The totals are what the node reports, and a worker reports its own. Placement trusts those figures. A worker that reports more capacity than it has will be given more replicas.

An app whose spec sets no `resources.memory` has no memory limit, and no app has a PID limit: hivemind sets no `PidsLimit`, and the spec has no field for one, because a 2.0.0 worker refuses its whole assignment list when a spec has a field it does not know. A replica that uses all the memory, or forks without end, can take down the other replicas on its node, and on the server's node `serve` and its store with them. Set `resources.memory` on every app; see [security](security.md#apps-on-one-node).

## Container logs

A container's output goes to its node's Docker log driver. When the daemon's default driver is `json-file`, Docker's own default, which keeps all of a container's output in one file that grows without end, hivemind creates each container with `json-file`, `max-size` `10m` and `max-file` `5`: at most five files of 10 MB, the oldest dropped first. For hivemind's containers these two options take the place of any `max-size` and `max-file` in the daemon's `log-opts` (`/etc/docker/daemon.json`); the daemon's other `json-file` options still apply. With another default driver, such as `local`, which rotates its files itself, or `journald`, hivemind leaves the log config to the daemon. The agent reads the driver from the daemon at most once a minute. `hivemind logs` reads what the driver keeps, so with `json-file` it shows the last 50 MB at most. A container created by a hivemind older than 2.2 keeps its unrotated log until it is replaced, at its next rollout.

## Deleting an app

`hivemind delete` marks the app deleted at once, so `status` and the other reads stop showing it. The next reconcile pass on each node removes that node's containers. The app's rows are removed once no node reports a container.

## Security

The API token, the admin token and the file modes are described in [security](security.md). In short:

- The API listens on `127.0.0.1:4483`. An address that is not loopback needs `--api-tls-cert` and `--api-tls-key`, or the server refuses to start.
- `init` generates an admin token: 32 bytes from `crypto/rand`, with the prefix `hm_`. It prints the token once, and writes it to `<data-dir>/admin.token` with mode 0600. The server stores only its SHA-256 digest, in the database.
- Every route except `GET /healthz` needs the token as `Authorization: Bearer`, or a session cookie from `POST /v1/session`. The check is in constant time. A cookie request that changes anything also needs the session's CSRF token. See [the API](api.md#authentication).
- With `--api-tls-cert`, the CLI trusts the system's roots, or the certificates in `--ca-cert`. See [the API](api.md#tls).
- `init` refuses to run when a cluster already exists, and it never overwrites the token file or the CA.
- Logs are redacted by the logger (`internal/logging`). It masks the value of any attribute whose key contains `token`, `secret`, `password`, `passwd`, `authorization`, `cookie`, `session`, `csrf`, `privatekey`, `apikey`, `credential` or `env`, with dashes, underscores and case ignored. It also masks any value of the config's secret type, and any string that holds an admin token (`hm_` and 43 characters), a join token, a bearer credential or a PEM private key. Session IDs, CSRF tokens and environment values cannot be recognised by their shape, so the code that logs must not pass them in. The logger writes text by default, or JSON with `--log-format json`.
- The metrics listener, `--metrics-addr`, must be a loopback address, and it needs the token too.
- The router (`--route-addr`) is public and has no authentication: it serves the routed apps' own traffic. See [security](security.md#routing).

The web UI is not built. The API has the login and logout routes a browser would use. The TLS flags apply to the API only.

## Data directories

The server's data directory holds the database (`hivemind.db`, with the SQLite WAL files beside it), the admin token file (`admin.token`), the cluster CA (`ca.key` and `ca.crt`, and during a [CA rotation](ca-rotation.md) the new CA in `ca-next.key` and `ca-next.crt`), and an optional config file (`hivemind.yaml`). The default is `/var/lib/hivemind` for root, and `$XDG_STATE_HOME/hivemind` or `~/.local/state/hivemind` for other users. `--data-dir` or `HIVEMIND_DATA_DIR` changes it. `hivemind backup` copies everything in it but `hivemind.yaml`, `hivemind.lock` and a `restored-epoch` left by `restore` into one file, and `hivemind restore` puts that file into an empty directory on another machine. See [backup](backup.md).

A worker's data directory holds its key (`node.key`), its certificate (`node.crt`), the cluster CA certificate (`ca.crt`; two of them during a CA rotation), and `node.json`, which records the node ID and the server it joined. `node.json` is written last, so its presence means the join finished. A renewal replaces `node.key` and `node.crt`. `cluster-epoch` holds the highest cluster epoch the worker has seen, once a server has sent one above 0, as after a restore ([the server's address](#the-servers-address)). There is nothing on a worker to back up. [Security](security.md#files) lists the file modes.

`init` refuses a worker's data directory before it creates anything, and a client command on a worker, which has no admin token, says to run it on the server.

`serve` and `join` hold an exclusive lock (flock) on `hivemind.lock` in the data directory while they run, and write their pid in it. A second `serve` or `join` on the same directory is refused with `another hivemind (pid N) runs on the data directory ...`, before it changes anything. The kernel drops the lock when the process exits, after a crash too; the file stays, and the next `serve` or `join` takes it.

The database uses SQLite in WAL mode, with `synchronous=FULL`. Every change is one transaction. The schema has a version number, and a newer schema is refused. A v1 data directory is refused with a message, so use a new directory for v2. See [upgrading](upgrading.md).
