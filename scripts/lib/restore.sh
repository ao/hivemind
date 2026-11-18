# Helpers for the scripts that lose the server's machine and restore its
# backup on a new container at a new address: scripts/integration-newaddr.sh
# and scripts/fault-suite.sh. Sourced after scripts/lib/cluster.sh, by a
# script whose network has a subnet of its own (CL_NET_SUBNET=auto), so that
# a container can be given an address. Written for bash 3.2 as well as 5.
#
# Besides the CL_* globals of cluster.sh, they use and set:
#   S            the server's container. lose_server replaces it, in S and
#                CL_NAMES[0], and sets CL_S_IP to the new one's address.
#   EXTRA        more containers that rs_cleanup removes. lose_server adds
#                each old server's container to it.
#   OLD_S OLD_IP the lost server's container and address (lose_server).
#   NEW_IP       the new server's address (lose_server).
#   LOST_AT      when the server was lost (lose_server).
#   RS_LOSSES    how many times the server was lost and restored: the epoch
#                of the cluster after the last restore, from 0 at init.
#   SERVED_AT    when the restored server's API answered (restore_and_serve).
#   BEFORE_SERVE a command restore_and_serve runs just before serve.
#   OLD_NET_ARGS docker network connect options of the old server when it
#                comes back, such as its alias (old_server_returns).
#   OLD_AT       when the old server came back (old_server_returns).
#   SERVER_ADDR  host:port the workers dial, as their log lines name it.
#   CONTAINERS   CONTAINERS[i]: worker_containers of the worker at node
#                index i, which workers_refuse_old expects to stay.
#   RS_DOWN_IDS  the node IDs, space-separated, of the workers that were
#                already lost when the old server went down. The old server
#                shows them lost, and every other worker waiting.

RS_LOSSES=0
RS_OLD=()

# rs_cleanup removes the containers in EXTRA and the Docker volume of each
# old server, then what cl_cleanup removes. After a failure it prints the
# end of the last old server's log first.
rs_cleanup() {
	local rc=$? c
	set +e
	if [ "$rc" -ne 0 ] && [ -n "${OLD_S:-}" ]; then
		echo "cluster: --- last lines of $OLD_S:/var/log/hivemind.log (the old server)" >&2
		docker exec "$OLD_S" sh -c 'tail -n 40 /var/log/hivemind.log' >&2 2>/dev/null
	fi
	for c in ${EXTRA[@]+"${EXTRA[@]}"}; do
		docker rm -fv "$c" >/dev/null 2>&1
	done
	for c in ${RS_OLD[@]+"${RS_OLD[@]}"}; do
		docker volume rm "$c-docker" >/dev/null 2>&1
	done
	(exit "$rc")
	cl_cleanup
}

# reconnect CONTAINER IP [OPTIONS...]: disconnect the container from the
# network and connect it again at IP, with the docker network connect
# options given, such as --alias: how an alias, standing for a DNS record,
# moves from one container to another.
reconnect() {
	local c=$1 ip=$2
	shift 2
	docker network disconnect "$CL_NET" "$c"
	docker network connect --ip "$ip" "$@" "$CL_NET" "$c"
	[ "$(cl_ip_of "$c")" = "$ip" ] || cl_fail "$c has address $(cl_ip_of "$c") after it was connected again, not $ip"
}

# worker_containers NODE: the IDs of the running hivemind containers on NODE.
worker_containers() { docker exec "$1" docker ps -q --filter label=hivemind.app | sort | tr '\n' ' '; }

worker_stopped() { ! docker exec "$1" pgrep -x hivemind >/dev/null; }

# epoch_is NODE N: the worker on NODE has seen epoch N.
epoch_is() { [ "$(docker exec "$1" cat /var/lib/hivemind/cluster-epoch 2>/dev/null)" = "$2" ]; }

# mark_logs NODE...: remember where each node's hivemind log ends now.
# since NODE prints what the node logged after that, or its whole log when
# it has no mark.
mark_logs() {
	local c
	for c in "$@"; do
		docker exec "$c" sh -c 'wc -l </var/log/hivemind.log' | tr -d ' ' >"$CL_WORK/$c.mark"
	done
}
since() {
	local n=0
	[ ! -s "$CL_WORK/$1.mark" ] || n=$(cat "$CL_WORK/$1.mark")
	docker exec "$1" tail -n "+$((n + 1))" /var/log/hivemind.log
}

# lose_server [DOCKER-RUN-OPTIONS...]: back the server up, then kill its
# container, as a machine that loses power, and start an empty node at
# NEW_IP, in a container of its own, with the docker-run options given and
# the backup in it. The old container is kept, stopped, with its data
# directory and Docker volume, as OLD_S at OLD_IP: old_server_returns
# starts it again. The first new server is <run>-n0r at .200 of the
# subnet, the second <run>-n0r2 at .201, and so on.
lose_server() {
	local n
	RS_LOSSES=$((RS_LOSSES + 1))
	docker exec "$S" hivemind backup "/root/hm-$RS_LOSSES.tar" >/dev/null || cl_fail "hivemind backup"
	docker cp "$S:/root/hm-$RS_LOSSES.tar" "$CL_WORK/hm.tar"
	docker kill "$S" >/dev/null
	LOST_AT=$(date +%s)
	OLD_S=$S
	OLD_IP=$CL_S_IP
	EXTRA=(${EXTRA[@]+"${EXTRA[@]}"} "$OLD_S")
	RS_OLD=(${RS_OLD[@]+"${RS_OLD[@]}"} "$OLD_S")
	trap rs_cleanup EXIT
	n=$((199 + RS_LOSSES))
	NEW_IP=${OLD_IP%.*}.$n
	[ "$NEW_IP" != "$OLD_IP" ] || NEW_IP=${OLD_IP%.*}.$((n + 1))
	cl_note "backed up $S at $OLD_IP, and killed its container"
	S=$CL_RUN-n0r
	[ "$RS_LOSSES" -eq 1 ] || S=$CL_RUN-n0r$RS_LOSSES
	CL_NAMES[0]=$S
	cl_run_node "$S" --ip "$NEW_IP" "$@"
	cl_ready_node "$S"
	[ "$(cl_ip_of "$S")" = "$NEW_IP" ] || cl_fail "the new server node has address $(cl_ip_of "$S"), not $NEW_IP"
	docker cp "$CL_WORK/hm.tar" "$S:/root/hm-$RS_LOSSES.tar"
	CL_S_IP=$NEW_IP
}

# restore_and_serve: restore the backup on the new node and start serve. The
# outage lasts at least 40 s, as a real restore does: longer than the
# worker's 30 s request timeout, which a request sent to the old server just
# before the loss waits out, since a lost machine sends no reset.
# BEFORE_SERVE, if set, is a command run just before serve starts.
restore_and_serve() {
	local out left
	left=$((LOST_AT + 40 - $(date +%s)))
	[ "$left" -le 0 ] || sleep "$left"
	out=$(docker exec "$S" hivemind restore "/root/hm-$RS_LOSSES.tar" --data-dir /var/lib/hivemind 2>&1) || cl_fail "hivemind restore: $out"
	grep -q "The cluster is at epoch $RS_LOSSES now" <<<"$out" || cl_fail "restore did not put the cluster at epoch $RS_LOSSES: $out"
	[ -z "${BEFORE_SERVE:-}" ] || "$BEFORE_SERVE"
	cl_serve_server
	wait_for 30 docker exec "$S" hivemind status || cl_fail "the restored server's API did not answer"
	SERVED_AT=$(date +%s)
	cl_check_server_listeners
	cl_note "restored on a new $S at $NEW_IP, serving $((SERVED_AT - LOST_AT)) s after the loss; listeners on $NEW_IP only"
}

# old_server_returns SERVE-ARGS...: the old server's machine comes back, as
# a recovered machine does: its container starts again at OLD_IP, with the
# network options in OLD_NET_ARGS (such as the alias), its data directory
# and its Docker volume, and serve runs there with SERVE-ARGS and its old
# store.
old_server_returns() {
	docker start "$OLD_S" >/dev/null
	# docker start may give it another address, and its old alias; it gets
	# OLD_IP and only what OLD_NET_ARGS gives.
	# shellcheck disable=SC2086
	reconnect "$OLD_S" "$OLD_IP" ${OLD_NET_ARGS:-}
	wait_for 90 docker exec "$OLD_S" docker info || cl_fail "the Docker daemon in $OLD_S did not start again"
	docker exec -d "$OLD_S" sh -c "exec hivemind serve $* >>/var/log/hivemind.log 2>&1"
	old_listens() { cl_listeners "$OLD_S" | grep -qx "$(cl_hexip "$OLD_IP"):1184"; }
	wait_for 30 old_listens || cl_fail "the old server did not listen on $OLD_IP:4484"
	OLD_AT=$(date +%s)
	cl_note "the old server is back at $OLD_IP, with its old store: serve $*"
}

# refusals NODE: how many times the worker on NODE has logged, since its
# mark, that the server it dials, at SERVER_ADDR, is from before a restore.
refusals() { since "$1" | grep -c "level=ERROR msg=\"the server at $SERVER_ADDR is from before a restore of this cluster" || true; }
refused_old() { [ "$(refusals "$1")" -ge 1 ]; }

# talks_again NODE: the worker on NODE has logged, since its mark, that the
# server at SERVER_ADDR is at the cluster's epoch again.
talks_again() { since "$1" | grep -q "the server at $SERVER_ADDR answers at this cluster.s epoch now; this worker talks to it again"; }

# workers_refuse_old: every worker refuses the old server, and logs it once;
# the old server says a newer copy of the cluster exists; for 20 s nothing
# moves: the workers' containers are the ones in CONTAINERS, and the old
# server has had no report from them, so in its hivemind nodes each one is
# waiting, or lost if it is in RS_DOWN_IDS.
workers_refuse_old() {
	local c i id rows want
	for ((i = 1; i < ${#CL_NAMES[@]}; i++)); do
		c=${CL_NAMES[$i]}
		wait_for 60 refused_old "$c" || cl_fail "$c did not refuse the old server: $(docker exec "$c" tail -n 5 /var/log/hivemind.log)"
	done
	docker exec "$OLD_S" grep -q 'a newer copy of this cluster exists: this server was replaced by a restore' /var/log/hivemind.log ||
		cl_fail "the old server did not say that a newer copy of the cluster exists: $(docker exec "$OLD_S" tail -n 5 /var/log/hivemind.log)"
	cl_note "the workers refuse the old server: $(since "${CL_NAMES[1]}" | grep -o 'server_epoch=[0-9]* seen_epoch=[0-9]*' | head -n 1)"
	sleep 20
	for ((i = 1; i < ${#CL_NAMES[@]}; i++)); do
		c=${CL_NAMES[$i]}
		[ "$(refusals "$c")" = 1 ] || cl_fail "$c logged the refusal $(refusals "$c") times, not once"
		[ "$(worker_containers "$c")" = "${CONTAINERS[$i]}" ] ||
			cl_fail "the containers on $c changed beside the old server: ${CONTAINERS[$i]}before, $(worker_containers "$c")now"
	done
	rows=$(docker exec "$OLD_S" hivemind nodes)
	for ((i = 1; i < ${#CL_NAMES[@]}; i++)); do
		id=${CL_IDS[$i]}
		want=waiting
		case " ${RS_DOWN_IDS:-} " in *" $id "*) want=lost ;; esac
		awk -v id="$id" '$1 == id' <<<"$rows" | grep -q " $want " ||
			cl_fail "$id is not $want on the old server, so it had a report from it: $(tr '\n' ';' <<<"$rows")"
	done
	cl_note "$(($(date +%s) - OLD_AT)) s with the old server up: each worker logged the refusal once, kept its containers, and sent it no report"
}

# old_server_goes: the old server stops, and its container with it.
old_server_goes() {
	docker exec "$OLD_S" pkill -TERM -x hivemind || true
	wait_for 30 worker_stopped "$OLD_S" || cl_fail "the old server did not stop"
	docker stop "$OLD_S" >/dev/null
}
