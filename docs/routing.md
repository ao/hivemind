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

`host` is an exact DNS name, and `port` is the container port the app listens on. Any node can run a router, the server included, and DNS for `www.example.com` can point at any or all of them. Every router serves every routed host.

## Decisions

These were made by the owner on 2026-11-14.

1. A router on every node that opts in, the server included, rather than one ingress node. DNS for a host can then list several machines, and losing one of them does not take the host down. The routers share their certificates and ACME state through the server, so a host has one certificate whichever router a client reaches.
2. The router is the one listener in hivemind that may bind `0.0.0.0`. It serves the apps' public traffic, so it must be reachable from the internet, and it has no authentication of its own: the apps behind it decide who may do what. `--route-addr` has no default, and without it there is no router. Every other listener still refuses a wildcard address.
3. A routed app publishes each route's container port on an ephemeral host port on its node's private address, not on loopback, so a router on any node can reach a replica on any node. Because the host port is ephemeral, several replicas of one routed app can share a node.
4. This reverses part of decision 2 in [PLAN.md](PLAN.md), which scoped hivemind to host-port networking with no proxy. Host ports work as before, and an app without routes is unchanged. Routed apps go through the router.

## How a request is routed

1. The server places a routed replica only on a node that reported a private address. The server's own node uses its `--advertise` address, or `127.0.0.1` when it has none (then there are no workers). A worker uses the local address of its connection to the server, which it sends when it registers; the server refuses one that is not an IP address, or is loopback, a wildcard or multicast. A worker older than 2.2 sends none, and takes no routed replicas.
2. The agent creates the container with each route port published on an ephemeral port of that address, and reports the port Docker chose. It reads the port again after every start, because Docker picks a new one each time.
3. The server builds the routing table from the reports: for each host, the `address:port` of every replica whose container is Ready (running, and passing its health check if it has one, the same signal a rolling update waits for), is the container its replica or surge is assigned, and is on a node whose lease is current. `GET /v1/routes` shows it.
4. Each router reads the table once a second: the server's from the store, a worker's over the node API. A router that cannot read it keeps serving the last table it read.
5. The router sends each request to the host's replicas in turn. A replica that cannot be dialled within 2 seconds is skipped for 3 seconds, and the request goes to the next one, with its body if none of it was sent. A request that fails after it reached a replica gets `502`.

The router sets `X-Forwarded-For` to the client's address, `X-Forwarded-Host` to the request's host and `X-Forwarded-Proto` to `https`, and drops the client's own values of those headers, `Forwarded` and `X-Real-Ip`. The `Host` header is the client's. Clients can use HTTP/1.1 or HTTP/2; the router speaks HTTP/1.1 to the replicas.

| Request | Reply |
|---|---|
| A host in the table, with a Ready replica | The replica's reply |
| A host in the table, with no Ready replica | `503` |
| A host not in the table, on the http port | `404`, with the body `not found`. No host is listed |
| A TLS handshake for a name not in the table | The handshake fails: there is no default certificate |
| A `Host` header that is not the name the client asked for in TLS (SNI) | `421` |
| Any request to a known host on the http port | `308` to the same path on https, unless it is an ACME HTTP-01 challenge |

## Rolling updates

A rolling update of a routed app moves one replica at a time, as for any app (see [concepts](concepts.md#rolling-updates)), and every move is a surge: the new copy starts beside the old one, on another node or on the same node, because a routed app holds no fixed host port. The new copy enters the table when it is Ready, and only then does the rollout move the replica to it. At that point the old copy leaves the table, and its node's agent keeps it running for 5 more seconds before it stops it, so that every router has read the new table and the requests already sent to the old copy finish. So a routed app keeps serving through an update, on one machine as on several, as long as each new copy becomes Ready. This closes, for routed apps only, the gap that host-port apps have on one machine.

A drain moves a routed replica the same way, to another node.

## Certificates

Certificates come from [certmagic](https://github.com/caddyserver/certmagic), over ACME, from the CA in `--acme-ca`: Let's Encrypt's production directory by default. `--acme-email` is the account's contact, and is required with `--route-addr`; registering the account accepts the CA's subscriber agreement. The router asks for a certificate for each host in the table and for no other name: there is no on-demand issuance, wildcards are not supported, and IP addresses are refused. certmagic renews each certificate before it expires, and retries a failed issuance with backoff. With the Let's Encrypt production CA, certmagic tries a retried issuance against Let's Encrypt's staging CA first, to spare the production rate limits.

HTTP-01 is the only challenge. The CA connects to port 80 of the host's address, so `--route-http-port` must be reachable as port 80 from the internet on at least one node that DNS lists. Any router can answer a challenge another router started, since the tokens are in the shared storage. A challenge for a host that a router has not read in the table yet makes it read the table at once, at most once a second, so the first validation of a new host does not fail on a router that lags the one that ordered the certificate. TLS-ALPN-01 is off, and DNS-01 is not offered. OCSP stapling is off: Let's Encrypt ended its OCSP service in 2025.

Certificates, their private keys, the ACME account and its key, and the challenge tokens are stored in the server's database, in a key/value table, and every router reads and writes them there: the server's router directly, a worker's over the node API. A router that receives a challenge another router started answers it from that table. Issuing a certificate takes a lock in the same database, so when every router asks for a new host's certificate at once, one of them issues it and the others find it stored. A lock is held for 30 seconds at a time and renewed every 10 seconds while its holder works; a holder that dies loses it 30 seconds after its last renewal. This follows certmagic's Storage documentation ("Processes running in a cluster should use the same Storage value") and its distributed HTTP-01 solver, "which allows the ACME HTTP-01 and TLS-ALPN challenges to be solved by an instance other than the one which initiated it". The ACME state is in the database, so `hivemind backup` carries it and `hivemind restore` brings it back.

## DNS

Point each routed host at the public address of one or more nodes that run a router: an `A` record (and an `AAAA` record for a router on an IPv6 address) per node. Every listed node must accept connections on ports 80 and 443, because the CA's validation and clients may reach any of them. A node that is down and still listed fails the requests that reach it; take it out of DNS, or put the routers behind a load balancer that checks them.

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

Once the certificate is issued, `https://www.example.com/` answers from any of the three machines. In the integration test, against Pebble with its validation delays off, all three routers served the host 7 seconds after the deploy (three Docker-in-Docker nodes on one EC2 c8g.4xlarge); a real CA takes as long as it takes. A second deploy that changes the spec rolls the replicas over without a failed request.

The settings have `HIVEMIND_` variables and config file keys, as the other settings: `HIVEMIND_ROUTE_ADDR`, `HIVEMIND_ROUTE_HTTP_PORT`, `HIVEMIND_ROUTE_HTTPS_PORT`, `HIVEMIND_ACME_EMAIL`, `HIVEMIND_ACME_CA`, and `route-addr` and so on in `hivemind.yaml`. See the [command reference](cli.md).

## Metrics

The server's `/metrics` has its own router's counters, and the expiry of the certificate it serves for each host:

- `hivemind_router_requests_total{host,code}`, where a host not in the table counts as `unknown`;
- `hivemind_router_upstream_errors_total{host,kind}`, with `kind` `dial` (the router tried the next replica) or `proxy` (the request failed after it reached one);
- `hivemind_router_cert_expiry_timestamp_seconds{host}`.

A worker has no metrics listener, so its router's counters are not exported yet.

## Limits

- Hosts are exact names. No wildcards, no IP addresses, at most 16 routes per app, and a host belongs to one app: a deploy that claims a host another app routes gets `409`, naming that app. Delete the route from that app first.
- No path routing, and no TCP or UDP routing. A route sends every path of its host to one container port.
- HTTP/1.1 to the replicas. gRPC and other HTTP/2-only backends are not supported.
- HTTP-01 only, so the CA must reach port 80 on a node in DNS.
- Request headers are limited to 64 KiB, and so are a replica's response headers. A request, body included, must be read within 5 minutes; a replica must start its reply within 60 seconds. A reply streams for as long as the replica sends it.
- The table a router serves is up to a second old, longer while it cannot reach the server. For up to 15 seconds after a node is lost, its replicas stay in the table and their dials fail; each such request costs up to 2 seconds before it goes to another replica.
- A request whose body has started to go to a replica is not retried on another one.
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
