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
- The CA rotation manager reads the rotation from the store at start. With more than one server, there is no rotation ([compatibility](#compatibility)), so it does not change.
- The routers' counters, which workers post every few seconds for `/metrics`, and how each router reads the routing table, are held by the server that takes the posts, the leader. A new leader has them again after the workers' next posts.

## Leader-only work

Only the leader runs:

- the sweep: lost nodes, pending replicas, expired join tokens;
- rollouts and drains, which the sweep and the agent's passes advance;
- the start grace;
- the CA rotation manager's run, and the steps of a rotation;
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

The CLI prints these as they are. Given a list of servers, it tries the next one on a marked `503`, and on a server it cannot connect to; on one whose connection broke after the request was sent, it tries the next only for a `GET` or a `HEAD`, since the server may have acted on a write, and says so. When a server gave a marked `503`, it tries the servers again every 500 ms for up to 3 s after the first one, which covers an election (1 to 2.5 s in the eighth soak), and then prints the last answer that no server leads, rather than the error of a server that did not answer. In the ninth soak, before this, with no leader and two servers whose processes were stopped, so that their machines took the connection and nothing answered the handshake, each of them cost a call 5 s, and the CLI printed the second one's `TLS handshake timeout`. In the eighth soak, before this, a read during the rolling update with the leader's restart failed with `the leader ... did not answer this server`: the stopping leader refused its connection, and each follower passed it to that leader, which was still the leader they knew.

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
hivemind join 10.0.0.5 --role server --token <token> --advertise 10.0.0.6
```

`join --role server` joins the cluster, and runs the new server in the foreground, as `join` runs a worker; `hivemind serve --advertise 10.0.0.6` runs it again after a stop. A server token and a worker token are not interchangeable: each is refused where the other is expected, with the same message as an unknown token. (The flag is `--role server`, as on `join`, not `--server`: every command that calls the API already has `--server`, the API's URL.)

What a joining server receives, in the join reply: its node ID, chosen by the leader, the CA certificate and the CA's private key, and the list of servers. It needs the key to issue its own listener certificate, and, once it is the leader, the workers' certificates. The reply goes over the join listener's TLS 1.3 connection, whose CA the joining machine has pinned by the fingerprint in the token, before it sends the token: the check a worker's join makes. The token is single use, expires, and is stored as a hash; the join listener's limit on failed joins applies. The key is written with mode 0600 in the 0700 data directory. The admin token is not sent: its SHA-256 is in the store, and the store comes by Raft snapshot over the server port. The joining server writes no `admin.token`; give its CLI the token with `HIVEMIND_TOKEN`.

The first server join turns Raft on, on the server the join reaches (or the leader it passes it to, which with one server is itself):

1. It bootstraps a Raft cluster with itself as the only voter, and opens the server port.
2. It hands raft the store as it is now as the log's first snapshot (`raft.Restore`), so a server that joins later gets that snapshot and then the entries after it.
3. From then on, writes go through the log.

Then, for every server join:

4. The leader adds the new server as a non-voter. A non-voter gets the log but does not count for a majority, so a server that joins and never comes up does not stop the cluster's writes.
5. The new server starts, receives the snapshot and the entries since, and once it has applied every entry the leader has committed, asks the leader, over the server port, to make it a voter.
6. The leader adds it as a voter.

`hivemind servers` lists the servers: ID, name, advertise address, Raft role (leader, follower, or non-voter while catching up), and the last time the leader heard from each. Without a leader, the server asked answers it itself, from its own copy of the configuration, and says there is no leader.

`hivemind server remove <id>` removes a server from the Raft configuration and the server table. It refuses to remove the last voter, and refuses a removal that would leave fewer servers that the leader heard from in the last 10 s than a majority of those that stay, unless given `--force`. The removed server's serve stops within about 5 s, once it has applied the deletion of its row from the server table (Raft sends a follower nothing once the change that removes it is in the leader's log, so the table, which `server remove` changes first, is how it learns), with exit status 3; its containers keep running, and its node is a node like any other: drain it and remove it with `hivemind node drain` and `hivemind node remove`. Its certificate is refused on the server port from then on. To use the machine as a server again, give it an empty data directory and a new server token.

How many servers: 3 or 5. With 2, both are needed for a write, so losing either stops writes: no better than one, and worse, since two machines can fail. `join-token --role server` warns when the cluster would have 2. Joins past 7 servers are refused: each write waits for a majority, and more servers make that slower without a need for it in a cluster of ten machines.

When a majority of the servers is down, or cut off from the rest, there is no leader: no writes, no placement, no rescheduling. The containers keep running, every worker keeps its own, and every router keeps serving with the routing table it last read. The API answers `503` with the reason ([clients](#the-api)). When enough servers are back, they elect a leader, which starts with the start grace.

## Compatibility

A cluster of one upgraded from 2.2.0 changes nothing until a second server joins: no Raft, no new listener, and writes go to the store as before. The store moves to schema version 18, which adds the applied index, the server table and a role for join tokens; it adds tables and a column only.

A worker of 2.2.0 or older knows one server, the one it joined. It goes on working through that server, which passes its requests to the leader, so a second server is not refused for its sake. It cannot fail over: if its server is down, it waits, as it does today. Upgrade the workers to have them fail over.

All servers run the same release. A server will not apply an entry from a leader at another schema version, and a joining server must run the leader's version. To upgrade the servers, stop them all, upgrade them, and start them again; their containers keep running meanwhile. A rolling upgrade of the servers is not done.

Backup: `hivemind backup` on any server copies that server's store and CA files, as on the one server, and `hivemind backup --api` is passed to the leader like any API request. Raft's directory and `server.json` are not in it. Restore makes a cluster of one: the restored data directory has no Raft, restore empties the store's server table and its applied index, and the epoch is one higher than the backup's, so every worker refuses the old servers' copies. The other servers join again, each with an empty data directory and a new server token, and their old node rows are removed with `hivemind node remove` as for any lost node. `serve` refuses a store that lists servers in a data directory without Raft state, as when `raft/` was deleted: serving it would start a second cluster beside the others ([backup](backup.md#a-cluster-of-several-servers)).

The CA rotation needs a cluster of one in 2.3.0: `hivemind ca rotate start` is refused while the cluster has more than one server, and a server join is refused while a rotation is in progress. Each server keeps the CA files in its own data directory, and a rotation changes them on one machine; carrying the rotation's files and steps to every server is not done. To rotate with several servers, remove all but one, rotate, and join the others again.

## Security

What changes:

- A new listener, the server port, on the cluster address of each server of a cluster with more than one, TLS 1.3 with client certificates of the server role only ([transport](#the-transport)).
- The CA's private key is on every server, not one. A server that is compromised can issue any certificate, as the one server could. Each server is as sensitive as the one server was.
- The store, with every app's environment values, the registry passwords and the routers' TLS keys, is on every server, and so is the Raft log, which holds the same values in its entries' statements. `raft/` is mode 0700, and `raft.db` and the snapshots 0600.
- The admin token's hash is on every server. The token itself stays where `init` wrote it.

What does not: the API still needs the admin token on every route but `GET /healthz`, checked by the leader; the node listener still needs a worker's certificate, checked by the follower at the handshake and by the leader again; the join listener still needs a token, redeemed by the leader.

The forwarding headers, the caller's address, whether it came over TLS, and a worker's certificate chain, are taken only by the leader's server port, on the forward protocol, from a peer whose certificate has the server role. The API and the node and join listeners delete them from every request they get, first, so an API client cannot claim another address to get past the login limit, and a worker cannot claim another node's certificate; a test of each sends forged headers and is served as itself. Firewalls: allow 4486 between the servers and from nowhere else, and 4484 and 4485 from every node to every server ([security](security.md#several-servers), [routing](routing.md#ports-and-firewall)).

## What is not done

- Rolling upgrades of the servers.
- The CA rotation with more than one server.
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

`scripts/integration-ha.sh`, CI's `ha` step, runs three servers and two workers with routers in Docker, and checks formation by join, deploys through every server, the leader killed during writes, the old leader back, a partition that isolates the leader, quorum lost, and `server remove` of a follower. To read each server's own copy of the store, it uses a test-only setting, `HIVEMIND_TEST_LOCAL_READS=1`, which only a binary built with `-tags hivemind_testknobs` reads: a server then answers a GET that carries `Hivemind-Test-Local-Read: 1` from its own store. A release binary refuses to start with it set.

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
