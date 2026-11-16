#!/usr/bin/env bash
# Edge suite: what hivemind does when the machine under it misbehaves.
#
#   ./scripts/edge-suite.sh                  every scenario
#   ./scripts/edge-suite.sh pulls node-disk  some of them
#
# Each scenario starts its own docker:dind nodes on one private network,
# breaks something, and checks what hivemind reports and how it recovers:
#
#   docker-restart  kill -9 of dockerd under a running agent, then a clean
#                   restart (SIGTERM). The agent keeps running and reports the
#                   daemon as unavailable without spinning; when the daemon is
#                   back every replica runs again, once, with its volume data.
#   pulls           pulls that fail: an image and a tag that do not exist on
#                   Docker Hub, a registry name that does not resolve, one that
#                   refuses connections, one that answers 429, one that answers
#                   404, and one that never answers (edgetool registry). Each
#                   replica says why it is pending, pulls back off (measured
#                   from the registry's request log and the agent's log), a
#                   fixed spec recovers, and another app on the node is still
#                   supervised.
#   node-disk       Docker's data root on a 300 MiB tmpfs, filled up. A pull and
#                   a create fail with a clear status and are not retried every
#                   pass; both recover when the space comes back.
#   server-disk     the server's data directory on a 16 MiB tmpfs, filled up.
#                   Writes fail with a clear error, reads keep working, serve
#                   stays up, and writes work again once there is space;
#                   integrity_check after.
#   kill-serve      kill -9 of serve, 20 times, under a client writing as fast
#                   as it can (edgetool write). After each kill the store
#                   passes integrity_check and the store's invariants, every
#                   acknowledged write is there, and the replicas converge with
#                   none lost or doubled.
#   crashloop       apps that exit at once, exit after 1 s, never pass their
#                   health check, and are OOM-killed, beside one that runs. The
#                   restarts back off, status and metrics say so, and the
#                   healthy app is not touched.
#   logs-restart    hivemind logs -f of a worker's replica and of the server's
#                   while serve is killed, and while it restarts cleanly. Each
#                   command ends, with a message, and does not hang.
#   datadir         a worker's data directory with node.key missing, node.json
#                   corrupt or empty, ca.crt replaced and node.crt missing: the
#                   worker refuses to start and says what to do.
#   limits          the largest spec the API accepts (every list at its limit),
#                   and 100 replicas, deployed for real.
#
# Each scenario prints CHECK lines (PASS or FAIL) and a RESULT line, and the
# suite ends with a summary; it exits 1 if any check failed. The observations
# are kept in $EDGE_OUT (default .scratch/edge-out). Every node's dockerd runs
# with -D, so the Engine API calls can be counted in its log.
#
# The nodes publish no ports. Every listener the tests start binds loopback in
# its node. On exit the suite removes its containers, network and volumes. The
# Go build cache volume (hivemind-cache) is kept.
set -euo pipefail

REPO=$(cd "$(dirname "$0")/.." && pwd)
# shellcheck source=lib/cluster.sh
. "$REPO/scripts/lib/cluster.sh"

ALL=(docker-restart pulls node-disk server-disk kill-serve crashloop logs-restart datadir limits)
if [ $# -gt 0 ]; then SCENARIOS=("$@"); else SCENARIOS=("${ALL[@]}"); fi
for s in "${SCENARIOS[@]}"; do
	case " ${ALL[*]} " in
	*" $s "*) ;;
	*)
		echo "edge-suite: unknown scenario $s (known: ${ALL[*]})" >&2
		exit 2
		;;
	esac
done

IMG=${EDGE_IMAGE:-busybox:stable}
EDGE_OUT=${EDGE_OUT:-$REPO/.scratch/edge-out}
mkdir -p "$EDGE_OUT"
SUMMARY=$EDGE_OUT/summary.txt
: >"$SUMMARY"
CUR=setup

cl_init edges
cl_trap_cleanup

now() { date +%s; }
note() { echo "edge: $CUR: $*"; }

# check NAME STATUS DETAIL: one CHECK line, kept for the summary.
FAILS=0
check() {
	local res=PASS
	if [ "$2" != 0 ]; then
		res=FAIL
		FAILS=$((FAILS + 1))
	fi
	echo "CHECK $res $CUR $1: $3" | tee -a "$SUMMARY"
}
pass() { check "$1" 0 "$2"; }
fail() { check "$1" 1 "$2"; }
result() { echo "RESULT $CUR: $*" | tee -a "$SUMMARY"; }

# ---------------------------------------------------------------- nodes

# ed_node NAME [DOCKER-RUN-OPTIONS...]: a dind node whose dockerd is not PID 1,
# so that it can be killed and started again. tini (--init) is PID 1 and reaps
# what dockerd leaves; the image's dind script sets up the cgroups and runs
# sleep. With ED_NO_DOCKER_VOLUME=1 the data root is not a volume (the
# node-disk scenario mounts a tmpfs there).
ed_node() {
	local c=$1
	shift
	local vol=(-v "$c-docker:/var/lib/docker")
	[ "${ED_NO_DOCKER_VOLUME:-}" = 1 ] && vol=()
	CL_NAMES[${#CL_NAMES[@]}]=$c
	docker run -d --name "$c" --privileged --init --network "$CL_NET" ${vol[@]+"${vol[@]}"} "$@" \
		--entrypoint dind "$DIND_IMAGE" sleep infinity >/dev/null
	ed_dockerd_start "$c"
	wait_for 90 docker exec "$c" docker info || cl_fail "the Docker daemon in $c did not start"
	docker cp "$CL_WORK/hivemind" "$c:/usr/local/bin/hivemind"
	docker cp "$CL_WORK/edgetool" "$c:/usr/local/bin/edgetool"
	docker exec "$c" apk add --no-cache jq curl >/dev/null 2>&1 || cl_fail "installing jq and curl in $c"
}

# ed_dockerd_start NODE: start dockerd, with -D so each API call is logged.
# The stale pid file of a killed daemon is removed, as the image's entrypoint
# does; containerd's is left, so a containerd that outlived dockerd is adopted.
ed_dockerd_start() {
	docker exec -d "$1" sh -c 'rm -f /var/run/docker.pid; exec dockerd -D -H unix:///var/run/docker.sock >>/var/log/dockerd.log 2>&1'
}

ed_rm_node() {
	docker rm -fv "$1" >/dev/null 2>&1 || true
	docker volume rm "$1-docker" >/dev/null 2>&1 || true
}

# ed_server NODE [SERVE-FLAGS...]: init and serve on NODE.
ed_server() {
	local c=$1
	shift
	docker exec "$c" hivemind init >/dev/null || cl_fail "init on $c"
	ED_SERVE_FLAGS=("$@")
	ed_serve "$c"
	wait_for 30 docker exec "$c" hivemind status || cl_fail "the API on $c did not answer"
}

ed_serve() {
	docker exec -d "$1" sh -c "exec hivemind serve --min-ready 0s ${ED_SERVE_FLAGS[*]:-} >>/var/log/hivemind.log 2>&1"
}

hm_pid() { # hm_pid NODE: PIDs of hivemind serve processes in NODE
	docker exec "$1" sh -c 'for p in /proc/[0-9]*; do [ "$(cat $p/comm 2>/dev/null)" = hivemind ] && tr "\0" " " <$p/cmdline 2>/dev/null | grep -q " serve" && echo ${p#/proc/}; done; true'
}

kill_serve() { # kill_serve NODE SIGNAL
	local p
	for p in $(hm_pid "$1"); do docker exec "$1" kill "-$2" "$p" 2>/dev/null || true; done
}

serve_down() { [ -z "$(hm_pid "$1")" ]; }

# cpu_ticks NODE PID: utime+stime of a process, in clock ticks.
cpu_ticks() { docker exec "$1" awk '{ print $14 + $15 }' "/proc/$2/stat" 2>/dev/null || echo 0; }

deploy_json() { # deploy_json NODE JSON
	docker exec -i "$1" hivemind deploy -f - <<<"$2" >/dev/null
}

token() { docker exec "$1" cat /var/lib/hivemind/admin.token; }

# api NODE METHOD PATH [BODY]: prints "CODE BODY" for one API request. A
# BODY of @FILE sends that file of the node.
api() {
	local c=$1 m=$2 p=$3 body=${4:-}
	docker exec -i "$c" sh -c '
		tok=$(cat /var/lib/hivemind/admin.token)
		out=$(mktemp)
		case "$3" in
		"") code=$(curl -s -o "$out" -w "%{http_code}" -X "$1" -H "Authorization: Bearer $tok" "http://127.0.0.1:4483$2") ;;
		@*) code=$(curl -s -o "$out" -w "%{http_code}" -X "$1" -H "Authorization: Bearer $tok" -H "Content-Type: application/json" --data-binary "$3" "http://127.0.0.1:4483$2") ;;
		*) code=$(printf "%s" "$3" | curl -s -o "$out" -w "%{http_code}" -X "$1" -H "Authorization: Bearer $tok" -H "Content-Type: application/json" --data-binary @- "http://127.0.0.1:4483$2") ;;
		esac
		printf "%s %s\n" "$code" "$(tr -d "\n" <"$out" | head -c 600)"
		rm -f "$out"' sh "$m" "$p" "$body"
}

# ---------------------------------------------------------------- convergence

# snapshot NODE: the state file for converged.awk (see fault-suite.sh), in
# $CL_WORK/state.tsv, for a one-node cluster: the server's status and the
# node's containers.
snapshot() {
	local s=$1 tmp=$CL_WORK/state.tmp id
	docker exec "$s" sh -c "hivemind status --json | jq -r '.[] | . as \$a | ([\"A\", \$a.name, (\$a.desired|tostring)] | @tsv), (.replicas[] | [\"R\", \$a.name, (.index|tostring), .node_id, .state, .container_id, .pending_reason, (.generation|tostring), .desired] | @tsv)'" >"$tmp" 2>/dev/null || {
		rm -f "$tmp"
		return 1
	}
	id=$(docker exec "$s" sh -c 'hivemind nodes --json | jq -r ".[] | select(.server) | .id"' 2>/dev/null) || return 1
	printf 'N\t%s\t%s\n' "$id" "$s" >>"$tmp"
	docker exec "$s" sh -c 'ids=$(docker ps -aq --no-trunc --filter label=hivemind.app); [ -z "$ids" ] || docker inspect -f "{{.Id}}|{{.State.Status}}|{{.State.StartedAt}}|{{index .Config.Labels \"hivemind.app\"}}|{{index .Config.Labels \"hivemind.replica\"}}|{{index .Config.Labels \"hivemind.generation\"}}" $ids' 2>/dev/null |
		awk -F'|' -v n="$s" 'BEGIN { OFS = "\t" } { print "C", n, substr($1, 1, 12), $2, $3, $4, $5, $6 }' >>"$tmp" || return 1
	mv "$tmp" "$CL_WORK/state.tsv"
}

converged() { snapshot "$1" && awk -F'\t' -f "$REPO/scripts/lib/converged.awk" "$CL_WORK/state.tsv" >/dev/null; }

# wait_converged SECS NODE: prints the seconds it took, or the reasons and
# returns 1.
wait_converged() {
	local secs=$1 s=$2 t0
	t0=$(now)
	if wait_for "$secs" converged "$s"; then
		echo $(($(now) - t0))
		return 0
	fi
	snapshot "$s" >/dev/null 2>&1 || true
	awk -F'\t' -f "$REPO/scripts/lib/converged.awk" "$CL_WORK/state.tsv" 2>&1 | tr '\n' ';'
	return 1
}

# replica_field NODE APP INDEX FIELD: one field of a replica's status (jq).
replica_field() {
	docker exec "$1" sh -c "hivemind status --json 2>/dev/null | jq -r --arg a '$2' '.[] | select(.name == \$a) | .replicas[] | select(.index == $3) | $4'" 2>/dev/null || true
}

# app_line NODE APP: "state|detail" of replica 0 of APP.
app_line() { replica_field "$1" "$2" 0 '(.state + "|" + (.error // "") + (.pending_reason // ""))'; }

# running_count_is NODE APP N: N replicas of APP are running.
running_count_is() {
	[ "$(docker exec "$1" sh -c "hivemind status --json | jq --arg a '$2' '[.[] | select(.name == \$a) | .replicas[] | select(.state == \"running\")] | length'" 2>/dev/null)" = "$3" ]
}

# failed_creates NODE: the creates the node's dockerd refused, from its log.
failed_creates() { docker exec "$1" sh -c "grep -c 'Handler for POST /containers/create returned error' /var/log/dockerd.log || true"; }

# agent_pulls NODE IMAGE: the pulls of IMAGE the agent logged.
agent_pulls() { docker exec "$1" sh -c "grep 'pulling image' /var/log/hivemind.log | grep -cF 'image=$2 ' || true"; }

# ---------------------------------------------------------------- scenarios

# Case 1. Docker restarts under a running agent.
scenario_docker_restart() {
	local s=$CL_RUN-dr pid c0 c1 st n secs ids0 ids1 vol0 vol1 first0 first1 fails r mode
	ed_node "$s"
	ed_server "$s"
	deploy_json "$s" '{"name":"vol","image":"'"$IMG"'","volumes":[{"name":"data","path":"/data"}],
	 "command":["sh","-c","[ -f /data/first ] || date +%s > /data/first; n=$(cat /data/n 2>/dev/null || echo 0); echo $((n + 1)) > /data/n; exec sleep 86400"]}'
	deploy_json "$s" '{"name":"web","image":"'"$IMG"'","ports":[{"host":8080,"container":80}],
	 "command":["sh","-c","mkdir -p /www && echo edge > /www/index.html && exec httpd -f -p 80 -h /www"]}'
	deploy_json "$s" '{"name":"multi","image":"'"$IMG"'","replicas":3,"command":["sleep","86400"]}'
	secs=$(wait_converged 120 "$s") || {
		fail setup "not converged: $secs"
		return
	}
	note "converged in ${secs} s"
	vol0=$(docker exec "$s" sh -c 'docker exec $(docker ps -q --filter label=hivemind.app=vol) cat /data/n')
	first0=$(docker exec "$s" sh -c 'docker exec $(docker ps -q --filter label=hivemind.app=vol) cat /data/first')

	for mode in KILL TERM; do
		ids0=$(docker exec "$s" docker ps -q --no-trunc --filter label=hivemind.app | sort | tr '\n' ' ')
		pid=$(hm_pid "$s")
		note "$mode: dockerd gets SIG$mode; serve is pid $pid"
		docker exec "$s" pkill "-$mode" -x dockerd || true
		wait_for 60 sh -c "! docker exec $s pgrep -x dockerd >/dev/null" || fail "$mode-stop" "dockerd still runs 60 s after SIG$mode"
		c0=$(cpu_ticks "$s" "$pid")
		n=$(docker exec "$s" sh -c 'pgrep -fx "sleep 86400" | wc -l')
		note "$mode: $n 'sleep 86400' processes still run with dockerd gone"
		sleep 20
		c1=$(cpu_ticks "$s" "$pid")
		if [ "$(hm_pid "$s")" = "$pid" ]; then pass "$mode-alive" "serve (pid $pid) kept running with dockerd down for 20 s"; else fail "$mode-alive" "serve is not running as pid $pid: '$(hm_pid "$s")'"; fi
		# Clock ticks are 1/100 s: 20 s of a busy loop is 2000.
		if [ $((c1 - c0)) -lt 200 ]; then pass "$mode-no-spin" "serve used $((c1 - c0)) ticks of CPU in 20 s with dockerd down"; else fail "$mode-no-spin" "serve used $((c1 - c0)) ticks of CPU in 20 s with dockerd down"; fi
		fails=$(docker exec "$s" sh -c "grep -c 'reconcile pass failed' /var/log/hivemind.log || true")
		st=$(app_line "$s" multi)
		note "$mode: replica multi/0 while dockerd is down: $st"
		case "$st" in
		unknown\|*ocker*) pass "$mode-status" "status says: $st" ;;
		*) fail "$mode-status" "status during the outage: '$st'" ;;
		esac
		docker exec "$s" sh -c 'tail -n 400 /var/log/hivemind.log' >"$EDGE_OUT/docker-restart-$mode-serve.log"

		ed_dockerd_start "$s"
		wait_for 90 docker exec "$s" docker info || {
			fail "$mode-back" "dockerd did not start again"
			return
		}
		if secs=$(wait_converged 120 "$s"); then
			pass "$mode-converged" "every replica ran again ${secs} s after dockerd was back"
		else
			fail "$mode-converged" "not converged 120 s after dockerd was back: $secs"
		fi
		ids1=$(docker exec "$s" docker ps -aq --no-trunc --filter label=hivemind.app | sort | tr '\n' ' ')
		n=$(docker exec "$s" sh -c 'docker ps -aq --filter label=hivemind.app | wc -l')
		if [ "$n" = 5 ]; then pass "$mode-no-duplicate" "5 containers for 5 replicas"; else fail "$mode-no-duplicate" "$n containers for 5 replicas"; fi
		if [ "$ids0" = "$ids1" ]; then
			note "$mode: the same containers were started again"
		else
			note "$mode: container IDs changed: before $ids0 after $ids1"
		fi
		vol1=$(docker exec "$s" sh -c 'docker exec $(docker ps -q --filter label=hivemind.app=vol) cat /data/n' 2>/dev/null || echo "?")
		first1=$(docker exec "$s" sh -c 'docker exec $(docker ps -q --filter label=hivemind.app=vol) cat /data/first' 2>/dev/null || echo "?")
		if [ "$first1" = "$first0" ] && [ "$vol1" -gt "$vol0" ] 2>/dev/null; then
			pass "$mode-volume" "the volume kept its data: first boot $first0 unchanged, boot count $vol0 -> $vol1"
		else
			fail "$mode-volume" "volume data: first $first0 -> $first1, boot count $vol0 -> $vol1"
		fi
		vol0=$vol1
		if docker exec "$s" wget -qO- http://127.0.0.1:8080/ 2>/dev/null | grep -q edge; then pass "$mode-web" "web answers on 8080 again"; else fail "$mode-web" "web does not answer on 8080"; fi
		r=$(replica_field "$s" multi 0 .restarts)
		note "$mode: restart count of multi/0 after the daemon restart: $r; 'reconcile pass failed' lines so far: $fails"
	done
	docker exec "$s" sh -c 'cat /var/log/hivemind.log' >"$EDGE_OUT/docker-restart-serve.log"
	result "see the CHECK lines"
}

# reg_attempts NODE LOG: the start times (s, relative to the first request) of
# the pull attempts in a request log of edgetool registry. Requests less
# than 3 s apart belong to one attempt.
reg_attempts() {
	docker exec "$1" cat "$2" 2>/dev/null | awk '
		NR == 1 { t0 = $1; last = -1e12 }
		{ if ($1 - last > 3000) printf "%.0f ", ($1 - t0) / 1000; last = $1 }'
}

# log_attempts NODE IMAGE: start times of the agent's "pulling image" lines for
# IMAGE, in s relative to the first.
log_attempts() {
	docker exec "$1" sh -c "grep 'pulling image' /var/log/hivemind.log | grep -F 'image=$2 ' | sed -n 's/^time=\([^ ]*\).*/\1/p'" |
		while read -r ts; do date -u -d "$(echo "$ts" | sed 's/T/ /; s/\..*//; s/Z$//')" +%s 2>/dev/null || echo 0; done |
		awk 'NR == 1 { t0 = $1 } { printf "%d ", $1 - t0 }'
}

# min_gap "T1 T2 ...": the smallest difference between consecutive times, or
# 999 for fewer than two.
min_gap() { echo "$1" | awk '{ m = 999; for (i = 2; i <= NF; i++) { d = $i - $(i - 1); if (d < m) m = d } print m }'; }

# Case 2. Image pull failures.
scenario_pulls() {
	local s=$CL_RUN-pu name t0 st att gap secs calmid n reg lines
	ed_node "$s"
	ed_server "$s"
	docker exec -d "$s" edgetool registry -listen 127.0.0.1:5001 -mode 429 -log /var/log/reg-429.log
	docker exec -d "$s" edgetool registry -listen 127.0.0.1:5002 -mode slow -delay 15m -log /var/log/reg-slow.log
	docker exec -d "$s" edgetool registry -listen 127.0.0.1:5004 -mode notfound -log /var/log/reg-404.log
	deploy_json "$s" '{"name":"calm","image":"'"$IMG"'","command":["sleep","86400"]}'
	secs=$(wait_converged 120 "$s") || {
		fail setup "not converged: $secs"
		return
	}

	declare -A IMAGES=(
		[nosuchimage]=docker.io/hivemindedge/no-such-image-7f3a:1
		[nosuchtag]=busybox:no-such-tag-7f3a
		[noresolve]=registry.invalid/edge/app:1
		[refused]=127.0.0.1:5003/edge/app:1
		[ratelimited]=127.0.0.1:5001/edge/app:1
		[notfound]=127.0.0.1:5004/edge/app:1
	)
	t0=$(now)
	for name in "${!IMAGES[@]}"; do
		deploy_json "$s" '{"name":"'"$name"'","image":"'"${IMAGES[$name]}"'","command":["sleep","86400"]}'
	done
	note "deployed ${#IMAGES[@]} apps with bad images at $(date -u +%T); watching for 150 s"
	sleep 20
	for name in "${!IMAGES[@]}"; do
		st=$(app_line "$s" "$name")
		echo "$name ${IMAGES[$name]} at 20 s: $st" >>"$EDGE_OUT/pulls-status.txt"
		case "$st" in
		pending\|*"${IMAGES[$name]}"*) pass "$name-status" "$st" ;;
		*) fail "$name-status" "replica 0 of $name: '$st'" ;;
		esac
	done
	sleep $((150 - ($(now) - t0)))
	for name in "${!IMAGES[@]}"; do
		att=$(log_attempts "$s" "${IMAGES[$name]}")
		gap=$(min_gap "$att")
		n=$(echo "$att" | wc -w)
		echo "$name: $n pulls, at $att s; smallest gap $gap s" >>"$EDGE_OUT/pulls-status.txt"
		if [ "$n" -ge 2 ] && [ "$n" -le 7 ] && [ "$gap" -ge 9 ]; then
			pass "$name-backoff" "$n pulls in 150 s, at $att s"
		else
			fail "$name-backoff" "$n pulls in 150 s, at $att s (want 2 to 7, at least 9 s apart)"
		fi
	done
	for reg in 429 404; do
		att=$(reg_attempts "$s" "/var/log/reg-$reg.log")
		gap=$(min_gap "$att")
		echo "registry $reg: request bursts at $att s; $(docker exec "$s" sh -c "wc -l < /var/log/reg-$reg.log") requests" >>"$EDGE_OUT/pulls-status.txt"
		if [ "$gap" -ge 9 ]; then pass "registry-$reg-interval" "pull attempts reach the registry at $att s"; else fail "registry-$reg-interval" "pull attempts reach the registry at $att s"; fi
	done
	# A registry that never answers, beside an app that exits. The daemon
	# gives up on the registry after 30 s; the exited app must not wait
	# for that.
	calmid=$(docker exec "$s" docker ps -q --filter label=hivemind.app=calm)
	deploy_json "$s" '{"name":"slow","image":"127.0.0.1:5002/edge/app:1","command":["sleep","86400"]}'
	sleep 5
	st=$(app_line "$s" slow)
	note "slow, 5 s after the deploy: $st"
	echo "slow at 5 s: $st" >>"$EDGE_OUT/pulls-status.txt"
	case "$st" in
	pending\|*"pulling 127.0.0.1:5002/edge/app:1"*) pass slow-status "$st" ;;
	*) fail slow-status "replica 0 of slow, 5 s into a pull that hangs: '$st'" ;;
	esac
	t0=$(now)
	docker exec "$s" docker kill "$calmid" >/dev/null
	if wait_for 30 sh -c "docker exec $s docker ps -q --filter label=hivemind.app=calm --filter status=running | grep -q ."; then
		secs=$(($(now) - t0))
		if [ "$secs" -le 8 ]; then
			pass "slow-other-apps" "calm was restarted ${secs} s after its exit, during the hanging pull"
		else
			fail "slow-other-apps" "calm was restarted only ${secs} s after its exit, during the hanging pull"
		fi
	else
		fail "slow-other-apps" "calm was not restarted within 30 s while a pull hung"
	fi
	sleep 40
	att=$(log_attempts "$s" 127.0.0.1:5002/edge/app:1)
	echo "slow: pulls at $att s" >>"$EDGE_OUT/pulls-status.txt"
	gap=$(min_gap "$att")
	if [ "$gap" -ge 39 ]; then pass slow-backoff "pulls of the hanging registry at $att s"; else fail slow-backoff "pulls of the hanging registry at $att s: the backoff must count from the end of a pull"; fi
	note "nosuchtag while slow hangs: $(app_line "$s" nosuchtag); pulls so far at $(log_attempts "$s" "${IMAGES[nosuchtag]}") s"

	# Fix every spec. Each replica runs within 90 s.
	for name in "${!IMAGES[@]}" slow; do
		deploy_json "$s" '{"name":"'"$name"'","image":"'"$IMG"'","command":["sleep","86400"]}'
	done
	if secs=$(wait_converged 90 "$s"); then
		pass "recovery" "every fixed app runs ${secs} s after the fix"
	else
		fail "recovery" "not converged 90 s after the fix: $secs"
	fi
	for name in "${!IMAGES[@]}" slow; do
		echo "$name after the fix: $(app_line "$s" "$name")" >>"$EDGE_OUT/pulls-status.txt"
	done
	docker exec "$s" sh -c 'cat /var/log/hivemind.log' >"$EDGE_OUT/pulls-serve.log"
	result "see the CHECK lines; details in pulls-status.txt"
}

# Case 3. A node's disk full.
scenario_node_disk() {
	local s=$CL_RUN-nd st c0 c1 p0 p1 secs a
	ED_NO_DOCKER_VOLUME=1 ed_node "$s" --tmpfs /var/lib/docker:rw,exec,dev,suid,size=300m
	ed_server "$s"
	deploy_json "$s" '{"name":"calm","image":"'"$IMG"'","command":["sleep","86400"]}'
	secs=$(wait_converged 120 "$s") || {
		fail setup "not converged: $secs"
		return
	}
	docker exec "$s" sh -c 'dd if=/dev/zero of=/var/lib/docker/edge-fill bs=1M 2>/dev/null; sync; df -h /var/lib/docker | tail -n 1'
	deploy_json "$s" '{"name":"newimg","image":"alpine:3.20","command":["sleep","86400"]}'
	deploy_json "$s" '{"name":"newctr","image":"'"$IMG"'","command":["sleep","86400"]}'
	sleep 10
	c0=$(failed_creates "$s")
	p0=$(agent_pulls "$s" alpine:3.20)
	sleep 60
	c1=$(failed_creates "$s")
	p1=$(agent_pulls "$s" alpine:3.20)
	for a in newimg newctr; do
		st=$(app_line "$s" "$a")
		echo "$a with the disk full: $st" >>"$EDGE_OUT/node-disk.txt"
		case "$st" in
		*"no space left"*) pass "$a-status" "$st" ;;
		*) fail "$a-status" "'$st'" ;;
		esac
	done
	note "in 60 s with the disk full: $((c1 - c0)) container creates, $((p1 - p0)) pulls"
	if [ $((c1 - c0)) -le 8 ]; then pass create-rate "$((c1 - c0)) creates in 60 s"; else fail create-rate "$((c1 - c0)) creates in 60 s: retried every pass"; fi
	if [ $((p1 - p0)) -le 6 ]; then pass pull-rate "$((p1 - p0)) pulls in 60 s"; else fail pull-rate "$((p1 - p0)) pulls in 60 s"; fi
	if [ "$(replica_field "$s" calm 0 .state)" = running ]; then pass calm "the running app kept running"; else fail calm "calm: $(app_line "$s" calm)"; fi
	if docker exec "$s" docker info >/dev/null 2>&1; then pass dockerd "dockerd still answers"; else fail dockerd "dockerd does not answer with its disk full"; fi
	docker exec "$s" rm -f /var/lib/docker/edge-fill
	if secs=$(wait_converged 360 "$s"); then
		pass recovery "both apps run ${secs} s after the space came back"
	else
		fail recovery "not converged 360 s after the space came back: $secs"
	fi
	docker exec "$s" sh -c 'cat /var/log/hivemind.log' >"$EDGE_OUT/node-disk-serve.log"
	result "see the CHECK lines"
}

# Case 4. The server's disk full.
scenario_server_disk() {
	local s=$CL_RUN-sd out code pid secs i ok=0 one0 one1
	ed_node "$s" --tmpfs /var/lib/hivemind:rw,size=16m,mode=0700
	ed_server "$s"
	deploy_json "$s" '{"name":"one","image":"'"$IMG"'","command":["sleep","86400"]}'
	secs=$(wait_converged 120 "$s") || {
		fail setup "not converged: $secs"
		return
	}
	pid=$(hm_pid "$s")
	one0=$(docker exec "$s" sh -c 'docker inspect -f "{{.Id}} {{.State.StartedAt}}" $(docker ps -q --filter label=hivemind.app=one)')
	docker exec "$s" sh -c 'dd if=/dev/zero of=/var/lib/hivemind/edge-fill bs=4k 2>/dev/null; df -k /var/lib/hivemind | tail -n 1'
	# SQLite reuses the space of its WAL file after a checkpoint, so writes
	# can go on for a while with the disk full. Each deploy adds 60 KB of env.
	docker exec "$s" sh -c 'head -c 60000 /dev/zero | tr "\0" x >/tmp/pad'
	for i in $(seq 1 120); do
		docker exec "$s" sh -c "printf '{\"image\":\"$IMG\",\"command\":[\"sleep\",\"86400\"],\"env\":{\"PAD\":\"%s$i\"}}' \"\$(cat /tmp/pad)\" >/tmp/two.json"
		out=$(api "$s" PUT /v1/apps/two @/tmp/two.json)
		code=${out%% *}
		case "$code" in
		2??) ok=$((ok + 1)) ;;
		*) break ;;
		esac
	done
	note "with the disk full, $ok writes of 60 KB went through before one failed: $out"
	echo "after $ok writes, PUT /v1/apps/two: $out" >>"$EDGE_OUT/server-disk.txt"
	case "$code" in
	507) pass write-status "deploy: $out" ;;
	*) fail write-status "deploy: $out" ;;
	esac
	out=$(docker exec "$s" hivemind scale one 2 2>&1 || true)
	echo "hivemind scale one 2: $out" >>"$EDGE_OUT/server-disk.txt"
	case "$out" in
	*space*) pass write-cli "scale: $out" ;;
	*) fail write-cli "scale: $out" ;;
	esac
	out=$(api "$s" GET /v1/status)
	if [ "${out%% *}" = 200 ]; then pass reads "GET /v1/status answers 200"; else fail reads "GET /v1/status: ${out:0:200}"; fi
	if docker exec "$s" hivemind status >/dev/null 2>&1 && docker exec "$s" hivemind nodes >/dev/null 2>&1; then pass reads-cli "status and nodes work"; else fail reads-cli "status or nodes failed"; fi
	# Longer than the 15 s lease, which the server cannot renew for its own
	# node while it cannot write.
	sleep 40
	if [ "$(hm_pid "$s")" = "$pid" ]; then pass serve-up "serve kept running for 40 s with the disk full"; else fail serve-up "serve is not running as pid $pid"; fi
	docker exec "$s" sh -c 'grep -i "full\|space" /var/log/hivemind.log | tail -n 5' >>"$EDGE_OUT/server-disk.txt"
	docker exec "$s" rm -f /var/lib/hivemind/edge-fill
	if wait_for 30 deploy_json "$s" '{"name":"three","image":"'"$IMG"'","command":["sleep","86400"]}'; then
		pass write-again "deploy works once there is space"
	else
		fail write-again "deploy still fails 30 s after the space came back"
	fi
	if secs=$(wait_converged 120 "$s"); then pass converged "converged ${secs} s after"; else fail converged "$secs"; fi
	one1=$(docker exec "$s" sh -c 'docker inspect -f "{{.Id}} {{.State.StartedAt}}" $(docker ps -q --filter label=hivemind.app=one)')
	if [ "$one0" = "$one1" ]; then pass kept "the server's container ran on through the full disk: $one1"; else fail kept "the server's container changed: $one0 -> $one1"; fi
	kill_serve "$s" TERM
	wait_for 30 serve_down "$s" || kill_serve "$s" KILL
	if out=$(docker exec "$s" edgetool storecheck 2>&1); then pass storecheck "$out"; else fail storecheck "$out"; fi
	docker exec "$s" apk add --no-cache sqlite >/dev/null 2>&1 || true
	out=$(docker exec "$s" sqlite3 /var/lib/hivemind/hivemind.db 'PRAGMA integrity_check' 2>&1 || true)
	if [ "$out" = ok ]; then pass integrity "PRAGMA integrity_check: ok"; else fail integrity "PRAGMA integrity_check: $out"; fi
	ed_serve "$s"
	if wait_for 30 docker exec "$s" hivemind status; then pass restart "serve starts again on the same data directory"; else fail restart "serve did not start again"; fi
	docker exec "$s" sh -c 'cat /var/log/hivemind.log' >"$EDGE_OUT/server-disk-serve.log"
	result "see the CHECK lines"
}

# Case 5. kill -9 of serve under write load, 20 times.
scenario_kill_serve() {
	local s=$CL_RUN-ks round out secs acked=0 badrounds=0 wsecs rounds=${KILL_ROUNDS:-20}
	ed_node "$s"
	ed_server "$s"
	docker exec "$s" mkdir -p /var/lib/edge
	for round in $(seq 1 "$rounds"); do
		docker exec -d "$s" sh -c 'edgetool write -acks /var/lib/edge/acks.jsonl >>/var/log/edge-writer.log 2>&1; echo done >/var/lib/edge/writer.done'
		wsecs=$((2 + $(od -An -N1 -tu1 /dev/urandom | tr -d ' ') % 4))
		sleep "$wsecs"
		kill_serve "$s" KILL
		wait_for 60 docker exec "$s" test -f /var/lib/edge/writer.done || note "round $round: the writer did not stop"
		docker exec "$s" rm -f /var/lib/edge/writer.done
		acked=$(docker exec "$s" sh -c 'grep -c "\"kind\":\"ack\"" /var/lib/edge/acks.jsonl || true')
		if ! out=$(docker exec "$s" edgetool storecheck 2>&1); then
			fail "round-$round-store" "$out"
			badrounds=$((badrounds + 1))
		fi
		ed_serve "$s"
		wait_for 60 docker exec "$s" hivemind status || {
			fail "round-$round-restart" "serve did not answer 60 s after the restart"
			return
		}
		if ! out=$(docker exec "$s" edgetool verify -acks /var/lib/edge/acks.jsonl 2>&1); then
			fail "round-$round-acked" "$out"
			badrounds=$((badrounds + 1))
		fi
		if ! secs=$(wait_converged 180 "$s"); then
			fail "round-$round-replicas" "$secs"
			badrounds=$((badrounds + 1))
		fi
		note "round $round: killed after ${wsecs} s of writes; $acked writes acknowledged in all; converged in ${secs} s"
	done
	docker exec "$s" tail -n 40 /var/log/edge-writer.log >"$EDGE_OUT/kill-serve-writer.log" 2>&1 || true
	if [ "$badrounds" = 0 ]; then
		pass all-rounds "$rounds kills: the store was intact, all $acked acknowledged writes were there, and the replicas converged each time"
	else
		fail all-rounds "$badrounds failed checks in $rounds rounds"
	fi
	result "$acked acknowledged writes in $rounds rounds"
}

# Case 8. Crash loops and fast exits.
scenario_crashloop() {
	local s=$CL_RUN-cl a calm0 calm1 st m tok r
	ed_node "$s"
	ed_server "$s"
	deploy_json "$s" '{"name":"calm","image":"'"$IMG"'","command":["sleep","86400"]}'
	wait_converged 120 "$s" >/dev/null || true
	calm0=$(docker exec "$s" sh -c 'docker inspect -f "{{.Id}} {{.State.StartedAt}}" $(docker ps -q --filter label=hivemind.app=calm)')
	deploy_json "$s" '{"name":"exit0","image":"'"$IMG"'","command":["sh","-c","exit 3"]}'
	deploy_json "$s" '{"name":"exit1s","image":"'"$IMG"'","command":["sh","-c","sleep 1; exit 1"]}'
	deploy_json "$s" '{"name":"unhealthy","image":"'"$IMG"'","command":["sleep","86400"],
	 "health":{"exec":["false"],"interval":"2s","timeout":"1s","retries":2}}'
	deploy_json "$s" '{"name":"oom","image":"'"$IMG"'","command":["tail","/dev/zero"],"resources":{"memory":"8Mi"}}'
	note "deployed four failing apps at $(date -u +%T); watching for 180 s"
	sleep 180
	for a in exit0 exit1s unhealthy oom; do
		st=$(replica_field "$s" "$a" 0 '"state=" + .state + " restarts=" + (.restarts|tostring) + " crash_looping=" + ((.crash_looping // false)|tostring) + " health=" + (.health // "") + " error=" + (.error // "")')
		echo "$a after 180 s: $st" >>"$EDGE_OUT/crashloop.txt"
		note "$a: $st"
	done
	docker exec "$s" hivemind status >>"$EDGE_OUT/crashloop.txt" 2>&1 || true
	for a in exit0 exit1s unhealthy oom; do
		r=$(replica_field "$s" "$a" 0 .restarts)
		# 180 s of 1, 2, 4, ... s waits is at most 8 restarts (1+2+...+128 = 255 s).
		if [ "${r:-0}" -ge 2 ] && [ "${r:-0}" -le 9 ]; then pass "$a-backoff" "$r restarts in 180 s"; else fail "$a-backoff" "${r:-?} restarts in 180 s, want 2 to 9"; fi
	done
	st=$(replica_field "$s" oom 0 '.error // ""')
	case "$st" in
	*"out of memory"*) pass oom-status "$st" ;;
	*) fail oom-status "status of the OOM-killed replica says '$st'" ;;
	esac
	st=$(replica_field "$s" exit0 0 '.error // ""')
	case "$st" in
	*"code 3"*) pass exit-status "$st" ;;
	*) fail exit-status "status of the exiting replica says '$st'" ;;
	esac
	st=$(replica_field "$s" unhealthy 0 '.error // ""')
	case "$st" in
	*health*) pass unhealthy-status "$st" ;;
	*) fail unhealthy-status "status of the unhealthy replica says '$st'" ;;
	esac
	tok=$(token "$s")
	m=$(docker exec "$s" sh -c "wget -qO- --header 'Authorization: Bearer $tok' http://127.0.0.1:4483/metrics | grep '^hivemind_container_restarts_total'")
	echo "$m" >>"$EDGE_OUT/crashloop.txt"
	for a in exit0 exit1s unhealthy oom; do
		if echo "$m" | grep "app=\"$a\"" | awk '{ v += $NF } END { exit !(v > 0) }'; then pass "$a-metric" "$(echo "$m" | grep "app=\"$a\"" | tr '\n' ' ')"; else fail "$a-metric" "no restarts in the metric for $a"; fi
	done
	calm1=$(docker exec "$s" sh -c 'docker inspect -f "{{.Id}} {{.State.StartedAt}}" $(docker ps -q --filter label=hivemind.app=calm)')
	if [ "$calm0" = "$calm1" ]; then pass calm "calm ran the whole time, same container and start time"; else fail calm "calm changed: $calm0 -> $calm1"; fi
	docker exec "$s" sh -c 'cat /var/log/hivemind.log' >"$EDGE_OUT/crashloop-serve.log"
	result "see the CHECK lines; details in crashloop.txt"
}

# ed_join SERVER WORKER: serve SERVER with --advertise, and join WORKER to it.
# Sets WID to the worker's node ID.
ed_join() {
	local s=$1 w=$2 sip tok
	sip=$(cl_ip_of "$s")
	ED_SERVE_FLAGS=(--advertise "$sip")
	docker exec "$s" hivemind init >/dev/null
	ed_serve "$s"
	wait_for 30 docker exec "$s" hivemind status || cl_fail "the API on $s did not answer"
	tok=$(docker exec "$s" hivemind join-token --ttl 1h)
	docker exec -d -e "HM_TOKEN=$tok" "$w" sh -c "exec hivemind join $sip:4484 --token \"\$HM_TOKEN\" >>/var/log/hivemind.log 2>&1"
	wait_for 60 docker exec "$w" test -s /var/lib/hivemind/node.json || cl_fail "$w did not join"
	WID=$(docker exec "$w" sh -c "sed -n 's/.*\"node_id\": *\"\(node-[0-9a-f]*\)\".*/\1/p' /var/lib/hivemind/node.json")
}

# Case 9. logs -f across a server restart.
scenario_logs_restart() {
	local s=$CL_RUN-ls w=$CL_RUN-lw idx_w idx_s mode t0 rc el err i where
	ed_node "$s"
	ed_node "$w"
	ed_join "$s" "$w"
	sleep 5
	deploy_json "$s" '{"name":"chatty","image":"'"$IMG"'","replicas":2,"ports":[{"host":18080,"container":80}],
	 "command":["sh","-c","while :; do date; sleep 1; done"]}'
	wait_for 90 running_count_is "$s" chatty 2 || {
		fail setup "chatty is not running on both nodes: $(docker exec "$s" hivemind status chatty)"
		return
	}
	idx_w=$(docker exec "$s" sh -c "hivemind status --json | jq -r '.[] | select(.name == \"chatty\") | .replicas[] | select(.node_id == \"$WID\") | .index'")
	idx_s=$((1 - idx_w))
	note "chatty replica $idx_w is on the worker, $idx_s on the server"
	for mode in KILL TERM; do
		for i in "$idx_w" "$idx_s"; do
			docker exec -d "$s" sh -c "rm -f /tmp/logs-$i.*; start=\$(date +%s); hivemind logs -f chatty $i >/tmp/logs-$i.out 2>/tmp/logs-$i.err; echo \$? \$(( \$(date +%s) - start )) >/tmp/logs-$i.rc"
		done
		sleep 6
		for i in "$idx_w" "$idx_s"; do
			[ "$(docker exec "$s" sh -c "wc -l </tmp/logs-$i.out")" -ge 2 ] || fail "$mode-streaming-$i" "logs -f of replica $i printed nothing in 6 s"
		done
		t0=$(now)
		kill_serve "$s" "$mode"
		note "$mode: serve signalled at $(date -u +%T)"
		for i in "$idx_w" "$idx_s"; do
			where=worker
			[ "$i" = "$idx_s" ] && where=server
			if wait_for 90 docker exec "$s" test -s "/tmp/logs-$i.rc"; then
				rc=$(docker exec "$s" cat "/tmp/logs-$i.rc")
				el=$(($(now) - t0))
				err=$(docker exec "$s" sh -c "tail -n 3 /tmp/logs-$i.err | tr '\n' ' '")
				echo "$mode $where replica $i: exit ${rc% *} after ~${el} s; stderr: $err" >>"$EDGE_OUT/logs-restart.txt"
				if [ "${rc% *}" != 0 ] && echo "$err" | grep -qi "server\|connection"; then
					pass "$mode-$where" "logs -f ended with exit ${rc% *} within ${el} s: $err"
				else
					fail "$mode-$where" "logs -f ended with exit ${rc% *} and stderr '$err': the stream end is not reported"
				fi
			else
				fail "$mode-$where" "logs -f of the $where's replica still runs 90 s after serve stopped"
				docker exec "$s" pkill -f "hivemind logs -f chatty $i" || true
			fi
		done
		wait_for 30 serve_down "$s" || kill_serve "$s" KILL
		ed_serve "$s"
		wait_for 60 docker exec "$s" hivemind status || {
			fail "$mode-restart" "serve did not come back"
			return
		}
		if wait_for 60 sh -c "docker exec $s hivemind logs --tail 2 chatty $idx_w | grep -q ."; then
			pass "$mode-worker-after" "the worker's replica log is readable again after the restart"
		else
			fail "$mode-worker-after" "hivemind logs of the worker's replica fails 60 s after the restart"
		fi
	done
	docker exec "$s" sh -c 'cat /var/log/hivemind.log' >"$EDGE_OUT/logs-restart-serve.log"
	docker exec "$w" sh -c 'cat /var/log/hivemind.log' >"$EDGE_OUT/logs-restart-worker.log"
	result "see the CHECK lines; details in logs-restart.txt"
}

# Case 10. A worker's data directory damaged.
scenario_datadir() {
	local s=$CL_RUN-dd w=$CL_RUN-dw what out rc p
	ed_node "$s"
	ed_node "$w"
	ed_join "$s" "$w"
	sleep 3
	for p in $(docker exec "$w" pgrep -x hivemind); do docker exec "$w" kill "$p" || true; done
	wait_for 20 sh -c "! docker exec $w pgrep -x hivemind" || true
	docker exec "$w" cp -a /var/lib/hivemind /var/lib/hivemind.good
	# Another cluster's CA, for the replaced ca.crt.
	docker exec "$w" sh -c 'hivemind init --data-dir /var/lib/other --client-config /tmp/other.yaml >/dev/null'
	for what in key-missing state-corrupt state-empty ca-replaced cert-missing; do
		docker exec "$w" sh -c 'rm -rf /var/lib/hivemind && cp -a /var/lib/hivemind.good /var/lib/hivemind'
		case $what in
		key-missing) docker exec "$w" rm /var/lib/hivemind/node.key ;;
		state-corrupt) docker exec "$w" sh -c 'head -c 20 /var/lib/hivemind.good/node.json >/var/lib/hivemind/node.json' ;;
		state-empty) docker exec "$w" sh -c ': >/var/lib/hivemind/node.json' ;;
		ca-replaced) docker exec "$w" cp /var/lib/other/ca.crt /var/lib/hivemind/ca.crt ;;
		cert-missing) docker exec "$w" rm /var/lib/hivemind/node.crt ;;
		esac
		rc=0
		out=$(docker exec "$w" timeout 20 hivemind serve --role worker 2>&1) || rc=$?
		out=$(echo "$out" | tail -n 3 | tr '\n' ' ')
		echo "$what: exit $rc: $out" >>"$EDGE_OUT/datadir.txt"
		if [ "$rc" != 0 ] && [ "$rc" != 124 ] && [ "$rc" != 143 ] && echo "$out" | grep -qi 'join\|restore\|backup'; then
			pass "$what" "exit $rc: $out"
		else
			fail "$what" "exit $rc: $out"
		fi
	done
	docker exec "$w" sh -c 'rm -rf /var/lib/hivemind && cp -a /var/lib/hivemind.good /var/lib/hivemind'
	docker exec -d "$w" sh -c 'exec hivemind serve --role worker >>/var/log/hivemind.log 2>&1'
	if wait_for 30 sh -c "docker exec $w grep -c 'worker running' /var/log/hivemind.log | grep -qx 2"; then pass intact "the restored data directory starts"; else fail intact "the restored data directory does not start"; fi
	result "see the CHECK lines; messages in datadir.txt"
}

# Case 7. The largest spec, deployed for real.
scenario_limits() {
	local s=$CL_RUN-li out secs
	ed_node "$s"
	ed_server "$s"
	docker exec "$s" sh -c '
		pad() { head -c "$1" /dev/zero | tr "\0" x; }
		args=""; for i in $(seq 1 61); do args="$args,\"$(pad 4096)\""; done
		env=""; for i in $(seq 1 128); do env="$env\"E$(printf %03d $i)\":\"$(pad 508)\","; done
		ports=""; for i in $(seq 1 32); do ports="$ports{\"host\":$((20000 + i)),\"container\":$((80 + i))},"; done
		vols=""; for i in $(seq 1 16); do vols="$vols{\"name\":\"v$i\",\"path\":\"/data/v$i\"},"; done
		routes=""; for i in $(seq 1 16); do routes="$routes{\"host\":\"r$i.edge.example\",\"port\":8080},"; done
		hc=""; for i in $(seq 1 63); do hc="$hc,\"$(pad 4096)\""; done
		printf "{\"name\":\"%s\",\"image\":\"busybox:stable\",\"command\":[\"sh\",\"-c\",\"exec sleep 86400\"%s],\"env\":{%s},\"ports\":[%s],\"volumes\":[%s],\"routes\":[%s],\"health\":{\"exec\":[\"true\"%s],\"interval\":\"5s\"}}" \
			"$(pad 31)y" "$args" "${env%,}" "${ports%,}" "${vols%,}" "${routes%,}" "$hc" >/tmp/largest.json'
	note "the largest spec is $(docker exec "$s" sh -c 'wc -c </tmp/largest.json') bytes"
	out=$(api "$s" POST /v1/apps @/tmp/largest.json)
	case "${out%% *}" in
	201) pass largest-accepted "the API accepted it" ;;
	*) fail largest-accepted "${out:0:300}" ;;
	esac
	if secs=$(wait_converged 120 "$s"); then
		pass largest-runs "its replica runs, ${secs} s after the deploy"
	else
		fail largest-runs "$secs"
	fi
	deploy_json "$s" '{"name":"hundred","image":"'"$IMG"'","replicas":100,"command":["sleep","86400"]}'
	if secs=$(wait_converged 600 "$s"); then pass hundred "100 replicas run, ${secs} s after the deploy"; else fail hundred "$secs"; fi
	out=$(api "$s" POST /v1/apps/hundred/scale '{"replicas":101}')
	case "${out%% *}" in
	400) pass hundred-and-one "$out" ;;
	*) fail hundred-and-one "$out" ;;
	esac
	result "see the CHECK lines"
}

# ---------------------------------------------------------------- main

note "building hivemind and edgetool"
cl_build
docker run --rm -v "$REPO:/src" -v "$CACHE_VOLUME:/cache" -w /src \
	-e GOMODCACHE=/cache/mod -e GOCACHE=/cache/build -e GOFLAGS=-buildvcs=false \
	-e GOPROXY=direct -e GOSUMDB=off -e GOTOOLCHAIN=local -e CGO_ENABLED=0 \
	"$GO_IMAGE" go build -trimpath -o "/src/${CL_WORK#"$REPO"/}/edgetool" ./test/edgetool
cl_create_network

for CUR in "${SCENARIOS[@]}"; do
	note "starting at $(date -u +%T)"
	start=$(now)
	before=${#CL_NAMES[@]}
	"scenario_${CUR//-/_}" || fail scenario "the scenario stopped early (exit $?)"
	note "done in $(($(now) - start)) s"
	# Free the scenario's nodes before the next one.
	for ((i = before; i < ${#CL_NAMES[@]}; i++)); do ed_rm_node "${CL_NAMES[$i]}"; done
done

echo
echo "edge suite: $FAILS failed checks"
cat "$SUMMARY"
[ "$FAILS" = 0 ]
