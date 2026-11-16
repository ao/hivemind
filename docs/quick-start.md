# Quick start

This deploys nginx on one Linux machine, using only loopback addresses. The commands are the ones in the README's quick start. `make quickstart-check` runs them against a node in Docker-in-Docker, and the check passes on this build.

## Before you start

- Linux, amd64 or arm64.
- Docker Engine 25 or newer, running. Your user needs access to `/var/run/docker.sock`, or you run hivemind as root. Without it, `serve` and `join` stop with `cannot reach Docker at unix:///var/run/docker.sock: ... permission denied; this user may not open the socket: add it to the docker group (sudo usermod -aG docker $USER, then log in again), or run hivemind as root`.
- The `hivemind` binary on your PATH. See the install section of the [README](../README.md).

## 1. Create the cluster

```sh
hivemind init
```

`init` creates the data directory and the cluster record. It prints the admin token once:

```
Created a cluster in /var/lib/hivemind (node node-ea6188a5133b).

Admin token, shown only this once:

    hm_...

It is also saved in /var/lib/hivemind/admin.token (mode 0600). The server keeps only its SHA-256 digest.
The CLI on this machine reads the server address and token file from /root/.config/hivemind/config.yaml.

Start the node with `hivemind serve`. From another shell on this machine, use

    export HIVEMIND_SERVER=http://127.0.0.1:4483
    export HIVEMIND_TOKEN=<the token above>

The API listens on 127.0.0.1:4483, a loopback address, so the CLI on another machine cannot reach it. To use it from there, serve the API on a private address with TLS: --api-addr, --api-tls-cert and --api-tls-key (see docs/api.md#tls).
```

The paths depend on who runs `init`. On Linux, a user other than root gets `~/.local/state/hivemind` and `~/.config/hivemind/config.yaml`. Setting `--data-dir` or `HIVEMIND_DATA_DIR` moves the data directory.

You do not need to copy the token for the CLI on this machine. `init` wrote a client config that names the server and the token file. As another user on this machine, or to use `curl`, pass the token yourself:

```sh
export HIVEMIND_SERVER=http://127.0.0.1:4483
export HIVEMIND_TOKEN=hm_...
```

The API listens on loopback only, so the CLI on another machine cannot reach it at that address. To use the CLI from another machine, serve the API on a non-loopback `--api-addr` with `--api-tls-cert` and `--api-tls-key`, and give that CLI the certificate with `--ca-cert`; see [the API](api.md#tls). `init` takes the same flags as `serve`, and writes the ones it is given to `<data-dir>/hivemind.yaml`, so a plain `hivemind serve` uses them.

`init` refuses to run again on the same data directory, so a second run cannot replace the token.

## 2. Start the node

In a terminal that stays open:

```sh
hivemind serve
```

The server listens on `127.0.0.1:4483`, and the agent starts with it. Logs go to standard error, one line for each request and each container action. Stop the node with Ctrl-C. Stopping it does not stop the containers, but nothing restarts or changes them while it is down.

## 3. Deploy an app

In a second terminal:

```sh
hivemind deploy --name web --image nginx:alpine --port 127.0.0.1:8080:80
```

```
web: created, generation 1, 1 replica
```

`deploy` returns once the server has stored the spec. The agent pulls the image and starts the container on its next pass, which the server triggers at once. The first pull can take a while.

`--port 127.0.0.1:8080:80` publishes container port 80 on host address 127.0.0.1, port 8080. The address is optional, and it defaults to 127.0.0.1.

## 4. Check the status

```sh
hivemind status
```

```
APP  IMAGE         GENERATION  DESIRED  RUNNING  READY
web  nginx:alpine  1           1        1        1

APP  REPLICA  NODE               GENERATION  STATE    HEALTH  RESTARTS  CONTAINER     DETAIL
web  0        node-ea6188a5133b  1           running  -       0         fe8344510103  -
```

`DESIRED` is the replica count the spec asks for, `RUNNING` counts the replicas whose container is running, and `READY` those of them that pass their health check, or have none, and are not crash-looping. A replica that runs but fails its health check counts in `RUNNING` and not in `READY`. During a rollout, a copy at the old generation counts until it is replaced. In the second table, `STATE` is the container's state from the engine, `HEALTH` is the health check's result (`-` when the app has none), `RESTARTS` counts the agent's restarts of the container, and `DETAIL` shows the reason a replica is pending or cannot run on its node, or else the last error.

Until the image is pulled, `STATE` is `pending` and `CONTAINER` is `-`. Run `status` again. When `STATE` is `running`, the port answers:

```sh
curl -fsS http://127.0.0.1:8080/ > /dev/null
```

## 5. Read the logs

```sh
hivemind logs web
```

This prints the log of replica 0's container. Add `-f` to keep the stream open until Ctrl-C, `--tail 20` for the last 20 lines, and a replica number after the app name for another replica.

## 6. Scale an app

`web` publishes host port 8080, and two replicas cannot both publish it. So the scale step uses an app without a port:

```sh
hivemind deploy --name worker --image nginx:alpine
hivemind scale worker 2
hivemind status worker
```

```
worker: created, generation 1, 1 replica
worker: 2 replicas
```

```
APP     IMAGE         GENERATION  DESIRED  RUNNING  READY
worker  nginx:alpine  1           2        2        2

APP     REPLICA  NODE               GENERATION  STATE    HEALTH  RESTARTS  CONTAINER     DETAIL
worker  0        node-ea6188a5133b  1           running  -       0         425040ec2a1e  -
worker  1        node-ea6188a5133b  1           running  -       0         b49469bbab0e  -
```

Scaling does not change the generation. Scaling down removes the highest-numbered replicas.

## 7. Change an app

Deploy the same name again with a different spec to change it. A change to the image, the environment, the ports or the other settings is a rolling update: replicas move to the new spec one at a time. To change `web`'s environment, for example:

```sh
hivemind deploy --name web --image nginx:alpine --port 127.0.0.1:8080:80 --env MODE=prod
hivemind status web
```

While the rollout runs, `status` prints a line such as `web: rolling out generation 2`. On one node, each replica is down while it is replaced, so `web` is unavailable for a few seconds at a time. If a replica does not become ready, the rollout pauses and `status` shows the reason. Deploying the old spec again rolls back.

## 8. Delete the apps

```sh
hivemind delete worker
hivemind delete web
```

```
worker: deleting; its containers stop on the next reconcile pass
```

The app is hidden at once. Its containers stop and are removed on the next pass, and the app's rows go once no node reports a container. The named volumes of an app are kept. This quick start has none.

## Next

- [Concepts](concepts.md) explains generations, reconcile, health checks, restarts, placement and the security defaults.
- [Add a machine](../README.md#add-a-machine) joins a worker to this server.
- [Security](security.md) lists every listener and file.
- [CLI reference](cli.md) lists every flag.
- [HTTP API](api.md) describes the routes that the CLI calls.
- The [README's limits](../README.md#limits) say what the current build does not do.
