# Hivemind assessment

State of the code at commit `0975ee6` (tag v1.0.2, 2025-08-07), checked on 2026-11-12.

Hivemind does not orchestrate containers today. A deploy request writes a row to SQLite and returns `success: true`; nothing ever creates a container. The containerd code path that would create one fails inside runc, the Docker runtime is a stub that returns fixed sample data, and nodes never join each other. About half of the non-test Go code (`internal/security` and `internal/resilience`, 11,911 of 23,216 lines) is not linked into the binary at all. Every HTTP route is unauthenticated and listens on all interfaces, and one of them can create or truncate files outside the data directory.

The rest of this document gives the evidence, with file:line references against that commit.

## How this was checked

- Build and tests: `golang:1.24` container (go1.24.13, linux/arm64, OrbStack on an Apple M-series Mac), `go build ./...`, `go vet ./...`, `go test -race -count=1 ./...`, then the same without `-race`.
- End to end: the cgo binary was run in a privileged `debian:bookworm-slim` container with containerd 1.6.20 and runc from Debian, on a private Docker bridge network with no published ports. Containerd's root was on a Docker volume (overlay on overlay does not mount). For three nodes, three such containers ran on the same network with `--runtime docker`.
- A small probe program called `internal/containerd.Manager` directly, to see whether the containerd path works when the API wiring is bypassed.
- Code reading for everything else. Two components (membership/scheduler/discovery/network policy, and storage/security/resilience) were read by a second reader and the findings below were spot-checked against the source.

## What works end to end

### One node

| Step | Result |
|---|---|
| `CGO_ENABLED=0` binary (what the Dockerfile and every cross-compiled release target produce) | Exits: `Binary was compiled with 'CGO_ENABLED=0', go-sqlite3 requires cgo to work` |
| cgo binary started outside the source tree | Exits: `html/template: pattern matches no files: templates/*.html` |
| Default runtime with no containerd | Waits 10 s, exits with a dial timeout |
| Listening sockets after start | `*:4483` (TCP, web and API), `*:7946` (TCP and UDP, memberlist) |
| `POST /api/deploy` with `docker.io/library/nginx:alpine` | `{"success":true,"container_id":"5308b6..."}`; containerd namespace `hivemind` has no containers and no tasks. The "container_id" is the app row's ID. |
| `POST /api/restart` with the app name | `app web not found` |
| `POST /api/scale` | `scaling not implemented` |
| `GET /api/nodes` | One node, with `CPUAvailable: 100` and `MemoryAvailable: 1073741824` regardless of the machine |
| `POST /api/volumes/create` with name `../../../tmp/evil` | Tried to create `/tmp/tmp/evil`, outside the data directory |
| Probe: `containerd.Manager.CreateContainer("nginx:alpine")` | `failed to resolve reference "nginx:alpine"`: short image names are not normalised |
| Probe: `CreateContainer("docker.io/library/nginx:alpine")` | Image pulls, container record is created, task fails: `runc create failed: cannot allocate tty if runc will detach without setting console socket`. The container record is left behind. |

### Three nodes

Each node's `/api/nodes` lists only itself. There is no flag or command to join a cluster, every node registers with memberlist under the same name (`"hivemind-node"`, `cmd/hivemind/main.go:227`), and nothing calls `Join` (see Membership below). Any node's API answered requests from the other containers with no credentials. With `--runtime docker`, `/api/containers` returns two fixed entries named `mock-container-1` and `mock-container-2`.

## Build

- `go build ./...` and `go vet ./...` pass.
- `github.com/mattn/go-sqlite3` is imported by `internal/storage/manager.go:13` and `internal/app/manager.go:14`, both on the startup path. With cgo off it builds a stub driver, so the Docker image (`Dockerfile:25` sets `CGO_ENABLED=0`) and the linux/arm64, darwin and windows release binaries (cross-compiled on an amd64 runner in `.github/workflows/release.yml`, which disables cgo) cannot start. linux/amd64 is the only release binary built with cgo, and it is dynamically linked against glibc.
- The web server parses `templates/*.html` relative to the working directory (`internal/web/server.go:98-105`) and serves `/assets` from `./assets` (`server.go:220`). Templates are skipped when `os.Args` contains `test.v` or `test.run` (`server.go:81-95`), so tests never exercise template loading.
- `go.mod` declares `go 1.22` with `toolchain go1.24.5`; the README says Go 1.18+, the badge says 1.21 to 1.23, the Dockerfile uses 1.23 and CI uses 1.23.

## Architecture

`cmd/hivemind/main.go` builds every component, stores references between some of them (`connectComponents`, `main.go:295-317`), starts four health-monitor loops (`main.go:319-331`) and starts the web server. The only CLI commands are the root command, which runs the server, and `version` (`main.go:114-141`).

| Component | Non-test lines | Constructed by main | Does real work at runtime |
|---|---:|---|---|
| `internal/app` (apps, SQLite) | 1,894 | yes | stores rows; never starts a container |
| `internal/containerd` | 1,038 | yes (default) | connects; create fails in runc; start, remove, logs and exec are placeholders |
| `internal/docker` | 169 | with `--runtime docker` | no: logs and returns sample data |
| `internal/membership` (memberlist) | 976 | yes | binds 0.0.0.0:7946; never joins anyone |
| `internal/node` | 740 | yes | reports invented CPU and memory figures |
| `internal/scheduler` | 986 | yes | never called |
| `internal/service` (discovery, DNS, proxy) | 1,473 | yes | `Initialize` never called; nothing registers services |
| `internal/storage` (volumes, SQLite) | 1,574 | yes | creates sparse files; mounting is a DB flag |
| `internal/web` (UI and API) | 1,713 | yes | serves 35 routes, none authenticated |
| `internal/security` | 8,591 | no | not linked into the binary |
| `internal/resilience` (circuit breaker, Redis) | 3,320 | no | not linked into the binary |

### Membership (SWIM)

`internal/membership/manager.go` wraps hashicorp/memberlist.

- The config is a bare struct literal (`manager.go:165-171`), not `memberlist.DefaultLANConfig()`. Probe, gossip and push-pull intervals, `TCPTimeout` and `HandoffQueueDepth` are all zero, so memberlist's failure detection loops do not run. A `Join` would probably fail on its zero TCP deadline even if something called it.
- No `SecretKey` and no label: gossip is plaintext and unauthenticated, on 0.0.0.0.
- `Start()`, which would run leader election and quorum checks, is never called, so `IsLeader` is always false. `Join` is only called from node-manager methods that nothing calls (`internal/node/manager.go:444`, `:527`).
- The delegates are stubs: `NotifyMsg` (`797-805`), `LocalState` (`837-841`) and `MergeRemoteState` (`844-848`). Event callbacks only log (`857-869`). `broadcastState` only logs (`617-621`, `655-659`), so the "distributed state store" is a local map.
- Quorum is `int(n * 0.51)` (`346`): 0 for one member, 1 for two, so both halves of a two-node split believe they have quorum.
- `Leave` returns early without closing `shutdownCh` on error and its check-then-close is not atomic (`476-488`); two concurrent calls can panic on a double close.

### Node manager

`internal/node/manager.go`.

- CPU usage is `10 + unix_seconds % 60` and memory is `20 + unix_seconds % 60` (`395-406`). Both stay under 90, so `CheckHealth` always reports healthy. Local resources are hardcoded to 100 CPU and 1 GiB (`136-139`).
- `Resources.CPU/Memory/Disk`, which the scheduler reads, are never set.
- Peer discovery (`StartDiscovery`, UDP broadcast on 0.0.0.0:8901) is never started. If it were, `msg[:18] == "HIVEMIND_DISCOVERY:"` (`288`) compares 18 bytes with a 19-byte string and never matches, and any datagram shorter than 18 bytes panics the process.
- The node ID is written to a hardcoded `/var/lib/hivemind/node_id` (`559`), ignoring `--data-dir`. If the write fails the node gets a new ID on every start.
- `GetNodes` and `GetNodeByID` return internal pointers (`491-499`, `677-687`) that race with writers.

### Scheduler

`internal/scheduler/enhanced_scheduler.go` (the older `scheduler.go` has no callers).

- Nothing outside tests calls `Schedule` or `ScheduleTask`. The app manager has no reference to the scheduler, and the web server is not given one (`main.go:269-278`).
- With one node, `ScheduleTask` returns it without checking resources (`537-542`). With two or more, available capacity is copied from `Resources.CPU/Memory/Disk` (`499-501`), which are always zero, so any task that requests resources fails with "no suitable node" (`652-655`).
- Used capacity is added (`556`) and never released.
- The `Random` strategy has no branch and returns nil (`664-681`). `BestFit` picks the least-loaded node (`607-609`), the same as `WorstFit`.
- Taints, priorities, affinity and overcommit maps are declared but never read in placement (`642-684`). `checkTenantQuotas` returns true (`715-719`); `rebalanceTasks` only logs (`735-740`).
- After the unlock at `534`, `s.nodes` and `s.tasks` are read and written without a lock. Concurrent `ScheduleTask` calls can hit Go's fatal "concurrent map iteration and map write" (`530` against `571`). The race detector reports this from the chaos tests.

### Service discovery

`internal/service/discovery.go`, `discovery_methods.go`. `Initialize()` is never called and nothing registers a service, so `/api/services` is always empty. If it were started:

- The DNS server listens on UDP :53 on all interfaces and answers every query with a fixed 12-byte header that claims one answer and contains none (`discovery.go:567`), one goroutine per packet with no limit (`535`, `561`).
- The "proxy" returns `200 {"status":"proxied"}` without forwarding (`682-690`), and binds :4483 (`183`), the same port as the web UI.
- "Round robin" is random (`621`); least-connections increments a counter that is never decremented (`540`).
- Endpoint slices are returned without copying (`490`, `505`) and read without a lock while health checks write them (`607` against `882`).

### Networking

There is no container networking. Containers created through containerd get the default OCI spec, a new network namespace with only loopback, and no CNI. Port mappings from the API are converted to `{ContainerPort: 0, HostPort: 0}` (`internal/containerd/manager.go:491`, `512`).

`internal/security/network_policy*.go` and `ipam_manager.go` are not linked into the binary. As written:

- The `HVM_POL_*` iptables chains are created but nothing jumps to them, `-C` receives the whole rule as one argument, and `"--dport N"` is passed as one argument, which iptables rejects (`network_policy_controller.go:529`, `533`, and the same pattern at the other rule sites).
- `EvaluateTraffic` contains hardcoded rules for test IPs 10.244.0.2 to .5 (`network_policy_controller.go:193-211`) and allows traffic when a tenant is unknown (`232-234`, `245-247`).
- Two deadlocks: `NetworkPolicyEnforcer.IsTrafficAllowed` holds the read lock (`network_policy.go:324`) and calls `logTraffic`, which takes the write lock (`509`); `RunReconciliation` holds the lock (`network_policy_controller_part2.go:20`) and calls `CheckPolicyViolations`, which takes it again (`79`).
- IPAM gives the gateway address (.1) to the first container (`ipam_manager.go:163-165` against `282`), computes subnets outside the parent network for masks that aren't a multiple of 8 (`137-138`), and keeps everything in memory.

### Volumes and storage

`internal/storage/manager.go`, `manager_methods.go`.

- A volume is a sparse file under `<data-dir>/storage/volumes/<name>`, created with `os.Create` and `Truncate` (`manager_methods.go:46-55`). There is no filesystem in it and it is never mounted: `MountVolume` sets a DB state (`171-204`). The app manager never calls it, and volumes in a deploy request are dropped before reaching the runtime (`internal/containerd/manager.go:467-498`).
- The volume name goes into `filepath.Join` unchecked (`manager_methods.go:46`). Through the unauthenticated `POST /api/volumes/create`, `{"name":"../storage.db"}` truncates the live database and `../../../../etc/<x>` reaches any path the process can write (it runs as root to talk to containerd). Confirmed in the end-to-end run above.
- Seven methods take `volumesMutex.Lock()` and then call `GetVolumeStatus`, which takes `RLock` (`manager.go:354`): `MountVolume`, `UnmountVolume`, `ResizeVolume`, `CloneVolume`, `SnapshotVolume`, `RestoreVolumeFromSnapshot`, `WriteToVolume`. Each self-deadlocks on any existing volume, and every later storage call, including the health monitor, then hangs.
- `/api/volumes/delete` deletes the DB row and leaves the file (`manager_methods.go:793-796`, `manager.go:261-295`).
- Encryption, ACL and QoS hooks are never set; `EncryptData` returns the input (`manager.go:499-506`).

### Runtimes

containerd (`internal/containerd/manager.go`), the default:

- `NewManager` dials `/run/containerd/containerd.sock` with a fixed 10 s timeout and the process exits if containerd is absent (`101-105`).
- `createContainer` sets `oci.WithTTY` and attaches `cio.WithStdio` (`564`, `573`), which runc rejects. The container's stdio would in any case be hivemind's own.
- `command` and `volumes` are dropped on the way to `createContainer` (`467-520`); image references are not normalised.
- `StartContainer` and `RemoveContainer` only edit an in-memory map (`138-172`). `GetContainerLogs` and `ExecInContainer` return placeholder strings (`427-445`). The container map is in memory, so containers that do exist are forgotten on restart.
- Every container is recorded as 1 CPU, 512 MiB, `NodeID: "local"` (`585-599`).

Docker (`internal/docker/manager.go`): every method logs and returns fixed data, for example `ListContainers` returns two `mock-container`s (`71-97`). `ListImages` with no containerd client returns three invented images (`internal/containerd/manager.go:210-243`). `internal/docker/manager_test.go` asserts this sample data.

### App manager

`internal/app/manager.go`, `manager_methods.go`.

- `DeployApp` calls `CreateApp`, which saves a row in state `pending` and returns (`manager_methods.go:103-205`, `628-643`). Nothing starts pending apps.
- `StartApp` is reachable only through `RestartApp`, which looks the app up by ID (`manager.go:597`) while both callers pass the app name (`internal/web/api_handlers.go:197`, `internal/web/handlers.go:314`), and `StopApp` refuses an app that is not running (`manager_methods.go:299`).
- The health loop calls `GetContainerStatus` with the app ID instead of a container ID (`manager_methods.go:553`); the container ID is never stored on the app.
- `CreateApp` contains a test fixture in production code: tenant `tenant-10` with CPU >= 2 and memory >= 6000 gets "quota exceeded" (`manager_methods.go:116-120`).
- `ScaleApp` returns "scaling not implemented" (`616-620`). `ZeroDowntimeDeploy`, `GetDeploymentStatus`, `RollbackDeployment` and the tenant CRUD methods return fixed values (`722-860`).

### Web UI and API

`internal/web`. Gin in debug mode, `0.0.0.0:<web-port>` (`server.go:197`), 13 HTML routes, 21 API routes and `/assets`. No authentication or CSRF protection on any of them, including deploy, restart, volume create and delete, and tenant create and delete. `pkg/client` targets `/api/containers` CRUD and `/api/nodes/:id`, which the server does not implement in that form.

### Storage of state

Two SQLite files: `<data-dir>/apps/app.db` (apps, plus `audit_logs` and `tenant_stats` tables that are never used) and `<data-dir>/storage/storage.db` (volumes). Containers, nodes, services, membership and scheduler state are in memory only.

### Redis circuit breaker and the rest of `internal/resilience`

Only `internal/resilience/example_test.go` and `circuit_breaker_test.go` import it. Read as library code:

- Half-open admits unlimited concurrent probes: `AllowRequest` checks `HalfOpenCalls` but never increments it (`circuit_breaker.go:391-397`).
- `RecordSuccess` unlocks before moving to Closed (`436-439`), so a concurrent reopen can be overwritten. If `HalfOpenSuccessThreshold > HalfOpenAllowedCalls` the breaker stays half-open forever.
- Metrics are passed to storage from goroutines outside the lock; with Redis, `json.Marshal` of `TimeInState` (`redis_storage.go:285`) can race with `transitionToState` writing it (`circuit_breaker.go:701`), which is a fatal error in Go.
- `RecordEvent` in Redis is a non-atomic get, append, set (`redis_storage.go:309-339`).
- Bulkhead: growing the limit or `Reset` while permits are held panics in `x/sync/semaphore` (`bulkhead.go:190`, `223`); shrinking blocks forever (`193-194`).
- `IsNetworkError` and `IsTimeoutError` always return true (`retry.go:315-326`).

### `internal/security`

Not linked into the binary; used only by tests. Notable because the docs present it as working:

- RBAC: a default `admin`/`admin123` user (`rbac.go:244`), tokens that never expire (`755` against `792-807`), `CreateUser` that overwrites an existing ID (`281`).
- Secrets: "encryption" is base64 with a key-ID prefix (`secret_management.go:561-609`), and `GetSecret` writes the decrypted value back into the store, so the second read fails (`245-253`).
- Volume encryption, image scanning and QoS enforcement are simulated (`storage_encryption.go:431-536`, `container_scanning.go:286-328`, `storage_qos.go:689-701`).
- Test-fixture branches in authorization and quota code: `storage_acl.go:617-627`, `tenant_manager.go:409-472`, `639-650`.

## Security problems in the running binary

In order of severity:

1. No authentication on any HTTP route, bound to all interfaces (`internal/web/server.go:141-188`, `197`). Anyone who can reach port 4483 can deploy, restart and delete.
2. Path traversal in volume create (`internal/storage/manager_methods.go:46-68`): create, truncate, zero-fill or delete files anywhere the process can write.
3. Unauthenticated, unencrypted memberlist on 0.0.0.0:7946, opened at start-up although nothing uses it (`internal/membership/manager.go:165-171`).
4. Gin debug mode in the release build, which prints every route at start.
5. Input to deploy (image, name, env, volumes) is not validated; container names and volume names go to the filesystem and runtime unchecked.

## Other bugs that affect the running binary

- Storage self-deadlocks on any mount, unmount, resize, clone, snapshot, restore or write (see Volumes).
- Every GET of `/nodes` or `/api/nodes` runs `CheckHealth`, which updates counters.
- `/var/lib/hivemind/node_id` is written regardless of `--data-dir`.
- The README's quick start (`./bin/hivemind --runtime docker`) starts a server whose container list is sample data.

## Tests

`go test -race -count=1 ./...`, same container as above:

| Package | Result |
|---|---|
| internal/docker | ok (asserts the stub's sample data) |
| internal/resilience | ok, 25.5 s, 2 Redis tests skipped |
| internal/web | ok (templates skipped in test mode) |
| test/chaos | FAIL: `TestCascadingFailures`, `TestRandomNodeFailures`, both "race detected during execution of test"; 16 data-race reports |
| test/integration | ok, 5 Docker subtests and 1 app-manager subtest skipped |
| test/network, test/performance, test/tenant, test/unit | ok |
| app, containerd, membership, node, scheduler, security, service, storage, pkg/* | no test files |

Totals with `-race`: 122 passed, 4 failed (2 tests and 2 subtests), 9 skipped, counting subtests.

Without `-race`, `test/chaos` passes and `test/unit` (`TestNodeManagerWithMembership`, `node_manager_test.go:238`) and `test/integration` (`TestComponentIntegration`, `integration_test.go:115`) fail on every run with `[]string{"test-node"} does not contain "test-node-1"`. Whether they pass depends on timing.

What the tests cover:

- No test creates a real container, starts the binary, makes an HTTP request to a running server, or runs more than one process.
- The chaos, performance and scheduler tests run against mocks from `test/fixtures` (`MockNodeManager`, `MockContainerManager`); "node failure" means changing what a mock returns.
- `internal/docker/manager_test.go` pins the stub's fixed output.
- Security tests exercise code that the binary does not include.
- No fuzz tests, and CI (`.github/workflows/test.yml`) runs unit tests and `gosec` only.

## Docs and release state

- README: the last line says MIT (`README.md:181`); `LICENSE` is GPL-3.0 and the badge says GPLv3.
- README claims "a fully functional web GUI" and Docker support "recommended for development".
- ROADMAP.md says "~50% COMPLETE" and marks the container runtime "100% complete" (`ROADMAP.md:3`, `23`).
- `docs/installation_guide.md` downloads `.tar.gz` assets (`41-90`) that no release has, and uses `hivemind join` (`282`), which does not exist. `docs/cli_reference.md` (1,158 lines) and `docs/api_reference.md` (2,122 lines) describe commands and endpoints that are not implemented; `docs/administration_guide.md:302` documents `hivemind security create-secret`.
- GitHub release v1.0.1 has raw binaries and `.sha256` files for six targets; five of them cannot start (sqlite stub). Tag v1.0.2 points at `0975ee6` and has no release.
- `release.yml` publishes a GHCR image built with `CGO_ENABLED=0`, which cannot start either.
