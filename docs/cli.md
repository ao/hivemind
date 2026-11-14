# Command reference

This file is generated from the command definitions. Run `make docs` after changing a
command, flag or help text. The test `TestCLIDocIsCurrent` fails when this file is out of date.

Settings are resolved in this order: a flag, then a `HIVEMIND_*` environment variable, then the
YAML config file (`serve` and `init` read `<data-dir>/hivemind.yaml` unless `--config` is given),
then the default. The client commands read the server address and token from their flags, then from
`HIVEMIND_SERVER` and `HIVEMIND_TOKEN`, then from the client config that `init` writes.

`init` and `serve` accept the client settings `--server` and `--token`. They do not use them.
`logs` accepts `--json` and ignores it.

Exit status is 0 on success and 1 on any error, including a bad flag or argument. Errors are written to
standard error with an `error:` prefix.

## hivemind

Run containers on a small cluster of machines

```
hivemind
```

| Command | Description |
|---|---|
| `backup` | Write a backup of the server's state to a file |
| `compose` | Convert a Docker Compose file to apps, and deploy or delete them |
| `delete` | Delete an app and stop its containers |
| `deploy` | Create or update an app |
| `init` | Create a cluster on this machine |
| `join` | Join a cluster as a worker, then run its agent |
| `join-token` | Print a token that lets another machine join this cluster |
| `logs` | Print a replica's container log |
| `node` | Manage one node |
| `nodes` | List the nodes in the cluster |
| `restore` | Restore a server's state from a backup into an empty data directory |
| `scale` | Set an app's replica count |
| `serve` | Run the server and the agent on this node |
| `status` | Show apps, their replicas and container state |
| `version` | Print the version |

### hivemind backup

Write a backup of the server's state to <file>, with mode 0600. The file must
not exist.

The backup holds the cluster CA's private key, which can sign a certificate for
any node, and the admin token. Anyone who has the file can join machines to the
cluster and call the API. Keep it as private as the data directory: mode 0600,
on a disk only you can read, and encrypted if it leaves the machine.

By default backup reads the data directory (--data-dir) on this machine. It
works whether serve is running or not: the store is copied with SQLite's
VACUUM INTO, which reads one consistent snapshot and does not block the
server. It needs read access to the data directory, so run it as the user
that runs serve.

--api asks the server for the backup instead (POST /v1/backup), with the
admin token. The server sends it only over TLS, or to a client on loopback.

The file is a tar archive: manifest.json, with a format version and the
SHA-256 of each file, then hivemind.db, ca.key, ca.crt and admin.token. backup
reads the file back and checks every digest before it keeps it. Restore it
with hivemind restore.

```
hivemind backup <file> [flags]
```

| Flag | Type | Default | Environment | Description |
|---|---|---|---|---|
| `--api` | bool | - | - | get the backup from the server's API instead of reading the data directory |
| `--ca-cert` | string | - | - | PEM file of the CA certificates to trust for an https server, instead of the system's (env HIVEMIND_CA_CERT; default from the client config) |
| `--client-config` | string | - | `HIVEMIND_CLIENT_CONFIG` | client config file (env HIVEMIND_CLIENT_CONFIG; default $XDG_CONFIG_HOME/hivemind/config.yaml or ~/.config/hivemind/config.yaml) |
| `--data-dir` | string | see description | `HIVEMIND_DATA_DIR` | the server's data directory (env HIVEMIND_DATA_DIR; default /var/lib/hivemind for root, else ~/.local/state/hivemind). The default is /var/lib/hivemind for root, and $XDG_STATE_HOME/hivemind or ~/.local/state/hivemind for other users. |
| `--json` | bool | - | - | print JSON instead of a table |
| `--server` | string | - | `HIVEMIND_SERVER` | server URL (env HIVEMIND_SERVER; default from the client config, then http://127.0.0.1:4483) |
| `--token` | string | - | `HIVEMIND_TOKEN` | API token (env HIVEMIND_TOKEN; default from the client config's token file) |

### hivemind compose

Turn each service in a Docker Compose file into an app:

  hivemind compose convert compose.yaml   print the app specs
  hivemind compose deploy compose.yaml    create or update the apps
  hivemind compose delete compose.yaml    delete the apps

Only part of Compose is read. Any other key is refused, with its path and a
reason, and every refusal is listed in one error. Variables come from the
environment, then from a .env file beside the compose file, and a variable
that is not set and has no default is an error. A port with no address is
published on 127.0.0.1, where Compose would publish it on every interface.
Apps do not find each other by service name. See docs/compose.md.

```
hivemind compose
```

| Command | Description |
|---|---|
| `convert` | Print the app specs for a compose file |
| `delete` | Delete the apps in a compose file |
| `deploy` | Create or update the apps in a compose file |

#### hivemind compose convert

Print the app spec for each service in a compose file, as YAML, one
document per service, sorted by name. Each document is a file that
hivemind deploy -f reads. - reads the compose file from standard input, and
the .env file from the working directory.

Nothing is sent to the server. A note on standard error names each port that
is published on 127.0.0.1 because the file gives it no address.

```
hivemind compose convert <file> [flags]
```

| Flag | Type | Default | Environment | Description |
|---|---|---|---|---|
| `--prefix` | string | - | - | name each app PREFIX-<service> instead of <service> |

#### hivemind compose delete

Delete the app for each service in a compose file, in name order, printing a
line for each. Only the service names are read, so the file's variables need
not be set, and keys that convert would refuse are ignored. An app that does
not exist is reported as not found and skipped. Volumes are kept, as
hivemind delete keeps them.

```
hivemind compose delete <file> [flags]
```

| Flag | Type | Default | Environment | Description |
|---|---|---|---|---|
| `--ca-cert` | string | - | - | PEM file of the CA certificates to trust for an https server, instead of the system's (env HIVEMIND_CA_CERT; default from the client config) |
| `--client-config` | string | - | `HIVEMIND_CLIENT_CONFIG` | client config file (env HIVEMIND_CLIENT_CONFIG; default $XDG_CONFIG_HOME/hivemind/config.yaml or ~/.config/hivemind/config.yaml) |
| `--json` | bool | - | - | print JSON instead of a table |
| `--prefix` | string | - | - | name each app PREFIX-<service> instead of <service> |
| `--server` | string | - | `HIVEMIND_SERVER` | server URL (env HIVEMIND_SERVER; default from the client config, then http://127.0.0.1:4483) |
| `--token` | string | - | `HIVEMIND_TOKEN` | API token (env HIVEMIND_TOKEN; default from the client config's token file) |

#### hivemind compose deploy

Convert a compose file, check every app, and then create or update each one
as hivemind deploy -f does, in name order, printing a line for each.

Nothing is sent until every service converts and its spec is valid. If the
server refuses an app, the command stops there: the apps before it stay
deployed, and the error names them.

```
hivemind compose deploy <file> [flags]
```

| Flag | Type | Default | Environment | Description |
|---|---|---|---|---|
| `--ca-cert` | string | - | - | PEM file of the CA certificates to trust for an https server, instead of the system's (env HIVEMIND_CA_CERT; default from the client config) |
| `--client-config` | string | - | `HIVEMIND_CLIENT_CONFIG` | client config file (env HIVEMIND_CLIENT_CONFIG; default $XDG_CONFIG_HOME/hivemind/config.yaml or ~/.config/hivemind/config.yaml) |
| `--json` | bool | - | - | print JSON instead of a table |
| `--prefix` | string | - | - | name each app PREFIX-<service> instead of <service> |
| `--server` | string | - | `HIVEMIND_SERVER` | server URL (env HIVEMIND_SERVER; default from the client config, then http://127.0.0.1:4483) |
| `--token` | string | - | `HIVEMIND_TOKEN` | API token (env HIVEMIND_TOKEN; default from the client config's token file) |

### hivemind delete

Delete an app and stop its containers

```
hivemind delete <app> [flags]
```

| Flag | Type | Default | Environment | Description |
|---|---|---|---|---|
| `--ca-cert` | string | - | - | PEM file of the CA certificates to trust for an https server, instead of the system's (env HIVEMIND_CA_CERT; default from the client config) |
| `--client-config` | string | - | `HIVEMIND_CLIENT_CONFIG` | client config file (env HIVEMIND_CLIENT_CONFIG; default $XDG_CONFIG_HOME/hivemind/config.yaml or ~/.config/hivemind/config.yaml) |
| `--json` | bool | - | - | print JSON instead of a table |
| `--server` | string | - | `HIVEMIND_SERVER` | server URL (env HIVEMIND_SERVER; default from the client config, then http://127.0.0.1:4483) |
| `--token` | string | - | `HIVEMIND_TOKEN` | API token (env HIVEMIND_TOKEN; default from the client config's token file) |

### hivemind deploy

Create or update an app, either from flags or from a YAML or JSON file:

  hivemind deploy --name web --image nginx:alpine --replicas 2 --port 8080:80 --env MODE=prod
  hivemind deploy -f app.yaml

--port is [bind:]host:container[/udp]. A port is published on 127.0.0.1 unless
it names another bind address. -f - reads the spec from standard input.

```
hivemind deploy [flags]
```

| Flag | Type | Default | Environment | Description |
|---|---|---|---|---|
| `--ca-cert` | string | - | - | PEM file of the CA certificates to trust for an https server, instead of the system's (env HIVEMIND_CA_CERT; default from the client config) |
| `--client-config` | string | - | `HIVEMIND_CLIENT_CONFIG` | client config file (env HIVEMIND_CLIENT_CONFIG; default $XDG_CONFIG_HOME/hivemind/config.yaml or ~/.config/hivemind/config.yaml) |
| `--env` | string, repeatable | - | - | environment variable, KEY=VALUE (repeatable) |
| `-f`, `--file` | string | - | - | app spec file, YAML or JSON (- for standard input) |
| `--image` | string | - | - | container image |
| `--json` | bool | - | - | print JSON instead of a table |
| `--name` | string | - | - | app name |
| `--port` | string, repeatable | - | - | published port, [bind:]host:container[/udp] (repeatable) |
| `--replicas` | int | - | - | number of replicas (default 1) |
| `--server` | string | - | `HIVEMIND_SERVER` | server URL (env HIVEMIND_SERVER; default from the client config, then http://127.0.0.1:4483) |
| `--token` | string | - | `HIVEMIND_TOKEN` | API token (env HIVEMIND_TOKEN; default from the client config's token file) |

### hivemind init

Create a cluster on this machine. init creates the data directory (mode 0700),
generates the admin token, writes it to <data-dir>/admin.token (mode 0600) and
stores only its SHA-256 digest in the cluster database. The token is printed
once. init also writes a client config that points the CLI at this server and
at the token file. With --api-tls-cert, the client config also names that
certificate as ca-cert, so the CLI on this machine trusts it. init refuses to
run if a cluster already exists.

```
hivemind init [flags]
```

| Flag | Type | Default | Environment | Description |
|---|---|---|---|---|
| `--advertise` | string | - | `HIVEMIND_ADVERTISE` | IP or host name other nodes use to reach this server; serve opens the cluster listeners on it |
| `--api-addr` | string | `127.0.0.1:4483` | `HIVEMIND_API_ADDR` | address of the API, host:port |
| `--api-tls-cert` | string | - | `HIVEMIND_API_TLS_CERT` | TLS certificate for the API (PEM) |
| `--api-tls-key` | string | - | `HIVEMIND_API_TLS_KEY` | TLS private key for the API (PEM) |
| `--client-config` | string | - | `HIVEMIND_CLIENT_CONFIG` | client config file to write (default $XDG_CONFIG_HOME/hivemind/config.yaml or ~/.config/hivemind/config.yaml; env HIVEMIND_CLIENT_CONFIG) |
| `--cluster-port` | string | `4484` | `HIVEMIND_CLUSTER_PORT` | port of the node-to-node listener; the join listener uses the port above it |
| `--config` | string | - | `HIVEMIND_CONFIG` | YAML config file (default <data-dir>/hivemind.yaml) |
| `--data-dir` | string | see description | `HIVEMIND_DATA_DIR` | directory for node state. The default is /var/lib/hivemind for root, and $XDG_STATE_HOME/hivemind or ~/.local/state/hivemind for other users. |
| `--docker-host` | string | `unix:///var/run/docker.sock` | `HIVEMIND_DOCKER_HOST` | Docker Engine endpoint |
| `--log-format` | string | `text` | `HIVEMIND_LOG_FORMAT` | log format: text or json |
| `--log-level` | string | `info` | `HIVEMIND_LOG_LEVEL` | log level: debug, info, warn or error |
| `--metrics-addr` | string | - | `HIVEMIND_METRICS_ADDR` | separate loopback address for /metrics, host:port; empty means API only |
| `--min-ready` | string | `30s` | - | how long a node must be ready before it gets new replicas; 0s means no wait |
| `--reconcile-interval` | string | `2s` | `HIVEMIND_RECONCILE_INTERVAL` | how often the agent reconciles containers and renews its lease (at most 5s) |
| `--server` | string | `http://127.0.0.1:4483` | `HIVEMIND_SERVER` | server URL for the client commands |
| `--token` | string | - | `HIVEMIND_TOKEN` | API token for the client commands (not logged; never read from the config file) |

### hivemind join

Join a cluster as a worker node and run its agent in the foreground.

  hivemind join 10.0.0.5 --token <token>

<server> is the server's address, host or host:port, where the port is its
cluster port (4484 by default). The worker generates a key and a certificate
request, presents the join token to the server's join port (the cluster port
plus one), and checks the reply against the CA fingerprint in the token. It
then stores its key (mode 0600), its certificate, the cluster CA and its node
ID in the data directory, and starts the agent.

The token is single use and expires; get one with hivemind join-token on the
server. --token is read from the flag only, not from HIVEMIND_TOKEN, which is
the admin token.

A joined machine keeps its state. After a restart, run
"hivemind serve --role worker" with the same data directory. A worker has no
API listener, so --advertise does not apply to it.

```
hivemind join <server> [flags]
```

| Flag | Type | Default | Environment | Description |
|---|---|---|---|---|
| `--api-addr` | string | `127.0.0.1:4483` | `HIVEMIND_API_ADDR` | address of the API, host:port |
| `--api-tls-cert` | string | - | `HIVEMIND_API_TLS_CERT` | TLS certificate for the API (PEM) |
| `--api-tls-key` | string | - | `HIVEMIND_API_TLS_KEY` | TLS private key for the API (PEM) |
| `--cluster-port` | string | `4484` | `HIVEMIND_CLUSTER_PORT` | port of the node-to-node listener; the join listener uses the port above it |
| `--config` | string | - | `HIVEMIND_CONFIG` | YAML config file (default <data-dir>/hivemind.yaml) |
| `--data-dir` | string | see description | `HIVEMIND_DATA_DIR` | directory for node state. The default is /var/lib/hivemind for root, and $XDG_STATE_HOME/hivemind or ~/.local/state/hivemind for other users. |
| `--docker-host` | string | `unix:///var/run/docker.sock` | `HIVEMIND_DOCKER_HOST` | Docker Engine endpoint |
| `--log-format` | string | `text` | `HIVEMIND_LOG_FORMAT` | log format: text or json |
| `--log-level` | string | `info` | `HIVEMIND_LOG_LEVEL` | log level: debug, info, warn or error |
| `--metrics-addr` | string | - | `HIVEMIND_METRICS_ADDR` | separate loopback address for /metrics, host:port; empty means API only |
| `--min-ready` | string | `30s` | - | how long a node must be ready before it gets new replicas; 0s means no wait |
| `--reconcile-interval` | string | `2s` | `HIVEMIND_RECONCILE_INTERVAL` | how often the agent reconciles containers and renews its lease (at most 5s) |
| `--server` | string | `http://127.0.0.1:4483` | `HIVEMIND_SERVER` | server URL for the client commands |
| `--token` | string | - | `HIVEMIND_TOKEN` | join token from hivemind join-token on the server |

### hivemind join-token

Print a join token. Run "hivemind join <server>:<cluster-port> --token <token>"
on the other machine. The token is single use, and it expires after --ttl
(default 24h, at most 168h). Its text carries the cluster CA's fingerprint, so
the joining machine only trusts this cluster's CA.

The token goes to standard output alone, so scripts can capture it. The
expiry goes to standard error. With --json, both go to standard output as JSON.

```
hivemind join-token [flags]
```

| Flag | Type | Default | Environment | Description |
|---|---|---|---|---|
| `--ca-cert` | string | - | - | PEM file of the CA certificates to trust for an https server, instead of the system's (env HIVEMIND_CA_CERT; default from the client config) |
| `--client-config` | string | - | `HIVEMIND_CLIENT_CONFIG` | client config file (env HIVEMIND_CLIENT_CONFIG; default $XDG_CONFIG_HOME/hivemind/config.yaml or ~/.config/hivemind/config.yaml) |
| `--json` | bool | - | - | print JSON instead of a table |
| `--server` | string | - | `HIVEMIND_SERVER` | server URL (env HIVEMIND_SERVER; default from the client config, then http://127.0.0.1:4483) |
| `--token` | string | - | `HIVEMIND_TOKEN` | API token (env HIVEMIND_TOKEN; default from the client config's token file) |
| `--ttl` | string | - | - | how long the token lasts, such as 1h (default 24h0m0s) |

### hivemind logs

Print the log of one replica's container, replica 0 by default. -f keeps the
stream open until Ctrl-C or until the container exits.

The replica can run on any node. The server reads a worker's replica through
that worker, which must be running and of this version: a worker that does not
answer in 10 seconds is reported by its node ID.

```
hivemind logs <app> [replica] [flags]
```

| Flag | Type | Default | Environment | Description |
|---|---|---|---|---|
| `--ca-cert` | string | - | - | PEM file of the CA certificates to trust for an https server, instead of the system's (env HIVEMIND_CA_CERT; default from the client config) |
| `--client-config` | string | - | `HIVEMIND_CLIENT_CONFIG` | client config file (env HIVEMIND_CLIENT_CONFIG; default $XDG_CONFIG_HOME/hivemind/config.yaml or ~/.config/hivemind/config.yaml) |
| `-f`, `--follow` | bool | - | - | keep printing new lines |
| `--json` | bool | - | - | print JSON instead of a table |
| `--server` | string | - | `HIVEMIND_SERVER` | server URL (env HIVEMIND_SERVER; default from the client config, then http://127.0.0.1:4483) |
| `--tail` | int | - | - | print only the last N lines (0 for all) |
| `--token` | string | - | `HIVEMIND_TOKEN` | API token (env HIVEMIND_TOKEN; default from the client config's token file) |

### hivemind node

Manage one node

```
hivemind node
```

| Command | Description |
|---|---|
| `remove` | Remove a node from the cluster for good |

#### hivemind node remove

Remove a node from the cluster for good. Stop hivemind on the node first.

The server's own node cannot be removed. A node that still renews its lease,
because hivemind runs on it, is refused unless --force is given.

On removal, the node's replicas are placed on other nodes. A replica with a
volume is not moved: its data was on the removed node. It waits with no node,
and its reason in status says so, until a deploy changes the app's spec (for
example, a new volume name) or the app is deleted.

The node's ID goes on the server's list of removed nodes. The server refuses
its certificate on the node API from then on, and never issues the ID again.
A removed worker that is still running stops with an error; its containers
keep running until they are removed by hand. To bring the machine back, join
it again with an empty data directory and a new join token.

```
hivemind node remove <id> [flags]
```

| Flag | Type | Default | Environment | Description |
|---|---|---|---|---|
| `--ca-cert` | string | - | - | PEM file of the CA certificates to trust for an https server, instead of the system's (env HIVEMIND_CA_CERT; default from the client config) |
| `--client-config` | string | - | `HIVEMIND_CLIENT_CONFIG` | client config file (env HIVEMIND_CLIENT_CONFIG; default $XDG_CONFIG_HOME/hivemind/config.yaml or ~/.config/hivemind/config.yaml) |
| `--force` | bool | - | - | remove the node even though it still renews its lease |
| `--json` | bool | - | - | print JSON instead of a table |
| `--server` | string | - | `HIVEMIND_SERVER` | server URL (env HIVEMIND_SERVER; default from the client config, then http://127.0.0.1:4483) |
| `--token` | string | - | `HIVEMIND_TOKEN` | API token (env HIVEMIND_TOKEN; default from the client config's token file) |

### hivemind nodes

List the nodes: ID, name, CPU and memory as each node's Docker daemon reports
them, the age of its lease, whether it is ready or lost, and how many replicas
are placed on it. A node is lost when its lease is more than 15 s old.

CERT EXPIRES is when the node's certificate expires, as the server recorded it
when it issued the certificate. A worker renews its certificate once two thirds
of its lifetime have passed, so a date less than four months away means the
node has not renewed. "-" means the server has no record yet.

```
hivemind nodes [flags]
```

| Flag | Type | Default | Environment | Description |
|---|---|---|---|---|
| `--ca-cert` | string | - | - | PEM file of the CA certificates to trust for an https server, instead of the system's (env HIVEMIND_CA_CERT; default from the client config) |
| `--client-config` | string | - | `HIVEMIND_CLIENT_CONFIG` | client config file (env HIVEMIND_CLIENT_CONFIG; default $XDG_CONFIG_HOME/hivemind/config.yaml or ~/.config/hivemind/config.yaml) |
| `--json` | bool | - | - | print JSON instead of a table |
| `--server` | string | - | `HIVEMIND_SERVER` | server URL (env HIVEMIND_SERVER; default from the client config, then http://127.0.0.1:4483) |
| `--token` | string | - | `HIVEMIND_TOKEN` | API token (env HIVEMIND_TOKEN; default from the client config's token file) |

### hivemind restore

Restore a backup made with hivemind backup into --data-dir, which must be
missing or empty. The directory gets mode 0700 and each file mode 0600.

restore checks the archive before it keeps anything: the format version, the
size and SHA-256 of every file, SQLite's integrity check on the store, and
that the CA key matches the CA certificate. A store at an older schema version
is migrated forward when serve opens it; a store from a newer hivemind is
refused. On any failure, the files it wrote are removed.

The backup holds the cluster CA's private key and the admin token. Delete
copies you no longer need.

Like init, restore writes a client config that points the CLI at this server
and at the restored token file, unless one exists.

To replace a lost server, restore on a machine with the same address and run
"hivemind serve --advertise <that address>". The workers reconnect without
joining again, because the CA and their certificates are unchanged. See
docs/backup.md.

```
hivemind restore <file> [flags]
```

| Flag | Type | Default | Environment | Description |
|---|---|---|---|---|
| `--client-config` | string | - | `HIVEMIND_CLIENT_CONFIG` | client config file to write (default $XDG_CONFIG_HOME/hivemind/config.yaml or ~/.config/hivemind/config.yaml; env HIVEMIND_CLIENT_CONFIG) |
| `--data-dir` | string | see description | `HIVEMIND_DATA_DIR` | the new data directory, missing or empty (env HIVEMIND_DATA_DIR; default /var/lib/hivemind for root, else ~/.local/state/hivemind). The default is /var/lib/hivemind for root, and $XDG_STATE_HOME/hivemind or ~/.local/state/hivemind for other users. |

### hivemind scale

Set an app's replica count

```
hivemind scale <app> <replicas> [flags]
```

| Flag | Type | Default | Environment | Description |
|---|---|---|---|---|
| `--ca-cert` | string | - | - | PEM file of the CA certificates to trust for an https server, instead of the system's (env HIVEMIND_CA_CERT; default from the client config) |
| `--client-config` | string | - | `HIVEMIND_CLIENT_CONFIG` | client config file (env HIVEMIND_CLIENT_CONFIG; default $XDG_CONFIG_HOME/hivemind/config.yaml or ~/.config/hivemind/config.yaml) |
| `--json` | bool | - | - | print JSON instead of a table |
| `--server` | string | - | `HIVEMIND_SERVER` | server URL (env HIVEMIND_SERVER; default from the client config, then http://127.0.0.1:4483) |
| `--token` | string | - | `HIVEMIND_TOKEN` | API token (env HIVEMIND_TOKEN; default from the client config's token file) |

### hivemind serve

Run the API server and the agent on this node. The API listens on --api-addr,
127.0.0.1:4483 by default. A non-loopback address needs --api-tls-cert and
--api-tls-key. SIGINT or SIGTERM stops the server; containers keep running.

--advertise <addr> also opens the cluster listeners, on that address only: the
node listener on --cluster-port (4484) and the join listener on the port above
it. Other machines join with hivemind join.

--role worker runs only the agent, for a machine that joined with hivemind
join. It has no API listener. --role server is the default.

GET /metrics on the API serves Prometheus metrics and needs the admin token.
--metrics-addr also serves it, and nothing else, on a separate loopback
address; that listener needs the token too. Logs go to stderr at
--log-level, as text or with --log-format json. Tokens, cookies, CSRF tokens,
private keys and env values are not logged.

```
hivemind serve [flags]
```

| Flag | Type | Default | Environment | Description |
|---|---|---|---|---|
| `--advertise` | string | - | `HIVEMIND_ADVERTISE` | IP or host name other nodes use to reach this server; serve opens the cluster listeners on it |
| `--api-addr` | string | `127.0.0.1:4483` | `HIVEMIND_API_ADDR` | address of the API, host:port |
| `--api-tls-cert` | string | - | `HIVEMIND_API_TLS_CERT` | TLS certificate for the API (PEM) |
| `--api-tls-key` | string | - | `HIVEMIND_API_TLS_KEY` | TLS private key for the API (PEM) |
| `--cluster-port` | string | `4484` | `HIVEMIND_CLUSTER_PORT` | port of the node-to-node listener; the join listener uses the port above it |
| `--config` | string | - | `HIVEMIND_CONFIG` | YAML config file (default <data-dir>/hivemind.yaml) |
| `--data-dir` | string | see description | `HIVEMIND_DATA_DIR` | directory for node state. The default is /var/lib/hivemind for root, and $XDG_STATE_HOME/hivemind or ~/.local/state/hivemind for other users. |
| `--docker-host` | string | `unix:///var/run/docker.sock` | `HIVEMIND_DOCKER_HOST` | Docker Engine endpoint |
| `--log-format` | string | `text` | `HIVEMIND_LOG_FORMAT` | log format: text or json |
| `--log-level` | string | `info` | `HIVEMIND_LOG_LEVEL` | log level: debug, info, warn or error |
| `--metrics-addr` | string | - | `HIVEMIND_METRICS_ADDR` | separate loopback address for /metrics, host:port; empty means API only |
| `--min-ready` | string | `30s` | - | how long a node must be ready before it gets new replicas; 0s means no wait |
| `--reconcile-interval` | string | `2s` | `HIVEMIND_RECONCILE_INTERVAL` | how often the agent reconciles containers and renews its lease (at most 5s) |
| `--role` | string | `server` | - | server (API, scheduler and agent) or worker (agent only, after hivemind join) |
| `--server` | string | `http://127.0.0.1:4483` | `HIVEMIND_SERVER` | server URL for the client commands |
| `--token` | string | - | `HIVEMIND_TOKEN` | API token for the client commands (not logged; never read from the config file) |

### hivemind status

Show apps, their replicas and container state

```
hivemind status [app] [flags]
```

| Flag | Type | Default | Environment | Description |
|---|---|---|---|---|
| `--ca-cert` | string | - | - | PEM file of the CA certificates to trust for an https server, instead of the system's (env HIVEMIND_CA_CERT; default from the client config) |
| `--client-config` | string | - | `HIVEMIND_CLIENT_CONFIG` | client config file (env HIVEMIND_CLIENT_CONFIG; default $XDG_CONFIG_HOME/hivemind/config.yaml or ~/.config/hivemind/config.yaml) |
| `--json` | bool | - | - | print JSON instead of a table |
| `--server` | string | - | `HIVEMIND_SERVER` | server URL (env HIVEMIND_SERVER; default from the client config, then http://127.0.0.1:4483) |
| `--token` | string | - | `HIVEMIND_TOKEN` | API token (env HIVEMIND_TOKEN; default from the client config's token file) |

### hivemind version

Print the version

```
hivemind version
```

