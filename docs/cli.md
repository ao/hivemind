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
| `delete` | Delete an app and stop its containers |
| `deploy` | Create or update an app |
| `init` | Create a cluster on this machine |
| `join` | Join another node's cluster (not implemented in this build) |
| `logs` | Print a replica's container log |
| `scale` | Set an app's replica count |
| `serve` | Run the server and the agent on this node |
| `status` | Show apps, their replicas and container state |
| `version` | Print the version |

### hivemind delete

Delete an app and stop its containers

```
hivemind delete <app> [flags]
```

| Flag | Type | Default | Environment | Description |
|---|---|---|---|---|
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
at the token file. It refuses to run if a cluster already exists.

```
hivemind init [flags]
```

| Flag | Type | Default | Environment | Description |
|---|---|---|---|---|
| `--advertise` | string | - | `HIVEMIND_ADVERTISE` | IP or host name other nodes would use to reach this node; checked but not used in this build, which starts no node listener |
| `--api-addr` | string | `127.0.0.1:4483` | `HIVEMIND_API_ADDR` | address of the API, host:port |
| `--api-tls-cert` | string | - | `HIVEMIND_API_TLS_CERT` | TLS certificate for the API (PEM) |
| `--api-tls-key` | string | - | `HIVEMIND_API_TLS_KEY` | TLS private key for the API (PEM) |
| `--client-config` | string | - | `HIVEMIND_CLIENT_CONFIG` | client config file to write (default $XDG_CONFIG_HOME/hivemind/config.yaml or ~/.config/hivemind/config.yaml; env HIVEMIND_CLIENT_CONFIG) |
| `--cluster-port` | string | `4484` | `HIVEMIND_CLUSTER_PORT` | port of the node-to-node listener; not used in this build |
| `--config` | string | - | `HIVEMIND_CONFIG` | YAML config file (default <data-dir>/hivemind.yaml) |
| `--data-dir` | string | see description | `HIVEMIND_DATA_DIR` | directory for node state. The default is /var/lib/hivemind for root, and $XDG_STATE_HOME/hivemind or ~/.local/state/hivemind for other users. |
| `--docker-host` | string | `unix:///var/run/docker.sock` | `HIVEMIND_DOCKER_HOST` | Docker Engine endpoint |
| `--log-format` | string | `text` | `HIVEMIND_LOG_FORMAT` | log format: text or json |
| `--log-level` | string | `info` | `HIVEMIND_LOG_LEVEL` | log level: debug, info, warn or error |
| `--metrics-addr` | string | - | `HIVEMIND_METRICS_ADDR` | separate loopback address for /metrics, host:port; empty means API only |
| `--reconcile-interval` | string | `2s` | `HIVEMIND_RECONCILE_INTERVAL` | how often the agent reconciles containers |
| `--server` | string | `http://127.0.0.1:4483` | `HIVEMIND_SERVER` | server URL for the client commands |
| `--token` | string | - | `HIVEMIND_TOKEN` | API token for the client commands (not logged) |

### hivemind join

Join another node's cluster (not implemented in this build)

```
hivemind join
```

### hivemind logs

Print the log of one replica's container, replica 0 by default. -f keeps the stream open until Ctrl-C.

```
hivemind logs <app> [replica] [flags]
```

| Flag | Type | Default | Environment | Description |
|---|---|---|---|---|
| `--client-config` | string | - | `HIVEMIND_CLIENT_CONFIG` | client config file (env HIVEMIND_CLIENT_CONFIG; default $XDG_CONFIG_HOME/hivemind/config.yaml or ~/.config/hivemind/config.yaml) |
| `-f`, `--follow` | bool | - | - | keep printing new lines |
| `--json` | bool | - | - | print JSON instead of a table |
| `--server` | string | - | `HIVEMIND_SERVER` | server URL (env HIVEMIND_SERVER; default from the client config, then http://127.0.0.1:4483) |
| `--tail` | int | - | - | print only the last N lines (0 for all) |
| `--token` | string | - | `HIVEMIND_TOKEN` | API token (env HIVEMIND_TOKEN; default from the client config's token file) |

### hivemind scale

Set an app's replica count

```
hivemind scale <app> <replicas> [flags]
```

| Flag | Type | Default | Environment | Description |
|---|---|---|---|---|
| `--client-config` | string | - | `HIVEMIND_CLIENT_CONFIG` | client config file (env HIVEMIND_CLIENT_CONFIG; default $XDG_CONFIG_HOME/hivemind/config.yaml or ~/.config/hivemind/config.yaml) |
| `--json` | bool | - | - | print JSON instead of a table |
| `--server` | string | - | `HIVEMIND_SERVER` | server URL (env HIVEMIND_SERVER; default from the client config, then http://127.0.0.1:4483) |
| `--token` | string | - | `HIVEMIND_TOKEN` | API token (env HIVEMIND_TOKEN; default from the client config's token file) |

### hivemind serve

Run the API server and the agent on this node. The API listens on --api-addr,
127.0.0.1:4483 by default. A non-loopback address needs --api-tls-cert and
--api-tls-key. SIGINT or SIGTERM stops the server; containers keep running.

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
| `--advertise` | string | - | `HIVEMIND_ADVERTISE` | IP or host name other nodes would use to reach this node; checked but not used in this build, which starts no node listener |
| `--api-addr` | string | `127.0.0.1:4483` | `HIVEMIND_API_ADDR` | address of the API, host:port |
| `--api-tls-cert` | string | - | `HIVEMIND_API_TLS_CERT` | TLS certificate for the API (PEM) |
| `--api-tls-key` | string | - | `HIVEMIND_API_TLS_KEY` | TLS private key for the API (PEM) |
| `--cluster-port` | string | `4484` | `HIVEMIND_CLUSTER_PORT` | port of the node-to-node listener; not used in this build |
| `--config` | string | - | `HIVEMIND_CONFIG` | YAML config file (default <data-dir>/hivemind.yaml) |
| `--data-dir` | string | see description | `HIVEMIND_DATA_DIR` | directory for node state. The default is /var/lib/hivemind for root, and $XDG_STATE_HOME/hivemind or ~/.local/state/hivemind for other users. |
| `--docker-host` | string | `unix:///var/run/docker.sock` | `HIVEMIND_DOCKER_HOST` | Docker Engine endpoint |
| `--log-format` | string | `text` | `HIVEMIND_LOG_FORMAT` | log format: text or json |
| `--log-level` | string | `info` | `HIVEMIND_LOG_LEVEL` | log level: debug, info, warn or error |
| `--metrics-addr` | string | - | `HIVEMIND_METRICS_ADDR` | separate loopback address for /metrics, host:port; empty means API only |
| `--reconcile-interval` | string | `2s` | `HIVEMIND_RECONCILE_INTERVAL` | how often the agent reconciles containers |
| `--server` | string | `http://127.0.0.1:4483` | `HIVEMIND_SERVER` | server URL for the client commands |
| `--token` | string | - | `HIVEMIND_TOKEN` | API token for the client commands (not logged) |

### hivemind status

Show apps, their replicas and container state

```
hivemind status [app] [flags]
```

| Flag | Type | Default | Environment | Description |
|---|---|---|---|---|
| `--client-config` | string | - | `HIVEMIND_CLIENT_CONFIG` | client config file (env HIVEMIND_CLIENT_CONFIG; default $XDG_CONFIG_HOME/hivemind/config.yaml or ~/.config/hivemind/config.yaml) |
| `--json` | bool | - | - | print JSON instead of a table |
| `--server` | string | - | `HIVEMIND_SERVER` | server URL (env HIVEMIND_SERVER; default from the client config, then http://127.0.0.1:4483) |
| `--token` | string | - | `HIVEMIND_TOKEN` | API token (env HIVEMIND_TOKEN; default from the client config's token file) |

### hivemind version

Print the version

```
hivemind version
```

