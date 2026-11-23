# Several servers

This is the design of the highly available control plane, new in 2.3.0. A cluster of one server, as every cluster was before it, depends on that machine: if it goes down, the containers keep running, but nothing is placed, lost nodes are not rescheduled, and the API is gone. A cluster can instead have 3 or 5 servers. They elect a leader, every server keeps a full copy of the store, and the cluster keeps working while a majority of the servers is up. A write the API acknowledged is on a majority of the servers before the answer goes out, so losing a minority loses none, and a replica is placed by one leader at a time, so it is never placed twice.

One server works as before. `init` makes a cluster of one, and nothing in this document runs until a second server joins.

## Consensus

The servers run Raft, through [hashicorp/raft](https://github.com/hashicorp/raft) v1.8.0. Writing a Raft is not the job here, and the hard parts of using one are elsewhere (what goes in the log, and what a new leader takes over). The candidates:

- hashicorp/raft is the library under Consul, Nomad and Vault, which run it in many thousands of clusters. It comes whole: leader election with pre-vote, log replication, snapshots and their transfer to a follower that is behind, membership changes one server at a time, non-voting members, and a pluggable transport, log store and snapshot store. hivemind supplies an FSM, a log store and a stream layer for the transport, and nothing of the protocol.
- etcd's raft (go.etcd.io/raft/v3) is as well tested, but it is the state machine only: the caller writes the transport, the storage, the snapshot transfer and the loop that drives it. That is the part where a bug loses data, and it would all be new code here.
- dragonboat is faster and much larger, and is used far less.

hashicorp/raft v1.8.0 adds seven modules to the build: go-hclog, go-msgpack/v2, go-metrics, go-immutable-radix, golang-lru, fatih/color and mattn/go-colorable (mattn/go-isatty is already in). go-metrics' own requirements (Prometheus, DataDog and others) are in go.sum but are not compiled: raft imports only its compat package. All are MPL-2.0 or MIT, which GPL-3.0 code may use. The size it adds to the binary is in [the measurements](#measurements).

### The log store and the stable store

Raft needs a log store (the entries) and a stable store (the current term and the vote). Both are a second SQLite file, `<data-dir>/raft/raft.db`, written through the modernc.org/sqlite driver that the store already uses, in WAL mode with `synchronous=FULL`. The other choice is raft-boltdb, which adds bbolt. SQLite here costs no dependency, its durability is the one the store already relies on, and a separate file keeps the log out of backups and out of the store's write lock. The log store says it is monotonic (`raft.MonotonicLogStore`), so after a snapshot is restored into it raft deletes the old entries rather than relying on a gap.

### Snapshots

The FSM snapshot is the store's own snapshot, `VACUUM INTO` a file (the path `hivemind backup` uses), streamed to raft's file snapshot store in `<data-dir>/raft/snapshots`, which keeps the last two. Raft takes one after 8192 entries or two minutes, whichever is later, and keeps 10240 entries after it, so a follower that was down for a few minutes catches up from the log, and one that was down for longer gets the snapshot. Each node's reports and lease renewals are entries: a steady cluster of three servers and two workers appended 210 a minute ([measurements](#measurements)), and the eighth soak's six nodes, with a write every 2 s, 5.7 a second: a snapshot about every 26 minutes.

A snapshot is restored into the open store in one transaction: the file is attached, every table is emptied and filled from it with foreign keys checked at commit, and the file is detached. A snapshot from another schema version is refused (see [versions](#versions)).

The store records the index of the last entry it applied, in the same transaction as the entry (`raft_applied`, schema version 18). Raft starts without restoring its last snapshot into the store (`NoSnapshotRestoreOnStart`), since the store on disk is already at least that far, and replays the entries after the snapshot; the store skips those it has applied. Without the index, a restart would apply them twice. A store older than the latest snapshot, as when a server stopped between raft saving a snapshot the leader sent it and the store loading it, loads that snapshot before raft starts.

### The transport

The servers talk to each other on one port of their own, the cluster port plus two: 4486 by default, beside the node listener on 4484 and the join listener on 4485. It binds the cluster address, the one the node listener binds (`--cluster-addr`, or the address `--advertise` names or resolves to), and never a wildcard, as the other cluster listeners do. Only a server opens it, and only once the cluster has a second server.

It is TLS 1.3 with a client certificate required on both sides. A certificate is accepted only if it chains to the cluster CA, names a node, carries the server role, and names a server that is in the Raft configuration and was not removed. The server role is a second URI name in the certificate, `hivemind://role/server`, which only a server's own certificate has: a server issues its listener certificate itself when it starts, from the CA, and the certificates the cluster issues to workers, at join and at renewal, never carry it. So a worker's certificate, which can reach the node listener, cannot reach this port.

Two things share the port, told apart by ALPN: `hivemind-raft/1` is Raft's own traffic (hashicorp/raft's network transport over the TLS connection), and `hivemind-forward/1` is HTTP, for the requests a follower passes to the leader ([clients](#clients)) and for a new server asking to be made a voter ([membership](#membership)). A connection that names neither is closed after the handshake.

The Raft address of a server is its advertise address with this port, such as `10.0.0.6:4486` or `hm-2.example.internal:4486`, and the dialing side checks that the certificate names that host, as a worker checks the server it dials.

Timing: raft's defaults for a LAN, a heartbeat timeout and an election timeout of 1 s and a leader lease of 500 ms. A follower that hears nothing for 1 to 2 s starts an election, so a new leader is expected within a few seconds of the old one's loss; the integration test measured 0.7 to 1.8 s after a kill and 1.4 to 1.9 s after a partition ([measurements](#measurements)). A leader that cannot reach a majority for 500 ms steps down. A connection on the server port is dialled with a 3 s timeout, and fails when the other machine has not acknowledged its data for 5 s (`TCP_USER_TIMEOUT`, on Linux), so a request passed to a leader whose machine is gone fails in seconds rather than at its client's timeout.

A leader whose `serve` stops, on SIGTERM as `systemctl stop` and `restart` send it, first hands the leadership on: raft's leadership transfer brings the follower that is furthest along up to date and has it start an election at once, which the others grant. It waits up to 3 s for that, before its API and router stop, and again before its Raft stops, in case it was elected again meanwhile; if the transfer fails, the others elect a leader once they stop hearing from it, as before. A write that reaches it during the transfer is refused as on a follower (`503`, no leader), and a client may send it again. In the eighth soak, before this, a leader that was restarted kept the leadership for the 10 s of its stop, with its API and router closed, and the cluster then had no leader for 1.0 to 1.7 s. In the unit test, three servers on loopback with timeouts cut to 300 ms, another server led, and wrote, 21, 22 and 102 ms after the transfer began, in three runs on the build host (c8g.4xlarge, `golang:1.27`).

Raft's own log lines go to serve's log, its debug lines at debug. It logs each failed RPC to another server at error, every try, which for one server that is down is about 25 lines a minute on the leader; hivemind logs those (`failed to heartbeat to`, `failed to appendEntries to`, `failed to make requestVote RPC` and the pipeline's two) at warn, the first for each server, then one every 30 s with the number left out since (`repeated=N`), and one at info, `a server answers again`, when it answers the leader. A broken connection from another server (`failed to decode incoming command`) is a warning too. A candidate's `Election timeout reached, restarting election`, which a server that cannot reach a majority logs every second or two, and `failed to flush response`, an answer on a connection that broke, are warnings limited the same way, one every 30 s each. A leader's `peer has newer term, stopping replication`, a follower answering with a newer leader's term, is at info for 10 s after this server began a handover, which asked for that election, and a warning otherwise; the handover's own line names the new leader once it has heard from it (`to=`). Raft's other errors stay errors.

## Replicating the store

Every write to the store goes through `Store.tx`, one SQLite transaction. Five writes did not: the node's version (`SetNodeVersion`), a node's CA rotation mark (`NoteCARotationNode`), and three of the routers' TLS storage operations (delete, renew a lock, unlock). They become transactions of their own.

With more than one server, a transaction that writes runs on the leader only, and works like this:

1. It takes the store's write lock, so the leader runs one write at a time, as today, and notes the leadership it starts under.
2. It runs the transaction as it does now, reads and writes, on the store's one write connection. A driver wrapper records each write statement that succeeds, with its arguments: `Exec`, and the `Exec` of a prepared statement. Reads are not recorded. The transaction's reads see its own writes, as before, so the code that decides what to write does not change.
3. It rolls the transaction back.
4. If it wrote anything, the statements, in order, are one Raft entry. `raft.Apply` returns once a majority of the servers have the entry in their logs and the leader has applied it.
5. Every server, the leader included, applies an entry by running its statements in one transaction, with the update of the applied index, and committing it.
6. The leader then runs what the transaction asked to run after its commit (the in-memory caches below), and returns.

Readers see the store as it was until the entry is applied, as they see it until a commit today. The write lock is held from step 1 to step 6, so nothing else writes in between, and the state the transaction read is the state the entry is applied to. A new leader takes writes only once it has applied every entry of the terms before its own (a Raft barrier), and an entry is appended only under the leadership its transaction started under, so no transaction reads a state that misses an entry before its own. A transaction that recorded no write, a read, runs on any server, on its copy; one that writes on a follower is refused before anything is appended.

### Why replaying the writes gives the same rows

An entry is applied on every server to the same state: the state after the entry before it, starting from the same snapshot. The leader recorded the statements against that state too. A write statement's effect depends only on the state it runs on and its arguments, provided nothing in it depends on anything else, which holds here:

- No statement calls a function with a value of its own: no `random()`, `randomblob()`, `datetime('now')`, `CURRENT_TIMESTAMP`, `julianday` or `strftime`. Every time is the store's clock, read in Go and passed as an argument, as the code already does. A test fails on any of these in the store's SQL.
- No table has `AUTOINCREMENT`, and the only `INTEGER PRIMARY KEY` tables are the single-row `cluster` and `ca_rotation`, with an explicit `id = 1`. Every other table has a key of its own data, so a row is the same row on every server. SQLite's implicit rowids may differ after a snapshot (`VACUUM` renumbers them), and nothing reads them.
- A statement that failed in the leader's transaction is not recorded. SQLite undoes a failed statement's own changes and the transaction goes on, so leaving it out changes nothing.
- A single `INSERT`, `UPDATE`, `DELETE` or `REPLACE` that changed no row is not recorded either: it changed nothing of the state the leader ran it on, every server applies the entry to that same state statement by statement, so it would change nothing there, and the statements after it see the same rows without it. A transaction of only such statements appends no entry, and costs no Raft round; on a follower it is still refused, as any transaction that runs a write. The count is SQLite's own (`sqlite3_changes`), which counts a row an `UPDATE` matched even when it sets the values the row has, so an `UPDATE` that can set the same values says so in its `WHERE`, as the placement's write of a pending replica's reason does (`AND pending_reason != ?`).
- No statement writes through a query (there is no `RETURNING`). The recorder refuses one that would, while it records, so a later change cannot slip it past replication.
- There are no triggers and no temporary tables. Cascading deletes are part of the statement and run the same everywhere.
- Arguments are encoded exactly: integers as integers, floats by their bits, strings and blobs as their bytes (JSON would replace invalid UTF-8 in a string, which a container's error text can hold), and times by `MarshalBinary`.
- Each entry carries the schema version of the leader that wrote it. A server at another version stops rather than apply it ([versions](#versions)).

The test applies the same batches to two stores and compares every row of every table, after a workload that touches every table: apps, scales and deletes, rollouts and drains, node registration, reports, loss and removal, routes and TLS storage, registries, join tokens, the CA rotation's marks.

If a server fails to apply an entry, as on a full disk, it must not go on with a store that lacks it: it tries once more, and then stops serve with the error. On its next start, raft gives it the entry again, since the store's applied index did not move. The leader's own transaction would have failed first on a full disk, before anything went into the log.

### In-memory state

Some of the store's state is kept in memory, and some of serve's is derived from the store. On a server that applies an entry another server wrote, or one that becomes the leader:

- The routing table cache's version is raised, so the next read reads the table again.
- The denylist of removed nodes is read again from `removed_nodes`.
- The digest of each node's last report, the time each node's rows were last rewritten, and the leases this process last committed are dropped, so the first report to a new leader rewrites the node's rows, as the first report to a restarted server does.
- The registry credential sizes and the placement cache are dropped.
- The start grace is set again when a server becomes the leader ([leader-only work](#leader-only-work)).
- The epoch is the store's, the same on every server; a server reads it at start, as today.
- The CA rotation manager follows the rotation's record in the store: at start, after each entry another server wrote, and when this server becomes the leader ([the CA rotation with several servers](#the-ca-rotation-with-several-servers)).
- What the leader keeps in memory of the leases and report stamps is dropped when it stops leading ([fewer Raft entries](#fewer-raft-entries)).
- The routers' counters, which workers post every few seconds for `/metrics`, and how each router reads the routing table, are held by the server that takes the posts, the leader. A new leader has them again after the workers' next posts.

## Leader-only work

Only the leader runs:

- the sweep: lost nodes, pending replicas, expired join tokens;
- rollouts and drains, which the sweep and the agent's passes advance;
- the start grace;
- the CA rotation manager's run, and the steps of a rotation, which every server then follows;
- writing the leases and report stamps it keeps in memory, every 4 s ([fewer Raft entries](#fewer-raft-entries));
- asking every server for its build every 10 s, and the step of the schema ([rolling upgrades](#the-step));
- issuing node certificates, at join and at renewal, since both arrive through the node and join listeners, which followers pass to the leader;
- the routers' TLS storage and its locks, which are store writes;
- backups through the API (`hivemind backup --api`), which a follower passes to the leader like any other request; `hivemind backup` on a server reads its own data directory, which has a copy of everything.

Each server issues its own listener certificate when it starts, from the CA, and renews it, as the one server does today.

A server that becomes the leader starts the sweep and the CA rotation manager's run, and a server that stops being the leader stops them; a write it had in flight then fails. Before the sweep's first pass, the new leader sets the start grace as `serve` does at its start (`store.SetStarted`), with `--start-grace` (2 minutes by default). Every node whose lease was current when the leader changed is then waiting: it is not lost, and its replicas stay where they are, until it reports to the new leader or the grace ends. A failover therefore does not mark live workers lost while they find the new leader, which takes them a few seconds. What it costs is the same as after a restart: a node that died with the old leader, such as the old leader's own machine, has its replicas placed again when the grace ends, not 15 s after its last lease. The own-node exception of the start grace becomes the new leader's own node, which reports through its own store.

Leases are stamped with the leader's clock, and a new leader judges them by its own. Keep the servers' clocks in sync with NTP: the grace covers a skew of seconds, not of minutes.

## Clients

### The API

Every server serves the API on its `--api-addr`, as the one server does. A follower passes every request to the leader but `GET /healthz`, which it answers itself, and `GET /v1/servers` while there is no leader. It proxies rather than redirects:

- The leader's API usually listens on loopback (`127.0.0.1:4483`, the default), so a redirect would point at an address the client cannot reach. The follower sends the request to the leader over the server port ([transport](#the-transport)) instead.
- The CLI then works with any server's address, or with a list of them (`HIVEMIND_SERVER=https://10.0.0.5:4483,https://10.0.0.6:4483`, or `--server` with the same), which it tries in order until one answers. With a list, it gives each server 2 s for the TCP connect, and 3 s in all to have a connection, the TLS handshake included, before it tries the next, where a single server gets 30 s, and it starts at the server that answered the last command given the same list. It keeps that server's URL in the user's cache directory (`$XDG_CACHE_HOME/hivemind`, `~/.cache/hivemind` on Linux), in a file of mode 0600 named by a hash of the list, and takes it only if the list has it. In the eighth soak, before this, with the first of three servers powered off every call waited 30 s for it, and then 3.1 s, until the machine was back.
- The caller's credential, the token or the session cookie and CSRF token, goes with the request, and the leader checks it, as if the caller had reached it directly. The follower does not check it: a session lives on the leader, so the follower could not. The follower sends, beside the request, the caller's address, for the leader's login limit and logs, and whether the caller came over TLS, which the backup route requires off loopback.

When no server is the leader, as during an election or when a majority is down, a follower answers every other request with `503` and `{"error": "no leader: ..."}`, saying how many servers it can reach and how many a write needs, and that running containers are not affected. A request that was passed to a server that is no longer the leader gets the same `503`, and is not passed on again. A write whose leader lost the leadership while the write was in its log gets `503` saying the write may or may not have been applied: Raft can commit it under the next leader. A request the follower passed to a leader that did not answer it, as one whose machine or process has just gone, gets `503` saying so (`the leader, ... did not answer this server`); it is marked like the no-leader answers when the leader cannot have acted on it, because the request never reached it or is a `GET` or `HEAD`, and otherwise says that the write may or may not have been applied. The no-leader answers carry the header `Hivemind-No-Leader: 1`, which says that the server did not act on the request and another server may.

The CLI prints these as they are. Given a list of servers, it tries the next one on a marked `503`, and on a server it cannot connect to; on one whose connection broke after the request was sent, it tries the next only for a `GET` or a `HEAD`, since the server may have acted on a write, and says so. When a server gave a marked `503`, it tries the servers again every 500 ms for up to 3 s after the first one, which covers an election (1 to 2.5 s in the eighth soak), and then prints the last answer that no server leads, rather than the error of a server that did not answer. A server that knows no leader says in `Hivemind-No-Leader-For` how long it has known none, in whole seconds: since it last heard from a leader, stopped leading, or started. Given one server that says 3 s or more, which is longer than an election takes, the CLI prints its answer at once, as there is no other server to try; it tries one server again for up to 3 s when it says less, as during an election, when it knows a leader it could not use, as while the leadership passes on, and when it does not say, as a 2.3 server does not. A list is tried for up to 3 s as before. In the fifth acceptance run, before this, with one of two servers stopped, each call given the other one's URL took 3 s to print that it had no leader. In the ninth soak, before this, with no leader and two servers whose processes were stopped, so that their machines took the connection and nothing answered the handshake, each of them cost a call 5 s, and the CLI printed the second one's `TLS handshake timeout`. In the eighth soak, before this, a read during the rolling update with the leader's restart failed with `the leader ... did not answer this server`: the stopping leader refused its connection, and each follower passed it to that leader, which was still the leader they knew.

### Workers

A worker keeps the servers it knows in `node.json`, as `servers` (host:port of each node listener), beside `server`, the one it joined. A server sends the list on every node API reply (`Hivemind-Servers`), from the store's server table, and the worker writes it to `node.json` when it changes. A worker dials the first server in the list that answers; when a request fails to connect or gets `503` for no leader, it moves to the next. Every server's certificate names its own advertise address, from the same cluster CA, so the worker checks each one as it checks the one server today, the epoch included.

A follower's node listener passes each request to the leader, with the worker's certificate chain, which the leader verifies again against the cluster CA before any handler reads it; every node route then works as on the one server. A node request whose write the leader loses to a change of leader gets the API's `503` and message, logged as a warning: marked no leader when nothing was written, so that the worker moves to the next server, and saying the write may or may not have been applied otherwise, which the agent's next pass sends again. In the ninth soak, before this, both were `500` with `internal error`, and an error line on the old leader at each change of leader. The follower's join listener passes joins to the leader the same way. So a worker can use any server, the log poll and the routers' TLS storage included, and its router keeps serving with the table it last read while no server answers, as it does today when the server is down.

A follower's own node, its agent, router, log requests and router counters, uses the node API too, through its own node listener, with its server certificate as the client certificate (a node certificate is issued for both client and server use). On the leader they use the store directly, as on the one server. So a follower's router, like a worker's, keeps the table it last read while no server leads, rather than read its own copy, whose leases nothing renews then. A router that starts reads the table again every 250 ms while the read fails, for up to 5 s, before it takes TLS connections, which wait in the listener's backlog meanwhile: a server's router that starts during an election, as a restarted leader's does, gets the table from the new leader, or from its own store once its server leads. Each of those reads gives way after 1 s, so that one stuck on a server that does not answer, as one whose process is stopped, leaves time for the next. One that starts while no server leads for longer starts without a table, as a worker's does while its server is away, and fails the TLS handshakes of every host until it has one; until then it reads the table every 250 ms, each read given 1 s, then twice as long after each that ran out, up to the 10 s a read has once it has a table. A server's own node starts each node API request at its own node listener while the server knows a leader, which the listener passes the request to; and a node API client whose request ended before a server gave it a connection starts the next request at the next server. In the ninth soak, before this, a restarted leader's router, with the other two servers' processes stopped, gave its one read at the start the whole 5 s, and its first poll another 10 s, to a stopped server, though its own server led after 1.7 s in one run and 8.1 s in the other.

### The routers' TLS storage

A follower's router reads the TLS storage, the certificates, their keys and the ACME account, from the follower's own copy of the store, and writes, deletes and takes the issuing locks through the node API, on the leader. So once it has its table it loads the certificates the store has, also while no server leads. Two reads still go to the leader: a key that the follower's copy does not have, since it may be one stored a moment ago whose entry the follower has not applied yet, and taking it for missing could make certmagic order a certificate the storage holds; and a challenge token, whose copy could be the previous challenge's for that host, which a CA's validation would fail. Without a leader, those fail, as they do for a worker's router whose server is away.

What a follower's router can read that is stale: a certificate, key or account that the leader has replaced, for the time between the leader's commit and the follower's apply of the entry, which is milliseconds while one leads, or for as long as no server leads. Each such value is one the CA issued for that host, or the account's key, and a certificate is served only while it has not expired, so a client accepts it; replacing it takes the lock on the leader, which waits for one. A certificate and its key are stored as separate entries, so a load between the two can find a new certificate with the old key, which fails to load, and certmagic tries again; the leader's own reads between the two writes could always see the same.

## Membership

`init` makes a cluster of one, with no Raft. To add a server:

```sh
# On a server of the cluster, which runs with --advertise (here 10.0.0.5), as for a
# worker's join. A server token works once and expires after 1 hour by default.
hivemind join-token --role server

# On the new machine, which needs Docker Engine and the hivemind binary, and an empty
# data directory. TCP 4484 to 4486 must be open between it and the other servers.
hivemind join 10.0.0.5 --role server --token-file join-token --advertise 10.0.0.6   # join-token: a file that holds the token
```

`join --role server` joins the cluster, and runs the new server in the foreground, as `join` runs a worker; `hivemind serve --advertise 10.0.0.6` runs it again after a stop, and so does a plain `hivemind serve`, since `join` writes the flags it was given, `--advertise` included, to `<data-dir>/hivemind.yaml`. To run it as a service, see [systemd](systemd.md#another-server). `join` has no API flags, so the new server's API listens on `127.0.0.1:4483` until its unit or its `hivemind.yaml` gives it `--api-addr`, `--api-tls-cert` and `--api-tls-key`. A server token and a worker token are not interchangeable: each is refused where the other is expected, with the same message as an unknown token. (The flag is `--role server`, as on `join`, not `--server`: every command that calls the API already has `--server`, the API's URL.)

What a joining server receives, in the join reply: its node ID, chosen by the leader, the CA certificate and the CA's private key, and the list of servers. It needs the key to issue its own listener certificate, and, once it is the leader, the workers' certificates. The reply goes over the join listener's TLS 1.3 connection, whose CA the joining machine has pinned by the fingerprint in the token, before it sends the token: the check a worker's join makes. The token is single use, expires, and is stored as a hash; the join listener's limit on failed joins applies. The key is written with mode 0600 in the 0700 data directory. The admin token is not sent: its SHA-256 is in the store, and the store comes by Raft snapshot over the server port. The joining server writes no `admin.token`; give its CLI the token with `HIVEMIND_TOKEN`.

The first server join turns Raft on, on the server the join reaches (or the leader it passes it to, which with one server is itself):

1. It bootstraps a Raft cluster with itself as the only voter, and opens the server port.
2. It hands raft the store as it is now as the log's first snapshot (`raft.Restore`), so a server that joins later gets that snapshot and then the entries after it.
3. From then on, writes go through the log.

Then, for every server join:

4. The leader adds the new server as a non-voter. A non-voter gets the log but does not count for a majority, so a server that joins and never comes up does not stop the cluster's writes.
5. The new server starts, receives the snapshot and the entries since, and once it has applied every entry the leader has committed, asks the leader, over the server port, to make it a voter.
6. The leader adds it as a voter.

`hivemind servers` lists the servers: ID, name, advertise address, Raft role (leader, follower, or non-voter while catching up), and the last time the leader heard from each. Without a leader, the server asked answers it itself, from its own copy of the configuration, with `leader` empty in the JSON and a `note` that says why; the CLI prints the list, then `error: no leader, as <server> sees it: ...`, and exits with status 1, with `--json` too.

`hivemind server remove <id>` removes a server from the Raft configuration and the server table. It refuses to remove the last voter, and refuses a removal that would leave fewer servers that the leader heard from in the last 10 s than a majority of those that stay, unless given `--force`. The removed server's serve stops within about 5 s, once it has applied the deletion of its row from the server table (Raft sends a follower nothing once the change that removes it is in the leader's log, so the table, which `server remove` changes first, is how it learns), with exit status 3. Its agent stops with it, so drain the server's node first, while it serves: `hivemind node drain <id>`, then `hivemind server remove <id>`, then `hivemind node remove <id>`. `server remove` refuses a server whose node still has replicas without a volume, and says to drain it: removed, the server would keep their containers running on the machine, and once its node's lease ran out they would be placed again on other nodes and run twice. A replica with a volume stays on the node, as after any drain. The node of a server that is down has nothing to move once it is lost, 15 s after its last lease, as its replicas have been placed again by then. `--force` removes the server anyway; its containers then run until the machine joins again or they are removed with `docker rm`. A `node drain` of a lost node has nothing to move and waits, so remove the node with `hivemind node remove`. Its certificate is refused on the server port from then on. To use the machine as a server again, give it an empty data directory and a new server token; the join removes the old containers.

A server that is down when it is removed never gets the entry that deletes its row: the leader sends it nothing. When its machine comes back, its vote requests are refused at the handshake, so it knows no leader, and its own node's requests go to the other servers' node listeners, whose replies list the servers without it. At the second check in a row that finds it so, 5 s apart, it stops with the removal message and exit status 3, about 10 to 15 s after its start. If its node was removed too, the node API's refusal of the node stops it the same way. Until then its API answers `503` for no leader. Its containers keep running, so drain its node before the removal, or, if it was down, remove the containers its node ran with `docker rm` before it comes back, or give it an empty data directory.

How many servers: 3 or 5. With 2, both are needed for a write, so losing either stops writes: no better than one, and worse, since two machines can fail. `join-token --role server` warns when the cluster would have 2. Joins past 7 servers are refused: each write waits for a majority, and more servers make that slower without a need for it in a cluster of ten machines.

When a majority of the servers is down, or cut off from the rest, there is no leader: no writes, no placement, no rescheduling. The containers keep running, every worker keeps its own, and every router keeps serving with the routing table it last read. The API answers `503` with the reason ([clients](#the-api)). When enough servers are back, they elect a leader, which starts with the start grace.

## Compatibility

A cluster of one upgraded from 2.2.0 changes nothing until a second server joins: no Raft, no new listener, and writes go to the store as before. The store moves to schema version 18, which adds the applied index, the server table and a role for join tokens; it adds tables and a column only.

A worker of 2.2.0 or older knows one server, the one it joined. It goes on working through that server, which passes its requests to the leader, so a second server is not refused for its sake. It cannot fail over: if its server is down, it waits, as it does today. Upgrade the workers to have them fail over.

In 2.3.0 all servers run the same release: a server will not apply an entry from a leader at another schema version, and a joining server must run the leader's version, so the servers are upgraded together. From 2.3.0 to 2.4.0 and on, they are upgraded one at a time ([rolling upgrades](#rolling-upgrades)).

Backup: `hivemind backup` on any server copies that server's store and CA files, as on the one server, and `hivemind backup --api` is passed to the leader like any API request. Raft's directory and `server.json` are not in it. Restore makes a cluster of one: the restored data directory has no Raft, restore empties the store's server table and its applied index, and the epoch is one higher than the backup's, so every worker refuses the old servers' copies. The other servers join again, each with an empty data directory and a new server token, and their old node rows are removed with `hivemind node remove` as for any lost node. The restored server runs as the node of the server the backup was taken on, and `restore` refuses a backup whose node was removed ([backup](backup.md#a-cluster-of-several-servers)). `serve` refuses a store that lists servers in a data directory without Raft state, as when `raft/` was deleted: serving it would start a second cluster beside the others ([backup](backup.md#a-cluster-of-several-servers)).

The CA rotation needs a cluster of one in 2.3.0: `hivemind ca rotate start` is refused while the cluster has more than one server, and a server join is refused while a rotation is in progress. In 2.4.0 a cluster of several rotates once its store is at schema version 19, which is once every server runs 2.4.0 ([the CA rotation with several servers](#the-ca-rotation-with-several-servers)); a server join is still refused during a rotation.

## Rolling upgrades

New in 2.4.0: the servers of a cluster are upgraded one at a time, while the others keep the cluster's writes going, and the store's schema moves once every server runs the new release.

### What an entry carries

Every log entry starts with a header of four bytes, `HMB` and a format byte. Format 1 (`HMB1`, the only one 2.3.0 writes or reads) is a batch of write statements, and carries, after the header, the schema version of the store it was recorded against. Format 2 (`HMB2`) is a step of the schema, from one version to the next, and carries the two versions. 2.3.0 refuses an entry whose header is not `HMB1`, and a batch at a schema version other than its own.

What changes is the version a batch is recorded at. In 2.3.0 it is the build's, which is the store's, since a store is always migrated to the build's version when it opens. In 2.4.0 it is the store's, the cluster's schema version, and a server of a cluster of several no longer migrates its store when it opens it ([below](#opening-the-store)). A 2.4.0 leader whose cluster is at version 18, 2.3.0's, records its batches at 18, so a 2.3.0 server applies them.

The statements of a batch ran on the leader's own store, in the transaction the batch records ([replicating the store](#replicating-the-store)), so a batch holds only statements that are valid at the version it carries: a statement on a column the store lacks fails there, before anything goes into the log. What a newer build must also not do at an older version is write a value that the older build reads another way. In 2.4.0, the only new writes at version 18 are the lease and report stamps of [fewer Raft entries](#fewer-raft-entries), which are the same statements 2.3.0 runs, less often; the CA rotation's new records need version 19, and a rotation with several servers waits for it.

### Each build's versions

Each build knows its schema up to a newest version, the length of its list of migrations (19 for 2.4.0), and runs a store at any version from an oldest one up to that one (18 for 2.4.0). Its code checks the store's version before it reads or writes what a later migration added. A release that adds no migration has the versions of the release before it; one that adds a migration raises the oldest version it runs to the newest of the release before, so two releases that each changed the schema are never both in one cluster ([what is refused](#what-is-refused)).

### Opening the store

A cluster of one migrates its store when `serve` opens it, as before: it has no other server to wait for.

A server of a cluster of several, one whose data directory has Raft state or `server.json`, opens its store at the version the store has, if the build runs it, and migrates nothing. A store at a version below the build's oldest is refused, as two releases apart; one above its newest is refused as newer than the build, as 2.3.0 refuses it. A server that joins makes its new store at its oldest version, and moves it on when it loads the leader's snapshot (below).

### Applying entries

A server applies:

- a batch recorded at its store's version, as before;
- a step from its store's version to the next, if the build knows the next: it runs that migration, its own copy of it, in one transaction with the applied index. Released migrations are never edited, so every build that knows a version runs the same statements to reach it.

It refuses anything else: a batch at another version, a step from another version or to one the build does not know, a format it does not know. A refused entry stops `serve` with the error, as any entry a server cannot apply does, rather than go on with a store that lacks it; unlike a failed write, it is not tried a second time, which could not change the answer. A leader that stops this way hands the leadership on first, as at any stop of `serve`. On its next start Raft gives it the entry again, and it refuses again, until it runs a build that applies it.

A server checks the applied index before the version: on a restart, Raft replays the entries after the last snapshot, and those the store applied before a step are skipped, not refused for their older version. In 2.3.0 the order made no difference.

A snapshot carries the version of the store it copies. A server loads one at its store's version as before, and one at a newer version that the build runs by first migrating its store to that version (the snapshot then replaces every row). It refuses an older snapshot, and one above the build's newest version. A snapshot Raft sends is never older than the store it goes to, since Raft sends one only to a server behind the leader's log, and the log's order puts every step before the batches recorded after it.

So, between releases N and N+1:

- A server of N+1 under a leader of N applies everything the leader writes: batches at N's version, which N+1 runs. Snapshots from that leader are at that version too.
- A server of N under a leader of N+1 does the same while the cluster is at N's version: the leader records at the store's version, and writes no step while a server of N is in the cluster. If a server of N gets a step anyway, as one that was moved back to N between the leader's last look and the step, it stops, as above, and so steps down if it leads.

### The step

The leader moves the schema, one version at a time, once every server can apply the next one. Every 10 s it asks each server of the Raft configuration, voters and non-voters, for its build, over the server port: a request of a new kind, `build`, on the forward protocol (`hivemind-forward/1`), which a server of 2.4.0 answers whether it leads or not, with its version, the oldest and newest schema version it runs, and its store's version. A server of 2.3.0 answers a kind it does not know with a refusal (as a follower, `503` with no leader), which the leader reads as a build that runs version 18 alone. A server that does not answer is not known. When every server's answer runs the next version, the leader appends the step, as a transaction of its own under the store's write lock, so no batch recorded at the old version can come after it in the log. A server that is down holds the step back: an upgrade ends once every server is up and upgraded, or removed with `hivemind server remove`.

The step takes no command. Until the leader writes it, every server can go back to 2.3.0, one at a time, as it came. Once it is written, a 2.3.0 binary refuses the store (`the store has schema version 19 and this build knows up to 18`), and the way back is a restore of a backup taken before the upgrade, which makes a cluster of one ([compatibility](#compatibility)).

A server join is taken when the joining build runs the cluster's version. The join request carries the joining build's oldest and newest versions; a 2.3.0 request carries only its version, and the leader takes it as 18 alone. So a 2.4.0 leader takes a 2.3.0 server while the cluster is at 18, and not after the step. A 2.3.0 leader takes only its own version, as before, and refuses a 2.4.0 join request before it reads the version, as an invalid join request, for the fields it does not know: join a 2.4.0 server to a cluster whose leader runs 2.4.0.

### The order

Upgrade the followers first, one at a time, and the leader last:

1. On a follower, install the new binary and run `systemctl restart hivemind`. Wait until `hivemind servers` shows it a voter that the leader heard from, with the new release, before the next server: with three servers, a second one down stops writes.
2. The same on the other followers.
3. The same on the leader. Its stop hands the leadership to the follower furthest along ([the transport](#the-transport)), which now runs the new release, and it starts again as a follower.
4. Within 10 s of the last server's start the leader writes the step, every server applies it, and `hivemind servers` shows the store at the new version.

Any order works: each direction is above. Followers first makes the upgrade one change of leader, at the end, and puts the leadership on the new release only once every follower runs it, so a fault of the new release shows on a follower first, while the old leader still takes the writes.

### What `hivemind servers` shows

A VERSION column, each server's release: from its answer to the build request, or, for a server that does not answer it, the version its node last sent with a node API request (`nodes.version`, which a server's own node sends as a worker's does), marked `(node)`; `-` when there is neither. Below the list, the store's schema version, and while the leader's build knows a later one, what the step waits for: `Store schema 18; it moves to 19 once every server runs it: node-1a2b (v2.3.0, which runs schema version 18 alone).` In the JSON, each server has `version`, `version_from` (`build` or `node`), `schema_oldest` and `schema_newest`, and the reply `schema`, `schema_next` and `schema_waits`. Against a 2.3.0 leader, which sends none of them, the CLI shows `-` and no schema line.

### What is refused

- Two releases apart: a server whose store is at a version below the build's oldest refuses to start, and says to upgrade every server to the release between first; a joining server whose versions do not include the cluster's is refused. For 2.4.0, whose oldest version is 2.3.0's, this refuses nothing yet; 2.5.0, if it adds a migration, refuses a store at 18.
- A downgrade past the step: the older binary refuses the newer store as it does today. Before the step, going back is allowed, and is the way back from an upgrade stopped half way.
- The step itself while a server is down, does not answer, or runs a build without the next version.

### Testing

Unit tests: opening a store of a cluster of several at each version against a build's oldest and newest, the version of a build being a store option in tests; applying each format at each version, and the applied index checked before the version; a snapshot at a newer version loaded after a local migration, and an older one refused; the step on three servers on loopback, held while one of them answers as 2.3.0 does and taken once it answers as 2.4.0; a 2.3.0 join request.

`scripts/integration-ha.sh` (or a script it runs): three servers and two workers on 2.3.0, built from the tag with `golang:1.27`, under the existing write load and the request streams through the workers' routers. The first follower is upgraded: a 2.3.0 leader with a 2.4.0 server. Then the second, and then the leader, whose stop hands the leadership to an upgraded server. Before its new binary goes in, the old leader is started once more on 2.3.0: a 2.4.0 leader with a 2.3.0 server, which applies what it writes. Then it is upgraded. After each step: every acknowledged write in every server's store, the three stores the same row for row, no replica on two nodes, no failed request through the workers' routers, and the releases in `hivemind servers`. At the end the step to 19 on every server, and a 2.3.0 binary refused on an upgraded data directory.

## The CA rotation with several servers

New in 2.4.0. The three steps and what each side trusts are those of [the rotation](ca-rotation.md); what changes is that every server follows them.

### Where the new CA is kept

The store records the new CA. Schema version 19 adds to `ca_rotation` the new CA's certificate and its private key, sealed, and to `cluster` the fingerprint of the cluster CA, the one in every server's `ca.crt`. The record is what every server follows; each server's `ca-next.crt` and `ca-next.key` are its own copy, which it writes from the record.

The key is sealed with AES-256-GCM, under a key derived with HKDF-SHA256 from the old CA's private key, with the two CAs' fingerprints as the associated data. Every server has the old CA's key, in `ca.key`, and nothing else in the cluster does, so the new key cannot be read from the store, the Raft log, its snapshots, or a copy of `hivemind.db` taken without `ca.key`. At `finish` every server's `ca.key` is replaced by the new CA's, and what is left of the sealed copy in older log entries and snapshots can no longer be opened, but with the old key from a backup taken before `finish`, which then gives the new key too: delete those backups once the rotation has finished ([security](security.md#several-servers)). Unsealed, the cluster's new CA key would stay readable in `raft.db` for as long as the log keeps those entries: 10240 entries after a snapshot, which with [fewer Raft entries](#fewer-raft-entries) is most of a day. A backup holds `ca.key` and the store, so it can open the sealed key, as a backup taken during a rotation holds both keys today.

The other way considered: each server fetches the new key from the leader over the server port when it sees the rotation in its store. That keeps the key out of the log, but needs a leader that holds the files when the server asks, which a server that comes back after a change of leader cannot count on. The record in the store is there for every server that applies the log.

### The steps on every server

The API's steps run on the leader, which writes each one to the store in one transaction, as the one server does. Every server, the leader included, follows the record: at its start, and after it applies an entry that changed `ca_rotation` or `cluster`:

- `start`: the leader makes the new CA in memory and records the rotation, with the certificate and the sealed key. Each server then opens the key with its `ca.key`, checks that it matches the certificate and the recorded fingerprint, writes `ca-next.key` and `ca-next.crt` with `O_EXCL`, syncs them and the directory, and only then accepts certificates from both CAs. The record first and the files second, the reverse of 2.3.0's order, since the record now holds all that the files hold: a server that stops in between writes the files at its next start. The step's reply waits for the leader's own files.
- `renew`: nothing on disk; each server's accepted CAs and the CA set follow the record.
- The listener: the leader records the move to the new CA once every node trusts it, the node of every server included: a server's own node now sends what its server accepts (`Hivemind-CA-Trust`), and a server's node that is down holds the move as a worker's does. Each server then issues itself a certificate from the new CA, and presents it from its next handshake, before it acts on anything after the move in the record. A server's own node presents its server's certificate, so it renews when that moves.
- `finish` and the end of an abort: each server does the file step for itself, renaming `ca-next.key` and `ca-next.crt` over `ca.key` and `ca.crt`, or removing them when the rotation went back to the old CA, and accepts the kept CA alone. The leader then deletes the rotation and records the kept CA's fingerprint in `cluster` in one transaction. A server that applies both entries before it looks does the file step from that fingerprint: a `ca-next.crt` with it is renamed over `ca.crt`, one without it is removed. In 2.3.0 a `ca-next.crt` with no rotation in the store was always from a `start` that had not recorded its rotation, and was removed; with the record first that cannot happen, and removing the file could drop the CA the cluster moved to, so the fingerprint decides. A server whose `ca.crt` is neither the recorded CA nor its `ca-next.crt` refuses to start, and says that it holds another CA than the cluster's.
- A leader that stops in the middle of `finish`, after the ending was recorded and before the rotation was deleted: the next leader finishes it when it starts to lead.

Connections on the server port that were opened before the end keep the certificates their handshakes checked; every new one is checked against the kept CA alone. The design had each server close, at the end, those whose peer certificate is from the CA that was dropped; that is not done, since neither Raft's transport nor the forward client lets a connection be closed by its certificate, and every peer on them is a server of the Raft configuration that was not removed.

The order keeps the servers connected. A server presents a certificate from the new CA only once every server accepts it: the listener moves only once every server's node trusts the new CA, and a server's node trusts it only once its server accepts it. A server stops accepting the old CA only once its own certificate is from the new one: the end comes after the move in the log, and a server's certificate moves before it acts on what comes after.

Workers follow the rotation as with the one server. A follower passes their requests to the leader, which records what each trusts and holds and sends the CA set; every server's node listener accepts what its record says.

### Joins and backups during a rotation

A worker joins during a rotation as with one server: any server's join listener passes the join to the leader, which issues the certificate from the CA the token names.

A server join stays refused while a rotation is in progress, and `start` is refused while a server is a non-voter catching up. A server that joined mid-rotation would get `ca.key` in its join reply and the sealed key from the store, and could follow; but its first certificate, before the snapshot reaches it, would be from the old CA whatever the listener, and a rotation is a few minutes of an operator's time. Join it after `finish`, when the join reply has the new CA.

`hivemind backup` on any server copies its store and its CA files, and checks them against the rotation the copied store records, as with one server. On a server that has applied a step and not yet written or renamed its files, the check fails and asks to run it again. `hivemind backup --api` goes to the leader, which holds its steps off meanwhile. A restore makes a cluster of one in the phase the backup was taken in, with both keys, as before.

### A server that is down

A server that is down for part of a rotation opens its store as it was when it stopped, applies the log or loads a snapshot, and follows the record as above: it writes its `ca-next.*` from the sealed key, takes each phase, issues itself a certificate from the new CA if the listener has moved, and does the file step if the rotation has ended. Until it is back, its node holds the listener move (unless it trusted the new CA before it stopped) and `finish` (unless it had renewed), as a worker's does, so the rotation does not leave it behind without `--force`. The others accept both CAs until `finish`, so it reaches them with the certificate its stale record gives it.

A server cut off by `finish --force` comes back with a certificate the others no longer accept, or trusting a CA they no longer present, and cannot reach the log, as a worker cut off cannot reach the server; raft's lines on it name the failed certificate check (`x509: certificate signed by unknown authority`). Remove it (`hivemind server remove`, then `hivemind node remove`) and join the machine again with an empty data directory.

### What is refused

`hivemind ca rotate start` with several servers is refused while the store is below version 19, which is while a server runs 2.3.0 or the step has not been written: the refusal says so and points to `hivemind servers`. A cluster of one is at 19 from its first start on 2.4.0. A rotation that a cluster of one had in progress when it was upgraded from 2.3.0 has no certificate or sealed key in its record; the server writes them from its `ca-next.*` at that start.

`hivemind ca status` adds a table of the servers, `SERVER`, `CA.CRT`, `CA-NEXT.CRT` and `CERTIFICATE FROM`, by the first 16 hex digits of each fingerprint, from each server's answer to the build request (at most 10 s old), so a server that has not followed a step shows.

### Testing

Unit tests: sealing and opening the key, refused with another CA's key and with other fingerprints; a server following the record from each phase, and from each pair of entries applied before it looks; the end decided by the recorded fingerprint; three servers on loopback through `start`, `renew` and `finish`, with one stopped through the listener move, and an abort in each phase.

`scripts/integration-ha.sh` (or a script it runs): three servers and a worker with a router, under writes and requests through the router. `start`; a follower stopped once its node trusts the new CA; `renew`; the worker renews and the listener moves without the stopped server; the stopped server started again, which writes its files, issues itself a certificate from the new CA and renews; the leader restarted once, so the next leader carries the rotation; `finish`. At the end every server's `ca.crt` and `ca.key` are the new CA, no `ca-next.*` is left, the worker's `ca.crt` holds the new CA alone, and no acknowledged write was lost and no request through the router failed.

## Fewer Raft entries

New in 2.4.0. In the ninth soak's steady state, 89 % of the log's entries were a node's lease: each agent reports every 2 s, which renews its lease and stamps its container rows, and renews the lease every 5 s besides, and each is an entry, written and fsynced on every server ([ninth soak](soak.md#steady-state-7)). Only the leader reads them: the sweep, the routing table, placement and the start grace all run on it.

### Batched, not kept out of the log

The leader could keep the leases in memory alone, and write a node's lease only when the node registers or comes back. Then every reader would have to read the leader's memory: the routing table, placement, the sweep, the start grace and node removal read `last_lease` in their SQL, and a follower's store, its backup and a new leader would hold leases as old as each node's registration. Batching keeps the leases in the store, close enough to the truth that no reader changes:

- On the leader of a cluster of several, a lease renewal, or a report that changes nothing but the time, from a node whose stored lease is no older than 9 s and which was not removed, writes nothing: the leader keeps the time in memory, for the lease and for the container rows' stamps.
- Every 4 s the leader writes every lease and stamp it keeps, for every node, in one transaction: one entry for the whole cluster.
- A renewal of a node whose stored lease is older than 9 s, or has lapsed, a node's registration, and a report that changes anything, are written at once, as before. So is the stamp of a report whose node's rows were deleted since, as by a removal.
- A leader that hands the leadership on at its stop writes what it keeps first.

A live node's stored lease is then never older than 13 s (9 + 4), inside the 15 s lease, so no reader sees it lapse; a node that stops reporting is lost 15 s after its last renewal, as before, since the write after that renewal records it. `hivemind nodes`, `/metrics`, the CA rotation's live nodes and the log routes read the leader's own view, the later of the stored lease and the kept one, so their lease ages stay to the second.

An agent also skips its 5 s renewal when a report succeeded within the last 5 s, after its first renewal, which finds out whether the server has the renewal at all: against a 2.0.0 server, which has none, it says so once, at the start. That takes away the renewals, two in seven of the node API requests of a node whose reports go through; it was the renewal's purpose to cover a pass that takes longer than the lease, which a recent report already did ([leases](concepts.md#leases-and-node-loss)).

A cluster of one has no log, and writes every renewal to the store, as before.

### Before and after

Three servers and ten nodes (the servers' three and seven workers), steady, with nothing written through the API, by `WORKERS=7 scripts/raft-rate.sh` as in [measurements](#measurements): 50 apps that no node can take, and the entries each server appended in 300 s after 60 s to settle, read from its `raft.db`. On an Apple M3 Pro, with the containers in OrbStack's Linux VM (12 CPUs, 16 GB) and each capped at one CPU:

| | Entries | Bytes | A minute |
|---|---|---|---|
| 2.3.0, built from its tag | 2,102 | 567,540 | 420 entries, 110 KiB |
| 2.4.0 | 75 | 79,862 | 15 entries, 15 KiB |

The three servers appended the same entries in each run. After, what is left is the batch of kept leases and stamps, one entry every 4 s, and the entries of what changes, which for a steady cluster is none. The entries no longer grow with the number of nodes; their size does, by two statements a node, so the bytes fell by less than the entries. `scripts/integration-haops.sh`, with three servers and two workers and a 120 s window, counted 210 or 211 entries a minute on 2.3.0 and 15 after the upgrade, in each of four runs on the same machine.

With fewer entries, a snapshot (after 8192 entries) is hours apart rather than minutes, and a follower that was down for hours catches up from the log, the 10240 entries the leader keeps after its snapshot, rather than from a snapshot.

### What failover costs

The same as in 2.3.0. A new leader sets the start grace ([leader-only work](#leader-only-work)): every node whose lease was current when the leader changed waits for its first report to the new leader, and is not lost meanwhile. The new leader finds the stored leases at most 13 s old, so every node that was live is inside the grace's window, as in 2.3.0; what the old leader kept and had not written, at most 4 s of renewals, is lost and not needed. A node that died with the old leader has its replicas placed again when the grace ends, as in 2.3.0.

A leader of 2.3.0 writes every renewal, and one of 2.4.0 batches them, in the same statements: a cluster in the middle of its upgrade may change leaders between the two at any time.

### Testing

Unit tests: a renewal and an unchanged report kept in memory and written in the next batch; one written at once when the stored lease is older than 9 s, has lapsed, or the node's rows were deleted; a node that stops reporting lost 15 s after its last renewal; a removal while a renewal is kept; the batch written at a handover; a new leader's start grace with the leases as the batch left them; the agent's skipped renewal. `scripts/raft-rate.sh` takes the number of workers, and measures 2.3.0, built from the tag, against this build for three servers and seven workers; the rolling upgrade's script counts the entries a minute before and after the upgrade.

## Security

What changes:

- A new listener, the server port, on the cluster address of each server of a cluster with more than one, TLS 1.3 with client certificates of the server role only ([transport](#the-transport)).
- The CA's private key is on every server, not one. A server that is compromised can issue any certificate, as the one server could. Each server is as sensitive as the one server was.
- The store, with every app's environment values, the registry passwords and the routers' TLS keys, is on every server, and so is the Raft log, which holds the same values in its entries' statements. `raft/` is mode 0700, and `raft.db` and the snapshots 0600.
- The admin token's hash is on every server. The token itself stays where `init` wrote it.
- During a CA rotation, the new CA's key is in the store and in the Raft log's entries and snapshots, sealed under a key derived from the current CA's, which every server has in `ca.key` ([where the new CA is kept](#where-the-new-ca-is-kept)). A copy of the store or of `raft.db` without `ca.key` does not give it, and once `finish` has replaced `ca.key` on every server, the sealed copies left in older entries cannot be opened.
- The build request on the server port, which a server answers to any peer with the server role, whether it leads or not: its version, the schema versions it runs, its store's, and the fingerprints of its CA files and of its certificate's CA. None of it is secret.

What does not: the API still needs the admin token on every route but `GET /healthz`, checked by the leader; the node listener still needs a worker's certificate, checked by the follower at the handshake and by the leader again; the join listener still needs a token, redeemed by the leader.

The forwarding headers, the caller's address, whether it came over TLS, and a worker's certificate chain, are taken only by the leader's server port, on the forward protocol, from a peer whose certificate has the server role. The API and the node and join listeners delete them from every request they get, first, so an API client cannot claim another address to get past the login limit, and a worker cannot claim another node's certificate; a test of each sends forged headers and is served as itself. Firewalls: allow 4486 between the servers and from nowhere else, and 4484 and 4485 from every node to every server ([security](security.md#several-servers), [routing](routing.md#ports-and-firewall)).

## What is not done

- A server join during a CA rotation, which stays refused ([joins and backups during a rotation](#joins-and-backups-during-a-rotation)), and closing, at a rotation's end, the server port connections opened with a certificate from the CA it dropped.
- A downgrade once the store's schema has moved: the way back is a backup restored as a cluster of one ([the step](#the-step)).
- Reads on followers. Every request goes to the leader, so the leader carries the whole load of the API and the node API.
- Removing a dead server by itself. Raft keeps it in the configuration until `hivemind server remove`, and a cluster of 3 with one dead server tolerates no further loss.
- Sessions. A browser session lives in the leader's memory and ends when the leader changes; the CLI's token is not affected.
- Turning Raft off again. A cluster that drops back to one server keeps running Raft, with one voter.

## Limits

- 3 to 7 servers, 3 or 5 recommended.
- Every write waits for a majority of the servers to write the entry to their logs, and for the leader to apply it: a round trip on the network and a second fsync on each server, where one server has one fsync. All writes on the leader are serialised, as they are on the one server.
- Network partitions: the side with a majority elects a leader and goes on. A leader on the minority side steps down within 500 ms of losing the majority, and its API answers `503`. A worker on the minority side keeps its containers, and goes to a server on the majority side if it can reach one. When the partition heals, the minority's servers take the majority's log, and their nodes report again.

## Testing

Unit tests: the store's replication (a workload over every table applied on a leader, a follower and a store that applies the entries afterwards and again, compared row by row, and against a store without replication; refused writes on a follower; caches after a change of leader; snapshots), the log store, three servers on loopback (formation from a cluster of one, a leader stopped, two of three down, forwarding with forged headers), the cluster listeners (forwarding, forged headers on the node listener, server join refusals, the servers header), the node API client's failover, and the CLI.

`scripts/integration-ha.sh`, CI's `ha` step, runs three servers and two workers with routers in Docker, and checks formation by join, deploys through every server, the leader killed during writes, the old leader back, a partition that isolates the leader, quorum lost, and `server remove` of the first server, refused while its node runs a replica and taken after a drain. It then runs `scripts/integration-haops.sh`, which builds 2.3.0 from its tag and upgrades three servers from it one at a time, measures the Raft entries a minute before and after, and rotates the CA of the upgraded cluster with a server stopped during the rotation ([rolling upgrades](#testing), [the CA rotation](#testing-1), [fewer entries](#testing-2)). To read each server's own copy of the store, it uses a test-only setting, `HIVEMIND_TEST_LOCAL_READS=1`, which only a binary built with `-tags hivemind_testknobs` reads: a server then answers a GET that carries `Hivemind-Test-Local-Read: 1` from its own store. A release binary refuses to start with it set.

## Measurements

On one c8g.4xlarge (16 arm64 vCPUs, Amazon Linux 2023), with the three servers and two workers of `scripts/integration-ha.sh` as Docker-in-Docker containers on one bridge network, so with no real network between them:

| What | Run 1 | Run 2 |
|---|---|---|
| A new leader after the leader's container was killed, as the survivors' own Raft state shows it | 1,764 ms | 746 ms |
| A write through a follower's API taken after the kill | 1,796 ms | 780 ms |
| A new leader after the leader was cut off the network | 1,905 ms | 1,355 ms |
| Writes acknowledged before the kill / after it / refused or timed out; every acknowledged one in every server's store | 26 / 51 / 3 | 25 / 53 / 2 |
| Requests through the workers' routers during the failover, all 200 | 40 | 40 |

In run 1, the old leader's replicas ran on other nodes 34 s after the kill, with a 30 s start grace. Three `PUT /v1/apps` on a three-server cluster in an earlier run of the same script took 8.9, 8.9 and 9.1 ms on the leader, as its request log has them; a cluster of one was not timed in the same way, so the cost of the log over one server is not measured here. In the unit tests, three servers on loopback with timeouts cut to 300 ms elected a new leader 608 ms after the leader stopped.

The Raft log of a steady cluster, by `scripts/raft-rate.sh` on the same machine: three servers and two workers as Docker-in-Docker containers, 50 apps of 2 replicas that no node can take (64 CPUs each), as the eighth soak's write-load had, nothing written through the API, and the entries each server appended in 300 s after 60 s to settle, read from its `raft.db`:

| | Entries | Bytes | A minute |
|---|---|---|---|
| At 7e6a508, before a pending replica's reason was written only when it changed and statements that changed nothing were left out | 1,171 | 856,699 | 234 entries, 167 KiB |
| At 0d0a814, with both | 1,051 | 283,770 | 210 entries, 55 KiB |

The three servers appended the same entries, to within one. Most of what is left is the nodes' reports and lease renewals, each an entry that changes rows; the change took away two thirds of the bytes, the reasons written again and the statements that changed no row, and one entry in ten, the transactions that changed nothing at all.

The binary for linux/amd64, built with `-trimpath -ldflags "-s -w"`, is 21,549,216 bytes, 1,642,496 bytes (8.3%) more than at the commit this branch was cut from (19,906,720).
