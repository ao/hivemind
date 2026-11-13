# Security

This lists every listener the code opens, what each one exposes, and the files hivemind writes. The claims come from the code in `cmd/hivemind`, `internal/auth`, `internal/cluster`, `internal/pki` and `internal/store`. The concepts are in [concepts](concepts.md).

## Listeners

| Listener | Default bind | Auth | Exposes |
|---|---|---|---|
| API | `127.0.0.1:4483` (`--api-addr`) | Bearer admin token, compared in constant time. `GET /healthz` needs no token and returns only `{"status":"ok"}` | Apps and their specs, including environment values. Status. Logs of this server's containers. Join token issuance. Metrics |
| Metrics | Off. `--metrics-addr` starts it, and it must be loopback | The admin token | Prometheus metrics only |
| Node listener | Off. Starts with `--advertise` on that address, port 4484 (`--cluster-port`) | TLS 1.3, and a client certificate that chains to the cluster CA and names a node. The node ID must match the one in the path | To a node: its own assignments, which include each replica's full spec and environment values. It can also report its own containers and register its own name and capacity |
| Join listener | Off. Starts with `--advertise` on that address, port 4485 | TLS 1.3 with the server certificate. No client certificate. The join token is the credential. Only `POST /join` is routed | The cluster CA certificate, and a node certificate for each valid token |

Listeners bind to the advertise address only. A wildcard address is refused at startup, both by config validation and when the socket is bound. The worker opens no listener.

The API TLS flags apply to the API only. The CLI has no option for trusting a private CA, so it can reach a TLS API only when the system already trusts its certificate.

Two more things reach the network from containers, not from hivemind. A container's published ports are bound to the address in its spec, `127.0.0.1` by default, so an app that publishes `0.0.0.0` is reachable from the network by its own choice. And the Docker Engine socket (`--docker-host`, `unix:///var/run/docker.sock` by default) is local. Anything that can write to that socket controls the host, and hivemind needs that access.

The join listener is open to the network whenever the server runs with `--advertise`. Its only protection is the token. Tokens expire, and failed joins from one address are limited (5 in 10 minutes).

## Files

The server's data directory is created with mode 0700.

| File | Mode | Holds |
|---|---|---|
| `hivemind.db`, with the WAL files beside it | 0600 | Apps and their specs, which include environment values. Replicas, nodes and their leases. Join tokens as SHA-256 hashes. The admin token as a SHA-256 hash |
| `admin.token` | 0600 | The admin token, in plain text. This is the only copy |
| `ca.key` | 0600 | The cluster CA's private key. It can sign any node certificate |
| `ca.crt` | 0644 | The cluster CA certificate. Public |
| `hivemind.yaml` | As you create it | Optional settings. hivemind does not write this file. It may hold a `token` if you put one there |

The CLI config that `init` writes is `~/.config/hivemind/config.yaml`, with mode 0600, in a directory that is 0700 when it is created. It names the server and the token file. It does not hold the token.

A worker's data directory is created with mode 0700.

| File | Mode | Holds |
|---|---|---|
| `node.key` | 0600 | The worker's private key |
| `node.crt` | 0644 | The worker's certificate. Public |
| `ca.crt` | 0644 | The cluster CA certificate. Public |
| `node.json` | 0600 | The node ID, the server's address and the join time |

A worker has no CA key, no database and no admin token.

## What a compromised worker can do

Assume an attacker has the worker's data directory, or root on the worker. The code allows the following.

- Read the assignment of every replica placed on the worker. That is the full spec, including environment values and volume names. Treat every environment value as exposed to each machine that runs the app.
- Report false container states for its own node. The server takes the report as given, and the scheduler and rollouts act on it.
- Renew or stop renewing its own lease. Stopping makes the server move its replicas.
- Register any name of up to 253 printable characters, and any capacity up to the limits the server checks: 1,048,576 CPUs and 2^50 bytes of memory. Placement trusts these figures, so the worker can attract more replicas, and so more environment values, than it has room for.
- Run whatever its Docker daemon will run, which is the worker's own host.

The worker cannot do the following.

- Call the node routes for another node. The node ID in the path must match the one in its certificate, or the server answers 403.
- Obtain a certificate for another node or for the server. It has no CA key, and the server ignores names and extensions in a certificate request.
- Join another machine, or pose as the server. Its certificate names no host, so a machine that checks the server's name rejects it. It cannot issue join tokens.
- Reach the API or the admin token, the CA key, or the database.
- Read the status or logs of replicas on other machines.

## No revocation yet

hivemind cannot revoke a node certificate, and it has no command to remove a node.

- A node certificate is valid for 365 days. A stolen one works on the node listener until it expires. There is no certificate revocation list and no OCSP.
- A node that is lost or compromised stays in the node table. Its replicas are placed elsewhere, but its access is not removed.
- The only way to cut off every node is to create a new cluster with `init` in a new data directory and join every machine again.

There is no renewal either. When a worker's certificate expires after 365 days, the worker cannot reach the server. `join` refuses a data directory that holds a joined node, so the worker needs an empty data directory and a new join token. The server's own certificate is issued again each time `serve --advertise` starts, so a restart renews it.
