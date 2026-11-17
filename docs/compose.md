# Compose files

`hivemind compose` reads a Docker Compose file and turns each service into an app spec. The work is done by the CLI: the server receives ordinary app specs, as from `hivemind deploy -f`, and knows nothing of Compose. This is new in the unreleased 2.2.0.

```sh
hivemind compose convert compose.yaml   # print the app specs as YAML
hivemind compose deploy compose.yaml    # create or update the apps
hivemind compose delete compose.yaml    # delete the apps
```

Only part of the [Compose specification](https://github.com/compose-spec/compose-spec/blob/main/spec.md) is read. Each key that is read maps onto a field of the app spec, and any other key is refused with its path and a reason. Nothing is dropped without a word: a file either converts completely, or the command fails and lists every problem.

## Commands

`convert` prints one YAML document per service, sorted by service name. Each document is an app spec that `hivemind deploy -f` accepts. It sends nothing to the server.

`deploy` converts the file and validates every app before it sends anything, and checks who deployed each app (below). Then it creates or updates each app in name order, as `hivemind deploy -f` does, and prints a line for each. If the server refuses an app, `deploy` stops there; the apps before it stay deployed, and the error names them.

Each app records the project that deployed it: the file's top-level `name`, or else the name of the file's directory, lowercased and without the characters a project name cannot have, as Docker Compose takes it (the working directory for `-`). `deploy` updates only an app that the same project deployed, so running it again is safe. Before the first write it reads every app of the file from the server. If an app of the same name exists that `hivemind deploy`, the API or another project deployed, the whole file is refused, every such app is named, and nothing is deployed:

```
error: conflict: app web was deployed with hivemind deploy or the API, not from compose project shop; hivemind compose deploy --replace (replace=true in the API) updates it anyway; nothing was deployed
```

The server checks the project again at each write, so an app that another deploy takes after that first read is still refused; then `deploy` stops at it, and the error names the apps already deployed, as for any other refusal.

`--replace` updates such an app, and the project then owns it. `hivemind deploy` can still update an app a project deployed, and the app stays the project's. Apps deployed with `compose deploy` before this check have no project, so the first `deploy` after an upgrade needs `--replace`.

`delete` deletes the app for each service, in name order. It reads only the service names, so the file's variables need not be set, and keys that `convert` would refuse do not matter. An app that does not exist is reported as `not found` and skipped. Volumes are kept, as with `hivemind delete`.

All three take `--prefix NAME`, which names each app `NAME-<service>` instead of `<service>`. Every app name must follow the app spec's rules, 1 to 32 lowercase letters, digits and dashes, starting and ending with a letter or digit, or the command fails and names the service. Compose allows service names that hivemind does not, such as `my_db`.

`deploy` and `delete` take the client flags that `deploy` takes: `--server`, `--token`, `--client-config`, `--ca-cert` and `--json`. See the [CLI reference](cli.md#hivemind-compose).

`-` in place of the file reads it from standard input, and then `.env` is read from the working directory. The compose file and the `.env` file are each limited to 1 MiB, the same as `deploy -f`.

Errors name the file and list the problems sorted by path:

```
error: compose.yaml: 2 problems:
  services.web.build: not supported: build and push the image, then name it in image
  services.web.depends_on: not supported: hivemind starts each app on its own, and apps reach each other only through published ports
```

No error message contains a value from an `environment` section or the value of a variable, since those may be secrets.

## Ports are published on 127.0.0.1

A port with no address is published on `127.0.0.1`, as every hivemind port is by default. Compose publishes such a port on every interface. `convert` and `deploy` print a note on standard error for each port this applies to:

```
note: services.web.ports[0]: published on 127.0.0.1:8080 only, the hivemind default; Compose would publish it on every interface. To publish elsewhere, write "0.0.0.0:8080:80" or another address
```

An address in the file is kept as written, `0.0.0.0` included.

## No service discovery

Apps do not find each other by service name. hivemind has no networks between apps and no DNS for them, so `depends_on`, `links` and `networks` are refused, and a hostname such as `db` in a connection string resolves to nothing. Each app starts on its own, in no particular order.

One app reaches another through a published port on an address its containers can reach. `127.0.0.1` inside a container is the container itself, so a port published on the node's `127.0.0.1` is reachable from the node's own processes but not from other containers. Publish the port on the node's private address instead, and pass that address to the client, for example with a variable from `.env`. The [worked example](#worked-example) does this. An app with a volume stays on the node where it was first placed, so its address does not change. See [Ports](concepts.md#ports) and [Placement](concepts.md#placement) in the concepts.

## What is read

### Top level

| Key | Handling |
|---|---|
| `services` | Required, with 1 to 100 services. |
| `volumes` | Named volumes. Each is `{}` or empty; any setting is refused (see [refusals](#refusals)). |
| `version` | Ignored. The Compose specification calls it obsolete. |
| `name` | The Compose project name, which `deploy` records with each app (see [commands](#commands)). It is 1 to 64 lowercase letters, digits, dashes and underscores, starting with a letter or digit, and may come from a variable. It does not change the app names; use `--prefix` for that. |
| `x-*` | Ignored. |

### Service keys

| Key | App spec | Handling |
|---|---|---|
| `image` | `image` | Required. |
| `command` | `command` | A list, or a string split into words by shell quoting rules (below). |
| `environment` | `env` | A mapping or a list of `KEY=VALUE`. |
| `ports` | `ports` | Short and long syntax (below). |
| `volumes` | `volumes` | Named volumes from the top level (below). |
| `deploy.replicas` | `replicas` | At least 1. An app with a volume can have only 1. |
| `deploy.mode` | | `replicated` only. |
| `deploy.resources.limits.cpus` | `resources.cpu` | A number or a string, such as `0.5`. |
| `deploy.resources.limits.memory` | `resources.memory` | A byte value (below). |
| `deploy.resources.reservations` | | `cpus` and `memory` only, and each equal to its limit or left out. hivemind uses one figure for the reservation and the limit. |
| `deploy.restart_policy.condition` | | `any` only. |
| `healthcheck` | `health` | An exec check (below). |
| `restart` | | `always` or `unless-stopped`. |
| `x-*` | | Ignored. |

The sections of the specification are [services](https://github.com/compose-spec/compose-spec/blob/main/spec.md#services-top-level-element), [image](https://github.com/compose-spec/compose-spec/blob/main/spec.md#image), [command](https://github.com/compose-spec/compose-spec/blob/main/spec.md#command), [environment](https://github.com/compose-spec/compose-spec/blob/main/spec.md#environment), [ports](https://github.com/compose-spec/compose-spec/blob/main/spec.md#ports), [volumes](https://github.com/compose-spec/compose-spec/blob/main/spec.md#volumes), [healthcheck](https://github.com/compose-spec/compose-spec/blob/main/spec.md#healthcheck), [restart](https://github.com/compose-spec/compose-spec/blob/main/spec.md#restart), and [deploy](https://github.com/compose-spec/compose-spec/blob/main/deploy.md) in its own file.

#### command

A list is used as it is; numbers in it become strings. A string is split into words as a POSIX shell splits them, and nothing in it is expanded:

- spaces, tabs and newlines outside quotes separate words;
- `'single quotes'` keep everything up to the next single quote;
- in `"double quotes"`, a backslash escapes `$`, `` ` ``, `"`, `\` and a newline, and is kept before any other character;
- outside quotes, a backslash keeps the next character.

`;`, `|`, `*` and the other shell operators are ordinary characters, so `command: echo a; echo b` runs `echo` with three arguments. Write `command: [sh, -c, "echo a; echo b"]` for a shell. Unbalanced quotes are an error. Variables are substituted before the string is split, as in Compose, so a variable whose value has spaces becomes several words.

`command: []` and `command: ''` mean an empty command in Compose. The app spec cannot hold one, so they are refused; leave `command` out to run the image's default command. hivemind has no `entrypoint`.

#### environment

In a mapping, a string value is used as it is, and a number or a boolean becomes the string Compose makes of it: `1.50` becomes `1.5`, `0x10` becomes `16`, `true` stays `true`. Quote a value to keep it exactly as written. A key with no value (`KEY:`) is refused; in Compose it takes the value from the shell, which here is written `KEY: ${KEY}`.

In a list, each item is `KEY=VALUE`, split at the first `=`. A bare `KEY` is refused for the same reason, and so is a key listed twice. Variables are substituted in the whole item, so `- ${NAME}_URL=...` works.

`env_file` is refused. List the variables under `environment`; their values can come from `.env` through variables.

#### ports

Short syntax is `[IP:]HOST:CONTAINER[/PROTOCOL]`, with one port on each side. An IPv6 address may be written in brackets, `[::1]:53:53/udp`. The protocol is `tcp`, the default, or `udp`. A container port alone (`"80"` or `80`) is refused, because hivemind publishes every port on a host port, and so are ranges such as `"8000-8010:8000-8010"`.

Long syntax takes `target`, `published`, `host_ip`, `protocol` and `mode`. `target` and `published` are required. `mode: host` is accepted; `mode: ingress`, the Compose default, is refused when it is written, because hivemind publishes a port on the node that runs the replica, as `host` mode does. A `published` range, `name` and `app_protocol` are refused.

With either syntax, no address means `127.0.0.1`; see [above](#ports-are-published-on-127001). Two replicas of an app with a host port cannot share a node, so `deploy.replicas` above 1 with a port needs that many nodes.

#### volumes

A service mounts a named volume declared at the top level, with `name:/path` or `name:/path:rw`, or the long syntax with `type: volume`, `source`, `target`, and `read_only: false` if you like. The volume of app `web` named `data` is the Docker volume `hivemind-web_data`; Compose would name it `<project>_data`, so data in an existing Compose volume is not found.

Refused:

- bind mounts: a source that starts with `.`, `/` or `~`, or `type: bind`;
- anonymous volumes, such as `/data` alone;
- read-only mounts, `:ro` or `read_only: true`, and the access modes `z` and `Z`; the app spec has no read-only mounts;
- `tmpfs` and every type other than `volume`, and the `volume`, `bind`, `tmpfs`, `image` and `consistency` options;
- a volume that two services mount. A hivemind volume belongs to one app.

#### deploy

`deploy.replicas` must be at least 1; Compose allows 0, hivemind does not. `restart_policy` may give `condition: any` or nothing; its other settings are refused, because hivemind restarts every replica that exits after a backoff of its own, from 1 second doubling to 5 minutes. `placement`, `update_config`, `rollback_config`, `endpoint_mode`, `labels`, the `pids` limit and devices are refused.

Byte values are written as in [the specification](https://github.com/compose-spec/compose-spec/blob/main/11-extension.md#specifying-byte-values): an amount and `b`, `k`, `kb`, `m`, `mb`, `g` or `gb`, in either case, where `k` is 1024 as Docker reads it. `512m` becomes `512Mi` in the app spec. A plain number is bytes, and a decimal amount such as `1.5g` is allowed. hivemind's own suffixes, such as `512Mi`, are refused here. The smallest memory limit is 6 MiB.

#### healthcheck

`test` is required, and becomes an exec check:

- `["CMD", "curl", "-f", "http://localhost/"]` runs the command as listed;
- `["CMD-SHELL", "curl -f http://localhost/ || exit 1"]` and a plain string run `/bin/sh -c` with the string, as Docker does;
- `["NONE"]` or `disable: true` mean no health check.

`interval`, `timeout` and `start_period` are [durations](https://github.com/compose-spec/compose-spec/blob/main/11-extension.md#specifying-durations) such as `30s` or `1m30s`; a number with no unit is refused. `retries` is a whole number. A setting left out gets hivemind's default, not Docker's: an interval of 10 seconds rather than 30, a timeout of 2 seconds rather than 30, and 3 retries. hivemind's limits apply: an interval of 1 second to 1 hour, and a timeout no longer than the interval. `start_interval` is refused. A health check with no `test` is refused too, since Compose would then use the image's `HEALTHCHECK`, which hivemind does not run.

#### restart

hivemind restarts every replica that exits, whatever its exit code. `always` and `unless-stopped` say the same and are accepted; `no` and `on-failure` are refused. Leaving `restart` out is accepted, though the Compose default is `no`.

## Interpolation

Variables are substituted in values, as the [specification](https://github.com/compose-spec/compose-spec/blob/main/12-interpolation.md) describes, and never in keys:

| Form | Value |
|---|---|
| `$VAR`, `${VAR}` | the value of `VAR` |
| `${VAR:-default}` | `default` if `VAR` is unset or empty |
| `${VAR-default}` | `default` if `VAR` is unset |
| `${VAR:?message}` | an error with the message if `VAR` is unset or empty |
| `${VAR?message}` | an error with the message if `VAR` is unset |
| `$$` | a literal `$` |

A default may contain variables, as in `${A:-${B:-x}}`, up to 16 deep. A name is letters, digits and underscores, not starting with a digit. A `$` followed by anything else, such as `$5`, is left as it is. Other shell forms, such as `${VAR:+x}` and `${VAR/a/b}`, are errors.

One rule is stricter than Compose: a variable that is not set, with no default, is an error. Compose substitutes an empty string and prints a warning. A variable set to an empty string is set.

Variables come from the CLI's environment first, then from the `.env` file in the compose file's directory. The environment wins, as in Compose. `.env` follows the [env_file format](https://github.com/compose-spec/compose-spec/blob/main/spec.md#env_file-format): `VAR=VALUE` lines, blank lines and `#` comments, an optional `export ` before the name, `'single quotes'` taken literally except for `\'`, `"double quotes"` with the escapes `\n`, `\r`, `\t`, `\\` and `\"`, and a `#` after a space starting a comment in an unquoted value. A line with only a name leaves that variable unset. Unquoted and double-quoted values are interpolated too, from the environment and then the lines above.

Values under `x-` keys are not read, so they are not interpolated there; a value under an `x-` key that a service uses through an alias is interpolated where it is used. Only strings are interpolated; numbers and booleans are used as they are.

In a flow list or mapping, quote a value that holds `${...}`: in `environment: [A=${B}]` YAML reads the braces as its own syntax, and the file does not parse. Write `environment: ["A=${B}"]`.

## YAML

The file is one YAML document. Anchors, aliases and merge keys (`<<`) work, as in Compose: a key written in a mapping wins over a merged one, and an earlier mapping in a merge list wins over a later one. A duplicate key is an error. So is an alias inside the value it names.

Aliases may expand into at most 100,000 values in all, so a small file cannot grow into a large one. yaml.v3 has its own guard against alias expansion, but it applies only when yaml.v3 decodes into Go values, and `hivemind compose` walks the parsed YAML itself.

`x-` keys are ignored in every mapping whose keys Compose defines: the top level, a service, `deploy` and its parts, `healthcheck`, a long-syntax port or mount, and a top-level volume. Under `services`, the top-level `volumes` and `environment`, the keys are names, so `x-web` there is a service, a volume or a variable.

The Compose tags `!reset` and `!override`, for merging several files, are refused; hivemind reads one file. So are other tags apart from YAML's standard scalar, sequence and mapping tags.

## Refusals

Each refused key gets one of these reasons. A key not listed here gets the general one for its place: "hivemind reads services and volumes only" at the top level, "the app spec has no equivalent" in a service, and a list of the accepted keys elsewhere.

| Key | Reason |
|---|---|
| `networks` (top level) | hivemind has no networks between apps: apps reach each other only through published ports |
| `secrets` (top level and service) | hivemind has no secrets: pass values in a service's environment, from variables |
| `configs` (top level and service) | hivemind has no configs: build the files into the image |
| `include` | hivemind reads one file: merge the files first, for example with `docker compose config` |
| `models` | hivemind runs containers only |
| `volumes.<name>.external` | hivemind creates each app's volumes, named `hivemind-<app>_<volume>`, and cannot mount one made elsewhere |
| `volumes.<name>.name` | hivemind names each volume `hivemind-<app>_<volume>` |
| `volumes.<name>.driver`, `driver_opts` | hivemind volumes use Docker's local driver, with no options |
| `volumes.<name>.labels` | hivemind labels each volume with `hivemind.app` and `hivemind.volume`, and nothing else |
| `build` | build and push the image, then name it in `image` |
| `depends_on`, `links`, `external_links` | hivemind starts each app on its own, and apps reach each other only through published ports |
| `networks` (service) | hivemind has no networks between apps |
| `network_mode` | hivemind gives each container Docker's default network |
| `expose` | apps reach each other only through published ports; list them under `ports` |
| `env_file` | list the variables under `environment`; a value can come from `.env` beside the file, as `${VAR}` |
| `container_name` | hivemind names each container `hivemind-<app>-<replica>-g<generation>` |
| `hostname`, `domainname` | hivemind does not set them |
| `labels` | hivemind labels each container with `hivemind.app`, `hivemind.replica`, `hivemind.generation` and `hivemind.routed`, and nothing else |
| `entrypoint` | the app spec has no entrypoint: put the whole command in `command`, or build an image with the entrypoint you need |
| `scale` | use `deploy.replicas` |
| `extends` | hivemind reads one file and does not merge services; use YAML anchors and merge keys |
| `profiles` | every service in the file becomes an app; remove the services you do not want |
| `volumes_from` | a hivemind volume belongs to one app |
| `tmpfs` | hivemind mounts named volumes only |
| `cpus`, `mem_limit`, `mem_reservation` | use `deploy.resources.limits` |
| `user`, `working_dir` | the container uses the image's; set them in the image |
| `stop_signal` | hivemind stops a container as `docker stop` does, with the image's stop signal |
| `logging` | hivemind uses the engine's default log driver, and keeps a json-file log to 5 files of 10 MB |
| `pull_policy` | hivemind pulls an image when the node does not have it |
| `ports[].mode: ingress` | ingress balances a port across a swarm; hivemind publishes it on the node that runs the replica |
| `ports[].name`, `app_protocol` | the app spec has no port names or application protocols |
| port ranges, container-only ports | list each port, with a host port |
| bind mounts, anonymous volumes, `tmpfs` | hivemind mounts named volumes only |
| `:ro`, `read_only: true` | the app spec has no read-only mounts |
| a volume in two services | a hivemind volume belongs to one app, as `hivemind-<app>_<volume>` |
| `deploy.replicas: 0` | an app has at least 1 replica; delete the app to stop it |
| `deploy.mode: global` | global mode runs a replica on every node; set `deploy.replicas` instead |
| `deploy.mode: replicated-job`, `global-job` | jobs are not supported: hivemind restarts a replica that exits |
| `deploy.placement` | hivemind places replicas by free CPU, memory and host ports; it has no constraints or preferences |
| `deploy.update_config` | hivemind updates one replica at a time; see [rolling updates](concepts.md#rolling-updates) |
| `deploy.rollback_config` | hivemind has no automatic rollback: deploy the old spec to roll back |
| `deploy.endpoint_mode` | hivemind starts each app on its own, and apps reach each other only through published ports |
| `deploy.labels` | hivemind has no service labels |
| `deploy.resources.limits.pids` | hivemind sets no PID limit |
| `deploy.resources.reservations.devices`, `generic_resources` | hivemind gives containers no devices |
| reservations that differ from the limits | hivemind uses one figure for both the reservation and the limit |
| `restart_policy.condition: none`, `on-failure`; `restart: no`, `on-failure` | hivemind restarts every replica that exits, whatever its exit code |
| `restart_policy.delay`, `max_attempts`, `window` | hivemind restarts a replica after a backoff that doubles from 1 second to 5 minutes, with no limit on attempts |
| `healthcheck.start_interval` | hivemind checks at one interval, during the start period too |
| `command: []`, `command: ''` | leave `command` out to run the image's default command |

## Worked example

A web front end, an API and its database. The database is published on the node's private address, which the API's container can reach.

`compose.yaml`:

```yaml
name: shop

x-restart: &restart
  restart: unless-stopped

x-api-env: &api-env
  LOG_LEVEL: info
  DB_HOST: ${DB_ADDR}

services:
  web:
    <<: *restart
    image: nginx:1.27-alpine
    ports:
      - "8080:80"
    healthcheck:
      test: ["CMD", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1/"]
      interval: 15s
      timeout: 3s
      retries: 3

  api:
    <<: *restart
    image: ghcr.io/example/shop-api:${API_TAG:-2.4.1}
    command: serve --listen :9000 --db "postgres://shop@${DB_ADDR}:5432/shop"
    environment:
      <<: *api-env
      DB_PASSWORD: ${DB_PASSWORD:?set DB_PASSWORD in .env}
      WORKERS: 4
    ports:
      - target: 9000
        published: 9000
        host_ip: 0.0.0.0
    deploy:
      resources:
        limits:
          cpus: "0.5"
          memory: 256m

  db:
    <<: *restart
    image: postgres:16
    environment:
      POSTGRES_USER: shop
      POSTGRES_PASSWORD: ${DB_PASSWORD}
      POSTGRES_DB: shop
    ports:
      - "${DB_ADDR}:5432:5432"
    volumes:
      - pgdata:/var/lib/postgresql/data
    healthcheck:
      test: pg_isready -U shop
      interval: 10s
      start_period: 30s
    deploy:
      resources:
        limits:
          memory: 1g
        reservations:
          memory: 1g

volumes:
  pgdata:
```

`.env` beside it:

```sh
DB_PASSWORD=change-me
API_TAG=2.5.0
# The private address of the node that runs db.
DB_ADDR=10.0.0.5
```

db's port is bound to `DB_ADDR`, so db is placed on the node whose address that is: a worker's private address, or the server's `--advertise` address, as `hivemind nodes --json` lists them under `address`. If no node has it, db stays `pending`, and its reason in `hivemind status` names the address. Its volume is created on its node, and db stays with it. A deploy with `DB_ADDR` set to another node's address does not move it: the rollout pauses, and says why. To move db, delete the app and deploy it again; it starts on the new node with an empty volume, and the old volume stays on the old node.

`hivemind compose convert compose.yaml` prints a note on standard error for the web port, which has no address:

```
note: services.web.ports[0]: published on 127.0.0.1:8080 only, the hivemind default; Compose would publish it on every interface. To publish elsewhere, write "0.0.0.0:8080:80" or another address
```

and the three specs on standard output:

```yaml
name: api
image: ghcr.io/example/shop-api:2.5.0
replicas: 1
command:
  - serve
  - --listen
  - :9000
  - --db
  - postgres://shop@10.0.0.5:5432/shop
env:
  DB_HOST: 10.0.0.5
  DB_PASSWORD: change-me
  LOG_LEVEL: info
  WORKERS: "4"
ports:
  - bind: 0.0.0.0
    host: 9000
    container: 9000
    protocol: tcp
resources:
  cpu: 0.5
  memory: 256Mi
---
name: db
image: postgres:16
replicas: 1
env:
  POSTGRES_DB: shop
  POSTGRES_PASSWORD: change-me
  POSTGRES_USER: shop
ports:
  - bind: 10.0.0.5
    host: 5432
    container: 5432
    protocol: tcp
volumes:
  - name: pgdata
    path: /var/lib/postgresql/data
resources:
  memory: 1Gi
health:
  exec:
    - /bin/sh
    - -c
    - pg_isready -U shop
  interval: 10s
  timeout: 2s
  retries: 3
  start_period: 30s
---
name: web
image: nginx:1.27-alpine
replicas: 1
ports:
  - bind: 127.0.0.1
    host: 8080
    container: 80
    protocol: tcp
health:
  exec:
    - wget
    - -q
    - -O
    - /dev/null
    - http://127.0.0.1/
  interval: 15s
  timeout: 3s
  retries: 3
```

The database's health check gets hivemind's 2-second timeout, since the file gives none; Docker's default would be 30 seconds.

`hivemind compose deploy compose.yaml` prints the same note, then one line for each app:

```
api: created, generation 1, 1 replica
db: created, generation 1, 1 replica
web: created, generation 1, 1 replica
```

`hivemind compose delete compose.yaml` deletes the three apps and keeps the `pgdata` volume, which is the Docker volume `hivemind-db_pgdata` on its node.

This file and its output are the golden test in `internal/compose/testdata/golden`.
