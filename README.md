<img src="assets/logo1.png" width="96" alt="">

# hivemind

![Test](https://github.com/ao/hivemind/workflows/Test/badge.svg)
![Go 1.26](https://img.shields.io/badge/Go-1.26-blue)
[![License: GPL v3](https://img.shields.io/badge/License-GPLv3-blue.svg)](LICENSE)

hivemind runs containers on a few Linux machines, using Docker Engine on each one. You describe an app with flags or a YAML file. hivemind keeps the requested number of replicas running, and starts a replacement when a container exits or fails its health check.

Version 2 is a rewrite of version 1, and 2.2.0 is the current release. A cluster has one server and any number of workers. A worker joins with a token, and the server and the worker then talk over TLS with certificates from the cluster's own CA.

Any node can run an HTTPS router that gets certificates from Let's Encrypt or another ACME CA, routes requests by host and path prefix, and speaks HTTP/2 without TLS to gRPC servers. `hivemind compose` deploys a Docker Compose file, `node drain` moves the replicas off a machine before it leaves, `registry login` lets the nodes pull from private registries, `backup` and `restore` bring a lost server back on a new machine, at a new address too, and `ca rotate` replaces the cluster CA without joining the machines again.

hivemind is for 1 to 10 Linux machines on one network. It has one server, or in the unreleased 2.3.0 three or five ([several servers](docs/ha.md)), publishes container ports on host ports or behind its HTTPS router, and has no overlay network, no built-in DNS and no tenancy. Docker Swarm, Nomad and k3s do much more than that. hivemind is smaller to learn and run, and it is not a replacement for them. The automated cluster test runs three nodes.

## Install

Download a binary from the [latest release](https://github.com/ao/hivemind/releases/latest) and check it against `SHA256SUMS`. On Linux amd64 (use `arm64` on ARM):

```sh
curl -fLO https://github.com/ao/hivemind/releases/latest/download/hivemind-linux-amd64
curl -fLO https://github.com/ao/hivemind/releases/latest/download/SHA256SUMS
grep ' hivemind-linux-amd64$' SHA256SUMS | sha256sum -c -
sudo install -m 0755 hivemind-linux-amd64 /usr/local/bin/hivemind
hivemind version
```

The `grep … | sha256sum -c -` form works with GNU coreutils and with BusyBox (Alpine). Each release has these assets:

```
hivemind-linux-amd64        node and client
hivemind-linux-arm64        node and client
hivemind-darwin-amd64       client only
hivemind-darwin-arm64       client only
hivemind-windows-amd64.exe  client only
hivemind-windows-arm64.exe  client only
SHA256SUMS
```

To build from source, use Go 1.26 or later:

```sh
git clone https://github.com/ao/hivemind.git
cd hivemind
CGO_ENABLED=0 go build -trimpath -o hivemind ./cmd/hivemind
```

A server or a worker (`hivemind serve`, `hivemind join`) needs Linux, amd64 or arm64, and Docker Engine 25 or newer. The other platforms can run the client against a server.

## Quick start

These commands run on one machine and use only loopback addresses. They assume `hivemind` is on your PATH and that you can reach the Docker socket. [docs/quick-start.md](docs/quick-start.md) explains each step and shows the output.

```sh
# Terminal 1. init creates the data directory and prints the admin token once.
hivemind init

# Terminal 1, and keep it running.
hivemind serve

# Terminal 2. --wait returns once the replica runs and is ready; the first pull
# can take a while.
hivemind deploy --name web --image nginx:alpine --port 127.0.0.1:8080:80 --wait
hivemind status
curl -fsS http://127.0.0.1:8080/ > /dev/null
hivemind logs web

# worker publishes no port. Two replicas of an app with a host port would clash.
hivemind deploy --name worker --image nginx:alpine
hivemind scale worker 2 --wait
hivemind status worker
hivemind delete worker
hivemind delete web
```

`init` writes a client config, so the CLI on the same machine finds the server and the token without any environment variables.

## Add a machine

A worker is a Linux machine with Docker Engine and the hivemind binary. The server must run with `--advertise`, set to the address the worker uses to reach it. The worker must be able to reach that address on ports 4484 and 4485.

```sh
# On the server. Stop `hivemind serve` from the quick start, then start it with --advertise.
hivemind serve --advertise 10.0.0.5

# On the server, in another terminal. The token goes to stdout, and its expiry to stderr.
hivemind join-token

# On the worker. Use the same address as --advertise, and the token from join-token.
hivemind join 10.0.0.5 --token hivemind-join-v1.<fingerprint>.<secret>
```

`join` stays in the foreground and runs the worker's agent. Stop it with Ctrl-C. Its containers keep running. To start it again later, run `hivemind serve --role worker` with the same data directory. `join` writes the worker flags it was given, such as `--route-addr` and `--acme-email`, to `<data-dir>/hivemind.yaml`, so the restart keeps them, the router included. To run the server and the workers as services that come back after a reboot, see [systemd](docs/systemd.md).

A token works once, and it expires after 24 hours. `hivemind join-token --ttl 1h` sets a shorter lifetime, up to 168 hours. Deploy as before. The NODE column of `hivemind status` shows where each replica runs.

`--advertise` can be a DNS name instead of an IP address, and the workers join that name. Then a server restored on a machine with another IP address needs only the name's record changed. See [the server's address](docs/concepts.md#the-servers-address).

`hivemind nodes` lists the nodes. To take a machine out of the cluster, run `hivemind node drain <id>` on the server, which moves its replicas to other nodes, each new copy starting before the old one stops, then `hivemind node remove <id>`. The server refuses the node's certificate from then on. In 2.0.0, which has no `node drain`, stop hivemind on the machine before `node remove`. See [concepts](docs/concepts.md#draining-a-node).

## Compose files

`hivemind compose deploy compose.yaml` makes an app of each service in a Docker Compose file, and `hivemind compose convert compose.yaml` prints the app specs instead. Only part of Compose is read, and any other key is refused with a reason. A port with no address is published on `127.0.0.1`, not on every interface as Compose does, and apps do not find each other by service name. See [docs/compose.md](docs/compose.md).

## HTTPS routing

An app can name `routes: [{host: www.example.com, port: 80}]`, and a node started with `--route-addr 0.0.0.0 --acme-email you@example.com` runs a router on ports 80 and 443. It gets a certificate for each routed host from Let's Encrypt (or the CA in `--acme-ca`), and sends each request to a Ready replica of the app on any node. Any node can run a router, and DNS can list any of them; the routers share their certificates through the server. A rolling update of a routed app keeps it serving, on one machine too. A route can take only a path prefix of its host, such as `/api`, so several apps can share a host, and `hivemind routes` prints the table. A route with `protocol: h2c` gets HTTP/2 without TLS from the routers, as gRPC servers need. Hosts are exact names: no wildcards, and no TCP or UDP routing. See [docs/routing.md](docs/routing.md).

## Network and authentication

- The API listens on `127.0.0.1:4483`. Every route except `GET /healthz` needs the admin token that `init` prints. A non-loopback `--api-addr` needs TLS as well. With a certificate from a private CA, give the CLI that CA with `--ca-cert` or `HIVEMIND_CA_CERT`.
- `serve --advertise` opens two more ports, on that address only. Port 4484 is the node API. It accepts only a machine that holds a certificate from the cluster CA. Port 4485 takes join requests, and each one needs a join token.
- In a cluster of several servers (unreleased 2.3.0), each server also opens the server port, 4486, on the same address: TLS 1.3 with client certificates of the server role only, for Raft and for the requests followers pass to the leader. Allow it between the servers only ([security](docs/security.md#several-servers)).
- `--route-addr` opens the router, on ports 80 and 443. It is the one listener that may bind `0.0.0.0`, and it has no authentication: it serves the routed apps' public traffic.
- Nothing else listens. Container ports are published on `127.0.0.1` unless the spec names another address. A routed app's ports are published on ports 30000 to 32767 of the node's private address, for the other nodes' routers; keep that network to the nodes, or firewall those ports.

[docs/security.md](docs/security.md) lists every listener and file, and what a compromised worker can and cannot do.

## Limits

- By default the server is one machine. If it goes down, running containers keep running, but nothing new is placed and lost nodes are not rescheduled. In the unreleased 2.3.0, a cluster can have three or five servers instead, which keep working while a majority of them is up ([several servers](docs/ha.md)); with two, losing either stops writes. All servers run the same release, a CA rotation needs a cluster of one, and a server that dies stays in the configuration until `hivemind server remove`. `hivemind backup` and `hivemind restore` bring it back on a new machine. The new machine can have any address when the server advertises a DNS name; when it advertises an IP address, give the new machine that address, or run `hivemind node set-server` on each worker. See [backup](docs/backup.md).
- A node certificate lasts 365 days, and nodes renew theirs on day 243. A 2.0.0 node cannot renew, so upgrade it within a year of joining (see [upgrading](docs/upgrading.md#from-200-to-220)). The only way to revoke one is to remove the node. The cluster CA lasts ten years; `hivemind ca rotate` replaces it in place, without joining machines again, and a 2.0.0 worker must be upgraded or joined again for that. See [CA rotation](docs/ca-rotation.md).
- A replica with a volume stays on its node. If that node is lost, the app is down until the node returns.
- `deploy` does not reject a host port that another app already publishes. The second app's replicas go where the port is free. Any that cannot go stay `pending`, with the reason. `deploy` prints a warning for each of them that names the app holding the port, and `deploy --wait` exits 1 at once when running replicas hold the port on every node that could take it. See [concepts](docs/concepts.md#placement).
- While a cluster is upgraded from 2.0.0, `hivemind logs` reads a replica on a worker only once the server and that worker both run 2.2.0. Until then, use `docker logs` on the worker.
- hivemind never deletes a volume, including when its app is deleted. Remove unused volumes with `docker volume rm` on the node.
- In a rolling update on one node, each replica of an app without routes is down while its new container starts, whether or not it has host ports. With a second eligible node, the new copy starts there first. A routed app's new copy always starts beside the old one. See [concepts](docs/concepts.md#rolling-updates).
- There is no web UI. The CLI and the HTTP API are the interfaces. The API has login and logout routes for a browser session, with a CSRF token.

## Documentation

- [Quick start](docs/quick-start.md)
- [Concepts](docs/concepts.md)
- [Compose files](docs/compose.md)
- [HTTPS routing](docs/routing.md)
- [Private registries](docs/registries.md): `hivemind registry login` for GHCR, ECR, Docker Hub and registries of your own
- [Security](docs/security.md)
- [Backup and restore](docs/backup.md)
- [Several servers](docs/ha.md): a cluster of three or five servers, with Raft
- [When things go wrong](docs/failures.md): Docker restarts, failing pulls, full disks, crash loops, damaged worker files, clock skew
- [Running under systemd](docs/systemd.md)
- [Soak test on four machines](docs/soak.md)
- [Scale test on ten machines](docs/scale.md): 100 apps, 500 replicas
- [CLI reference](docs/cli.md)
- [HTTP API](docs/api.md)
- [Upgrading from 2.0.0 or v1](docs/upgrading.md)
- [Roadmap](ROADMAP.md)
- [Assessment of v1](docs/ASSESSMENT.md) and [the v2 plan](docs/PLAN.md)

## Development

```sh
make test              # go test -race -count=1 ./...
make docs              # regenerate docs/cli.md
make quickstart-check  # run the quick start above against a node in Docker-in-Docker
```

`scripts/integration-cluster.sh` runs three nodes in Docker-in-Docker and checks placement and node loss. It needs Docker.

## License

hivemind is licensed under the GNU General Public License v3.0. See [LICENSE](LICENSE).
