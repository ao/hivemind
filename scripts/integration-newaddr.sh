#!/usr/bin/env bash
# Restores a hivemind server on a machine with a new IP address, in Docker,
# twice: once for a cluster whose server advertises a DNS name, once for one
# that advertises an IP address. The workers must come back without joining
# again.
#
# Each part runs three docker:dind containers, a server and two workers, on
# a private network of its own with a subnet of its own, and none of them
# publishes a port. The hivemind binary is built in a golang container from
# this checkout. The helpers are in scripts/lib/cluster.sh, and those for
# the loss, the restore and the old server's return in scripts/lib/restore.sh,
# which scripts/fault-suite.sh shares. Two apps run in
# each part: spread, three replicas without ports, and web, three replicas
# on host port 8080, one per node.
#
# By name:
#   - the server's container has the network alias hm-server, and serve runs
#     with --advertise hm-server. Its cluster listeners bind the address the
#     name resolves to, and nothing else. The workers join hm-server.
#   - the server is backed up, and its container is killed; the container,
#     its data directory and its Docker volume are kept for later. A new
#     container at another IP, without the alias, restores the backup (at
#     epoch 1) and serves with --advertise hm-server --cluster-addr <its
#     IP>, as before a DNS record is updated: its listeners bind that IP,
#     and it warns that the name does not resolve.
#   - every node runs a router, against Pebble (a test ACME CA) and its DNS,
#     pebble-challtestsrv, on the same network, as in
#     integration-routing.sh; a curl container plays the client. A third
#     app, who, has a route and two replicas, both on the workers: the
#     server's node is ineligible while it is deployed.
#   - the alias moves to the new container (disconnected from the network
#     and connected again, at the same IP, with the alias), as the record
#     would be updated, 30 s after the restored server started: later than
#     the 15 s lease timeout, as when the workers' resolvers keep the old
#     record a while. Until then the workers cannot reach it. 20 s after
#     the start both are "waiting" in hivemind nodes, not lost. The workers,
#     never restarted, reconnect: the same processes, node.json and
#     node.crt unchanged, both ready, the apps back at their counts, no
#     node lost, no replica twice, and the workers' containers the ones
#     that ran before.
#   - requests through a worker's router, from before the loss until the
#     workers are back, all get 200. Requests through the new server's
#     router, from before it starts until the alias moves, get no 503: its
#     routing table has the waiting workers' replicas from the start.
#   - both workers have seen epoch 1. Then the restored server stops, the
#     old container starts again at its old IP with the alias, as a
#     recovered machine and a stale record would, and serve runs there on
#     the old store. Both workers refuse it and log that once, the old
#     server logs that a newer copy of the cluster exists, and for 20 s
#     nothing moves: the workers' containers are the same, and the old
#     server has no report from them. Then the alias leaves the old
#     container, whose serve keeps running, and the restored server starts
#     again under it: the workers, never restarted, report to it within
#     30 s, while the old server still runs, with no second refusal and no
#     "talks to it again" line, as they never refused the restored
#     server's address. Then the old server stops. A worker's router serves
#     every request throughout.
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
#   - the old container starts again at its old IP, and serve runs there
#     on the old store. set-server to it, on a stopped worker that has seen
#     epoch 1, is refused as from before a restore, and node.json is
#     unchanged; the worker starts again on the restored server.
#
# It publishes no ports. On exit it removes the containers, the networks and
# the volumes it created, the old server's included, also after a failure or
# Ctrl-C. The Go build cache volume (hivemind-cache) is kept.
#
# Needs Docker on the host, and the docker:dind, golang, Pebble,
# pebble-challtestsrv, curl and traefik/whoami images. Run it from anywhere
# in the repository.
set -euo pipefail

REPO=$(cd "$(dirname "$0")/.." && pwd)
# shellcheck source=lib/cluster.sh
. "$REPO/scripts/lib/cluster.sh"
# shellcheck source=lib/restore.sh
. "$REPO/scripts/lib/restore.sh"

ALIAS=hm-server

PEBBLE_IMAGE=${PEBBLE_IMAGE:-ghcr.io/letsencrypt/pebble:latest}
CHALL_IMAGE=${CHALL_IMAGE:-ghcr.io/letsencrypt/pebble-challtestsrv:latest}
CURL_IMAGE=${CURL_IMAGE:-curlimages/curl:8.11.1}
APP_IMAGE=${APP_IMAGE:-traefik/whoami:v1.10.3}
HOST=who.newaddr.test
HTTP_PORT=5002 # Pebble's httpPort: where it checks HTTP-01 answers
HTTPS_PORT=8443
ACME_CA=https://pebble:14000/dir
MINICA=/etc/pebble-minica.pem
EXTRA=()

# route_flags ADDR: the router's flags, with the router on ADDR.
route_flags() {
	echo "--route-addr $1 --route-http-port $HTTP_PORT --route-https-port $HTTPS_PORT --acme-email ops@newaddr.test --acme-ca $ACME_CA"
}

# start_acme: Pebble; its DNS, which answers HOST with the three nodes'
# addresses; and the client, a curl container that trusts Pebble's root.
# rs_cleanup removes them.
start_acme() {
	local c chall_ip
	PEBBLE=$CL_RUN-pebble
	CHALL=$CL_RUN-chall
	CLIENT=$CL_RUN-client
	EXTRA=("$PEBBLE" "$CHALL" "$CLIENT")
	trap rs_cleanup EXIT
	docker run -d --name "$CHALL" --network "$CL_NET" "$CHALL_IMAGE" \
		-defaultIPv4 "$(cl_ip_of "$S")" -defaultIPv6 "" -http01 "" -https01 "" -tlsalpn01 "" -doh "" >/dev/null
	chall_ip=$(cl_ip_of "$CHALL")
	docker run -d --name "$PEBBLE" --network "$CL_NET" --network-alias pebble \
		-e PEBBLE_VA_NOSLEEP=1 -e PEBBLE_WFE_NONCEREJECT=0 \
		"$PEBBLE_IMAGE" -config test/config/pebble-config.json -dnsserver "$chall_ip:8053" >/dev/null
	docker run -d --name "$CLIENT" --network "$CL_NET" --entrypoint sleep "$CURL_IMAGE" 100000 >/dev/null
	docker cp "$PEBBLE:/test/certs/pebble.minica.pem" "$CL_WORK/minica.pem"
	for c in "${CL_NAMES[@]}"; do
		docker cp "$CL_WORK/minica.pem" "$c:$MINICA"
	done
	docker cp "$CL_WORK/minica.pem" "$CLIENT:/tmp/minica.pem"
	wait_for 60 docker exec "$CLIENT" curl -sf --cacert /tmp/minica.pem "$ACME_CA" || cl_fail "Pebble did not answer at $ACME_CA"
	docker exec "$CLIENT" sh -c "curl -sf --cacert /tmp/minica.pem https://pebble:15000/roots/0 >/tmp/root.pem" ||
		cl_fail "could not read Pebble's root"
	docker exec "$CLIENT" curl -sf -d "{\"host\":\"$HOST\",\"addresses\":[\"$(cl_ip_of "$S")\",\"$W1_IP\",\"$W2_IP\"]}" \
		"http://$chall_ip:8055/add-a" >/dev/null || cl_fail "challtestsrv did not take the A records"
	cl_note "Pebble is up; challtestsrv answers $HOST with the three nodes"
}

# code IP: the status code of an https request for HOST through IP's router.
code() {
	docker exec "$CLIENT" curl -s -o /dev/null -w '%{http_code}' --max-time 10 --cacert /tmp/root.pem \
		--resolve "$HOST:$HTTPS_PORT:$1" "https://$HOST:$HTTPS_PORT/" || true
}
served() { [ "$(code "$1")" = 200 ]; }

# loop_start FILE IP: requests through IP's router in the client, one after
# another, each status code on a line of FILE ("000" for none), until
# FILE.stop exists.
loop_start() {
	docker exec -d "$CLIENT" sh -c "while [ ! -e $1.stop ]; do curl -s -o /dev/null -w '%{http_code}\n' --connect-timeout 3 \
		--max-time 15 --cacert /tmp/root.pem --resolve $HOST:$HTTPS_PORT:$2 https://$HOST:$HTTPS_PORT/ >>$1; sleep 0.05; done"
}

# loop_stop FILE [WAIT]: stops the loop and prints its codes, one a line,
# after WAIT seconds (16 by default, for the request in flight to end).
loop_stop() {
	docker exec "$CLIENT" touch "$1.stop"
	sleep "${2:-16}"
	docker exec "$CLIENT" cat "$1"
}

# codes_summary prints the count of each code on standard input.
codes_summary() { sort | uniq -c | awk '{ printf "%s %s; ", $2, $1 }'; }

# who_on_workers: who's two replicas run, one on each worker.
who_on_workers() {
	local rows
	rows=$(app_rows who)
	[ "$(awk '$3 == "running"' <<<"$rows" | wc -l)" -eq 2 ] &&
		[ "$(awk -v a="$W1_ID" -v b="$W2_ID" '$3 == "running" && ($2 == a || $2 == b) { print $2 }' <<<"$rows" | sort -u | wc -l)" -eq 2 ]
}

# deploy_who: the routed app, with the server's node ineligible, so both
# replicas go to the workers; then the node is eligible again, and the
# first worker's router serves the host.
deploy_who() {
	docker exec "$S" hivemind node eligibility "$CL_S_ID" --disable >/dev/null || cl_fail "making the server's node ineligible"
	docker exec -i "$S" hivemind deploy -f - >/dev/null <<EOF || cl_fail "deploy who"
name: who
image: $APP_IMAGE
replicas: 2
routes:
  - host: $HOST
    port: 80
health:
  http:
    port: 80
    path: /health
  interval: 1s
  timeout: 1s
EOF
	wait_for 180 who_on_workers || cl_fail "who did not run on both workers: $(app_rows who | tr '\n' ';')"
	docker exec "$S" hivemind node eligibility "$CL_S_ID" --enable >/dev/null || cl_fail "making the server's node eligible"
	wait_for 120 served "$W1_IP" || cl_fail "the router on $W1 does not serve $HOST: $(code "$W1_IP")"
	cl_note "who runs on both workers, and $W1's router serves $HOST"
}

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

# no_duplicates: no replica name appears on two nodes, for any app.
no_duplicates() {
	[ -z "$( (containers_of spread; containers_of web; containers_of who) | awk '{ print $2 }' | sort | uniq -d)" ]
}

file_sum() { docker exec "$1" sha256sum "$2" | awk '{ print $1 }'; }

workers_ready() {
	[ "$(docker exec "$S" hivemind nodes | awk -v a="$W1_ID" -v b="$W2_ID" '($1 == a || $1 == b) && / ready /' | wc -l)" -eq 2 ]
}

workers_waiting() {
	[ "$(docker exec "$S" hivemind nodes | awk -v a="$W1_ID" -v b="$W2_ID" '($1 == a || $1 == b) && / waiting /' | wc -l)" -eq 2 ]
}

# server_address: the ADDRESS of the server's node in hivemind nodes.
server_address() { docker exec "$S" hivemind nodes | awk '$3 == "server" { print $4 }'; }

# start_cluster: three nodes, the server's container with ALIAS when
# CL_ADVERTISE is set, serve, the workers joined, and both apps at their
# counts. With ROUTED set, the ACME containers too, a router on every node,
# and who on the workers. Records what the checks after the loss compare
# against.
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
	W1_IP=$(cl_ip_of "$W1")
	W2_IP=$(cl_ip_of "$W2")
	# Workers take replicas once they have been ready for 5 s.
	CL_SERVE_ARGS="--min-ready 5s"
	if [ -n "${ROUTED:-}" ]; then
		start_acme
		CL_SERVE_ARGS="$CL_SERVE_ARGS $(route_flags 0.0.0.0)"
		CL_SERVER_ENV=SSL_CERT_FILE=$MINICA
		CL_JOIN_ARGS=$(route_flags @IP@)
		CL_JOIN_ENV=SSL_CERT_FILE=$MINICA
	fi
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
	[ -z "${ROUTED:-}" ] || deploy_who
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
	if [ -n "${ROUTED:-}" ]; then
		wait_for 60 who_on_workers || cl_fail "who is not on both workers: $(app_rows who | tr '\n' ';')"
	fi
	wait_for 60 no_duplicates || cl_fail "a replica runs twice: $( (containers_of spread; containers_of web; containers_of who) | sort | tr '\n' ';')"
	if docker exec "$S" grep "node lost" /var/log/hivemind.log; then
		cl_fail "the restored server released a node whose worker came back"
	fi
	for i in 1 2; do
		c=${CL_NAMES[$i]}
		[ "$(worker_containers "$c")" = "${CONTAINERS[$i]}" ] ||
			cl_fail "the containers on $c changed: ${CONTAINERS[$i]}before, $(worker_containers "$c")now"
	done
	[ "$(server_address)" = "$NEW_IP" ] || cl_fail "hivemind nodes gives the server address $(server_address), not $NEW_IP"
	# What the workers dial: the server's --advertise after its address,
	# and each worker's node.json in nodes --json.
	local dials=${CL_JOIN_ADDR:-$NEW_IP}:4484
	[ "$(docker exec "$S" hivemind nodes --json | grep -cF "\"dials\": \"$dials\"")" -eq 2 ] ||
		cl_fail "nodes --json does not show both workers dialling $dials: $(docker exec "$S" hivemind nodes --json | tr '\n' ' ')"
	if [ -n "${CL_ADVERTISE:-}" ]; then
		docker exec "$S" hivemind nodes | awk '$3 == "server"' | grep -qF "$NEW_IP ($CL_ADVERTISE)" ||
			cl_fail "hivemind nodes does not show the server as $NEW_IP ($CL_ADVERTISE): $(docker exec "$S" hivemind nodes | tr '\n' ';')"
	fi
	cl_note "apps converged, no replica twice, no node lost, the workers' containers kept; hivemind nodes: server at $NEW_IP${CL_ADVERTISE:+ ($CL_ADVERTISE)}, both workers dial $dials"
}

# Part 1: a server that advertises a DNS name.
by_name() {
	local pids i left codes total bad ok n503 after
	cl_init newaddr-name
	CL_NET_SUBNET=auto
	CL_ADVERTISE=$ALIAS
	CL_JOIN_ADDR=$ALIAS
	ROUTED=1
	start_cluster
	pids="$(docker exec "$W1" pgrep -x hivemind) $(docker exec "$W2" pgrep -x hivemind)"
	# Requests through the first worker's router, across the loss and the
	# restore.
	loop_start /tmp/w1 "$W1_IP"

	lose_server
	docker cp "$CL_WORK/minica.pem" "$S:$MINICA"
	CL_SERVE_ARGS="--min-ready 5s --cluster-addr $NEW_IP $(route_flags 0.0.0.0)"
	# Requests through the new server's router, from before it starts.
	server_loop() { loop_start /tmp/s "$NEW_IP"; }
	BEFORE_SERVE=server_loop
	restore_and_serve
	docker exec "$S" grep -q -- '--advertise does not resolve' /var/log/hivemind.log ||
		cl_fail "serve did not warn that $ALIAS does not resolve: $(docker exec "$S" tail -n 5 /var/log/hivemind.log)"
	docker exec "$S" grep -q 'workers that were up before this start wait for their first report.* waiting=2 ' /var/log/hivemind.log ||
		cl_fail "serve did not log that two workers wait: $(docker exec "$S" tail -n 5 /var/log/hivemind.log)"

	# The workers cannot reach the new server until the record changes.
	# Past the lease timeout they wait; they are not lost.
	left=$((SERVED_AT + 20 - $(date +%s)))
	[ "$left" -le 0 ] || sleep "$left"
	workers_waiting || cl_fail "20 s after the start the workers are not waiting: $(docker exec "$S" hivemind nodes | tr '\n' ';')"
	cl_note "20 s after the start both workers are waiting: $(docker exec "$S" hivemind nodes | awk -v a="$W1_ID" '$1 == a' | tr -s ' ')"

	left=$((SERVED_AT + 30 - $(date +%s)))
	[ "$left" -le 0 ] || sleep "$left"
	# The new server's router is checked up to here: the move below takes
	# its address away for a moment. A request in flight is not counted.
	codes=$(loop_stop /tmp/s 0)
	ok=$(grep -cx 200 <<<"$codes" || true)
	n503=$(grep -cx 503 <<<"$codes" || true)
	after=$(awk 'up && $1 != "200" { n++ } $1 == "200" { up = 1 } END { print n + 0 }' <<<"$codes")
	[ "$n503" -eq 0 ] && [ "$after" -eq 0 ] && [ "$ok" -ge 50 ] ||
		cl_fail "requests through the new server's router: $(codes_summary <<<"$codes")($after not 200 after the first 200)"
	cl_note "the new server's router from before its start: $(codes_summary <<<"$codes")no 503, all 200 once it listened"

	# The record is updated, 30 s after the start: the alias moves to the
	# new server.
	reconnect "$S" "$NEW_IP" --alias "$ALIAS"
	docker exec "$S" pgrep -x hivemind >/dev/null || cl_fail "serve stopped when the alias moved"
	cl_note "moved the alias $ALIAS to $NEW_IP, $(($(date +%s) - SERVED_AT)) s after serve started"
	cl_check_server_listeners

	check_back
	codes=$(loop_stop /tmp/w1)
	total=$(wc -l <<<"$codes")
	bad=$(grep -cvx 200 <<<"$codes" || true)
	[ "$total" -ge 200 ] && [ "$bad" -eq 0 ] ||
		cl_fail "requests through $W1's router across the restore: $(codes_summary <<<"$codes")"
	cl_note "$W1's router across the loss and the restore: $total requests, $bad not 200"
	[ "$(docker exec "$W1" pgrep -x hivemind) $(docker exec "$W2" pgrep -x hivemind)" = "$pids" ] || cl_fail "a worker process was restarted"
	for i in 1 2; do
		[ "$(file_sum "${CL_NAMES[$i]}" /var/lib/hivemind/node.json)" = "${SUM_STATE[$i]}" ] || cl_fail "node.json on ${CL_NAMES[$i]} changed"
		epoch_is "${CL_NAMES[$i]}" 1 || cl_fail "${CL_NAMES[$i]} has not seen the restored server's epoch 1: $(docker exec "${CL_NAMES[$i]}" cat /var/lib/hivemind/cluster-epoch 2>&1)"
	done
	cl_note "by name: the workers were never restarted, node.json is unchanged on both, and both have seen epoch 1"

	# The old server's machine comes back while the restored server is
	# down, and the name leads to the old one again, as a stale record or
	# hosts file would. The workers refuse it and keep their containers,
	# and the first worker's router serves every request.
	loop_start /tmp/w1b "$W1_IP"
	docker exec "$S" pkill -TERM -x hivemind || true
	wait_for 30 worker_stopped "$S" || cl_fail "the restored server did not stop"
	reconnect "$S" "$NEW_IP"
	OLD_NET_ARGS="--alias $ALIAS"
	SERVER_ADDR=$ALIAS:4484
	old_server_returns --advertise "$ALIAS" --cluster-addr "$OLD_IP" --min-ready 5s
	workers_refuse_old

	# The name leads to the restored server again while the old one still
	# runs, as when the record is put right before the old machine is
	# stopped: the alias leaves the old container, whose serve keeps
	# running, and the restored server starts again under it. A worker
	# closes the connection a refused reply came on, so its next ask, at
	# most 10 s later, resolves the name again: both workers, never
	# restarted, report to the restored server while the old one runs. In
	# the fourth soak a worker stayed on the old server, on its pooled
	# connection, until the old server stopped. The restored server's
	# address was never refused, so no worker logs that it talks to it
	# "again", and none logs another refusal.
	reconnect "$OLD_S" "$OLD_IP"
	reconnect "$S" "$NEW_IP" --alias "$ALIAS"
	cl_serve_server
	wait_for 30 docker exec "$S" hivemind status || cl_fail "the restored server's API did not answer after its restart"
	SERVED_AT=$(date +%s)
	wait_for 30 workers_ready ||
		cl_fail "the workers did not report to the restored server while the old one ran: $(docker exec "$S" hivemind nodes | tr '\n' ';') $W1: $(docker exec "$W1" tail -n 3 /var/log/hivemind.log)"
	docker exec "$OLD_S" pgrep -x hivemind >/dev/null || cl_fail "the old server stopped before the workers reported to the restored one"
	cl_note "both workers report to the restored server $(($(date +%s) - SERVED_AT)) s after its API answered, with the old server still up"
	for c in "$W1" "$W2"; do
		[ "$(refusals "$c")" = 1 ] || cl_fail "$c logged the refusal $(refusals "$c") times, not once"
		if since "$c" | grep -q 'this worker talks to it again'; then
			cl_fail "$c logged that it talks to a server again, though it never refused the restored server's address: $(since "$c" | grep 'talks to it again')"
		fi
	done
	old_server_goes
	check_back
	codes=$(loop_stop /tmp/w1b)
	total=$(wc -l <<<"$codes")
	bad=$(grep -cvx 200 <<<"$codes" || true)
	[ "$total" -ge 100 ] && [ "$bad" -eq 0 ] ||
		cl_fail "requests through $W1's router while the old server ran: $(codes_summary <<<"$codes")"
	[ "$(docker exec "$W1" pgrep -x hivemind) $(docker exec "$W2" pgrep -x hivemind)" = "$pids" ] || cl_fail "a worker process was restarted"
	cl_note "by name, the old server back: $W1's router served $total requests, $bad not 200; the workers took the restored server again, never restarted"
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
	for i in 1 2; do
		epoch_is "${CL_NAMES[$i]}" 1 || cl_fail "${CL_NAMES[$i]} has not seen the restored server's epoch 1: $(docker exec "${CL_NAMES[$i]}" cat /var/lib/hivemind/cluster-epoch 2>&1)"
		SUM_STATE[$i]=$(file_sum "${CL_NAMES[$i]}" /var/lib/hivemind/node.json)
	done

	# The old server's machine comes back at the old address. The workers
	# dial the new one; set-server back to the old one is refused, as it is
	# from before the restore.
	OLD_NET_ARGS=
	old_server_returns --advertise "$OLD_IP" --min-ready 5s
	docker exec "$W1" pkill -TERM -x hivemind || true
	wait_for 30 worker_stopped "$W1" || cl_fail "the worker on $W1 did not stop"
	refused 1 "$OLD_IP" "the server at $OLD_IP:4484 is from before a restore of this cluster: it is at epoch 0, and this worker has seen epoch 1"
	docker exec "$OLD_S" grep -q 'a newer copy of this cluster exists: this server was replaced by a restore' /var/log/hivemind.log ||
		cl_fail "the old server did not say that a newer copy of the cluster exists: $(docker exec "$OLD_S" tail -n 5 /var/log/hivemind.log)"
	docker exec -d "$W1" sh -c "exec hivemind serve --role worker >>/var/log/hivemind.log 2>&1"
	wait_for 60 docker exec "$W1" pgrep -x hivemind || cl_fail "the worker on $W1 did not start again"
	wait_for 60 workers_ready || cl_fail "the workers are not ready after the refused set-server: $(docker exec "$S" hivemind nodes | tr '\n' ';')"
	for i in 1 2; do
		c=${CL_NAMES[$i]}
		[ "$(worker_containers "$c")" = "${CONTAINERS[$i]}" ] ||
			cl_fail "the containers on $c changed beside the old server: ${CONTAINERS[$i]}before, $(worker_containers "$c")now"
	done
	old_server_goes
	cl_note "by IP, the old server back: set-server to it refused, the workers on the restored server with their containers"
}

(by_name)
(by_ip)
cl_note "newaddr: all checks passed"
