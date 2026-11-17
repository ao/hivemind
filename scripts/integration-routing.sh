#!/usr/bin/env bash
# Runs a three-node hivemind cluster with a router on every node, against a
# private ACME CA, and checks HTTPS routing end to end.
#
# Three docker:dind nodes share one private network with Pebble (a test ACME
# CA), pebble-challtestsrv (its DNS, which answers the test host with the
# three nodes' addresses) and a curl container that plays the client. None
# of them publishes a port, and no request goes to a real CA. The server
# runs its router on 0.0.0.0; each worker runs its router on its own address.
# The routers' HTTP port is 5002, the port Pebble validates HTTP-01 on, and
# their HTTPS port is 8443. The nodes trust Pebble's own TLS certificate for
# the ACME API through SSL_CERT_FILE; the client trusts the root Pebble
# issues under.
#
# Checks:
#   - listeners: the routers on 0.0.0.0 on the server and on each worker's
#     address; the API, cluster and join ports as before, never on a
#     wildcard; each node's private address in hivemind nodes
#   - an app with a route and two replicas runs them on two nodes, each
#     publishing port 80 on an ephemeral port of its node's address
#   - the certificate: one is issued, once, though all three routers asked
#     for it at the same time (the storage lock), and every router serves it
#   - through each node's router, with SNI, replies come from both replicas;
#     http redirects to https with 308, and an unknown host gets a 404
#   - a replica gets the router's forwarding headers and the table's host,
#     none of the client's; an h2c upgrade reaches it as plain HTTP/1.1;
#     each router logs its connection caps
#   - the server's /metrics has every router's counters and certificate
#     expiry, by node (the workers post theirs through the node API)
#   - a rolling update: every response during it, through all three routers,
#     is a 200
#   - path routes: a second app on the same host under /v1. Through each
#     router, /v1 and the paths under it reach it and every other path the
#     first app, with the path as sent; dot segments, encoded slashes and
#     backslashes get 400; hivemind routes shows both. A rolling update of
#     each app in turn, with requests for / and /v1/x through all three
#     routers: every one is a 200 from the app that owns the path
#   - a node that runs a replica is killed: requests through the other two
#     routers keep getting 200s while the replica is placed again
#
# On exit it removes the containers, the network and the volumes it created,
# also after a failure or Ctrl-C. The Go build cache volume (hivemind-cache)
# is kept.
#
# Needs Docker on the host, and the docker:dind, golang, Pebble,
# pebble-challtestsrv, curl and traefik/whoami images. Run it from anywhere
# in the repository.
set -euo pipefail

REPO=$(cd "$(dirname "$0")/.." && pwd)
# shellcheck source=lib/cluster.sh
. "$REPO/scripts/lib/cluster.sh"

PEBBLE_IMAGE=${PEBBLE_IMAGE:-ghcr.io/letsencrypt/pebble:latest}
CHALL_IMAGE=${CHALL_IMAGE:-ghcr.io/letsencrypt/pebble-challtestsrv:latest}
CURL_IMAGE=${CURL_IMAGE:-curlimages/curl:8.11.1}
APP_IMAGE=${APP_IMAGE:-traefik/whoami:v1.10.3}
HOST=www.routing.test
HTTP_PORT=5002 # Pebble's httpPort: where it checks HTTP-01 answers
HTTPS_PORT=8443
EMAIL=ops@routing.test
ACME_CA=https://pebble:14000/dir

cl_init routing
cl_add_node
cl_add_node
cl_add_node
S=${CL_NAMES[0]}
W1=${CL_NAMES[1]}
W2=${CL_NAMES[2]}
PEBBLE=$CL_RUN-pebble
CHALL=$CL_RUN-chall
CLIENT=$CL_RUN-client
EXTRA=("$PEBBLE" "$CHALL" "$CLIENT")

rt_cleanup() {
	local rc=$? c
	# errexit would end the trap at the first failing command below, before
	# cl_cleanup removes the nodes.
	set +e
	if [ "$rc" -ne 0 ]; then
		echo "routing: --- last lines of Pebble:" >&2
		docker logs --tail 30 "$PEBBLE" >&2 2>&1 || true
	fi
	for c in "${EXTRA[@]}"; do
		docker rm -fv "$c" >/dev/null 2>&1 || true
	done
	(exit "$rc")
	cl_cleanup
}
trap rt_cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

cl_start_nodes
S_IP=$(cl_ip_of "$S")
W1_IP=$(cl_ip_of "$W1")
W2_IP=$(cl_ip_of "$W2")
IPS=("$S_IP" "$W1_IP" "$W2_IP")
cl_note "addresses: server $S_IP, $W1 $W1_IP, $W2 $W2_IP"

# The test CA and its DNS. challtestsrv answers every A query with the
# server's address unless told otherwise, and no AAAA query, so Pebble
# validates over IPv4 on the private network.
docker run -d --name "$CHALL" --network "$CL_NET" "$CHALL_IMAGE" \
	-defaultIPv4 "$S_IP" -defaultIPv6 "" -http01 "" -https01 "" -tlsalpn01 "" -doh "" >/dev/null
CHALL_IP=$(cl_ip_of "$CHALL")
docker run -d --name "$PEBBLE" --network "$CL_NET" --network-alias pebble \
	-e PEBBLE_VA_NOSLEEP=1 -e PEBBLE_WFE_NONCEREJECT=0 \
	"$PEBBLE_IMAGE" -config test/config/pebble-config.json -dnsserver "$CHALL_IP:8053" >/dev/null
docker run -d --name "$CLIENT" --network "$CL_NET" --entrypoint sleep "$CURL_IMAGE" 100000 >/dev/null
docker cp "$PEBBLE:/test/certs/pebble.minica.pem" "$CL_WORK/minica.pem"
for c in "${CL_NAMES[@]}"; do
	docker cp "$CL_WORK/minica.pem" "$c:/etc/pebble-minica.pem"
done
docker cp "$CL_WORK/minica.pem" "$CLIENT:/tmp/minica.pem"
wait_for 60 docker exec "$CLIENT" curl -sf --cacert /tmp/minica.pem "$ACME_CA" || cl_fail "Pebble did not answer at $ACME_CA"
docker exec "$CLIENT" curl -sf -d "{\"host\":\"$HOST\",\"addresses\":[\"$S_IP\",\"$W1_IP\",\"$W2_IP\"]}" \
	"http://$CHALL_IP:8055/add-a" >/dev/null || cl_fail "challtestsrv did not take the A records"
cl_note "Pebble is up; challtestsrv answers $HOST with the three nodes"

route_flags() { # route_flags ADDR
	echo "--route-addr $1 --route-http-port $HTTP_PORT --route-https-port $HTTPS_PORT --acme-email $EMAIL --acme-ca $ACME_CA"
}

# The server, with its router on every address.
CL_S_IP=$S_IP
CL_S_ID=$(docker exec "$S" hivemind init | sed -n 's/.*(node \(node-[0-9a-f]*\)).*/\1/p')
[ -n "$CL_S_ID" ] || cl_fail "init printed no node ID"
docker exec -d -e SSL_CERT_FILE=/etc/pebble-minica.pem "$S" sh -c \
	"exec hivemind serve --advertise $S_IP --min-ready 0s $(route_flags 0.0.0.0) >>/var/log/hivemind.log 2>&1"
wait_for 30 docker exec "$S" hivemind status || cl_fail "the API did not answer"

# The workers, each with its router on its own address.
CL_IDS[0]=$CL_S_ID
for i in 1 2; do
	c=${CL_NAMES[$i]}
	tok=$(docker exec "$S" hivemind join-token --ttl 1h 2>/dev/null) || cl_fail "join-token failed"
	docker exec -d -e "HM_TOKEN=$tok" -e SSL_CERT_FILE=/etc/pebble-minica.pem "$c" sh -c \
		"exec hivemind join $S_IP:4484 --token \"\$HM_TOKEN\" $(route_flags "${IPS[$i]}") >>/var/log/hivemind.log 2>&1"
	wait_for 60 docker exec "$c" test -s /var/lib/hivemind/node.json || cl_fail "$c did not join"
	CL_IDS[$i]=$(cl_node_id_of "$c")
done
cl_note "joined: ${CL_IDS[*]}"

# Listeners. The router is the one listener that binds a wildcard, and only
# on the server, where --route-addr is 0.0.0.0.
hexport() { printf '%04X' "$1"; }
HP=$(hexport $HTTP_PORT)
SP=$(hexport $HTTPS_PORT)
wait_for 30 sh -c "docker exec $S sh -c 'cat /proc/net/tcp' | awk '\$4 == \"0A\" { print \$2 }' | grep -qx 00000000:$SP" ||
	cl_fail "the server's router is not on 0.0.0.0:$HTTPS_PORT: $(cl_listeners "$S" | tr '\n' ' ')"
l=$(cl_listeners "$S")
grep -qx "00000000:$HP" <<<"$l" || cl_fail "the server's router is not on 0.0.0.0:$HTTP_PORT: $l"
cl_check_server_listeners
for i in 1 2; do
	c=${CL_NAMES[$i]}
	wait_for 30 sh -c "docker exec $c cat /proc/net/tcp | awk '\$4 == \"0A\" { print \$2 }' | grep -qx $(cl_hexip "${IPS[$i]}"):$SP" ||
		cl_fail "$c's router is not on ${IPS[$i]}:$HTTPS_PORT: $(cl_listeners "$c" | tr '\n' ' ')"
	l=$(cl_listeners "$c")
	grep -qx "$(cl_hexip "${IPS[$i]}"):$HP" <<<"$l" || cl_fail "$c's router is not on ${IPS[$i]}:$HTTP_PORT: $l"
	wild=$(grep -E "^(0{8}|0{32}):($HP|$SP)$" <<<"$l" || true)
	[ -z "$wild" ] || cl_fail "$c's router is on a wildcard address, though --route-addr names one: $wild"
done
cl_check_worker_listeners
cl_note "routers: 0.0.0.0 on the server, each worker's address on the workers; hivemind ports as before"

nodes_json=$(docker exec "$S" hivemind nodes --json)
for ip in "${IPS[@]}"; do
	grep -q "\"address\": *\"$ip\"" <<<"$nodes_json" || cl_fail "hivemind nodes has no node at address $ip: $nodes_json"
done
cl_note "each node registered its private address"

# The app: two replicas of whoami, which answers with its host name, the
# name it is given with -name, and the request it got.
deploy() { # deploy VERSION
	docker exec -i "$S" hivemind deploy -f - >/dev/null <<EOF
name: who
image: $APP_IMAGE
replicas: 2
env:
  V: "$1"
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
}
deploy 1 || cl_fail "deploy who"
DEPLOYED_AT=$(date +%s)

# who_rows prints "index node generation state" for each replica.
who_rows() {
	docker exec "$S" hivemind status who | awk '$1 == "who" && $2 ~ /^[0-9]+$/ { print $2, $3, $4, $5 }'
}
on_two_nodes() { # on_two_nodes GEN
	local rows
	rows=$(who_rows)
	[ "$(awk -v g="$1" '$3 == g && $4 == "running"' <<<"$rows" | wc -l)" -eq 2 ] &&
		[ "$(awk -v g="$1" '$3 == g && $4 == "running" { print $2 }' <<<"$rows" | sort -u | wc -l)" -eq 2 ] &&
		! docker exec "$S" hivemind status who | grep -q "rolling out"
}
wait_for 120 on_two_nodes 1 || cl_fail "who did not run two replicas on two nodes: $(who_rows | tr '\n' ';')"
cl_note "who runs two replicas on two nodes"

for i in 0 1 2; do
	c=${CL_NAMES[$i]}
	ports=$(docker exec "$c" docker ps --filter label=hivemind.app=who --format '{{.Ports}}')
	[ -z "$ports" ] && continue
	grep -qE "^${IPS[$i]//./\\.}:[0-9]+->80/tcp$" <<<"$ports" ||
		cl_fail "a who replica on $c does not publish port 80 on ${IPS[$i]} alone: $ports"
done
cl_note "each replica publishes port 80 on an ephemeral port of its node's address only"

# code IP [PATH]: the status code of an https request through IP's router.
code() {
	docker exec "$CLIENT" curl -s -o /dev/null -w '%{http_code}' --max-time 10 --cacert /tmp/root.pem \
		--resolve "$HOST:$HTTPS_PORT:$1" "https://$HOST:$HTTPS_PORT${2:-/}" || true
}
docker exec "$CLIENT" sh -c "curl -sf --cacert /tmp/minica.pem https://pebble:15000/roots/0 >/tmp/root.pem" ||
	cl_fail "could not read Pebble's root"
served() { [ "$(code "$1")" = 200 ]; }
any_served() { served "$S_IP" || served "$W1_IP" || served "$W2_IP"; }
wait_for 120 any_served || cl_fail "no router served $HOST with a certificate 120 s after the deploy"
cl_note "the first router served $HOST $(($(date +%s) - DEPLOYED_AT)) s after the deploy"
# The other routers wait for the lock in turn, find the certificate in the
# shared storage, and load it.
for ip in "${IPS[@]}"; do
	wait_for 60 served "$ip" || cl_fail "the router on $ip does not serve $HOST: $(code "$ip")"
done
cl_note "all three routers served $HOST $(($(date +%s) - DEPLOYED_AT)) s after the deploy"

# One certificate, though every router asked: the storage lock let one
# router issue it, and the others found it in storage.
sleep 5
obtained=0
for c in "${CL_NAMES[@]}"; do
	n=$(docker exec "$c" grep -c 'certificate obtained successfully' /var/log/hivemind.log || true)
	obtained=$((obtained + n))
	docker exec "$c" grep -q 'managing certificates' /var/log/hivemind.log || cl_fail "$c's router did not manage $HOST"
done
issued=$(docker logs "$PEBBLE" 2>&1 | grep -c 'Issued certificate serial' || true)
[ "$obtained" -eq 1 ] || cl_fail "the routers obtained $obtained certificates, want 1"
[ "$issued" -eq 1 ] || cl_fail "Pebble issued $issued certificates, want 1"
cl_note "three routers asked for the certificate at once; Pebble issued 1, and every router serves it"

# Replies from both replicas through each router.
for ip in "${IPS[@]}"; do
	names=$(docker exec "$CLIENT" sh -c "for i in 1 2 3 4 5 6 7 8 9 10; do curl -s --max-time 10 --cacert /tmp/root.pem \
		--resolve $HOST:$HTTPS_PORT:$ip https://$HOST:$HTTPS_PORT/ | sed -n 's/^Hostname: //p'; done" | sort -u)
	[ "$(wc -l <<<"$names")" -eq 2 ] || cl_fail "the router on $ip answered from $(wc -l <<<"$names") replicas, want 2: $names"
done
cl_note "each of the three routers spreads requests over both replicas"

redir=$(docker exec "$CLIENT" curl -s -o /dev/null -w '%{http_code} %{redirect_url}' --resolve "$HOST:$HTTP_PORT:$W1_IP" \
	"http://$HOST:$HTTP_PORT/a?b=c")
[ "$redir" = "308 https://$HOST:$HTTPS_PORT/a?b=c" ] || cl_fail "http is not redirected to https: $redir"
unknown=$(docker exec "$CLIENT" curl -s -o /dev/null -w '%{http_code}' --resolve "nope.routing.test:$HTTP_PORT:$W2_IP" \
	"http://nope.routing.test:$HTTP_PORT/")
body=$(docker exec "$CLIENT" curl -s --resolve "nope.routing.test:$HTTP_PORT:$W2_IP" "http://nope.routing.test:$HTTP_PORT/")
[ "$unknown" = 404 ] && [ "$body" = "not found" ] || cl_fail "an unknown host got: $unknown $body"
cl_note "http redirects to https with 308; an unknown host gets a plain 404"

# What a replica receives. whoami echoes the request: the forwarding headers
# are the router's, none of the client's, and Host is the table's host
# whatever the client wrote.
CLIENT_IP=$(cl_ip_of "$CLIENT")
seen=$(docker exec "$CLIENT" curl -s --max-time 10 --http1.1 --cacert /tmp/root.pem --resolve "$HOST:$HTTPS_PORT:$W1_IP" \
	-H "Host: ${HOST^^}.:1234" -H "X-Forwarded-For: 6.6.6.6" -H "X-Forwarded-Port: 80" -H "X-Forwarded-Prefix: /admin" \
	-H "True-Client-IP: 6.6.6.6" -H "X-Real-IP: 6.6.6.6" -H "Forwarded: for=6.6.6.6" "https://$HOST:$HTTPS_PORT/" | tr -d '\r')
want_lines=("Host: $HOST:$HTTPS_PORT" "X-Forwarded-For: $CLIENT_IP" "X-Forwarded-Host: $HOST:$HTTPS_PORT"
	"X-Forwarded-Port: $HTTPS_PORT" "X-Forwarded-Proto: https")
for l in "${want_lines[@]}"; do
	grep -qxF "$l" <<<"$seen" || cl_fail "the replica did not get \"$l\": $(tr '\n' ';' <<<"$seen")"
done
if grep -qiE '6\.6\.6\.6|^(X-Forwarded-Prefix|True-Client-Ip|X-Real-Ip|Forwarded):' <<<"$seen"; then
	cl_fail "a forwarding header from the client reached the replica: $(tr '\n' ';' <<<"$seen")"
fi
# An h2c upgrade reaches the replica as plain HTTP/1.1.
seen=$(docker exec "$CLIENT" curl -s --max-time 10 --http1.1 --cacert /tmp/root.pem --resolve "$HOST:$HTTPS_PORT:$W2_IP" \
	-H "Connection: Upgrade, HTTP2-Settings" -H "Upgrade: h2c" -H "HTTP2-Settings: AAMAAABkAARAAAAAAAIAAAAA" \
	-w '%{http_code}\n' "https://$HOST:$HTTPS_PORT/" | tr -d '\r')
[ "$(tail -n 1 <<<"$seen")" = 200 ] && grep -q '^Hostname: ' <<<"$seen" ||
	cl_fail "an h2c upgrade request was not answered as HTTP/1.1: $(tr '\n' ';' <<<"$seen")"
if grep -qiE '^(Upgrade|Http2-Settings):' <<<"$seen"; then
	cl_fail "the h2c upgrade reached the replica: $(tr '\n' ';' <<<"$seen")"
fi
# Each router logs its connection caps; the per-address one is the flag's default.
for c in "${CL_NAMES[@]}"; do
	docker exec "$c" grep -q 'router listening.*max_conns=[1-9][0-9]* max_conns_per_ip=256' /var/log/hivemind.log ||
		cl_fail "$c's router did not log its connection caps: $(docker exec "$c" grep 'router listening' /var/log/hivemind.log)"
done
cl_note "replicas get the router's forwarding headers and the table's host, never the client's; h2c is not passed; caps logged"

# Every router's counters reach the server's /metrics, each under its node's
# ID: the server's own at once, the workers' through the node API every 15 s.
server_metrics() {
	docker exec "$S" sh -c 'wget -qO- --header "Authorization: Bearer $(cat /var/lib/hivemind/admin.token)" http://127.0.0.1:4483/metrics'
}
router_reported() { # router_reported NODE_ID: /metrics has 200s for $HOST from the node's router
	server_metrics | grep -qE "^hivemind_router_requests_total\{node=\"$1\",host=\"${HOST//./\\.}\",code=\"200\"\} [1-9]"
}
for i in 0 1 2; do
	wait_for 45 router_reported "${CL_IDS[$i]}" ||
		cl_fail "/metrics has no requests from ${CL_IDS[$i]}'s router: $(server_metrics | grep '^hivemind_router_requests_total' | tr '\n' ';')"
done
m=$(server_metrics)
grep -qE "^hivemind_router_requests_total\{node=\"${CL_IDS[2]}\",host=\"unknown\",code=\"404\"\} [1-9]" <<<"$m" ||
	cl_fail "/metrics does not have ${CL_IDS[2]}'s 404 for an unknown host: $(grep '^hivemind_router_requests_total' <<<"$m" | tr '\n' ';')"
for id in "${CL_IDS[@]}"; do
	grep -q "^hivemind_router_cert_expiry_timestamp_seconds{node=\"$id\",host=\"$HOST\"} " <<<"$m" ||
		cl_fail "/metrics has no certificate expiry from $id's router: $(grep '^hivemind_router_cert' <<<"$m" | tr '\n' ';')"
done
[ "$(grep -c '^# TYPE hivemind_router_requests_total ' <<<"$m")" -eq 1 ] || cl_fail "/metrics repeats the router request family"
cl_note "the server's /metrics has every router's requests and certificate, by node"

# count_loop starts requests through the given routers in the client, one
# after another, each code on a line of FILE, until FILE.stop exists.
count_loop() { # count_loop FILE IP...
	local f=$1
	shift
	docker exec -d "$CLIENT" sh -c "while [ ! -e $f.stop ]; do for ip in $*; do curl -s -o /dev/null -w '%{http_code}\n' \
		--max-time 15 --cacert /tmp/root.pem --resolve $HOST:$HTTPS_PORT:\$ip https://$HOST:$HTTPS_PORT/ >>$f; done; done"
}
stop_loop() { # stop_loop FILE: stops the loop and prints "total failed"
	docker exec "$CLIENT" touch "$1.stop"
	sleep 16
	docker exec "$CLIENT" sh -c "awk '{ n++ } \$1 != \"200\" { bad++ } END { print n+0, bad+0 }' $1"
}

# A rolling update, with requests through all three routers the whole time.
count_loop /tmp/rollout "${IPS[@]}"
sleep 3
deploy 2 || cl_fail "deploy who version 2"
ROLL_AT=$(date +%s)
wait_for 180 on_two_nodes 2 || cl_fail "the rollout did not finish: $(who_rows | tr '\n' ';')"
cl_note "rollout to generation 2 finished in $(($(date +%s) - ROLL_AT)) s"
sleep 10 # the old copies' drain delay, and their stop
read -r total bad <<<"$(stop_loop /tmp/rollout)"
[ "$total" -ge 30 ] || cl_fail "only $total requests during the rollout"
[ "$bad" -eq 0 ] || cl_fail "$bad of $total requests failed during the rollout: $(docker exec "$CLIENT" sort /tmp/rollout | uniq -c)"
cl_note "rolling update: $total requests through the three routers, $bad failed"
old=$(for c in "${CL_NAMES[@]}"; do docker exec "$c" docker ps -a --filter label=hivemind.app=who --filter label=hivemind.generation=1 -q; done)
[ -z "$old" ] || cl_fail "generation 1 containers are left: $old"

# Path routes: a second app, api, on the same host under /v1 (whoami answers
# /api itself, with JSON, so the prefix is /v1). Every
# router sends /v1 and what is under it to api, and every other path to
# who, as the client sent it; paths that could be read as under another
# prefix are refused with 400.
deploy_api() { # deploy_api VERSION
	docker exec -i "$S" hivemind deploy -f - >/dev/null <<EOF
name: api
image: $APP_IMAGE
replicas: 2
env:
  V: "$1"
command: [-name, api]
routes:
  - host: $HOST
    path: /v1
    port: 80
health:
  http:
    port: 80
    path: /health
  interval: 1s
  timeout: 1s
EOF
}
app_rows() { # app_rows APP: "index node generation state" for each replica
	docker exec "$S" hivemind status "$1" | awk -v a="$1" '$1 == a && $2 ~ /^[0-9]+$/ { print $2, $3, $4, $5 }'
}
rolled() { # rolled APP GEN: two replicas run at GEN, and no rollout is in flight
	[ "$(app_rows "$1" | awk -v g="$2" '$3 == g && $4 == "running"' | wc -l)" -eq 2 ] &&
		! docker exec "$S" hivemind status "$1" | grep -q "rolling out"
}
deploy_api 1 || cl_fail "deploy api"
wait_for 120 rolled api 1 || cl_fail "api did not run two replicas: $(app_rows api | tr '\n' ';')"
# name_at IP PATH: the -name of the replica that answered, or the code.
name_at() {
	local out
	out=$(docker exec "$CLIENT" curl -s --path-as-is --max-time 10 --cacert /tmp/root.pem --resolve "$HOST:$HTTPS_PORT:$1" \
		-w '\ncode=%{http_code}\n' "https://$HOST:$HTTPS_PORT$2" | tr -d '\r') || true
	if grep -q '^Name: ' <<<"$out"; then
		sed -n 's/^Name: //p' <<<"$out"
	else
		sed -n 's/^code=//p' <<<"$out"
	fi
}
api_served() { [ "$(name_at "$1" /v1)" = api ]; }
for ip in "${IPS[@]}"; do
	wait_for 30 api_served "$ip" || cl_fail "the router on $ip does not send /v1 to api: $(name_at "$ip" /v1)"
done
for ip in "${IPS[@]}"; do
	# No "//" here: the router passes it on as it is, and whoami's own mux
	# answers it with a 301 to the clean path. The unit tests cover it.
	for x in "/ who" "/index.html who" "/v1 api" "/v1/ api" "/v1/users/7 api" "/v1x who" "/V1/x who" "/v1/x/ api" \
		"/v1/../x 400" "/x/../v1/y 400" "/v1/%2e%2e/x 400" "/v1%2Fx 400" "/v1/..;/x 400" "/v1%5C..%5Cx 400"; do
		p=${x% *} want=${x#* }
		got=$(name_at "$ip" "$p")
		[ "$got" = "$want" ] || cl_fail "the router on $ip sent $p to \"$got\", want $want"
	done
done
# api gets the request as the client sent it: no prefix is stripped.
seen=$(docker exec "$CLIENT" curl -s --max-time 10 --cacert /tmp/root.pem --resolve "$HOST:$HTTPS_PORT:$W2_IP" \
	"https://$HOST:$HTTPS_PORT/v1/users/7?q=a%2Fb" | tr -d '\r')
grep -qxF "GET /v1/users/7?q=a%2Fb HTTP/1.1" <<<"$seen" || cl_fail "api did not get the path as sent: $(tr '\n' ';' <<<"$seen")"
routes_out=$(docker exec "$S" hivemind routes 2>&1)
grep -qE "^$HOST +/ +who +80 +[0-9.:]+,[0-9.:]+$" <<<"$routes_out" && grep -qE "^$HOST +/v1 +api +80 +[0-9.:]+,[0-9.:]+$" <<<"$routes_out" ||
	cl_fail "hivemind routes does not show both routes with two endpoints each: $(tr '\n' ';' <<<"$routes_out")"
if grep -q "before path routes" <<<"$routes_out"; then
	cl_fail "hivemind routes warns about a router of this build: $routes_out"
fi
cl_note "path routes: through each of the three routers, /v1 goes to api and the rest to who, unchanged; 6 kinds of path refused with 400"

# A rolling update of each app in turn, with requests for / and for /v1/x
# through all three routers the whole time: each must be a 200 from the app
# that owns the path.
docker exec -i "$CLIENT" sh -c 'cat >/tmp/paths.sh' <<'EOF'
#!/bin/sh
# paths.sh FILE HOST PORT IP...: until FILE.stop exists, requests / and
# /v1/x through each router in turn, and appends "PATH CODE NAME" for each.
f=$1 host=$2 port=$3
shift 3
while [ ! -e "$f.stop" ]; do
	for ip in "$@"; do
		for p in / /v1/x; do
			out=$(curl -s --max-time 15 --cacert /tmp/root.pem --resolve "$host:$port:$ip" -w '\ncode=%{http_code}\n' "https://$host:$port$p" | tr -d '\r')
			code=$(printf '%s\n' "$out" | sed -n 's/^code=//p')
			name=$(printf '%s\n' "$out" | sed -n 's/^Name: //p')
			echo "$p ${code:-none} ${name:--}" >>"$f"
		done
	done
done
EOF
docker exec -d "$CLIENT" sh /tmp/paths.sh /tmp/paths "$HOST" "$HTTPS_PORT" "${IPS[@]}"
sleep 3
deploy_api 2 || cl_fail "deploy api version 2"
ROLL_AT=$(date +%s)
wait_for 180 rolled api 2 || cl_fail "the rollout of api did not finish: $(app_rows api | tr '\n' ';')"
cl_note "rollout of api to generation 2 finished in $(($(date +%s) - ROLL_AT)) s"
sleep 10
deploy 3 || cl_fail "deploy who version 3"
ROLL_AT=$(date +%s)
wait_for 180 on_two_nodes 3 || cl_fail "the rollout of who did not finish: $(who_rows | tr '\n' ';')"
cl_note "rollout of who to generation 3 finished in $(($(date +%s) - ROLL_AT)) s"
sleep 10
docker exec "$CLIENT" touch /tmp/paths.stop
sleep 16
read -r total bad <<<"$(docker exec "$CLIENT" awk '{ n++ } !(($1 == "/" && $2 == "200" && $3 == "who") || ($1 == "/v1/x" && $2 == "200" && $3 == "api")) { bad++ } END { print n+0, bad+0 }' /tmp/paths)"
[ "$total" -ge 60 ] || cl_fail "only $total requests during the two rollouts"
[ "$bad" -eq 0 ] || cl_fail "$bad of $total requests failed or reached the wrong app during the rollouts: $(docker exec "$CLIENT" sort /tmp/paths | uniq -c | tr '\n' ';')"
cl_note "rolling updates of api, then who: $total requests for / and /v1/x through the three routers, $bad failed or misrouted"

# Kill a worker that runs a replica. The other two routers keep answering.
victim=""
rows=$(who_rows)
for i in 1 2; do
	if awk -v n="${CL_IDS[$i]}" '$2 == n && $4 == "running"' <<<"$rows" | grep -q .; then
		victim=$i
		break
	fi
done
[ -n "$victim" ] || cl_fail "no worker runs a who replica: $rows"
VC=${CL_NAMES[$victim]}
SURVIVORS=()
for i in 0 1 2; do
	[ "$i" = "$victim" ] || SURVIVORS+=("${IPS[$i]}")
done
count_loop /tmp/kill "${SURVIVORS[@]}"
sleep 3
docker kill "$VC" >/dev/null
KILLED_AT=$(date +%s)
cl_note "killed $VC (${CL_IDS[$victim]}) at $(date -u +%T)"
replaced() { # two replicas run, none on the killed node
	local rows
	rows=$(who_rows)
	[ "$(awk '$4 == "running"' <<<"$rows" | wc -l)" -eq 2 ] &&
		! awk -v n="${CL_IDS[$victim]}" '$2 == n' <<<"$rows" | grep -q .
}
wait_for 120 replaced || cl_fail "the replica on $VC was not placed again: $(who_rows | tr '\n' ';')"
cl_note "the replica moved off $VC $(($(date +%s) - KILLED_AT)) s after the kill"
sleep 15
read -r total bad <<<"$(stop_loop /tmp/kill)"
[ "$total" -ge 20 ] || cl_fail "only $total requests after the kill"
[ "$bad" -eq 0 ] || cl_fail "$bad of $total requests failed after the kill: $(docker exec "$CLIENT" sort /tmp/kill | uniq -c)"
cl_note "node loss: $total requests through the two other routers, $bad failed"

cl_note "PASS"
