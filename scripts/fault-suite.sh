#!/usr/bin/env bash
# Fault suite: a hivemind cluster in Docker, with faults injected into it.
#
#   NODES=3 ./scripts/fault-suite.sh   (default)
#   NODES=5 ./scripts/fault-suite.sh
#
# NODES is the total number of nodes: one server and NODES-1 workers, each a
# docker:dind container on one private network, as in integration-cluster.sh.
# The workloads are tiny: busybox httpd on host port 8080 (app web, NODES+1
# replicas, so one replica stays pending on the port) and busybox sleep (app
# spread, NODES replicas).
#
# The scenarios run one after another on one cluster. Each starts from a
# converged cluster and ends with the convergence check:
#
#   kill-worker   docker kill a worker. Its spread replica runs elsewhere within
#                 30 s. The worker starts again with its data directory; the
#                 cluster converges with no duplicates.
#   kill-server   kill -9 the hivemind serve process on the server. Containers
#                 on every node keep their IDs and StartedAt, and nothing new
#                 appears while the server is down. After the restart, nothing
#                 restarts and the cluster converges.
#   partition     iptables DROP of the worker's traffic to the server's cluster
#                 port. Its replicas move after the lease expires; the worker's
#                 containers keep running. After the heal the worker removes the
#                 containers that moved, and the cluster converges.
#   slow          tc netem adds 2000 ms of delay to a worker. For 60 s no node
#                 is lost and no replica moves.
#   flap          10 partition/heal cycles in 120 s on a worker. Then the new
#                 replicas placed in the 30 s after the last heal must avoid the
#                 flapping node (MinReady).
#
# Convergence (see converged): every replica is either running on the node the
# server assigns it, with that container running, or pending with a placement
# reason; the server's status names the same container IDs the engines report;
# no container is left over; no (app, replica) pair is on two nodes.
#
# It publishes no ports. On exit, also after a failure or Ctrl-C, it removes
# its containers, network and volumes. The Go build cache (hivemind-cache) is
# kept.
set -euo pipefail

NODES=${NODES:-3}
case "$NODES" in
3 | 5) ;;
*)
	echo "fault-suite: NODES must be 3 or 5, got '$NODES'" >&2
	exit 2
	;;
esac

REPO=$(cd "$(dirname "$0")/.." && pwd)
# shellcheck source=lib/cluster.sh
. "$REPO/scripts/lib/cluster.sh"

WEB_IMAGE=${WEB_IMAGE:-busybox:stable}
CONVERGE_SECS=${CONVERGE_SECS:-240}
WEB_REPLICAS=$((NODES + 1))
SPREAD_REPLICAS=$NODES
RESULTS=()

cl_init "fault$NODES"
for ((i = 0; i < NODES; i++)); do cl_add_node; done
cl_trap_cleanup
S=${CL_NAMES[0]}

now() { date +%s; }

# result NAME TEXT: one line per scenario, kept for the summary.
result() {
	RESULTS[${#RESULTS[@]}]="$1: $2"
	cl_note "RESULT nodes=$NODES $1: $2"
}

node_name_of_id() { # NODE_ID: the container name of a node
	local i
	for ((i = 0; i < NODES; i++)); do
		if [ "${CL_IDS[$i]}" = "$1" ]; then
			echo "${CL_NAMES[$i]}"
			return 0
		fi
	done
	return 1
}

# container_rows prints one "C" line per hivemind container on each node that
# answers, exited ones included: node name, the first 12 ID characters, state,
# StartedAt, app, replica index and generation. A node that does not answer
# (a killed worker) contributes nothing.
container_rows() {
	local i c
	for ((i = 0; i < NODES; i++)); do
		c=${CL_NAMES[$i]}
		docker exec "$c" sh -c 'ids=$(docker ps -aq --no-trunc --filter label=hivemind.app); [ -z "$ids" ] || docker inspect -f "{{.Id}}|{{.State.Status}}|{{.State.StartedAt}}|{{index .Config.Labels \"hivemind.app\"}}|{{index .Config.Labels \"hivemind.replica\"}}|{{index .Config.Labels \"hivemind.generation\"}}" $ids' 2>/dev/null |
			awk -F'|' -v n="$c" 'BEGIN { OFS = "\t" } { print "C", n, substr($1, 1, 12), $2, $3, $4, $5, $6 }' || true
	done
}

# take_snapshot writes $CL_WORK/state.tsv: the nodes (N), the server's status
# (A per app, R per replica), and the containers on each node (C). It fails,
# and leaves the old file alone, when the server does not answer.
take_snapshot() {
	local tmp=$CL_WORK/state.tmp i
	docker exec "$S" sh -c "hivemind status --json | jq -r '.[] | . as \$a | ([\"A\", \$a.name, (\$a.desired|tostring)] | @tsv), (.replicas[] | [\"R\", \$a.name, (.index|tostring), .node_id, .state, .container_id, .pending_reason, (.generation|tostring), .desired] | @tsv)'" >"$tmp" 2>/dev/null || {
		rm -f "$tmp"
		return 1
	}
	for ((i = 0; i < NODES; i++)); do
		printf 'N\t%s\t%s\n' "${CL_IDS[$i]}" "${CL_NAMES[$i]}"
	done >>"$tmp"
	container_rows >>"$tmp"
	mv "$tmp" "$CL_WORK/state.tsv"
}

# check_converged reads state.tsv and prints "converged", or the reasons it is
# not, and returns 1.
check_converged() {
	awk -F'\t' '
	function bad(msg) { if (nbad < 6) reasons[nbad] = msg; nbad++ }
	$1 == "N" { name[$2] = $3; next }
	$1 == "A" { want[$2] = $3; next }
	$1 == "R" {
		k = $2 "|" $3
		rnode[k] = $4; rstate[k] = $5; rcid[k] = $6; rreason[k] = $7; rdes[k] = $9
		next
	}
	$1 == "C" {
		k = $6 "|" $7
		cstate[$2 "|" $3] = $4
		copies[k]++
		crow[++nc] = $2 "|" $3 "|" k
		next
	}
	END {
		for (k in rnode) {
			if (rdes[k] != "running") { bad(k " is desired " rdes[k]); continue }
			if (rstate[k] == "running") {
				key = name[rnode[k]] "|" substr(rcid[k], 1, 12)
				if (cstate[key] != "running") bad(k " is reported running as " substr(rcid[k], 1, 12) " on " rnode[k] ", but the engine does not run it")
			} else if (rstate[k] == "pending" && rnode[k] == "" && rreason[k] != "") {
				# expected: no node yet, with a placement reason
			} else {
				bad(k " state " rstate[k] " node [" rnode[k] "] reason [" rreason[k] "]")
			}
			split(k, p, "|")
			cnt[p[1]]++
		}
		for (a in want) {
			if (cnt[a] + 0 != want[a] + 0) bad("app " a " has " cnt[a] + 0 " replicas in the server, desired " want[a])
		}
		# Every container must be the one the server assigns to its replica on
		# its node: no stray container, and no replica on two nodes.
		for (i = 1; i <= nc; i++) {
			split(crow[i], c, "|")
			k = c[3] "|" c[4]
			if (!(k in rnode)) { bad("stray container " c[2] " on " c[1] " for " k); continue }
			if (name[rnode[k]] != c[1] || substr(rcid[k], 1, 12) != c[2]) bad("container " c[2] " on " c[1] " is not the one assigned to " k)
		}
		for (k in copies) {
			if (copies[k] > 1) bad("duplicate: " k " is on " copies[k] " containers")
		}
		if (nbad == 0) { print "converged"; exit 0 }
		for (i = 0; i < nbad && i < 6; i++) print reasons[i]
		exit 1
	}' "$CL_WORK/state.tsv"
}

converged() { take_snapshot && check_converged >/dev/null; }

# wait_converged: wait for the convergence check, or fail with its reasons.
wait_converged() {
	local what=$1
	if ! wait_for "$CONVERGE_SECS" converged; then
		take_snapshot >/dev/null 2>&1 || true
		cl_fail "$what: not converged after ${CONVERGE_SECS} s: $(check_converged 2>&1 | tr '\n' ';')"
	fi
}

# save_running FILE: the running containers now, as C lines, for kept_ok.
save_running() {
	take_snapshot || cl_fail "the server does not answer; cannot save the containers"
	awk -F'\t' '$1 == "C" && $4 == "running"' "$CL_WORK/state.tsv" >"$1"
}

# kept_ok FILE: every container in FILE is still running with the same ID and
# StartedAt, so it has not been stopped or restarted.
kept_ok() {
	local current
	current=$(container_rows)
	printf '%s\n' "$current" >"$CL_WORK/now.tsv"
	awk -F'\t' '
	FILENAME == ARGV[1] { if ($4 == "running") cur[$2 "|" $3 "|" $5] = 1; next }
	{ key = $2 "|" $3 "|" $5; if (!(key in cur)) { miss++; if (miss <= 5) print "not running as before: " $2 " " $3 " " $6 " " $7 } }
	END { exit miss > 0 }' "$CL_WORK/now.tsv" "$1"
}

# new_containers_ok FILE: no container that is not in FILE has appeared.
new_containers_ok() {
	local current
	current=$(container_rows)
	printf '%s\n' "$current" >"$CL_WORK/now.tsv"
	awk -F'\t' '
	FILENAME == ARGV[1] { seen[$2 "|" $3] = 1; next }
	{ if (!(($2 "|" $3) in seen)) { new++; print "new container: " $2 " " $3 " " $6 " " $7 } }
	END { exit new > 0 }' "$1" "$CL_WORK/now.tsv"
}

# spread_one_each: every node runs exactly one running spread replica.
spread_one_each() {
	take_snapshot || return 1
	awk -F'\t' -v n="$NODES" '
	$1 == "N" { nodes[$2] = 1 }
	$1 == "R" && $2 == "spread" && $5 == "running" { cnt[$4]++ }
	END {
		for (id in nodes) if (cnt[id] != 1) exit 1
		exit 0
	}' "$CL_WORK/state.tsv"
}

# place_spread: spread is deleted and deployed again, so it has one replica on
# each node. Placed replicas never move, so without this a worker can end up
# with none, and a fault on it would test nothing. The wait is the readiness
# wait (--min-ready): a node that has just come back gets no replica for 30 s,
# so the deploy waits 40 s first.
place_spread() {
	cl_note "placing spread: one replica on each node"
	docker exec "$S" hivemind delete spread >/dev/null 2>&1 || true
	wait_converged "delete spread"
	sleep 40
	docker exec -i "$S" hivemind deploy -f - >/dev/null <<EOF || cl_fail "deploy spread"
{"name":"spread","image":"$WEB_IMAGE","replicas":$SPREAD_REPLICAS,"command":["sleep","86400"]}
EOF
	wait_for "$CONVERGE_SECS" spread_one_each || cl_fail "spread does not have one running replica on each node: $(awk -F'\t' '$1 == "R" && $2 == "spread"' "$CL_WORK/state.tsv" | tr '\n' ';')"
	wait_converged "place spread"
}

# must_hold_spread NODE_ID: NODE_ID runs at least one spread replica now. A
# fault on a node that holds nothing proves nothing about rescheduling.
must_hold_spread() {
	take_snapshot || cl_fail "the server does not answer"
	local n
	n=$(awk -F'\t' -v n="$1" '$1 == "R" && $2 == "spread" && $4 == n && $5 == "running" { c++ } END { print c + 0 }' "$CL_WORK/state.tsv")
	[ "$n" -ge 1 ] || cl_fail "$1 holds no running spread replica before the fault, so the scenario tests nothing: $(awk -F'\t' '$1 == "R" && $2 == "spread"' "$CL_WORK/state.tsv" | tr '\n' ';')"
}

# spread_off_node NODE_ID: spread has its replicas running, none on NODE_ID.
spread_off_node() {
	take_snapshot || return 1
	awk -F'\t' -v n="$1" -v want="$SPREAD_REPLICAS" '
	$1 == "R" && $2 == "spread" && $5 == "running" { run++; if ($4 == n) on++ }
	END { exit !(run == want && on == 0) }' "$CL_WORK/state.tsv"
}

# lost_count NODE_ID: how many times the server has logged NODE_ID as lost.
lost_count() {
	docker exec "$S" sh -c "grep 'node lost' /var/log/hivemind.log | grep -c '$1' || true"
}

serve_worker() { # serve_worker NAME: the worker agent, in the background
	docker exec -d "$1" sh -c 'exec hivemind serve --role worker >>/var/log/hivemind.log 2>&1'
}

kill_server_process() {
	docker exec "$S" sh -c 'for p in /proc/[0-9]*; do if [ "$(cat $p/comm 2>/dev/null)" = hivemind ]; then kill -9 "${p#/proc/}"; fi; done; true'
}

# setup: the cluster, the apps, and a converged state.
setup() {
	local i
	cl_note "starting $NODES nodes: $S (server) and $((NODES - 1)) workers"
	cl_start_nodes
	cl_start_server
	cl_check_server_listeners
	cl_join
	docker exec "$S" apk add --no-cache jq >/dev/null || cl_fail "installing jq in the server node"
	# The workers take replicas only after 30 s of readiness (--min-ready), and
	# placed replicas never move. Deploy after that wait, or every replica lands
	# on the server and the worker scenarios test nothing.
	cl_note "waiting 40 s, the readiness wait for the workers, before the first deploy"
	sleep 40
	docker exec -i "$S" hivemind deploy -f - >/dev/null <<EOF || cl_fail "deploy web"
{"name":"web","image":"$WEB_IMAGE","replicas":$WEB_REPLICAS,
 "command":["sh","-c","mkdir -p /www && echo hivemind-web > /www/index.html && exec httpd -f -p 80 -h /www"],
 "ports":[{"host":8080,"container":80}]}
EOF
	docker exec -i "$S" hivemind deploy -f - >/dev/null <<EOF || cl_fail "deploy spread"
{"name":"spread","image":"$WEB_IMAGE","replicas":$SPREAD_REPLICAS,"command":["sleep","86400"]}
EOF
	wait_converged "setup"
	cl_note "setup: converged with $WEB_REPLICAS web replicas (one pending on port 8080) and $SPREAD_REPLICAS spread replicas"
}

scenario_kill_worker() {
	local c=${CL_NAMES[1]} id=${CL_IDS[1]} t0 t_res t_back t_conv
	place_spread
	must_hold_spread "$id"
	cl_note "kill-worker: docker kill $c ($id)"
	docker kill "$c" >/dev/null
	t0=$(now)
	wait_for 60 spread_off_node "$id" || cl_fail "kill-worker: spread replicas did not run elsewhere within 60 s"
	t_res=$(($(now) - t0))
	[ "$t_res" -le 30 ] || cl_fail "kill-worker: replicas took ${t_res} s to run elsewhere; the limit is 30 s"
	cl_note "kill-worker: spread replicas run on other nodes after ${t_res} s"

	docker start "$c" >/dev/null
	wait_for 90 docker exec "$c" docker info || cl_fail "kill-worker: the Docker daemon in $c did not start again"
	serve_worker "$c"
	t_back=$(now)
	wait_converged "kill-worker"
	t_conv=$(($(now) - t0))
	cl_note "kill-worker: converged $((t_conv)) s after the kill; the worker came back $(($(now) - t_back)) s before the check ended"
	result kill-worker "PASS, replicas run elsewhere in ${t_res} s, converged in ${t_conv} s"
}

scenario_kill_server() {
	local pre=$CL_WORK/pre-server.tsv t0 t_up t_conv i
	save_running "$pre"
	kill_server_process
	t0=$(now)
	cl_note "kill-server: killed the hivemind serve process on $S at $(date -u +%T)"
	for ((i = 0; i < 9; i++)); do
		sleep 5
		docker exec "$S" hivemind status >/dev/null 2>&1 && cl_fail "kill-server: the API still answers"
		kept_ok "$pre" || cl_fail "kill-server: a container stopped or restarted while the server was down"
		new_containers_ok "$pre" || cl_fail "kill-server: a container was started while the server was down"
	done
	cl_note "kill-server: 45 s down, containers unchanged, nothing new scheduled"

	cl_serve_server
	wait_for 60 docker exec "$S" hivemind status || cl_fail "kill-server: the API did not answer after the restart"
	t_up=$(($(now) - t0))
	wait_converged "kill-server"
	t_conv=$(($(now) - t0))
	kept_ok "$pre" || cl_fail "kill-server: a container restarted after the server came back"
	new_containers_ok "$pre" || cl_fail "kill-server: a new container appeared after the server came back"
	cl_note "kill-server: API back after ${t_up} s, converged after ${t_conv} s; no container restarted"
	result kill-server "PASS, API back in ${t_up} s, converged in ${t_conv} s, no restarts"
}

scenario_partition() {
	local c=${CL_NAMES[1]} id=${CL_IDS[1]} pre=$CL_WORK/pre-partition.tsv t0 t_res t_heal t_conv
	place_spread
	save_running "$pre"
	must_hold_spread "$id"
	docker exec "$c" iptables -I OUTPUT -p tcp -d "$CL_S_IP" --dport 4484 -j DROP || cl_fail "partition: iptables in $c"
	t0=$(now)
	cl_note "partition: $c cannot reach $CL_S_IP:4484 from $(date -u +%T)"
	wait_for 90 spread_off_node "$id" || cl_fail "partition: spread replicas did not move off $id within 90 s"
	t_res=$(($(now) - t0))
	cl_note "partition: spread replicas moved off $id after ${t_res} s"
	kept_ok "$pre" || cl_fail "partition: a container stopped while its node was partitioned"
	cl_note "partition: the partitioned node kept its containers running"

	docker exec "$c" iptables -D OUTPUT -p tcp -d "$CL_S_IP" --dport 4484 -j DROP || cl_fail "heal: iptables in $c"
	t_heal=$(now)
	cl_note "heal: rule removed at $(date -u +%T)"
	wait_converged "partition"
	t_conv=$(($(now) - t_heal))
	cl_note "partition: converged $t_conv s after the heal"
	result partition "PASS, replicas moved in ${t_res} s, converged $t_conv s after heal"
}

scenario_slow() {
	local c=${CL_NAMES[1]} id=${CL_IDS[1]} pre=$CL_WORK/pre-slow.tsv lost0 k
	place_spread
	must_hold_spread "$id"
	docker exec "$c" apk add --no-cache iproute2 >/dev/null || cl_fail "slow: installing iproute2 in $c"
	docker exec "$c" tc qdisc add dev eth0 root netem delay 2000ms || cl_fail "slow: tc netem in $c"
	save_running "$pre"
	lost0=$(lost_count "$id")
	cl_note "slow: 2000 ms delay on $c eth0; watching for 60 s"
	for ((k = 0; k < 12; k++)); do
		sleep 5
		[ "$(lost_count "$id")" -eq "$lost0" ] || cl_fail "slow: $id was declared lost while it was only slow"
		kept_ok "$pre" || cl_fail "slow: a container of the slow node stopped or restarted"
	done
	cl_note "slow: no loss and no change to the containers in 60 s"
	docker exec "$c" tc qdisc del dev eth0 root || cl_fail "slow: removing the netem qdisc"
	wait_converged "slow"
	result slow "PASS, no node lost and no container changed in 60 s of 2000 ms delay"
}

scenario_flap() {
	local c=${CL_NAMES[2]} id=${CL_IDS[2]} lost0 lost1 k node a rep t_heal
	lost0=$(lost_count "$id")
	cl_note "flap: $c, 10 partition/heal cycles, 8 s down and 4 s up"
	for ((k = 1; k <= 10; k++)); do
		docker exec "$c" iptables -I OUTPUT -p tcp -d "$CL_S_IP" --dport 4484 -j DROP || cl_fail "flap: iptables in $c"
		sleep 8
		docker exec "$c" iptables -D OUTPUT -p tcp -d "$CL_S_IP" --dport 4484 -j DROP || cl_fail "flap: heal in $c"
		t_heal=$(now)
		if [ "$k" -lt 10 ]; then sleep 4; fi
	done
	lost1=$(lost_count "$id")
	cl_note "flap: last heal at $(date -u +%T); the server logged $((lost1 - lost0)) losses of $id during the flaps"
	[ "$lost1" -gt "$lost0" ] || cl_fail "flap: the flaps did not lapse the lease, so MinReady is not tested"

	# Placements in the 30 s after the last heal must avoid the flapping node.
	# Each step scales spread up by one, a fresh index, and reads where it went.
	a=$SPREAD_REPLICAS
	for ((k = 0; k < 3; k++)); do
		if [ "$k" -eq 0 ]; then sleep 2; else sleep 12; fi
		docker exec "$S" hivemind scale spread $((SPREAD_REPLICAS + k + 1)) >/dev/null || cl_fail "flap: scale spread"
		take_snapshot || cl_fail "flap: status"
		node=$(awk -F'\t' -v r="$((a + k))" '$1 == "R" && $2 == "spread" && $3 == r { print $4 }' "$CL_WORK/state.tsv")
		[ -n "$node" ] || cl_fail "flap: new replica $((a + k)) has no node"
		[ "$node" != "$id" ] || cl_fail "flap: replica $((a + k)) was placed on the flapping node $id $(( $(now) - t_heal )) s after the last heal"
		cl_note "flap: $(($(now) - t_heal)) s after the last heal, replica $((a + k)) went to another node"
	done
	sleep 6
	docker exec "$S" hivemind scale spread "$SPREAD_REPLICAS" >/dev/null || cl_fail "flap: scale spread back"
	wait_converged "flap"
	result flap "PASS, $((lost1 - lost0)) losses of the flapping node; no placement on it within 30 s of the last heal"
}

setup
scenario_kill_worker
scenario_kill_server
scenario_partition
scenario_slow
scenario_flap

cl_note "summary for $NODES nodes:"
for r in "${RESULTS[@]}"; do cl_note "  $r"; done
cl_note "all fault scenarios passed on $NODES nodes"
