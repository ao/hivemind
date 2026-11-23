#!/usr/bin/env bash
# Runs what 2.4.0 adds to a cluster of several servers (docs/ha.md), in
# Docker: a rolling upgrade of the servers from 2.3.0, the Raft entries a
# minute before and after it, and a CA rotation of three servers.
# scripts/integration-ha.sh runs it after its own checks; HA_OPS=0 there
# skips it.
#
# Five docker:dind containers on one private network, with Pebble, its DNS
# and a curl client, as in scripts/integration-ha.sh: servers s0, s1 and
# s2 at fixed addresses, workers w1 and w2 with routers. The old binary,
# OLD_TAG (v2.3.1, the published 2.3, unless HIVEMIND_OLD_TAG says
# otherwise), is built from the tag's source with golang:1.27, as this build is: from
# HIVEMIND_OLD_SRC, or .scratch/hivemind-$OLD_TAG-src, or the tag in this
# repository, or the tag's archive on GitHub. Both are built with -tags
# hivemind_testknobs, and the servers run with HIVEMIND_TEST_LOCAL_READS=1,
# so that each server's own store can be read.
#
# The rolling upgrade (docs/ha.md#rolling-upgrades):
#   - three servers and two workers on 2.3.0, with apps on every node and a
#     routed app on the workers; the Raft entries each server appends in a
#     steady window (HAOPS_WINDOW s, 120 by default)
#   - writers on the three servers, each a registry login every 0.5 s and a
#     logout of the one from ten logins before, and requests through both
#     workers' routers every 0.5 s, from here to the end of the upgrade
#   - the first follower upgraded in place: a 2.3.0 leader with a 2.4.0
#     server, which applies what the leader writes; then the second
#   - the leader stopped with SIGTERM: it hands the leadership to an
#     upgraded server. Started once more on 2.3.0: a 2.4.0 leader with a
#     2.3.0 server, which applies what it writes; the store stays at 18, and
#     hivemind servers says the step waits for it. Then it is upgraded
#   - the step to schema version 19 on every server's store, and a 2.3.0
#     binary refused on an upgraded data directory
#   - every acknowledged write in every server's store, the three stores the
#     same row for row, no replica on two nodes, and every request through
#     the workers' routers answered 200
#   - the Raft entries in the same steady window again, on this build: at
#     most a quarter of 2.3.0's
# The CA rotation with several servers
# (docs/ha.md#the-ca-rotation-with-several-servers), on the upgraded
# cluster, with the writers and the router requests going on:
#   - start: every server writes its own ca-next.crt and ca-next.key, the
#     same as the leader's; a server join is refused
#   - a follower stopped once its node trusts the new CA; renew; the
#     workers renew and the leader's certificate moves without it
#   - the stopped follower started again: it follows, its certificate
#     moves, and its node renews
#   - the leader restarted, so that the next leader carries the rotation;
#     finish
#   - every server's ca.crt and ca.key the new CA's, the same on the three,
#     no ca-next.* left, each worker's ca.crt one CA, every acknowledged
#     write kept and every router request answered 200
#
# On exit it removes the containers, the network and the volumes it
# created, also after a failure or Ctrl-C. Needs Docker, jq, and the images
# scripts/integration-ha.sh needs; the nodes' apk add of sqlite, which
# reads raft.db, needs the network.
set -euo pipefail

REPO=$(cd "$(dirname "$0")/.." && pwd)
# shellcheck source=lib/cluster.sh
. "$REPO/scripts/lib/cluster.sh"

command -v jq >/dev/null || {
	echo "haops: needs jq" >&2
	exit 1
}

PEBBLE_IMAGE=${PEBBLE_IMAGE:-ghcr.io/letsencrypt/pebble:latest}
CHALL_IMAGE=${CHALL_IMAGE:-ghcr.io/letsencrypt/pebble-challtestsrv:latest}
CURL_IMAGE=${CURL_IMAGE:-curlimages/curl:8.11.1}
APP_IMAGE=${APP_IMAGE:-traefik/whoami:v1.10.3}
OLD_TAG=${HIVEMIND_OLD_TAG:-v2.3.1}
NEW_VERSION=v2.4.0-haops-test
WINDOW=${HAOPS_WINDOW:-120}
HOST=www.haops.test
HTTP_PORT=5002
HTTPS_PORT=8443
EMAIL=ops@haops.test
ACME_CA=https://pebble:14000/dir

CL_GO_TAGS=hivemind_testknobs
CL_GO_LDFLAGS="-X main.version=$NEW_VERSION"
CL_NET_SUBNET=auto
cl_init haops
for _ in 1 2 3 4 5; do cl_add_node; done
S=("${CL_NAMES[0]}" "${CL_NAMES[1]}" "${CL_NAMES[2]}")
W1=${CL_NAMES[3]}
W2=${CL_NAMES[4]}
PEBBLE=$CL_RUN-pebble
CHALL=$CL_RUN-chall
CLIENT=$CL_RUN-client
EXTRA=("$PEBBLE" "$CHALL" "$CLIENT")

haops_cleanup() {
	local rc=$? c
	set +e
	for c in "${EXTRA[@]}"; do
		docker rm -fv "$c" >/dev/null 2>&1 || true
	done
	(exit "$rc")
	cl_cleanup
}
trap haops_cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

# old_src prints a directory with the source of OLD_TAG.
old_src() {
	local d
	if [ -n "${HIVEMIND_OLD_SRC:-}" ]; then
		echo "$HIVEMIND_OLD_SRC"
		return
	fi
	d="$REPO/.scratch/hivemind-$OLD_TAG-src"
	if [ -f "$d/go.mod" ]; then
		echo "$d"
		return
	fi
	mkdir -p "$d"
	if git -C "$REPO" rev-parse -q --verify "refs/tags/$OLD_TAG" >/dev/null 2>&1; then
		git -C "$REPO" archive "$OLD_TAG" | tar -x -C "$d"
	else
		curl -fsSL "https://github.com/ao/hivemind/archive/refs/tags/$OLD_TAG.tar.gz" | tar -xz -C "$d" --strip-components=1
	fi
	[ -f "$d/go.mod" ] || cl_fail "no source of $OLD_TAG in $d"
	echo "$d"
}

# build_old builds OLD_TAG from its source into $CL_WORK/hivemind-old, as
# cl_build builds this tree.
build_old() {
	local src
	src=$(old_src)
	mkdir -p "$CL_WORK"
	cl_note "building $OLD_TAG from $src"
	docker run --rm -v "$src:/src" -v "$CL_WORK:/out" -v "$CACHE_VOLUME:/cache" -w /src \
		-e GOMODCACHE=/cache/mod -e GOCACHE=/cache/build -e GOFLAGS=-buildvcs=false \
		-e GOPROXY=direct -e GOSUMDB=off -e GOTOOLCHAIN=local -e CGO_ENABLED=0 \
		"$GO_IMAGE" go build -trimpath -tags hivemind_testknobs -ldflags "-X main.version=$OLD_TAG" -o /out/hivemind-old ./cmd/hivemind
}

cl_build
build_old
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
	docker cp "$CL_WORK/hivemind-old" "$c:/usr/local/bin/hm-old"
done
docker exec "${S[0]}" hm-old version | grep -q "^hivemind $OLD_TAG " || cl_fail "the old binary is not $OLD_TAG: $(docker exec "${S[0]}" hm-old version)"
docker exec "${S[0]}" hivemind version | grep -q "^hivemind $NEW_VERSION " || cl_fail "the new binary is not $NEW_VERSION"
W1_IP=$(cl_ip_of "$W1")
W2_IP=$(cl_ip_of "$W2")
cl_note "servers ${SIP[*]}; workers $W1_IP $W2_IP"

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

# BIN[i] is the binary server i runs: hm-old or hivemind.
BIN=(hm-old hm-old hm-old)
SERVE_ENV=(-e HIVEMIND_TEST_LOCAL_READS=1 -e HIVEMIND_MIN_READY=0s -e HIVEMIND_START_GRACE=30s)

# serve_server I: start server I's serve with BIN[I].
serve_server() {
	docker exec -d "${SERVE_ENV[@]}" "${S[$1]}" sh -c "exec ${BIN[$1]} serve --advertise ${SIP[$1]} >>/var/log/hivemind.log 2>&1"
}
# serving I: server I runs serve, or the join of a server; not the CLI the
# writers run there. The brackets keep pgrep from matching its own shell.
serving() { docker exec "${S[$1]}" sh -c "pgrep -f '[s]erve --advertise' || pgrep -f '[-]-role server'" >/dev/null; }
# stop_server I: SIGTERM to server I's serve, as systemctl stop sends, and
# wait for it to exit.
stop_server() {
	docker exec "${S[$1]}" sh -c "pkill -TERM -f '[s]erve --advertise'; pkill -TERM -f '[-]-role server'; true"
	local end=$(($(date +%s) + 30))
	while serving "$1"; do
		[ "$(date +%s)" -lt "$end" ] || cl_fail "${S[$1]}'s serve did not stop within 30 s of SIGTERM"
		sleep 0.2
	done
}

SID=()
SID[0]=$(docker exec "${S[0]}" hm-old init | sed -n 's/.*(node \(node-[0-9a-f]*\)).*/\1/p')
[ -n "${SID[0]}" ] || cl_fail "init printed no node ID"
serve_server 0
wait_for 30 docker exec "${S[0]}" hivemind status || cl_fail "s0's API did not answer"
TOKEN=$(docker exec "${S[0]}" cat /var/lib/hivemind/admin.token)

# hm I ARGS...: this build's CLI on server I, with the admin token.
hm() {
	local i=$1
	shift
	docker exec -e "HIVEMIND_TOKEN=$TOKEN" -e HIVEMIND_SERVER=http://127.0.0.1:4483 "${S[$i]}" hivemind "$@"
}
hmi() {
	local i=$1
	shift
	docker exec -i -e "HIVEMIND_TOKEN=$TOKEN" -e HIVEMIND_SERVER=http://127.0.0.1:4483 "${S[$i]}" hivemind "$@"
}
# hm_any ARGS...: hm on the first server whose serve answers.
hm_any() {
	local i
	for i in 0 1 2; do
		if serving "$i" && hm "$i" "$@" 2>/dev/null; then
			return 0
		fi
	done
	return 1
}
local_get() {
	docker exec "${S[$1]}" wget -q -O - --header "Authorization: Bearer $TOKEN" --header "Hivemind-Test-Local-Read: 1" \
		"http://127.0.0.1:4483$2"
}
roles() { hm "$1" servers 2>/dev/null | awk '$1 ~ /^node-/ { print $1, $4 }'; }
leader_of() { roles "$1" | awk '$2 == "leader" { print $1 }'; }
voters_are() {
	local r
	r=$(roles "$1")
	[ "$(wc -l <<<"$r")" -eq "$2" ] && [ "$(grep -c ' leader$' <<<"$r")" -eq 1 ] &&
		[ "$(grep -c ' follower$' <<<"$r")" -eq $(($2 - 1)) ]
}
index_of() {
	local i
	for i in 0 1 2; do
		[ "${SID[$i]:-}" = "$1" ] && echo "$i" && return 0
	done
	return 1
}
# local_schema I: the schema version of server I's own store, as its own
# servers reply gives it (this build), or nothing (2.3.0).
local_schema() { local_get "$1" /v1/servers 2>/dev/null | jq -r '.schema // empty'; }
# node_version ID: the version node ID last sent, as the leader lists it.
node_version() { hm_any nodes --json | jq -r --arg id "$1" '.[] | select(.id == $id) | .version // empty'; }

for i in 1 2; do
	tok=$(hm 0 join-token --role server --ttl 10m 2>/dev/null) || cl_fail "join-token --role server"
	docker exec -d -e "HM_TOKEN=$tok" "${SERVE_ENV[@]}" "${S[$i]}" \
		sh -c "umask 077; printf %s \"\$HM_TOKEN\" >/root/join-token; exec hm-old join ${SIP[0]}:4484 --role server --token-file /root/join-token --advertise ${SIP[$i]} >>/var/log/hivemind.log 2>&1"
	wait_for 60 docker exec "${S[$i]}" test -s /var/lib/hivemind/server.json || cl_fail "${S[$i]} did not join"
	SID[$i]=$(docker exec "${S[$i]}" sed -n 's/.*"node_id": *"\(node-[0-9a-f]*\)".*/\1/p' /var/lib/hivemind/server.json | head -n 1)
	wait_for 90 voters_are 0 $((i + 1)) || cl_fail "s0 does not list $((i + 1)) voting servers: $(roles 0 | tr '\n' ';')"
done
# A server that join started runs under join's own process; from here on
# each server runs serve, so that it can be stopped and started alike.
for i in 1 2; do
	stop_server "$i"
	serve_server "$i"
	wait_for 60 voters_are 0 3 || cl_fail "${S[$i]} is not a voter again after its restart: $(roles 0 | tr '\n' ';')"
done
cl_note "three servers on $OLD_TAG: $(roles 0 | tr '\n' ';')"

route_flags() { echo "--route-addr $1 --route-http-port $HTTP_PORT --route-https-port $HTTPS_PORT --acme-email $EMAIL --acme-ca $ACME_CA"; }
WID=()
n=0
for c in "$W1" "$W2"; do
	ip=$(cl_ip_of "$c")
	tok=$(hm 0 join-token --ttl 1h 2>/dev/null) || cl_fail "join-token"
	docker exec -d -e "HM_TOKEN=$tok" -e SSL_CERT_FILE=/etc/pebble-minica.pem "$c" sh -c \
		"printf %s \"\$HM_TOKEN\" | hm-old join ${SIP[0]}:4484 --token - $(route_flags "$ip") >>/var/log/hivemind.log 2>&1"
	wait_for 60 docker exec "$c" test -s /var/lib/hivemind/node.json || cl_fail "$c did not join"
	WID[$n]=$(cl_node_id_of "$c")
	n=$((n + 1))
done
servers_in_node_json() { [ "$(docker exec "$1" sh -c "sed -n '/\"servers\"/,/]/p' /var/lib/hivemind/node.json" | grep -c ':4484')" -eq 3 ]; }
for c in "$W1" "$W2"; do
	wait_for 60 servers_in_node_json "$c" || cl_fail "$c does not keep the three servers in node.json"
done

app_rows() { hm_any status "$1" | awk -v app="$1" '$1 == app && $2 ~ /^[0-9]+$/ { print $2, $3, $5 }'; }
app_running() { [ "$(app_rows "$1" | awk '$3 == "running"' | wc -l)" -eq "$2" ]; }
for a in app0 app1 app2; do
	hm 0 deploy --name "$a" --image nginx:alpine --replicas 2 >/dev/null || cl_fail "deploy $a"
done
for id in "${SID[@]}"; do hm 0 node eligibility "$id" --disable >/dev/null || cl_fail "eligibility --disable $id"; done
hmi 0 deploy -f - >/dev/null <<EOF || cl_fail "deploy who"
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
wait_for 180 app_running who 2 || cl_fail "who does not run two replicas: $(app_rows who | tr '\n' ';')"
for id in "${SID[@]}"; do hm 0 node eligibility "$id" --enable >/dev/null || cl_fail "eligibility --enable $id"; done
for a in app0 app1 app2; do
	wait_for 180 app_running "$a" 2 || cl_fail "$a does not run two replicas: $(app_rows "$a" | tr '\n' ';')"
done
docker exec "$CLIENT" sh -c "curl -sf --cacert /tmp/minica.pem https://pebble:15000/roots/0 >/tmp/root.pem" || cl_fail "Pebble's root"
code() {
	docker exec "$CLIENT" curl -s -o /dev/null -w '%{http_code}' --max-time 10 --cacert /tmp/root.pem \
		--resolve "$HOST:$HTTPS_PORT:$1" "https://$HOST:$HTTPS_PORT/" || true
}
served() { [ "$(code "$1")" = 200 ]; }
for ip in "$W1_IP" "$W2_IP"; do
	wait_for 150 served "$ip" || cl_fail "the router on $ip does not serve $HOST: $(code "$ip")"
done
cl_note "apps on every node, and who served through both workers' routers"

# raft_rate LABEL: the entries each server appends in WINDOW s of steady
# state, nothing written through the API, after 30 s to settle. Sets RATE
# to the highest of the three, a minute.
for i in 0 1 2; do docker exec "${S[$i]}" apk add -q --no-cache sqlite >/dev/null || cl_fail "apk add sqlite on ${S[$i]}"; done
raft_idx() { docker exec "${S[$1]}" sqlite3 /var/lib/hivemind/raft/raft.db 'SELECT max(idx) FROM logs'; }
raft_rate() {
	local i n i0=() i1 r
	sleep 30
	for i in 0 1 2; do i0[$i]=$(raft_idx "$i"); done
	sleep "$WINDOW"
	RATE=0
	for i in 0 1 2; do
		i1=$(raft_idx "$i")
		n=$((i1 - i0[i]))
		r=$((n * 60 / WINDOW))
		[ "$r" -gt "$RATE" ] && RATE=$r
		cl_note "$1: ${S[$i]} appended $n entries in $WINDOW s, $r a minute"
	done
}
raft_rate "$OLD_TAG, three servers and two workers"
RATE_BEFORE=$RATE

# The writers: on each server, through its own API, a registry login every
# 0.5 s, and a logout of the one from ten logins before, so that the store
# keeps at most ten of each writer's. Each line "ok|fail login|logout N".
writer() {
	docker exec -d -e "HIVEMIND_TOKEN=$TOKEN" "${S[$1]}" sh -c "rm -f /tmp/w.stop /tmp/w.done; i=0
		while [ ! -e /tmp/w.stop ]; do
			i=\$((i+1))
			if echo pw | hivemind registry login $WPREFIX$1-\$i.haops.test --username u --password-stdin >/dev/null 2>/tmp/w.err; then
				echo \"ok login \$i\"
			else
				echo \"fail login \$i \$(head -c 160 /tmp/w.err | tr '\n' ' ')\"
			fi
			if [ \$i -gt 10 ]; then
				j=\$((i-10))
				if hivemind registry logout $WPREFIX$1-\$j.haops.test >/dev/null 2>/tmp/w.err; then
					echo \"ok logout \$j\"
				else
					echo \"fail logout \$j \$(head -c 160 /tmp/w.err | tr '\n' ' ')\"
				fi
			fi
			sleep 0.5
		done >>/tmp/w.out 2>&1; touch /tmp/w.done"
}
# expected I: the hosts writer I's logins put in the store and its
# logouts did not take out, by acknowledged writes; gone I: those an
# acknowledged logout took out. A write that failed may or may not have
# been applied, and is in neither.
expected() {
	docker exec "${S[$1]}" awk -v w="$WPREFIX$1" '$1 == "ok" && $2 == "login" { in_[$3] = 1 } $2 == "logout" { out[$3] = 1 }
		END { for (k in in_) if (!(k in out)) print w "-" k ".haops.test" }' /tmp/w.out
}
gone() {
	docker exec "${S[$1]}" awk -v w="$WPREFIX$1" '$1 == "ok" && $2 == "logout" { print w "-" $3 ".haops.test" }' /tmp/w.out
}
start_load() {
	local i
	for i in 0 1 2; do
		docker exec "${S[$i]}" rm -f /tmp/w.out
		writer "$i"
	done
	docker exec -d "$CLIENT" sh -c "rm -f /tmp/rt.stop /tmp/rt.out; while [ ! -e /tmp/rt.stop ]; do for ip in $W1_IP $W2_IP; do
		curl -s -o /dev/null -w '%{http_code}\n' --max-time 10 --cacert /tmp/root.pem --resolve $HOST:$HTTPS_PORT:\$ip https://$HOST:$HTTPS_PORT/; done; sleep 0.5; done >/tmp/rt.out 2>&1"
}
stop_load() {
	local i
	for i in 0 1 2; do docker exec "${S[$i]}" touch /tmp/w.stop; done
	for i in 0 1 2; do
		wait_for 30 docker exec "${S[$i]}" test -e /tmp/w.done || cl_fail "the writer on ${S[$i]} did not stop"
	done
	docker exec "$CLIENT" touch /tmp/rt.stop
	sleep 2
}
# check_writes LABEL: every server's own store has every acknowledged
# write, and none an acknowledged logout took out; the three stores have
# the same registries.
store_has_writes() {
	local list miss=0 h i
	list=$(local_get "$1" /v1/registries) || return 1
	for i in 0 1 2; do
		for h in $(expected "$i"); do grep -q "\"$h\"" <<<"$list" || miss=$((miss + 1)); done
		for h in $(gone "$i"); do grep -q "\"$h\"" <<<"$list" && miss=$((miss + 1)); done
	done
	[ "$miss" -eq 0 ]
}
stores_agree() {
	local a b c
	a=$(local_get 0 /v1/registries) && b=$(local_get 1 /v1/registries) && c=$(local_get 2 /v1/registries) || return 1
	[ "$a" = "$b" ] && [ "$b" = "$c" ]
}
check_writes() {
	local i ok=0 fail=0
	for i in 0 1 2; do
		wait_for 30 store_has_writes "$i" || cl_fail "$1: ${S[$i]}'s store lacks acknowledged writes, or has one a logout took out: $(local_get "$i" /v1/registries | head -c 600)"
		ok=$((ok + $(docker exec "${S[$i]}" grep -c '^ok ' /tmp/w.out || true)))
		fail=$((fail + $(docker exec "${S[$i]}" grep -c '^fail ' /tmp/w.out || true)))
	done
	wait_for 30 stores_agree || cl_fail "$1: the three stores have different registries"
	WRITES_OK=$ok
	WRITES_FAILED=$fail
	cl_note "$1: $ok writes acknowledged, $fail failed (those through a server whose serve was stopped); every acknowledged one is in every server's store"
}
check_routers() {
	local total bad
	total=$(docker exec "$CLIENT" sh -c 'wc -l </tmp/rt.out')
	bad=$(docker exec "$CLIENT" sh -c 'grep -vc "^200$" /tmp/rt.out || true')
	[ "$total" -gt 0 ] || cl_fail "$1: no requests through the routers"
	[ "$bad" -eq 0 ] || cl_fail "$1: requests through the workers' routers failed: $(docker exec "$CLIENT" sort /tmp/rt.out | uniq -c | tr '\n' ';')"
	ROUTER_TOTAL=$total
	cl_note "$1: $total requests through the workers' routers, all 200"
}
containers_of() {
	local c
	for c in "${CL_NAMES[@]}"; do
		docker exec "$c" docker ps --filter "label=hivemind.app=$1" --format '{{.Names}}' 2>/dev/null | sed "s/^/$c /" || true
	done
}
no_duplicates() {
	local dup
	dup=$( (for a in app0 app1 app2 who; do containers_of "$a"; done) | awk '{ print $2 }' | sort | uniq -d)
	[ -z "$dup" ]
}

# upgrade I BIN: server I's serve stopped with SIGTERM, and started on BIN;
# it is a voter again, its node reports BIN's version, and its store has
# every acknowledged write.
upgrade() {
	local i=$1 want
	stop_server "$i"
	BIN[$i]=$2
	serve_server "$i"
	want=$NEW_VERSION
	[ "$2" = hm-old ] && want=$OLD_TAG
	wait_for 90 voters_are "$(other_than "$i")" 3 || cl_fail "${S[$i]} is not a voter after its start on $2: $(roles "$(other_than "$i")" | tr '\n' ';')"
	at_version() { [ "$(node_version "${SID[$i]}")" = "$want" ]; }
	wait_for 60 at_version || cl_fail "${S[$i]}'s node does not report $want: $(node_version "${SID[$i]}")"
	wait_for 30 store_has_writes "$i" || cl_fail "${S[$i]}, on $2, lacks acknowledged writes"
}
# other_than I: a server other than I whose serve runs.
other_than() {
	local i
	for i in 0 1 2; do
		[ "$i" -ne "$1" ] && serving "$i" && echo "$i" && return 0
	done
	return 1
}

WPREFIX=u
start_load
sleep 3
L=$(leader_of 0)
LI=$(index_of "$L") || cl_fail "no leader: $(roles 0 | tr '\n' ';')"
F=()
for i in 0 1 2; do [ "$i" -ne "$LI" ] && F[${#F[@]}]=$i; done
cl_note "the leader is ${S[$LI]} ($L); the followers ${S[${F[0]}]} and ${S[${F[1]}]} are upgraded first"

upgrade "${F[0]}" hivemind
[ "$(leader_of "$LI")" = "$L" ] || cl_fail "the leader changed when a follower was upgraded: $(roles "$LI" | tr '\n' ';')"
[ "$(local_schema "${F[0]}")" = 18 ] || cl_fail "the upgraded follower's store is not at 18 under a 2.3.0 leader: $(local_schema "${F[0]}")"
sleep 5
wait_for 30 store_has_writes "${F[0]}" || cl_fail "the 2.4.0 follower does not apply what the 2.3.0 leader writes"
cl_note "a 2.3.0 leader with a 2.4.0 follower: ${S[${F[0]}]} applies what the leader writes, and its store stays at 18"
upgrade "${F[1]}" hivemind

# The leader: its stop hands the leadership to an upgraded server.
stop_server "$LI"
new_leads() { local l; l=$(leader_of "${F[0]}"); [ -n "$l" ] && [ "$l" != "$L" ]; }
wait_for 10 new_leads || cl_fail "no upgraded server leads after the old leader's stop: $(roles "${F[0]}" | tr '\n' ';')"
NL=$(leader_of "${F[0]}")
cl_note "the 2.3.0 leader handed the leadership to $NL, on $NEW_VERSION"
# Started once more on 2.3.0: a 2.4.0 leader with a 2.3.0 server.
serve_server "$LI"
wait_for 90 voters_are "${F[0]}" 3 || cl_fail "the old leader, on 2.3.0 again, is not a voter: $(roles "${F[0]}" | tr '\n' ';')"
sleep 15
wait_for 30 store_has_writes "$LI" || cl_fail "the 2.3.0 server does not apply what the 2.4.0 leader writes"
for i in 0 1 2; do
	[ "$i" -eq "$LI" ] && continue
	[ "$(local_schema "$i")" = 18 ] || cl_fail "${S[$i]}'s store moved to $(local_schema "$i") with a 2.3.0 server in the cluster"
done
waits=$(hm "${F[0]}" servers --json | jq -r '.schema_waits // empty')
grep -q "${SID[$LI]}" <<<"$waits" || cl_fail "hivemind servers does not say the step waits for the 2.3.0 server: $(hm "${F[0]}" servers)"
grep -q "(node)" <<<"$(hm "${F[0]}" servers)" || cl_fail "hivemind servers does not mark the 2.3.0 server's version as its node's: $(hm "${F[0]}" servers)"
cl_note "a 2.4.0 leader with a 2.3.0 server: it applies what the leader writes, the store stays at 18, and hivemind servers says the step waits for it: $waits"
upgrade "$LI" hivemind
at19() {
	local i
	for i in 0 1 2; do [ "$(local_schema "$i")" = 19 ] || return 1; done
}
wait_for 60 at19 || cl_fail "the stores did not move to 19 once every server ran $NEW_VERSION: $(for i in 0 1 2; do local_schema "$i"; done | tr '\n' ' ')"
logged_step() {
	local i
	for i in 0 1 2; do
		docker exec "${S[$i]}" grep -q "every server runs the next schema version" /var/log/hivemind.log && return 0
	done
	return 1
}
logged_step || cl_fail "no server logged the step"
cl_note "every server runs $NEW_VERSION, and the store moved to schema version 19 through the log: $(hm_any servers | tail -n 1)"

# A 2.3.0 binary refuses an upgraded data directory.
DI=${F[1]}
stop_server "$DI"
out=$(docker exec "${S[$DI]}" sh -c "timeout 20 hm-old serve --advertise ${SIP[$DI]} 2>&1; echo rc=\$?" | tail -n 3)
grep -q "schema version 19 and this build knows up to 18" <<<"$out" || cl_fail "2.3.0 on an upgraded data directory: $out"
serve_server "$DI"
wait_for 90 voters_are "$DI" 3 || cl_fail "${S[$DI]} is not a voter after the refused downgrade"
cl_note "2.3.0 on an upgraded data directory is refused: $(grep -o 'the store has schema version 19[^;]*' <<<"$out" | head -n 1)"

stop_load
check_writes "the rolling upgrade"
UPGRADE_WRITES=$WRITES_OK
UPGRADE_FAILED=$WRITES_FAILED
check_routers "the rolling upgrade"
UPGRADE_ROUTER=$ROUTER_TOTAL
wait_for 60 no_duplicates || cl_fail "a replica runs twice after the upgrade: $( (for a in app0 app1 app2 who; do containers_of "$a"; done) | sort | tr '\n' ';')"
cl_note "no replica runs twice"

raft_rate "$NEW_VERSION, three servers and two workers"
RATE_AFTER=$RATE
[ "$((RATE_AFTER * 4))" -le "$RATE_BEFORE" ] || cl_fail "the Raft entries a minute went from $RATE_BEFORE to $RATE_AFTER, not to a quarter or less"
cl_note "Raft entries a minute, steady: $RATE_BEFORE on $OLD_TAG, $RATE_AFTER on $NEW_VERSION"

# The CA rotation with several servers.
WPREFIX=r
start_load
sleep 3
L=$(leader_of 0)
LI=$(index_of "$L") || cl_fail "no leader before the rotation"
F=()
for i in 0 1 2; do [ "$i" -ne "$LI" ] && F[${#F[@]}]=$i; done
fp_of() { docker exec "${S[$1]}" sh -c "test -f /var/lib/hivemind/$2 && sha256sum /var/lib/hivemind/$2 | cut -c1-64"; }
ca_status() { hm "$LI" ca status --json; }
out=$(hm "$LI" ca rotate start 2>&1) || cl_fail "ca rotate start: $out"
NEXT=$(ca_status | jq -r '.next.fingerprint')
[ -n "$NEXT" ] && [ "$NEXT" != null ] || cl_fail "no new CA after start: $(ca_status)"
has_next() { [ -n "$(fp_of "$1" ca-next.crt)" ] && [ -n "$(fp_of "$1" ca-next.key)" ]; }
for i in 0 1 2; do
	wait_for 20 has_next "$i" || cl_fail "${S[$i]} did not write its copy of the new CA"
done
for f in ca-next.crt ca-next.key ca.crt; do
	[ "$(fp_of 0 $f)" = "$(fp_of 1 $f)" ] && [ "$(fp_of 1 $f)" = "$(fp_of 2 $f)" ] || cl_fail "the servers' $f differ after start"
done
docker exec "${S[$LI]}" sh -c 'stat -c %a /var/lib/hivemind/ca-next.key' | grep -qx 600 || cl_fail "ca-next.key is not mode 0600"
if out=$(hm "$LI" join-token --role server 2>&1); then cl_fail "a server join token was issued during a rotation: $out"; fi
cl_note "start: every server wrote the same ca-next.crt and ca-next.key from the record; a server join token is refused: ${out:0:120}"
# trusts ID: the rotation counts node ID as trusting the new CA.
trusts() { ca_status | jq -e --arg id "$1" '.nodes[] | select(.id == $id) | .trusts' >/dev/null; }
renewed() { ca_status | jq -e --arg id "$1" '.nodes[] | select(.id == $id) | .renewed' >/dev/null; }
DOWN=${F[0]}
wait_for 60 trusts "${SID[$DOWN]}" || cl_fail "${SID[$DOWN]}, a server's node, does not trust the new CA: $(hm "$LI" ca status)"
stop_server "$DOWN"
cl_note "${S[$DOWN]} stopped once its node trusts the new CA"
for id in "${WID[@]}"; do
	wait_for 60 trusts "$id" || cl_fail "worker $id does not trust the new CA: $(hm "$LI" ca status)"
done
out=$(hm "$LI" ca rotate renew 2>&1) || cl_fail "ca rotate renew: $out"
moved() { ca_status | jq -e '.rotation.listener_moved' >/dev/null; }
wait_for 60 moved || cl_fail "the leader's certificate did not move with ${S[$DOWN]} down: $(hm "$LI" ca status)"
for id in "${WID[@]}"; do
	wait_for 60 renewed "$id" || cl_fail "worker $id did not renew under the new CA: $(hm "$LI" ca status)"
done
if out=$(hm "$LI" ca rotate finish 2>&1); then cl_fail "finish went ahead with ${S[$DOWN]}'s node not renewed: $out"; fi
grep -q "${SID[$DOWN]}" <<<"$out" || cl_fail "finish's refusal does not name the stopped server's node: $out"
cl_note "renew; the workers renewed and the leader's certificate moved with ${S[$DOWN]} down; finish waits for its node"
serve_server "$DOWN"
wait_for 90 voters_are "$LI" 3 || cl_fail "${S[$DOWN]} is not a voter after its start"
wait_for 60 renewed "${SID[$DOWN]}" || cl_fail "${S[$DOWN]}'s node did not renew once it was back: $(hm "$LI" ca status)"
srv_moved() { [ "$(hm "$LI" ca status --json | jq -r --arg id "${SID[$DOWN]}" '.servers[] | select(.id == $id) | .listener')" = "$NEXT" ]; }
wait_for 60 srv_moved || cl_fail "${S[$DOWN]}'s certificate is not from the new CA: $(hm "$LI" ca status)"
cl_note "${S[$DOWN]}, back, followed the rotation: its certificate is from the new CA, and its node renewed"
stop_server "$LI"
serve_server "$LI"
wait_for 90 voters_are "$LI" 3 || cl_fail "the old leader is not a voter after its restart"
L2=$(leader_of "$LI")
L2I=$(index_of "$L2") || cl_fail "no leader after the restart"
LI=$L2I
finish_ready() { [ "$(ca_status | jq '.rotation.waiting | length')" -eq 0 ]; }
wait_for 60 finish_ready || cl_fail "finish waits for nodes after the leader's restart: $(hm "$LI" ca status)"
out=$(hm "$LI" ca rotate finish 2>&1) || cl_fail "ca rotate finish: $out"
ended() {
	local i
	for i in 0 1 2; do
		[ -z "$(fp_of "$i" ca-next.crt)" ] && [ -z "$(fp_of "$i" ca-next.key)" ] || return 1
	done
	[ "$(fp_of 0 ca.crt)" = "$(fp_of 1 ca.crt)" ] && [ "$(fp_of 1 ca.crt)" = "$(fp_of 2 ca.crt)" ] &&
		[ "$(fp_of 0 ca.key)" = "$(fp_of 1 ca.key)" ] && [ "$(fp_of 1 ca.key)" = "$(fp_of 2 ca.key)" ]
}
wait_for 30 ended || cl_fail "the servers' CA files after finish: $(for i in 0 1 2; do echo "${S[$i]} $(docker exec "${S[$i]}" ls /var/lib/hivemind)"; done | tr '\n' ';')"
[ "$(ca_status | jq -r '.current.fingerprint')" = "$NEXT" ] || cl_fail "the cluster CA after finish is not the new one: $(hm "$LI" ca status)"
# A worker drops the old CA from its ca.crt at its next look at the
# server's set, within a few seconds of finish.
one_ca() { [ "$(docker exec "$1" grep -c 'BEGIN CERTIFICATE' /var/lib/hivemind/ca.crt)" -eq 1 ]; }
for c in "$W1" "$W2"; do
	wait_for 30 one_ca "$c" || cl_fail "$c's ca.crt holds $(docker exec "$c" grep -c 'BEGIN CERTIFICATE' /var/lib/hivemind/ca.crt) CAs 30 s after finish"
done
stop_load
check_writes "the CA rotation"
check_routers "the CA rotation"
cl_note "finish: every server's ca.crt and ca.key are the new CA's, the same on the three, with no ca-next.* left; each worker's ca.crt holds it alone"
cl_note "haops: Raft entries a minute $RATE_BEFORE before and $RATE_AFTER after; the upgrade kept $UPGRADE_WRITES acknowledged writes ($UPGRADE_FAILED failed) and $UPGRADE_ROUTER router requests answered 200; the rotation kept $WRITES_OK ($WRITES_FAILED failed) and $ROUTER_TOTAL"
cl_note "haops: all checks passed"
