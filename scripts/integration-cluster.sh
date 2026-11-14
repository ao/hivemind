#!/usr/bin/env bash
# Runs a three-node hivemind cluster in Docker and checks what the cluster
# does with replicas, host ports and a lost node.
#
# Three docker:dind containers share one private user-defined network, and none
# of them publishes a port. The server runs in node s with --advertise set to
# its address on that network. Workers w1 and w2 join it with join tokens. The
# hivemind binary is built in a golang container from this checkout, and copied
# into each node. The helpers are in scripts/lib/cluster.sh, shared with
# scripts/fault-suite.sh.
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
#   - node removal: removing w1 while it runs is refused without --force.
#     With --force, its spread replica moves to another node, its web replica
#     waits on the host port, hivemind nodes no longer lists it, its worker
#     exits with the removal message, the server logs the refused request,
#     and starting the worker again fails at once
#
# It publishes no ports. On exit it removes the containers, the network and the
# volumes it created, also after a failure or Ctrl-C. The Go build cache volume
# (hivemind-cache) is kept.
#
# Needs Docker on the host, and the docker:dind and golang images. Run it from
# anywhere in the repository.
set -euo pipefail

REPO=$(cd "$(dirname "$0")/.." && pwd)
# shellcheck source=lib/cluster.sh
. "$REPO/scripts/lib/cluster.sh"

cl_init integration
cl_add_node
cl_add_node
cl_add_node
cl_trap_cleanup
S=${CL_NAMES[0]}
W1=${CL_NAMES[1]}
W2=${CL_NAMES[2]}

cl_start_nodes
cl_start_server
cl_note "addresses: server $CL_S_IP, $W1 $(cl_ip_of "$W1"), $W2 $(cl_ip_of "$W2")"
cl_check_server_listeners
cl_note "server listeners: API on 127.0.0.1:4483; cluster and join ports on $CL_S_IP only"
cl_join
W1_ID=${CL_IDS[1]}
W2_ID=${CL_IDS[2]}
cl_check_worker_listeners
cl_note "workers listen on no hivemind port"

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
	for c in "${CL_NAMES[@]}"; do
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

# Task 9: four replicas on host port 8080.
docker exec "$S" hivemind deploy --name web --image nginx:alpine --replicas 4 --port 8080:80 >/dev/null ||
	cl_fail "deploy web"
# A worker takes replicas only after it has been ready for --min-ready (30 s).
# The wait is long enough for both workers to pass it.
cl_note "waiting 40 s, the readiness wait for the workers"
sleep 40
wait_for 150 web_ok || cl_fail "web did not settle to three running replicas on three nodes and one pending on the host port: $(app_rows web | tr '\n' ';')"
cl_note "web: three replicas run on three nodes; one is pending on host port 8080"

for id in "$CL_S_ID" "$W1_ID" "$W2_ID"; do
	if [ "$(running_on web "$id")" -eq 1 ]; then
		c=$S
		[ "$id" = "$W1_ID" ] && c=$W1
		[ "$id" = "$W2_ID" ] && c=$W2
		wait_for 30 docker exec "$c" sh -c 'wget -q -O - http://127.0.0.1:8080/ | grep -q nginx' ||
			cl_fail "the web replica on $id does not answer on 127.0.0.1:8080"
	fi
done
cl_note "each node answers on 127.0.0.1:8080 for its web replica"

# Task 10: spread has three replicas without ports, one per node.
docker exec "$S" hivemind deploy --name spread --image nginx:alpine --replicas 3 >/dev/null || cl_fail "deploy spread"
wait_for 90 spread_on_three || cl_fail "spread did not run on three nodes: $(app_rows spread | tr '\n' ';')"

docker kill "$W2" >/dev/null
killed_at=$(date +%s)
cl_note "killed $W2 ($W2_ID) at $(date -u +%T)"
wait_for 30 spread_off "$W2_ID" ||
	cl_fail "spread replicas did not move off $W2_ID within 30 s: $(app_rows spread | tr '\n' ';')"
cl_note "spread moved off $W2_ID within $(($(date +%s) - killed_at)) s"

# web's replica 3 was already pending. The replica that ran on the killed node
# is the second pending one, and its reason is the host-port conflict.
rows=$(app_rows web)
inuse=$(awk '$3 == "pending" && /in use/' <<<"$rows" | wc -l)
[ "$inuse" -eq 2 ] || cl_fail "web does not have two replicas pending on the host port: $(tr '\n' ';' <<<"$rows")"
cl_note "web: two replicas pending on host port 8080 (index, node, state, reason): $(awk '$3 == "pending"' <<<"$rows" | tr '\n' ';')"

# Worker back, with its data directory.
docker start "$W2" >/dev/null
wait_for 90 docker exec "$W2" docker info || cl_fail "the Docker daemon in $W2 did not start again"
docker exec -d "$W2" sh -c 'exec hivemind serve --role worker >>/var/log/hivemind.log 2>&1'
cl_note "restarted $W2 with its data directory"

# The worker is ready again after 30 s, and web's replica goes back to it.
wait_for 150 web_ok || cl_fail "web did not come back to three running replicas: $(app_rows web | tr '\n' ';')"
wait_for 60 no_duplicates || cl_fail "a replica is duplicated across nodes: $( (containers_of web; containers_of spread) | sort | tr '\n' ';')"
[ "$(count_running spread)" -eq 3 ] || cl_fail "spread is not at three running replicas after the worker returned"
cl_note "after the return: web and spread at their replica counts, no duplicate replicas"

# The listeners are unchanged after the restart.
cl_check_server_listeners
cl_check_worker_listeners
cl_note "listeners unchanged after the restart"

# worker_stopped NODE: no hivemind process runs in NODE.
worker_stopped() { ! docker exec "$1" pgrep -x hivemind >/dev/null 2>&1; }

# Node removal. w1 still renews its lease, so a plain remove is refused.
if out=$(docker exec "$S" hivemind node remove "$W1_ID" 2>&1); then
	cl_fail "removing a running worker without --force succeeded: $out"
fi
grep -q -- "--force" <<<"$out" || cl_fail "the refusal does not mention --force: $out"
cl_note "node remove without --force refused: $out"
out=$(docker exec "$S" hivemind node remove "$W1_ID" --force 2>&1) || cl_fail "node remove --force $W1_ID: $out"
removed_at=$(date +%s)
cl_note "removed $W1 ($W1_ID) at $(date -u +%T): $out"

wait_for 30 spread_off "$W1_ID" ||
	cl_fail "spread replicas did not move off the removed $W1_ID within 30 s: $(app_rows spread | tr '\n' ';')"
cl_note "spread moved off $W1_ID within $(($(date +%s) - removed_at)) s"
rows=$(app_rows web)
[ "$(awk '$3 == "running"' <<<"$rows" | wc -l)" -eq 2 ] && [ "$(awk '$3 == "pending" && /in use/' <<<"$rows" | wc -l)" -eq 2 ] ||
	cl_fail "web is not at two running and two pending on the host port after the removal: $(tr '\n' ';' <<<"$rows")"
if docker exec "$S" hivemind nodes | grep -q "$W1_ID"; then
	cl_fail "hivemind nodes still lists $W1_ID: $(docker exec "$S" hivemind nodes | tr '\n' ';')"
fi

# The removed worker gets 403 on its next call, and exits.
wait_for 30 worker_stopped "$W1" || cl_fail "the worker on $W1 still runs 30 s after its removal"
docker exec "$W1" grep -q "was removed from the cluster" /var/log/hivemind.log ||
	cl_fail "the worker on $W1 did not log its removal"
docker exec "$S" grep -q "request from a removed node refused" /var/log/hivemind.log ||
	cl_fail "the server did not log a refused request from $W1_ID"
if out=$(docker exec "$W1" hivemind serve --role worker 2>&1); then
	cl_fail "the removed worker started again: $out"
fi
grep -q "was removed from the cluster" <<<"$out" || cl_fail "a restart of the removed worker failed for another reason: $out"
cl_note "the removed worker exited, and a restart is refused: $(tail -n 1 <<<"$out")"

# Its containers are left running, as documented; remove them by hand, then
# no replica is duplicated and spread is at its replica count.
[ -n "$(docker exec "$W1" docker ps -q --filter label=hivemind.app)" ] || cl_fail "the removed worker stopped its containers"
docker exec "$W1" sh -c 'docker rm -f $(docker ps -aq --filter label=hivemind.app)' >/dev/null
wait_for 60 no_duplicates || cl_fail "a replica is duplicated after the removal: $( (containers_of web; containers_of spread) | sort | tr '\n' ';')"
[ "$(count_running spread)" -eq 3 ] || cl_fail "spread is not at three running replicas after the removal"
cl_note "after the removal: spread at three running replicas, no duplicates"

cl_note "all cluster checks passed"
