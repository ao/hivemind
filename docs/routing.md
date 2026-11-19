# HTTPS routing

A node started with `--route-addr` runs a router: an HTTPS reverse proxy that serves the hosts named in the apps' `routes`, with certificates from an ACME CA, and sends each request to a Ready replica of the app, on any node. This is new in the unreleased 2.2.0.

```yaml
name: web
image: nginx:alpine
replicas: 3
routes:
  - host: www.example.com
    port: 80
```

`host` is an exact DNS name, and `port` is the container port the app listens on. Any node can run a router, the server included, and DNS for `www.example.com` can point at any or all of them. Every router serves every routed host. A route can also take only a path prefix of its host, so that one host is served by several apps (see [paths](#paths)).

## Decisions

These were made by the owner on 2026-11-14.

1. A router on every node that opts in, the server included, rather than one ingress node. DNS for a host can then list several machines, and losing one of them does not take the host down. The routers share their certificates and ACME state through the server, so a host has one certificate whichever router a client reaches.
2. The router is the one listener in hivemind that may bind `0.0.0.0`. It serves the apps' public traffic, so it must be reachable from the internet, and it has no authentication of its own: the apps behind it decide who may do what. `--route-addr` has no default, and without it there is no router. Every other listener still refuses a wildcard address.
3. A routed app publishes each route's container port on an ephemeral host port on its node's private address, not on loopback, so a router on any node can reach a replica on any node. Because the host port is ephemeral, several replicas of one routed app can share a node.
4. This reverses part of decision 2 in [PLAN.md](PLAN.md), which scoped hivemind to host-port networking with no proxy. Host ports work as before, and an app without routes is unchanged. Routed apps go through the router.

Path routes were added on 2026-11-17, with these choices, each explained under [paths](#paths):

5. A path is a prefix matched on segment boundaries, the longest one first, on the request's decoded and cleaned path. The replica gets the path as the client sent it; there is no prefix stripping and no `X-Forwarded-Prefix`.
6. On a host with path routes, the router refuses with `400` a path that an app could resolve to another prefix than the router chose: encoded slashes and backslashes, backslashes, dot segments and control characters. A host without path routes is served as before.
7. A host may have path routes without a whole host route; other paths get `404`.
8. A router of a build before path routes gets a table without paths, in which a host with path routes has no endpoints, so it answers `503` for that host rather than send one app's requests to another. The server tells routers apart by whether they ask for paths, not by version.

Route protocols were added on 2026-11-19, with these choices, each explained under [protocols](#protocols):

9. A route's `protocol` is `http`, HTTP/1.1 to the replicas as before, or `h2c`, HTTP/2 over cleartext TCP with prior knowledge, which gRPC servers speak. The router uses Go's own HTTP/2, with no new module. There is no TLS to the replicas, and no `Upgrade: h2c` from HTTP/1.1.
10. A client on HTTP/1.1 to an h2c route is proxied, as HTTP/2 to the replica. gRPC clients speak HTTP/2 anyway; other HTTP/2 backends get every client.
11. An h2c request is a stream that lasts as long as it moves. It has no whole-request read limit; it ends when no data passes either way for 10 minutes, as an upgraded connection does.
12. A router of a build before protocols gets each h2c route with no endpoints, and answers `503` for it, rather than speak HTTP/1.1 to replicas that may speak only HTTP/2. The server tells it apart by what it asks for, as for paths.
13. The agent's HTTP health check stays HTTP/1.1. An app whose server speaks only h2c uses a TCP check or an exec check.

## How a request is routed

1. The server places a routed replica only on a node that reported a private address. The server's own node uses its `--advertise` address, or `127.0.0.1` when it has none (then there are no workers). A worker uses the local address of its connection to the server, which it sends when it registers; the server refuses one that is not an IP address, or is loopback, a wildcard or multicast. A worker older than 2.2 sends none, and takes no routed replicas.
2. The agent creates the container with each route port published on an ephemeral port of that address, and reports the port Docker chose. It reads the port again after every start, because Docker picks a new one each time.
3. The server builds the routing table from the reports: for each host, the `address:port` of every replica whose container is Ready (running, and passing its health check if it has one, the same signal a rolling update waits for), is the container its replica or surge is assigned, and is on a node whose lease is current. `GET /v1/routes` shows it.
4. Each router reads the table once a second: the server's from the store, a worker's over the node API. A router that cannot read it keeps serving the last table it read.
5. The router finds the request's route: for a host with path routes, the one whose prefix is the longest one of the request's path, or else the whole host's (see [paths](#paths)). It sends the request to that route's replicas in turn. A replica that cannot be dialled within 2 seconds is skipped for 3 seconds, doubling up to 30 seconds while its dials keep failing ([limits](#limits)), and the request goes to the next one, with its body if none of it was sent. A request that fails after it reached a replica gets `502`.

The router removes every request header whose name starts with `X-Forwarded-`, and `Forwarded`, `X-Real-Ip`, `True-Client-Ip`, `X-Client-Ip`, `X-Cluster-Client-Ip` and `Cf-Connecting-Ip`, comparing names without case and with `_` read as `-` (a CGI-style server turns `X_Forwarded_For` and `X-Forwarded-For` into the same variable). Then it sets `X-Forwarded-For` to the address the connection came from, `X-Forwarded-Proto` to `https`, `X-Forwarded-Port` to the https port, and `Host` and `X-Forwarded-Host` to the host as the routing table has it: in lower case, without a trailing dot, and with `:<port>` only when `--route-https-port` is not 443, as in the router's redirects to https. Whatever case, trailing dot or port the client wrote in `Host`, the app sees the same value, so the links it builds from `Host` point at the routed host. Clients can use HTTP/1.1 or HTTP/2; the router speaks HTTP/1.1 to the replicas, or HTTP/2 to those of an h2c route (see [protocols](#protocols)).

WebSocket and other HTTP/1.1 upgrades pass through to the replica of an http route. An upgrade to `h2c` does not: a replica that switched to it would speak HTTP/2 with the client directly, past the router's checks and header rewriting. A replica that should get HTTP/2 from the router has an h2c route instead. The router takes the `h2c` token out of `Upgrade` and drops `HTTP2-Settings`, so the request reaches the replica as plain HTTP/1.1, as RFC 9110 allows a server to ignore an upgrade. A replica that answers `101` for a protocol the request did not name gets the client a `502`.

| Request | Reply |
|---|---|
| A host in the table, with a Ready replica for the request's route | The replica's reply |
| A host in the table, with no Ready replica for the request's route | `503` |
| A path that no route of a host with path routes takes, because the host has no whole host route | `404`, with the body `not found` |
| On a host with path routes, a path with an encoded slash or backslash, a backslash, a dot segment or a control character | `400`, whose body names the one found, as `bad request: the path has a dot segment (. or .., or %2E)` (see [paths](#paths)) |
| A host not in the table, on the http port | `404`, with the body `not found`. No host is listed |
| A TLS handshake for a name not in the table | The handshake fails: there is no default certificate |
| A `Host` header that is not the name the client asked for in TLS (SNI) | `421` |
| Any request to a known host on the http port | `308` to the same path on https, unless it is an ACME HTTP-01 challenge |

## Paths

A route with a `path` takes only the requests under that prefix of its host, so one host can be served by several apps:

```yaml
name: web
image: nginx:alpine
routes:
  - host: www.example.com
    port: 80
---
name: api
image: ghcr.io/example/api:2
routes:
  - host: www.example.com
    path: /api
    port: 8080
```

These are two files, one per app, for `hivemind deploy -f`. A request for `www.example.com/api` or anything under `/api/` goes to `api`, and every other request for the host to `web`. `hivemind routes` shows the table.

### Matching

- A route without `path`, or with `/`, takes the whole host: every request that no path route of the host takes.
- A path is a prefix on segment boundaries: `/api` takes `/api`, `/api/` and `/api/users/7`, and not `/apix` or `/api.json`. A trailing slash in the spec is dropped, so `/api/` is `/api`.
- The longest prefix wins: with `/api` and `/api/v2` on one host, `/api/v2/x` goes to the `/api/v2` route and `/api/v20` to `/api`.
- The comparison is exact, so case matters: `/API` is not under `/api`. That follows RFC 3986, where a path is case-sensitive.
- The router matches on the request's decoded path, as most apps read it, with empty segments and a trailing slash cleaned away: `//api//users/` is matched as `/api/users`, and `/%61pi` as `/api`. The query string plays no part.
- The replica gets the request as the client sent it. The router does not take the prefix off, add one or clean the path: an app under `/api` sees `/api/users/7`.
- A host may have path routes and no whole host route. A request for a path none of them takes then gets `404` from the router.
- Certificates stay per host: one certificate for `www.example.com`, whichever apps share it. The metrics stay by host too, with no path label, so a client cannot grow the label set.

### What a path may hold

A path starts with `/`, is at most 256 bytes, and is one or more segments separated by single slashes. A segment holds letters, digits and `- . _ ~ ! $ & ' ( ) * + , = : @`, which RFC 3986 allows in a path segment as they are, and is not `.` or `..`. Anything else is refused when the app is deployed, with the character it found:

- `%`: the router matches the decoded path, so a prefix is written decoded. An encoded prefix such as `/a%2Fb` would be one segment to some apps and two to others.
- `?` and `#` end a path, and an empty segment (`//`) would never match, since requests are cleaned of them.
- `.` and `..` segments name another path.
- `;`: servlet containers such as Tomcat read what follows it in a segment as parameters and strip it before they route, so a prefix with `;` would match differently there.
- Spaces, control characters, `\`, the characters RFC 3986 does not allow in a path (`"`, `<`, `>`, `[`, `]`, `^`, `{`, `|`, `}` and the backtick), and anything outside ASCII. A letter outside ASCII has more than one spelling in Unicode (composed and decomposed), which would make two prefixes of one name.

### Requests the router refuses

On a host with at least one path route, the router answers `400` to a request whose path, after decoding, has:

- an encoded slash or backslash (`%2F`, `%5C`). Decoded, it splits a segment in two; left encoded, it does not, and apps differ in which they do, so `/api%2Fx` is under `/api` to some apps and not to others;
- a backslash, which some servers read as a slash;
- a `.` or `..` segment, written as it is or encoded (`%2E`), also with `;` parameters after it (`..;`). `/admin/../api/x` would go to the `/api` app, and an app that does not resolve dots itself, or a servlet container that strips `;x` first, could serve its own `/admin` for it;
- a control character, `%00` included.

Browsers and curl remove dot segments before they send a request, and do not encode slashes in paths, so these requests are made by hand. The checks are what keeps a request with such a path from reaching an app that does not own the path it resolves to; the router refuses rather than rewrite, so what an app receives is always what the client sent. A host with only a whole host route is not checked: the router has no path decision to make there, and passes every path to the app as before.

### Conflicts

A host and path belong to one app. A deploy that claims a host and path another app routes gets `409`, naming that app, as in `host www.example.com with path /api is routed by app api; remove it there first`; for the whole host the message is as before, `host www.example.com is routed by app web; ...`. Apps may share a host with different paths. One app may route several paths of a host, to one container port or to several; the same host and path twice in one app is refused when it is deployed.

### No prefix stripping

The router does not take the prefix off before it sends a request on, and there is no `strip_prefix`. An app behind a stripped prefix still builds its links, its redirects (`Location`) and its cookie paths with the prefix, so it has to know it anyway, and the router does not rewrite responses to put it back; most frameworks have a base path setting for that, and an app that serves under `/api` itself needs nothing. For the same reason the router sets no `X-Forwarded-Prefix`: it would tell the app that a prefix was taken off when none was, and frameworks that read it (Spring's `ForwardedHeaderFilter`, for one) would put it in front of the path a second time. A client's `X-Forwarded-Prefix` is removed, as before.

### Mixed versions

A router of a build before path routes cannot tell paths apart, and decodes the routing table strictly: a `path` field, or a host listed twice, would make it refuse the whole table and serve its last one. The server gives each router the table it can read. This build's router asks the node API for the table with `?paths=1` (and `protocols=1`, see [protocols](#mixed-versions-of-route-protocols)) and gets every route. A router that does not ask gets one entry per host and no paths, and a host with a path route is in it with no endpoints: that router answers `503` for the host, and keeps its certificate, rather than send the requests for `/api` to `web`, with the cookies and `Authorization` headers that clients send `api`. Hosts without path routes are served by such a router as before.

The server decides by what the router asks, not by its version. A version does not say whether a build has path routes: routing and path routes are both in the unreleased 2.2.0, builds between them are described from the same tag, and a build from source without a tag reports `dev`. The version is in the warnings, which say which nodes to upgrade:

- the server logs `this node's router is of a build before path routes, and answers 503 for the hosts that have them; upgrade the node`, with the node, its version and the hosts, when such a router first reads a table with such hosts, and again when they change;
- `hivemind routes` names each such router that read the table in the last minute on standard error, with the hosts it answers `503` for, and `GET /v1/routes` lists them in `old_routers`;
- `hivemind status` prints a warning line for each app with a route on such a host, from `warnings` in the app status.

Upgrade every node that runs a router before giving a host its first path route. A router that was upgraded reads paths at its next read, within a second, and the warnings go.

## Protocols

A route's `protocol` is what the routers speak to its replicas: `http`, the default, for HTTP/1.1, or `h2c`, for HTTP/2 over cleartext TCP with prior knowledge (RFC 9113, section 3.3). That is how a gRPC server listens, so a gRPC service is routed with `protocol: h2c`, and so is any other server that should get HTTP/2 from the router.

```yaml
name: cart
image: ghcr.io/example/cart:1
replicas: 2
routes:
  - host: www.example.com
    path: /shop.Cart
    port: 50051
    protocol: h2c
health:
  tcp:
    port: 50051
```

A gRPC call is a `POST` to `/<package>.<Service>/<Method>`, so a path route for each service, as here, or the whole host takes them. The routes of one host, and of one app, may differ in protocol: `www.example.com/` can be an http route of another app. The value is read in any case. `http` is stored as nothing, so writing it changes nothing, and a spec without `protocol` has the template it had before protocols existed: an upgrade starts no rollout. Anything else is refused when the app is deployed, as `routes[0]: protocol "h2": use http or h2c`. `hivemind routes` shows each route's protocol, and `GET /v1/routes` has it ([API](api.md#routing-table)).

On an h2c route the router:

- speaks HTTP/2 only to the replicas, with Go's standard library (`net/http` with `Protocols` set to unencrypted HTTP/2). One connection to each replica carries many requests at once, and the router opens another when the replica's own stream limit is reached. A replica that speaks only HTTP/1.1 behind an h2c route gets the clients `502`, and so does an h2c-only replica behind an http route;
- does everything else as on an http route: the forwarding headers (the client's removed, the router's set), the 2-second dial timeout and the backoff of a replica that cannot be dialled, TCP_USER_TIMEOUT, the retry of a GET, HEAD or OPTIONS request with no body whose connection failed before a reply (a gRPC call is a POST, so it is not retried), the [metrics](#metrics) and the connection caps;
- passes the request body to the replica as the client sends it, and flushes the reply to the client after every write, so a server stream and a bidirectional stream flow both ways as they come, with nothing held in the router. The reply's trailers reach the client, and with them a gRPC call's status (`grpc-status` and `grpc-message`). `TE: trailers` from the client reaches the replica;
- removes `Upgrade` and `Connection`, since HTTP/2 has no upgrades: a WebSocket request reaches the replica as a plain request, which a WebSocket server refuses.

A gRPC client speaks HTTP/2 to the router over TLS, negotiated with ALPN, as to any gRPC service on 443: in Go, `grpc.NewClient("www.example.com:443", grpc.WithTransportCredentials(credentials.NewTLS(nil)))`; with grpcurl, `grpcurl www.example.com:443 shop.Cart/List`. A client on HTTP/1.1 is proxied too: the router sends its request to the replica as HTTP/2, and its reply comes back chunked, with the trailers at the end. That serves browsers and tools that reach an HTTP/2 backend. gRPC itself needs HTTP/2 from the client as well, and every gRPC client speaks it; gRPC-Web is not translated.

grpcurl without `-proto` or `-protoset` asks the server for its services over gRPC's reflection service first, which is a service of its own, `/grpc.reflection.v1.ServerReflection`. With a path route for `/shop.Cart` alone, that call goes to the host's other routes, and grpcurl stops with `server does not support the reflection API`. Route the reflection service to the app as well, or route the whole host to it, or give grpcurl the `.proto` files:

```yaml
routes:
  - host: www.example.com
    path: /shop.Cart
    port: 50051
    protocol: h2c
  - host: www.example.com
    path: /grpc.reflection.v1.ServerReflection
    port: 50051
    protocol: h2c
```

On 2026-11-18, grpcurl 1.9.3 against `kong/grpcbin` behind such routes listed the services and made a unary call through each of three routers; with the first route alone, `list` and the call both failed as above. A server that has only the older `grpc.reflection.v1alpha.ServerReflection` needs that path instead.

### Streams and their limits

A request on an h2c route can be a stream that lasts as long as it moves. Its limits:

- No whole-request read limit. On an http route a request, body included, must be read within 5 minutes; on an h2c route a client may send for as long as it likes, as a client stream or a bidirectional stream does.
- No data either way for 10 minutes, in the request's body or in its reply, ends the request, as it does an upgraded connection: the router resets the replica's stream, and the client's stream is reset, or gets `502` if its reply had not started. HTTP/2 PING frames, which gRPC keepalives send, are answered by the router and do not count: they keep a client's connection open, not its streams. A stream that may be quiet for longer must send something.
- The replica must send the reply's headers within 60 seconds of the end of the request: for a unary call or a server stream, from when the client sent its message; for a client stream or a bidirectional stream, from when the client ends its side. After the headers there is no reply timeout. A server that sends nothing at all, not even headers, until its first message, as grpc-go does, gets the client a `502`, which gRPC clients report as `UNAVAILABLE`, on a server stream that waits longer than that for its first message. Such a server should send its headers at once on long waits (`stream.SendHeader` in grpc-go).
- A write to the client that makes no progress for 60 seconds ends the client's connection, with every stream on it; a stream that pauses between writes is not cut.
- One client connection carries at most 250 streams at once, Go's default for the router's HTTP/2 server.
- A rolling update stops the old copy 5 seconds after it leaves the table, and the stop gives the app 10 seconds after SIGTERM ([rolling updates](#rolling-updates)), so a stream on it has that long to end. A gRPC server that stops gracefully (`GracefulStop` in grpc-go) sends GOAWAY and finishes its open calls in that time; a longer stream is cut, and its client calls again, which reaches a new copy. A router that is stopped waits up to 10 seconds for requests in flight.

`internal/router/h2c_test.go` runs these with limits of 200 ms in place of the 60-second ones and 1 second in place of the 10-minute one: a server stream and a bidirectional stream of 1.4 s pass, a stream quiet both ways ends 1 s after its last data, at both ends, and a replica that sends nothing after the request gets `502` at the 200 ms limit.

### Health checks

The agent's HTTP check speaks HTTP/1.1. A server that speaks only HTTP/2 without TLS, as a gRPC server does, answers it with an HTTP/2 frame or closes the connection, so the check fails and the replica never becomes Ready; `internal/health` tests that. The agent then restarts the container as unhealthy, again and again. `hivemind status` shows `HEALTH` `starting` and `RESTARTS` going up, and, while a restart waits out its backoff, `crash-looping: failed its health check 3 times in a row: http GET /: ... malformed HTTP response "\x00\x00\x06\x04..."`; the node's log has a `restarted unhealthy container` line with the same error at each restart. For such a server, use:

- a TCP check on its port, as above: the replica is ready once the server listens;
- an exec check that asks the server over gRPC's health protocol from inside the container, such as [grpc_health_probe](https://github.com/grpc-ecosystem/grpc-health-probe), when the image has it: `exec: [/bin/grpc_health_probe, -addr=127.0.0.1:50051]`;
- or a server that speaks HTTP/1.1 and h2c on one port (Go's `net/http` with `Protocols` set to both), or an HTTP health port of its own, and an HTTP check.

The HTTP check has no HTTP/2 setting. A gRPC server refuses a request that is not a gRPC call, so the setting would help only HTTP/2 servers without HTTP/1.1, and a new field in the check would make a worker of an earlier build refuse its assignments.

### Changing a route's protocol

A change to a route's protocol is a change to the app's routes, so it is a rolling update, and the routers take the new protocol within a second, while the old copies still run. To move a running app from HTTP/1.1 to h2c, or back, deploy in two steps: first a server that speaks both on the port, then the new protocol.

### Mixed versions of route protocols

A router of a build before protocols decodes the table strictly, so a `protocol` field would make it refuse the whole table and serve its last one. This build's router asks for `?paths=1&protocols=1`. A router that asks for paths only gets no protocol field, and each h2c route with no endpoints: it answers `503` for the route, keeps the host's certificate, and serves the host's other routes as before, rather than send HTTP/1.1 to replicas that may speak only HTTP/2. A router before path routes gets no endpoints for a host whose one route is h2c, as for a host with path routes. The warnings are those of [paths](#mixed-versions), with their own text:

- the server logs `this node's router is of a build before route protocols, and answers 503 for the h2c routes; upgrade the node`, with the node, its version and the routes, when such a router first reads a table with h2c routes, and again when they change;
- `hivemind routes` names each such router on standard error, as `warning: the router on node node-3f2a... (v2.2.0-31-gdef) is of a build before route protocols, and answers 503 for the h2c routes www.example.com/shop.Cart; upgrade the node`, and `GET /v1/routes` lists it in `old_routers`, with the routes in `h2c`;
- `hivemind status` prints a warning line for each app with such a route.

Upgrade every node that runs a router before giving a route protocol `h2c`. The agent does not use a route's protocol, and assignments do not carry it, so a worker of an earlier build runs an h2c app's replicas; only its router holds them back. A server of a build before protocols refuses a spec with `protocol`, as an unknown field. The store has no new schema for protocols, so an earlier build opens a store that has h2c routes, reads them as http routes, and its routers send HTTP/1.1 to those replicas, which an h2c-only server answers by closing the connection: `502`. Before going back to an earlier build, change such routes to http or delete them.

## Rolling updates

A rolling update of a routed app moves one replica at a time, as for any app (see [concepts](concepts.md#rolling-updates)), and every move is a surge: the new copy starts beside the old one, on another node or on the same node, because a routed app holds no fixed host port. The new copy enters the table when it is Ready, and only then does the rollout move the replica to it. At that point the old copy leaves the table, and its node's agent keeps it running for 5 more seconds before it stops it, so that every router has read the new table and the requests already sent to the old copy finish. So a routed app keeps serving through an update, on one machine as on several, as long as each new copy becomes Ready. This closes, for routed apps only, the gap that host-port apps have on one machine. A stream on an h2c route that is open on the old copy has those 5 seconds, and the 10 the app gets after SIGTERM, to end ([streams](#streams-and-their-limits)).

A drain moves a routed replica the same way, to another node.

## Certificates

Certificates come from [certmagic](https://github.com/caddyserver/certmagic), over ACME, from the CA in `--acme-ca`: Let's Encrypt's production directory by default. `--acme-email` is the account's contact, and is required with `--route-addr`; registering the account accepts the CA's subscriber agreement. The router asks for a certificate for each host in the table and for no other name: there is no on-demand issuance, wildcards are not supported, and IP addresses are refused. certmagic renews each certificate in the last third of its lifetime, checking every 10 minutes, and retries a failed issuance with backoff. A router that starts with a certificate already in that window renews it at once, rather than at its first check; one that another router has already renewed in the shared storage is loaded from there. With the Let's Encrypt production CA, certmagic tries a retried issuance against Let's Encrypt's staging CA first, to spare the production rate limits.

HTTP-01 is the only challenge. The CA connects to port 80 of the host's address, so `--route-http-port` must be reachable as port 80 from the internet on at least one node that DNS lists. When a router starts, it reads the table and loads the certificates already in the shared storage before it takes TLS connections, so the first handshakes after a start do not fail; connections that arrive meanwhile wait, up to 5 seconds. Any router can answer a challenge another router started, since the tokens are in the shared storage. A challenge for a host that a router has not read in the table yet makes it read the table at once, at most once a second, so the first validation of a new host does not fail on a router that lags the one that ordered the certificate. That read is the router's own, with a 5-second limit: a client that gives up does not cancel it.

certmagic answers a request for `/.well-known/acme-challenge/<token>` on a routed host from a challenge it started itself, and otherwise reads the host's challenge from the shared storage, whatever the token: on a worker that read is a request to the server. Anyone can send such requests, so the router bounds those reads. For each host one read runs at a time, the next starts no sooner than 250 ms after the one before it started (at most 4 a second), and every request waits for the first read that starts after it arrived, so the requests that arrive together share one read, and a challenge that another router stored a moment before the CA's request is still found. A request whose host has no challenge pending gets the redirect to https; certmagic logs it at debug level. When the storage cannot be read, as while the server is away, the router logs one warning until a read works again. TLS-ALPN-01 is off, and DNS-01 is not offered. OCSP stapling is off: Let's Encrypt ended its OCSP service in 2025.

Certificates, their private keys, the ACME account and its key, and the challenge tokens are stored in the server's database, in a key/value table, and every router reads and writes them there: the server's router directly, a worker's over the node API. A router that receives a challenge another router started answers it from that table. Issuing a certificate takes a lock in the same database, so when every router asks for a new host's certificate at once, one of them issues it and the others find it stored. A lock is held for 30 seconds at a time and renewed every 10 seconds while its holder works; a holder that dies loses it 30 seconds after its last renewal. This follows certmagic's Storage documentation ("Processes running in a cluster should use the same Storage value") and its distributed HTTP-01 solver, "which allows the ACME HTTP-01 and TLS-ALPN challenges to be solved by an instance other than the one which initiated it". The ACME state is in the database, so `hivemind backup` carries it and `hivemind restore` brings it back.

## DNS

Point each routed host at the public address of one or more nodes that run a router: an `A` record (and an `AAAA` record for a router on an IPv6 address) per node. Every listed node must accept connections on ports 80 and 443, because the CA's validation and clients may reach any of them. A node that is down and still listed fails the requests that reach it; take it out of DNS, or put the routers behind a load balancer that checks them.

It can fail renewals too. The CA validates an HTTP-01 challenge by connecting to an address of the host's record, which can be the dead node's, and the challenge then fails whichever router asked for it. Pebble, the test CA, takes the first address listed: in the [fourth soak](soak.md#certificates-and-the-harnesss-dns-answers), with a lost server's address still first in each host's answers, every renewal failed with `dial tcp 10.0.0.1:80: connect: no route to host` until the answers were put right. So when a node that runs a router is replaced or removed, as after a [restore at a new address](backup.md#replacing-a-lost-server), change the records of every routed host it was in. An alert on `hivemind_router_cert_expiry_timestamp_seconds - time()` ([metrics](#metrics)) catches renewals that keep failing well before a certificate expires: certmagic renews in the last third of a certificate's lifetime, so with 90-day certificates anything under about 25 days left means the renewals have failed for days.

## Ports and firewall

| Port | Bind | Reachable from |
|---|---|---|
| `--route-http-port` (80) | `--route-addr` | The internet: ACME HTTP-01 and redirects |
| `--route-https-port` (443) | `--route-addr` | The internet: the apps' traffic |
| Ephemeral ports of routed containers | The node's private address | The other nodes' routers only |

The ephemeral ports are in Docker's ephemeral range, the kernel's `net.ipv4.ip_local_port_range` (usually 32768 to 60999). Docker publishes them on the node's private address, so any machine that can reach that address can reach the apps directly, without the router. On a private network that only the cluster's nodes share, that is the same exposure as the network itself. Otherwise, allow those ports from the other nodes only. Docker adds its own iptables rules for published ports, so a filter goes in its `DOCKER-USER` chain and matches the original destination. For a node at `10.0.0.5` whose peers are in `10.0.0.0/24`:

```sh
iptables -I DOCKER-USER -p tcp -m conntrack --ctorigdst 10.0.0.5 --ctorigdstport 32768:60999 \
  ! -s 10.0.0.0/24 -j DROP
```

Check the rule against the Docker documentation for your version ("Packet filtering and firewalls"). See [security](security.md#routing) for what the router exposes.

The other direction matters as much: every node must accept the whole range from every other node. A firewall or cloud security group between the nodes that allows only hivemind's own ports (4484 and 4485) leaves each router able to reach only the replicas on its own node; its dials to the others time out after 2 seconds, the endpoint is skipped for 3 seconds, then for longer up to 30, and tried again, so requests that a router sends to another node wait 2 seconds before they go to a local replica, and a router on a node without a replica of the host answers `502` once every endpoint has failed. On EC2 that means a security group rule for TCP 32768 to 60999 from the nodes' own group; the [second soak](soak.md#second-soak-routing-a-private-registry-and-clients-outside-the-cluster) needed one, which its first set-up lacked.

## Example

Three machines on one private network, `10.0.0.5` (the server), `10.0.0.6` and `10.0.0.7`, each with a public address, and DNS for `www.example.com` listing the three public addresses.

```sh
# On the server.
hivemind init
hivemind serve --advertise 10.0.0.5 --route-addr 0.0.0.0 --acme-email ops@example.com

# On the server, in another terminal: a join token for each worker.
hivemind join-token

# On each worker.
hivemind join 10.0.0.5 --token <token> --route-addr 0.0.0.0 --acme-email ops@example.com

# On the server.
cat > web.yaml <<'EOF'
name: web
image: nginx:alpine
replicas: 3
routes:
  - host: www.example.com
    port: 80
health:
  http:
    port: 80
EOF
hivemind deploy -f web.yaml
hivemind status web
# HIVEMIND_TOKEN: the admin token that init printed, also in <data-dir>/admin.token.
curl -s http://127.0.0.1:4483/v1/routes -H "Authorization: Bearer $HIVEMIND_TOKEN"
```

`join` runs the worker in the foreground, and writes the router's flags it was given to `<data-dir>/hivemind.yaml` (`route-addr: "0.0.0.0"`, `acme-email: "ops@example.com"`), so the worker keeps its router when it runs as a service or is started again with `hivemind serve --role worker`; a flag given to `serve` overrides the file. See [systemd](systemd.md#worker).

Once the certificate is issued, `https://www.example.com/` answers from any of the three machines. In the integration test, against Pebble with its validation delays off, all three routers served the host 7 seconds after the deploy (three Docker-in-Docker nodes on one EC2 c8g.4xlarge); a real CA takes as long as it takes. A second deploy that changes the spec rolls the replicas over without a failed request.

The settings have `HIVEMIND_` variables and config file keys, as the other settings: `HIVEMIND_ROUTE_ADDR`, `HIVEMIND_ROUTE_HTTP_PORT`, `HIVEMIND_ROUTE_HTTPS_PORT`, `HIVEMIND_ROUTE_MAX_CONNS_PER_IP`, `HIVEMIND_ACME_EMAIL`, `HIVEMIND_ACME_CA`, and `route-addr` and so on in `hivemind.yaml`. See the [command reference](cli.md).

## Metrics

The server's `/metrics` has every router's counters, and the expiry of the certificate each one serves for each host, labelled with the node's ID:

- `hivemind_router_requests_total{node,host,code}`, where a host not in the table counts as `unknown`;
- `hivemind_router_upstream_errors_total{node,host,kind}`, with `kind` `dial` (the router tried the next replica), `connection` (the connection failed before a reply, and a GET, HEAD or OPTIONS request with no body went to the next replica) or `proxy` (any other failure after it reached one);
- `hivemind_router_connections_refused_total{node,reason}`, the connections closed as they were accepted, with `reason` `address` (the client's address had as many open as `--route-max-conns-per-ip`) or `total` (the router had as many open as its total cap; see [limits](#limits));
- `hivemind_router_cert_expiry_timestamp_seconds{node,host}`.

A worker has no metrics listener. It posts its router's counters to the server over the node API every 15 seconds, so its series lag by up to that. The server keeps only the hosts it routes, and `unknown`, from a worker's report, and from its own router's at each scrape, so a host whose apps were deleted drops out for every router; it shows a worker's report for 2 minutes after it arrives, so a worker that stops drops out. Sum over `node` for a host's totals across the routers, for example `sum by (host, code) (rate(hivemind_router_requests_total[5m]))`. A server older than 2.2 has no route for the counters; a 2.2 worker logs that once and keeps routing.

## Limits

- Hosts are exact names. No wildcards, no IP addresses, and at most 16 routes per app. A host and path belong to one app: a deploy that claims a host and path another app routes gets `409`, naming that app. Delete the route from that app first. Paths are prefixes only, with no patterns or regular expressions, and the router does not strip them (see [paths](#paths)).
- No TCP or UDP routing.
- HTTP/1.1 or, on an h2c route, HTTP/2 without TLS to the replicas ([protocols](#protocols)). No TLS to the replicas, and no gRPC-Web translation.
- HTTP-01 only, so the CA must reach port 80 on a node in DNS.
- Request headers are limited to 64 KiB, and so are a replica's response headers. A request, body included, must be read within 5 minutes, except on an h2c route, where a request that carries no data either way for 10 minutes ends instead ([streams](#streams-and-their-limits)); a replica must start its reply within 60 seconds of the end of the request. A reply streams for as long as the replica sends it.
- A router keeps at most 256 connections open from one client address, over its two ports together; for IPv6 the address is the client's /64, since one machine can pick addresses from a whole /64. `--route-max-conns-per-ip` sets the number, and 0 turns the cap off: do that for a router behind a TCP load balancer or proxy that does not keep the clients' addresses, where every connection comes from the proxy. A router also keeps at most half of the file descriptors its open-file limit leaves after 1,024 for the rest of hivemind (a connection the router proxies holds two, the client's and the replica's): 523,776 with a limit of 1,048,576, a quarter of the limit when it is 2,048 or less, and 16,384 when the limit is unlimited or cannot be read (Windows). Raise the limit (`LimitNOFILE` in systemd, `ulimit -n`) to raise it. The router logs both caps when it starts, as `max_conns` and `max_conns_per_ip`, the latter 0 when it is off. On a node without `--route-addr` the router settings do nothing, and `serve` and the worker log a warning naming each one that was given, as `router settings are ignored without --route-addr: this node runs no router ignored="--route-max-conns-per-ip (flag --route-max-conns-per-ip)"`. A connection over a cap is closed as it is accepted, with no reply, logged at debug level and counted in `hivemind_router_connections_refused_total`.
- A write to an https client that makes no progress for 60 seconds, as to a client that stopped reading, ends the connection. The limit counts from each write, so a reply that pauses between writes, such as a stream of events, is not cut.
- An upgraded connection, such as a WebSocket, that carries no data either way for 10 minutes is closed, and so is a request on an h2c route. An app that keeps quiet WebSockets open longer should send pings more often than that, as most WebSocket libraries can; a quiet gRPC stream needs a message, since HTTP/2 PINGs do not count.
- A router does not cap its connections to the replicas. An HTTP/2 client can run up to 250 requests at once on one connection (Go's default), and on an http route each that is in flight holds its own connection to a replica, so the per-address cap bounds one client to 64,000 requests in flight; the replica's own limits apply to the rest. On an h2c route the requests share the router's connections to each replica.
- The table a router serves is up to a second old, longer while it cannot reach the server. For up to 15 seconds after a node is lost, its replicas stay in the table and their dials fail; each such request costs up to 2 seconds before it goes to another replica. While the server is away, a worker's router keeps its last table, with the server's replicas in it. A replica whose dial failed is skipped for 3 seconds, and for twice as long after each failure that follows, up to 30 seconds; a request that gets through, or the replica leaving the table, starts it at 3 seconds again. So a dead replica costs one such request every 30 seconds per router once its dials have failed for about a minute, not one every 3 seconds.
- After `serve` starts, as after a restore, the table keeps the replicas of each worker that was up before the start as the worker last reported them, until it reports again or the start grace ends (`--start-grace`, 2 minutes by default; see [concepts](concepts.md#leases-and-node-loss)), and past the grace while no worker that was waiting has reported, as the workers then stay waiting. A worker that died while the server was down therefore stays in the table that long, and requests pay a failed dial for its replicas; a worker that is up, and has not reached the restored server yet, keeps its traffic. The server's own replicas from before the start are left out until its agent reports them.
- A request whose body has started to go to a replica is not retried on another one.
- A router keeps idle connections to each replica. When a replica's machine stops without closing them (power off), a request already sent on such a connection fails after 5 seconds without an acknowledgement (TCP_USER_TIMEOUT, on Linux), instead of waiting for the 60-second reply timeout. A GET, HEAD or OPTIONS request with no body then goes to the next replica, as nginx's `proxy_next_upstream` does by default; any other request gets `502`.
- A worker's router needs a 2.2 server. Workers older than 2.2 take no routed replicas.
- The routers share one ACME account and one set of certificates. Each router on each node holds every routed host's private key in memory.

## Dependencies

certmagic and the modules it brings in, with their licences, as read from each module's licence file. All are compatible with hivemind's GPL-3.0.

| Module | Version | Licence |
|---|---|---|
| github.com/caddyserver/certmagic | v0.25.6 | Apache-2.0 |
| github.com/mholt/acmez/v3 | v3.1.7 | Apache-2.0 |
| github.com/caddyserver/zerossl | v0.1.6 | MIT |
| github.com/libdns/libdns | v1.1.1 | MIT |
| github.com/klauspost/cpuid/v2 | v2.4.0 | MIT |
| github.com/miekg/dns | v1.1.73 | BSD-3-Clause |
| github.com/zeebo/blake3 | v0.2.4 | CC0-1.0 |
| go.uber.org/zap | v1.28.0 | MIT |
| go.uber.org/zap/exp | v0.3.0 | MIT |
| go.uber.org/multierr | v1.11.0 | MIT |
| golang.org/x/crypto | v0.57.0 | BSD-3-Clause |
| golang.org/x/net | v0.59.0 | BSD-3-Clause |
| golang.org/x/text | v0.42.0 | BSD-3-Clause |

## Tests

`scripts/integration-routing.sh` runs three Docker-in-Docker nodes, each with a router, against Pebble, a test ACME CA, and pebble-challtestsrv, which answers the test host with the three nodes' addresses, all on one private Docker network with no published port. It checks the listeners, that one certificate is issued although the three routers ask at once, that each router spreads requests over both replicas, that no request fails during a rolling update, and that requests through the other routers keep succeeding when a node that runs a replica is killed. On one EC2 c8g.4xlarge with sequential curl requests from a container on the same network: 4,734 requests during the rolling update, which took 13 seconds, and 4,446 after the kill, while the lost replica was placed again in 18 seconds; none failed.

For path routes it runs a second app under `/v1` on the same host, and checks through each router which app every path reaches, and that the paths the router refuses get `400`. Then it rolls the path app over, and then the whole host app, while requests for `/` and for `/v1/x` go through all three routers in turn. On the same machine, on 2026-11-17: 6,090 requests during the two rollouts, which took 11 and 14 seconds, and none failed or reached the wrong app. `scripts/integration-mixed.sh` checks the table a router of a build before path routes is given, with curl and a worker's node certificate in place of such a router, since no release without paths exists to run.

For h2c routes it runs a server of gRPC's wire format, `test/h2ctest`, under `/h2ctest.Echo` on the same host. It speaks only HTTP/2 without TLS, as a gRPC server does, and is built in the test from Go's standard library and imported into each node, so the test needs no gRPC image; the router never reads a message, so gRPC's framing, its content type and its trailers are what the test has to carry, and protobuf would add nothing. Through each router it makes a unary call, a server stream of five messages 50 ms apart and a bidirectional stream in lockstep, over HTTP/2, and a unary call over HTTP/1.1, and checks the messages, that they come from one replica, and `grpc-status` 0 in the trailers. Then it rolls the server over while a client makes those three calls through the three routers in turn. On one EC2 c8g.4xlarge, on 2026-11-19: 360 calls during the rollout, which took 11 seconds, 120 of each kind, and none failed. `scripts/integration-mixed.sh` checks the tables a router before paths and a router before protocols are given with h2c routes, and the warnings, with curl in place of such routers.
