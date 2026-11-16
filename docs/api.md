# HTTP API

The API is served by `hivemind serve`. The default address is `http://127.0.0.1:4483`. The CLI uses this API, and so can scripts. Every route is JSON, except the log stream and `/metrics`.

There is no web UI in this build. The API takes the bearer token, or a session cookie from [login](#sessions) for a browser.

## Authentication

Send the admin token as one bearer header:

```
Authorization: Bearer hm_...
```

The scheme is case-insensitive. A request with more than one `Authorization` header is refused. The token is compared in constant time, against its SHA-256 digest.

`GET /healthz` is the only route that needs no token. The match is exact: a `HEAD` request, a `POST`, or a path with a trailing slash needs the token.

A request without a valid token gets `401` with the body `{"error":"unauthorized"}` and the header `WWW-Authenticate: Bearer realm="hivemind"`. The check runs before routing, so an unknown path without a token also gets `401`, not `404`.

Every reply to a request that passes the check carries the server's version in the `Hivemind-Version` header, such as `Hivemind-Version: v2.2.0`; `hivemind version` reads it from `GET /v1/status`. `GET /healthz` and a `401` do not carry it. A server older than 2.2 sends no such header.

The token is in `<data-dir>/admin.token`. `init` prints it once.

```sh
curl -H "Authorization: Bearer $(cat /var/lib/hivemind/admin.token)" http://127.0.0.1:4483/v1/status
```

## TLS

With `--api-tls-cert` and `--api-tls-key`, the API is served over TLS 1.2 or newer. The CLI checks the server's certificate against the system's roots, or, with `--ca-cert <file>` or `HIVEMIND_CA_CERT`, against the certificates in that PEM file and nothing else. It also checks that the certificate names the host in the server URL. There is no option to skip either check.

A certificate in the `--ca-cert` file is trusted as it is, so the file can hold the CA that signed the server's certificate, or the server's certificate itself. `init` with `--api-tls-cert` writes that path into the client config as `ca-cert`, so the CLI on the server's machine trusts it.

```sh
hivemind status --server https://10.0.0.5:4483 --ca-cert ca.crt
```

The CLI's minimum is TLS 1.2, and it offers 1.3 first, so it gets 1.3 from `hivemind serve`. The 1.2 floor lets it reach an API behind a proxy that ends TLS at 1.2. Go's TLS 1.2 cipher suites are all ECDHE with AEAD ciphers, so 1.2 allows no weak cipher.

## Sessions

A browser can trade the token for a session cookie once, and send the cookie after that.

`POST /v1/session` with the bearer header and no body answers `200` with `{"csrf_token": "..."}`, and sets the cookie `hivemind_session`: `HttpOnly`, `SameSite=Strict`, `Path=/`, and `Secure` when the API is served over TLS. A session lasts 12 hours from login. A restart of the server ends every session. Only the bearer token logs in: a request with the cookie and no bearer header gets `401`.

With the cookie, every route works as with the token, with one more rule: `POST`, `PUT`, `PATCH` and `DELETE` need the CSRF token, in the `X-CSRF-Token` header or a `csrf_token` form field. Without it, or with a wrong one, the answer is `403` with `{"error":"csrf_token_invalid"}`, and nothing changes. A request that sends an `Authorization` header is checked by that header alone. A cookie beside a bad header does not help it.

`DELETE /v1/session` ends the session and clears the cookie, with `204`. With the cookie, it needs the CSRF token like any other unsafe request.

Failed logins are counted per source address. After 5 in 10 minutes, `POST /v1/session` from that address gets `429`, with a `Retry-After` header, until the window ends, whatever token it sends. Other routes are not limited: the token is 256 random bits, and API clients send it on every request. Behind a proxy, every client has the proxy's address and shares one count.

## Errors

Every error is a JSON object with one field:

```json
{"error": "app \"web\": not found"}
```

| Status | When |
|---|---|
| 400 | Invalid JSON, an unknown field, an empty body, an invalid app spec, an invalid name or query value |
| 401 | No valid token or session |
| 403 | A cookie request to an unsafe method without the session's CSRF token: `{"error":"csrf_token_invalid"}` |
| 404 | No such app or node, no drain of the node, or no container for the replica on its node. Unknown routes give `{"error":"not found"}` |
| 405 | Wrong method for a known path: `{"error":"method not allowed"}` |
| 409 | Removing the server's own node, or a node that still renews its lease without `force=true`. An ineligible node that runs nothing needs no `force`. A put whose `routes` name a host that another app routes: `{"error":"conflict: host www.example.com is routed by app web; remove it there first"}` |
| 413 | Request body over 1 MiB |
| 429 | Too many failed logins from this address, or too many log streams open to one node |
| 500 | Unexpected failure. The message is `internal error`, and the details go to the server log |
| 502 | A worker could not read a log, or broke the stream before it started |
| 503 | The container engine cannot be reached: `the container engine is unavailable`. For logs, also a lost node, the engine on the replica's node, or a server without `--advertise` |
| 504 | A worker did not answer a log request in 10 seconds |
| 507 | The server's disk is full: `the server's disk is full, so the change was not saved: ...`. Nothing of the change is stored, and reads still work |

## Limits

- Request bodies are at most 1 MiB (1048576 bytes).
- Request headers are at most 64 KiB.
- The server gives 10 seconds to read the headers, 30 seconds to read a request, 60 seconds to write a response, and holds an idle connection for 120 seconds. A followed log stream lifts the write deadline, and it ends when the server shuts down.
- `replica` in a logs request is 0 to 99. `tail` is 0 to 100000, and 0 means all lines.

## Join tokens

`POST /v1/join-tokens` issues a token for one machine to join the cluster with `hivemind join`. It needs the admin token like every other route. It needs the cluster CA that `init` creates. Without the CA, the route answers 503 and says to run `hivemind init`. A token can be used only while the server runs with `--advertise`, which opens the join listener.

The body is optional. `ttl` is a Go duration such as `"1h"`. It defaults to `24h`, and it must be above zero and at most `168h`. Any other value gets 400.

```json
{"token": "hivemind-join-v1.<64 hex digits>.<43 characters>", "expires_at": "2026-11-14T10:00:00Z"}
```

The token is shown once. The server keeps a SHA-256 hash of its secret, and a token works once, before it expires. The token carries the CA fingerprint, so the joining machine trusts only this cluster's CA. `hivemind join-token` calls this route and prints the token. See [concepts](concepts.md#joining-a-machine) for the join flow.

## Nodes

`GET /v1/nodes` lists every node, sorted by ID:

```json
[{"id": "node-<12 hex digits>", "name": "host-a", "cpu": 4, "memory": 8254390272,
  "last_lease": "2026-11-13T10:00:00Z", "lease_age_seconds": 1.2, "state": "ready",
  "server": true, "replicas": 2, "cert_not_after": "2027-11-14T10:00:00Z", "eligible": true,
  "address": "10.0.0.5", "version": "v2.2.0"}]
```

`address` is the node's private address, where its routed replicas publish their ports. It is absent for a node that reported none, such as a worker older than 2.2, which takes no routed replicas. `version` is the hivemind version the node sends with each request to the server, and the server's own for the server's node. It is absent for a node that sends none, which a version older than 2.2 does. `cpu` and `memory` (bytes) are what the node's Docker daemon reports. `state` is `ready` while the lease is at most 15 s old, and `lost` after that. In the first 15 s after `serve` starts, a node that has not reported to it yet is `waiting`: the server counts its lease from its own start, so it is not lost yet. `server` marks the node that runs the server. `replicas` counts the replicas placed on the node that should run. `cert_not_after` is when the node's certificate expires, as the server recorded it when it issued the certificate or saw the node present it. It is absent when there is no record, as for a server without `--advertise`. `cert_not_renewing` is `true`, and present only then, when that certificate is more than a day past its renewal time, two thirds of its lifetime: the node is not renewing it, as a 2.0.0 worker never does. `eligible` is false for a node that takes no new replicas. A node with a drain, in progress or finished, also has a `drain` object, described below.

`POST /v1/nodes/{id}/eligibility` with `{"eligible": false}` makes the node ineligible: it takes no new replicas, and keeps the ones it runs. `{"eligible": true}` makes it eligible again, and on a node with a drain in progress it cancels the drain: copies still starting on other nodes are removed, and the replicas stay on the node. The reply:

```json
{"id": "node-<12 hex digits>", "name": "host-b", "eligible": true, "drain_cancelled": true}
```

`POST /v1/nodes/{id}/drain` makes the node ineligible and starts a drain of it, and takes its first step before it replies. The body is a JSON object with an optional `deadline`, a Go duration such as `"10m"`, above 0 and at most `168h`. Without one, a new drain gets 10 minutes. On a node that is already draining, a `deadline` sets a new deadline from now, and `{}` changes nothing. A finished drain is replaced by a new one. `GET /v1/nodes/{id}/drain` reads the drain, and answers `404` for a node without one. Both answer with the drain:

```json
{"node_id": "node-<12 hex digits>", "state": "draining",
 "started_at": "2026-11-14T10:00:00Z", "deadline": "2026-11-14T10:10:00Z",
 "moved": 2, "stopped": 0, "lost": 0, "remaining": 2, "old_containers": 1,
 "moving": [{"app": "spread", "index": 1, "to": "node-<12 hex digits>", "since": "2026-11-14T10:00:04Z"}],
 "blocked": [{"app": "web", "index": 3, "reason": "host port 127.0.0.1:8080/tcp is in use on every other eligible node"}]}
```

- `state` is `draining` or `done`. A finished drain has `finished_at`.
- `moved` counts the replicas that moved to another node, and `stopped` those that lost the node at the deadline and were placed again.
- `lost` counts the replicas that were placed again because the node itself was lost during the drain: its lease went 15 s without a renewal. `lost_at` is when that first happened; it is absent when the node was not lost.
- `remaining` counts the replicas still on the node that should run.
- `old_containers` counts the containers the node still reports for replicas that have left it. Its agent removes them on its next pass.
- `moving` lists the moves in flight: a copy of the replica is starting on `to`.
- `blocked` lists the replicas that did not move at the drain's last step, and why.

The server's own node can be made ineligible and drained. Its replicas move to workers, and the server keeps running. What a drain does is in [concepts](concepts.md#draining-a-node).

`DELETE /v1/nodes/{id}` removes a node for good. It refuses, with `409`, the server's own node, and a node whose lease is current, unless the query has `force=true` or the node is ineligible and runs nothing, as after a drain. An unknown node, or one already removed, gets `404`. On success:

```json
{"id": "node-<12 hex digits>", "name": "host-b", "rescheduled": 3, "held": 1}
```

`rescheduled` counts the replicas that lost the node and are placed again. `held` counts the replicas with a volume, which wait with no node because their data was on the removed node. What removal does is in [concepts](concepts.md#removing-a-node), and what it does to the node's certificate is in [security](security.md#revocation-by-removal).

## Backup

`POST /v1/backup` streams a backup of the server's data directory as `application/x-tar`: the store, the cluster CA certificate and private key, and the admin token file. The format is in [backup](backup.md#the-file). `hivemind backup --api` calls this route.

The archive holds the CA private key, so the server sends it only over TLS, or to a client on loopback. A plain-HTTP request from another address gets `403`. The API listens on loopback unless it has TLS, so that refusal matters only behind a proxy. The store is copied before the reply starts, and a failure there is a `500`. A failure after the archive has started aborts the connection, so a client sees a broken transfer, not a short file that looks complete. Check the archive before you keep it; `hivemind backup` does.

## Routing table

`GET /v1/routes` returns the routing table that every router serves, sorted by host:

```json
{"routes": [{"host": "www.example.com", "app": "web", "port": 80,
  "endpoints": ["10.0.0.5:32768", "10.0.0.6:32771"]}]}
```

`port` is the container port the route names. `endpoints` are the `address:port` of each replica that takes the host's traffic now: its container is Ready, it is the copy the replica (or its rollout surge) is assigned, and its node's lease is current. A host whose app has no such replica has `"endpoints": []`, and a router answers it with `503`. See [routing](routing.md).

## Registries

`PUT /v1/registries/{host}` stores the credential that every pull of an image from `{host}` sends, or replaces the one stored:

```json
{"username": "octocat", "password": "<token>"}
```

The reply is the stored record, without the password:

```json
{"host": "ghcr.io", "username": "octocat", "created_at": "2026-11-16T10:00:00Z", "updated_at": "2026-11-16T10:00:00Z"}
```

`{host}` is a registry as an image reference names it: a host name or IPv4 address with an optional port, or `localhost`, such as `ghcr.io` or `registry.example.com:5000`. It is stored in lower case, and `index.docker.io` and `registry-1.docker.io` are stored as `docker.io`. A host with a scheme or a path, a port outside 1 to 65535, or a name Docker reads as a Docker Hub namespace (no dot, no port, not `localhost`) gets `400`. The username must be 1 to 256 bytes with no colon, and the password 1 to 16,384 bytes; both must be UTF-8 without control characters, or the request gets `400`. A 101st registry gets `409`. The credential is not checked with the registry.

`GET /v1/registries` lists the stored credentials by host, in the same form, as `{"registries": [...]}`. `DELETE /v1/registries/{host}` removes one and answers `204`; a host with none gets `404`. No route returns a password.

A change affects the next pull only; running containers keep running. How a credential is used, and who gets it, is in [registries](registries.md).

## Node API (internal)

Workers talk to the server over a second API, on the cluster port that `serve --advertise` opens. It has routes for a worker to join, register, read its assignments, report its containers, renew its certificate, take and answer log requests, read the routing table, and use the routers' shared TLS storage. It is internal. Its routes, bodies and errors may change in any release, so this document does not describe them, except that a removed node gets `403` on every one of them, and a certificate that has expired by the server's clock gets `401`. The listeners and the trust model are in [security](security.md).

## Routes

| Method and path | Body | Success | Purpose |
|---|---|---|---|
| `GET /healthz` | none | 200 `{"status":"ok"}` | Liveness. No token |
| `GET /v1/apps` | none | 200, a list of apps | List apps |
| `POST /v1/apps` | app spec | 201 when created, 200 when updated, with `PutResult` | Create or update the app named in the body |
| `GET /v1/apps/{name}` | none | 200, `AppView` | Read one app |
| `PUT /v1/apps/{name}` | app spec | 201 or 200, with `PutResult` | Create or update the app at `{name}`. A `name` in the body must match. The query may name an origin, as `origin=compose:shop` (at most 128 printable characters): an app that exists with another origin, or with none, is refused with `409` unless the query also has `replace=true`, and the app then takes the origin. Without `origin`, the app keeps the one it has |
| `DELETE /v1/apps/{name}` | none | 202 `{"name":"...","status":"deleting"}` | Delete the app |
| `POST /v1/apps/{name}/scale` | `{"replicas": N}` | 200, `AppView` | Set the replica count, 1 to 100 |
| `GET /v1/apps/{name}/status` | none | 200, `AppStatus` | Replicas of one app |
| `GET /v1/apps/{name}/logs` | none | 200, `text/plain` | Log of one replica's container |
| `GET /v1/status` | none | 200, a list of `AppStatus` | Replicas of every app |
| `POST /v1/session` | none | 200, `{"csrf_token"}`, and the session cookie | Log in. See [sessions](#sessions) |
| `DELETE /v1/session` | none | 204 | Log out |
| `POST /v1/join-tokens` | `{"ttl": "1h"}`, optional | 201, `{"token", "expires_at"}` | Issue a token that lets one machine join. See [join tokens](#join-tokens) |
| `GET /v1/nodes` | none | 200, a list of `NodeView` | List the nodes. See [nodes](#nodes) |
| `DELETE /v1/nodes/{id}` | none. `?force=true` removes a node that still renews its lease | 200, `RemoveNodeReply` | Remove a node for good |
| `POST /v1/nodes/{id}/eligibility` | `{"eligible": false}` or `true` | 200, `{"id", "name", "eligible", "drain_cancelled"}` | Stop or let a node take new replicas. See [nodes](#nodes) |
| `POST /v1/nodes/{id}/drain` | `{"deadline": "10m"}`, or `{}` | 200, the drain | Drain a node, or set a new deadline on its drain |
| `GET /v1/nodes/{id}/drain` | none | 200, the drain | Read a node's drain |
| `GET /v1/routes` | none | 200, `{"routes": [...]}` | The routing table. See [routing table](#routing-table) |
| `GET /v1/registries` | none | 200, `{"registries": [...]}` | The stored registry credentials, without passwords. See [registries](#registries) |
| `PUT /v1/registries/{host}` | `{"username", "password"}` | 200, the record without the password | Store or replace a registry credential |
| `DELETE /v1/registries/{host}` | none | 204 | Remove a registry credential |
| `POST /v1/backup` | none | 200, `application/x-tar` | A backup of the data directory, CA private key included. TLS or loopback only. See [backup](#backup) |
| `GET /metrics` | none | 200, Prometheus text | Metrics |

An app that has been deleted is treated as missing by every route, even before its containers are gone.

A write returns once the store has the change. The containers follow on the next reconcile pass, which the server triggers at once. Poll `/v1/apps/{name}/status` to see the replicas run.

A write that changes the spec other than the replica count starts a rollout, which moves the replicas one at a time. The write returns before any replica has moved. Watch the `rollout` field in `/v1/apps/{name}/status` until it disappears.

## Objects

`AppView` is what the app routes return:

```json
{
  "name": "web",
  "generation": 1,
  "replicas": 2,
  "spec": { "...": "the app spec, as stored" },
  "created_at": "2026-11-13T10:00:00Z",
  "updated_at": "2026-11-13T10:00:00Z",
  "origin": "compose:shop"
}
```

`origin` is what deployed the app, as the `origin` of its put named it; `hivemind compose deploy` sends `compose:<project>`. It is absent for an app deployed without one.

`PutResult` wraps it: `{"app": AppView, "created": true}`.

`AppStatus` is one app's replicas:

```json
{
  "name": "web",
  "image": "nginx:alpine",
  "generation": 1,
  "desired": 2,
  "running": 2,
  "ready": 2,
  "replicas": [
    {"index": 0, "node_id": "node-<12 hex digits>", "generation": 1, "desired": "running",
     "state": "running", "container_id": "<the engine's 64-character ID>", "restarts": 0, "ready": true}
  ]
}
```

The fields of each replica are `index`, `node_id`, `generation`, `desired` (`running` or `removed`), `pending_reason` (only when set), `state`, `container_id` (only when set), `error` (only when set), `restarts`, `health` (`starting`, `healthy` or `unhealthy`; only when the app has a health check), `ready`, `crash_looping` (only when set) and `next_restart` (RFC 3339, only while a restart is scheduled). `state` is the engine's state of the container: `created`, `running`, `paused`, `restarting`, `exited`, `dead` or `removing`. A replica with no container yet is `pending`. If the agent could not list containers, it is `unknown`. It is also `unknown`, with the container ID last reported, when that report was made before `serve` started, as after a restart or a restore, or when the node that made it is lost, as a volume replica held on its node is, until the node reports again. Such a replica does not count as running or ready, in the app status or in `hivemind_replicas_running` and `hivemind_replicas_ready`.

`running` counts the replicas that should run whose container is running, at the generation the replica is at, so during a rollout a copy at the old generation counts until it is replaced. `ready` counts those of them that are ready to take traffic: passing their health check, or with none, and not crash-looping. A container that runs but fails its health check counts in `running` and not in `ready`.

While a rollout is in progress, or after it has paused, the app status has a `rollout` object. It is omitted otherwise:

```json
"rollout": {"target": 2, "state": "in_progress", "reason": "", "moving": 0}
```

- `target` is the generation the rollout moves the replicas to.
- `state` is `in_progress` or `paused`. A finished rollout is not reported.
- `reason` is set when the rollout is paused. It is a sentence such as `rollout to generation 3 stopped: replica 0 is not ready 1m0s after its container was created`, or `... replica 0 is not ready after 1m0s` for a copy whose container never appeared.
- `moving` is the index of the replica being moved, or -1 between moves.

A paused rollout stays paused until a spec with another template is deployed. Rolling back is deploying the old spec.

### App spec

The spec is the JSON form of the YAML file that `deploy -f` reads. Unknown fields are errors.

| Field | Type | Notes |
|---|---|---|
| `name` | string | Required. 1 to 32 characters: lowercase letters, digits and dashes, starting and ending with a letter or digit |
| `image` | string | Required. The spec keeps the text you wrote. The engine gets the normalised form, so `nginx:alpine` runs as `docker.io/library/nginx:alpine` |
| `replicas` | integer | 1 to 100. Defaults to 1 |
| `command` | list of strings | Up to 64 arguments, each up to 4096 bytes |
| `env` | object | Keys match `[A-Za-z_][A-Za-z0-9_]*`. Up to 128 variables and 64 KiB in total |
| `ports` | list | Up to 32. Each has `host` and `container` (1 to 65535), `bind` (an IP address, default `127.0.0.1`), `protocol` (`tcp` or `udp`, default `tcp`) |
| `volumes` | list | Up to 16. Each has `name` and an absolute `path` other than `/`. An app with volumes has one replica. The Docker volume is named `hivemind-<app>_<name>` |
| `resources` | object | `cpu` (0, or 0.01 to 1024) and `memory` (0, or 6Mi to 1Ti). Counted by placement and applied as container limits. 0 means no limit |
| `health` | object | Exactly one of `http` (`path`, default `/`; `port`), `tcp` (`port`) or `exec` (a list of arguments). Also `interval` (default `10s`), `timeout` (default `2s`), `retries` (1 to 10, default 3) and `start_period` (default 0). Durations are strings such as `5s` |
| `routes` | list | Up to 16. Each has `host`, an exact DNS name of at least two labels (lowercased, a trailing dot dropped; no wildcard, IP address or underscore), and `port`, the container port (1 to 65535). A host appears once, and belongs to one app: a put that claims another app's host gets `409`. A change to `routes` is a rolling update. See [routing](routing.md) |

`memory` takes a whole number of bytes, or a string such as `512Mi` or `2G`. The suffixes `k`, `M`, `G` and `T` are powers of 1000, and `Ki`, `Mi`, `Gi` and `Ti` are powers of 1024.

## Logs

`GET /v1/apps/{name}/logs` takes three query parameters:

- `replica` (default 0): the replica index.
- `tail` (default 0): the number of lines from the end. 0 returns the whole log.
- `follow` (default false): keep the stream open until the client disconnects, or the container exits.

The log is `text/plain`. Standard output and standard error are both written to the body, in the order the engine wrote them. The node that runs the replica reads its container's log. It picks the running container, or else the one with the highest generation. A replica on the server's node, or with no node, is read on the server.

The body is chunked, and ends with the trailer `Hivemind-Log-End`: `done` when the log ended, because the container stopped or the lines asked for were sent; `shutdown` when the server is shutting down and closed the stream; `error` when reading the log failed after the status line was sent (the server logs why). A body that stops without the trailer was cut off with its connection. A server before 2.2 sends no trailer. `hivemind logs -f` exits 0 on `done` and 1 otherwise, and does not reconnect.

A replica on a worker is read by that worker, which sends the log to the server as it reads it. Standard output and standard error stay separate frames on the way, and the body is the same as for a replica on the server. The worker has no listener: it takes the request over a poll it keeps open to the server. [Security](security.md#logs-from-workers) describes the routes. The errors that are particular to a worker:

- `404` when the worker has no container for the replica. For a replica that is pending, the message says so and why, as for a replica on the server.
- `503` when the node is lost (its lease is more than 15 s old), when the container engine on the node is unavailable, or when the server runs without `--advertise` and so cannot reach workers.
- `504` with `worker node-... did not answer within 10s; is it running?` when the worker did not take the request and start the stream in 10 seconds. The message names the worker's version when it reports one. A worker of version 2.0.0 does not take log requests: once the server has run for a minute, a worker that reports no version and has not polled for log requests gets `worker node-... runs a hivemind older than 2.1, which cannot stream logs; upgrade it`.
- `429` when the node already has 16 log streams open.
- `502` when the worker could not read the log for another reason.

With `follow`, closing the request ends the worker's stream and the log read on the worker. The server copies a worker's stream to the response as it arrives and keeps none of it, so a user who reads slowly slows the worker down.

## Metrics

`GET /metrics` is Prometheus text format, version 0.0.4, and needs the token. Cluster values are read from the store on each scrape.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `hivemind_apps` | gauge | none | Apps in the store |
| `hivemind_replicas_desired` | gauge | `app` | Replicas the spec asks for |
| `hivemind_replicas_running` | gauge | `app` | Replicas whose container at the current generation is running |
| `hivemind_replicas_ready` | gauge | `app` | Running replicas that pass their health check, or have none |
| `hivemind_container_restarts_total` | counter | `app`, `replica` | Restarts of the current container by the agent. Resets after 10 minutes of healthy running |
| `hivemind_node_lease_age_seconds` | gauge | `node` | Seconds since the node last reported in |
| `hivemind_node_cert_expiry_timestamp_seconds` | gauge | `node` | Unix time at which the node's certificate expires, as the server last recorded it. A node with no record has no sample. Alert on `hivemind_node_cert_expiry_timestamp_seconds - time() < 30 * 86400`: a node renews with about 122 days left, so less than 30 means its renewals fail |
| `hivemind_reconcile_duration_seconds` | histogram | none | Wall time of the local agent's passes |
| `hivemind_reconcile_errors_total` | counter | none | Passes that returned an error |
| `hivemind_http_requests_total` | counter | `method`, `route`, `code` | Requests by method, route pattern and status |
| `hivemind_router_requests_total` | counter | `host`, `code` | Requests to the server's router, with `--route-addr`. A host not in the routing table is `unknown` |
| `hivemind_router_upstream_errors_total` | counter | `host`, `kind` | Requests from the server's router to a replica that failed: `dial` (it tried the next replica) or `proxy` (after it reached one) |
| `hivemind_router_cert_expiry_timestamp_seconds` | gauge | `host` | Unix time at which the certificate the server's router serves for the host expires. A host without a certificate yet has no sample |

`route` is the route pattern, such as `/v1/apps/{name}`, or `unmatched`. A client cannot make the label set grow. The router's `host` label is a host in the routing table or `unknown`, so a client cannot grow it either. A worker's router is not in these metrics.

There are no metrics for rollouts yet. `GET /v1/apps/{name}/status` reports them.

`--metrics-addr` starts a second listener on a loopback address. It serves `GET /metrics` only, and it needs the token too.

## Health

`GET /healthz` returns `200` with `{"status":"ok"}` while the server answers. It does not check the container engine.
