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
#   - logs: hivemind logs on the server reads a spread replica on w1, with
#     --tail and in full, stdout and stderr. With -f it shows a line written
#     after it started, exits 0 on SIGINT, and the worker logs that its stream
#     ended
#   - Task 10: a worker is killed. Its replica of an app without ports runs on
#     another node within 30 s. Its replica of the app with port 8080 stays
#     pending with the host-port reason
#   - Task 10: the worker starts again with its data directory. No replica of
#     any app is duplicated, and each app is back at its replica count
#   - certificate renewal: the binary is built with -tags hivemind_testknobs,
#     and the server runs with HIVEMIND_TEST_CERT_LIFETIME=10m, so node
#     certificates fall due after about 6m40s. Each worker renews over the
#     node API and keeps working without a restart: node.crt changes, the
#     key and certificate stay mode 0600, the server logs each renewal and
#     renews its own certificate, and the apps stay at their counts. After
#     the first certificates have expired, both workers are still ready, no
#     certificate was refused, and hivemind logs still reads w1
#   - disaster recovery: hivemind backup on the server, from the data
#     directory and through the API. The server container, its data
#     directory and its Docker volume are destroyed. A new server node gets
#     the same IP, hivemind restore puts the backup in an empty data
#     directory, and serve starts. Both workers, which were never restarted,
#     reconnect with their certificates, and the apps converge with no
#     duplicate replica
#   - a slow pass: nine replicas of busybox sleep, which ignores SIGTERM, are
#     deleted; each agent takes about 30 s to stop its three, and no node is
#     lost meanwhile, because leases are renewed apart from the passes
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
# The test-only build tag, and a short certificate lifetime, so a renewal
# happens during the run. The network gets its own subnet, so the server can
# be replaced by a new container at the same address.
CL_GO_TAGS=hivemind_testknobs
CL_SERVER_ENV=HIVEMIND_TEST_CERT_LIFETIME=10m
CL_NET_SUBNET=auto
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
# The first node certificates were issued by now, and last 10 minutes.
JOINED_AT=$(date +%s)
W1_ID=${CL_IDS[1]}
W2_ID=${CL_IDS[2]}
cl_check_worker_listeners
cl_note "workers listen on no hivemind port"

# cert_sum NODE: the SHA-256 of the worker's node.crt.
cert_sum() { docker exec "$1" sha256sum /var/lib/hivemind/node.crt | awk '{ print $1 }'; }
W1_CERT0=$(cert_sum "$W1")
W2_CERT0=$(cert_sum "$W2")

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

# Logs of a replica on a worker, read with hivemind logs on the server. The
# marker requests go to nginx inside the replica's container. Each is a 404,
# which nginx logs twice: the error on stderr and the access line on stdout.
LOG_IDX=$(app_rows spread | awk -v n="$W1_ID" '$2 == n && $3 == "running" { print $1; exit }')
[ -n "$LOG_IDX" ] || cl_fail "no spread replica runs on $W1_ID: $(app_rows spread | tr '\n' ';')"
LOG_CTR=$(docker exec "$W1" docker ps -q --filter label=hivemind.app=spread --filter "label=hivemind.replica=$LOG_IDX")
[ -n "$LOG_CTR" ] || cl_fail "no container for spread replica $LOG_IDX in $W1"
mark() { docker exec "$W1" docker exec "$LOG_CTR" wget -q -O /dev/null "http://127.0.0.1/$1" || true; }
streams_ended() { docker exec "$W1" grep -c 'log stream ended' /var/log/hivemind.log || true; }
for i in 1 2 3 4 5 6; do mark "marker-$i"; done
out=$(docker exec "$S" hivemind logs spread "$LOG_IDX" --tail 3 2>&1) || cl_fail "hivemind logs of a replica on $W1_ID: $out"
[ "$(wc -l <<<"$out")" -eq 3 ] || cl_fail "--tail 3 printed $(wc -l <<<"$out") lines: $out"
grep -q "GET /marker-6 " <<<"$out" || cl_fail "the tail of the log on $W1_ID has no marker-6: $out"
grep -q "GET /marker-3 " <<<"$out" && cl_fail "--tail 3 printed marker-3, which is four lines from the end: $out"
out=$(docker exec "$S" hivemind logs spread "$LOG_IDX" 2>&1) || cl_fail "hivemind logs without --tail: $out"
for i in 1 2 3 4 5 6; do grep -q "GET /marker-$i " <<<"$out" || cl_fail "the full log has no marker-$i: $out"; done
grep -q 'marker-6" failed' <<<"$out" || cl_fail "the full log has no stderr line for marker-6: $out"
cl_note "hivemind logs on the server read spread replica $LOG_IDX on $W1_ID, with --tail 3 and in full, stdout and stderr"

# Follow: a new line arrives through the stream, and SIGINT ends the command,
# and the stream on the worker, which logs one line once its log read ends.
before=$(streams_ended)
docker exec -d "$S" sh -c "hivemind logs spread $LOG_IDX -f --tail 1 >/tmp/follow.out 2>&1; echo \"exit \$?\" >>/tmp/follow.out"
wait_for 20 docker exec "$S" grep -q "GET /marker-6 " /tmp/follow.out || cl_fail "the followed log did not start: $(docker exec "$S" cat /tmp/follow.out)"
mark marker-follow
wait_for 20 docker exec "$S" grep -q "GET /marker-follow " /tmp/follow.out ||
	cl_fail "the followed log did not show a new line: $(docker exec "$S" cat /tmp/follow.out)"
[ "$(streams_ended)" -eq "$before" ] || cl_fail "the worker ended the followed stream before the user did"
docker exec "$S" pkill -INT -f '^hivemind logs' || cl_fail "no hivemind logs process to stop"
wait_for 10 docker exec "$S" grep -qx "exit 0" /tmp/follow.out ||
	cl_fail "hivemind logs -f did not exit 0 on SIGINT: $(docker exec "$S" cat /tmp/follow.out)"
stream_ended_since() { [ "$(streams_ended)" -gt "$before" ]; }
wait_for 10 stream_ended_since ||
	cl_fail "the worker's log stream did not end after SIGINT: $(docker exec "$W1" tail -n 5 /var/log/hivemind.log)"
cl_note "hivemind logs -f followed a new line from $W1_ID, exited 0 on SIGINT, and the worker ended its stream"

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

# Certificate renewal. Certificates last 10 minutes here, so each worker
# renews about 6m40s after it joined, and keeps running.
renewed() { # renewed NODE CERT0: the worker logged a renewal and its node.crt changed
	docker exec "$1" grep -q "node certificate renewed" /var/log/hivemind.log && [ "$(cert_sum "$1")" != "$2" ]
}
wait_for 480 renewed "$W1" "$W1_CERT0" || cl_fail "$W1 did not renew its certificate"
wait_for 120 renewed "$W2" "$W2_CERT0" || cl_fail "$W2 did not renew its certificate"
for c in "$W1" "$W2"; do
	modes=$(docker exec "$c" stat -c '%a %n' /var/lib/hivemind/node.key /var/lib/hivemind/node.crt)
	[ "$(awk '$1 != "600"' <<<"$modes")" = "" ] || cl_fail "$c: renewed files are not mode 0600: $modes"
	if docker exec "$c" sh -c 'ls /var/lib/hivemind/*.next' >/dev/null 2>&1; then
		cl_fail "$c left staging files after a renewal"
	fi
done
for id in "$W1_ID" "$W2_ID"; do
	docker exec "$S" grep "node certificate renewed" /var/log/hivemind.log | grep -q "node=$id" ||
		cl_fail "the server did not log a renewal for $id"
done
docker exec "$S" grep -q "server certificate renewed" /var/log/hivemind.log || cl_fail "the server did not renew its own certificate"
# Both workers still run the process they started with, and still report.
for c in "$W1" "$W2"; do
	docker exec "$c" pgrep -x hivemind >/dev/null || cl_fail "the worker on $c stopped"
done
wait_for 60 web_ok || cl_fail "web is not at its counts after the renewals: $(app_rows web | tr '\n' ';')"
spread_three() { [ "$(count_running spread)" -eq 3 ]; }
wait_for 60 spread_three || cl_fail "spread is not at three running replicas after the renewals: $(app_rows spread | tr '\n' ';')"
nodes_out=$(docker exec "$S" hivemind nodes)
[ "$(grep -c ' ready .* (in ' <<<"$nodes_out")" -eq 3 ] || cl_fail "hivemind nodes does not show three ready nodes with a certificate expiry: $nodes_out"
cl_note "both workers renewed their certificates and kept working; $(docker exec "$W1" grep 'node certificate renewed' /var/log/hivemind.log | tail -n 1)"

# Past the end of the first certificates. A connection a worker opened before
# its renewal still presents the old certificate, and the server refuses that
# from now on. w1 has had its log poll open since it started, so this is where
# a connection kept from before the renewal would show: its lease would lapse,
# or its logs stop answering.
left=$((JOINED_AT + 600 + 20 - $(date +%s)))
if [ "$left" -gt 0 ]; then
	cl_note "waiting $left s, until the first certificates have expired"
	sleep "$left"
fi
both_ready() {
	[ "$(docker exec "$S" hivemind nodes | awk -v a="$W1_ID" -v b="$W2_ID" '($1 == a || $1 == b) && / ready /' | wc -l)" -eq 2 ]
}
# A lease lapses after 15 s, so a worker that lost the server is lost by then.
sleep 20
both_ready || cl_fail "a worker is not ready after its first certificate expired: $(docker exec "$S" hivemind nodes | tr '\n' ';')"
for c in "$W1" "$W2"; do
	docker exec "$c" pgrep -x hivemind >/dev/null || cl_fail "the worker on $c stopped after its first certificate expired"
	if docker exec "$c" grep -q 'expired or not yet valid' /var/log/hivemind.log; then
		cl_fail "the server refused a certificate of $c: $(docker exec "$c" grep 'expired or not yet valid' /var/log/hivemind.log | tail -n 1)"
	fi
done
mark marker-after-expiry
out=$(docker exec "$S" hivemind logs spread "$LOG_IDX" --tail 2 2>&1) || cl_fail "hivemind logs of $W1_ID after its first certificate expired: $out"
grep -q "GET /marker-after-expiry " <<<"$out" || cl_fail "the log from $W1_ID after the expiry has no new marker: $out"
cl_note "after the first certificates expired, both workers are ready and hivemind logs reads $W1_ID"

# Disaster recovery. Back up the server while it runs, from the data
# directory and through the API, and check both archives.
docker exec "$S" hivemind backup /root/hm.tar >/dev/null || cl_fail "hivemind backup"
docker exec "$S" hivemind backup /root/hm-api.tar --api >/dev/null || cl_fail "hivemind backup --api"
[ "$(docker exec "$S" stat -c %a /root/hm.tar)" = 600 ] || cl_fail "the backup file is not mode 0600"
docker cp "$S:/root/hm.tar" "$CL_WORK/hm.tar"
web_before=$(app_rows web | awk '{ print $1, $2, $3 }' | sort | tr '\n' ';')
cl_note "backed up the server; web was: $web_before"

# The server machine is lost: its container, its data directory and its
# Docker volume. Its containers go with it.
docker rm -fv "$S" >/dev/null
docker volume rm "$S-docker" >/dev/null
lost_at=$(date +%s)
cl_note "destroyed $S, its data directory and its Docker volume"

# A new machine at the same address, with the backup restored into an empty
# data directory.
cl_run_node "$S" --ip "$CL_S_IP"
cl_ready_node "$S"
[ "$(cl_ip_of "$S")" = "$CL_S_IP" ] || cl_fail "the new server node has address $(cl_ip_of "$S"), not $CL_S_IP"
docker cp "$CL_WORK/hm.tar" "$S:/root/hm.tar"
out=$(docker exec "$S" hivemind restore /root/hm.tar --data-dir /var/lib/hivemind 2>&1) || cl_fail "hivemind restore: $out"
[ "$(docker exec "$S" stat -c %a /var/lib/hivemind)" = 700 ] || cl_fail "the restored data directory is not mode 0700"
modes=$(docker exec "$S" sh -c 'stat -c "%a %n" /var/lib/hivemind/*')
[ "$(awk '$1 != "600"' <<<"$modes")" = "" ] || cl_fail "restored files are not mode 0600: $modes"
if out=$(docker exec "$S" hivemind restore /root/hm.tar --data-dir /var/lib/hivemind 2>&1); then
	cl_fail "a restore into a non-empty data directory succeeded"
fi
cl_serve_server
wait_for 30 docker exec "$S" hivemind status || cl_fail "the restored server's API did not answer"
cl_note "restored the backup on a new $S at $CL_S_IP, $(($(date +%s) - lost_at)) s after the loss"

# The workers never restarted. They reconnect with their own certificates,
# the server node starts its replicas again, and nothing is duplicated.
workers_ready() {
	local n
	n=$(docker exec "$S" hivemind nodes | awk -v a="$W1_ID" -v b="$W2_ID" '($1 == a || $1 == b) && / ready /' | wc -l)
	[ "$n" -eq 2 ]
}
wait_for 60 workers_ready || cl_fail "the workers did not reconnect to the restored server: $(docker exec "$S" hivemind nodes | tr '\n' ';')"
for c in "$W1" "$W2"; do
	docker exec "$c" pgrep -x hivemind >/dev/null || cl_fail "the worker on $c stopped while the server was down"
done
cl_note "both workers reconnected to the restored server without joining again"
# Their leases in the backup are as old as the outage. The restored server
# counts each lease from its own start, so it releases neither worker.
if docker exec "$S" grep "node lost" /var/log/hivemind.log; then
	cl_fail "the restored server released a node whose worker was running"
fi
wait_for 150 web_ok || cl_fail "web did not converge on the restored server: $(app_rows web | tr '\n' ';')"
wait_for 90 spread_three || cl_fail "spread did not converge on the restored server: $(app_rows spread | tr '\n' ';')"
wait_for 60 no_duplicates || cl_fail "a replica is duplicated after the restore: $( (containers_of web; containers_of spread) | sort | tr '\n' ';')"
cl_check_server_listeners
cl_note "after the restore: web $(app_rows web | awk '{ print $1, $2, $3 }' | sort | tr '\n' ';') spread at three, no duplicates"

# A slow pass. busybox sleep as PID 1 ignores SIGTERM, so Docker stops each
# container only when its 10 s grace ends, and a pass stops one container at
# a time: with three per node, a pass takes about 30 s. The agents renew
# their leases apart from their passes, so no node may be lost meanwhile.
docker exec -i "$S" hivemind deploy -f - >/dev/null <<'EOF' || cl_fail "deploy slowstop"
name: slowstop
image: busybox:1.36
replicas: 9
command: ["sleep", "3600"]
EOF
slowstop_up() { [ "$(count_running slowstop)" -eq 9 ]; }
wait_for 120 slowstop_up || cl_fail "slowstop did not reach nine running replicas: $(app_rows slowstop | tr '\n' ';')"
per_node=$(app_rows slowstop | awk '{ print $2 }' | sort | uniq -c | tr '\n' ';')
docker exec "$S" hivemind delete slowstop >/dev/null || cl_fail "delete slowstop"
deleted_at=$(date +%s)
# The API hides a deleted app at once; its containers go one by one.
slowstop_gone() { [ -z "$(containers_of slowstop)" ]; }
while ! slowstop_gone; do
	if docker exec "$S" hivemind nodes | grep -q ' lost '; then
		cl_fail "a node was lost while its agent stopped slowstop's containers, $(($(date +%s) - deleted_at)) s after the delete: $(docker exec "$S" hivemind nodes | tr '\n' ';')"
	fi
	[ $(($(date +%s) - deleted_at)) -lt 120 ] || cl_fail "slowstop was not removed in 120 s"
	sleep 1
done
took=$(($(date +%s) - deleted_at))
[ "$took" -gt 15 ] || cl_fail "slowstop was removed in $took s, under the 15 s lease, so the check proves nothing"
cl_note "slowstop ($per_node) was removed in $took s, and no node was lost meanwhile"

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
