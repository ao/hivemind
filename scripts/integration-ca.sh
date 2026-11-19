#!/usr/bin/env bash
# Rotates the cluster CA of a three-node cluster in Docker, four times, with
# a routed app serving through a router on every node.
#
# Three docker:dind nodes on one private network, as in
# scripts/integration-routing.sh (helpers in scripts/lib/cluster.sh), with
# Pebble as the ACME CA, pebble-challtestsrv as its DNS, and a curl container
# as the client. Each node runs its router on its own address. No container
# publishes a port.
#
#   1. A whole rotation, start, renew and finish, while requests go through
#      all three routers the whole time: every one is a 200; no worker joins
#      again (node.json and node IDs unchanged); each worker logs the new
#      CA's fingerprint as hivemind ca status shows it; the server's
#      certificate moves only once every node trusts the new CA; at the end
#      ca.crt is the new CA on every node, ca-next.* is gone from the server,
#      and the listeners are where they were.
#   2. A worker offline during a rotation: renew goes ahead without it once
#      its lease has lapsed; finish refuses, naming it as down; finish
#      --force cuts it off; started again, it says the CA was rotated without
#      it; after hivemind node remove it joins again with an empty data
#      directory and serves through its router.
#   3. An abort in the renew phase, after a worker has renewed: the rotation
#      turns round, every node renews back, finish deletes the new CA, and
#      ca.crt is the CA from before on every node. Requests through all three
#      routers all get 200s.
#   4. A restore of a backup taken in the renew phase: serve stops, the data
#      directory is restored from the backup, serve starts at a higher epoch,
#      the workers come back without joining, the restored server is in the
#      renew phase with both CAs, and finish ends the rotation. Requests
#      through the workers' routers all get 200s across the restore.
#
# On exit it removes the containers, the network and the volumes it created,
# also after a failure or Ctrl-C. Needs Docker, jq, and the docker:dind,
# golang, Pebble, pebble-challtestsrv, curl and traefik/whoami images.
set -euo pipefail

REPO=$(cd "$(dirname "$0")/.." && pwd)
# shellcheck source=lib/cluster.sh
. "$REPO/scripts/lib/cluster.sh"

PEBBLE_IMAGE=${PEBBLE_IMAGE:-ghcr.io/letsencrypt/pebble:latest}
CHALL_IMAGE=${CHALL_IMAGE:-ghcr.io/letsencrypt/pebble-challtestsrv:latest}
CURL_IMAGE=${CURL_IMAGE:-curlimages/curl:8.11.1}
APP_IMAGE=${APP_IMAGE:-traefik/whoami:v1.10.3}
HOST=www.carotate.test
HTTP_PORT=5002 # Pebble's httpPort: where it checks HTTP-01 answers
HTTPS_PORT=8443
EMAIL=ops@carotate.test
ACME_CA=https://pebble:14000/dir
DATA=/var/lib/hivemind

command -v jq >/dev/null || {
	echo "carotate: needs jq" >&2
	exit 1
}

cl_init carotate
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

ca_cleanup() {
	local rc=$? c
	set +e
	for c in "${EXTRA[@]}"; do
		docker rm -fv "$c" >/dev/null 2>&1 || true
	done
	(exit "$rc")
	cl_cleanup
}
trap ca_cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

cl_start_nodes
S_IP=$(cl_ip_of "$S")
W1_IP=$(cl_ip_of "$W1")
W2_IP=$(cl_ip_of "$W2")
IPS=("$S_IP" "$W1_IP" "$W2_IP")
cl_note "addresses: server $S_IP, $W1 $W1_IP, $W2 $W2_IP"

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

route_flags() { # route_flags ADDR
	echo "--route-addr $1 --route-http-port $HTTP_PORT --route-https-port $HTTPS_PORT --acme-email $EMAIL --acme-ca $ACME_CA"
}
serve_server() {
	docker exec -d -e SSL_CERT_FILE=/etc/pebble-minica.pem "$S" sh -c \
		"exec hivemind serve --advertise $S_IP --min-ready 0s $(route_flags "$S_IP") >>/var/log/hivemind.log 2>&1"
	wait_for 30 docker exec "$S" hivemind status || cl_fail "the API did not answer"
}
join_worker() { # join_worker INDEX
	local c=${CL_NAMES[$1]} tok
	tok=$(docker exec "$S" hivemind join-token --ttl 1h 2>/dev/null) || cl_fail "join-token failed"
	docker exec -d -e "HM_TOKEN=$tok" -e SSL_CERT_FILE=/etc/pebble-minica.pem "$c" sh -c \
		"exec hivemind join $S_IP:4484 --token \"\$HM_TOKEN\" $(route_flags "${IPS[$1]}") >>/var/log/hivemind.log 2>&1"
	wait_for 60 docker exec "$c" test -s "$DATA/node.json" || cl_fail "$c did not join"
	CL_IDS[$1]=$(cl_node_id_of "$c")
}
serve_worker() { # serve_worker INDEX
	local c=${CL_NAMES[$1]}
	docker exec -d -e SSL_CERT_FILE=/etc/pebble-minica.pem "$c" sh -c \
		"exec hivemind serve --role worker $(route_flags "${IPS[$1]}") >>/var/log/hivemind.log 2>&1"
}
hm_pids() { # hm_pids NODE: the PIDs of hivemind processes in NODE
	docker exec "$1" sh -c 'for p in /proc/[0-9]*; do [ "$(cat $p/comm 2>/dev/null)" = hivemind ] && echo ${p#/proc/}; done; true'
}
hm_stopped() { [ -z "$(hm_pids "$1")" ]; }
stop_hivemind() { # stop_hivemind NODE: SIGTERM, then SIGKILL after 30 s
	local p
	for p in $(hm_pids "$1"); do docker exec "$1" kill -TERM "$p" 2>/dev/null || true; done
	if ! wait_for 30 hm_stopped "$1"; then
		for p in $(hm_pids "$1"); do docker exec "$1" kill -KILL "$p" 2>/dev/null || true; done
		wait_for 10 hm_stopped "$1" || cl_fail "hivemind did not stop on $1"
	fi
}
epoch_of_server() { # the epoch the server's last start logged
	docker exec "$S" grep 'cluster listening' /var/log/hivemind.log | tail -n 1 | grep -o ' epoch=[0-9]*' | cut -d= -f2
}

CL_S_IP=$S_IP
CL_S_ID=$(docker exec "$S" hivemind init | sed -n 's/.*(node \(node-[0-9a-f]*\)).*/\1/p')
[ -n "$CL_S_ID" ] || cl_fail "init printed no node ID"
CL_IDS[0]=$CL_S_ID
serve_server
join_worker 1
join_worker 2
cl_note "joined: ${CL_IDS[*]}"

docker exec -i "$S" hivemind deploy -f - >/dev/null <<EOF || cl_fail "deploy who"
name: who
image: $APP_IMAGE
replicas: 3
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

code() { # code IP: the status of one https request through IP's router
	docker exec "$CLIENT" curl -s -o /dev/null -w '%{http_code}' --max-time 10 --cacert /tmp/root.pem \
		--resolve "$HOST:$HTTPS_PORT:$1" "https://$HOST:$HTTPS_PORT/" || true
}
docker exec "$CLIENT" sh -c "curl -sf --cacert /tmp/minica.pem https://pebble:15000/roots/0 >/tmp/root.pem" || cl_fail "could not read Pebble's root"
served() { [ "$(code "$1")" = 200 ]; }
for ip in "${IPS[@]}"; do
	wait_for 180 served "$ip" || cl_fail "the router on $ip does not serve $HOST: $(code "$ip")"
done
cl_note "who is served through all three routers"

count_loop() { # count_loop FILE IP...: requests through the routers until FILE.stop
	local f=$1
	shift
	docker exec "$CLIENT" rm -f "$f" "$f.stop"
	docker exec -d "$CLIENT" sh -c "while [ ! -e $f.stop ]; do for ip in $*; do curl -s -o /dev/null -w '%{http_code}\n' \
		--max-time 15 --cacert /tmp/root.pem --resolve $HOST:$HTTPS_PORT:\$ip https://$HOST:$HTTPS_PORT/ >>$f; done; sleep 0.2; done"
}
stop_loop() { # stop_loop FILE: stops the loop and prints "total failed"
	docker exec "$CLIENT" touch "$1.stop"
	sleep 16
	docker exec "$CLIENT" sh -c "awk '{ n++ } \$1 != \"200\" { bad++ } END { print n+0, bad+0 }' $1"
}
check_loop() { # check_loop FILE WHAT MIN
	local total bad
	read -r total bad <<<"$(stop_loop "$1")"
	[ "$total" -ge "$3" ] || cl_fail "only $total requests during $2"
	[ "$bad" -eq 0 ] || cl_fail "$bad of $total requests failed during $2: $(docker exec "$CLIENT" sort "$1" | uniq -c | tr '\n' ';')"
	cl_note "$2: $total requests through the routers, none failed"
}

ca_json() { docker exec "$S" hivemind ca status --json; }
ca_q() { ca_json | jq -r "$1"; }
waiting_none() { [ "$(ca_q '.rotation.waiting | length')" = 0 ]; }
listener_moved() { [ "$(ca_q '.rotation.listener_moved')" = true ]; }
renewed_count() { ca_q '[.nodes[] | select(.renewed)] | length'; }
some_renewed() { [ "$(renewed_count)" -ge 1 ]; }
fsum() { docker exec "$1" sha256sum "$2" | awk '{ print $1 }'; }
pem_count() { docker exec "$1" grep -c 'BEGIN CERTIFICATE' "$2"; }
ready_nodes() { # ready_nodes ID...: every node is ready in hivemind nodes
	local id rows
	rows=$(docker exec "$S" hivemind nodes)
	for id in "$@"; do
		awk -v id="$id" '$1 == id && / ready /' <<<"$rows" | grep -q . || return 1
	done
}
rotate() { # rotate STEP [--force]: one step; fails the run if it fails
	local out
	out=$(docker exec "$S" hivemind ca rotate "$@" 2>&1) || cl_fail "hivemind ca rotate $*: $out"
}
# one_ca SUM: ca.crt is one CA with that SHA-256 on every node, and the
# server has no ca-next files. It prints what differs, and fails, if not.
one_ca() {
	local c i
	for ((i = 0; i < 3; i++)); do
		c=${CL_NAMES[$i]}
		if [ "$(pem_count "$c" "$DATA/ca.crt")" != 1 ] || [ "$(fsum "$c" "$DATA/ca.crt")" != "$1" ]; then
			echo "$c's ca.crt has $(pem_count "$c" "$DATA/ca.crt") CAs, SHA-256 $(fsum "$c" "$DATA/ca.crt"), want one, $1"
			return 1
		fi
	done
	if [ -n "$(docker exec "$S" sh -c "ls $DATA/ca-next.* 2>/dev/null; true")" ]; then
		echo "ca-next files are left on the server"
		return 1
	fi
}
# wait_one_ca SUM: waits up to 60 s for one_ca SUM, and fails the run with
# what differs.
wait_one_ca() {
	wait_for 60 one_ca "$1" || cl_fail "$(one_ca "$1")"
}
workers_trust_both() { # every worker's ca.crt has two CAs
	local c
	for c in "$W1" "$W2"; do
		[ "$(pem_count "$c" "$DATA/ca.crt")" = 2 ] || return 1
	done
}

# 1. A whole rotation under load.
CA0=$(fsum "$S" "$DATA/ca.crt")
declare -a STATE0
for i in 1 2; do STATE0[$i]=$(fsum "${CL_NAMES[$i]}" "$DATA/node.json"); done
count_loop /tmp/rot1 "${IPS[@]}"
sleep 3
rotate start
NEXT_FP=$(ca_q '.next.fingerprint')
[ "$(ca_q '.rotation.phase')" = trust ] || cl_fail "after start: $(ca_json)"
wait_for 90 waiting_none || cl_fail "the workers did not trust the new CA: $(ca_json)"
wait_for 30 workers_trust_both || cl_fail "a worker's ca.crt does not have both CAs"
for c in "$W1" "$W2"; do
	docker exec "$c" grep -q "trusts a new cluster CA.*ca=$NEXT_FP" /var/log/hivemind.log ||
		cl_fail "$c did not log the new CA's fingerprint $NEXT_FP"
done
[ "$(ca_q '.rotation.listener_moved')" = false ] || cl_fail "the server's certificate moved in phase trust"
cl_note "start: both workers trust the new CA ${NEXT_FP:0:16}, and logged its fingerprint"
rotate renew
wait_for 120 waiting_none || cl_fail "the workers did not renew: $(ca_json)"
wait_for 60 listener_moved || cl_fail "the server's certificate did not move: $(ca_json)"
for c in "$W1" "$W2"; do
	docker exec "$c" grep -q "renewing the node certificate under the CA the cluster's CA rotation goes to.* ca=${NEXT_FP:0:16} " /var/log/hivemind.log ||
		cl_fail "$c did not renew under the new CA, or its line does not name it: $(docker exec "$c" grep 'renewing the node certificate' /var/log/hivemind.log)"
done
for p in "a node trusts the CA the rotation goes to, the new one in ca-next.crt" "a node presents a certificate from the CA the rotation goes to, the new one in ca-next.crt"; do
	[ "$(docker exec "$S" grep -c "$p.* ca=${NEXT_FP:0:16} " /var/log/hivemind.log)" -ge 2 ] ||
		cl_fail "the server did not log, for both workers, \"$p\" with ca=${NEXT_FP:0:16}: $(docker exec "$S" grep 'a node ' /var/log/hivemind.log | tail -n 4)"
done
cl_note "renew: both workers renewed under the new CA, then the server's certificate moved"
rotate finish
[ "$(ca_q '.rotation')" = null ] || cl_fail "a rotation after finish: $(ca_json)"
[ "$(ca_q '.current.fingerprint')" = "$NEXT_FP" ] || cl_fail "ca.crt is not the new CA after finish"
CA1=$(fsum "$S" "$DATA/ca.crt")
[ "$CA1" != "$CA0" ] || cl_fail "ca.crt did not change"
wait_one_ca "$CA1"
sleep 5
check_loop /tmp/rot1 "the rotation" 60
for i in 1 2; do
	[ "$(fsum "${CL_NAMES[$i]}" "$DATA/node.json")" = "${STATE0[$i]}" ] || cl_fail "${CL_NAMES[$i]} joined again: node.json changed"
	[ "$(cl_node_id_of "${CL_NAMES[$i]}")" = "${CL_IDS[$i]}" ] || cl_fail "${CL_NAMES[$i]} has another node ID"
	[ "$(docker exec "${CL_NAMES[$i]}" grep -c 'joined the cluster' /var/log/hivemind.log)" = 1 ] || cl_fail "${CL_NAMES[$i]} joined twice"
done
ready_nodes "${CL_IDS[@]}" || cl_fail "a node is not ready after the rotation: $(docker exec "$S" hivemind nodes | tr '\n' ';')"
cl_check_server_listeners
cl_check_worker_listeners
cl_note "rotation 1: every node has the new CA alone, nobody joined again, the listeners are as before"

# 2. A worker offline during a rotation.
OFF_ID=${CL_IDS[2]}
stop_hivemind "$W2"
lost() { docker exec "$S" hivemind nodes | awk -v id="$OFF_ID" '$1 == id' | grep -q ' lost '; }
rotate start
wait_for 60 lost || cl_fail "$W2 is not lost: $(docker exec "$S" hivemind nodes | tr '\n' ';')"
wait_for 90 waiting_none || cl_fail "renew waits for more than the live nodes: $(ca_json)"
rotate renew
wait_for 120 some_renewed || cl_fail "$W1 did not renew: $(ca_json)"
[ "$(ca_q '.rotation.listener_moved')" = false ] || cl_fail "the server's certificate moved while $W2, which does not trust the new CA, is in the cluster"
if out=$(docker exec "$S" hivemind ca rotate finish 2>&1); then
	cl_fail "finish went ahead with $W2 offline: $out"
fi
grep -q "$OFF_ID" <<<"$out" && grep -q "is down" <<<"$out" && grep -q -- "--force" <<<"$out" ||
	cl_fail "finish's refusal does not name $OFF_ID as down: $out"
cl_note "finish refused, naming $OFF_ID as down"
rotate finish --force
CA2=$(fsum "$S" "$DATA/ca.crt")
serve_worker 2
wait_for 60 docker exec "$W2" grep -q "rotated without this node" /var/log/hivemind.log ||
	cl_fail "$W2 did not say the CA was rotated without it: $(docker exec "$W2" tail -n 5 /var/log/hivemind.log)"
stop_hivemind "$W2"
cl_note "finish --force cut $W2 off; started again, it says the CA was rotated without it"
docker exec "$S" hivemind node remove "$OFF_ID" >/dev/null || cl_fail "node remove $OFF_ID"
docker exec "$W2" sh -c "docker rm -f \$(docker ps -aq --filter label=hivemind.app) >/dev/null 2>&1; rm -rf $DATA; true"
join_worker 2
[ "${CL_IDS[2]}" != "$OFF_ID" ] || cl_fail "$W2 joined again with its old ID"
wait_for 60 ready_nodes "${CL_IDS[2]}" || cl_fail "the rejoined $W2 is not ready"
wait_for 180 served "$W2_IP" || cl_fail "the rejoined $W2's router does not serve $HOST"
wait_one_ca "$CA2"
cl_note "rotation 2: $W2 joined again as ${CL_IDS[2]}, and serves through its router"

# 3. An abort in the renew phase.
count_loop /tmp/rot3 "${IPS[@]}"
sleep 3
rotate start
wait_for 90 waiting_none || cl_fail "the workers did not trust the third new CA: $(ca_json)"
rotate renew
wait_for 120 some_renewed || cl_fail "no worker renewed: $(ca_json)"
rotate abort
[ "$(ca_q '.rotation.toward')" = current ] || cl_fail "abort in the renew phase did not turn the rotation round: $(ca_json)"
wait_for 120 waiting_none || cl_fail "the workers did not renew back: $(ca_json)"
# The lines on both sides name the CA the rotation goes back to as that,
# by its fingerprint. In the fifth soak they called it the new CA.
TO3=$(ca_q '.rotation.to')
FROM3=$(ca_q '.rotation.from')
for c in "$W1" "$W2"; do
	# A worker that had not renewed under the new CA before the abort has
	# nothing to renew back.
	docker exec "$c" grep -q "renewing the node certificate under the CA the cluster's CA rotation goes to.* ca=${FROM3:0:16} " /var/log/hivemind.log || continue
	docker exec "$c" grep -q "renewing the node certificate under the CA the cluster's CA rotation goes to.* ca=${TO3:0:16} from=${FROM3:0:16}" /var/log/hivemind.log ||
		cl_fail "$c did not log its renewal back with ca=${TO3:0:16} from=${FROM3:0:16}: $(docker exec "$c" grep 'renewing the node certificate' /var/log/hivemind.log | tail -n 2)"
done
for p in "a node trusts the CA the rotation goes back to, the old one in ca.crt, after an abort" \
	"a node presents a certificate from the CA the rotation goes back to, the old one in ca.crt, after an abort"; do
	[ "$(docker exec "$S" grep -c "$p.* ca=${TO3:0:16} leaving=${FROM3:0:16}" /var/log/hivemind.log)" -ge 2 ] ||
		cl_fail "the server did not log, for both workers, \"$p\" with ca=${TO3:0:16}: $(docker exec "$S" grep 'a node ' /var/log/hivemind.log | tail -n 4)"
done
cl_note "abort: both sides named ${TO3:0:16} as the CA the rotation goes back to"
rotate finish
[ "$(fsum "$S" "$DATA/ca.crt")" = "$CA2" ] || cl_fail "after the abort, ca.crt is not the CA from before"
wait_one_ca "$CA2"
sleep 5
check_loop /tmp/rot3 "the aborted rotation" 40
cl_note "rotation 3: aborted in the renew phase, every node renewed back, and the cluster has its CA from before"

# 4. A restore of a backup from the renew phase.
EPOCH0=$(epoch_of_server)
declare -a STATE4
for i in 1 2; do STATE4[$i]=$(fsum "${CL_NAMES[$i]}" "$DATA/node.json"); done
rotate start
wait_for 90 waiting_none || cl_fail "the workers did not trust the fourth new CA: $(ca_json)"
rotate renew
wait_for 120 waiting_none || cl_fail "the workers did not renew: $(ca_json)"
wait_for 60 listener_moved || cl_fail "the server's certificate did not move: $(ca_json)"
NEXT4=$(ca_q '.next.fingerprint')
docker exec "$S" hivemind backup /root/mid.tar >/dev/null || cl_fail "hivemind backup during the rotation"
docker exec "$S" tar -tf /root/mid.tar | grep -x ca-next.key >/dev/null || cl_fail "the backup has no ca-next.key"
count_loop /tmp/rot4 "$W1_IP" "$W2_IP"
sleep 3
stop_hivemind "$S"
docker exec "$S" mv "$DATA" "$DATA.before-restore"
out=$(docker exec "$S" hivemind restore /root/mid.tar --data-dir "$DATA" 2>&1) || cl_fail "hivemind restore: $out"
grep -q "during a rotation of the cluster CA (phase renew)" <<<"$out" || cl_fail "restore did not say the backup was taken during a rotation: $out"
serve_server
wait_for 90 ready_nodes "${CL_IDS[1]}" "${CL_IDS[2]}" || cl_fail "the workers did not come back to the restored server"
EPOCH1=$(epoch_of_server)
[ "$EPOCH1" -gt "${EPOCH0:-0}" ] || cl_fail "the restored server's epoch $EPOCH1 is not above $EPOCH0"
[ "$(ca_q '.rotation.phase')" = renew ] && [ "$(ca_q '.next.fingerprint')" = "$NEXT4" ] ||
	cl_fail "the restored server is not in the rotation's renew phase: $(ca_json)"
wait_for 90 waiting_none || cl_fail "the restored server waits for nodes: $(ca_json)"
rotate finish
CA4=$(fsum "$S" "$DATA/ca.crt")
[ "$(ca_q '.current.fingerprint')" = "$NEXT4" ] || cl_fail "after the restore and finish, ca.crt is not the rotation's new CA"
wait_one_ca "$CA4"
for i in 1 2; do
	[ "$(fsum "${CL_NAMES[$i]}" "$DATA/node.json")" = "${STATE4[$i]}" ] || cl_fail "${CL_NAMES[$i]} joined again after the restore"
done
wait_for 120 served "$S_IP" || cl_fail "the restored server's router does not serve $HOST"
check_loop /tmp/rot4 "the restore" 40
cl_check_server_listeners
cl_note "rotation 4: a backup from the renew phase restored at epoch $EPOCH1, the workers came back without joining, and finish ended it"
cl_note "PASS"
