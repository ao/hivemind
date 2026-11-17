#!/usr/bin/env bash
# The probe loop. Runs until killed (scenario.sh probe-start / probe-stop).
#
# Every 10 s, one "probe" line and its details:
#   - api: whether hivemind status --json answered on the server;
#   - converged: the fault suite's check (scripts/lib/converged.awk) on the
#     server's status and every node's containers; "held" is the same check
#     allowing a volume replica that waits for its lost node;
#   - dups: (app, replica) pairs with a running container on more than one node;
#   - one "reach" line per replica that publishes a port, on the node the
#     server assigns it: curl of web on 127.0.0.1:8080, and a GET of the kv
#     value on 127.0.0.1:6379, both run on that node over ssh. While the API
#     is down, the last assignment it gave is used.
# Every 60 s, one "res" line per node (the hivemind process's RSS, threads,
# CPU time, open fds, start time; on the server the store's files; Docker's
# dangling volumes, and the largest json-file log of a hivemind container
# and the most such files one has), fifteen
# "lat" lines (five each of GET /v1/status, /v1/nodes and /v1/apps on the
# server: curl's time_total and the reply's size), and one "metrics" line
# (whether /metrics has go_goroutines, and its value).
set -uo pipefail
. "$(dirname "$0")/lib.sh"

INTERVAL=${PROBE_INTERVAL:-10}
RES_EVERY=${RES_EVERY:-6}
STATE=$SOAK_DIR/probe-state.tsv
LAST=$SOAK_DIR/last-assign.tsv
KV_VALUE=$(cat "$SOAK_DIR/kv-value" 2>/dev/null || true)

# On a node: the checks for the apps it should serve, one line each.
read -r -d '' REMOTE_WEB <<'EOF' || true
r=$(curl -sS -o /dev/null -m 3 -w '%{http_code} %{time_total}' http://127.0.0.1:$P/ 2>&1) && [ "${r%% *}" = 200 ] && echo "ok code=${r% *} s=${r#* }" || echo "fail err=\"$(echo "$r" | tr -d '\n"' | cut -c1-80)\""
EOF
read -r -d '' REMOTE_KV <<'EOF' || true
v=$(timeout 3 bash -c 'exec 3<>/dev/tcp/127.0.0.1/6379 || exit 1; printf "GET soak:canary\r\n" >&3; IFS= read -r -t 2 a <&3; IFS= read -r -t 2 b <&3; printf "%s|%s" "${a%$(printf "\r")}" "${b%$(printf "\r")}"' 2>/dev/null) && echo "ok reply=$v" || echo "fail err=connect"
EOF

read -r -d '' REMOTE_RES <<'EOF' || true
pid=$(systemctl show -p MainPID --value hivemind.service 2>/dev/null)
la=$(cut -d' ' -f1 /proc/loadavg); ma=$(awk '/^MemAvailable/ { print $2 }' /proc/meminfo)
if [ -z "$pid" ] || [ "$pid" = 0 ] || [ ! -d /proc/$pid ]; then echo "pid=0 load1=$la memavail_kb=$ma"; exit 0; fi
rss=$(awk '/^VmRSS/ { print $2 }' /proc/$pid/status); thr=$(awk '/^Threads/ { print $2 }' /proc/$pid/status)
set -- $(sed 's/^.*) //' /proc/$pid/stat)
fds=$(ls /proc/$pid/fd | wc -l)
st=""
for f in hivemind.db hivemind.db-wal hivemind.db-shm; do
	[ -f /var/lib/hivemind/$f ] && st="$st ${f#hivemind.}_bytes=$(stat -c %s /var/lib/hivemind/$f)"
done
cs=$(docker ps -q --filter label=hivemind.app | wc -l)
# Volumes no container uses (anonymous volumes left behind), and the json-file
# logs of hivemind's containers: the most files one container has, and the
# largest file.
dv=$(docker volume ls -qf dangling=true | wc -l)
lg=$(for c in $(docker ps -aq --no-trunc --filter label=hivemind.app); do find /var/lib/docker/containers/$c -maxdepth 1 -name '*-json.log*' -printf '%s\n' 2>/dev/null | awk '{ n++; if ($1 > m) m = $1 } END { printf "%d %d\n", n, m }'; done | awk '{ if ($1 > f) f = $1; if ($2 > m) m = $2 } END { printf "logfiles_max=%d logbytes_max=%d", f, m }')
echo "pid=$pid rss_kb=$rss threads=$thr utime=${12} stime=${13} starttime=${20} clk_tck=$(getconf CLK_TCK) fds=$fds$st containers=$cs dangling_volumes=$dv $lg load1=$la memavail_kb=$ma"
EOF

read -r -d '' REMOTE_LAT <<'EOF' || true
tok=$(cat /var/lib/hivemind/admin.token) || exit 1
cfg() { printf 'header = "Authorization: Bearer %s"\n' "$tok"; }
for r in status nodes apps; do
	for i in 1 2 3 4 5; do
		cfg | curl -sS -o /dev/null -K - -m 5 -w "%{http_code} %{time_total} $r %{size_download}\n" http://127.0.0.1:4483/v1/$r 2>/dev/null || echo "fail 0 $r 0"
	done
done
if m=$(cfg | curl -sS -K - -m 5 http://127.0.0.1:4483/metrics 2>/dev/null); then
	g=$(printf '%s\n' "$m" | awk '$1 == "go_goroutines" { print $2 }')
	# the nodes whose routers have series, by the node label
	rn=$(printf '%s\n' "$m" | awk -F'node="' '/^hivemind_router_requests_total\{/ { split($2, a, "\""); if (!(a[1] in n)) { n[a[1]] = 1; c++ } } END { printf "%d", c }')
	echo "metrics lines=$(printf '%s\n' "$m" | wc -l) go_goroutines=${g:-absent} router_nodes=${rn:-0}"
else
	echo "metrics fail"
fi
EOF

# reach_node NAME ID ROWS: the reach checks for NAME, from the R rows it is assigned.
reach_node() {
	local n=$1 id=$2 rows=$3 cmd="" line out
	while IFS=$'\t' read -r _ app idx _; do
		case "$app" in
		web) cmd+="P=8080; printf 'web $idx '; $REMOTE_WEB"$'\n' ;;
		front) cmd+="P=8081; printf 'front $idx '; $REMOTE_WEB"$'\n' ;;
		kv) cmd+="printf 'kv $idx '; $REMOTE_KV"$'\n' ;;
		esac
	done <<<"$rows"
	[ -n "$cmd" ] || return 0
	if out=$(on "$n" "$cmd" 2>/dev/null) && [ -n "$out" ]; then
		while read -r app idx line; do
			if [ "$app" = kv ] && [[ "$line" == ok* ]]; then
				# the reply is "$<len>|<value>": the value must be the one written at setup
				if [ "${line#*|}" = "$KV_VALUE" ]; then line="ok value=match"; else line="fail value=\"${line#ok reply=}\""; fi
			fi
			ev reach "app=$app replica=$idx node=$n $line"
		done <<<"$out"
	else
		while IFS=$'\t' read -r _ app idx _; do
			case "$app" in web | kv | front) ev reach "app=$app replica=$idx node=$n fail err=ssh" ;; esac
		done <<<"$rows"
	fi
}

probe_once() {
	local t0 api conv held d un reasons i n id rows src pids=()
	t0=$(now_ms)
	collect "$STATE"
	if api_ok "$STATE"; then
		api=ok
		awk -F'\t' '$1 == "R"' "$STATE" >"$LAST.tmp" && mv "$LAST.tmp" "$LAST"
		src=live
		if reasons=$(check_converged "$STATE" 0); then conv=yes; else conv=no; fi
		if check_converged "$STATE" 1 >/dev/null; then held=yes; else held=no; fi
	else
		api=fail conv=no held=no src=last reasons="api did not answer"
	fi
	d=$(dups "$STATE" | tr '\n' ';')
	un=$(unreachable "$STATE")
	ev probe "api=$api converged=$conv held=$held running=$(running "$STATE") dups=$(dups "$STATE" | wc -l | tr -d ' ') unreachable=${un:-none} took_ms=$(($(now_ms) - t0))"
	[ "$conv" = yes ] || ev probe-why "$(tr '\n' ';' <<<"$reasons" | cut -c1-400)"
	[ -z "$d" ] || ev dup "$d"
	# Reach checks on the nodes the server assigns (or last assigned) the port apps.
	[ -f "$LAST" ] || return 0
	for i in "${!NAMES[@]}"; do
		n=${NAMES[$i]}
		id=$(id_of "$n")
		rows=$(awk -F'\t' -v id="$id" '$1 == "R" && $4 == id && $9 == "running" && ($2 == "web" || $2 == "kv" || $2 == "front")' "$LAST")
		[ -n "$rows" ] || continue
		reach_node "$n" "$id" "$rows" &
		pids+=($!)
	done
	[ ${#pids[@]} -eq 0 ] || wait "${pids[@]}"
	ev probe-src "assign=$src took_ms=$(($(now_ms) - t0))"
}

resources_once() {
	local n out line
	for n in "${NAMES[@]}"; do
		(
			if out=$(on "$n" "sudo bash -s" <<<"$REMOTE_RES" 2>/dev/null); then
				ev res "node=$n $out"
			else
				ev res "node=$n fail"
			fi
		) &
	done
	if out=$(on "$SERVER" "sudo bash -s" <<<"$REMOTE_LAT" 2>/dev/null); then
		while read -r a b c d; do
			case "$a" in
			metrics) ev metrics "$b $c $d" ;;
			fail) ev lat "fail route=/v1/$c" ;;
			*) ev lat "code=$a s=$b route=/v1/$c bytes=$d" ;;
			esac
		done <<<"$out"
	else
		ev lat "fail err=ssh"
	fi
	wait
}

trap 'ev probe-loop "stop"; exit 0' TERM INT
ev probe-loop "start interval=${INTERVAL}s resources_every=$((INTERVAL * RES_EVERY))s nodes=${NAMES[*]}"
start=$(date +%s)
k=0
while :; do
	if [ $((k % RES_EVERY)) -eq 0 ]; then resources_once & fi
	probe_once
	k=$((k + 1))
	next=$((start + k * INTERVAL))
	nowt=$(date +%s)
	if [ "$nowt" -lt "$next" ]; then
		sleep $((next - nowt))
	else
		ev probe-loop "late by $((nowt - next)) s"
		# skip the cycles we missed, keep the 10 s grid
		k=$(((nowt - start) / INTERVAL + 1))
		sleep $((start + k * INTERVAL - nowt))
	fi
done
