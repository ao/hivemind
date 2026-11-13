# Concepts

This describes the current build, which runs on one machine. Where the v2 plan says something the code does not do yet, this file says so. [PLAN.md](PLAN.md) has the full plan.

## Server and agent

`hivemind init` creates a cluster on one machine. That machine is the node, and it runs both parts:

- The server keeps the desired state in a SQLite database in the data directory. It serves the HTTP API, and it decides where each replica runs.
- The agent runs the containers. Every pass, it reads its assignments from the server, compares them with the containers that carry hivemind labels, starts what is missing, removes what is extra, and reports what it found.

`hivemind serve` runs both in one process. The agent and the server share the database in the same process, so there is no network protocol between them yet. The agent keeps no state of its own. Its record is the set of containers and their labels, so an agent that restarts picks them up again.

When `serve` stops, containers keep running, but nothing changes them. An exited container stays exited until `serve` runs again.

## Apps and replicas

An app is a desired state: a container image, the command, environment, ports, volumes, resources, a health check, and a replica count. Replicas are numbered from 0 to the count minus one. The count is 1 by default and may be at most 100.

The spec limits are checked on every write. Among them: names are up to 32 lowercase letters, digits and dashes, an image is up to 255 characters, there are at most 128 environment variables and 64 KiB of them in total, at most 32 ports and 16 volumes.

Replica placement is trivial today. Every replica goes to the node that runs the server. `FirstNode` is the only placement function that `serve` uses. The `scheduler` package has placement code for several nodes, with capacity, host-port conflicts and volume pinning, but `serve` does not call it yet.

## Generations

Each app has a generation number, which starts at 1. It goes up when anything in the spec changes, except the replica count. Changing the image, the environment, the ports, the health check, the resources or the volumes all count. Deleting an app and deploying it again under the same name gives it a higher generation, so the old containers are replaced.

A replica's container is named for its generation, `hivemind-<app>-<replica>-g<generation>`. The agent keeps a container only when its generation matches the one the server wants. A container of an older generation is removed.

Scaling does not change the generation. Scaling up starts the new replicas at the current generation. Scaling down marks the high-numbered replicas for removal.

## Labels

Every container hivemind starts carries three labels: `hivemind.app`, `hivemind.replica` and `hivemind.generation`. The agent finds its containers by the `hivemind.app` label, and matches them to replicas by the other two.

## Reconcile

The agent runs a pass every 2 seconds (`--reconcile-interval`), and a pass also runs straight after any change through the API. A pass does the following, in order:

1. Fetch the assignments for this node.
2. List the containers that carry a `hivemind.app` label.
3. Remove the containers that match no assignment, whose generation is old, or that are dead or being removed.
4. For each assignment, supervise the container that is there, or create and start one. A missing image is pulled.
5. Report each replica's state to the server.

A failed replica does not stop the pass. The error goes in the status, and the next pass tries again. If the server cannot be read, the pass changes nothing, so containers keep running.

A spec change is not a rolling update. Step 3 removes every container of the old generation before step 4 starts the new ones, so all replicas of an app are down while the new ones are created and started. Rolling updates are on another branch.

## Health checks and restarts

An app may have one health check: HTTP, TCP or exec.

- HTTP sends `GET` to the container's address on its Docker network, on the container port, and passes on a 2xx or 3xx answer. Redirects are not followed. The path defaults to `/`.
- TCP passes when a connection to the container port opens.
- Exec runs the command inside the container, and passes on exit code 0.

The defaults are an interval of 10 seconds, a timeout of 2 seconds, and 3 retries. A `start_period` makes failures ignored for that long after the container starts. A replica becomes unhealthy after the retries fail in a row.

An app without a health check counts as ready as soon as its container runs.

The agent restarts a container that exits, whatever its exit code, and a container that becomes unhealthy. The wait before each restart doubles from 1 second and stops at 5 minutes. A container that has been running and healthy for 10 minutes starts over at 1 second. The restart count is kept per container, and the agent reads it back from the last status report when it starts.

The restart policy in the container itself is `no`. The agent is the only thing that restarts a container. A container that exits while `serve` is down is not restarted until `serve` runs again.

Status reports the state of each replica, its node, its generation and its last error. It does not show the restart count, the health or the crash loop. The restart count and the ready count are in [`/metrics`](api.md#metrics). Showing them in status is on the roadmap.

## Ports

A port in the spec is published on a host address. The default address is `127.0.0.1`, so a port is reachable from the machine only. Write `0.0.0.0:8080:80` or another address to publish it elsewhere. The protocol is `tcp` by default, and `udp` is allowed. An IPv6 address goes in brackets, such as `[::1]:8080:80`.

Two apps on one node should not publish the same host port, and nothing stops them from doing it. In a test on Docker 29 (`docker:dind`, one node, 2026-11-13), the second app's first start failed with `port is already allocated`, and the agent logged that once. On the next pass the container started anyway, without its published port, and `status` showed it `running` with no error. Until the scheduler's host-port check is wired in, check the ports you publish.

There is no overlay network and no DNS. A container gets an address on its Docker network, which other containers on the same node can reach, but nothing finds it by name.

## Volumes

A volume is a Docker named volume on the node, mounted at an absolute path other than `/`. An app with volumes can have only one replica.

Volume names are not scoped to an app. The Docker volume is named by the name in the spec, so two apps that use the same volume name share the data. Choose names that are unique on the node.

Deleting an app removes its containers and keeps the named volumes. A deploy that uses the same volume name finds the data again.

With one node, every volume is on that node, so nothing needs pinning yet. The plan's Design section sets the pinning rule for several nodes: a volume-pinned app is never moved.

## Resources

`resources.cpu` and `resources.memory` are checked for range and stored with the spec. They are not applied to the container, and nothing checks them against the node's capacity. A replica does not get a memory or CPU limit.

## Deleting an app

`hivemind delete` marks the app deleted at once, so `status` and the other reads stop showing it. The next reconcile pass on the node removes its containers. The app's rows are removed once the node reports that no containers are left.

## Security defaults

- The API listens on `127.0.0.1:4483`. An address that is not loopback needs `--api-tls-cert` and `--api-tls-key`, or the server refuses to start.
- `init` generates an admin token: 32 bytes from `crypto/rand`, with the prefix `hm_`. It prints the token once, and writes it to `<data-dir>/admin.token` with mode 0600. The server stores only its SHA-256 digest, in the database.
- Every route except `GET /healthz` needs the token as `Authorization: Bearer`. The check is in constant time. See [the API](api.md#authentication).
- `init` refuses to run when a cluster already exists, and it never overwrites the token file.
- The data directory is mode 0700. The database file is mode 0600, because specs hold environment variables, which can be secrets.
- Logs leave out tokens, cookies, CSRF tokens, private keys and environment values. The redaction is in the logger, and the code that logs is written not to pass them in.
- The metrics listener, `--metrics-addr`, must be a loopback address, and it needs the token too.

Not built yet: the node-to-node listener and its mutual TLS (`--advertise`, port 4484, and the join tokens), the web UI with its login and CSRF forms, and any use of the TLS flags beyond the API. The `pki` and `auth` packages contain code for the UI sessions and the cluster CA, but no route or listener uses them.

## Data directory

The data directory holds the database (`hivemind.db`), the admin token file (`admin.token`), and an optional config file (`hivemind.yaml`). The default is `/var/lib/hivemind` for root, and `$XDG_STATE_HOME/hivemind` or `~/.local/state/hivemind` for other users. `--data-dir` or `HIVEMIND_DATA_DIR` changes it.

The database uses SQLite in WAL mode, with `synchronous=FULL`. Every change is one transaction. The schema has a version number, and a newer schema is refused. A v1 data directory is not recognised yet, so use a new directory for v2. See [upgrading](upgrading.md).
