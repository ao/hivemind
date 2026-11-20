#!/usr/bin/env bash
# Runs a cluster of three servers and two workers in Docker, and checks what
# docs/ha.md says a cluster of several servers does.
#
# Five docker:dind containers share one private network, with Pebble (a test
# ACME CA), pebble-challtestsrv (its DNS) and a curl container that plays
# the client of the routers. None of them publishes a port. Servers s0, s1
# and s2 have fixed addresses, which their Raft addresses name; workers w1
# and w2 run routers. The binary is built with -tags hivemind_testknobs, and
# the servers run with HIVEMIND_TEST_LOCAL_READS=1, so a GET with the
# Hivemind-Test-Local-Read header is answered from the server's own store,
# where every other request goes to the leader: that is how the checks read
# each server's copy. The helpers are in scripts/lib/cluster.sh.
#
# Checks:
#   - formation by join: s0 is a cluster of one; s1 and s2 join it with
#     server join tokens; each becomes a voter once it has caught up, and
#     hivemind servers shows a leader and two followers. The server port,
#     4486, listens on each server's own address only
#   - the workers join through s1, a follower, which passes the join to the
#     leader, and keep the three servers in node.json
#   - deploys through every server: an app deployed through each one is
#     listed by all three, and runs
#   - the leader is killed while two writers on the followers make writes
#     (registry credentials, one every 0.4 s): a new leader within 10 s,
#     as the survivors' own Raft state shows it, and the API's writes again;
#     writes go on; every write the API answered 2xx, before the kill and
#     after it, is in the store of both survivors and, once it is back, of
#     the old leader, and the report prints the counts; no replica runs
#     twice; the workers' leases stay current, so they follow; 1 s requests
#     through both workers' routers to a routed app are all 200
#   - the old leader comes back with its data directory: it rejoins as a
#     follower and catches up
#   - the leader's serve is stopped with SIGTERM: it hands the leadership
#     on first, so another server leads within 5 s of the signal and its log
#     says so; writes through both followers during the stop are counted,
#     and none fails for want of a leader; it exits within 8 s, without the
#     cluster listener's shutdown warning; started again, it is a follower
#   - a partition that isolates the leader (docker network disconnect): it
#     steps down; the other two elect a leader and take writes; a write
#     through the isolated server is refused with "no leader"; after the
#     partition heals, all three have the same rows and one leader
#   - quorum lost: hivemind serve stopped on two of the three. A write
#     through the last one is refused with "no leader" and "a write needs 2",
#     hivemind servers says there is no leader, and every container keeps
#     running. The two come back, and writes work again
#   - hivemind server remove of a follower: it leaves the configuration, its
#     serve stops with the removal message, and the two left take writes
#
# On exit it removes the containers, the network and the volumes it
# created, also after a failure or Ctrl-C. The Go build cache volume
# (hivemind-cache) is kept. Needs Docker, and the docker:dind, golang,
# Pebble, pebble-challtestsrv, curl, nginx and traefik/whoami images.
set -euo pipefail

REPO=$(cd "$(dirname "$0")/.." && pwd)
# shellcheck source=lib/cluster.sh
. "$REPO/scripts/lib/cluster.sh"

PEBBLE_IMAGE=${PEBBLE_IMAGE:-ghcr.io/letsencrypt/pebble:latest}
CHALL_IMAGE=${CHALL_IMAGE:-ghcr.io/letsencrypt/pebble-challtestsrv:latest}
CURL_IMAGE=${CURL_IMAGE:-curlimages/curl:8.11.1}
APP_IMAGE=${APP_IMAGE:-traefik/whoami:v1.10.3}
HOST=www.ha.test
HTTP_PORT=5002
HTTPS_PORT=8443
EMAIL=ops@ha.test
ACME_CA=https://pebble:14000/dir
# The longest a failover may take to give a leader that writes.
ELECTION_LIMIT=10

CL_GO_TAGS=hivemind_testknobs
CL_NET_SUBNET=auto
cl_init ha
for _ in 1 2 3 4 5; do cl_add_node; done
S=("${CL_NAMES[0]}" "${CL_NAMES[1]}" "${CL_NAMES[2]}")
W1=${CL_NAMES[3]}
W2=${CL_NAMES[4]}
PEBBLE=$CL_RUN-pebble
CHALL=$CL_RUN-chall
CLIENT=$CL_RUN-client
EXTRA=("$PEBBLE" "$CHALL" "$CLIENT")

ha_cleanup() {
	local rc=$? c
	set +e
	for c in "${EXTRA[@]}"; do
		docker rm -fv "$c" >/dev/null 2>&1 || true
	done
	(exit "$rc")
	cl_cleanup
}
trap ha_cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

# The nodes, the servers at fixed addresses on the network's subnet, so a
# server that stops and starts, or is cut off and connected again, keeps
# its Raft address.
cl_build
cl_create_network
SUBNET=$(docker network inspect -f '{{range .IPAM.Config}}{{.Subnet}}{{end}}' "$CL_NET")
BASE=${SUBNET%.*}
SIP=("$BASE.11" "$BASE.12" "$BASE.13")
for i in 0 1 2; do
	cl_run_node "${S[$i]}" --ip "${SIP[$i]}"
done
cl_run_node "$W1"
cl_run_node "$W2"
for c in "${CL_NAMES[@]}"; do
	cl_ready_node "$c"
done
W1_IP=$(cl_ip_of "$W1")
W2_IP=$(cl_ip_of "$W2")
cl_note "servers ${SIP[*]}; workers $W1_IP $W2_IP"

# Pebble, its DNS, which answers the routed host with the workers, and the
# client.
docker run -d --name "$CHALL" --network "$CL_NET" "$CHALL_IMAGE" \
	-defaultIPv4 "$W1_IP" -defaultIPv6 "" -http01 "" -https01 "" -tlsalpn01 "" -doh "" >/dev/null
CHALL_IP=$(cl_ip_of "$CHALL")
docker run -d --name "$PEBBLE" --network "$CL_NET" --network-alias pebble \
	-e PEBBLE_VA_NOSLEEP=1 -e PEBBLE_WFE_NONCEREJECT=0 \
	"$PEBBLE_IMAGE" -config test/config/pebble-config.json -dnsserver "$CHALL_IP:8053" >/dev/null
docker run -d --name "$CLIENT" --network "$CL_NET" --entrypoint sleep "$CURL_IMAGE" 100000 >/dev/null
docker cp "$PEBBLE:/test/certs/pebble.minica.pem" "$CL_WORK/minica.pem"
for c in "$W1" "$W2"; do
	docker cp "$CL_WORK/minica.pem" "$c:/etc/pebble-minica.pem"
done
docker cp "$CL_WORK/minica.pem" "$CLIENT:/tmp/minica.pem"
wait_for 60 docker exec "$CLIENT" curl -sf --cacert /tmp/minica.pem "$ACME_CA" || cl_fail "Pebble did not answer at $ACME_CA"
docker exec "$CLIENT" curl -sf -d "{\"host\":\"$HOST\",\"addresses\":[\"$W1_IP\",\"$W2_IP\"]}" \
	"http://$CHALL_IP:8055/add-a" >/dev/null || cl_fail "challtestsrv did not take the A records"

# serve_server I: start, or start again, the hivemind of server I. The start
# grace is 30 s, so a node the old leader's loss took is placed again 30 s
# after the new leader starts.
serve_server() {
	docker exec -d -e HIVEMIND_TEST_LOCAL_READS=1 -e HIVEMIND_MIN_READY=0s -e HIVEMIND_START_GRACE=30s "${S[$1]}" \
		sh -c "exec hivemind serve --advertise ${SIP[$1]} >>/var/log/hivemind.log 2>&1"
}

# s0, a cluster of one.
SID=()
SID[0]=$(docker exec "${S[0]}" hivemind init | sed -n 's/.*(node \(node-[0-9a-f]*\)).*/\1/p')
[ -n "${SID[0]}" ] || cl_fail "init printed no node ID"
serve_server 0
wait_for 30 docker exec "${S[0]}" hivemind status || cl_fail "s0's API did not answer"
TOKEN=$(docker exec "${S[0]}" cat /var/lib/hivemind/admin.token)

# hm I ARGS...: the CLI on server I, with the admin token.
hm() {
	local i=$1
	shift
	docker exec -e "HIVEMIND_TOKEN=$TOKEN" -e HIVEMIND_SERVER=http://127.0.0.1:4483 "${S[$i]}" hivemind "$@"
}

# hmi I ARGS...: hm with standard input.
hmi() {
	local i=$1
	shift
	docker exec -i -e "HIVEMIND_TOKEN=$TOKEN" -e HIVEMIND_SERVER=http://127.0.0.1:4483 "${S[$i]}" hivemind "$@"
}

# local_get I PATH: GET PATH from server I's own store.
local_get() {
	docker exec "${S[$1]}" wget -q -O - --header "Authorization: Bearer $TOKEN" --header "Hivemind-Test-Local-Read: 1" \
		"http://127.0.0.1:4483$2"
}

# local_leader I: the leader as server I's own Raft state names it, or nothing.
local_leader() { local_get "$1" /v1/servers 2>/dev/null | sed -n 's/^{"leader":"\([^"]*\)".*/\1/p'; }

# roles I: "id role" of each server, as server I lists them.
roles() { hm "$1" servers 2>/dev/null | awk '$1 ~ /^node-/ { print $1, $4 }'; }

# leader_of I: the leader's ID as server I sees it, or nothing.
leader_of() { roles "$1" | awk '$2 == "leader" { print $1 }'; }

# voters_are I N: server I lists N servers, all voters, one of them the leader.
voters_are() {
	local r
	r=$(roles "$1")
	[ "$(wc -l <<<"$r")" -eq "$2" ] && [ "$(grep -c ' leader$' <<<"$r")" -eq 1 ] &&
		[ "$(grep -c ' follower$' <<<"$r")" -eq $(($2 - 1)) ]
}

# index_of ID: the server index of node ID.
index_of() {
	local i
	for i in 0 1 2; do
		[ "${SID[$i]:-}" = "$1" ] && echo "$i" && return 0
	done
	return 1
}

# Formation by join.
for i in 1 2; do
	tok=$(hm 0 join-token --role server --ttl 10m 2>"$CL_WORK/token.err") || cl_fail "join-token --role server: $(cat "$CL_WORK/token.err")"
	if [ "$i" -eq 1 ]; then
		grep -q "the cluster has two" "$CL_WORK/token.err" || cl_fail "join-token --role server did not warn of a cluster of two: $(cat "$CL_WORK/token.err")"
	fi
	docker exec -d -e "HM_TOKEN=$tok" -e HIVEMIND_TEST_LOCAL_READS=1 -e HIVEMIND_MIN_READY=0s -e HIVEMIND_START_GRACE=30s "${S[$i]}" \
		sh -c "exec hivemind join ${SIP[0]}:4484 --role server --token \"\$HM_TOKEN\" --advertise ${SIP[$i]} >>/var/log/hivemind.log 2>&1"
	wait_for 60 docker exec "${S[$i]}" test -s /var/lib/hivemind/server.json || cl_fail "${S[$i]} did not join"
	SID[$i]=$(docker exec "${S[$i]}" sed -n 's/.*"node_id": *"\(node-[0-9a-f]*\)".*/\1/p' /var/lib/hivemind/server.json | head -n 1)
	wait_for 90 voters_are 0 $((i + 1)) || cl_fail "s0 does not list $((i + 1)) voting servers: $(roles 0 | tr '\n' ';')"
	cl_note "${S[$i]} (${SID[$i]}) joined, and is a voter: $(roles 0 | tr '\n' ';')"
done
for i in 0 1 2; do
	wait_for 30 voters_are "$i" 3 || cl_fail "${S[$i]} does not list three voting servers: $(roles "$i" | tr '\n' ';')"
done
# The server port on each server's own address only; never a wildcard.
for i in 0 1 2; do
	l=$(cl_listeners "${S[$i]}")
	grep -qx "$(cl_hexip "${SIP[$i]}"):1186" <<<"$l" || cl_fail "${S[$i]}'s server port is not on ${SIP[$i]}:4486: $l"
	wild=$(grep -E '^(0{8}|0{32}):(1183|1184|1185|1186)$' <<<"$l" || true)
	[ -z "$wild" ] || cl_fail "${S[$i]} listens on a wildcard address on a hivemind port: $wild"
done
cl_note "three servers, a leader and two followers; the server port listens on each server's address only"

# The workers join through s1, a follower, with routers.
route_flags() { echo "--route-addr $1 --route-http-port $HTTP_PORT --route-https-port $HTTPS_PORT --acme-email $EMAIL --acme-ca $ACME_CA"; }
WID=()
n=0
for c in "$W1" "$W2"; do
	ip=$(cl_ip_of "$c")
	tok=$(hm 1 join-token --ttl 1h 2>/dev/null) || cl_fail "join-token through s1 failed"
	docker exec -d -e "HM_TOKEN=$tok" -e SSL_CERT_FILE=/etc/pebble-minica.pem "$c" sh -c \
		"exec hivemind join ${SIP[1]}:4484 --token \"\$HM_TOKEN\" $(route_flags "$ip") >>/var/log/hivemind.log 2>&1"
	wait_for 60 docker exec "$c" test -s /var/lib/hivemind/node.json || cl_fail "$c did not join through s1"
	WID[$n]=$(cl_node_id_of "$c")
	n=$((n + 1))
done
servers_in_node_json() { [ "$(docker exec "$1" sh -c "sed -n '/\"servers\"/,/]/p' /var/lib/hivemind/node.json" | grep -c ':4484')" -eq 3 ]; }
for c in "$W1" "$W2"; do
	wait_for 60 servers_in_node_json "$c" || cl_fail "$c does not keep the three servers in node.json: $(docker exec "$c" cat /var/lib/hivemind/node.json)"
done
cl_note "workers ${WID[*]} joined through s1, a follower, and keep the three servers in node.json"

# app_rows I APP: "index node state" of each replica, as server I lists them.
app_rows() {
	hm "$1" status "$2" | awk -v app="$2" '$1 == app && $2 ~ /^[0-9]+$/ { print $2, $3, $5 }'
}
running_count() { app_rows "$1" "$2" | awk '$3 == "running"' | wc -l; }
app_running() { [ "$(running_count "$1" "$2")" -eq "$3" ]; }

# Deploys through every server.
for i in 0 1 2; do
	hm "$i" deploy --name "app$i" --image nginx:alpine --replicas 2 >/dev/null || cl_fail "deploy app$i through ${S[$i]}"
done
for i in 0 1 2; do
	for a in app0 app1 app2; do
		out=$(hm "$i" status) || cl_fail "status through ${S[$i]}"
		grep -q "^$a " <<<"$out" || cl_fail "${S[$i]} does not list $a: $out"
	done
done
for a in app0 app1 app2; do
	wait_for 180 app_running 1 "$a" 2 || cl_fail "$a does not run two replicas: $(app_rows 1 "$a" | tr '\n' ';')"
done
cl_note "an app deployed through each server is listed by all three, and runs"

# The routed app, on the workers: the servers take no new replica while it
# is placed.
for id in "${SID[@]}"; do hm 0 node eligibility "$id" --disable >/dev/null || cl_fail "eligibility --disable $id"; done
hmi 2 deploy -f - >/dev/null <<EOF || cl_fail "deploy who"
name: who
image: $APP_IMAGE
replicas: 2
command: [-name, who]
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
wait_for 180 app_running 0 who 2 || cl_fail "who does not run two replicas: $(app_rows 0 who | tr '\n' ';')"
for id in "${SID[@]}"; do hm 0 node eligibility "$id" --enable >/dev/null || cl_fail "eligibility --enable $id"; done
docker exec "$CLIENT" sh -c "curl -sf --cacert /tmp/minica.pem https://pebble:15000/roots/0 >/tmp/root.pem" || cl_fail "Pebble's root"
code() {
	docker exec "$CLIENT" curl -s -o /dev/null -w '%{http_code}' --max-time 10 --cacert /tmp/root.pem \
		--resolve "$HOST:$HTTPS_PORT:$1" "https://$HOST:$HTTPS_PORT/" || true
}
served() { [ "$(code "$1")" = 200 ]; }
for ip in "$W1_IP" "$W2_IP"; do
	wait_for 150 served "$ip" || cl_fail "the router on $ip does not serve $HOST: $(code "$ip")"
done
cl_note "who is served through both workers' routers"

# The leader is killed, with writes going on.
LEADER=$(leader_of 0)
LI=$(index_of "$LEADER") || cl_fail "no leader: $(roles 0 | tr '\n' ';')"
F=()
for i in 0 1 2; do [ "$i" -ne "$LI" ] && F[${#F[@]}]=$i; done
cl_note "the leader is ${S[$LI]} ($LEADER); writers run on ${S[${F[0]}]} and ${S[${F[1]}]}"
# writer I: on server I, a registry credential every 0.4 s, up to 40, each
# line "ok N" or "fail N reason".
writer() {
	docker exec -d -e "HIVEMIND_TOKEN=$TOKEN" "${S[$1]}" sh -c "rm -f /tmp/writer.stop; i=0
		while [ ! -e /tmp/writer.stop ] && [ \$i -lt 40 ]; do
			i=\$((i+1))
			if echo pw | hivemind registry login w$1-\$i.ha.test --username u --password-stdin >/dev/null 2>/tmp/writer.err; then
				echo \"ok \$i \$(date +%s)\"
			else
				echo \"fail \$i \$(date +%s) \$(head -c 200 /tmp/writer.err | tr '\n' ' ')\"
			fi
			sleep 0.4
		done >/tmp/writer.out 2>&1; touch /tmp/writer.done"
}
# The routers, every second through each worker, from now to the end of
# the failover.
docker exec -d "$CLIENT" sh -c "rm -f /tmp/rt.stop; while [ ! -e /tmp/rt.stop ]; do for ip in $W1_IP $W2_IP; do
	curl -s -o /dev/null -w '%{http_code}\n' --max-time 10 --cacert /tmp/root.pem --resolve $HOST:$HTTPS_PORT:\$ip https://$HOST:$HTTPS_PORT/; done; sleep 1; done >/tmp/rt.out 2>&1"
writer "${F[0]}"
writer "${F[1]}"
sleep 5
docker kill "${S[$LI]}" >/dev/null
KILLED=$(date +%s)
cl_note "killed ${S[$LI]}, the leader, at $(date -u +%T)"
# The election, as the survivors' own Raft state has it: no forwarding,
# which would wait for the dead leader.
elected() { # elected OLD I...: one of the servers I... leads, and it is not OLD
	local old=$1 i l
	shift
	for i in "$@"; do
		l=$(local_leader "$i")
		if [ -n "$l" ] && [ "$l" != "$old" ] && [ "$(index_of "$l")" = "$i" ]; then
			return 0
		fi
	done
	return 1
}
ms_since() { echo $(( $(date +%s%3N) - $1 )); }
KILLED_MS=$(date +%s%3N)
until elected "$LEADER" "${F[@]}"; do
	[ "$(ms_since "$KILLED_MS")" -lt $((ELECTION_LIMIT * 1000)) ] || cl_fail "no new leader within $ELECTION_LIMIT s of the kill: $(local_get "${F[0]}" /v1/servers)"
	sleep 0.2
done
ELECTION_MS=$(ms_since "$KILLED_MS")
api_writes() { echo pw | hmi "${F[0]}" registry login probe.ha.test --username u --password-stdin >/dev/null 2>&1; }
wait_for 30 api_writes || cl_fail "the API does not take writes through ${S[${F[0]}]} 30 s after the kill"
API_MS=$(ms_since "$KILLED_MS")
NEWLEADER=$(local_leader "${F[0]}")
cl_note "new leader $NEWLEADER elected ${ELECTION_MS} ms after the kill; the API took a write through a follower ${API_MS} ms after it"
writers_done() { docker exec "${S[${F[0]}]}" test -e /tmp/writer.done && docker exec "${S[${F[1]}]}" test -e /tmp/writer.done; }
wait_for 120 writers_done || cl_fail "the writers did not finish"
docker exec "$CLIENT" touch /tmp/rt.stop
# acked I: the hosts writer I's API calls answered 2xx.
acked() { docker exec "${S[$1]}" awk -v w="$1" '$1 == "ok" { print "w" w "-" $2 ".ha.test" }' /tmp/writer.out; }
ACKED_BEFORE=0
ACKED_AFTER=0
FAILED=0
for i in "${F[@]}"; do
	b=$(docker exec "${S[$i]}" awk -v k="$KILLED" '$1 == "ok" && $3 < k' /tmp/writer.out | wc -l)
	a=$(docker exec "${S[$i]}" awk -v k="$KILLED" '$1 == "ok" && $3 >= k' /tmp/writer.out | wc -l)
	f=$(docker exec "${S[$i]}" awk '$1 == "fail"' /tmp/writer.out | wc -l)
	ACKED_BEFORE=$((ACKED_BEFORE + b))
	ACKED_AFTER=$((ACKED_AFTER + a))
	FAILED=$((FAILED + f))
done
[ "$ACKED_AFTER" -gt 0 ] || cl_fail "no write was acknowledged after the kill: $(docker exec "${S[${F[0]}]}" cat /tmp/writer.out | tr '\n' ';')"
# has_all I: server I's own store has every acknowledged write.
has_all() {
	local list miss=0 h i
	list=$(local_get "$1" /v1/registries) || return 1
	for i in "${F[@]}"; do
		for h in $(acked "$i"); do
			grep -q "\"$h\"" <<<"$list" || miss=$((miss + 1))
		done
	done
	[ "$miss" -eq 0 ]
}
for i in "${F[@]}"; do
	wait_for 20 has_all "$i" || cl_fail "${S[$i]}'s store lacks acknowledged writes: $(local_get "$i" /v1/registries)"
done
cl_note "writes: $ACKED_BEFORE acknowledged before the kill, $ACKED_AFTER after it, $FAILED refused or timed out; every acknowledged one is in both survivors' stores"
ROUTER_TOTAL=$(docker exec "$CLIENT" sh -c 'wc -l </tmp/rt.out')
ROUTER_BAD=$(docker exec "$CLIENT" sh -c 'grep -vc "^200$" /tmp/rt.out || true')
[ "$ROUTER_BAD" -eq 0 ] || cl_fail "requests through the workers' routers failed during the failover: $(docker exec "$CLIENT" sort /tmp/rt.out | uniq -c | tr '\n' ';')"
cl_note "routers: $ROUTER_TOTAL requests through the workers' routers during the failover, all 200"
# The workers follow: their leases stay current through the new leader.
workers_ready() {
	local out
	out=$(hm "${F[0]}" nodes) || return 1
	for id in "${WID[@]}"; do
		[ "$(awk -v id="$id" '$1 == id && / ready / { n++ } END { print n + 0 }' <<<"$out")" -eq 1 ] || return 1
	done
}
wait_for 30 workers_ready || cl_fail "the workers are not ready through the new leader: $(hm "${F[0]}" nodes)"
cl_note "both workers stay ready through the new leader"

# containers_of APP: "node name" of every running container of APP.
containers_of() {
	local c
	for c in "${CL_NAMES[@]}"; do
		docker exec "$c" docker ps --filter "label=hivemind.app=$1" --format '{{.Names}}' 2>/dev/null | sed "s/^/$c /" || true
	done
}
no_duplicates() {
	local dup
	dup=$( (for a in app0 app1 app2 who pin; do containers_of "$a"; done) | awk '{ print $2 }' | sort | uniq -d)
	[ -z "$dup" ]
}
# apps_back: each app runs two replicas, none on the dead leader's node.
apps_back() {
	local a
	for a in app0 app1 app2; do
		[ "$(app_rows "${F[0]}" "$a" | awk -v dead="$LEADER" '$3 == "running" && $2 != dead' | wc -l)" -eq 2 ] || return 1
	done
}
wait_for 150 apps_back || cl_fail "the apps are not back at their counts: $(for a in app0 app1 app2; do app_rows "${F[0]}" "$a"; done | tr '\n' ';')"
no_duplicates || cl_fail "a replica runs twice: $( (for a in app0 app1 app2 who; do containers_of "$a"; done) | sort | tr '\n' ';')"
cl_note "the old leader's replicas run elsewhere $(($(date +%s) - KILLED)) s after the kill (the start grace is 30 s); no replica runs twice"

# The old leader comes back.
docker start "${S[$LI]}" >/dev/null
wait_for 90 docker exec "${S[$LI]}" docker info || cl_fail "the Docker daemon in ${S[$LI]} did not start again"
serve_server "$LI"
back_as_follower() {
	[ "$(roles "$LI" | awk -v id="$LEADER" '$1 == id { print $2 }')" = follower ] && has_all "$LI"
}
wait_for 90 back_as_follower || cl_fail "the old leader is not a follower that caught up: $(roles "$LI" | tr '\n' ';')"
for i in 0 1 2; do
	wait_for 30 voters_are "$i" 3 || cl_fail "${S[$i]} does not list three voters after the return: $(roles "$i" | tr '\n' ';')"
done
cl_note "${S[$LI]} came back as a follower, and has every acknowledged write"
wait_for 60 no_duplicates || cl_fail "a replica runs twice after the old leader came back: $( (for a in app0 app1 app2 who; do containers_of "$a"; done) | sort | tr '\n' ';')"

# The leader's serve stopped with SIGTERM, as systemctl stop sends it: it
# hands the leadership on first, so another server leads before it stops.
GL=$(leader_of 0)
GI=$(index_of "$GL") || cl_fail "no leader before the leader's stop: $(roles 0 | tr '\n' ';')"
GF=()
for i in 0 1 2; do [ "$i" -ne "$GI" ] && GF[${#GF[@]}]=$i; done
# gwriter I: on server I, through its own API, 20 registry credentials 0.25 s
# apart, each logged out of again, so that the store stays under its 100
# registries; each write a line "ok login|logout N" or "fail ... reason".
gwriter() {
	docker exec -d -e "HIVEMIND_TOKEN=$TOKEN" "${S[$1]}" sh -c "rm -f /tmp/gw.done; i=0
		while [ \$i -lt 20 ]; do
			i=\$((i+1))
			if echo pw | hivemind registry login g$1-\$i.ha.test --username u --password-stdin >/dev/null 2>/tmp/gw.err; then
				echo \"ok login \$i\"
				if hivemind registry logout g$1-\$i.ha.test >/dev/null 2>/tmp/gw.err; then
					echo \"ok logout \$i\"
				else
					echo \"fail logout \$i \$(head -c 200 /tmp/gw.err | tr '\n' ' ')\"
				fi
			else
				echo \"fail login \$i \$(head -c 200 /tmp/gw.err | tr '\n' ' ')\"
			fi
			sleep 0.25
		done >/tmp/gw.out 2>&1; touch /tmp/gw.done"
}
gwriter "${GF[0]}"
gwriter "${GF[1]}"
sleep 1
STOP_MS=$(date +%s%3N)
docker exec "${S[$GI]}" pkill -TERM -x hivemind || cl_fail "no hivemind on ${S[$GI]}"
until elected "$GL" "${GF[@]}"; do
	[ "$(ms_since "$STOP_MS")" -lt 5000 ] || cl_fail "no new leader within 5 s of the leader's stop: $(local_get "${GF[0]}" /v1/servers)"
	sleep 0.05
done
HANDOVER_MS=$(ms_since "$STOP_MS")
serve_stopped() { ! docker exec "${S[$1]}" pgrep -x hivemind >/dev/null; }
until serve_stopped "$GI"; do
	[ "$(ms_since "$STOP_MS")" -lt 30000 ] || cl_fail "${S[$GI]}'s serve did not stop within 30 s of SIGTERM"
	sleep 0.2
done
STOPPED_MS=$(ms_since "$STOP_MS")
docker exec "${S[$GI]}" grep -q "handed the leadership on" /var/log/hivemind.log ||
	cl_fail "the stopped leader did not hand the leadership on: $(docker exec "${S[$GI]}" tail -n 20 /var/log/hivemind.log)"
# Once a follower, it passed its own node's log poll to the new leader,
# which its node listener's stop ends at once, rather than wait 10 s for
# it and warn (the eighth soak's fifth finding).
if docker exec "${S[$GI]}" grep -q 'msg="cluster shutdown"' /var/log/hivemind.log || [ "$STOPPED_MS" -ge 8000 ]; then
	cl_fail "the stopped leader's serve took ${STOPPED_MS} ms to stop: $(docker exec "${S[$GI]}" tail -n 20 /var/log/hivemind.log)"
fi
cl_note "the leader's serve, stopped with SIGTERM, handed the leadership on: $(local_leader "${GF[0]}") led ${HANDOVER_MS} ms after the signal, and the process exited ${STOPPED_MS} ms after it"
gw_done() { docker exec "${S[${GF[0]}]}" test -e /tmp/gw.done && docker exec "${S[${GF[1]}]}" test -e /tmp/gw.done; }
wait_for 120 gw_done || cl_fail "the writers through the followers did not finish"
GW_OK=0
GW_FAIL=0
for i in "${GF[@]}"; do
	GW_OK=$((GW_OK + $(docker exec "${S[$i]}" grep -c '^ok ' /tmp/gw.out || true)))
	GW_FAIL=$((GW_FAIL + $(docker exec "${S[$i]}" grep -c '^fail ' /tmp/gw.out || true)))
	# The CLI tries again while no server leads, so none may fail for it.
	if docker exec "${S[$i]}" grep -q '^fail .*no leader' /tmp/gw.out; then
		cl_fail "a write through ${S[$i]} failed for want of a leader during the leader's stop: $(docker exec "${S[$i]}" grep '^fail ' /tmp/gw.out | tr '\n' ';')"
	fi
done
cl_note "writes through the followers during the leader's stop: $GW_OK acknowledged, $GW_FAIL failed$( [ "$GW_FAIL" -eq 0 ] || echo ": $(for i in "${GF[@]}"; do docker exec "${S[$i]}" grep '^fail ' /tmp/gw.out; done | tr '\n' ';')")"
serve_server "$GI"
follows_again() { [ "$(roles "$GI" | awk -v id="$GL" '$1 == id { print $2 }')" = follower ]; }
wait_for 90 follows_again || cl_fail "the stopped leader is not a follower after its start: $(roles "$GI" | tr '\n' ';')"
for i in 0 1 2; do
	wait_for 30 voters_are "$i" 3 || cl_fail "${S[$i]} does not list three voters after the stopped leader's start: $(roles "$i" | tr '\n' ';')"
done

# A partition that isolates the leader.
LEADER=$(leader_of 0)
LI=$(index_of "$LEADER") || cl_fail "no leader before the partition"
F=()
for i in 0 1 2; do [ "$i" -ne "$LI" ] && F[${#F[@]}]=$i; done
docker network disconnect "$CL_NET" "${S[$LI]}"
cl_note "cut ${S[$LI]} ($LEADER), the leader, off the network"
CUT_MS=$(date +%s%3N)
until elected "$LEADER" "${F[@]}"; do
	[ "$(ms_since "$CUT_MS")" -lt $((ELECTION_LIMIT * 1000)) ] || cl_fail "the majority did not elect a leader within $ELECTION_LIMIT s of the cut: $(local_get "${F[0]}" /v1/servers)"
	sleep 0.2
done
PARTITION_MS=$(ms_since "$CUT_MS")
cl_note "the majority elected $(local_leader "${F[0]}") ${PARTITION_MS} ms after the cut"
api_majority() { echo pw | hmi "${F[0]}" registry login part-majority.ha.test --username u --password-stdin >/dev/null 2>&1; }
wait_for 30 api_majority || cl_fail "the majority refused a write"
minority_refuses() {
	local out
	if out=$(echo pw | hmi "$LI" registry login part-minority.ha.test --username u --password-stdin 2>&1); then
		cl_fail "the isolated server took a write: $out"
	fi
	grep -q "no leader" <<<"$out"
}
wait_for 20 minority_refuses || cl_fail "the isolated server's refusal does not say there is no leader: $(echo pw | hmi "$LI" registry login part-minority.ha.test --username u --password-stdin 2>&1)"
out=$(echo pw | hmi "$LI" registry login part-minority.ha.test --username u --password-stdin 2>&1 || true)
cl_note "the isolated server refuses writes: ${out:0:200}"
docker network connect --ip "${SIP[$LI]}" "$CL_NET" "${S[$LI]}" || cl_fail "could not connect ${S[$LI]} again"
converged() {
	local a b c
	a=$(local_get 0 /v1/registries) && b=$(local_get 1 /v1/registries) && c=$(local_get 2 /v1/registries) || return 1
	[ "$a" = "$b" ] && [ "$b" = "$c" ] && grep -q '"part-majority.ha.test"' <<<"$a" && ! grep -q '"part-minority.ha.test"' <<<"$a"
}
wait_for 60 converged || cl_fail "the servers did not converge after the partition healed"
for i in 0 1 2; do
	wait_for 30 voters_are "$i" 3 || cl_fail "${S[$i]} does not list three voters after the heal: $(roles "$i" | tr '\n' ';')"
done
cl_note "after the heal, the three stores have the same registries, the majority's write and not the minority's, and one leader"

# Quorum lost: two of three servers stop.
running_total() {
	local c t=0 n
	for c in "${CL_NAMES[@]}"; do
		n=$(docker exec "$c" docker ps -q --filter label=hivemind.app | wc -l)
		t=$((t + n))
	done
	echo "$t"
}
BEFORE=$(running_total)
LAST=2
for i in 0 1; do
	docker exec "${S[$i]}" pkill hivemind || cl_fail "no hivemind on ${S[$i]}"
done
no_leader_at_last() { [ -z "$(local_leader $LAST)" ]; }
wait_for 20 no_leader_at_last || cl_fail "${S[$LAST]} still sees a leader with two servers down: $(roles $LAST | tr '\n' ';')"
if out=$(echo pw | hmi $LAST registry login quorum.ha.test --username u --password-stdin 2>&1); then
	cl_fail "a write was taken with two of three servers down: $out"
fi
grep -q "no leader" <<<"$out" && grep -q "a write needs 2" <<<"$out" || cl_fail "the refusal without a quorum is not clear: $out"
cl_note "with two of three servers down, a write gets: $out"
servers_out=$(hm $LAST servers 2>&1) || cl_fail "hivemind servers without a leader: $servers_out"
grep -q "^No leader" <<<"$servers_out" || cl_fail "hivemind servers does not say there is no leader: $servers_out"
sleep 20
AFTER=$(running_total)
[ "$AFTER" -eq "$BEFORE" ] || cl_fail "containers stopped without a quorum: $BEFORE before, $AFTER after 20 s"
cl_note "every container keeps running without a quorum ($AFTER)"
serve_server 0
serve_server 1
wait_for 60 voters_are 2 3 || cl_fail "the cluster did not recover: $(roles 2 | tr '\n' ';')"
echo pw | hmi 2 registry login quorum-back.ha.test --username u --password-stdin >/dev/null || cl_fail "no write after the servers came back"
cl_note "the two servers came back; writes work again"

# removed_stopped I: server I's serve has stopped, with the removal message.
removed_stopped() {
	! docker exec "${S[$1]}" pgrep -x hivemind >/dev/null && docker exec "${S[$1]}" grep -q "was removed from the cluster's servers" /var/log/hivemind.log
}
# live_server NOT: a server whose serve runs, other than server NOT.
live_server() {
	local i
	for i in 0 1 2; do
		[ "$i" -ne "$1" ] && docker exec "${S[$i]}" pgrep -x hivemind >/dev/null && echo "$i" && return 0
	done
	return 1
}

# server remove of s0, the server init made, whose node runs a replica: it
# is refused, and the refusal gives the drain. Drained first, s0 is
# removed: it leaves the configuration, its serve stops with the removal
# message, the servers left take writes, and no replica runs twice.
TI=0
K=$(live_server "$TI") || cl_fail "no server runs but ${S[$TI]}"
NSERVERS=$(roles "$K" | wc -l)
others=$(hm "$K" nodes | awk -v t="${SID[$TI]}" '$1 ~ /^node-/ && $1 != t && / ready / && / eligible / { print $1 }')
for id in $others; do hm "$K" node eligibility "$id" --disable >/dev/null || cl_fail "eligibility --disable $id"; done
hm "$K" deploy --name pin --image nginx:alpine --replicas 1 >/dev/null || cl_fail "deploy pin"
pinned() { [ "$(app_rows "$K" pin | awk -v t="${SID[$TI]}" '$2 == t && $3 == "running"' | wc -l)" -eq 1 ]; }
wait_for 120 pinned || cl_fail "pin does not run on ${SID[$TI]}: $(app_rows "$K" pin | tr '\n' ';')"
for id in $others; do hm "$K" node eligibility "$id" --enable >/dev/null || cl_fail "eligibility --enable $id"; done
if out=$(hm "$K" server remove "${SID[$TI]}" 2>&1); then
	cl_fail "server remove took ${SID[$TI]}, whose node runs pin's replica: $out"
fi
grep -q "hivemind node drain ${SID[$TI]}" <<<"$out" || cl_fail "the refusal of server remove does not give the drain: $out"
docker exec "${S[$TI]}" pgrep -x hivemind >/dev/null || cl_fail "${S[$TI]}'s serve stopped after a refused server remove"
cl_note "server remove of ${SID[$TI]}, whose node runs pin's replica, is refused: ${out:0:240}"
out=$(hm "$K" node drain "${SID[$TI]}" --deadline 5m 2>&1) || cl_fail "node drain ${SID[$TI]}: $out"
out=$(hm "$K" server remove "${SID[$TI]}" 2>&1) || cl_fail "server remove ${SID[$TI]} after its drain: $out"
wait_for 30 removed_stopped "$TI" || cl_fail "the removed server's serve did not stop: $(docker exec "${S[$TI]}" tail -n 5 /var/log/hivemind.log)"
wait_for 30 voters_are "$K" $((NSERVERS - 1)) || cl_fail "the cluster does not list $((NSERVERS - 1)) voters after the removal: $(roles "$K" | tr '\n' ';')"
echo pw | hmi "$K" registry login after-remove.ha.test --username u --password-stdin >/dev/null || cl_fail "no write after the removal"
wait_for 120 app_running "$K" pin 1 || cl_fail "pin does not run one replica after the removal: $(app_rows "$K" pin | tr '\n' ';')"
sleep 20
no_duplicates || cl_fail "a replica runs twice after the drained server's removal: $( (for a in app0 app1 app2 who pin; do containers_of "$a"; done) | sort | tr '\n' ';')"
cl_note "drained first, ${SID[$TI]} left the configuration, its serve stopped with the removal message, the servers left take writes, and no replica runs twice: $out"
cl_note "ha: election ${ELECTION_MS} ms after the kill, API writes again at ${API_MS} ms; a leader ${HANDOVER_MS} ms after the leader's SIGTERM; election ${PARTITION_MS} ms after the cut; writes acknowledged ${ACKED_BEFORE} before and ${ACKED_AFTER} after the kill, all kept on every server; ${FAILED} refused; ${ROUTER_TOTAL} router requests, all 200"
cl_note "ha: all checks passed"
