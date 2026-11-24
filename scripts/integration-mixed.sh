#!/usr/bin/env bash
# Runs a cluster of mixed versions in Docker: the 2.0.0 release binary and a
# build of this checkout, and checks what each side shows of the other.
#
# Three docker:dind containers on one private network, as in
# scripts/integration-cluster.sh (helpers in scripts/lib/cluster.sh):
#   n0  the server: 2.0.0, then upgraded in place to this build
#   n1  a worker on this build, joined to the 2.0.0 server (workers first)
#   n2  a worker that stays on 2.0.0
# The 2.0.0 binary is the release's hivemind-linux-<arch>, checked against
# the release's SHA256SUMS, and kept in .scratch/ for the next run.
# HIVEMIND_OLD_BIN names another copy instead.
#
# With the 2.0.0 server:
#   - this build's CLI shows "-", not "ineligible", for the eligibility 2.0.0
#     does not send; node drain says the server is older than 2.2 and has no
#     node drain; hivemind version says the server reports no version
#   - n1 logs once each that the server has no log streaming and no lease
#     renewal between status reports, and never that it is not registered,
#     over more than two of the old 30 s retries; it logs that the server
#     refused its address
# After the server is upgraded, on the same data directory:
#   - the store migration logs one line per schema step, from 2.0.0's 6 to
#     this build's version, whatever it is
#   - a second serve on the data directory is refused with the pid
#   - n1 registers its address without a restart, and nodes shows it
#   - nodes shows this build's version for n0 and n1, and "-" for n2;
#     hivemind version prints the server's
#   - with a host that has a whole host route in one app and a path route
#     in another, the node API's table for a router of a build before path
#     routes (no ?paths=1, an older version) has each host once, no path
#     field, and that host with no endpoints; with ?paths=1 it has the path.
#     The server logs a warning naming the node, and hivemind routes and
#     status warn, until the router reads paths. curl in a curlimages/curl
#     container, with n1's node certificate, plays that router
#   - with h2c routes, a whole host and a path, the table for a router of a
#     build with paths and before route protocols (?paths=1 only) has no
#     protocol field and both routes with no endpoints; for a router before
#     paths, the h2c host has none either; with ?paths=1&protocols=1 both
#     have "protocol":"h2c". The server logs a warning naming the node and
#     its version, and hivemind routes and status warn, until the router
#     reads protocols. curl plays the router again
#   - hivemind logs of the replica on n2 says the worker is older than 2.1
#     and cannot stream logs; the log of the replica on n1 is read
# A rotation of the cluster CA with n2 still on 2.0.0, which has no
# rotation (docs/ca-rotation.md#workers-of-an-earlier-build):
#   - after ca rotate start, n1 trusts the new CA and logs its fingerprint;
#     the server marks n2 a build without rotation and logs that once;
#     ca status shows n2 as an old build, and it and hivemind status name
#     it as what renew waits for; renew refuses, naming it
#   - after renew --force, n1 renews under the new CA, and the server's
#     certificate stays on the old one. n2 keeps working on the old CA: its
#     ca.crt is the old CA alone, it stays ready, and it runs a new replica,
#     placed with the other two nodes ineligible. hivemind status names it
#     as what finish waits for, and finish refuses, naming it
#   - n2 is upgraded in place to this build, on its data directory: it
#     trusts the new CA, renews under it, and the server's certificate
#     moves. finish passes, ca.crt is the new CA alone on every node, and n2
#     kept its node ID, node.json and containers, without joining again
# 2.0.0 has no router, so no requests go through routers here.
#
# It publishes no ports. On exit it removes the containers, the network and
# the volumes it created, also after a failure or Ctrl-C. Needs Docker, curl,
# jq, and the docker:dind, golang and curlimages/curl images.
set -euo pipefail

REPO=$(cd "$(dirname "$0")/.." && pwd)
# shellcheck source=lib/cluster.sh
. "$REPO/scripts/lib/cluster.sh"

command -v jq >/dev/null || {
	echo "mixed: needs jq" >&2
	exit 1
}

OLD_VERSION=v2.0.0
NEW_VERSION=v2.2.0-mixed-test

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1"; else shasum -a 256 "$1"; fi | awk '{ print $1 }'
}

# fetch_old sets OLD_BIN to the checked 2.0.0 binary for linux on this
# machine's architecture.
fetch_old() {
	if [ -n "${HIVEMIND_OLD_BIN:-}" ]; then
		OLD_BIN=$HIVEMIND_OLD_BIN
		return
	fi
	local arch base want got sums
	case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) cl_fail "no $OLD_VERSION release binary for $(uname -m)" ;;
	esac
	base=https://github.com/ao/hivemind/releases/download/$OLD_VERSION
	OLD_BIN=$REPO/.scratch/hivemind-$OLD_VERSION-linux-$arch
	mkdir -p "$REPO/.scratch"
	if [ ! -s "$OLD_BIN" ]; then
		cl_note "downloading $base/hivemind-linux-$arch"
		curl -fsSL -o "$OLD_BIN.tmp" "$base/hivemind-linux-$arch" || cl_fail "downloading $base/hivemind-linux-$arch"
		mv "$OLD_BIN.tmp" "$OLD_BIN"
	fi
	sums=$(curl -fsSL "$base/SHA256SUMS") || cl_fail "downloading $base/SHA256SUMS"
	want=$(awk -v f="hivemind-linux-$arch" '$2 == f || $2 == "*" f { print $1 }' <<<"$sums")
	got=$(sha256 "$OLD_BIN")
	if [ -z "$want" ] || [ "$want" != "$got" ]; then
		rm -f "$OLD_BIN"
		cl_fail "the $OLD_VERSION binary has SHA-256 $got; SHA256SUMS has ${want:-no line} for hivemind-linux-$arch"
	fi
	chmod +x "$OLD_BIN"
	cl_note "$OLD_VERSION binary checked against SHA256SUMS: $got"
}

# count NODE PATTERN: lines of the node's hivemind log that hold PATTERN.
count() {
	docker exec "$1" grep -cF -- "$2" /var/log/hivemind.log || true
}

# replica_on APP NODE-ID: the index of the app's replica placed on the node.
replica_on() {
	docker exec "$S" hivemind status "$1" --json |
		awk -v n="\"$2\"" '/"index":/ { gsub(/[^0-9]/, "", $2); i = $2 } /"node_id":/ && index($0, n) { print i; exit }'
}

cl_init mixed
CL_GO_LDFLAGS="-X main.version=$NEW_VERSION"
cl_add_node
cl_add_node
cl_add_node
cl_trap_cleanup
fetch_old
cl_start_nodes
for c in "${CL_NAMES[@]}"; do
	docker cp "$OLD_BIN" "$c:/usr/local/bin/hivemind-2.0.0"
done
S=${CL_NAMES[0]} W1=${CL_NAMES[1]} W2=${CL_NAMES[2]}
S_IP=$(cl_ip_of "$S") W1_IP=$(cl_ip_of "$W1")
docker exec "$S" hivemind-2.0.0 version | has "^hivemind $OLD_VERSION " || cl_fail "the old binary is not $OLD_VERSION"
docker exec "$S" hivemind version | has "^hivemind $NEW_VERSION " || cl_fail "the new binary is not $NEW_VERSION"

# The 2.0.0 server, and a worker of each version.
S_ID=$(docker exec "$S" hivemind-2.0.0 init | sed -n 's/.*(node \(node-[0-9a-f]*\)).*/\1/p')
[ -n "$S_ID" ] || cl_fail "2.0.0 init printed no node ID"
docker exec -d "$S" sh -c "exec hivemind-2.0.0 serve --advertise $S_IP --min-ready 0s >>/var/log/hivemind.log 2>&1"
wait_for 30 docker exec "$S" hivemind status || cl_fail "the 2.0.0 API did not answer"
join() {
	local c=$1 bin=$2 tok
	tok=$(docker exec "$S" hivemind-2.0.0 join-token --ttl 1h 2>/dev/null) || cl_fail "join-token failed"
	docker exec -d -e "HM_TOKEN=$tok" "$c" sh -c "exec $bin join $S_IP --token \"\$HM_TOKEN\" >>/var/log/hivemind.log 2>&1"
	wait_for 60 docker exec "$c" test -s /var/lib/hivemind/node.json || cl_fail "$c did not join"
}
join "$W1" hivemind
w1_started=$(date +%s)
join "$W2" hivemind-2.0.0
W1_ID=$(cl_node_id_of "$W1") W2_ID=$(cl_node_id_of "$W2")
cl_note "server $S_ID ($OLD_VERSION), $W1_ID ($NEW_VERSION), $W2_ID ($OLD_VERSION)"
three_ready() { [ "$(docker exec "$S" hivemind nodes | grep -c ' ready ')" -eq 3 ]; }
wait_for 60 three_ready || cl_fail "three nodes are not ready: $(docker exec "$S" hivemind nodes | tr '\n' ';')"

docker exec "$S" hivemind deploy --name spread --image busybox:stable --replicas 3 -- \
	sh -c 'while true; do echo tick; sleep 1; done' >/dev/null || cl_fail "deploy to the 2.0.0 server failed"
three_running() { [ "$(docker exec "$S" hivemind status spread --json | grep -c '"state": "running"')" -eq 3 ]; }
wait_for 180 three_running || cl_fail "spread is not running 3 replicas: $(docker exec "$S" hivemind status spread | tr '\n' ';')"
IDX_W1=$(replica_on spread "$W1_ID") IDX_W2=$(replica_on spread "$W2_ID")
[ -n "$IDX_W1" ] && [ -n "$IDX_W2" ] || cl_fail "spread has no replica on each worker: $(docker exec "$S" hivemind status spread | tr '\n' ';')"

# This build's CLI against the 2.0.0 server.
nodes_json=$(docker exec "$S" hivemind nodes --json)
[ "$(grep -c '"eligible": null' <<<"$nodes_json")" -eq 3 ] || cl_fail "nodes --json from 2.0.0 does not leave eligible unknown: $nodes_json"
if docker exec "$S" hivemind nodes | has ineligible; then
	cl_fail "nodes shows a 2.0.0 server's nodes as ineligible: $(docker exec "$S" hivemind nodes | tr '\n' ';')"
fi
if out=$(docker exec "$S" hivemind node drain "$W2_ID" 2>&1); then
	cl_fail "node drain against 2.0.0 succeeded: $out"
fi
grep -qF "the server is older than 2.2 and has no node drain; upgrade the server" <<<"$out" || cl_fail "node drain against 2.0.0: $out"
docker exec "$S" hivemind version | has -F "server http://127.0.0.1:4483: older than 2.2, which reports no version" ||
	cl_fail "hivemind version against 2.0.0: $(docker exec "$S" hivemind version | tr '\n' ';')"
cl_note "the CLI against 2.0.0: eligibility unknown, node drain says the server is older"

# The new worker with the 2.0.0 server, over more than two of the old 30 s
# retries.
wait_for 60 sh -c "docker exec $W1 grep -qF 'has no log streaming; upgrade the server' /var/log/hivemind.log" ||
	cl_fail "$W1 did not log that the server has no log streaming"
while [ $(($(date +%s) - w1_started)) -lt 75 ]; do sleep 1; done
for p in "has no log streaming; upgrade the server" "has no lease renewal between status reports; upgrade the server" \
	"the server refused this node's address"; do
	[ "$(count "$W1" "$p")" -eq 1 ] || cl_fail "$W1 logged \"$p\" $(count "$W1" "$p") times, want once"
done
[ "$(count "$W1" "not registered")" -eq 0 ] || cl_fail "$W1 logged that it is not registered: $(docker exec "$W1" grep -F 'not registered' /var/log/hivemind.log | head -n 3)"
cl_note "$W1 with 2.0.0: one line per missing feature in $(($(date +%s) - w1_started)) s, none about registration"

# Upgrade the server in place.
docker exec "$S" pkill -f 'hivemind-2.0.0 serve' || cl_fail "no 2.0.0 serve to stop"
old_gone() { ! docker exec "$S" pgrep -f 'hivemind-2.0.0 serve' >/dev/null; }
wait_for 20 old_gone || cl_fail "the 2.0.0 server did not stop"
up=$(date +%s)
docker exec -d "$S" sh -c "exec hivemind serve --advertise $S_IP --min-ready 0s >>/var/log/hivemind.log 2>&1"
wait_for 30 docker exec "$S" hivemind status || cl_fail "the upgraded API did not answer"
steps=$(count "$S" 'msg="store schema migrated"')
last_to=$(docker exec "$S" grep -F 'msg="store schema migrated"' /var/log/hivemind.log | sed -n 's/.* to=\([0-9]*\).*/\1/p' | tail -n 1)
# One step per version from 2.0.0's 6 to the last one logged.
[ -n "$last_to" ] && [ "$steps" -ge 1 ] && [ "$steps" -eq $((last_to - 6)) ] ||
	cl_fail "the migration logged $steps steps up to version ${last_to:-none}, want one per version from 6: $(docker exec "$S" grep -F 'schema' /var/log/hivemind.log | tr '\n' ';')"
if out=$(docker exec "$S" hivemind serve --advertise "$S_IP" 2>&1); then
	cl_fail "a second serve on the data directory started"
fi
grep -q "another hivemind (pid [0-9]*) runs on the data directory /var/lib/hivemind" <<<"$out" || cl_fail "a second serve: $out"
cl_note "the migration logged $steps steps, 6 to $last_to; a second serve is refused: $out"

has_address() { docker exec "$S" hivemind nodes --json | has -F "\"address\": \"$W1_IP\""; }
wait_for 60 has_address || cl_fail "$W1 did not register its address after the upgrade: $(docker exec "$S" hivemind nodes | tr '\n' ';')"
[ "$(count "$W1" "the server took this node's address")" -eq 1 ] || cl_fail "$W1 did not log that the server took its address"
two_versions() { [ "$(docker exec "$S" hivemind nodes --json | grep -cF "\"version\": \"$NEW_VERSION\"")" -eq 2 ]; }
wait_for 30 two_versions || cl_fail "nodes does not show $NEW_VERSION for the server and $W1: $(docker exec "$S" hivemind nodes --json | tr '\n' ' ')"
row() { docker exec "$S" hivemind nodes | awk -v n="$1" '$1 == n { print $4, $5 }'; }
[ "$(row "$W1_ID")" = "$W1_IP $NEW_VERSION" ] || cl_fail "the nodes row of $W1_ID: $(row "$W1_ID")"
[ "$(row "$W2_ID")" = "- -" ] || cl_fail "the nodes row of $W2_ID, on 2.0.0: $(row "$W2_ID")"
docker exec "$S" hivemind version | has -F "server http://127.0.0.1:4483: hivemind $NEW_VERSION" ||
	cl_fail "hivemind version against the upgraded server: $(docker exec "$S" hivemind version | tr '\n' ';')"
cl_note "after the upgrade: $W1 registered its address in $(($(date +%s) - up)) s without a restart; versions shown"

# Path routes and a router of a build before them. Such a router asks the
# node API for the table without ?paths=1 and decodes it strictly, so it
# must get no path field and each host once, and a host with a path route
# with no endpoints. The request is made as that router makes it, with W1's
# node certificate and an older version; this build's router asks with
# ?paths=1 and gets the path.
docker exec -i "$S" hivemind deploy -f - >/dev/null <<EOF || cl_fail "deploy web with routes"
name: web
image: busybox:stable
command: [sleep, "3600"]
routes:
  - {host: www.mixed.test, port: 80}
  - {host: only.mixed.test, port: 80}
EOF
docker exec -i "$S" hivemind deploy -f - >/dev/null <<EOF || cl_fail "deploy api with a path route"
name: api
image: busybox:stable
command: [sleep, "3600"]
routes:
  - {host: www.mixed.test, path: /api, port: 8080}
EOF
mkdir -p "$CL_WORK/w1-cert"
for f in node.crt node.key ca.crt; do
	docker cp "$W1:/var/lib/hivemind/$f" "$CL_WORK/w1-cert/$f"
done
node_routes() { # node_routes QUERY [VERSION]: GET routes on the node API as W1, a router of VERSION
	docker run --rm --network "$CL_NET" --user "$(id -u)" -v "$CL_WORK/w1-cert:/certs:ro" "${CURL_IMAGE:-curlimages/curl:8.11.1}" \
		-sf --cacert /certs/ca.crt --cert /certs/node.crt --key /certs/node.key -H "Hivemind-Version: ${2:-v2.2.0-before-paths}" \
		"https://$S_IP:4484/node/v1/nodes/$W1_ID/routes$1"
}
old_table=$(node_routes "") || cl_fail "the node API did not answer W1's GET routes"
if grep -qF '"path"' <<<"$old_table"; then
	cl_fail "a router before paths got a path field: $old_table"
fi
dups=$(grep -o '"host":"[^"]*"' <<<"$old_table" | sort | uniq -d)
[ -z "$dups" ] || cl_fail "a router before paths got a host twice: $dups in $old_table"
grep -qF '{"host":"www.mixed.test","endpoints":[]}' <<<"$old_table" ||
	cl_fail "a router before paths did not get www.mixed.test without endpoints: $old_table"
grep -qF '"host":"only.mixed.test"' <<<"$old_table" || cl_fail "a router before paths did not get only.mixed.test: $old_table"
new_table=$(node_routes "?paths=1&protocols=1" "$NEW_VERSION") || cl_fail "the node API did not answer W1's GET routes?paths=1&protocols=1"
grep -qF '"host":"www.mixed.test","path":"/api"' <<<"$new_table" || cl_fail "a router of this build did not get the path route: $new_table"
# The read without paths again, and the warnings it causes: the server's
# log, hivemind routes and the status of each app on the host.
node_routes "" >/dev/null || cl_fail "GET routes as W1"
# Other routers before paths may warn first: wait for the line that names W1.
# The last grep reads all its input: grep -q would end at the first match,
# and with pipefail the docker exec it cut off would fail the pipeline.
warned() { docker exec "$S" grep -F "is of a build before path routes" /var/log/hivemind.log | grep -F "node=$W1_ID" >/dev/null; }
wait_for 10 warned ||
	cl_fail "the server did not log a warning about the router before paths that names $W1_ID: $(docker exec "$S" grep -F 'before path routes' /var/log/hivemind.log)"
routes_err=$(docker exec "$S" hivemind routes 2>&1 >/dev/null)
grep -qF "warning: the router on node $W1_ID (v2.2.0-before-paths) is of a build before path routes, and answers 503 for www.mixed.test" <<<"$routes_err" ||
	cl_fail "hivemind routes does not warn about $W1_ID's router: $routes_err"
status_out=$(docker exec "$S" hivemind status)
for app in web api; do
	grep -qF "$app: warning: the router on node $W1_ID (v2.2.0-before-paths)" <<<"$status_out" ||
		cl_fail "hivemind status does not warn for $app: $(tr '\n' ';' <<<"$status_out")"
done
# Once the router reads paths, and protocols, as this build's does, the
# warnings go.
node_routes "?paths=1&protocols=1" "$NEW_VERSION" >/dev/null || cl_fail "GET routes?paths=1&protocols=1 as W1"
routes_err=$(docker exec "$S" hivemind routes 2>&1 >/dev/null)
[ -z "$routes_err" ] || cl_fail "hivemind routes still warns after the router read paths: $routes_err"
cl_note "a router before paths gets each host once and no path field, with no endpoints for www.mixed.test; the server, routes and status warn until it reads paths"

# Route protocols and a router of a build before them, which has paths: it
# asks for ?paths=1 only, and decodes strictly, so it must get no protocol
# field, and no endpoints for an h2c route, which it would send HTTP/1.1. A
# router before paths gets none for a host whose route is h2c. This build's
# router asks for protocols and gets them.
docker exec -i "$S" hivemind deploy -f - >/dev/null <<EOF2 || cl_fail "deploy grpc with h2c routes"
name: grpc
image: busybox:stable
command: [sleep, "3600"]
routes:
  - {host: grpc.mixed.test, port: 50051, protocol: h2c}
  - {host: www.mixed.test, path: /echo.Echo, port: 50051, protocol: h2c}
EOF2
before=v2.2.0-before-protocols
paths_table=$(node_routes "?paths=1" "$before") || cl_fail "the node API did not answer W1's GET routes?paths=1"
if grep -qF '"protocol"' <<<"$paths_table"; then
	cl_fail "a router before protocols got a protocol field: $paths_table"
fi
for want in '{"host":"grpc.mixed.test","endpoints":[]}' '{"host":"www.mixed.test","path":"/echo.Echo","endpoints":[]}' '"host":"www.mixed.test","path":"/api"'; do
	grep -qF "$want" <<<"$paths_table" || cl_fail "a router before protocols did not get $want: $paths_table"
done
old_table=$(node_routes "") || cl_fail "the node API did not answer W1's GET routes"
grep -qF '{"host":"grpc.mixed.test","endpoints":[]}' <<<"$old_table" && ! grep -qF '"protocol"' <<<"$old_table" ||
	cl_fail "a router before paths did not get grpc.mixed.test without endpoints and protocol: $old_table"
new_table=$(node_routes "?paths=1&protocols=1" "$NEW_VERSION") || cl_fail "the node API did not answer W1's GET routes?paths=1&protocols=1"
for want in '"host":"grpc.mixed.test","protocol":"h2c"' '"host":"www.mixed.test","path":"/echo.Echo","protocol":"h2c"'; do
	grep -qF "$want" <<<"$new_table" || cl_fail "a router of this build did not get $want: $new_table"
done
node_routes "?paths=1" "$before" >/dev/null || cl_fail "GET routes?paths=1 as W1"
warned() { docker exec "$S" grep -F "is of a build before route protocols" /var/log/hivemind.log | grep -F "node=$W1_ID" | grep -F "version=$before" >/dev/null; }
wait_for 10 warned ||
	cl_fail "the server did not log a warning about the router before protocols that names $W1_ID and $before: $(docker exec "$S" grep -F 'before route protocols' /var/log/hivemind.log)"
routes_err=$(docker exec "$S" hivemind routes 2>&1 >/dev/null)
grep -qF "warning: the router on node $W1_ID ($before) is of a build before route protocols, and answers 503 for the h2c routes grpc.mixed.test, www.mixed.test/echo.Echo; upgrade the node" <<<"$routes_err" ||
	cl_fail "hivemind routes does not warn about $W1_ID's router: $routes_err"
docker exec "$S" hivemind routes | has -E '^grpc\.mixed\.test +/ +grpc +50051 +h2c ' ||
	cl_fail "hivemind routes does not show grpc.mixed.test as h2c: $(docker exec "$S" hivemind routes | tr '\n' ';')"
status_out=$(docker exec "$S" hivemind status)
grep -qF "grpc: warning: the router on node $W1_ID ($before) is of a build before route protocols" <<<"$status_out" ||
	cl_fail "hivemind status does not warn for grpc: $(tr '\n' ';' <<<"$status_out")"
if grep -qF "web: warning: the router on node $W1_ID ($before)" <<<"$status_out"; then
	cl_fail "hivemind status warns for web, which has no h2c route: $(tr '\n' ';' <<<"$status_out")"
fi
node_routes "?paths=1&protocols=1" "$NEW_VERSION" >/dev/null || cl_fail "GET routes?paths=1&protocols=1 as W1"
routes_err=$(docker exec "$S" hivemind routes 2>&1 >/dev/null)
[ -z "$routes_err" ] || cl_fail "hivemind routes still warns after the router read protocols: $routes_err"
docker exec "$S" hivemind delete grpc >/dev/null || cl_fail "delete grpc"
cl_note "a router before protocols gets no protocol field and no endpoints for the h2c routes, one before paths none for grpc.mixed.test; the server, routes and status warn until it reads protocols"

# Logs, once the server has run a minute: the 2.0.0 worker is named as
# such, and the new worker streams.
while [ $(($(date +%s) - up)) -lt 66 ]; do sleep 1; done
if out=$(docker exec "$S" hivemind logs spread "$IDX_W2" 2>&1); then
	cl_fail "logs of the replica on 2.0.0 worker $W2_ID worked: $out"
fi
grep -qF "worker $W2_ID runs a hivemind older than 2.1, which cannot stream logs; upgrade it" <<<"$out" ||
	cl_fail "logs of the replica on $W2_ID: $out"
w1_logs() { docker exec "$S" hivemind logs spread "$IDX_W1" --tail 3 2>/dev/null | has tick; }
# The relay and the replica's next line can take a while on a slow machine.
wait_for 30 w1_logs ||
	cl_fail "logs of the replica on $W1_ID did not read: $(docker exec "$S" hivemind logs spread "$IDX_W1" --tail 3 2>&1)"
for p in "has no log streaming; upgrade the server" "has no lease renewal between status reports; upgrade the server"; do
	[ "$(count "$W1" "$p")" -eq 1 ] || cl_fail "$W1 logged \"$p\" again after the upgrade"
done
cl_note "logs: $out"

# A rotation of the cluster CA with n2 still on 2.0.0, a build without
# rotation.
DATA=/var/lib/hivemind
ca_json() { docker exec "$S" hivemind ca status --json; }
ca_q() { ca_json | jq -r "$1"; }
node_q() { ca_json | jq -r --arg id "$1" ".nodes[] | select(.id == \$id) | .$2"; } # node_q NODE-ID FIELD
fsum() { docker exec "$1" sha256sum "$2" | awk '{ print $1 }'; }
pem_count() { docker exec "$1" grep -c 'BEGIN CERTIFICATE' "$2"; }
running_ids() { docker exec "$1" docker ps -q --filter label=hivemind.app | sort | tr '\n' ' '; }
rotate() {
	local out
	out=$(docker exec "$S" hivemind ca rotate "$@" 2>&1) || cl_fail "hivemind ca rotate $*: $out"
}
is_true() { [ "$(node_q "$1" "$2")" = true ]; } # is_true NODE-ID FIELD
state_of() { docker exec "$S" hivemind nodes | awk -v n="$1" '$1 == n' | grep -oE ' (ready|lost|waiting) ' | tr -d ' '; }
# names_old OUTPUT: OUTPUT has a line that names n2 as a build without
# rotation.
names_old() { grep -F "$W2_ID" <<<"$1" | has -F "is a build without CA rotation (older than 2.2): upgrade it, or remove it"; }
CA0=$(fsum "$S" "$DATA/ca.crt")
W2_STATE=$(fsum "$W2" "$DATA/node.json")
rotate start
NEXT_FP=$(ca_q '.next.fingerprint')
wait_for 60 is_true "$W1_ID" trusts || cl_fail "$W1 does not trust the new CA: $(ca_json)"
wait_for 60 is_true "$W2_ID" old_build || cl_fail "the server did not mark $W2_ID as a build without rotation: $(ca_json)"
docker exec "$W1" grep -q "trusts a new cluster CA.*ca=$NEXT_FP" /var/log/hivemind.log || cl_fail "$W1 did not log the new CA's fingerprint $NEXT_FP"
[ "$(ca_q '[.rotation.waiting[].node] | join(" ")')" = "$W2_ID" ] || cl_fail "renew does not wait for $W2_ID alone: $(ca_json)"
ca_text=$(docker exec "$S" hivemind ca status)
awk -v n="$W2_ID" '$1 == n' <<<"$ca_text" | has -F "no (old build)" || cl_fail "ca status does not show $W2_ID as an old build: $(tr '\n' ';' <<<"$ca_text")"
names_old "$ca_text" || cl_fail "ca status does not say renew waits for $W2_ID, a build without rotation: $(tr '\n' ';' <<<"$ca_text")"
status_out=$(docker exec "$S" hivemind status)
grep -qF "Cluster CA rotation, phase trust: hivemind ca rotate renew waits for:" <<<"$status_out" && names_old "$status_out" ||
	cl_fail "hivemind status does not name $W2_ID as a build without rotation: $(tr '\n' ';' <<<"$status_out")"
if out=$(docker exec "$S" hivemind ca rotate renew 2>&1); then
	cl_fail "renew went ahead with $W2_ID on $OLD_VERSION: $out"
fi
grep -qF "$W2_ID" <<<"$out" && grep -qF "is a build without CA rotation" <<<"$out" && grep -qF -- "--force" <<<"$out" ||
	cl_fail "renew's refusal does not name $W2_ID as a build without rotation: $out"
cl_note "start: $W1 trusts the new CA ${NEXT_FP:0:16}; ca status and status name $W2_ID as a build without rotation; renew refuses: $out"

rotate renew --force
wait_for 90 is_true "$W1_ID" renewed || cl_fail "$W1 did not renew under the new CA: $(ca_json)"
docker exec "$W1" grep -q "renewing the node certificate under the CA the cluster's CA rotation goes to" /var/log/hivemind.log || cl_fail "$W1 did not log its renewal"
[ "$(ca_q '.rotation.listener_moved')" = false ] || cl_fail "the server's certificate moved while $W2_ID trusts the old CA only"
# n2 works on the old CA: its ca.crt is the old CA alone, its certificate is
# from it, its lease is current, and it takes a new replica, with the two
# other nodes ineligible.
[ "$(pem_count "$W2" "$DATA/ca.crt")" = 1 ] && [ "$(fsum "$W2" "$DATA/ca.crt")" = "$CA0" ] || cl_fail "$W2's ca.crt is not the old CA alone"
if is_true "$W2_ID" renewed; then cl_fail "$W2_ID, on $OLD_VERSION, is marked renewed under the new CA"; fi
docker exec "$S" hivemind node eligibility "$S_ID" --disable >/dev/null || cl_fail "making $S_ID ineligible"
docker exec "$S" hivemind node eligibility "$W1_ID" --disable >/dev/null || cl_fail "making $W1_ID ineligible"
docker exec "$S" hivemind deploy --name late --image busybox:stable --replicas 1 -- sleep 3600 >/dev/null || cl_fail "deploy late"
late_on_w2() {
	[ "$(replica_on late "$W2_ID")" = 0 ] && docker exec "$S" hivemind status late --json | has '"state": "running"' &&
		[ -n "$(docker exec "$W2" docker ps -q --filter label=hivemind.app=late)" ]
}
wait_for 120 late_on_w2 || cl_fail "late did not run on $W2_ID: $(docker exec "$S" hivemind status late | tr '\n' ';')"
docker exec "$S" hivemind node eligibility "$S_ID" --enable >/dev/null || cl_fail "making $S_ID eligible"
docker exec "$S" hivemind node eligibility "$W1_ID" --enable >/dev/null || cl_fail "making $W1_ID eligible"
[ "$(state_of "$W2_ID")" = ready ] || cl_fail "$W2_ID is not ready in the renew phase: $(docker exec "$S" hivemind nodes | tr '\n' ';')"
status_out=$(docker exec "$S" hivemind status)
grep -qF "Cluster CA rotation, phase renew: hivemind ca rotate finish waits for:" <<<"$status_out" && names_old "$status_out" ||
	cl_fail "hivemind status in the renew phase does not name $W2_ID: $(tr '\n' ';' <<<"$status_out")"
if out=$(docker exec "$S" hivemind ca rotate finish 2>&1); then
	cl_fail "finish went ahead with $W2_ID on $OLD_VERSION: $out"
fi
grep -qF "$W2_ID" <<<"$out" && grep -qF "is a build without CA rotation" <<<"$out" ||
	cl_fail "finish's refusal does not name $W2_ID as a build without rotation: $out"
[ "$(count "$S" "a node of a build without CA rotation is in the cluster")" -eq 1 ] &&
	docker exec "$S" grep -F "a node of a build without CA rotation is in the cluster" /var/log/hivemind.log | has -F "node=$W2_ID" ||
	cl_fail "the server did not log once that $W2_ID is a build without rotation: $(docker exec "$S" grep -F 'without CA rotation' /var/log/hivemind.log | tr '\n' ';')"
cl_note "renew --force: $W1 renewed, the server's certificate stays on the old CA, $W2_ID runs a new replica on it; finish refuses: $out"

# n2 is upgraded in place, on its data directory, and follows the rotation
# from where it is, without joining again.
W2_RUNNING=$(running_ids "$W2")
docker exec "$W2" pkill -f 'hivemind-2.0.0 join' || cl_fail "no $OLD_VERSION worker to stop on $W2"
w2_old_gone() { ! docker exec "$W2" pgrep -f 'hivemind-2.0.0' >/dev/null; }
wait_for 30 w2_old_gone || cl_fail "the $OLD_VERSION worker on $W2 did not stop"
docker exec -d "$W2" sh -c "exec hivemind serve --role worker >>/var/log/hivemind.log 2>&1"
up2=$(date +%s)
wait_for 120 is_true "$W2_ID" renewed || cl_fail "$W2 did not renew under the new CA after its upgrade: $(ca_json)"
for p in "trusts a new cluster CA.*ca=$NEXT_FP" "renewing the node certificate under the CA the cluster's CA rotation goes to"; do
	docker exec "$W2" grep -q "$p" /var/log/hivemind.log || cl_fail "$W2 did not log \"$p\" after its upgrade: $(docker exec "$W2" tail -n 5 /var/log/hivemind.log)"
done
if is_true "$W2_ID" old_build; then cl_fail "$W2_ID is still marked a build without rotation after its upgrade"; fi
listener_moved() { [ "$(ca_q '.rotation.listener_moved')" = true ]; }
wait_for 60 listener_moved || cl_fail "the server's certificate did not move to the new CA: $(ca_json)"
none_waiting() { [ "$(ca_q '.rotation.waiting | length')" = 0 ]; }
wait_for 30 none_waiting || cl_fail "finish still waits for nodes: $(ca_json)"
cl_note "$W2 upgraded: it renewed under the new CA $(($(date +%s) - up2)) s after its start, and the server's certificate moved"
rotate finish
[ "$(ca_q '.rotation')" = null ] && [ "$(ca_q '.current.fingerprint')" = "$NEXT_FP" ] || cl_fail "after finish: $(ca_json)"
CA1=$(fsum "$S" "$DATA/ca.crt")
[ "$CA1" != "$CA0" ] || cl_fail "ca.crt did not change"
one_ca() {
	local c
	for c in "$S" "$W1" "$W2"; do
		[ "$(pem_count "$c" "$DATA/ca.crt")" = 1 ] && [ "$(fsum "$c" "$DATA/ca.crt")" = "$CA1" ] || return 1
	done
}
wait_for 60 one_ca || cl_fail "ca.crt is not the new CA alone on every node: $(for c in "$S" "$W1" "$W2"; do printf '%s %s CAs %s; ' "$c" "$(pem_count "$c" "$DATA/ca.crt")" "$(fsum "$c" "$DATA/ca.crt")"; done)"
[ "$(cl_node_id_of "$W2")" = "$W2_ID" ] && [ "$(fsum "$W2" "$DATA/node.json")" = "$W2_STATE" ] &&
	[ "$(docker exec "$W2" grep -c 'joined the cluster' /var/log/hivemind.log)" = 1 ] || cl_fail "$W2 joined again"
[ "$(running_ids "$W2")" = "$W2_RUNNING" ] || cl_fail "the containers on $W2 changed across its upgrade: $W2_RUNNING before, $(running_ids "$W2") now"
three_ready || cl_fail "a node is not ready after finish: $(docker exec "$S" hivemind nodes | tr '\n' ';')"
cl_note "finish: ca.crt is the new CA alone on every node; $W2 kept its node ID, node.json and containers"
cl_note "PASS"
