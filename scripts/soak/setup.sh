#!/usr/bin/env bash
# Install hivemind on the soak machines, as a user would, and deploy the
# workloads.
#
#   SOAK_DIR=... BIN=path/to/hivemind scripts/soak/setup.sh
#   SOAK_DIR=... scripts/soak/setup.sh rejoin NAME    join a removed worker again
#
# The first machine in $NODES_FILE becomes the server, the others workers.
# Each machine needs Docker Engine running, jq, curl, iptables, and an ssh
# login with passwordless sudo. Each step is the one the docs give
# (docs/quick-start.md, the README's "Add a machine", docs/systemd.md):
#
#   1. install the binary in /usr/local/bin on every machine;
#   2. server: hivemind init, then a systemd unit that runs
#      hivemind serve --advertise <its private address>;
#   3. each worker: hivemind join with a join token from the server; once
#      node.json exists, stop the foreground join and start the worker's unit,
#      which runs hivemind serve --role worker;
#   4. wait for every node to be ready, plus the 30 s --min-ready wait;
#   5. deploy web (nginx, 3 replicas, 127.0.0.1:8080), spread (busybox sleep,
#      6 replicas, no port) and kv (redis with --appendonly yes, a volume,
#      127.0.0.1:6379), wait for convergence, and write the kv value.
#      With SETUP_APPS=0 it stops after step 4, for a run that deploys its
#      own apps (scale.sh).
#
# With SOAK_ROUTES=1 (the routing soak; run outside.sh setup first):
#   - each node is prepared first (prepare_node in lib.sh: /etc/hosts, Pebble's
#     root in the system trust, the registry's CA in /etc/docker/certs.d);
#   - every node runs a router on its own private address (route_flags), the
#     server through its unit, each worker through join and its unit;
#   - the registry credential is stored with hivemind registry login;
#   - the apps are www (the private image, 3 replicas, routed as
#     www.soak.test, HTTP health check), api (nginx, 2 replicas, routed as
#     api.soak.test, health check), spread, kv, and the compose project
#     "shop" (compose-shop.yaml: front, nginx on 127.0.0.1:8081, and cache,
#     redis without a port);
#   - it waits until every router serves both hosts.
#
# It logs the machines' versions and every TCP listener, and fails if a
# hivemind or app port listens on a wildcard address.
set -euo pipefail
. "$(dirname "$0")/lib.sh"
HERE=$(cd "$(dirname "$0")" && pwd)

WEB_IMAGE=${WEB_IMAGE:-nginx:alpine}
SPREAD_IMAGE=${SPREAD_IMAGE:-busybox:stable}
KV_IMAGE=${KV_IMAGE:-redis:7-alpine}

# machine_facts NAME: versions and sizes, one log line each.
machine_facts() {
	local n=$1
	ev machine "$n kernel=$(on "$n" 'uname -r') os=\"$(on "$n" '. /etc/os-release; echo $PRETTY_NAME')\""
	ev machine "$n docker=$(on "$n" "docker version --format '{{.Server.Version}}'") cpus=$(on "$n" nproc) mem_kb=$(on "$n" "awk '/MemTotal/ {print \$2}' /proc/meminfo")"
	ev machine "$n cpu=\"$(on "$n" "lscpu | sed -n 's/^Model name: *//p' | head -n 1")\" hivemind=\"$(on "$n" "$HM version" | tr '\n' ' ')\""
}

# install_server: the server node, initialised, served by systemd, its API up.
install_server() {
	note "server $SERVER ($SERVER_IP): init, unit with serve --advertise $SERVER_IP"
	# init prints the admin token once; keep it out of the log. It is in
	# $DATA/admin.token, mode 0600, for the CLI as root.
	hm "$SERVER" init >/dev/null
	install_unit "$SERVER" "$(server_unit "$SERVER_IP")"
	wait_for 60 hm "$SERVER" status || die "the API on $SERVER did not answer"
}


# write_ids: the "id name" map, from each worker's node.json and the server's node list.
write_ids() {
	local i n id
	: >"$IDS_FILE.tmp"
	id=$(hm "$SERVER" nodes --json | jq -r '.[] | select(.server) | .id')
	[ -n "$id" ] || die "no server node in hivemind nodes"
	echo "$id $SERVER" >>"$IDS_FILE.tmp"
	for ((i = 1; i < ${#NAMES[@]}; i++)); do
		n=${NAMES[$i]}
		id=$(on "$n" "sudo jq -r .node_id $DATA/node.json")
		[ -n "$id" ] || die "$n has no node ID"
		echo "$id $n" >>"$IDS_FILE.tmp"
	done
	mv "$IDS_FILE.tmp" "$IDS_FILE"
	ev ids "$(tr '\n' ' ' <"$IDS_FILE")"
}

all_ready() {
	local want=${#NAMES[@]}
	[ "$(hm "$SERVER" nodes --json | jq '[.[] | select(.state == "ready")] | length')" = "$want" ]
}

deploy() { # deploy JSON
	hm "$SERVER" "deploy -f -" <<<"$1" >/dev/null
}

deploy_routed() {
	deploy "$(www_spec 1)"
	deploy "{\"name\":\"api\",\"image\":\"$WEB_IMAGE\",\"replicas\":2,\"routes\":[{\"host\":\"api.soak.test\",\"port\":80}],\"health\":{\"http\":{\"port\":80,\"path\":\"/\"},\"interval\":\"2s\",\"timeout\":\"1s\"}}"
	deploy "{\"name\":\"spread\",\"image\":\"$SPREAD_IMAGE\",\"replicas\":6,\"command\":[\"sleep\",\"86400\"]}"
	deploy "{\"name\":\"kv\",\"image\":\"$KV_IMAGE\",\"command\":[\"redis-server\",\"--appendonly\",\"yes\"],\"ports\":[{\"host\":6379,\"container\":6379,\"bind\":\"127.0.0.1\"}],\"volumes\":[{\"name\":\"data\",\"path\":\"/data\"}]}"
	hm "$SERVER" "compose deploy -" <"$HERE/compose-shop.yaml" >/dev/null || die "compose deploy shop"
	ev setup "deployed www api spread kv; compose project shop: front cache"
}

kv_node() { # the soak name of the node that runs kv replica 0
	local st id
	st=$(hm "$SERVER" status --json)
	id=$(jq -r '.[] | select(.name == "kv") | .replicas[0].node_id // ""' <<<"$st")
	[ -n "$id" ] && name_of_id "$id"
}


check_listeners() {
	local n l bad
	for n in "${NAMES[@]}"; do
		l=$(on "$n" "sudo ss -ltnpH" | awk '{ print $4, $6 }')
		ev listeners "$n $(awk '{ printf "%s;", $1 }' <<<"$l")"
		bad=$(awk '{ split($1, a, ":"); port = a[length(a)]; addr = substr($1, 1, length($1) - length(port) - 1) }
			(port == 4483 || port == 4484 || port == 4485 || port == 8080 || port == 8081 || port == 6379 || port == 80 || port == 443) && (addr == "0.0.0.0" || addr == "*" || addr == "[::]") { print $1 }' <<<"$l")
		[ -z "$bad" ] || die "$n: a hivemind or app port listens on a wildcard address: $bad"
	done
}

main() {
	local n val
	ev setup "begin nodes=${#NAMES[@]} server=$SERVER binary_sha256=$(sha256sum "$BIN" | cut -d' ' -f1)"
	for n in "${NAMES[@]}"; do install_bin "$n" & done
	wait
	for n in "${NAMES[@]}"; do machine_facts "$n"; done
	if [ "$SOAK_ROUTES" = 1 ]; then
		for n in "${NAMES[@]}"; do prepare_node "$n"; done
	fi
	install_server
	for ((i = 1; i < ${#NAMES[@]}; i++)); do join_worker "${NAMES[$i]}"; done
	write_ids
	wait_for 60 all_ready || die "not every node is ready: $(hm "$SERVER" nodes)"
	note "all ${#NAMES[@]} nodes ready; waiting 40 s, the --min-ready wait, before the first deploy"
	sleep 40
	if [ "${SETUP_APPS:-1}" = 0 ]; then
		ev setup "end: no apps (SETUP_APPS=0)"
		note "setup done, without apps"
		return 0
	fi
	if [ "$SOAK_ROUTES" = 1 ]; then
		"$HERE/outside.sh" login
		deploy_routed
	else
		deploy "{\"name\":\"web\",\"image\":\"$WEB_IMAGE\",\"replicas\":3,\"ports\":[{\"host\":8080,\"container\":80,\"bind\":\"127.0.0.1\"}]}"
		deploy "{\"name\":\"spread\",\"image\":\"$SPREAD_IMAGE\",\"replicas\":6,\"command\":[\"sleep\",\"86400\"]}"
		deploy "{\"name\":\"kv\",\"image\":\"$KV_IMAGE\",\"command\":[\"redis-server\",\"--appendonly\",\"yes\"],\"ports\":[{\"host\":6379,\"container\":6379,\"bind\":\"127.0.0.1\"}],\"volumes\":[{\"name\":\"data\",\"path\":\"/data\"}]}"
		ev setup "deployed web spread kv"
	fi
	wait_for 300 converged_now || die "not converged 300 s after the deploy: $(check_converged "$SOAK_DIR/setup.tsv" | tr '\n' ';')"
	[ "$SOAK_ROUTES" = 1 ] && "$HERE/outside.sh" wait-served 300
	n=$(kv_node) || die "kv has no node"
	val="written-$(date -u +%FT%TZ)"
	[ "$(kv_set "$n" "$val")" = "+OK" ] || die "could not write the kv value on $n"
	echo "$val" >"$SOAK_DIR/kv-value"
	ev kv "write node=$n key=soak:canary value=$val"
	for n in "${NAMES[@]}"; do
		ev images "$n $(on "$n" "docker images --digests --format '{{.Repository}}:{{.Tag}}@{{.Digest}}'" | tr '\n' ' ')"
	done
	check_listeners
	ev setup "end: converged; $(awk -F'\t' '$1 == "R" { printf "%s|%s@%s ", $2, $3, $4 }' "$SOAK_DIR/setup.tsv")"
	note "setup done"
}

converged_now() { snapshot "$SOAK_DIR/setup.tsv" && check_converged "$SOAK_DIR/setup.tsv" >/dev/null; }

# rejoin NAME: bring back a worker that hivemind node remove removed, as
# docs/cli.md says: stop its service, remove its containers by hand, empty its
# data directory, and join again with a new token.
rejoin() {
	local n=$1
	note "rejoining $n with an empty data directory"
	SSH_TIMEOUT=180 on "$n" "sudo systemctl disable --now hivemind.service; ids=\$(sudo docker ps -aq --filter label=hivemind.app); [ -z \"\$ids\" ] || sudo docker rm -f \$ids >/dev/null; sudo rm -rf $DATA" ||
		die "$n: could not clear the old node"
	join_worker "$n"
	write_ids
	ev setup "rejoined $n as $(id_of "$n")"
}

case "${1:-}" in
rejoin) rejoin "${2:?name}" ;;
*) main "$@" ;;
esac
