# Security

This lists every listener the code opens, what each one exposes, and the files hivemind writes. The claims come from the code in `cmd/hivemind`, `internal/agent`, `internal/auth`, `internal/cluster`, `internal/pki`, `internal/router`, `internal/runtime/docker`, `internal/tlsstore` and `internal/store`. The concepts are in [concepts](concepts.md).

## Listeners

| Listener | Default bind | Auth | Exposes |
|---|---|---|---|
| API | `127.0.0.1:4483` (`--api-addr`) | Bearer admin token, compared in constant time, or a session cookie that the token bought at `POST /v1/session`. A cookie request to `POST`, `PUT`, `PATCH` or `DELETE` also needs the session's CSRF token. `GET /healthz` needs no token and returns only `{"status":"ok"}` | Apps and their specs, including environment values. Status. Logs of every node's containers. Join token issuance. Nodes, and their removal. Registry credentials: storing and removing them, and their hosts and usernames; no route returns a password. Metrics. A backup of the data directory, with the CA private key and the admin token, sent only over TLS or to a loopback client |
| Metrics | Off. `--metrics-addr` starts it, and it must be loopback | The admin token | Prometheus metrics only |
| Node listener | Off. Starts with `--advertise`, on one address: `--cluster-addr`, or the address `--advertise` names or resolves to on this machine. Port 4484 (`--cluster-port`) | TLS 1.3, and a client certificate that chains to the cluster CA, names a node that was not removed, and is valid by the server's clock at each request. The node ID must match the one in the path | To a node: its own assignments, which include each replica's full spec and environment values, and the registry credential for each replica's image, if one is stored and the node asks for it. It can also report its own containers, register its own name, capacity and private address, get a new certificate for its own ID, take the log requests queued for it, and send the logs it was asked for. For its router: the whole routing table, and read and write access to the shared TLS storage, which holds every routed host's certificate and private key and the ACME account key |
| Join listener | Off. Starts with `--advertise`, on the node listener's address, port 4485 | TLS 1.3 with the server certificate. No client certificate. The join token is the credential. Only `POST /join` is routed | The cluster CA certificate, and a node certificate for each valid token |
| Router, http | Off. Starts with `--route-addr`, on any node, port 80 (`--route-http-port`). It may be `0.0.0.0` | None. It serves the apps' public traffic | ACME HTTP-01 answers for the routed hosts, a `308` to https for every other request to one, and `404` for any other host |
| Router, https | Off. Starts with `--route-addr`, port 443 (`--route-https-port`). It may be `0.0.0.0` | None of its own: whatever each app does. TLS 1.2 or later, with a certificate from the ACME CA for each routed host | Every routed app, to anyone who can reach the port. A host not in the routing table gets no certificate, and a TLS handshake for it fails |

The API, metrics, node and join listeners bind to the address they are given, and refuse a wildcard address at startup, both in config validation and when the socket is bound. The router is the one listener that may bind `0.0.0.0` or `::`: it is meant to be public, and `--route-addr` has no default. A worker opens no listener but the router, when it has `--route-addr`. See [routing](#routing).

### Logs from workers

The worker has no listener, so the server cannot connect to it to ask for a log. The worker asks instead. It keeps one request open on the node listener, `GET /node/v1/nodes/{id}/logs/requests`, and the server answers it, within 25 seconds, with a log request or with nothing. When `hivemind logs` names a replica on that worker, the server queues a request with a random 128-bit ID for the replica's node. The worker takes it, reads the container's log from its own Docker daemon, and sends it back as the body of `POST /node/v1/nodes/{id}/logs/streams/{request}`. The server copies the body to the user's response as it arrives, so nothing waits in memory for the whole log.

Both routes are on the node listener, behind its client certificate and the denylist, like every other node route, and the node ID in the path must be the one in the certificate. A node gets only the requests queued for its own ID. A stream for a request queued for another node gets `403`, and leaves that request as it was. A request ID that is not waiting for a stream gets `404`, so an ID that timed out, or one already used, cannot be replayed. Each node can have 16 log requests queued or streaming and 4 polls open at once. Past either cap, the API or the node gets `429`.

Kubernetes and Nomad read logs the other way round. The kubelet serves logs on its own HTTPS port, and the API server connects to it. A Nomad client serves its own HTTP API, and a Nomad server that cannot reach a client forwards the request over the connection the client already holds to it. Both need a listener on the node, or a multiplexed connection. Here the worker dials every connection, each is an ordinary HTTPS request with a fixed timeout or a fixed end, and the worker still listens on nothing.

The API TLS flags apply to the API only. The CLI checks a TLS API's certificate against the system's roots, or against the PEM file in `--ca-cert`, `HIVEMIND_CA_CERT` or the client config's `ca-cert`, and checks that it names the host in the server URL. It has no option to skip the check. Its minimum is TLS 1.2. See [the API](api.md#tls).

The session cookie is `HttpOnly` and `SameSite=Strict`, and `Secure` when the API is served over TLS. Sessions are held in the server's memory, keyed by the SHA-256 of the session ID, and last 12 hours. Failed logins are limited per source address (5 in 10 minutes), and the limited address gets `429` at `POST /v1/session`.

Three more things reach the network from containers, not from hivemind's listeners. A container's published ports are bound to the address in its spec, `127.0.0.1` by default, so an app that publishes `0.0.0.0` is reachable from the network by its own choice; the other containers on its node reach its ports whatever the address ([apps on one node](#apps-on-one-node)). A routed app's route ports are published on ephemeral ports of its node's private address, so every machine that can reach that address can reach the app without going through a router; see [routing](#routing) for the firewall rule. And the Docker Engine socket (`--docker-host`, `unix:///var/run/docker.sock` by default) is local. Anything that can write to that socket controls the host, and hivemind needs that access.

The join listener is open to the network whenever the server runs with `--advertise`. Its only protection is the token. Tokens expire, and failed joins from one address are limited (5 in 10 minutes).

## Files

The server's data directory is created with mode 0700.

| File | Mode | Holds |
|---|---|---|
| `hivemind.db`, with the WAL files beside it | 0600 | Apps and their specs, which include environment values. Replicas, nodes and their leases. The IDs of removed nodes. Node certificate expiry times. Join tokens as SHA-256 hashes. The admin token as a SHA-256 hash. The routers' TLS storage: each routed host's certificate and private key, the ACME account and its private key, and pending challenge tokens. Registry credentials: each registry's host, username and password, the password in plain text like environment values |
| `admin.token` | 0600 | The admin token, in plain text. This is the only copy |
| `ca.key` | 0600 | The cluster CA's private key. It can sign any node certificate |
| `ca.crt` | 0644 | The cluster CA certificate. Public |
| `hivemind.yaml` | 0600 when `init` writes it; as you create it otherwise | Optional settings for `serve`. `init` writes the serve flags it was given here; nothing else writes it. A `token` key in it is refused |
| `hivemind.lock` | 0600 | The pid of the `serve` that holds it locked |

The CLI config that `init` writes is `~/.config/hivemind/config.yaml`, with mode 0600, in a directory that is 0700 when it is created. It names the server and the token file, and with `--api-tls-cert`, the API certificate as `ca-cert`. It does not hold the token.

A worker's data directory is created with mode 0700.

| File | Mode | Holds |
|---|---|---|
| `node.key` | 0600 | The worker's private key |
| `node.crt` | 0644 at join, 0600 after a renewal | The worker's certificate. Public |
| `ca.crt` | 0644 | The cluster CA certificate. Public |
| `node.json` | 0600 | The node ID, the server's address and the join time |
| `node.key.next`, `node.crt.next` | 0600 | Only during a renewal: the new pair, before it is renamed into place |
| `hivemind.lock` | 0600 | The pid of the `join` or `serve --role worker` that holds it locked |

A worker has no CA key, no database and no admin token.

A backup file (`hivemind backup`) is written with mode 0600. It holds `hivemind.db`, `ca.key`, `ca.crt` and `admin.token`, so it grants everything the server's data directory does. A restored data directory is 0700, with every file 0600, `ca.crt` included. See [backup](backup.md).

## What a compromised worker can do

Assume an attacker has the worker's data directory, or root on the worker. The code allows the following.

- Read the assignment of every replica placed on the worker. That is the full spec, including environment values and volume names, and the registry credential for the replica's image. Treat every environment value as exposed to each machine that runs the app, and every registry credential as exposed to each machine that runs an image from that registry.
- Report false container states for its own node. The server takes the report as given, and the scheduler and rollouts act on it. The report's text, a container's state and error among it, is stored with each control character made a space, so it cannot carry terminal escapes to `hivemind status`; the CLI also escapes, as `\x1b`, any control character in the server's text it prints in `status`, `nodes`, `node drain` and error messages. `hivemind logs` copies a container's output as it is.
- Renew or stop renewing its own lease. Stopping makes the server move its replicas.
- Register any name of up to 253 printable characters, and any capacity up to the limits the server checks: 1,048,576 CPUs and 2^50 bytes of memory. Placement trusts these figures, so the worker can attract more replicas, and so more environment values and registry credentials, than it has room for.
- Run whatever its Docker daemon will run, which is the worker's own host.
- Renew its own certificate, with a key of its choice, for as long as it is not removed. Removal stops that: the renewal route refuses a removed node like every other route.
- Send any text as the log of a replica placed on it, or refuse to send it. Logs from a worker are what the worker says they are.
- Hold 4 log polls and 16 log streams open on the server.
- Read the TLS private key of every routed host and the ACME account key, through the TLS storage routes, whether or not the worker runs a router. With them it can pose as any routed host to a client that reaches it, and act as the cluster's ACME account, until the certificates are replaced and the account key is changed. This is in addition to the environment values it already sees.
- Write and delete keys in the TLS storage, within certmagic's namespace and the limits: 128 KiB a value and 10,000 keys. It can replace a host's certificate and key, which every router then serves, delete them so the routers issue again, fill the storage, or hold the issuing locks, 30 seconds at a time for as long as it renews them, so no router issues. It can hold at most 1,000 locks at once, and every expired lock is deleted whenever a lock is taken, so lock names it uses once do not fill the database.
- Register any private address that is an IP address and is not loopback, a wildcard or multicast, and report any ports for its own containers. The routers then send the traffic of the routed replicas placed on that worker to that address and those ports, which can be another machine on the routers' network.
- Read the whole routing table: every routed host, and the address and port of each of its replicas.

A worker's router needs nothing more than this: the routes above are what it uses.

The worker cannot do the following.

- Call the node routes for another node. The node ID in the path must match the one in its certificate, or the server answers 403.
- Obtain a certificate for another node or for the server. It has no CA key, and the server ignores names and extensions in a certificate request. A renewal is issued for the ID in the certificate the worker presents, and the route checks that the ID in its path is that one.
- Join another machine, or pose as the server. Its certificate names no host, so a machine that checks the server's name rejects it. It cannot issue join tokens.
- Reach the API or the admin token, the CA key, or the database, apart from the TLS storage.
- Read the status or logs of replicas on other machines, or take or answer the log requests queued for another node.
- Read the credential of a registry that no replica placed on it pulls from.
- Read or write anything in the TLS storage outside certmagic's namespace (`acme/`, `certificates/`, `ocsp/` and two single keys), or renew or release a lock another node holds: the server keeps each lock under the ID of the node that took it.
- Add endpoints for replicas on other nodes: an endpoint's address is the one its node registered.

## Routing

The router ([routing](routing.md)) changes what is exposed in three ways. The trust model of the CA and the node API does not change: the router adds node routes behind the same listener, certificate checks, denylist and node ID check as every other node route, and it adds nothing to the API or the join listener.

- The router's two ports are public by design, on every node started with `--route-addr`. They have no authentication. They answer only for the hosts in the routing table: a TLS handshake for another name fails, and a plain HTTP request for one gets `404` without a list of hosts. Request headers are limited to 64 KiB and a request must be read within 5 minutes. The `X-Forwarded-*` headers an app receives are set by the router, never taken from the client. An app behind a route is reachable by anyone who can reach a router, so it must authenticate its own users.
- The routed containers' ports are published on the node's private address, on ports in the ephemeral range, so that the routers on the other nodes can reach them. Any machine that can reach that address reaches the apps directly, without TLS and without the router. Keep the nodes' private network to the nodes, or add a rule that allows those ports from the other nodes only. With Docker's iptables rules, for a node at `10.0.0.5` whose peers are in `10.0.0.0/24`: `iptables -I DOCKER-USER -p tcp -m conntrack --ctorigdst 10.0.0.5 --ctorigdstport 32768:60999 ! -s 10.0.0.0/24 -j DROP`. The rule must still let the other nodes in: a firewall or cloud security group that blocks the range between the nodes leaves each router with only its own node's replicas ([routing](routing.md#ports-and-firewall)).
- The certificates' private keys and the ACME account key are in the server's database, and every worker can read them through the node API (see above). `hivemind backup` includes them. A worker's router keeps them in memory only.

## Apps on one node

hivemind keeps apps on one node apart no further than Docker does by default. Trust the apps you put on a node with each other, or give an app a node of its own.

- Network. Every container hivemind starts is on Docker's default bridge network, as `docker run` without `--network` puts it. Containers on one node reach each other's container ports at their container addresses (`172.17.0.x`), whether the port is published or not, and whatever address it is published on: a port published on `127.0.0.1` is private from other machines, not from the other apps on its node. An app that other apps on its node must not reach has to authenticate its clients. Containers on different nodes reach each other only through published host ports.
- Memory and processes. An app has no memory limit unless its spec sets `resources.memory`, and no CPU limit unless it sets `resources.cpu`. No app has a PID limit. A replica that uses all the memory, or forks without end, can take down every replica on its node, and on the server's node `serve` and its store. Set `resources.memory` on every app, and keep apps you do not trust off the server's node with `hivemind node eligibility <id> --disable`.
- Disk. With the daemon's default log driver `json-file`, each container's log is rotated at 10 MB, and five files are kept ([concepts](concepts.md#container-logs)); with another driver, the daemon's settings apply. An app's volumes and its containers' own filesystems have no size limit.
- Labels. hivemind sets its four `hivemind.*` labels on every container, so a label in the image cannot change how the agent treats the container.

## Registry credentials

`hivemind registry login` ([registries](registries.md)) stores a username and password for an image registry. They are stored like environment values: in plain text, in `hivemind.db`, which is mode 0600 in a 0700 directory, and in every backup. The Docker daemon needs the password as it is, so there is no hash to keep instead.

- They reach only nodes that run an image from that registry. The server adds a credential to the assignment of a replica that is to run an image from the registry, when the node asks for credentials. Each node reads only its own assignments, so a node that runs nothing from the registry never gets its credential. A worker keeps it in memory and gives it to its own Docker daemon, over the local socket, in the `X-Registry-Auth` header of each pull. It writes it to no file.
- No API route returns a password. `GET /v1/registries` returns the host, the username and the times, and `PUT` replies with the same. The routes need the admin token, like every other.
- No log line has a password or the encoded header. A refused pull's status names the registry and the command to run, and not the username, and once a credential was sent it leaves out what the registry said, which can name the user. Other pull errors have the password taken out, as it is and in the forms a message may hold it (percent-escaped, escaped as in JSON or Go's `%q`, and `user:password` in each kind of base64), with the header and the `account=` of a token URL.
- The worker sends a credential only for the registry its host names, the image's own, and checks that in the assignment before it pulls.

The integration test pulls with a random password from a registry with htpasswd and TLS, and then checks serve's log, the Docker daemon's log, `status` and the API's replies for the password. Use a token that can only read, and treat it as exposed to every machine that runs an image from its registry.

## Revocation by removal

There is no certificate revocation list and no OCSP. A node certificate is valid for 365 days, and it keeps verifying against the cluster CA after the node is removed. Removing the node is what makes it useless. Renewal does not revoke the old certificate either: it stays valid until it expires. The worker replaces the old key on disk, so only a copy taken before the renewal can still use it, and removing the node stops both.

`hivemind node remove <id>` (`DELETE /v1/nodes/{id}`) puts the node's ID on a denylist, the `removed_nodes` table in the server's database, in the same transaction that deletes the node. From then on:

- The node listener checks the denylist on every request, after the TLS handshake and before routing, and answers `403` with `this node was removed from the cluster`. No node route is reachable with the certificate, whatever path the request names.
- The store refuses the ID when a node registers or reports, so a request that was already past the check when the removal committed cannot bring the node back.
- The join listener never issues a removed ID to a new node.
- The node API's renewal route is refused the same way, so a removed node cannot get a certificate that outlasts the one it has. The store refuses the renewal again in the transaction that records it, so a renewal that races the removal is not handed out.
- The denylist is never pruned, and it survives restarts. A removed ID cannot be allowed again. Restoring a backup brings back the denylist as it was when the backup was taken; remove again any node removed after it. See [backup](backup.md#replacing-a-lost-server).

So a stolen certificate stops working when its node is removed. The node's key and certificate files can stay where they are: they no longer grant anything. A worker that is still running when it is removed gets `403` on its next call, logs that it was removed, and exits with an error. It does not stop its containers. The server has placed their replicas elsewhere already, and stopping them on the strength of one reply is left to the operator, who can run `docker rm -f $(docker ps -aq --filter label=hivemind.app)` on that machine. Starting the worker again fails at once with the same error. The machine can join again only with an empty data directory and a new join token, and it gets a new ID.

Removal needs the admin token. It refuses the server's own node, and it refuses a node that still renews its lease unless `--force` is given, so a node is not cut off by mistake while it runs. To cut off a compromised node that is still running, use `--force`.

Removal does not reach what the node already has: the assignments it read before, including their environment values, and any containers it runs. Treat those values as exposed, and change them. To cut off every node at once, create a new cluster with `init` in a new data directory and join every machine again.

## Certificate renewal

A worker renews its certificate once two thirds of its lifetime have passed: day 243 of 365, with about 122 days left. It checks at start and then every hour. It generates a new key, sends a certificate request for it to the node API's renewal route, and authenticates with its current certificate, which must still be valid. The server:

- issues for the node ID in the client certificate. The ID in the route's path must be the same one, or the request gets `403`. The request's names and extensions are ignored, as at join;
- refuses a removed node with `403`, before routing and again when it records the certificate;
- refuses a certificate that has expired, or is not yet valid, by its clock. The TLS handshake checks this once per connection. The server checks it again on every node API request, because a worker reuses its connection for as long as it keeps calling;
- issues a certificate with the same 365-day lifetime as at join, and records its expiry.

The worker checks the reply: one certificate, chaining to the cluster CA it already has, naming its own node ID, for the new key. It writes the new key and certificate with mode 0600 under staging names, syncs them, and renames them over `node.key` and `node.crt`. A crash between the two renames is finished the next time the worker starts. The worker's TLS config reads the certificate at each handshake (`GetClientCertificate`). After a renewal, the worker sends every later request on new connections, so each one presents the new certificate without a restart. A request already in flight, such as the long poll for log requests, finishes on its old connection, which is then closed rather than reused.

The server renews its own listener certificate the same way, from its CA, without the network. Both listeners read it at each handshake (`GetCertificate`).

A renewal that fails is tried again at the next check, and logged. A worker whose certificate has expired, after more than about four months of failed renewals or a machine that was off for that long, cannot renew: the server refuses the certificate. It logs, and fails at start with, the fix: run `hivemind node remove <id>` on the server, then join the machine again with an empty data directory and a new join token.

`hivemind nodes` shows each node's certificate expiry, and `/metrics` has `hivemind_node_cert_expiry_timestamp_seconds`. A node that joined before 2.1 has its expiry recorded on its first request to a 2.1 server.

The cluster CA lasts ten years. It cannot be rotated; that is out of scope for now. In the CA's last year, certificates are issued to end when the CA does, and the server logs a warning once a day. When the CA expires, no node can reach the server. Before then, create a new cluster with `init` in a new data directory and join every machine to it.

Tests use a shorter certificate lifetime. Only a binary built with `-tags hivemind_testknobs` reads it, from `HIVEMIND_TEST_CERT_LIFETIME`. A release build has no code that shortens a certificate, and refuses to start the cluster listener when that variable is set.
