#!/usr/bin/env bash
# raft-rate.sh REPO [SECONDS [LABEL]]: the Raft entries a steady cluster of three
# servers and WORKERS workers (2 by default) appends, built from the tree at
# REPO, which may be another release's source, as .scratch/hivemind-v2.3.0-src
# (scripts/integration-haops.sh). 50 apps of
# 2 replicas that no node can take (64 CPUs each) wait as pending, as the
# eighth soak's write-load's did; nothing writes through the API in the
# window. Prints, for each server, the entries and bytes appended in the
# window (SECONDS, 300 by default) and per minute. LABEL names the run in
# the output. Needs what scripts/integration-ha.sh needs, and the network
# for the nodes' apk add of sqlite, which reads raft.db.
set -euo pipefail
REPO=$(cd "$1" && pwd)
WINDOW=${2:-300}
LABEL=${3:-$(basename "$REPO")}
# shellcheck source=/dev/null
. "$REPO/scripts/lib/cluster.sh"
cl_init raftrate
WORKERS=${WORKERS:-2}
for _ in $(seq 1 $((3 + WORKERS))); do cl_add_node; done
cl_trap_cleanup
S=("${CL_NAMES[0]}" "${CL_NAMES[1]}" "${CL_NAMES[2]}")
cl_start_nodes
IP=()
for i in 0 1 2; do IP[$i]=$(cl_ip_of "${S[$i]}"); done
serve() { docker exec -d -e HIVEMIND_MIN_READY=0s "${S[$1]}" sh -c "exec hivemind serve --advertise ${IP[$1]} >>/var/log/hivemind.log 2>&1"; }
docker exec "${S[0]}" hivemind init >/dev/null
serve 0
wait_for 30 docker exec "${S[0]}" hivemind status || cl_fail "s0's API"
TOKEN=$(docker exec "${S[0]}" cat /var/lib/hivemind/admin.token)
hm() { local i=$1; shift; docker exec -i -e "HIVEMIND_TOKEN=$TOKEN" "${S[$i]}" hivemind "$@"; }
voters() { [ "$(hm 0 servers | awk '$1 ~ /^node-/ && ($4 == "leader" || $4 == "follower")' | wc -l)" -eq "$1" ]; }
for i in 1 2; do
	tok=$(hm 0 join-token --role server --ttl 10m 2>/dev/null)
	docker exec -d -e "HM_TOKEN=$tok" -e HIVEMIND_MIN_READY=0s "${S[$i]}" \
		sh -c "exec hivemind join ${IP[0]}:4484 --role server --token \"\$HM_TOKEN\" --advertise ${IP[$i]} >>/var/log/hivemind.log 2>&1"
	wait_for 120 voters $((i + 1)) || cl_fail "${S[$i]} is not a voter: $(hm 0 servers)"
done
for c in "${CL_NAMES[@]:3}"; do
	tok=$(hm 0 join-token --ttl 1h 2>/dev/null)
	docker exec -d -e "HM_TOKEN=$tok" "$c" sh -c "exec hivemind join ${IP[0]}:4484 --token \"\$HM_TOKEN\" >>/var/log/hivemind.log 2>&1"
done
nodes_ready() { [ "$(hm 0 nodes | grep -c ' ready ')" -eq $((3 + WORKERS)) ]; }
wait_for 120 nodes_ready || cl_fail "not every node is ready: $(hm 0 nodes)"
for n in $(seq 1 50); do
	printf 'name: big%d\nimage: nginx:alpine\nreplicas: 2\nresources:\n  cpu: 64\n' "$n" | hm 0 deploy -f - >/dev/null
done
cl_note "50 apps of 2 replicas that no node can take; settling for 60 s"
sleep 60
for i in 0 1 2; do docker exec "${S[$i]}" apk add -q --no-cache sqlite >/dev/null; done
q() { docker exec "${S[$1]}" sqlite3 /var/lib/hivemind/raft/raft.db "$2"; }
declare -a I0
for i in 0 1 2; do
	I0[$i]=$(q "$i" 'SELECT max(idx) FROM logs')
done
sleep "$WINDOW"
for i in 0 1 2; do
	i1=$(q "$i" 'SELECT max(idx) FROM logs')
	b=$(q "$i" "SELECT coalesce(sum(length(data)), 0) FROM logs WHERE idx > ${I0[$i]}")
	n=$((i1 - I0[i]))
	cl_note "$LABEL ${S[$i]}: $n entries, $b bytes in ${WINDOW} s: $((n * 60 / WINDOW)) entries and $((b * 60 / WINDOW / 1024)) KiB a minute"
done
hm 0 status | tail -n 3 || true
