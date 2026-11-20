#!/usr/bin/env bash
# Install hivemind on the soak machines, as a user would, and deploy the
# workloads.
#
#   SOAK_DIR=... BIN=path/to/hivemind scripts/soak/setup.sh
#   SOAK_DIR=... scripts/soak/setup.sh rejoin NAME    join a removed worker again
#   SOAK_DIR=... scripts/soak/setup.sh check-paths    which app answers each path
#                                                     of $PATH_HOST, per router
#
# The first machine in $NODES_FILE becomes the server, the others workers.
#
# With SOAK_SERVERS=N (the soak of several servers, docs/ha.md, ha.sh), the
# first N machines are servers: the first one init makes, as below; each of
# the others joins it with a server token (hivemind join-token --role server)
# and hivemind join <first> --role server --advertise <its address> in the
# foreground until hivemind servers lists it as a voter, then runs the same
# unit as the first, serve --advertise <its address> with the router's
# flags (join_server in lib.sh); the workers join the first server's
# address, and keep the list of servers in node.json. Before hivemind runs,
# ports_check (lib.sh) checks that the security group lets the servers reach
# each other's port 4486 and the client the API's 4483. With SOAK_API_TLS=1
# (the default with N > 1) every server serves the API on its private
# address with a certificate from a CA made for the run (api_certs), and the
# outside machine gets the CLI as a client that knows every server
# (wload.sh client-setup). sqlite3 is installed on the servers for the
# write-load's check.
# Each machine needs Docker Engine running, jq, curl, iptables, and an ssh
# login with passwordless sudo. Each step is the one the docs give
# (docs/quick-start.md, the README's "Add a machine", docs/systemd.md):
#
#   1. install the binary in /usr/local/bin on every machine;
#   2. server: hivemind init, then a systemd unit that runs
#      hivemind serve --advertise <its private address>;
#   3. each worker: hivemind join with a join token from the server; once
#      node.json exists, stop the foreground join and start the worker's unit,
#      which runs hivemind serve --role worker with no flags: join wrote its
#      router flags to hivemind.yaml (WORKER_UNIT_FLAGS=1 puts them in the
#      unit too, as before the sixth soak); with SOAK_ROUTES=1, until
#      hivemind nodes shows ROUTER yes for it;
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
# With SOAK_PATHS=1 as well (the fourth soak), two more routed apps share one
# host, proot on $PATH_HOST and papi on $PATH_HOST/api (path_spec in lib.sh),
# and check_paths logs which app answers each of a list of paths through
# every router, and the paths the router refuses.
#
# With SOAK_GRPC=1 as well (the sixth soak), grpc runs test/h2ctest's server
# under $PATH_HOST/h2ctest.Echo on an h2c route (grpc_spec in lib.sh), and
# check_grpc makes one checked call of each kind through every router.
#   SOAK_DIR=... scripts/soak/setup.sh check-grpc
#
# With SERVER_NAME set (the third soak), the server advertises that name
# (--advertise NAME --cluster-addr <its address>), the workers join the name,
# and a line in /etc/hosts on every machine, the outside one included, stands
# in for its DNS record (lib.sh).
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
	# $DATA/admin.token, mode 0600, for the CLI as root (and, with
	# SOAK_API_TLS=1, copied to $SOAK_ETC for the soak's client config).
	on "$SERVER" "sudo $HM init" >/dev/null
	token_to "$SERVER"
	install_unit "$SERVER" "$(server_unit "$SERVER_IP")"
	wait_for 60 hm "$SERVER" status || die "the API on $SERVER did not answer"
	ev setup "$SERVER unit ExecStart=$(on "$SERVER" "systemctl show -p ExecStart --value hivemind.service | sed -n 's/.*argv\[\]=\([^;]*\);.*/\1/p'")"
	if [ "$SOAK_SERVERS" -gt 1 ] && [ "$SOAK_ROUTES" = 1 ]; then
		wait_for 90 router_up_id "$SERVER" || die "$SERVER: no router: $(hma nodes 2>&1 | tr -s ' \n' ' ')"
	fi
}

# servers_facts: with several servers, how they see the cluster: hivemind
# servers on each, and each worker's list of servers in its node.json.
servers_facts() {
	local n
	[ "$SOAK_SERVERS" -gt 1 ] || return 0
	ev setup "servers $(servers_line)"
	while read -r line; do ev servers "$line"; done < <(hma servers 2>&1)
	for n in "${NAMES[@]:SOAK_SERVERS}"; do
		ev setup "$n node.json server=$(on "$n" "sudo jq -c '[.server, .servers]' $DATA/node.json" 2>&1 | tr -d ' ')"
	done
}

# write_ids: the "id name" map, from each worker's node.json and the server's node list.
write_ids() {
	local i n id
	: >"$IDS_FILE.tmp"
	if [ "$SOAK_SERVERS" = 1 ]; then
		id=$(hm "$SERVER" nodes --json | jq -r '.[] | select(.server) | .id')
	else
		id=$(node_id_on "$SERVER") || id=""
	fi
	[ -n "$id" ] || die "no server node in hivemind nodes"
	echo "$id $SERVER" >>"$IDS_FILE.tmp"
	for ((i = 1; i < ${#NAMES[@]}; i++)); do
		n=${NAMES[$i]}
		if is_server "$n"; then
			id=$(node_id_on "$n") || id=""
		else
			id=$(on "$n" "sudo jq -r .node_id $DATA/node.json")
		fi
		[ -n "$id" ] || die "$n has no node ID"
		echo "$id $n" >>"$IDS_FILE.tmp"
	done
	mv "$IDS_FILE.tmp" "$IDS_FILE"
	ev ids "$(tr '\n' ' ' <"$IDS_FILE")"
}

all_ready() {
	local want=${#NAMES[@]}
	[ "$(hma nodes --json | jq '[.[] | select(.state == "ready")] | length')" = "$want" ]
}

deploy() { # deploy JSON
	hma_in "deploy -f -" <<<"$1" >/dev/null
}

deploy_routed() {
	deploy "$(www_spec 1)"
	deploy "$(api_spec)"
	deploy "$(spread_spec)"
	deploy "$(kv_spec)"
	hma_in "compose deploy -" < <(shop_compose) >/dev/null || die "compose deploy shop"
	ev setup "deployed www api spread kv; compose project shop: front cache"
	if [ "$SOAK_PATHS" = 1 ]; then
		deploy "$(path_spec proot / 1)"
		deploy "$(path_spec papi /api 1)"
		ev setup "deployed proot ($PATH_HOST/) and papi ($PATH_HOST/api)"
	fi
	if [ "$SOAK_GRPC" = 1 ]; then
		deploy "$(grpc_spec 1)"
		ev setup "deployed grpc ($PATH_HOST$GRPC_PATH, h2c, $REGISTRY/soak/h2ctest:1)"
	fi
}

# check_grpc: through every router, one checked call of each kind to grpc
# with test/h2ctest's client on the outside machine (the message back, from
# one replica, HTTP/2, grpc-status 0): unary, a server stream of five
# messages, a bidirectional stream, and a unary call over HTTP/1.1, which the
# router sends on as HTTP/2. One "grpc" line per call: router, kind, exit
# code and the replica that answered, or the error.
check_grpc() {
	local ip k kind flag cmd="" out
	for ip in "${IPS[@]}"; do
		for k in unary server bidi "unary -h1"; do
			kind=${k%% *} flag=${k#"$kind"}
			cmd+="printf '%s %s ' $ip '${k// /_}'; o=\$(/var/lib/soak-client/h2ctest call -url https://$PATH_HOST -connect $ip:443 -ca /var/lib/soak-client/root.pem$flag $kind 2>&1); echo \"rc=\$? \$o\" | tr '\n' ' '; echo; "
		done
	done
	out=$(SSH_TIMEOUT=120 on "$OUT_NAME" "$cmd") || die "check_grpc on $OUT_NAME"
	while read -r line; do ev grpc "$line"; done <<<"$out"
	grep -q 'rc=[^0]' <<<"$out" && note "check_grpc: a call failed: $(grep 'rc=[^0]' <<<"$out" | head -n 3 | tr '\n' ';')"
	return 0
}

# grpc_served: every router routes a unary call to grpc.
grpc_served() {
	local ip cmd=""
	for ip in "${IPS[@]}"; do
		cmd+="/var/lib/soak-client/h2ctest call -url https://$PATH_HOST -connect $ip:443 -ca /var/lib/soak-client/root.pem unary >/dev/null 2>&1 || exit 1; "
	done
	on "$OUT_NAME" "$cmd"
}

# check_paths: through every router, which app answers each path of
# $PATH_HOST (the "Name:" line of whoami's reply), and the paths the router
# must refuse. Run on the outside machine, which trusts Pebble's root.
check_paths() {
	local ip p cmd="" out
	for ip in "${IPS[@]}"; do
		for p in / /x /api /api/ /api/x /apix /api.json //api//x /%61pi/x /API/x /api/../x /api%2Fx; do
			cmd+="printf '%s %s ' $ip '$p'; curl -s --path-as-is -m 5 --cacert /var/lib/soak-client/root.pem --resolve $PATH_HOST:443:$ip -o /tmp/soak-path.out -w '%{http_code} ' 'https://$PATH_HOST$p'; grep -m1 '^Name: ' /tmp/soak-path.out || { head -c 80 /tmp/soak-path.out | tr '\n' ' '; echo; }; "
		done
	done
	out=$(on "$OUT_NAME" "$cmd") || die "check_paths on $OUT_NAME"
	while read -r line; do ev paths "$line"; done <<<"$out"
	ev paths "routes $(hma routes 2>&1 | tr -s ' \n' ' ')"
}

kv_node() { # the soak name of the node that runs kv replica 0
	local st id
	st=$(hma status --json)
	id=$(jq -r '.[] | select(.name == "kv") | .replicas[0].node_id // ""' <<<"$st")
	[ -n "$id" ] && name_of_id "$id"
}


check_listeners() {
	local n l bad
	for n in "${NAMES[@]}"; do
		l=$(on "$n" "sudo ss -ltnpH" | awk '{ print $4, $6 }')
		ev listeners "$n $(awk '{ printf "%s;", $1 }' <<<"$l")"
		bad=$(awk '{ split($1, a, ":"); port = a[length(a)]; addr = substr($1, 1, length($1) - length(port) - 1) }
			(port == 4483 || port == 4484 || port == 4485 || port == 4486 || port == 8080 || port == 8081 || port == 6379 || port == 80 || port == 443) && (addr == "0.0.0.0" || addr == "*" || addr == "[::]") { print $1 }' <<<"$l")
		[ -z "$bad" ] || die "$n: a hivemind or app port listens on a wildcard address: $bad"
	done
}

main() {
	local n val
	ev setup "begin nodes=${#NAMES[@]} servers=${SRV_NAMES[*]} api_tls=$SOAK_API_TLS binary_sha256=$(sha256sum "$BIN" | cut -d' ' -f1)"
	for n in "${NAMES[@]}"; do install_bin "$n" & done
	wait
	for n in "${NAMES[@]}"; do machine_facts "$n"; done
	if [ "$SOAK_ROUTES" = 1 ]; then
		for n in "${NAMES[@]}"; do prepare_node "$n"; done
	fi
	# the security group, for the server port and the API off loopback
	ports_check
	# sqlite3 on the servers, for the write-load's check of each one's store (wload.sh)
	if [ "${WLOAD:-$SOAK_API_TLS}" = 1 ]; then
		for n in "${SRV_NAMES[@]}"; do
			SSH_TIMEOUT=300 on "$n" "command -v sqlite3 >/dev/null || sudo dnf install -y -q sqlite >/dev/null" || die "$n: no sqlite3"
			ev machine "$n sqlite3=$(on "$n" "sqlite3 --version | cut -d' ' -f1")"
		done
	fi
	api_certs
	# the server's name, on the outside machine too
	if [ -n "$SERVER_NAME" ] && [ -n "$OUT_NAME" ]; then dns_line "$OUT_NAME" "$(dns_ip)"; fi
	install_server
	for n in "${SRV_NAMES[@]:1}"; do join_server "$n"; done
	for n in "${NAMES[@]:SOAK_SERVERS}"; do join_worker "$n"; done
	write_ids
	wait_for 60 all_ready || die "not every node is ready: $(hma nodes)"
	# The table with its ROUTER column, as an operator reads it.
	while read -r line; do ev nodes "$line"; done < <(hma nodes 2>&1)
	servers_facts
	# the client that knows every server, on the outside machine (wload.sh)
	if [ "$SOAK_API_TLS" = 1 ] && [ -n "${WL_CLIENT:-$OUT_NAME}" ]; then "$HERE/wload.sh" client-setup; fi
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
	[ "$SOAK_ROUTES" = 1 ] && [ "$SOAK_PATHS" = 1 ] && check_paths
	if [ "$SOAK_ROUTES" = 1 ] && [ "$SOAK_GRPC" = 1 ]; then
		wait_for 120 grpc_served || die "not every router routes a unary call to grpc"
		check_grpc
	fi
	n=$(kv_node) || die "kv has no node"
	val="written-$(date -u +%FT%TZ)"
	[ "$(kv_set "$n" "$val")" = "+OK" ] || die "could not write the kv value on $n"
	echo "$val" >"$SOAK_DIR/kv-value"
	ev kv "write node=$n key=soak:canary value=$val"
	for n in "${NAMES[@]}"; do
		ev images "$n $(on "$n" "docker images --digests --format '{{.Repository}}:{{.Tag}}@{{.Digest}}'" | tr '\n' ' ')"
		# the log options the agent gave each container, and Docker's default driver
		ev logconfig "$n driver=$(on "$n" "docker info --format '{{.LoggingDriver}}'") $(on "$n" "ids=\$(docker ps -q --filter label=hivemind.app); [ -z \"\$ids\" ] || docker inspect -f '{{index .Config.Labels \"hivemind.app\"}}:{{.HostConfig.LogConfig.Type}}:{{.HostConfig.LogConfig.Config}}' \$ids" | tr ' \n' '_ ')"
		ev volumes "$n dangling=$(on "$n" "docker volume ls -qf dangling=true | wc -l") all=$(on "$n" "docker volume ls -q | tr '\n' ','")"
		if [ "$SOAK_ROUTES" = 1 ]; then ev ports "setup $n $(routed_ports "$n")"; fi
	done
	# compose must refuse a file that reads a HIVEMIND_ variable
	n=$(printf 'services:\n  leak:\n    image: busybox:stable\n    environment:\n      T: ${HIVEMIND_TOKEN:-x}\n' | hm "$SERVER" "compose convert -" 2>&1) &&
		die "compose convert took a file that reads HIVEMIND_TOKEN: $n"
	ev setup "compose refuses HIVEMIND_: $(tr '\n' ';' <<<"$n" | cut -c1-200)"
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
	# -v: the anonymous volumes of an image's VOLUME lines go with their
	# containers (the fifth soak left redis's behind); named volumes stay
	SSH_TIMEOUT=180 on "$n" "sudo systemctl disable --now hivemind.service; ids=\$(sudo docker ps -aq --filter label=hivemind.app); [ -z \"\$ids\" ] || sudo docker rm -fv \$ids >/dev/null; sudo rm -rf $DATA" ||
		die "$n: could not clear the old node"
	join_worker "$n"
	write_ids
	ev setup "rejoined $n as $(id_of "$n")"
}

case "${1:-}" in
rejoin) rejoin "${2:?name}" ;;
check-paths) check_paths ;;
check-grpc) check_grpc ;;
*) main "$@" ;;
esac
