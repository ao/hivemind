# Rotating the cluster CA

The cluster CA signs every node certificate, and every node trusts it: the server checks each worker's client certificate against it, and each worker checks the server's certificate against it. It lasts ten years from `init`. A rotation replaces it with a new CA, without joining any machine again and without stopping any container, in three steps the operator runs on the server:

```
hivemind ca rotate start     # make the new CA; every node learns to trust both
hivemind ca rotate renew     # every node gets a certificate from the new CA
hivemind ca rotate finish    # the old CA is trusted nowhere, and its key is deleted
```

`hivemind ca status` shows where a rotation is and which nodes it waits for. `hivemind ca rotate abort` undoes a rotation before `finish`. Every command needs the admin token and runs against the server's API (`GET /v1/ca`, `POST /v1/ca/rotation/{start,renew,finish,abort}`); a worker has no part in it but to follow what the server says.

With several servers ([several servers](ha.md)), every server follows the rotation: the leader runs the steps, records the new CA in the store with its key sealed, and every server writes its own copy of the new CA from that record, moves its own certificate with the listener CA and does its own file step at the end ([the CA rotation with several servers](ha.md#the-ca-rotation-with-several-servers)). It needs every server on 2.4.0 or later, with the store at schema version 19: `start` is refused before that, and while a server is a non-voter catching up. A server join is refused while a rotation is in progress. In 2.3.0 a rotation needs a cluster of one: remove all but one server with `hivemind server remove`, rotate, and join the others again.

Two reasons to rotate: the CA is in its last year (the server warns once a day then), or its key may have leaked. See [after a leak](#after-a-leak) for what a rotation does and does not do in the second case.

## Terms

The old CA is the one in `ca.crt` and `ca.key` when `start` runs. The new CA is the one `start` makes, in `ca-next.crt` and `ca-next.key`, in the server's data directory beside the old one, and in every server's with several, with the same modes (0644 and 0600). A CA is named by its fingerprint, the SHA-256 of its public key, as join tokens already name it.

Every node means every row of `hivemind nodes` but the server's own: each worker that has registered and was not removed, whether it is up or not. A live node is one whose lease is current.

The listener CA is the CA that signs the server's own certificate, which the node listener and the join listener present.

## Phases

The server keeps the rotation in its store (schema version 17): the fingerprints of the old and the new CA, from version 19 the new CA itself, its key sealed, the phase, the direction (towards the new CA, or back to the old one after an abort), and the listener CA. For each node it keeps three marks, reset when a phase changes direction: the node trusts the CA the rotation goes towards; the node has presented a certificate from it; the node's requests say nothing of what it trusts, so it is a build without rotation. Every step writes the store in one transaction, and every step that changes files records what it is about to do first, so a server that crashes, or is stopped, in any phase starts again in the same phase and finishes a step it was in.

### start

`start` makes a new ECDSA P-256 CA, valid for ten years, and records the rotation in the store, in phase `trust`, with the new CA's certificate and its key sealed under a key derived from the old CA's (schema version 19). Then the server, every server with several, writes `ca-next.key` and `ca-next.crt` from the record with `O_EXCL` and syncs them. The record first: a server that stops between the two writes the files at its next start. A rotation already in progress is refused. A store of 2.3.0's version 18 (a cluster of one that has Raft, for the seconds before its leader moves the schema) takes 2.3.0's order: the files first, then the record, and a crash between the two leaves files that `serve` removes at its next start, as `start` does before it makes its own.

In this phase the server:

- accepts client certificates from both CAs;
- keeps its own certificate from the old CA: no worker trusts the new one yet;
- issues every certificate, at join and at renewal, from the old CA, as before. (A renewal is never issued from an older CA than the certificate it replaces: a node that presents a certificate from the new CA, which happens in this phase only after a restore, renews under the new CA.);
- tells every worker, on every node API reply, that the CA set has changed (a header, `Hivemind-CA`, with a digest of it). A worker that sees a digest it has not applied asks for the set (`GET /node/v1/nodes/{id}/ca`), checks it, adds the new CA to its `ca.crt`, and from then on trusts both.

Every request from a worker of this build says which CAs it trusts (`Hivemind-CA-Trust`, their fingerprints). The server records, for each node, that it trusts the new CA once a request says so. A request a node sent before it took the new CA can come after a later one, as a long poll that fails over to another server does, so a node stops counting as trusting the new CA only after a second request without it, 2 s or more after the first, with none that lists it in between. A worker writes the new `ca.crt` (to `ca.crt.next`, synced, then renamed) before it sends the new set, so a node the server counts as trusting the new CA trusts it on disk, also after a restart. The header is the worker's own claim, which the server cannot check. A compromised worker that lies about it gains nothing: the server's certificate moves to the new CA sooner, and the only node that then fails its handshake is the one that lied.

`hivemind ca status` lists the nodes that do not trust the new CA yet. Once it lists none, run `renew`.

### renew

`renew` moves the rotation to phase `renew`. It refuses while a live node does not trust the new CA, and names each one, with why: it has not asked for the set yet, or it is a build without rotation. `--force` goes ahead anyway. A node that is down does not hold `renew` back: it learns the new CA when it comes back, and renews then.

In this phase the server:

- still accepts client certificates from both CAs;
- issues a renewal from the new CA to a node whose request says it trusts the new CA, and from the old CA to any other node, such as a build without rotation, which could not check a certificate from the new one;
- tells every worker, in the CA set, to renew now under the new CA. A worker that trusts the new CA and holds a certificate from the old one renews at once, with a new key, as at any renewal. It then presents the new certificate on new connections, and the server marks the node renewed when it sees it;
- refuses, with 403, a certificate from the old CA for a node it has seen with one from the new CA. A worker that has renewed sends nothing with its old certificate after that; a certificate from the old CA for that node is someone else's ([after a leak](#after-a-leak));
- renews its own certificate, the listener CA's, last: once every node trusts the new CA, it issues itself a certificate from the new CA and records the listener CA in the store. Both listeners present it from their next handshake. Every node, not every live one: a node that does not trust the new CA cannot finish a TLS handshake with a server certificate from it, so it could not come back even to learn the CA. Switching early would cut such a node off before `finish`, and `finish` is the one step meant to cut nodes off, and only knowingly. A node that is down and never comes back holds the switch until it is removed, or until `finish --force`.

`hivemind ca status` lists the nodes that have not renewed, and says whether the server's certificate is from the new CA yet. Once it lists none, run `finish`.

### finish

`finish` refuses while any node has not renewed, and names each one, with why: down since some time, a build without rotation, does not trust the new CA yet, or has not renewed under it yet. In a rotation an abort turned round, the reasons speak of the old CA it goes back to, and of requests since the rotation turned round. `--force` cuts those nodes off: they can no longer reach the server, and each must join again ([offline nodes](#nodes-that-are-offline)).

`finish` then, in this order:

1. issues the server's certificate from the new CA, if the listener CA was still the old one (only with `--force`);
2. records in the store that the rotation is ending towards the new CA, and from then on accepts client certificates from the new CA only. The CA set the workers get has the new CA only, and each worker removes the old CA from its `ca.crt`;
3. renames `ca-next.key` over `ca.key`, then `ca-next.crt` over `ca.crt`, and syncs the directory. The old key's file is gone: renaming over it unlinks it. Most filesystems do not overwrite its blocks, and backups taken before `finish` hold it ([backup](#backup-and-restore));
4. deletes the rotation from the store, and records the new CA's fingerprint as the cluster CA's, in one transaction. With several servers, every server does 2 and 3 for itself as it follows the record, and one that finds the rotation deleted before it did renames its `ca-next.*` by that fingerprint.

A server that stops between 2 and 4 does the rest of 3 and 4 when it starts: a `ca-next.key` that is still there is renamed over `ca.key`, a `ca-next.crt` over `ca.crt`, and the record is deleted once `ca.crt` has the new CA's fingerprint and `ca.key` matches it. With several servers, a leader that stops there leaves the rest to the next leader.

A connection opened before `finish` with a certificate from the old CA was checked when it opened. The server checks again, on every request, that the client certificate is from a CA it trusts now, as it checks the certificate's expiry on every request today, so such a connection is refused at its next request.

### abort

`abort` undoes a rotation that has not finished.

- In phase `trust`, no certificate from the new CA was issued (unless the server was restored from a backup taken in phase `trust` after nodes had renewed, and a node has presented a certificate from the new CA since; then `abort` does what it does in phase `renew`): the server records that the rotation is ending towards the old CA, accepts client certificates from the old CA only, deletes `ca-next.key` and `ca-next.crt`, and deletes the rotation. The workers remove the new CA from their `ca.crt` at their next request. A server that stops in the middle deletes what is left when it starts.
- In phase `renew`, some nodes may hold certificates from the new CA, and the server's own may be from it, so the new CA cannot be dropped at once: those nodes would be cut off. `abort` turns the rotation round instead: it goes back towards the old CA, in phase `renew`, with the marks of every node reset. Every node still trusts the old CA (a worker removes a CA only when the server's set no longer has it, which happens at `finish`), apart from a node that joined after the server's certificate moved to the new CA, which learns the old CA from the set the way the others learnt the new one. Nodes renew back under the old CA, the server's certificate goes back to the old CA once every node trusts it, and `finish` ends it, with the same checks, deleting the new CA. A rotation that is going back cannot be aborted again; finish it, then start a new one. `hivemind ca status` names the CA the rotation goes to, by file and fingerprint (`back towards the old CA, ca.crt 50f80d35, after an abort`), and its `TRUSTS TARGET` and `CERTIFICATE FROM` columns read against that CA; the server's log lines and the workers' name it the same way. When an abort ends the rotation, at once in phase `trust` or at the `finish` after a turn, the cluster is back on the CA it had before `start`: the backups taken during the rotation hold the new CA's key, and both steps say to delete them; those from before `start` are still good.

### What each side trusts

For a rotation from the old CA A to the new CA B:

| Phase | Server accepts client certificates from | Server's own certificate from | Certificates issued from | CA set sent to workers | A worker trusts |
|---|---|---|---|---|---|
| no rotation | A | A | A | A | A |
| trust | A, B | A | A | A, B | A, then A and B once it has the set |
| renew | A, B | A, then B once every node trusts B | B for a node that trusts B, A for any other; at join, the server's own CA | A, B, renew under B | A and B |
| after finish | B | B | B | B | B |

After an abort in phase `renew`, the table holds with A and B swapped, until `finish`.

### The join listener and join tokens

The join listener presents the server's certificate, so it moves to the new CA when the server's certificate does. A joining node pins the CA its token names, and checks that the server's certificate chains to it, so a join token names the listener CA when it is issued: `hivemind join-token` gives a token for the old CA until the server's certificate moves, and for the new CA after. A token issued before the move stops working at the move; the joining node says that its token names another CA than the server presents, and that a token issued before a rotation of the CA must be replaced. The server takes a token that names either CA; the joining node's pin is what decides.

A node that joins during a rotation gets its certificate from the CA its token names, which is the CA it pinned, and `ca.crt` with that CA, as at any join. Its first node API reply then tells it the CA set, and it follows the rotation from there like any other node. The join reply's format does not change, so a joining node of an earlier build still joins.

### How a worker gets the new CA

Over the node API, from the route `GET /node/v1/nodes/{id}/ca`, behind the same client certificate, denylist and epoch checks as every other node route. The reply has each CA's certificate, and for a CA the worker may not trust yet, a cross-certificate: the new CA's name and public key, signed with the key of the CA the rotation comes from. A worker adds a CA to its `ca.crt` only when both hold:

- the reply came over a connection whose server certificate the worker checked against a CA it already trusts, for the server's name, as for every node API request; and
- the CA's cross-certificate has the CA's own name and public key, and its signature checks against a CA the worker already trusts.

The first alone would do while only the server can answer on that connection. The second makes trust in a new CA depend on the old CA's key itself, which only the server's data directory holds, and not on whatever produced the reply. The server's own TLS key is a different key, made at each start and held in memory: with it, someone can pose as the server to the workers until that certificate expires, but cannot make a worker trust a CA of their own for good. A CA in the reply that the worker cannot check that way is not added. A worker that joined after the server's certificate moved to the new CA has no use for the old one, and does not add it.

The worker also checks the reply against what it holds: it never removes the CA that signed its own certificate, nor the CA that the reply's own connection was checked against, and never keeps an empty set. A worker keeps its connection to the server open as long as it calls, so at `finish` its connection may still be the one it opened while the server's certificate was from the old CA; a set without that CA is then read again on a new connection, before it is refused. It logs each CA it adds or removes, with its fingerprint, and the fingerprints can be compared with `hivemind ca status` on the server.

`ca.crt` holds every CA a worker trusts, one PEM certificate each, the CA of its own certificate first. A worker changes it to `ca.crt.next`, syncs it, then renames it over `ca.crt`, and removes a stale `ca.crt.next` at start. After a change it opens new connections to the server, with the new set, as it does after a renewal. `hivemind node set-server`, on a worker, checks the new address against every CA in it.

### The CLI and a TLS API

The API's TLS certificate is the operator's (`--api-tls-cert`), and the CLI checks it against the system's roots or the file in `--ca-cert`. The cluster CA has no part in it, and a rotation changes nothing there.

## Nodes that are offline

A node that is down during a rotation holds back the server's certificate (in phase `renew`) and `finish`, and `hivemind ca status` names it with the time of its last lease. If it comes back before `finish`, it follows the rotation from where it is: it gets the CA set, adds the new CA, and renews.

`finish` refuses while such a node has not renewed, and names it. `finish --force` goes ahead, and the node is cut off: its certificate is from the old CA, which the server no longer accepts, and the server's certificate is from the new CA, which the node does not trust. Coming back, it fails its TLS handshake with the server, and each failed attempt in its log says that the server's certificate is from a CA it does not trust, that the address is another cluster's server or the cluster's CA was rotated without it, and what to do for the second: run `hivemind node remove <id>` on the server, then join the machine again with an empty data directory and a new join token. It does not stop its containers, as a removed node does not; the server has placed its replicas elsewhere once its lease lapsed.

The server knows only which nodes it has not seen present a certificate from the new CA. After a restore of a backup taken before the nodes renewed, its marks are older than the nodes, and a node it lists may hold a certificate from the new CA already. So `finish --force` says it went ahead without those nodes, that one whose certificate is from the old CA is cut off and one that renewed unseen comes back by itself, and, 30 s later, logs which of them have reached the server since (`these have reached the server since ... need nothing`) and which have not (`these have not reached the server in the 30 s since`, at warn, with what to do). A node that is down is in the second list until it is back; its own log says whether it was cut off. Join a node again only once it is in the second list and its log says so.

A node cut off this way tries the server every few seconds, and the server names it at warn, once a minute: a node whose certificate is from a CA the cluster no longer trusts by the node ID in that certificate, which the line says is not verified, and the CA it presented with it (`a node presented a certificate from a CA this cluster does not trust, and was refused ... node=node-3c5d8032c4c8 node_id_verified=false ca=83d3dd58...`); a node that refuses the server's certificate, as one that trusts the old CA alone does, by the address it registered, since a node sends its own certificate only once it accepts the server's (`a node refused this server's certificate ... node=node-3c5d8032c4c8`). Both say what to do. The certificate is still refused. Other failed handshakes on the node listener are logged at info, as `TLS handshake error on the node listener`, with the address and the error.

## Workers of an earlier build

A worker of a build without rotation sends no `Hivemind-CA-Trust` and ignores `Hivemind-CA`. It trusts the old CA only, and keeps working through `start` and `renew` as before: the server's certificate stays from the old CA while it is in the cluster, and its renewals, when they fall due, are issued from the old CA. The server marks it as a build without rotation at its first request after `start`, logs that once, and `hivemind ca status` and `hivemind status` name it. It holds the server's certificate and `finish` back. Upgrade it, and it follows the rotation from where it is, without joining again; or remove it; or `finish --force`, which cuts it off, after which it fails every handshake and must join again with an upgraded binary.

`scripts/integration-mixed.sh` runs this with a 2.0.0 worker in Docker. After `start`, `hivemind ca status` shows it as `no (old build)`, and it and `hivemind status` name it as what `renew` waits for; `renew` refuses, and `renew --force` goes ahead. The 2.0.0 worker keeps the old CA alone in its `ca.crt`, stays `ready`, and runs a replica placed on it in the renew phase, while the server's certificate stays on the old CA; `finish` refuses, naming it. Then the worker's binary is replaced and it starts again on its data directory: it trusts the new CA, renews under it, the server's certificate moves, and `finish` passes, with `node.json`, its node ID and its containers unchanged.

## Backup and restore

A backup holds `ca-next.key` and `ca-next.crt` too while a rotation runs, beside `ca.key` and `ca.crt`, so a backup taken during a rotation restores a server in the same phase, with both keys. A build without rotation refuses such a backup, as it refuses any backup with a file it does not know. A backup taken outside a rotation has the same files as before.

`backup` copies the store first and then the CA files. A step of a rotation that ran in between would leave them out of step, so backup checks them against the rotation the copied store records, and fails, asking to run it again, when they differ. Through the API, the server makes a backup and a rotation step one after the other, never at once. `restore` checks the same before it keeps anything, and says when the backup was taken during a rotation, in which phase.

What a restore brings back:

- A backup from during a rotation, restored while the cluster is still in it: the server has both CAs, accepts both, and learns from each node's requests what it trusts and holds now, so the marks the backup had are corrected as the nodes come back. Run the step the cluster was at again: `renew`, if the backup was from phase `trust` and nodes had renewed since. If the cluster had finished, and the restored server's certificate is from the old CA, the workers, which trust the new CA only, fail their handshakes; `hivemind ca rotate finish --force` on the restored server moves its certificate to the new CA, and they come back. A backup from phase `trust` needs `hivemind ca rotate renew` first: `finish`, with `--force` too, refuses that phase with `it is in phase trust, and no node has renewed: run hivemind ca rotate renew first`. `renew` goes ahead without `--force`, since the workers cannot reach the server and so are not live; `hivemind ca status` names them as down.
- A backup from before `start`, restored during a rotation, has no new CA. Nodes that still hold certificates from the old CA come back, and remove the new CA from their `ca.crt`, as after an abort. Nodes that had renewed hold a certificate from a CA the restored server does not have, and must join again. So take a backup after `start`: `start` says so.
- A backup from before `finish`, restored after it, has the old CA, which the workers no longer trust, as its current CA. If its record still has the rotation, `finish` (with `--force` if its certificate was from the old CA, and after `renew` if the record is in phase `trust`) completes it again, as above. On three EC2 machines, a backup from phase `trust` restored after `finish`: the workers failed their handshakes from the restored server's start, `renew` and then `finish --force` moved its certificate, and the two workers reached it again 0.4 and 0.7 s later, with their node IDs, certificates and containers. At `finish --force` the server names them as nodes it has not seen present a certificate from the new CA, and says that such a node comes back by itself if it renewed unseen; 30 s later it logs that they have reached it, and need nothing ([offline nodes](#nodes-that-are-offline)). Take a backup after `finish`; `finish` says so, and says that older backups hold the old key.

The epoch (`cluster.epoch`, [backup](backup.md#if-the-old-server-comes-back)) does not change in a rotation, and a restore raises it in any phase, as before. The two guard against different things and do not interfere: the epoch keeps workers off a server from before a restore, whatever its CA; the CA keeps them off anything that cannot prove it holds a CA they trust. A server from before a restore that comes back after the restored server finished a rotation fails the workers' handshakes, before its epoch is read.

The denylist is by node ID, and node IDs do not change in a rotation, so a removed node stays refused whichever CA its certificate is from. A node removed during a rotation drops out of every list the rotation waits on. Restoring a backup brings back the denylist and the rotation as they were; remove again any node removed after it.

## The CA's last year

In the CA's last year, node certificates are issued to end when the CA does, and the server logs, once a day, that the CA expires and how long is left, and now: run `hivemind ca rotate start`. While a rotation runs, the warning names its phase and the command for the next step. Certificates from the new CA are not cut short: it has ten years. `hivemind ca status` shows each CA's expiry.

## After a leak

Someone with the old CA's key can make a certificate for any node ID and any server name. A rotation ends that at `finish`: the old CA is then trusted nowhere. Until then, it does not:

- During the rotation, such a certificate for a node in the cluster is taken for that node, and can renew under the new CA like the node itself. The server refuses a certificate from the old CA for a node once it has seen that node with one from the new CA, so this works only for a node that has not renewed yet, and the real node's renewal, if it comes second, is refused and shows up in `hivemind ca status` as not renewed. When in doubt about a node, remove it, which no certificate gets past, and join the machine again.
- Someone with the old key and a place on the network between a worker and the server can pose as the server to that worker, as they could before the rotation, and give it a CA of their own, cross-signed with the old key. The worker logs the fingerprint of every CA it adds: compare them with `hivemind ca status` on the server.
- Backups taken before `finish` hold the old key. Delete them once the rotation has finished.

So after a leak, run the rotation through without waiting, check the fingerprints, and remove any node whose state in `hivemind ca status` is not what it should be.

## Commands

| Command | Route | Does |
|---|---|---|
| `hivemind ca status [--json]` | `GET /v1/ca` | Each CA's fingerprint, start and expiry; the phase and its direction; the listener CA; for each node, whether it is live, what it trusts, which CA its certificate is from, and whether it is a build without rotation; the nodes the next step waits for, and why |
| `hivemind ca rotate start` | `POST /v1/ca/rotation/start` | Makes the new CA; phase `trust` |
| `hivemind ca rotate renew [--force]` | `POST /v1/ca/rotation/renew` | Phase `renew`; refuses while a live node does not trust the new CA |
| `hivemind ca rotate finish [--force]` | `POST /v1/ca/rotation/finish` | Ends the rotation; refuses while a node has not renewed |
| `hivemind ca rotate abort` | `POST /v1/ca/rotation/abort` | Ends a rotation in phase `trust`; turns one in phase `renew` back towards the old CA |

They need the admin token, like every API route, and the server: a rotation runs in the `serve` process, which holds the data directory's lock, and a server without `--advertise` rotates too, with no nodes to wait for. Each step's reply is the status after it, and a refusal names the nodes it waits for.
