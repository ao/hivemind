# Concepts

This describes the build on this branch. It runs one server and any number of workers, on Linux machines with Docker Engine. Where a limit applies, the section says so. [PLAN.md](PLAN.md) has the full plan.

## Server and workers

`hivemind init` creates a cluster on one machine. That machine is the server. It keeps the desired state in a SQLite database in the data directory, serves the HTTP API on `127.0.0.1:4483` by default, decides where each replica runs, and runs an agent for its own containers.

A worker is another machine that joined with `hivemind join`. It runs an agent and nothing else: no database and no API. Each pass, the agent reads its assignments from the server over the node API, compares them with its containers, and reports back.

`hivemind serve` runs the server and its agent in one process. `hivemind serve --role worker` runs a worker that has already joined. Stopping either one leaves its containers running, but nothing changes them until the agent runs again. If the server is down, workers keep their containers and no new replica is placed.

The agent keeps no state of its own. Its record is the set of containers and their labels, so an agent that restarts picks them up again.

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

## Node certificates and the node API

A node certificate is ECDSA P-256, signed by the cluster CA, and valid for 365 days. Its URI name is `hivemind://node/<id>`, and its common name is the same ID. The server's own certificate also carries the address it advertises, so a worker can check the name it dialed.

The node API runs on the cluster port (`--cluster-port`, 4484 by default) on the `--advertise` address. It opens only when the server runs with `--advertise`. A request is accepted only when the client presents a certificate that chains to the cluster CA and names a node. Each route then checks that the node ID in its path is the node ID in the certificate, and answers 403 if it is not. A node can therefore read its own assignments, report its own containers, and register its own name and capacity. It cannot read or write another node's data.

The node API is internal, and its routes are not a stable interface. See [the API](api.md#node-api-internal).

## Leases and node loss

Each pass of a worker's agent reports its containers to the server. That report renews the node's lease. A pass runs every `--reconcile-interval` (2 s by default), and joining and registering also renew the lease. If a worker cannot read its assignments, it does not report, so its lease ages.

A node is lost when its lease is more than 15 s old. The 15 s is fixed. The server checks leases every 5 s, so a lost node is noticed 15 to 20 s after its last renewal. The server logs one line for each node it marks as lost.

Two things follow from that:

- `--reconcile-interval` is at most 5 s, so a worker can miss two passes before its 15 s lease runs out. A longer value is refused at start-up.
- A worker that cannot reach the server for 15 s is treated as lost, and its replicas are placed again.

## Placement

The scheduler decides where each replica runs. Its input is the nodes, the replicas already placed, and the time. It runs on every deploy, scale, node registration, status report and sweep. A replica without a node is `pending` until one takes it. For each replica, the scheduler applies these rules:

- Only eligible nodes take new replicas. A node is eligible when its lease is current and it has been ready for `--min-ready` (see below). The server's own node is always ready.
- The CPU and memory the replica asks for must fit in what is left on the node, after the replicas already placed there. A node's totals are the CPU count and memory that its Docker daemon reports. A replica's `resources` are counted here, but the container gets no limit (see [Resources](#resources)).
- Its host ports must not clash with the ports of replicas already on the node. Two ports clash when the number and protocol match and the bind addresses overlap. A wildcard address (`0.0.0.0` or `::`) overlaps every address. Two different specific addresses do not overlap. The default bind is `127.0.0.1`.
- Among the nodes that fit, it takes the one with the fewest replicas of the same app. Then the lowest load, which is the larger of the CPU and memory fractions in use. Then the fewest replicas overall, and then the lowest node ID.

Placement never moves a replica that already has a node. Moves come only from node loss and from rollouts.

A replica that cannot be placed gives its reason in the status `DETAIL` column, and in the `pending_reason` field of the API. Reasons include `no node is ready`, `no node has been ready for 30s`, `no node has 1 CPU free`, and `host port 127.0.0.1:8080/tcp is in use on every eligible node`.

Two apps that publish the same host port cannot share a node. `deploy` accepts the second app anyway. Its replicas go to the eligible nodes where the port is free, and any replica that has no such node stays `pending` with the host-port reason. It is placed once the first app's containers are gone from a node.

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

## Rolling updates

A change to an app's spec, other than its replica count, starts a rollout. The rollout moves the replicas to the new generation, one at a time. The rules come from the store's rollout code (`internal/store/rollout.go`).

- One move runs at a time. A move starts only when the previous one has finished, so at most one replica is being moved and at most one extra copy runs.
- A move has two forms. A surge places the new copy on another eligible node that has the app's host ports free. The copy runs beside the old one, and the replica moves there once the copy is ready. An app with volumes is never surged. In place stops the old container and starts the new one on the same node, so that replica is down while it starts.
- The next move starts only when the replica in flight reports ready at the new generation.
- A move has a deadline: the health check's start period, plus its retries at its interval, plus 30 seconds. An app with no health check gets 2 minutes. A move that is not ready by then pauses the rollout. A move whose container is crash-looping pauses it too.
- A pause undoes the move in flight. An in-place move goes back to the old generation, and a surge is removed. The other replicas are not touched. The rollout stays paused, with a reason, until you deploy a spec with another template.
- Rolling back is deploying the old spec again. It is a new generation, so it is a rollout like any other.
- Deploying another template during a rollout abandons the move in flight, removes its surge, and starts again from the state the replicas are in.
- A rollout ends when every replica runs the target generation. Its state then drops out of `status`.

On a single node every move is in place: the replica being moved is `pending` while its new container starts, and the others keep running. With a second node that is eligible and has the app's host ports free, the move becomes a surge.

`hivemind status` prints a line for each app with a rollout in progress or paused, such as `web: rolling out generation 2`, or the pause reason. The API reports the same thing as the `rollout` field of an app's status. See [the API](api.md#objects).

## Apps and replicas

An app is a desired state: a container image, the command, environment, ports, volumes, resources, a health check, and a replica count. Replicas are numbered from 0 to the count minus one. The count is 1 by default and may be at most 100.

The spec limits are checked on every write. Among them: names are up to 32 lowercase letters, digits and dashes, an image is up to 255 characters, there are at most 128 environment variables and 64 KiB of them in total, at most 32 ports and 16 volumes.

## Generations

Each app has a generation number, which starts at 1. It goes up when anything in the spec changes, except the replica count. Changing the image, the environment, the ports, the health check, the resources or the volumes all count. Deleting an app and deploying it again under the same name gives it a higher generation, so the old containers are replaced.

Each replica has its own generation. A change to the spec sets the app's generation, but the replicas keep their old generation until a rollout moves them, one at a time (see [Rolling updates](#rolling-updates)). The app's `generation` in `status` is the newest one, and each replica row shows the generation its container is at.

A replica's container is named for its generation, `hivemind-<app>-<replica>-g<generation>`. The agent keeps a container only when its generation matches the one the server wants for that replica. A container of an older generation is removed once the replica has moved on.

Scaling does not change the generation. Scaling up starts the new replicas at the current generation. Scaling down marks the high-numbered replicas for removal.

## Labels

Every container hivemind starts carries three labels: `hivemind.app`, `hivemind.replica` and `hivemind.generation`. The agent finds its containers by the `hivemind.app` label, and matches them to replicas by the other two.

## Reconcile

The agent runs a pass every 2 seconds (`--reconcile-interval`), and a pass also runs straight after any change through the API. On the server, the agent reads its assignments from the store. On a worker, it reads them over the node API. A pass does the following, in order:

1. Fetch the assignments for this node.
2. List the containers that carry a `hivemind.app` label.
3. Remove the containers that match no assignment, whose generation is old, or that are dead or being removed.
4. For each assignment, supervise the container that is there, or create and start one. A missing image is pulled.
5. Report each replica's state to the server.

A failed replica does not stop the pass. The error goes in the status, and the next pass tries again. If the assignments cannot be read, the pass changes nothing, so containers keep running, and the agent does not report, so its lease ages.

## Health checks and restarts

An app may have one health check: HTTP, TCP or exec.

- HTTP sends `GET` to the container's address on its Docker network, on the container port, and passes on a 2xx or 3xx answer. Redirects are not followed. The path defaults to `/`.
- TCP passes when a connection to the container port opens.
- Exec runs the command inside the container, and passes on exit code 0.

The defaults are an interval of 10 seconds, a timeout of 2 seconds, and 3 retries. A `start_period` makes failures ignored for that long after the container starts. A replica becomes unhealthy after the retries fail in a row.

An app without a health check counts as ready as soon as its container runs.

The agent restarts a container that exits, whatever its exit code, and a container that becomes unhealthy. The wait before each restart doubles from 1 second and stops at 5 minutes. A container that has been running and healthy for 10 minutes starts over at 1 second. The restart count is kept per container, and the agent reads it back from the last status report when it starts.

The restart policy in the container itself is `no`. The agent is the only thing that restarts a container. A container that exits while the agent is down is not restarted until the agent runs again.

Status reports the state of each replica, its node, its generation, its health, its restart count and its last error. While a restart waits out its backoff, the replica is crash-looping and status gives the time of the next restart: `hivemind status` prints "crash-looping, next restart in 40s".

## Ports

A port in the spec is published on a host address. The default address is `127.0.0.1`, so a port is reachable from the machine only. Write `0.0.0.0:8080:80` or another address to publish it elsewhere. The protocol is `tcp` by default, and `udp` is allowed. An IPv6 address goes in brackets, such as `[::1]:8080:80`.

Replicas that publish the same host port cannot share a node. The scheduler enforces this (see [Placement](#placement)). Two more cases:

- A port held by something outside hivemind, such as another program on the machine, makes Docker refuse the bind with `port is already allocated`. The agent removes that container, and the replica stays `pending` with the error in status. Each pass tries again with a new container, so the replica starts, with its port published, once the port is free. A container whose start failed is never started again, because Docker can let a second start succeed without the published port.
- Containers on different machines reach each other only through host ports. There is no overlay network and no DNS. A container gets an address on its Docker network, which other containers on the same node can reach, but nothing finds it by name.

## Volumes

A volume is a Docker named volume on the node where the replica runs, mounted at an absolute path other than `/`. An app with volumes can have only one replica.

The replica stays on the node where it was first placed. If that node is lost, the replica is not moved, and the app is down until the node returns (see [Node loss and rescheduling](#node-loss-and-rescheduling)).

Volume names are not scoped to an app. The Docker volume is named by the name in the spec, so two apps that use the same volume name on one node share the data. On different nodes they get different volumes. Choose names that are unique on the node.

Deleting an app removes its containers and keeps the named volumes. A deploy that uses the same volume name on the same node finds the data again.

## Resources

`resources.cpu` and `resources.memory` are checked for range and stored with the spec. Placement counts them against each node's CPU and memory, so a node is not given more replicas than its totals allow. They are not applied to the container: a replica has no Docker CPU or memory limit.

The totals are what the node reports, and a worker reports its own. Placement trusts those figures. A worker that reports more capacity than it has will be given more replicas.

## Deleting an app

`hivemind delete` marks the app deleted at once, so `status` and the other reads stop showing it. The next reconcile pass on each node removes that node's containers. The app's rows are removed once no node reports a container.

## Security

The API token, the admin token and the file modes are described in [security](security.md). In short:

- The API listens on `127.0.0.1:4483`. An address that is not loopback needs `--api-tls-cert` and `--api-tls-key`, or the server refuses to start.
- `init` generates an admin token: 32 bytes from `crypto/rand`, with the prefix `hm_`. It prints the token once, and writes it to `<data-dir>/admin.token` with mode 0600. The server stores only its SHA-256 digest, in the database.
- Every route except `GET /healthz` needs the token as `Authorization: Bearer`. The check is in constant time. See [the API](api.md#authentication).
- `init` refuses to run when a cluster already exists, and it never overwrites the token file or the CA.
- Logs are redacted by the logger (`internal/logging`). It masks the value of any attribute whose key contains `token`, `secret`, `password`, `passwd`, `authorization`, `cookie`, `session`, `csrf`, `privatekey`, `apikey`, `credential` or `env`, with dashes, underscores and case ignored. It also masks any value of the config's secret type, and any string that holds an admin token (`hm_` and 43 characters), a join token, a bearer credential or a PEM private key. Session IDs, CSRF tokens and environment values cannot be recognised by their shape, so the code that logs must not pass them in. The logger writes text by default, or JSON with `--log-format json`.
- The metrics listener, `--metrics-addr`, must be a loopback address, and it needs the token too.

The web UI is not built. The `auth` package has code for UI sessions, and no route uses it. The TLS flags apply to the API only.

## Data directories

The server's data directory holds the database (`hivemind.db`, with the SQLite WAL files beside it), the admin token file (`admin.token`), the cluster CA (`ca.key` and `ca.crt`), and an optional config file (`hivemind.yaml`). The default is `/var/lib/hivemind` for root, and `$XDG_STATE_HOME/hivemind` or `~/.local/state/hivemind` for other users. `--data-dir` or `HIVEMIND_DATA_DIR` changes it.

A worker's data directory holds its key (`node.key`), its certificate (`node.crt`), the cluster CA certificate (`ca.crt`), and `node.json`, which records the node ID and the server it joined. `node.json` is written last, so its presence means the join finished. [Security](security.md#files) lists the file modes.

The database uses SQLite in WAL mode, with `synchronous=FULL`. Every change is one transaction. The schema has a version number, and a newer schema is refused. A v1 data directory is refused with a message, so use a new directory for v2. See [upgrading](upgrading.md).
