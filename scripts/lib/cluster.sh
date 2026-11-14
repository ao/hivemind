# Shared helpers for the scripts that run a hivemind cluster in Docker: one
# server and N-1 workers, each a docker:dind container on a private network.
# Sourced by scripts/integration-cluster.sh and scripts/fault-suite.sh. The
# caller sets REPO before sourcing, and calls cl_init, then cl_start_nodes,
# and cl_trap_cleanup. Written for bash 3.2 (macOS) as well as bash 5.
#
# Node names: index 0 is the server, 1.. are workers. CL_NAMES and CL_IDS are
# parallel arrays; CL_IDS is filled by cl_join.

DIND_IMAGE=${DIND_IMAGE:-docker:dind}
GO_IMAGE=${GO_IMAGE:-golang:1.27}
CACHE_VOLUME=${CACHE_VOLUME:-hivemind-cache}
CL_SOCK=/var/run/docker.sock

cl_note() { echo "cluster: $*"; }

cl_fail() {
	echo "cluster: FAIL: $*" >&2
	cl_dump_logs
	exit 1
}

# cl_init NAME: sets the run name, the network, the work directory and the
# scratch dir for the binary. Run names are unique per invocation.
cl_init() {
	local suffix
	suffix=$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
	CL_SUFFIX=$suffix
	CL_RUN="hivemind-$1-$suffix"
	CL_NET="$CL_RUN-net"
	CL_WORK="$REPO/.scratch/$1-$suffix"
	CL_NAMES=()
	CL_IDS=()
}

# cl_add_node: append a container name for the next node index.
cl_add_node() {
	CL_NAMES[${#CL_NAMES[@]}]="$CL_RUN-n${#CL_NAMES[@]}"
}

cl_cleanup() {
	local rc=$? c
	for c in "${CL_NAMES[@]}"; do
		docker rm -fv "$c" >/dev/null 2>&1 || true
	done
	docker network rm "$CL_NET" >/dev/null 2>&1 || true
	for c in "${CL_NAMES[@]}"; do
		docker volume rm "$c-docker" >/dev/null 2>&1 || true
	done
	[ -n "${CL_WORK:-}" ] && rm -rf "$CL_WORK"
	exit "$rc"
}

# cl_trap_cleanup: run cl_cleanup on exit, and turn Ctrl-C and TERM into an exit.
cl_trap_cleanup() {
	trap cl_cleanup EXIT
	trap 'exit 130' INT
	trap 'exit 143' TERM
	trap 'exit 129' HUP
}

cl_dump_logs() {
	local c
	for c in "${CL_NAMES[@]}"; do
		echo "cluster: --- last lines of $c:/var/log/hivemind.log" >&2
		docker exec "$c" sh -c 'tail -n 40 /var/log/hivemind.log' >&2 2>/dev/null || true
	done
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

cl_ip_of() {
	docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$1"
}

# cl_hexip prints an IPv4 address the way /proc/net/tcp writes it: little-endian hex.
cl_hexip() {
	local IFS=.
	set -- $1
	printf '%02X%02X%02X%02X' "$4" "$3" "$2" "$1"
}

# cl_listeners prints the local address:port of each listening TCP socket in a node.
cl_listeners() {
	docker exec "$1" sh -c 'cat /proc/net/tcp /proc/net/tcp6' | awk '$4 == "0A" { print $2 }' | sort -u
}

cl_node_id_of() {
	docker exec "$1" sh -c "sed -n 's/.*\"node_id\": *\"\(node-[0-9a-f]*\)\".*/\1/p' /var/lib/hivemind/node.json | head -n 1"
}

# cl_start_nodes: create the network, start every node as dind, copy in the
# binary built into $CL_WORK/hivemind, and wait for each Docker daemon.
cl_start_nodes() {
	local c
	mkdir -p "$CL_WORK"
	cl_note "building the hivemind binary for linux in $GO_IMAGE"
	docker run --rm -v "$REPO:/src" -v "$CACHE_VOLUME:/cache" -w /src \
		-e GOMODCACHE=/cache/mod -e GOCACHE=/cache/build -e GOFLAGS=-buildvcs=false \
		-e GOPROXY=direct -e GOSUMDB=off -e GOTOOLCHAIN=local -e CGO_ENABLED=0 \
		"$GO_IMAGE" go build -trimpath -o "/src/${CL_WORK#"$REPO"/}/hivemind" ./cmd/hivemind
	docker network create "$CL_NET" >/dev/null
	for c in "${CL_NAMES[@]}"; do
		# The first argument is dockerd, so the image's entrypoint adds no TCP
		# listeners. Only the unix socket is served.
		docker run -d --name "$c" --privileged --network "$CL_NET" -v "$c-docker:/var/lib/docker" \
			"$DIND_IMAGE" dockerd -H "unix://$CL_SOCK" >/dev/null
	done
	for c in "${CL_NAMES[@]}"; do
		wait_for 90 docker exec "$c" docker info || cl_fail "the Docker daemon in $c did not start"
		docker cp "$CL_WORK/hivemind" "$c:/usr/local/bin/hivemind"
	done
}

# cl_start_server: init the server (index 0) and serve it with --advertise on
# its own address. Sets CL_S_IP and CL_S_ID.
cl_start_server() {
	local s=${CL_NAMES[0]}
	CL_S_IP=$(cl_ip_of "$s")
	CL_S_ID=$(docker exec "$s" hivemind init | sed -n 's/.*(node \(node-[0-9a-f]*\)).*/\1/p')
	[ -n "$CL_S_ID" ] || cl_fail "init printed no node ID"
	cl_serve_server
	wait_for 30 docker exec "$s" hivemind status || cl_fail "the API did not answer on 127.0.0.1:4483"
}

# cl_serve_server starts (or restarts) the server process in the background.
cl_serve_server() {
	docker exec -d "${CL_NAMES[0]}" sh -c "exec hivemind serve --advertise $CL_S_IP >>/var/log/hivemind.log 2>&1"
}

# cl_join: each worker (index 1..) gets a join token from the server and joins.
# Sets CL_IDS[i] to the node ID of each node, the server included.
cl_join() {
	local s=${CL_NAMES[0]} i tok c
	CL_IDS[0]=$CL_S_ID
	for ((i = 1; i < ${#CL_NAMES[@]}; i++)); do
		c=${CL_NAMES[$i]}
		tok=$(docker exec "$s" hivemind join-token --ttl 1h 2>/dev/null) || cl_fail "join-token failed"
		docker exec -d -e "HM_TOKEN=$tok" "$c" sh -c "exec hivemind join $CL_S_IP:4484 --token \"\$HM_TOKEN\" >>/var/log/hivemind.log 2>&1"
		wait_for 60 docker exec "$c" test -s /var/lib/hivemind/node.json || cl_fail "$c did not join"
		CL_IDS[$i]=$(cl_node_id_of "$c")
		[ -n "${CL_IDS[$i]}" ] || cl_fail "$c has no node ID in node.json"
	done
	cl_note "joined: ${CL_NAMES[0]} ${CL_IDS[0]}; $(for ((i = 1; i < ${#CL_NAMES[@]}; i++)); do printf '%s %s; ' "${CL_NAMES[$i]}" "${CL_IDS[$i]}"; done)"
}

# cl_check_server_listeners: the API only on 127.0.0.1:4483, the cluster and
# join ports only on the advertise address, and no wildcard for either.
cl_check_server_listeners() {
	local s=${CL_NAMES[0]} l extra wild
	l=$(cl_listeners "$s")
	grep -qx '0100007F:1183' <<<"$l" || cl_fail "the API is not on 127.0.0.1:4483; listeners: $l"
	grep -qx "$(cl_hexip "$CL_S_IP"):1184" <<<"$l" || cl_fail "the cluster port is not on $CL_S_IP:4484; listeners: $l"
	grep -qx "$(cl_hexip "$CL_S_IP"):1185" <<<"$l" || cl_fail "the join port is not on $CL_S_IP:4485; listeners: $l"
	extra=$(grep -E ':(1183|1184|1185)$' <<<"$l" | grep -vxE "0100007F:1183|$(cl_hexip "$CL_S_IP"):118[45]" || true)
	[ -z "$extra" ] || cl_fail "the server listens on an unexpected address for hivemind ports: $extra"
	wild=$(grep -E '^(0{8}|0{32}):(1183|1184|1185)$' <<<"$l" || true)
	[ -z "$wild" ] || cl_fail "a hivemind port listens on a wildcard address: $wild"
}

# cl_check_worker_listeners: no worker listens on a hivemind port.
cl_check_worker_listeners() {
	local c extra i
	for ((i = 1; i < ${#CL_NAMES[@]}; i++)); do
		c=${CL_NAMES[$i]}
		extra=$(cl_listeners "$c" | grep -E ':(1183|1184|1185)$' || true)
		[ -z "$extra" ] || cl_fail "$c listens on a hivemind port, which a worker must not: $extra"
	done
}
