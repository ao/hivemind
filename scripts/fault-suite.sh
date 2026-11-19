#!/usr/bin/env bash
# Fault suite: a hivemind cluster in Docker, with faults injected into it.
#
#   NODES=3 ./scripts/fault-suite.sh   (default)
#   NODES=5 ./scripts/fault-suite.sh
#
# NODES is the total number of nodes: one server and NODES-1 workers, each a
# docker:dind container on one private network with a subnet of its own, as
# in integration-cluster.sh. The server advertises a name, hm-server, a
# network alias of its container, and the workers join that name, as the
# docs recommend, so that the server can be restored at another address.
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
#   flap          6 partition/heal cycles in 146 s on a worker, each partition
#                 longer than the lease. Then the new replicas placed in the
#                 30 s after the last heal must avoid the flapping node
#                 (MinReady).
#   lose-server-partition
#                 a worker is cut off from port 4484 at any address, and its
#                 spread replicas move once its lease lapses. Then the server's
#                 whole machine is lost: a backup is taken, the latest one, and
#                 the server's container is killed (kept, stopped, with its
#                 data directory, for later). A new container at another
#                 address restores the backup at the next epoch and serves
#                 with --advertise hm-server --cluster-addr <its address>, as
#                 in integration-newaddr.sh (helpers in scripts/lib/restore.sh).
#                 20 s after its start the workers that were up are waiting,
#                 and the cut-off one, already lost in the backup, is lost. At
#                 30 s, inside the 2 minute start grace, the alias moves to it,
#                 as a DNS record would. The workers that were up report, with
#                 their containers as before the loss and none new; the cut-off
#                 one has kept its containers running. Then the partition
#                 heals, and the cluster converges: the cut-off worker reaches
#                 the new server by name without joining again and removes the
#                 copies that moved. No worker that was up is lost, and the
#                 workers that were up still run the containers they ran
#                 before the loss.
#   lose-server-slow
#                 the same loss and restore, with tc netem adding 2000 ms of
#                 delay to a worker throughout instead of the partition. Every
#                 worker waits, then reports, the slow one too, inside the
#                 grace; no node is lost, and no worker's container changes,
#                 before or after the delay is removed and the cluster
#                 converges.
#   start-hold-late
#                 serve on the server is killed, every worker's packets to
#                 port 4484 are dropped (iptables), and serve starts again
#                 with --start-grace 30s. Past the grace no worker has
#                 reported, so the server holds them: every worker waiting,
#                 none lost, nothing placed again, and the error line. Then
#                 the first worker's rule comes off, and 12 s later the
#                 others': the first one's report ends the hold, the server
#                 logs that the others wait for the start grace again, and
#                 they report inside it. No worker is lost, and no container
#                 on any node changes or appears. In the fifth soak, where the
#                 hold ended for every worker at once, a worker 0.5 s behind
#                 the first was lost, and one 12 s behind had its replicas
#                 started again on the server's machine.
#
# After each restore, the old server comes back: the restored server stops,
# the alias moves to the old container, which starts again at its old
# address with its old store, and serve runs there. Every worker refuses it,
# as it has seen the higher epoch, and logs that once; for 20 s their
# containers stay as they were, and the old server has no report from them.
# Then the old server stops, the restored one serves again under the alias,
# every worker reports to it again, never restarted, and the cluster
# converges. On 3 nodes on a 16-vCPU arm64 machine (EC2 c8g.4xlarge) the two
# took about 7 minutes together, and the whole suite 17.
#
# Convergence (see converged): every replica is either running on the node the
# server assigns it, with that container running, or pending with a placement
# reason; the server's status names the same container IDs the engines report;
# no container is left over; no (app, replica) pair is on two nodes.
#
# It publishes no ports. On exit, also after a failure or Ctrl-C, it removes
# its containers, the old servers' included, network and volumes. The Go
# build cache (hivemind-cache) is kept.
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
# shellcheck source=lib/restore.sh
. "$REPO/scripts/lib/restore.sh"

WEB_IMAGE=${WEB_IMAGE:-busybox:stable}
CONVERGE_SECS=${CONVERGE_SECS:-240}
WEB_REPLICAS=$((NODES + 1))
SPREAD_REPLICAS=$NODES
RESULTS=()
ALIAS=hm-server
EXTRA=()

cl_init "fault$NODES"
for ((i = 0; i < NODES; i++)); do cl_add_node; done
cl_trap_cleanup
S=${CL_NAMES[0]}
CL_NET_SUBNET=auto
CL_SERVER_RUN_ARGS="--network-alias $ALIAS"
CL_ADVERTISE=$ALIAS
CL_JOIN_ADDR=$ALIAS
SERVER_ADDR=$ALIAS:4484

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
	awk -F'\t' -f "$REPO/scripts/lib/converged.awk" "$CL_WORK/state.tsv"
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
	# Each partition lasts longer than the 15 s lease and one 5 s sweep, so
	# each one lapses the lease, the last one included. A node whose lease
	# lapsed is ready again only from its next renewal, so the placements
	# below fall within its 30 s wait whatever the timing. With partitions
	# shorter than the lease, whether the last one lapsed was down to chance.
	cl_note "flap: $c, 6 partition/heal cycles, 21 s down and 4 s up"
	for ((k = 1; k <= 6; k++)); do
		docker exec "$c" iptables -I OUTPUT -p tcp -d "$CL_S_IP" --dport 4484 -j DROP || cl_fail "flap: iptables in $c"
		sleep 21
		docker exec "$c" iptables -D OUTPUT -p tcp -d "$CL_S_IP" --dport 4484 -j DROP || cl_fail "flap: heal in $c"
		t_heal=$(now)
		if [ "$k" -lt 6 ]; then sleep 4; fi
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

# running_on FILE NAME...: the running hivemind containers of the nodes
# named, as C lines, into FILE.
running_on() {
	local f=$1
	shift
	container_rows | awk -F'\t' -v names=" $* " '$4 == "running" && index(names, " " $2 " ")' >"$f"
}

# same_containers FILE NAME...: the nodes named run the containers in FILE,
# with the same IDs and StartedAt, and no other. If not, it prints which
# are gone and which are new, and fails.
same_containers() {
	local f=$1 w=$CL_WORK/same.want g=$CL_WORK/same.got
	shift
	running_on "$CL_WORK/same.tsv" "$@"
	cut -f 2,3,5 "$f" | sort >"$w"
	cut -f 2,3,5 "$CL_WORK/same.tsv" | sort >"$g"
	cmp -s "$w" "$g" && return 0
	echo "gone: $(comm -23 "$w" "$g" | tr '\t\n' ' ;') new: $(comm -13 "$w" "$g" | tr '\t\n' ' ;')"
	return 1
}

# state_is ID STATE: the node's row in the server's hivemind nodes has STATE.
state_is() { docker exec "$S" hivemind nodes | awk -v id="$1" '$1 == id' | grep -q " $2 "; }

# all_state STATE ID...: every node named has STATE.
all_state() {
	local want=$1 id
	shift
	for id in "$@"; do state_is "$id" "$want" || return 1; done
}

server_id() { docker exec "$S" hivemind nodes | awk '$3 == "server" { print $1 }'; }

# web_settled: web runs one replica on every node, and its extra replica is
# pending on the port, as after setup.
web_settled() {
	take_snapshot || return 1
	awk -F'\t' -v n="$NODES" '
	$1 == "R" && $2 == "web" && $5 == "running" { run++; on[$4] = 1 }
	END { c = 0; for (k in on) c++; exit !(run == n && c == n) }' "$CL_WORK/state.tsv"
}

# lose_and_restore WHAT WAITING: the server's machine is lost, and its
# latest backup restored on a new container at a new address (see the
# header), with --cluster-addr, as the alias still leads nowhere. The
# restored server says the name does not resolve, keeps the server's node
# ID, and waits for WAITING workers.
lose_and_restore() {
	local what=$1 waiting=$2
	lose_server
	docker exec "$S" apk add --no-cache jq >/dev/null || cl_fail "$what: installing jq in the new server node"
	CL_SERVE_ARGS="--cluster-addr $NEW_IP"
	restore_and_serve
	[ "$(server_id)" = "${CL_IDS[0]}" ] || cl_fail "$what: the restored server's node is $(server_id), not ${CL_IDS[0]}"
	docker exec "$S" grep -q -- '--advertise does not resolve' /var/log/hivemind.log ||
		cl_fail "$what: serve did not warn that $ALIAS does not resolve: $(docker exec "$S" tail -n 5 /var/log/hivemind.log)"
	docker exec "$S" grep -q "workers that were up before this start wait for their first report.* waiting=$waiting " /var/log/hivemind.log ||
		cl_fail "$what: serve did not log that $waiting workers wait: $(docker exec "$S" grep 'wait for their first report' /var/log/hivemind.log)"
}

# at_since_served SECONDS: sleep until SECONDS after the restored server
# answered.
at_since_served() {
	local left=$((SERVED_AT + $1 - $(now)))
	[ "$left" -le 0 ] || sleep "$left"
}

# move_record WHAT: 30 s after the restored server started, past the 15 s
# lease timeout and inside the 2 minute start grace, the alias moves to it,
# as a DNS record would be updated.
move_record() {
	at_since_served 30
	reconnect "$S" "$NEW_IP" --alias "$ALIAS"
	docker exec "$S" pgrep -x hivemind >/dev/null || cl_fail "$1: serve stopped when the alias moved"
	cl_check_server_listeners
	cl_note "$1: moved the alias $ALIAS to $NEW_IP, $(($(now) - SERVED_AT)) s after serve started"
}

# not_lost WHAT ID...: the restored server has logged none of the nodes
# named as lost.
not_lost() {
	local what=$1 id
	shift
	for id in "$@"; do
		[ "$(lost_count "$id")" -eq 0 ] || cl_fail "$what: the restored server lost $id, which was up: $(docker exec "$S" grep 'node lost' /var/log/hivemind.log | tr '\n' ';')"
	done
}

# old_server_check WHAT: the restored server stops, the alias moves to the
# old server's container, which starts again at its old address with its
# old store, and serve runs there. Every worker refuses it (see
# workers_refuse_old in lib/restore.sh). Then the old server stops, the
# restored one serves again under the alias, every worker, never restarted,
# reports to it again, and the cluster converges.
old_server_check() {
	local what=$1 c i
	for ((i = 1; i < NODES; i++)); do
		c=${CL_NAMES[$i]}
		epoch_is "$c" "$RS_LOSSES" || cl_fail "$what: $c has not seen the restored server's epoch $RS_LOSSES: $(docker exec "$c" cat /var/lib/hivemind/cluster-epoch 2>&1)"
		CONTAINERS[$i]=$(worker_containers "$c")
	done
	mark_logs "${CL_NAMES[@]:1}"
	docker exec "$S" pkill -TERM -x hivemind || true
	wait_for 30 worker_stopped "$S" || cl_fail "$what: the restored server did not stop"
	reconnect "$S" "$NEW_IP"
	OLD_NET_ARGS="--alias $ALIAS"
	old_server_returns --advertise "$ALIAS" --cluster-addr "$OLD_IP"
	workers_refuse_old
	old_server_goes
	reconnect "$S" "$NEW_IP" --alias "$ALIAS"
	cl_serve_server
	wait_for 30 docker exec "$S" hivemind status || cl_fail "$what: the restored server's API did not answer after its restart"
	for ((i = 1; i < NODES; i++)); do
		c=${CL_NAMES[$i]}
		wait_for 90 talks_again "$c" || cl_fail "$what: $c did not take the restored server again: $(docker exec "$c" tail -n 5 /var/log/hivemind.log)"
	done
	wait_converged "$what, after the old server"
	cl_note "$what: the old server at $OLD_IP was refused by every worker; the restored one has them back"
}

scenario_lose_server_partition() {
	local what=lose-server-partition c=${CL_NAMES[1]} id=${CL_IDS[1]} pre=$CL_WORK/pre-lsp.tsv cut=$CL_WORK/pre-lsp-cut.tsv
	local up=() up_ids=() i out t_start t0 t_moved t_back t_heal t_conv
	t_start=$(now)
	for ((i = 2; i < NODES; i++)); do
		up=(${up[@]+"${up[@]}"} "${CL_NAMES[$i]}")
		up_ids=(${up_ids[@]+"${up_ids[@]}"} "${CL_IDS[$i]}")
	done
	place_spread
	must_hold_spread "$id"
	# No destination: the worker stays cut off from the new server too.
	docker exec "$c" iptables -I OUTPUT -p tcp --dport 4484 -j DROP || cl_fail "$what: iptables in $c"
	t0=$(now)
	cl_note "$what: $c cannot reach port 4484 at any address from $(date -u +%T)"
	wait_for 90 spread_off_node "$id" || cl_fail "$what: spread replicas did not move off $id within 90 s"
	t_moved=$(($(now) - t0))
	cl_note "$what: spread replicas moved off $id after $t_moved s"
	sleep 5
	running_on "$pre" "${up[@]}"
	running_on "$cut" "$c"

	lose_and_restore "$what" "${#up[@]}"
	# The workers that were up wait. The cut-off one, whose lease had lapsed
	# when the server went down, is lost at once.
	at_since_served 20
	all_state waiting "${up_ids[@]}" || cl_fail "$what: 20 s after the start the workers that were up are not waiting: $(docker exec "$S" hivemind nodes | tr '\n' ';')"
	state_is "$id" lost || cl_fail "$what: 20 s after the start the cut-off $id is not lost: $(docker exec "$S" hivemind nodes | tr '\n' ';')"
	cl_note "$what: 20 s after the start, ${#up[@]} worker(s) waiting, the cut-off $id lost"
	move_record "$what"
	wait_for 90 all_state ready "${up_ids[@]}" || cl_fail "$what: the workers that were up did not reconnect: $(docker exec "$S" hivemind nodes | tr '\n' ';')"
	t_back=$(($(now) - SERVED_AT))
	cl_note "$what: the workers that were up report, $t_back s after serve started"
	out=$(same_containers "$pre" "${up[@]}") || cl_fail "$what: the containers of the workers that were up changed: $out"
	out=$(same_containers "$cut" "$c") || cl_fail "$what: the cut-off worker's containers changed before the heal: $out"
	if state_is "$id" ready; then cl_fail "$what: the cut-off $id reported through the partition"; fi
	not_lost "$what" "${up_ids[@]}"

	docker exec "$c" iptables -D OUTPUT -p tcp --dport 4484 -j DROP || cl_fail "$what: heal: iptables in $c"
	t_heal=$(now)
	cl_note "$what: healed at $(date -u +%T)"
	# The cut-off worker takes web's pending replica once it is past
	# --min-ready; converged alone allows it to be pending.
	wait_for "$CONVERGE_SECS" web_settled || cl_fail "$what: web does not run on every node after the heal: $(awk -F'\t' '$1 == "R" && $2 == "web"' "$CL_WORK/state.tsv" | tr '\n' ';')"
	wait_converged "$what"
	t_conv=$(($(now) - t_heal))
	[ "$(cl_node_id_of "$c")" = "$id" ] || cl_fail "$what: $c has node ID $(cl_node_id_of "$c"), not $id"
	[ "$(docker exec "$c" grep -c 'joined the cluster' /var/log/hivemind.log)" = 1 ] || cl_fail "$what: $c joined again"
	out=$(same_containers "$pre" "${up[@]}") || cl_fail "$what: the containers of the workers that were up changed after the heal: $out"
	not_lost "$what" "${up_ids[@]}"
	cl_note "$what: converged $t_conv s after the heal; the cut-off worker is back without joining again"

	RS_DOWN_IDS=$id
	old_server_check "$what"
	out=$(same_containers "$pre" "${up[@]}") || cl_fail "$what: the containers of the workers that were up changed beside the old server: $out"
	not_lost "$what" "${up_ids[@]}"
	result "$what" "PASS in $(($(now) - t_start)) s, replicas moved in ${t_moved} s; restored at $NEW_IP, workers that were up back ${t_back} s after serve started, none lost, containers kept; converged ${t_conv} s after heal; old server refused"
}

scenario_lose_server_slow() {
	local what=lose-server-slow c=${CL_NAMES[1]} id=${CL_IDS[1]} pre=$CL_WORK/pre-lss.tsv
	local workers=("${CL_NAMES[@]:1}") ids=("${CL_IDS[@]:1}") out t_start t_back t_conv
	t_start=$(now)
	place_spread
	must_hold_spread "$id"
	docker exec "$c" apk add --no-cache iproute2 >/dev/null || cl_fail "$what: installing iproute2 in $c"
	docker exec "$c" tc qdisc add dev eth0 root netem delay 2000ms || cl_fail "$what: tc netem in $c"
	cl_note "$what: 2000 ms delay on $c eth0"
	running_on "$pre" "${workers[@]}"

	lose_and_restore "$what" "${#workers[@]}"
	at_since_served 20
	all_state waiting "${ids[@]}" || cl_fail "$what: 20 s after the start the workers are not waiting: $(docker exec "$S" hivemind nodes | tr '\n' ';')"
	cl_note "$what: 20 s after the start, ${#workers[@]} workers waiting"
	move_record "$what"
	wait_for 90 all_state ready "${ids[@]}" || cl_fail "$what: the workers did not reconnect, the slow one included: $(docker exec "$S" hivemind nodes | tr '\n' ';')"
	t_back=$(($(now) - SERVED_AT))
	cl_note "$what: every worker reports, the slow $c too, $t_back s after serve started"
	out=$(same_containers "$pre" "${workers[@]}") || cl_fail "$what: a worker's containers changed: $out"
	not_lost "$what" "${ids[@]}"

	docker exec "$c" tc qdisc del dev eth0 root || cl_fail "$what: removing the netem qdisc"
	wait_converged "$what"
	t_conv=$(($(now) - SERVED_AT))
	out=$(same_containers "$pre" "${workers[@]}") || cl_fail "$what: a worker's containers changed: $out"
	not_lost "$what" "${ids[@]}"

	RS_DOWN_IDS=
	old_server_check "$what"
	out=$(same_containers "$pre" "${workers[@]}") || cl_fail "$what: a worker's containers changed beside the old server: $out"
	not_lost "$what" "${ids[@]}"
	result "$what" "PASS in $(($(now) - t_start)) s, restored at $NEW_IP with 2000 ms delay on a worker; every worker back ${t_back} s after serve started, none lost, no container changed; converged ${t_conv} s after serve started; old server refused"
}

# log_count PATTERN: how many lines of the server's log match PATTERN.
log_count() { docker exec "$S" sh -c "grep -c '$1' /var/log/hivemind.log || true"; }

scenario_start_hold_late() {
	local what=start-hold-late first=${CL_NAMES[1]} first_id=${CL_IDS[1]} pre=$CL_WORK/pre-shl.tsv
	local workers=("${CL_NAMES[@]:1}") ids=("${CL_IDS[@]:1}") rest=("${CL_NAMES[@]:2}") rest_ids=("${CL_IDS[@]:2}")
	local lost0=() errors0 ended0 saved=${CL_SERVE_ARGS:-} c i id left t_start t_heal1 t_back1 t_heal2 t_back2
	t_start=$(now)
	place_spread
	for id in "${ids[@]}"; do must_hold_spread "$id"; done
	save_running "$pre"
	for ((i = 0; i < ${#ids[@]}; i++)); do lost0[i]=$(lost_count "${ids[$i]}"); done
	errors0=$(log_count 'no worker has reported to this server since it started')
	ended0=$(log_count "which ends the start grace")

	kill_server_process
	for c in "${workers[@]}"; do
		docker exec "$c" iptables -I OUTPUT -p tcp --dport 4484 -j DROP || cl_fail "$what: iptables in $c"
	done
	cl_note "$what: killed serve on $S, and dropped every worker's packets to port 4484, at $(date -u +%T)"
	CL_SERVE_ARGS="$saved --start-grace 30s"
	cl_serve_server
	CL_SERVE_ARGS=$saved
	wait_for 60 docker exec "$S" hivemind status || cl_fail "$what: the API did not answer after the restart"
	SERVED_AT=$(now)
	docker exec "$S" grep -q "workers that were up before this start wait for their first report.* waiting=${#workers[@]} " /var/log/hivemind.log ||
		cl_fail "$what: serve did not log that ${#workers[@]} workers wait: $(docker exec "$S" grep 'wait for their first report' /var/log/hivemind.log | tail -n 1)"

	# Past the 30 s grace, with no report: the hold.
	at_since_served 40
	all_state waiting "${ids[@]}" || cl_fail "$what: past the grace the workers are not all waiting: $(docker exec "$S" hivemind nodes | tr '\n' ';')"
	docker exec "$S" hivemind nodes | grep -q 'none of them has reported since the server started' ||
		cl_fail "$what: hivemind nodes does not say the server holds the workers: $(docker exec "$S" hivemind nodes | tr '\n' ';')"
	[ "$(log_count 'no worker has reported to this server since it started')" -gt "$errors0" ] ||
		cl_fail "$what: serve did not log the hold at error"
	kept_ok "$pre" || cl_fail "$what: a container stopped or restarted in the hold"
	new_containers_ok "$pre" || cl_fail "$what: a container was started in the hold"
	cl_note "$what: $(($(now) - SERVED_AT)) s after the start, past its 30 s grace: every worker waiting, held, nothing placed again"

	docker exec "$first" iptables -D OUTPUT -p tcp --dport 4484 -j DROP || cl_fail "$what: heal: iptables in $first"
	t_heal1=$(now)
	wait_for 30 state_is "$first_id" ready || cl_fail "$what: $first did not report within 30 s of its heal: $(docker exec "$S" hivemind nodes | tr '\n' ';')"
	t_back1=$(($(now) - t_heal1))
	cl_note "$what: $first healed, and reported ${t_back1} s later"
	if [ "${#rest[@]}" -gt 0 ]; then
		left=$((t_heal1 + 12 - $(now)))
		[ "$left" -le 0 ] || sleep "$left"
		all_state waiting "${rest_ids[@]}" || cl_fail "$what: 12 s after the first heal the others are not waiting: $(docker exec "$S" hivemind nodes | tr '\n' ';')"
		[ "$(log_count "which ends the start grace")" -gt "$ended0" ] ||
			cl_fail "$what: serve did not log the end of the hold: $(docker exec "$S" tail -n 5 /var/log/hivemind.log | tr '\n' ';')"
		docker exec "$S" grep "which ends the start grace" /var/log/hivemind.log | tail -n 1 | grep -q "waiting=${#rest[@]} " ||
			cl_fail "$what: the end of the hold does not name ${#rest[@]} workers waiting: $(docker exec "$S" grep "ends the start grace's hold" /var/log/hivemind.log | tail -n 1)"
		for c in "${rest[@]}"; do
			docker exec "$c" iptables -D OUTPUT -p tcp --dport 4484 -j DROP || cl_fail "$what: heal: iptables in $c"
		done
		t_heal2=$(now)
		wait_for 30 all_state ready "${rest_ids[@]}" || cl_fail "$what: the others did not report within 30 s of their heal: $(docker exec "$S" hivemind nodes | tr '\n' ';')"
		t_back2=$(($(now) - t_heal2))
		cl_note "$what: the others healed $((t_heal2 - t_heal1)) s after $first, reported ${t_back2} s later"
	fi
	for ((i = 0; i < ${#ids[@]}; i++)); do
		[ "$(lost_count "${ids[$i]}")" -eq "${lost0[$i]}" ] ||
			cl_fail "$what: the server lost ${ids[$i]}: $(docker exec "$S" grep 'node lost' /var/log/hivemind.log | tail -n 3 | tr '\n' ';')"
	done
	kept_ok "$pre" || cl_fail "$what: a container stopped or restarted after the heals"
	new_containers_ok "$pre" || cl_fail "$what: a replica was placed again after the heals"
	wait_converged "$what"
	kept_ok "$pre" || cl_fail "$what: a container stopped or restarted by convergence"
	new_containers_ok "$pre" || cl_fail "$what: a container appeared by convergence"
	result "$what" "PASS in $(($(now) - t_start)) s; held past a 30 s grace, $first reported ${t_back1} s after its heal, the others healed 12 s later and reported in ${t_back2:-0} s; none lost, no container changed"
}

setup
scenario_kill_worker
scenario_kill_server
scenario_partition
scenario_slow
scenario_flap
scenario_lose_server_partition
scenario_lose_server_slow
scenario_start_hold_late

cl_note "summary for $NODES nodes:"
for r in "${RESULTS[@]}"; do cl_note "  $r"; done
cl_note "all fault scenarios passed on $NODES nodes"
