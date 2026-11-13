#!/usr/bin/env bash
# Runs a three-node hivemind cluster in Docker and checks what the cluster
# does with replicas, host ports and a lost node.
#
# Three docker:dind containers share one private user-defined network, and none
# of them publishes a port. The server runs in node s with --advertise set to
# its address on that network. Workers w1 and w2 join it with join tokens. The
# hivemind binary is built in a golang container from this checkout, and copied
# into each node.
#
# Checks:
#   - listening sockets (/proc/net/tcp and tcp6) in each node: the API only on
#     127.0.0.1:4483 on the server, the cluster port (4484) and the join port
#     (4485) only on the advertise address, no wildcard on any of them, and no
#     listener on those ports in a worker
#   - Task 9: four replicas that publish host port 8080 land as three replicas
#     on three different nodes, and the fourth stays pending with the
#     host-port reason; each node answers on 127.0.0.1:8080
#   - Task 10: a worker is killed. Its replica of an app without ports runs on
#     another node within 30 s. Its replica of the app with port 8080 stays
#     pending with the host-port reason
#   - Task 10: the worker starts again with its data directory. No replica of
#     any app is duplicated, and each app is back at its replica count
#
# It publishes no ports. On exit it removes the containers, the network and the
# volumes it created, also after a failure. The Go build cache volume
# (hivemind-cache) is kept.
#
# Needs Docker on the host, and the docker:dind and golang images. Run it from
# anywhere in the repository.
set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
DIND_IMAGE=${DIND_IMAGE:-docker:dind}
GO_IMAGE=${GO_IMAGE:-golang:1.27}
CACHE_VOLUME=${CACHE_VOLUME:-hivemind-cache}

suffix=$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
run="hivemind-cl-$suffix"
net="$run-net"
work="$repo/.scratch/cluster-$suffix"
names=("$run-s" "$run-w1" "$run-w2")
S=${names[0]}
W1=${names[1]}
W2=${names[2]}
sock=/var/run/docker.sock

cleanup() {
	local rc=$?
	for c in "${names[@]}"; do
		docker rm -fv "$c" >/dev/null 2>&1 || true
	done
	docker network rm "$net" >/dev/null 2>&1 || true
	for c in "${names[@]}"; do
		docker volume rm "$c-docker" >/dev/null 2>&1 || true
	done
	rm -rf "$work"
	exit "$rc"
}
trap cleanup EXIT

note() { echo "cluster: $*"; }

dump_logs() {
	local c
	for c in "${names[@]}"; do
		echo "cluster: --- last lines of $c:/var/log/hivemind.log" >&2
		docker exec "$c" sh -c 'tail -n 40 /var/log/hivemind.log' >&2 2>/dev/null || true
	done
}

fail() {
	echo "cluster: FAIL: $*" >&2
	dump_logs
	exit 1
}

# wait_for SECONDS COMMAND...: retries COMMAND once a second until it succeeds
# or SECONDS of wall time have passed.
wait_for() {
	local secs=$1
	shift
	local end=$(($(date +%s) + secs))
	while :; do
		if "$@" >/dev/null 2>&1; then
			return 0
		fi
		if [ "$(date +%s)" -ge "$end" ]; then
			return 1
		fi
		sleep 1
	done
}

ip_of() {
	docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$1"
}

# hexip prints an IPv4 address the way /proc/net/tcp writes it: little-endian hex.
hexip() {
	local IFS=.
	set -- $1
	printf '%02X%02X%02X%02X' "$4" "$3" "$2" "$1"
}

# listeners prints the local address:port of each listening TCP socket in a node.
listeners() {
	docker exec "$1" sh -c 'cat /proc/net/tcp /proc/net/tcp6' | awk '$4 == "0A" { print $2 }' | sort -u
}

node_id_of() {
	docker exec "$1" sh -c "sed -n 's/.*\"node_id\": *\"\(node-[0-9a-f]*\)\".*/\1/p' /var/lib/hivemind/node.json | head -n 1"
}

# app_rows APP prints "index node state detail" for each replica of APP, from
# the replica table of hivemind status. The node is "-" when the replica has
# none.
app_rows() {
	docker exec "$S" hivemind status "$1" | awk -v app="$1" '$1 == app && $2 ~ /^[0-9]+$/ {
		d = ""
		for (i = 7; i <= NF; i++) d = d $i " "
		print $2, $3, $5, d
	}'
}

running_on() { # running_on APP NODE: count of APP's running replicas on NODE
	app_rows "$1" | awk -v n="$2" '$3 == "running" && $2 == n' | wc -l
}

count_running() { # count_running APP
	app_rows "$1" | awk '$3 == "running"' | wc -l
}

# web_ok: three replicas run on three nodes, and one is pending on the host port.
web_ok() {
	local rows n_run n_nodes n_inuse
	rows=$(app_rows web)
	n_run=$(awk '$3 == "running"' <<<"$rows" | wc -l)
	n_nodes=$(awk '$3 == "running" { print $2 }' <<<"$rows" | sort -u | wc -l)
	n_inuse=$(awk '$3 == "pending" && /in use/' <<<"$rows" | wc -l)
	[ "$n_run" -eq 3 ] && [ "$n_nodes" -eq 3 ] && [ "$n_inuse" -eq 1 ]
}

# spread_on_three: three replicas of spread run, on three nodes.
spread_on_three() {
	local rows n_run n_nodes
	rows=$(app_rows spread)
	n_run=$(awk '$3 == "running"' <<<"$rows" | wc -l)
	n_nodes=$(awk '$3 == "running" { print $2 }' <<<"$rows" | sort -u | wc -l)
	[ "$n_run" -eq 3 ] && [ "$n_nodes" -eq 3 ]
}

# spread_off NODE: three replicas of spread run, and none is on NODE.
spread_off() {
	local rows n_run n_on
	rows=$(app_rows spread)
	n_run=$(awk '$3 == "running"' <<<"$rows" | wc -l)
	n_on=$(awk -v n="$1" '$3 == "running" && $2 == n' <<<"$rows" | wc -l)
	[ "$n_run" -eq 3 ] && [ "$n_on" -eq 0 ]
}

# containers_of APP prints "node name state" for every container of APP in
# every node, exited ones included.
containers_of() {
	local c
	for c in "${names[@]}"; do
		docker exec "$c" docker ps -a --filter "label=hivemind.app=$1" --format '{{.Names}} {{.State}}' |
			sed "s/^/$c /"
	done
}

# no_duplicates: no replica name appears twice across the nodes, for any app.
no_duplicates() {
	local dup
	dup=$( (containers_of web; containers_of spread) | awk '{ print $2 }' | sort | uniq -d)
	[ -z "$dup" ]
}

mkdir -p "$work"
note "building the hivemind binary for linux in $GO_IMAGE"
docker run --rm -v "$repo:/src" -v "$CACHE_VOLUME:/cache" -w /src \
	-e GOMODCACHE=/cache/mod -e GOCACHE=/cache/build -e GOFLAGS=-buildvcs=false \
	-e GOPROXY=direct -e GOSUMDB=off -e GOTOOLCHAIN=local -e CGO_ENABLED=0 \
	"$GO_IMAGE" go build -trimpath -o "/src/.scratch/cluster-$suffix/hivemind" ./cmd/hivemind

note "starting three nodes on $net: $S (server), $W1 and $W2 (workers)"
docker network create "$net" >/dev/null
for c in "${names[@]}"; do
	# The first argument is dockerd, so the image's entrypoint adds no TCP
	# listeners. Only the unix socket is served.
	docker run -d --name "$c" --privileged --network "$net" -v "$c-docker:/var/lib/docker" \
		"$DIND_IMAGE" dockerd -H "unix://$sock" >/dev/null
done
for c in "${names[@]}"; do
	wait_for 90 docker exec "$c" docker info || fail "the Docker daemon in $c did not start"
	docker cp "$work/hivemind" "$c:/usr/local/bin/hivemind"
done
S_IP=$(ip_of "$S")
W1_IP=$(ip_of "$W1")
W2_IP=$(ip_of "$W2")
note "addresses: server $S_IP, $W1 $W1_IP, $W2 $W2_IP"

# Server: init, then serve with --advertise on its own address.
S_ID=$(docker exec "$S" hivemind init | sed -n 's/.*(node \(node-[0-9a-f]*\)).*/\1/p')
[ -n "$S_ID" ] || fail "init printed no node ID"
docker exec -d "$S" sh -c "exec hivemind serve --advertise $S_IP >>/var/log/hivemind.log 2>&1"
wait_for 30 docker exec "$S" hivemind status || fail "the API did not answer on 127.0.0.1:4483"

# Listening sockets on the server.
l=$(listeners "$S")
grep -qx '0100007F:1183' <<<"$l" || fail "the API is not on 127.0.0.1:4483; listeners: $l"
grep -qx "$(hexip "$S_IP"):1184" <<<"$l" || fail "the cluster port is not on $S_IP:4484; listeners: $l"
grep -qx "$(hexip "$S_IP"):1185" <<<"$l" || fail "the join port is not on $S_IP:4485; listeners: $l"
extra=$(grep -E ':(1183|1184|1185)$' <<<"$l" | grep -vxE "0100007F:1183|$(hexip "$S_IP"):118[45]" || true)
[ -z "$extra" ] || fail "the server listens on an unexpected address for hivemind ports: $extra"
wild=$(grep -E '^(0{8}|0{32}):(1183|1184|1185)$' <<<"$l" || true)
[ -z "$wild" ] || fail "a hivemind port listens on a wildcard address: $wild"
note "server listeners: API on 127.0.0.1:4483; cluster and join ports on $S_IP only"

# Workers: a join token each, then join.
TOKEN1=$(docker exec "$S" hivemind join-token --ttl 1h 2>/dev/null) || fail "join-token failed"
docker exec -d -e "HM_TOKEN=$TOKEN1" "$W1" sh -c "exec hivemind join $S_IP:4484 --token \"\$HM_TOKEN\" >>/var/log/hivemind.log 2>&1"
wait_for 60 docker exec "$W1" test -s /var/lib/hivemind/node.json || fail "$W1 did not join"
TOKEN2=$(docker exec "$S" hivemind join-token --ttl 1h 2>/dev/null) || fail "join-token failed"
docker exec -d -e "HM_TOKEN=$TOKEN2" "$W2" sh -c "exec hivemind join $S_IP:4484 --token \"\$HM_TOKEN\" >>/var/log/hivemind.log 2>&1"
wait_for 60 docker exec "$W2" test -s /var/lib/hivemind/node.json || fail "$W2 did not join"
W1_ID=$(node_id_of "$W1")
W2_ID=$(node_id_of "$W2")
[ -n "$W1_ID" ] && [ -n "$W2_ID" ] || fail "a worker has no node ID in node.json"
note "joined: server $S_ID, $W1 $W1_ID, $W2 $W2_ID"

for c in "$W1" "$W2"; do
	extra=$(listeners "$c" | grep -E ':(1183|1184|1185)$' || true)
	[ -z "$extra" ] || fail "$c listens on a hivemind port, which a worker must not: $extra"
done
note "workers listen on no hivemind port"

# A worker takes replicas only after it has been ready for --min-ready (30 s).
# The wait is long enough for both workers to pass it.
note "waiting 40 s, the readiness wait for the workers"
sleep 40

# Task 9: four replicas on host port 8080.
docker exec "$S" hivemind deploy --name web --image nginx:alpine --replicas 4 --port 8080:80 >/dev/null ||
	fail "deploy web"
wait_for 150 web_ok || fail "web did not settle to three running replicas on three nodes and one pending on the host port: $(app_rows web | tr '\n' ';')"
note "web: three replicas run on three nodes; one is pending on host port 8080"

for id in "$S_ID" "$W1_ID" "$W2_ID"; do
	if [ "$(running_on web "$id")" -eq 1 ]; then
		c=$S
		[ "$id" = "$W1_ID" ] && c=$W1
		[ "$id" = "$W2_ID" ] && c=$W2
		wait_for 30 docker exec "$c" sh -c 'wget -q -O - http://127.0.0.1:8080/ | grep -q nginx' ||
			fail "the web replica on $id does not answer on 127.0.0.1:8080"
	fi
done
note "each node answers on 127.0.0.1:8080 for its web replica"

# Task 10: spread has three replicas without ports, one per node.
docker exec "$S" hivemind deploy --name spread --image nginx:alpine --replicas 3 >/dev/null || fail "deploy spread"
wait_for 90 spread_on_three || fail "spread did not run on three nodes: $(app_rows spread | tr '\n' ';')"

docker kill "$W2" >/dev/null
killed_at=$(date +%s)
note "killed $W2 ($W2_ID) at $(date -u +%T)"
wait_for 30 spread_off "$W2_ID" ||
	fail "spread replicas did not move off $W2_ID within 30 s: $(app_rows spread | tr '\n' ';')"
note "spread moved off $W2_ID within $(($(date +%s) - killed_at)) s"

# web's replica 3 was already pending. The replica that ran on the killed node
# is the second pending one, and its reason is the host-port conflict.
rows=$(app_rows web)
inuse=$(awk '$3 == "pending" && /in use/' <<<"$rows" | wc -l)
[ "$inuse" -eq 2 ] || fail "web does not have two replicas pending on the host port: $(tr '\n' ';' <<<"$rows")"
note "web: two replicas pending on host port 8080 (index, node, state, reason): $(awk '$3 == "pending"' <<<"$rows" | tr '\n' ';')"

# Worker back, with its data directory.
docker start "$W2" >/dev/null
wait_for 90 docker exec "$W2" docker info || fail "the Docker daemon in $W2 did not start again"
docker exec -d "$W2" sh -c 'exec hivemind serve --role worker >>/var/log/hivemind.log 2>&1'
note "restarted $W2 with its data directory"

# The worker is ready again after 30 s, and web's replica goes back to it.
wait_for 150 web_ok || fail "web did not come back to three running replicas: $(app_rows web | tr '\n' ';')"
wait_for 60 no_duplicates || fail "a replica is duplicated across nodes: $( (containers_of web; containers_of spread) | sort | tr '\n' ';')"
[ "$(count_running spread)" -eq 3 ] || fail "spread is not at three running replicas after the worker returned"
note "after the return: web and spread at their replica counts, no duplicate replicas"

# The listeners are unchanged after the restart.
l=$(listeners "$S")
grep -qx '0100007F:1183' <<<"$l" && grep -qx "$(hexip "$S_IP"):1184" <<<"$l" || fail "server listeners changed: $l"
extra=$(listeners "$W2" | grep -E ':(1183|1184|1185)$' || true)
[ -z "$extra" ] || fail "$W2 listens on a hivemind port after the restart: $extra"

note "all cluster checks passed"
