#!/usr/bin/env bash
# Restores a hivemind server on a machine with a new IP address, in Docker,
# twice: once for a cluster whose server advertises a DNS name, once for one
# that advertises an IP address. The workers must come back without joining
# again.
#
# Each part runs three docker:dind containers, a server and two workers, on
# a private network of its own with a subnet of its own, and none of them
# publishes a port. The hivemind binary is built in a golang container from
# this checkout. The helpers are in scripts/lib/cluster.sh. Two apps run in
# each part: spread, three replicas without ports, and web, three replicas
# on host port 8080, one per node.
#
# By name:
#   - the server's container has the network alias hm-server, and serve runs
#     with --advertise hm-server. Its cluster listeners bind the address the
#     name resolves to, and nothing else. The workers join hm-server.
#   - the server is backed up, and its container, data directory and Docker
#     volume are destroyed. A new container at another IP, without the
#     alias, restores the backup and serves with --advertise hm-server
#     --cluster-addr <its IP>, as before a DNS record is updated: its
#     listeners bind that IP, and it warns that the name does not resolve.
#   - the alias moves to the new container (disconnected from the network
#     and connected again, at the same IP, with the alias), as the record
#     would be updated. The workers, never restarted, reconnect: the same
#     processes, node.json and node.crt unchanged, both ready, the apps
#     back at their counts, no replica twice, and the workers' containers
#     the ones that ran before.
#
# By IP address:
#   - serve runs with --advertise <its IP>, and the workers join that IP.
#   - after the same loss, a new container at another IP first runs another
#     cluster's server there. hivemind node set-server <new IP> on a
#     stopped worker is refused, as from another CA, and node.json is
#     unchanged; so is set-server to a port where nothing listens.
#   - the other server stops, the backup is restored, and serve runs with
#     --advertise <new IP>. It warns that the workers need set-server.
#     set-server under a name the new certificate lacks is refused, and so
#     is set-server beside a running worker. Each worker is stopped, moved
#     with set-server, and started with serve --role worker. Both are ready
#     with their node IDs and certificates, no node was lost, the apps are
#     back at their counts, no replica runs twice, and the workers'
#     containers are the ones that ran before.
#
# It publishes no ports. On exit it removes the containers, the networks and
# the volumes it created, also after a failure or Ctrl-C. The Go build cache
# volume (hivemind-cache) is kept.
#
# Needs Docker on the host, and the docker:dind and golang images. Run it
# from anywhere in the repository.
set -euo pipefail

REPO=$(cd "$(dirname "$0")/.." && pwd)
# shellcheck source=lib/cluster.sh
. "$REPO/scripts/lib/cluster.sh"

ALIAS=hm-server

# app_rows APP prints "index node state detail" for each replica of APP, from
# the replica table of hivemind status.
app_rows() {
	docker exec "$S" hivemind status "$1" | awk -v app="$1" '$1 == app && $2 ~ /^[0-9]+$/ {
		d = ""
		for (i = 7; i <= NF; i++) d = d $i " "
		print $2, $3, $5, d
	}'
}

# on_three APP: three replicas of APP run, on three nodes.
on_three() {
	local rows
	rows=$(app_rows "$1")
	[ "$(awk '$3 == "running"' <<<"$rows" | wc -l)" -eq 3 ] &&
		[ "$(awk '$3 == "running" { print $2 }' <<<"$rows" | sort -u | wc -l)" -eq 3 ]
}

apps_converged() { on_three spread && on_three web; }

# containers_of APP prints "node name state" for each container of APP.
containers_of() {
	local c
	for c in "${CL_NAMES[@]}"; do
		docker exec "$c" docker ps -a --filter "label=hivemind.app=$1" --format '{{.Names}} {{.State}}' | sed "s/^/$c /"
	done
}

# no_duplicates: no replica name appears on two nodes, for either app.
no_duplicates() {
	[ -z "$( (containers_of spread; containers_of web) | awk '{ print $2 }' | sort | uniq -d)" ]
}

# worker_containers NODE: the IDs of the hivemind containers on NODE.
worker_containers() { docker exec "$1" docker ps -q --filter label=hivemind.app | sort | tr '\n' ' '; }

file_sum() { docker exec "$1" sha256sum "$2" | awk '{ print $1 }'; }

workers_ready() {
	[ "$(docker exec "$S" hivemind nodes | awk -v a="$W1_ID" -v b="$W2_ID" '($1 == a || $1 == b) && / ready /' | wc -l)" -eq 2 ]
}

# server_address: the ADDRESS of the server's node in hivemind nodes.
server_address() { docker exec "$S" hivemind nodes | awk '$3 == "server" { print $4 }'; }

worker_stopped() { ! docker exec "$1" pgrep -x hivemind >/dev/null; }

# start_cluster: three nodes, the server's container with ALIAS when
# CL_ADVERTISE is set, serve, the workers joined, and both apps at their
# counts. Records what the checks after the loss compare against.
start_cluster() {
	local c i
	cl_add_node
	cl_add_node
	cl_add_node
	cl_trap_cleanup
	S=${CL_NAMES[0]}
	W1=${CL_NAMES[1]}
	W2=${CL_NAMES[2]}
	cl_build
	cl_create_network
	if [ -n "${CL_ADVERTISE:-}" ]; then
		cl_run_node "$S" --network-alias "$ALIAS"
	else
		cl_run_node "$S"
	fi
	cl_run_node "$W1"
	cl_run_node "$W2"
	for c in "${CL_NAMES[@]}"; do
		cl_ready_node "$c"
	done
	# Workers take replicas once they have been ready for 5 s.
	CL_SERVE_ARGS="--min-ready 5s"
	cl_start_server
	cl_check_server_listeners
	cl_join
	W1_ID=${CL_IDS[1]}
	W2_ID=${CL_IDS[2]}
	cl_note "server $CL_S_IP, advertising ${CL_ADVERTISE:-$CL_S_IP}; $W1 $(cl_ip_of "$W1") joined $(docker exec "$W1" sed -n 's/.*"server": *"\([^"]*\)".*/\1/p' /var/lib/hivemind/node.json)"
	# Placement spreads replicas only over nodes past --min-ready.
	wait_for 60 workers_ready || cl_fail "the workers are not ready: $(docker exec "$S" hivemind nodes | tr '\n' ';')"
	sleep 7
	docker exec "$S" hivemind deploy --name spread --image nginx:alpine --replicas 3 >/dev/null || cl_fail "deploy spread"
	docker exec "$S" hivemind deploy --name web --image nginx:alpine --replicas 3 --port 8080:80 >/dev/null || cl_fail "deploy web"
	wait_for 180 apps_converged || cl_fail "the apps did not start on three nodes: spread $(app_rows spread | tr '\n' ';') web $(app_rows web | tr '\n' ';')"
	# Per worker, by node index: node.json, node.crt and the containers.
	SUM_STATE=()
	SUM_CERT=()
	CONTAINERS=()
	for i in 1 2; do
		c=${CL_NAMES[$i]}
		SUM_STATE[$i]=$(file_sum "$c" /var/lib/hivemind/node.json)
		SUM_CERT[$i]=$(file_sum "$c" /var/lib/hivemind/node.crt)
		CONTAINERS[$i]=$(worker_containers "$c")
	done
	cl_note "spread and web run on three nodes"
}

# lose_server: back the server up, then destroy its container, data
# directory and Docker volume, and start an empty node at NEW_IP with the
# docker-run options given.
lose_server() {
	docker exec "$S" hivemind backup /root/hm.tar >/dev/null || cl_fail "hivemind backup"
	docker cp "$S:/root/hm.tar" "$CL_WORK/hm.tar"
	docker rm -fv "$S" >/dev/null
	docker volume rm "$S-docker" >/dev/null
	LOST_AT=$(date +%s)
	OLD_IP=$CL_S_IP
	NEW_IP=${OLD_IP%.*}.200
	[ "$NEW_IP" != "$OLD_IP" ] || NEW_IP=${OLD_IP%.*}.201
	cl_note "backed up and destroyed $S at $OLD_IP"
	cl_run_node "$S" --ip "$NEW_IP" "$@"
	cl_ready_node "$S"
	[ "$(cl_ip_of "$S")" = "$NEW_IP" ] || cl_fail "the new server node has address $(cl_ip_of "$S"), not $NEW_IP"
	docker cp "$CL_WORK/hm.tar" "$S:/root/hm.tar"
	CL_S_IP=$NEW_IP
}

# restore_and_serve: restore the backup on the new node and start serve. The
# outage lasts at least 40 s, as a real restore does: longer than the
# worker's 30 s request timeout, which a request sent to the old server just
# before the loss waits out, since a lost machine sends no reset.
restore_and_serve() {
	local out left
	left=$((LOST_AT + 40 - $(date +%s)))
	[ "$left" -le 0 ] || sleep "$left"
	out=$(docker exec "$S" hivemind restore /root/hm.tar --data-dir /var/lib/hivemind 2>&1) || cl_fail "hivemind restore: $out"
	cl_serve_server
	wait_for 30 docker exec "$S" hivemind status || cl_fail "the restored server's API did not answer"
	SERVED_AT=$(date +%s)
	cl_check_server_listeners
	cl_note "restored on a new $S at $NEW_IP, serving $((SERVED_AT - LOST_AT)) s after the loss; listeners on $NEW_IP only"
}

# check_back: the workers are ready with their node IDs and certificates,
# the apps converge with no replica twice and no node lost, and the
# workers' containers are the ones that ran before.
check_back() {
	local c i
	wait_for 120 workers_ready || cl_fail "the workers did not reconnect: $(docker exec "$S" hivemind nodes | tr '\n' ';')"
	cl_note "both workers ready, $(($(date +%s) - SERVED_AT)) s after serve started"
	for i in 1 2; do
		c=${CL_NAMES[$i]}
		[ "$(cl_node_id_of "$c")" = "${CL_IDS[$i]}" ] || cl_fail "$c has node ID $(cl_node_id_of "$c"), not ${CL_IDS[$i]}"
		[ "$(file_sum "$c" /var/lib/hivemind/node.crt)" = "${SUM_CERT[$i]}" ] || cl_fail "$c has another certificate"
		[ "$(docker exec "$c" grep -c 'joined the cluster' /var/log/hivemind.log)" = 1 ] || cl_fail "$c joined again"
	done
	wait_for 150 apps_converged || cl_fail "the apps did not converge: spread $(app_rows spread | tr '\n' ';') web $(app_rows web | tr '\n' ';')"
	wait_for 60 no_duplicates || cl_fail "a replica runs twice: $( (containers_of spread; containers_of web) | sort | tr '\n' ';')"
	if docker exec "$S" grep "node lost" /var/log/hivemind.log; then
		cl_fail "the restored server released a node whose worker came back"
	fi
	for i in 1 2; do
		c=${CL_NAMES[$i]}
		[ "$(worker_containers "$c")" = "${CONTAINERS[$i]}" ] ||
			cl_fail "the containers on $c changed: ${CONTAINERS[$i]}before, $(worker_containers "$c")now"
	done
	[ "$(server_address)" = "$NEW_IP" ] || cl_fail "hivemind nodes gives the server address $(server_address), not $NEW_IP"
	cl_note "apps converged, no replica twice, no node lost, the workers' containers kept; hivemind nodes: server at $NEW_IP"
}

# Part 1: a server that advertises a DNS name.
by_name() {
	local pids i
	cl_init newaddr-name
	CL_NET_SUBNET=auto
	CL_ADVERTISE=$ALIAS
	CL_JOIN_ADDR=$ALIAS
	start_cluster
	pids="$(docker exec "$W1" pgrep -x hivemind) $(docker exec "$W2" pgrep -x hivemind)"

	lose_server
	CL_SERVE_ARGS="--min-ready 5s --cluster-addr $NEW_IP"
	restore_and_serve
	docker exec "$S" grep -q -- '--advertise does not resolve' /var/log/hivemind.log ||
		cl_fail "serve did not warn that $ALIAS does not resolve: $(docker exec "$S" tail -n 5 /var/log/hivemind.log)"

	# The record is updated: the alias moves to the new server.
	docker network disconnect "$CL_NET" "$S"
	docker network connect --ip "$NEW_IP" --alias "$ALIAS" "$CL_NET" "$S"
	[ "$(cl_ip_of "$S")" = "$NEW_IP" ] || cl_fail "the new server has address $(cl_ip_of "$S") after the alias moved, not $NEW_IP"
	docker exec "$S" pgrep -x hivemind >/dev/null || cl_fail "serve stopped when the alias moved"
	cl_note "moved the alias $ALIAS to $NEW_IP"
	cl_check_server_listeners

	check_back
	[ "$(docker exec "$W1" pgrep -x hivemind) $(docker exec "$W2" pgrep -x hivemind)" = "$pids" ] || cl_fail "a worker process was restarted"
	for i in 1 2; do
		[ "$(file_sum "${CL_NAMES[$i]}" /var/lib/hivemind/node.json)" = "${SUM_STATE[$i]}" ] || cl_fail "node.json on ${CL_NAMES[$i]} changed"
	done
	cl_note "by name: the workers were never restarted, and node.json is unchanged on both"
}

# set_server NODE ADDRESS: run node set-server on NODE, and print its output.
set_server() { docker exec "$1" hivemind node set-server "$2" 2>&1; }

# refused INDEX ADDRESS TEXT: set-server to ADDRESS on the worker at node
# index INDEX fails, says TEXT, and leaves node.json as it was.
refused() {
	local out c=${CL_NAMES[$1]}
	if out=$(set_server "$c" "$2"); then
		cl_fail "set-server $2 on $c succeeded: $out"
	fi
	grep -qF -- "$3" <<<"$out" || cl_fail "set-server $2 on $c did not say \"$3\": $out"
	[ "$(file_sum "$c" /var/lib/hivemind/node.json)" = "${SUM_STATE[$1]}" ] || cl_fail "a refused set-server $2 changed node.json on $c"
	cl_note "refused: set-server $2: ${out#error: }"
}

# move_worker INDEX: stop the worker at node index INDEX, move it to
# NEW_IP, and start it again.
move_worker() {
	local out c=${CL_NAMES[$1]}
	docker exec "$c" pkill -TERM -x hivemind || true
	wait_for 30 worker_stopped "$c" || cl_fail "the worker on $c did not stop"
	out=$(set_server "$c" "$NEW_IP") || cl_fail "set-server $NEW_IP on $c: $out"
	docker exec "$c" grep -q "\"server\": \"$NEW_IP:4484\"" /var/lib/hivemind/node.json || cl_fail "node.json on $c does not name $NEW_IP:4484"
	docker exec -d "$c" sh -c "exec hivemind serve --role worker >>/var/log/hivemind.log 2>&1"
	cl_note "moved $c: $(head -n 1 <<<"$out")"
}

# Part 2: a server that advertises its IP address.
by_ip() {
	cl_init newaddr-ip
	CL_NET_SUBNET=auto
	unset CL_ADVERTISE CL_JOIN_ADDR
	start_cluster

	# The new machine has an alias that its certificate will not name.
	lose_server --network-alias hm-other-name

	# Another cluster's server at the new address, as a machine that took
	# the address, or one in the middle, would be. Its client config is its
	# own, so the CLI on the node still finds the restored server's later.
	docker exec "$S" hivemind init --data-dir /root/other --client-config /root/other-client.yaml >/dev/null
	docker exec -d "$S" sh -c "exec hivemind serve --data-dir /root/other --advertise $NEW_IP --api-addr 127.0.0.1:4493 >>/var/log/other.log 2>&1"
	other_up() { cl_listeners "$S" | grep -qx "$(cl_hexip "$NEW_IP"):1184"; }
	wait_for 30 other_up || cl_fail "the other cluster's server did not listen on $NEW_IP:4484"
	docker exec "$W1" pkill -TERM -x hivemind || true
	wait_for 30 worker_stopped "$W1" || cl_fail "the worker on $W1 did not stop"
	refused 1 "$NEW_IP" "not from this worker's cluster CA"
	refused 1 "$NEW_IP:4490" "cannot reach a server at $NEW_IP:4490"
	docker exec "$S" pkill -TERM -f 'hivemind serve --data-dir /root/other'
	other_down() { ! other_up; }
	wait_for 30 other_down || cl_fail "the other cluster's server did not stop"

	CL_SERVE_ARGS="--min-ready 5s"
	restore_and_serve
	docker exec "$S" grep -q "hivemind node set-server $NEW_IP" /var/log/hivemind.log ||
		cl_fail "serve did not say that the workers need set-server: $(docker exec "$S" tail -n 5 /var/log/hivemind.log)"
	refused 1 hm-other-name "has a certificate from this cluster's CA, but it names $NEW_IP, not hm-other-name"
	refused 2 "$NEW_IP" "stop it (systemctl stop hivemind), run set-server, then start it again"
	move_worker 1
	move_worker 2
	cl_note "both workers moved, $(($(date +%s) - SERVED_AT)) s after serve started"
	check_back
}

(by_name)
(by_ip)
cl_note "newaddr: all checks passed"
