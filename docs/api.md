# HTTP API

The API is served by `hivemind serve`. The default address is `http://127.0.0.1:4483`. The CLI uses this API, and so can scripts. Every route is JSON, except the log stream and `/metrics`.

There is no web UI in this build. The code for UI sessions and CSRF exists, but no route uses it, so the API accepts only the bearer token.

## Authentication

Send the admin token as one bearer header:

```
Authorization: Bearer hm_...
```

The scheme is case-insensitive. A request with more than one `Authorization` header is refused. The token is compared in constant time, against its SHA-256 digest.

`GET /healthz` is the only route that needs no token. The match is exact: a `HEAD` request, a `POST`, or a path with a trailing slash needs the token.

A request without a valid token gets `401` with the body `{"error":"unauthorized"}` and the header `WWW-Authenticate: Bearer realm="hivemind"`. The check runs before routing, so an unknown path without a token also gets `401`, not `404`.

The token is in `<data-dir>/admin.token`. `init` prints it once.

```sh
curl -H "Authorization: Bearer $(cat /var/lib/hivemind/admin.token)" http://127.0.0.1:4483/v1/status
```

## Errors

Every error is a JSON object with one field:

```json
{"error": "app \"web\": not found"}
```

| Status | When |
|---|---|
| 400 | Invalid JSON, an unknown field, an empty body, an invalid app spec, an invalid name or query value |
| 401 | No valid token |
| 404 | No such app, or no container for the replica on the server's node. Unknown routes give `{"error":"not found"}` |
| 405 | Wrong method for a known path: `{"error":"method not allowed"}` |
| 413 | Request body over 1 MiB |
| 500 | Unexpected failure. The message is `internal error`, and the details go to the server log |
| 503 | The container engine cannot be reached: `the container engine is unavailable` |

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

## Node API (internal)

Workers talk to the server over a second API, on the cluster port that `serve --advertise` opens. It has routes for a worker to join, register, read its assignments and report its containers. It is internal. Its routes, bodies and errors may change in any release, so this document does not describe them. The listeners and the trust model are in [security](security.md).

## Routes

| Method and path | Body | Success | Purpose |
|---|---|---|---|
| `GET /healthz` | none | 200 `{"status":"ok"}` | Liveness. No token |
| `GET /v1/apps` | none | 200, a list of apps | List apps |
| `POST /v1/apps` | app spec | 201 when created, 200 when updated, with `PutResult` | Create or update the app named in the body |
| `GET /v1/apps/{name}` | none | 200, `AppView` | Read one app |
| `PUT /v1/apps/{name}` | app spec | 201 or 200, with `PutResult` | Create or update the app at `{name}`. A `name` in the body must match |
| `DELETE /v1/apps/{name}` | none | 202 `{"name":"...","status":"deleting"}` | Delete the app |
| `POST /v1/apps/{name}/scale` | `{"replicas": N}` | 200, `AppView` | Set the replica count, 1 to 100 |
| `GET /v1/apps/{name}/status` | none | 200, `AppStatus` | Replicas of one app |
| `GET /v1/apps/{name}/logs` | none | 200, `text/plain` | Log of one replica's container |
| `GET /v1/status` | none | 200, a list of `AppStatus` | Replicas of every app |
| `POST /v1/join-tokens` | `{"ttl": "1h"}`, optional | 201, `{"token", "expires_at"}` | Issue a token that lets one machine join. See [join tokens](#join-tokens) |
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
  "updated_at": "2026-11-13T10:00:00Z"
}
```

`PutResult` wraps it: `{"app": AppView, "created": true}`.

`AppStatus` is one app's replicas:

```json
{
  "name": "web",
  "image": "nginx:alpine",
  "generation": 1,
  "desired": 2,
  "running": 2,
  "replicas": [
    {"index": 0, "node_id": "node-<12 hex digits>", "generation": 1, "desired": "running",
     "state": "running", "container_id": "<the engine's 64-character ID>", "restarts": 0, "ready": true}
  ]
}
```

The fields of each replica are `index`, `node_id`, `generation`, `desired` (`running` or `removed`), `pending_reason` (only when set), `state`, `container_id` (only when set), `error` (only when set), `restarts`, `health` (`starting`, `healthy` or `unhealthy`; only when the app has a health check), `ready`, `crash_looping` (only when set) and `next_restart` (RFC 3339, only while a restart is scheduled). `state` is the engine's state of the container: `created`, `running`, `paused`, `restarting`, `exited`, `dead` or `removing`. A replica with no container yet is `pending`. If the agent could not list containers, it is `unknown`.

`running` counts the replicas whose container is running at the current generation.

While a rollout is in progress, or after it has paused, the app status has a `rollout` object. It is omitted otherwise:

```json
"rollout": {"target": 2, "state": "in_progress", "reason": "", "moving": 0}
```

- `target` is the generation the rollout moves the replicas to.
- `state` is `in_progress` or `paused`. A finished rollout is not reported.
- `reason` is set when the rollout is paused. It is a sentence such as `rollout to generation 3 stopped: replica 0 is not ready after 1m0s`.
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
| `volumes` | list | Up to 16. Each has `name` and an absolute `path` other than `/`. An app with volumes has one replica |
| `resources` | object | `cpu` (0 to 1024) and `memory` (0 to 1Ti). Stored, not applied |
| `health` | object | Exactly one of `http` (`path`, default `/`; `port`), `tcp` (`port`) or `exec` (a list of arguments). Also `interval` (default `10s`), `timeout` (default `2s`), `retries` (1 to 10, default 3) and `start_period` (default 0). Durations are strings such as `5s` |

`memory` takes a whole number of bytes, or a string such as `512Mi` or `2G`. The suffixes `k`, `M`, `G` and `T` are powers of 1000, and `Ki`, `Mi`, `Gi` and `Ti` are powers of 1024.

## Logs

`GET /v1/apps/{name}/logs` takes three query parameters:

- `replica` (default 0): the replica index.
- `tail` (default 0): the number of lines from the end. 0 returns the whole log.
- `follow` (default false): keep the stream open until the client disconnects, or the container exits.

The log is `text/plain`. Standard output and standard error are both written to the body. The server reads the replica's container on its own node. It picks the running container, or else the one with the highest generation. A replica that has no container on the server's node gets `404`, and that includes every replica placed on a worker. A worker has no API, so read those logs with `docker logs` on the worker.

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
| `hivemind_reconcile_duration_seconds` | histogram | none | Wall time of the local agent's passes |
| `hivemind_reconcile_errors_total` | counter | none | Passes that returned an error |
| `hivemind_http_requests_total` | counter | `method`, `route`, `code` | Requests by method, route pattern and status |

`route` is the route pattern, such as `/v1/apps/{name}`, or `unmatched`. A client cannot make the label set grow.

There are no metrics for rollouts yet. `GET /v1/apps/{name}/status` reports them.

`--metrics-addr` starts a second listener on a loopback address. It serves `GET /metrics` only, and it needs the token too.

## Health

`GET /healthz` returns `200` with `{"status":"ok"}` while the server answers. It does not check the container engine.
