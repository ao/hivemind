<img src="assets/logo1.png" width="96" alt="">

# hivemind

![Test](https://github.com/ao/hivemind/workflows/Test/badge.svg)
![Go 1.26](https://img.shields.io/badge/Go-1.26-blue)
[![License: GPL v3](https://img.shields.io/badge/License-GPLv3-blue.svg)](LICENSE)

hivemind runs containers on a few Linux machines, using Docker Engine on each one. You describe an app with flags or a YAML file. hivemind keeps the requested number of replicas running, and starts a replacement when a container exits or fails its health check.

This is version 2, a rewrite of version 1. Version 2.0.0 is not released yet. The current build runs one server and any number of workers. A worker joins with a token, and the server and the worker then talk over TLS with certificates from the cluster's own CA.

hivemind is for 1 to 10 Linux machines on one network. It has one server, publishes container ports on host ports, and has no overlay network, no built-in DNS and no tenancy. Docker Swarm, Nomad and k3s do much more than that. hivemind is smaller to learn and run, and it is not a replacement for them. The automated cluster test runs three nodes.

## Install

The first v2 release is pending. When it is published, its assets are named as the release workflow names them:

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

# Terminal 2.
hivemind deploy --name web --image nginx:alpine --port 127.0.0.1:8080:80
hivemind status
curl -fsS http://127.0.0.1:8080/ > /dev/null
hivemind logs web

# worker publishes no port. Two replicas of an app with a host port would clash.
hivemind deploy --name worker --image nginx:alpine
hivemind scale worker 2
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

`join` stays in the foreground and runs the worker's agent. Stop it with Ctrl-C. Its containers keep running. To start it again later, run `hivemind serve --role worker` with the same data directory.

A token works once, and it expires after 24 hours. `hivemind join-token --ttl 1h` sets a shorter lifetime, up to 168 hours. Deploy as before. The NODE column of `hivemind status` shows where each replica runs.

## Network and authentication

- The API listens on `127.0.0.1:4483`. Every route except `GET /healthz` needs the admin token that `init` prints. A non-loopback `--api-addr` needs TLS as well.
- `serve --advertise` opens two more ports, on that address only. Port 4484 is the node API. It accepts only a machine that holds a certificate from the cluster CA. Port 4485 takes join requests, and each one needs a join token.
- Nothing else listens. Container ports are published on `127.0.0.1` unless the spec names another address.

[docs/security.md](docs/security.md) lists every listener and file, and what a compromised worker can and cannot do.

## Limits

- The server is one machine. If it goes down, running containers keep running, but nothing new is placed and lost nodes are not rescheduled.
- A node's certificate cannot be revoked or renewed, and there is no command to remove a node. A certificate lasts 365 days. See [security](docs/security.md#no-revocation-yet).
- A replica with a volume stays on its node. If that node is lost, the app is down until the node returns.
- `deploy` does not reject a host port that another app already publishes. The second app's replicas go where the port is free. Any that cannot go stay `pending`, with the reason. See [concepts](docs/concepts.md#placement).
- `hivemind logs` cannot read replicas that run on a worker. Use `docker logs` on that machine.
- hivemind never deletes a volume, including when its app is deleted. Remove unused volumes with `docker volume rm` on the node.
- In a rolling update on one node, each replica is down while its new container starts. With a second eligible node, the new copy starts there first. See [concepts](docs/concepts.md#rolling-updates).
- The CLI cannot trust a private CA for an API served with TLS.
- There is no web UI. The CLI and the HTTP API are the interfaces.

## Documentation

- [Quick start](docs/quick-start.md)
- [Concepts](docs/concepts.md)
- [Security](docs/security.md)
- [CLI reference](docs/cli.md)
- [HTTP API](docs/api.md)
- [Upgrading from v1](docs/upgrading.md)
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
