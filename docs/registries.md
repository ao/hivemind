# Private registries

`hivemind registry login` stores a username and password, or a token, for an image registry. Each node then sends it when it pulls an image from that registry. This is new in the unreleased 2.2.0.

```sh
printf '%s' "$TOKEN" | hivemind registry login ghcr.io --username octocat --password-stdin
hivemind deploy --name api --image ghcr.io/acme/api:1.4
hivemind registry list
hivemind registry logout ghcr.io
```

The password comes from standard input (`--password-stdin`) or a file (`--password-file`). No flag takes it on the command line, where the process list shows it and the shell history keeps it. One trailing newline is removed. `--password-stdin` refuses a terminal, which would echo what is typed. The credential is not checked with the registry when it is stored: a wrong one shows at the next pull. `registry list` shows the host, the username and when the credential was last stored. Nothing shows the password again. The API routes are in [the API](api.md#registries).

## Which registry an image is from

hivemind reads the registry from an image reference the way Docker does. The first part of the name is a registry if it has a dot or a colon, is `localhost`, or has an upper-case letter. Otherwise the image is on Docker Hub.

| Image | Registry |
|---|---|
| `nginx`, `acme/api:1`, `docker.io/acme/api` | `docker.io` |
| `index.docker.io/acme/api`, `registry-1.docker.io/acme/api` | `docker.io` |
| `ghcr.io/acme/api:1.4` | `ghcr.io` |
| `registry.example.com:5000/team/api@sha256:...` | `registry.example.com:5000` |
| `localhost:5000/api` | `localhost:5000` |
| `myregistry/api` | `docker.io`: `myregistry` is a Docker Hub namespace here |

Hosts are compared in lower case, and the port is part of the host: `registry.example.com` and `registry.example.com:443` are two registries. `registry login myregistry` is refused, since no image could name it. There is one credential per registry, used for every image from it, by every app.

## When a credential is used

A node pulls an image only when its Docker daemon does not have it. The agent then sends the credential stored for the image's registry, if there is one, to the node's Docker daemon in the `X-Registry-Auth` header of `POST /images/create`, as the [Docker Engine API](https://docs.docker.com/reference/api/engine/version/v1.44/#tag/Image/operation/ImageCreate) asks, and the daemon sends it to the registry.

A login, a second login that replaces a credential, and a logout affect the next pull only. Running containers keep running. Docker keeps the images it has pulled, so a new replica on a node that already has the image starts without a pull and without a credential, after a logout too. It follows that any app on a node can run an image that another app pulled there. hivemind never pulls an image the node already has.

A pull the registry refuses leaves the replica `pending`, and `hivemind status` says which registry refused it and what to run:

```
hivemind-api-0-g1: pulling ghcr.io/acme/api:1.4: ghcr.io wants a credential, and none is stored for it; run: hivemind registry login ghcr.io; next pull after 10:04:20 UTC
hivemind-api-0-g1: pulling ghcr.io/acme/api:1.4: ghcr.io refused the stored credential (unauthorized); run: hivemind registry login ghcr.io; next pull after 10:04:20 UTC
```

The message has neither the username nor the password, and none of what the registry said once a credential was sent, since a registry's error can carry the username in a token URL. Docker Hub gives one answer for an image that does not exist and one that is private, so its message says "no such image, or it is private".

A failed pull is not tried again at once. The node waits 10 seconds, then 20, 40 and so on up to 5 minutes, for that image and credential, counted from the end of the pull. A new login or a logout changes the credential, so the next pull goes at the next pass, within about 2 seconds. Before 2.2 the agent pulled again on every pass. A pull that takes more than 2 seconds goes on in the background, while the node supervises its other replicas, and the status says `pulling <image>, started <time>; the registry has not answered yet`.

## Docker Hub

Docker Hub is `docker.io`. `index.docker.io` and `registry-1.docker.io` are the same host. Use an access token rather than the account password:

```sh
printf '%s' "$DOCKERHUB_TOKEN" | hivemind registry login docker.io --username <user> --password-stdin
```

A Docker Hub credential is sent with every pull from Docker Hub, public images included, as `docker pull` does after `docker login`. Docker Hub limits anonymous pulls more than authenticated ones.

## GitHub Container Registry

GHCR takes your GitHub username and a personal access token (classic) with the `read:packages` scope, which is what GitHub's documentation for the container registry asks for.

```sh
printf '%s' "$GHCR_TOKEN" | hivemind registry login ghcr.io --username <github-user> --password-stdin
```

## Amazon ECR

The registry is `<account>.dkr.ecr.<region>.amazonaws.com`, the username is `AWS`, and the password is a token from `aws ecr get-login-password`:

```sh
aws ecr get-login-password --region <region> |
  hivemind registry login <account>.dkr.ecr.<region>.amazonaws.com --username AWS --password-stdin
```

An ECR token lasts 12 hours. hivemind has no credential helper and does not renew it, so run the login again before it expires, for example every 6 hours from cron on the server:

```
0 */6 * * * aws ecr get-login-password --region <region> | /usr/local/bin/hivemind registry login <account>.dkr.ecr.<region>.amazonaws.com --username AWS --password-stdin
```

cron's `PATH` does not include `/usr/local/bin`, so the job names `hivemind` in full, as in [backup](backup.md#taking-a-backup); name `aws` in full too if it is installed there. If `aws` fails, `registry login` gets no password, refuses it, and keeps the stored credential. The cron job needs the CLI's server and token: run it as the user that ran `hivemind init`, whose client config names them, or set `HIVEMIND_SERVER` and `HIVEMIND_TOKEN`. Once a token has expired, a pull says `refused the stored credential (the token has expired)`. Replicas that run already are not affected.

## A registry of your own

A registry with its own CA, such as `registry:2` with htpasswd, needs two things on every node, because each node's Docker daemon makes the TLS connection:

1. The CA certificate at `/etc/docker/certs.d/<host:port>/ca.crt`, for example `/etc/docker/certs.d/registry.example.com:5000/ca.crt`. Docker reads it at each pull, so the daemon needs no restart. hivemind does not copy CA certificates to nodes.
2. A login for `<host:port>`, once, on the server: `hivemind registry login registry.example.com:5000 --username <user> --password-stdin`.

Docker treats a registry on `127.0.0.0/8`, such as `localhost:5000`, as insecure by default, and then also talks to it over plain HTTP, credential included. Any other registry must have TLS that the daemon trusts.

`scripts/integration-docker.sh` tests this: `registry:2` with htpasswd and a certificate from a CA made for the run, trusted through `certs.d`. Without a login the replica stays `pending` with the login message; after `registry login` it runs; after `registry logout` a new replica's pull fails again; and with a wrong password the message says the credential was refused. The test then checks that neither password is in serve's log, the Docker daemon's log, `status`, or any API reply.

## What is stored where

- The server keeps each credential in `hivemind.db` (mode 0600, in a 0700 directory): the host, the username and the password as given, in plain text, the way it keeps environment values. The Docker daemon needs the password as it is, so there is nothing to hash. `hivemind backup` includes it.
- No API route returns a password. `GET /v1/registries` returns the host, the username and the times.
- A credential goes to a node only with the assignment of a replica placed there that is to run an image from that registry, over the node API's mutual TLS. A node that runs nothing from the registry never gets it. The worker keeps it in memory, and hands it to its own Docker daemon over the local socket for each pull. hivemind writes it to no file on a worker, and the Docker daemon does not store it.
- hivemind logs no credential. A pull's log line says only whether a credential was sent (`logged_in=true`).

Treat a credential as exposed to every machine that runs an image from its registry, as with environment values, and use a token that can only read. [Security](security.md#registry-credentials) has what a compromised worker can do with one.

## Workers older than 2.2

A 2.1 worker decodes its assignments with a decoder that refuses unknown fields. Had the server added a credential to one assignment, the worker would refuse the whole list and stop reconciling every replica on the node. So a 2.2 worker asks for credentials, with `?registry_auth=1` on its assignments request, and the server sends them only to a node that asks. A 2.1 worker gets none: it pulls a private image without a credential, the pull fails with Docker's message, and its other replicas are not affected. Placement does not know which workers can pull a private image, so upgrade the workers before you deploy one, or drain the old ones. A 2.2 worker with a 2.1 server asks and gets nothing, because that server has no credentials.
