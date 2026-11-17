#!/usr/bin/env bash
# The soak scenarios, and the background loops.
#
#   scenario.sh probe-start | probe-stop | watch-start | watch-stop
#   scenario.sh mark TEXT...                  a "mark" line in the log
#   scenario.sh begin NAME TARGET             wait for convergence; save the state before a fault
#   scenario.sh measure NAME TARGET MODE [SECS]
#   scenario.sh reboot TARGET                 sudo systemctl reboot on TARGET, measured
#   scenario.sh partition TARGET [SECS]       iptables DROP from TARGET to the server's
#                                             cluster port for SECS (60), measured
#   scenario.sh server-backup                 hivemind backup on the server, copied here
#   scenario.sh server-replace [SECS]         wait for the server machine to go and come
#                                             back empty; install, restore, start; measured
#   scenario.sh server-move NEW NEWIP MODE    the server lost, restored on NEW at another
#                                             address; MODE name (change the record of
#                                             $SERVER_NAME) or ip (set-server on each
#                                             worker); measured
#   scenario.sh dns IP                        point $SERVER_NAME at IP on every machine
#   scenario.sh set-server-all IP             stop, set-server IP, start, on each worker
#   scenario.sh set-server-checks T ADDR...   set-server on T to each ADDR must be refused
#   scenario.sh metrics-snap LABEL            the server's router series, to a file
#   scenario.sh metrics-watch SECS            log the nodes with router series, every 5 s
#   scenario.sh kv-check NAME                 read the kv value where kv runs
#   scenario.sh upgrade                       install $BIN everywhere, restart the server, then
#                                             each worker once the last one's routed
#                                             replicas are ready
#   scenario.sh end NAME
#
# A fault that needs the machine's owner (power off, power on, a new machine)
# is done outside this script; "measure" observes it. MODE is:
#   loss    after a power-off: until the node is lost, its replicas without a
#           volume run elsewhere, and the cluster has converged, allowing a
#           volume replica that waits for the node;
#   return  after a power-on: until the node is ready, its old containers are
#           gone, and the cluster has converged;
#   full    both, for a fault that ends by itself (reboot, partition).
# Each observation is an "event" line the first time it holds, and a line on
# each change of the port-22, node-state, convergence and duplicate checks.
set -uo pipefail
. "$(dirname "$0")/lib.sh"
HERE=$(cd "$(dirname "$0")" && pwd)

# place_spread: delete spread and deploy it again, so that every node holds a
# replica. Placed replicas never move, so a node that came back holds nothing.
place_spread() {
	local spec
	spec="{\"name\":\"spread\",\"image\":\"${SPREAD_IMAGE:-busybox:stable}\",\"replicas\":6,\"command\":[\"sleep\",\"86400\"]}"
	note "placing spread again so that every node holds a replica"
	hm "$SERVER" delete spread >/dev/null
	wait_for 300 converged_now || die "not converged after deleting spread"
	sleep 40
	hm "$SERVER" "deploy -f -" <<<"$spec" >/dev/null || die "deploy spread"
	wait_for 300 converged_now || die "not converged after deploying spread"
}

converged_now() { snapshot "$SOAK_DIR/now.tsv" && check_converged "$SOAK_DIR/now.tsv" >/dev/null; }

# movable FILE ID: the replicas without a volume that FILE places on ID.
movable() {
	awk -F'\t' -v id="$2" -v vol=" $VOLUME_APPS " '$1 == "R" && $4 == id && $9 == "running" && index(vol, " " $2 " ") == 0 { print $2 "|" $3 }' "$1"
}
held_on() {
	awk -F'\t' -v id="$2" -v vol=" $VOLUME_APPS " '$1 == "R" && $4 == id && $9 == "running" && index(vol, " " $2 " ") > 0 { print $2 "|" $3 }' "$1"
}

begin() {
	local name=$1 T=$2 pre=$SOAK_DIR/pre-$1.tsv id
	ev scenario "$name begin target=$T"
	wait_for 600 converged_now || die "$name: not converged before the fault: $(check_converged "$SOAK_DIR/now.tsv" | tr '\n' ';')"
	id=$(id_of "$T")
	if [ "$T" != "$SERVER" ] && [ -z "$(movable "$SOAK_DIR/now.tsv" "$id")" ]; then place_spread; fi
	snapshot "$pre" || die "$name: no status"
	ev scenario "$name ready target=$T movable=$(movable "$pre" "$id" | tr '\n' ' ') held=$(held_on "$pre" "$id" | tr '\n' ' ') containers=$(awk -F'\t' -v n="$T" '$1 == "C" && $2 == n { printf "%s:%s|%s,", $3, $6, $7 }' "$pre")"
	ev scenario "$name placement $(awk -F'\t' 'FILENAME == ARGV[1] { split($0, x, " "); nm[x[1]] = x[2]; next } $1 == "R" { printf "%s|%s@%s ", $2, $3, nm[$4] }' "$IDS_FILE" "$pre")"
}

end_scenario() {
	local name=$1
	snapshot "$SOAK_DIR/now.tsv" || true
	ev scenario "$name end converged=$(check_converged "$SOAK_DIR/now.tsv" >/dev/null && echo yes || echo no) placement $(awk -F'\t' 'FILENAME == ARGV[1] { split($0, x, " "); nm[x[1]] = x[2]; next } $1 == "R" { printf "%s|%s@%s ", $2, $3, nm[$4] }' "$IDS_FILE" "$SOAK_DIR/now.tsv")"
}

# old_on FILE PRE T KEYS: the pre-fault containers of T, for the replicas in
# KEYS, that T still has. Empty when they are all gone.
old_on() {
	awk -F'\t' -v n="$3" -v keys=" $4 " '
		FILENAME == ARGV[1] { if ($1 == "C" && $2 == n && index(keys, " " $6 "|" $7 " ") > 0) old[$3] = $6 "|" $7; next }
		$1 == "C" && $2 == n && ($3 in old) { printf "%s:%s,", $3, old[$3] }' "$2" "$1"
}

measure() {
	local name=$1 T=$2 mode=$3 secs=${4:-900}
	local id pre=$SOAK_DIR/pre-$1.tsv cur=$SOAK_DIR/measure-$1.tsv end t d v s c h
	local keys held p22=up st=ready conv="" hconv="" allrun="" dupl="" lost=0 moved=0 oldrm=0 back=0 kvdone=0 olds="" where
	end=$(($(date +%s) + secs))
	id=$(id_of "$T")
	keys=$(movable "$pre" "$id" | tr '\n' ' ')
	held=$(held_on "$pre" "$id" | tr '\n' ' ')
	[ -n "${keys// /}" ] || moved=1
	if [ "$mode" = return ]; then p22=down st=lost lost=1 moved=1; fi
	ev measure "$name begin target=$T mode=$mode movable=$keys held=$held"
	while [ "$(date +%s)" -lt "$end" ]; do
		t=$(now_ms)
		if port22 "$T"; then v=up; else v=down; fi
		[ "$v" = "$p22" ] || {
			ev event "$name $T port22-$v"
			p22=$v
		}
		collect "$cur" nodes
		if api_ok "$cur"; then
			s=$(node_state "$cur" "$id")
			if [ -n "$s" ] && [ "$s" != "$st" ]; then
				ev event "$name $T node-$s"
				st=$s
				[ "$s" = lost ] && lost=1
				[ "$s" = ready ] && [ "$lost" = 1 ] && back=1
			fi
			if [ "$moved" = 0 ]; then
				where=$(awk -F'\t' -v id="$id" -v keys=" $keys " 'FILENAME == ARGV[1] { split($0, x, " "); nm[x[1]] = x[2]; next }
					$1 == "R" && index(keys, " " $2 "|" $3 " ") > 0 { n++; if ($4 != id && $4 != "" && $5 == "running") { ok++; w = w $2 "|" $3 "@" nm[$4] " " } }
					END { if (ok == n) print w }' "$IDS_FILE" "$cur")
				if [ -n "$where" ]; then
					ev event "$name $T replicas-elsewhere $where"
					moved=1
				fi
			fi
			if check_converged "$cur" 0 >/dev/null; then c=yes; else c=no; fi
			if check_converged "$cur" 1 >/dev/null; then h=yes; else h=no; fi
			[ "$c" = "$conv" ] || {
				ev event "$name converged-$c"
				conv=$c
			}
			[ "$h" = "$hconv" ] || {
				ev event "$name converged-held-$h"
				hconv=$h
			}
			v=$(running "$cur")
			if [ "${v%/*}" = "${v#*/}" ]; then v=yes; else v=no; fi
			[ "$v" = "$allrun" ] || {
				ev event "$name all-running-$v $(running "$cur")"
				allrun=$v
			}
		fi
		d=$(dups "$cur" | tr '\n' ';')
		[ "$d" = "$dupl" ] || {
			ev event "$name dups ${d:-none}"
			dupl=$d
		}
		# The containers T had before the fault, for replicas that moved, must go
		# once T is back in touch.
		if [ "$oldrm" = 0 ] && [ "$moved" = 1 ] && [ -n "${keys// /}" ] && ! grep -q "^U	$T$" "$cur"; then
			v=$(old_on "$cur" "$pre" "$T" "$keys")
			if [ -z "$v" ] && { [ "$lost" = 1 ] || [ "$mode" = return ]; } && [ "$p22" = up ]; then
				ev event "$name $T old-containers-removed"
				oldrm=1
			elif [ -n "$v" ] && [ "$v" != "$olds" ]; then
				ev event "$name $T old-containers-present $v"
				olds=$v
			fi
		fi
		if [ -n "${held// /}" ] && [ "$back" = 1 ] && [ "$kvdone" = 0 ] &&
			awk -F'\t' -v id="$id" '$1 == "R" && $2 == "kv" && $4 == id && $5 == "running" { f = 1 } END { exit !f }' "$cur"; then
			v=$(kv_get "$T" 2>/dev/null)
			if [ -n "$v" ]; then
				if [ "$v" = "$(cat "$SOAK_DIR/kv-value")" ]; then ev event "$name kv-value-match"; else ev event "$name kv-value-mismatch got=$v"; fi
				kvdone=1
			fi
		fi
		case "$mode" in
		loss) [ "$lost" = 1 ] && [ "$moved" = 1 ] && [ "$hconv" = yes ] && break ;;
		return) [ "$st" = ready ] && [ "$conv" = yes ] && { [ "$oldrm" = 1 ] || [ -z "${keys// /}" ]; } && { [ -z "${held// /}" ] || [ "$kvdone" = 1 ]; } && break ;;
		full) [ "$lost" = 1 ] && [ "$st" = ready ] && [ "$conv" = yes ] && { [ "$oldrm" = 1 ] || [ -z "${keys// /}" ]; } && { [ -z "${held// /}" ] || [ "$kvdone" = 1 ]; } && break ;;
		esac
		d=$((1000 - ($(now_ms) - t)))
		[ "$d" -le 0 ] || sleep "$((d / 1000)).$(printf '%03d' $((d % 1000)))"
	done
	if [ "$(date +%s)" -ge "$end" ]; then
		ev event "$name measure-timeout mode=$mode lost=$lost moved=$moved old_removed=$oldrm state=$st converged=$conv held=$hconv"
		return 1
	fi
	ev measure "$name end mode=$mode"
}

reboot_node() {
	local T=$1 name=reboot-worker
	begin "$name" "$T"
	ev event "$name $T fault reboot"
	on "$T" "sudo systemctl reboot" >/dev/null 2>&1 || true
	measure "$name" "$T" full 900
	end_scenario "$name"
}

partition() {
	local T=$1 secs=${2:-60} name=partition-worker ip
	ip=$SERVER_IP
	begin "$name" "$T"
	measure "$name" "$T" full 900 &
	local mpid=$!
	sleep 1
	on "$T" "sudo iptables -I OUTPUT -p tcp -d $ip --dport 4484 -j DROP" || die "partition: iptables on $T"
	ev event "$name $T fault partition to $ip:4484"
	sleep "$secs"
	on "$T" "sudo iptables -D OUTPUT -p tcp -d $ip --dport 4484 -j DROP" || die "heal: iptables on $T"
	ev event "$name $T heal"
	wait "$mpid"
	end_scenario "$name"
}

server_backup() {
	local f=/var/tmp/hivemind-soak.tar
	on "$SERVER" "sudo rm -f $f && sudo $HM backup $f" >/dev/null || die "hivemind backup on $SERVER"
	(umask 077 && SSH_TIMEOUT=60 on "$SERVER" "sudo cat $f" >"$SOAK_DIR/backup.tar") || die "copying the backup"
	chmod 600 "$SOAK_DIR/backup.tar"
	ev event "lose-server backup bytes=$(stat -c %s "$SOAK_DIR/backup.tar") sha256=$(sha256sum "$SOAK_DIR/backup.tar" | cut -d' ' -f1) entries=$(tar -tf "$SOAK_DIR/backup.tar" | tr '\n' ',')"
}

# restore_server: on the new, empty server machine, as docs/backup.md says.
restore_server() {
	install_bin "$SERVER" || return 1
	# The machine set-up that is not hivemind's state: hosts, trust, certs.d.
	if [ "$SOAK_ROUTES" = 1 ]; then prepare_node "$SERVER" || return 1; fi
	(SSH_TIMEOUT=60 on "$SERVER" "umask 077; cat > /var/tmp/hivemind-soak.tar" <"$SOAK_DIR/backup.tar") || return 1
	on "$SERVER" "sudo $HM restore /var/tmp/hivemind-soak.tar --data-dir $DATA && rm -f /var/tmp/hivemind-soak.tar" >/dev/null || return 1
	ev event "lose-server restored"
	install_unit "$SERVER" "$(server_unit "$SERVER_IP")" || return 1
	ev event "lose-server service-started"
}

# worker_kept PRE: the running containers on the workers in PRE still run,
# with the same ID and start time, and no new one runs. Prints what changed.
worker_kept() {
	local cur=$SOAK_DIR/kept.tsv i n
	: >"$cur"
	for ((i = 1; i < ${#NAMES[@]}; i++)); do
		n=${NAMES[$i]}
		container_rows_of "$n" >>"$cur" 2>/dev/null || printf 'U\t%s\n' "$n" >>"$cur"
	done
	# only the workers of the list now: after a move, the old server's
	# machine is in PRE and gone
	awk -F'\t' -v s="$SERVER" -v w=" ${NAMES[*]:1} " '
		FILENAME == ARGV[1] { if ($1 == "C" && $4 == "running" && $2 != s && index(w, " " $2 " ")) pre[$2 "|" $3 "|" $5] = $6 "|" $7; next }
		$1 == "U" { out = out "unreachable:" $2 "," ; next }
		$1 == "C" && $4 == "running" { k = $2 "|" $3 "|" $5; now[k] = 1; if (!(k in pre)) out = out "new:" $2 ":" $3 ":" $6 "|" $7 "," }
		END { for (k in pre) if (!(k in now)) { split(k, a, "|"); out = out "gone:" a[1] ":" a[2] ":" pre[k] "," } print out }' "$1" "$cur"
}

server_replace() {
	local secs=${1:-2400} name=lose-server pre=$SOAK_DIR/pre-lose-server.tsv cur=$SOAK_DIR/measure-lose-server.tsv
	local end t d v api=up p22=up phase=wait-down kept="" k=0 rpid="" webs n s ready="" conv="" allrun="" i
	[ -s "$SOAK_DIR/backup.tar" ] || die "no backup; run server-backup first"
	end=$(($(date +%s) + secs))
	# the workers that run a web replica: they should keep answering throughout
	webs=$(awk -F'\t' 'FILENAME == ARGV[1] { split($0, x, " "); nm[x[1]] = x[2]; next } $1 == "R" && $2 == "web" && $5 == "running" { print nm[$4] }' "$IDS_FILE" "$pre" | grep -vx "$SERVER" | sort -u | tr '\n' ' ')
	ev measure "$name begin web_on_workers=$webs"
	while [ "$(date +%s)" -lt "$end" ]; do
		t=$(now_ms)
		if port22 "$SERVER"; then v=up; else v=down; fi
		[ "$v" = "$p22" ] || {
			ev event "$name $SERVER port22-$v"
			p22=$v
		}
		if on "$SERVER" "curl -fsS -m 2 http://127.0.0.1:4483/healthz" >/dev/null 2>&1; then v=up; else v=down; fi
		[ "$v" = "$api" ] || {
			ev event "$name api-$v"
			api=$v
		}
		for n in $webs; do
			(if out=$(on "$n" "curl -sS -o /dev/null -m 2 -w '%{http_code} %{time_total}' http://127.0.0.1:8080/" 2>/dev/null) && [ "${out%% *}" = 200 ]; then
				ev reach-fast "app=web node=$n ok s=${out#* }"
			else ev reach-fast "app=web node=$n fail"; fi) &
		done
		case "$phase" in
		wait-down)
			if [ "$p22" = down ]; then
				forget_host "$SERVER"
				phase=wait-up
				ev event "$name machine-gone"
			fi
			;;
		wait-up)
			if [ "$p22" = up ] && on "$SERVER" "test -f /var/lib/hm-ready" >/dev/null 2>&1; then
				ev event "$name machine-ready"
				# a fresh login, so the session has the groups user data gave it
				forget_host "$SERVER"
				restore_server >>"$SOAK_DIR/restore.out" 2>&1 &
				rpid=$!
				phase=restoring
			fi
			;;
		restoring)
			if ! kill -0 "$rpid" 2>/dev/null; then
				wait "$rpid" || die "$name: restore failed: $(tail -n 5 "$SOAK_DIR/restore.out")"
				phase=started
			fi
			;;
		esac
		# while the API is away: do the workers' containers stay as they were?
		if [ "$phase" != started ] && [ $((k % 5)) -eq 0 ]; then
			v=$(worker_kept "$pre")
			[ "$v" = "$kept" ] || {
				ev event "$name workers-kept ${v:-unchanged}"
				kept=$v
			}
		fi
		if [ "$phase" = started ] && [ "$api" = up ]; then
			collect "$cur" nodes
			if api_ok "$cur"; then
				for ((i = 1; i < ${#NAMES[@]}; i++)); do
					n=${NAMES[$i]}
					s=$(node_state "$cur" "$(id_of "$n")")
					case " $ready " in *" $n "*) ;; *)
						if [ "$s" = ready ]; then
							ev event "$name $n node-ready"
							ready="$ready $n"
						fi
						;;
					esac
				done
				if check_converged "$cur" 0 >/dev/null; then v=yes; else v=no; fi
				[ "$v" = "$conv" ] || {
					ev event "$name converged-$v"
					conv=$v
				}
				v=$(running "$cur")
				if [ "${v%/*}" = "${v#*/}" ]; then v=yes; else v=no; fi
				[ "$v" = "$allrun" ] || {
					ev event "$name all-running-$v $(running "$cur")"
					allrun=$v
				}
				d=$(dups "$cur" | tr '\n' ';')
				[ -z "$d" ] || ev event "$name dups $d"
				[ "$conv" = yes ] && [ "$allrun" = yes ] && [ "$(wc -w <<<"$ready")" -eq $((${#NAMES[@]} - 1)) ] && break
			fi
		fi
		k=$((k + 1))
		d=$((1000 - ($(now_ms) - t)))
		[ "$d" -le 0 ] || sleep "$((d / 1000)).$(printf '%03d' $((d % 1000)))"
	done
	wait
	if [ "$(date +%s)" -ge "$end" ]; then
		ev event "$name measure-timeout phase=$phase api=$api ready=$ready converged=$conv"
		return 1
	fi
	ev event "$name workers-kept-after $(worker_kept "$pre")"
	ev measure "$name end"
	end_scenario "$name"
}

# routed_ready ID: every routed replica the server places on node ID runs and
# passes its health check, in hivemind status.
ROUTED_APPS=${ROUTED_APPS:-www api}
routed_ready() {
	hm "$SERVER" status --json 2>/dev/null | jq -e --arg i "$1" --arg apps " $ROUTED_APPS " \
		'[.[] | .name as $n | select($apps | contains(" " + $n + " ")) | .replicas[] | select(.node_id == $i and .desired == "running") | (.state == "running" and .ready == true)] | all' >/dev/null
}

upgrade_wait() {
	[ "${UPGRADE_ALL_AT_ONCE:-0}" = 1 ] && return 0
	if wait_for 120 routed_ready "$(id_of "$1")"; then
		ev event "upgrade $1 routed-ready"
		sleep 2 # the routers read the table once a second
	else
		ev event "upgrade $1 routed-ready-timeout"
	fi
}

# upgrade: install $BIN on every node and restart hivemind, the server first,
# as a user would with a new release.
upgrade() {
	local n i
	ev scenario "upgrade begin binary_sha256=$(sha256sum "$BIN" | cut -d" " -f1)"
	for n in "${NAMES[@]}"; do install_bin "$n" || die "installing on $n"; done
	ev event "upgrade $SERVER restart"
	on "$SERVER" "sudo systemctl restart hivemind" || die "restart on $SERVER"
	wait_for 60 hm "$SERVER" status || die "the API did not come back"
	ev event "upgrade api-up"
	# One node at a time, as docs/upgrading.md says for routed apps: the next
	# restarts once the last one's routed replicas are ready again.
	# UPGRADE_ALL_AT_ONCE=1 restarts the workers back to back, as the second
	# soak did.
	upgrade_wait "$SERVER"
	for ((i = 1; i < ${#NAMES[@]}; i++)); do
		ev event "upgrade ${NAMES[$i]} restart"
		on "${NAMES[$i]}" "sudo systemctl restart hivemind" || die "restart on ${NAMES[$i]}"
		upgrade_wait "${NAMES[$i]}"
	done
	wait_for 300 converged_now || die "not converged after the upgrade"
	for n in "${NAMES[@]}"; do ev machine "$n hivemind=\"$(on "$n" "$HM version" | tr "\n" " ")\""; done
	end_scenario upgrade
}

kv_check() {
	local st id n v
	st=$(hm "$SERVER" status --json) || die "no status"
	id=$(jq -r '.[] | select(.name == "kv") | .replicas[0].node_id // ""' <<<"$st")
	n=$(name_of_id "$id")
	v=$(kv_get "$n" 2>/dev/null) || v=""
	if [ "$v" = "$(cat "$SOAK_DIR/kv-value")" ]; then ev event "${1:-kv} kv-value-match node=$n"; else ev event "${1:-kv} kv-value-mismatch node=$n got=${v:-none}"; fi
}

# dns_set IP: the server name's record now names IP. Its stand-in, the line
# in /etc/hosts, changes on every node and on the outside machine at once.
dns_set() {
	local ip=$1 n pids=()
	[ -n "$SERVER_NAME" ] || die "dns_set: no SERVER_NAME"
	echo "$ip" >"$DNS_FILE"
	ev event "dns-update $SERVER_NAME $ip begin"
	for n in "${NAMES[@]}" ${OUT_NAME:+"$OUT_NAME"}; do
		(if dns_line "$n" "$ip"; then ev event "dns-update $n done"; else ev event "dns-update $n fail"; fi) &
		pids+=($!)
	done
	wait "${pids[@]}"
	ev event "dns-update $SERVER_NAME $ip end"
}

# switch_server NEW NEWIP: from now on the server is NEW at NEWIP: the node
# list, the ID map, and this process's view of them.
switch_server() {
	local new=$1 newip=$2 sid i
	sid=$(id_of "$SERVER")
	ev event "server-switch $SERVER/$SERVER_IP -> $new/$newip id=$sid"
	NAMES[0]=$new IPS[0]=$newip SERVER=$new SERVER_IP=$newip
	for i in "${!NAMES[@]}"; do echo "${NAMES[$i]} ${IPS[$i]}"; done >"$NODES_FILE.tmp" && mv "$NODES_FILE.tmp" "$NODES_FILE"
	awk -v i="$sid" -v n="$new" '$1 == i { $2 = n } { print }' "$IDS_FILE" >"$IDS_FILE.tmp" && mv "$IDS_FILE.tmp" "$IDS_FILE"
	ev ids "$(tr '\n' ' ' <"$IDS_FILE")"
}

# restart_loops: the probe loop and the watcher read the node list when they
# start; start the ones that run again.
restart_loops() {
	local l
	for l in probe watch; do
		if [ -f "$SOAK_DIR/$l.pid" ]; then
			bg_stop "$l"
			bg_start "$l" "$HERE/$l.sh"
		fi
	done
}

# set_server_all IP: on each worker in turn, stop the service, move it with
# hivemind node set-server IP, and start it, as docs/backup.md says.
set_server_all() {
	local ip=$1 i n out rc
	for ((i = 1; i < ${#NAMES[@]}; i++)); do
		n=${NAMES[$i]}
		ev event "set-server $n begin"
		out=$(SSH_TIMEOUT=60 on "$n" "sudo systemctl stop hivemind && { sudo $HM node set-server $ip 2>&1; rc=\$?; sudo systemctl start hivemind; exit \$rc; }" 2>&1)
		rc=$?
		ev event "set-server $n end rc=$rc $(tr '\n' ';' <<<"$out" | cut -c1-300)"
	done
}

# routes_watch SECS: the server's routing table (GET /v1/routes) every
# 200 ms for SECS, one "routes" line at each change: host=endpoint,...
read -r -d '' REMOTE_ROUTES <<'EOF' || true
set -o pipefail
tok=$(cat /var/lib/hivemind/admin.token)
end=$(($(date +%s) + $1)); last=
while [ "$(date +%s)" -lt "$end" ]; do
	r=$(printf 'header = "Authorization: Bearer %s"\n' "$tok" | curl -sf -K - -m 1 http://127.0.0.1:4483/v1/routes |
		jq -r '[.routes[] | "\(.host)=\(.endpoints | join(","))"] | join(" ")' 2>/dev/null) || r=down
	[ "$r" = "$last" ] || { echo "$(date -u +%s%3N) $r"; last=$r; }
	sleep 0.2
done
EOF
routes_watch() {
	local t r
	on "$SERVER" "sudo bash -s $1" <<<"$REMOTE_ROUTES" 2>/dev/null | while read -r t r; do
		ev event "routes server_ms=$t ${r:-empty}"
	done
}

# server_move NEW NEWIP MODE: the server machine is lost, and the backup is
# restored on NEW, a new machine at another address. Begin with "begin
# lose-server-MODE <server>" and server-backup; the operator then powers the
# old machine off and starts NEW, while this measures. MODE:
#   name  the server advertises $SERVER_NAME: restore, serve with
#         --cluster-addr NEWIP while the name still names the old machine,
#         then change the name's record (dns_set NEWIP);
#   ip    the server advertises its address: restore, serve --advertise NEWIP,
#         then move each worker with set-server (set_server_all NEWIP).
# Until every worker is ready, the cluster has converged and every replica runs.
server_move() {
	local new=$1 newip=$2 mode=$3 name=lose-server-$3
	local pre=$SOAK_DIR/pre-lose-server-$3.tsv cur=$SOAK_DIR/measure-lose-server-$3.tsv
	local old=$SERVER oldip=$SERVER_IP end t d v api=up p22=up np22=down phase=wait-down kept="" k=0 rpid="" mpid=""
	local n s ready="" conv="" allrun="" dupl="" i t0 sid
	[ -s "$SOAK_DIR/backup.tar" ] || die "no backup; run server-backup first"
	[ -s "$pre" ] || die "run begin $name first"
	case "$mode" in name) [ -n "$SERVER_NAME" ] || die "mode name needs SERVER_NAME" ;; ip) ;; *) die "mode is name or ip" ;; esac
	end=$(($(date +%s) + 2400))
	t0=$(date -u +%s)
	ev measure "$name begin old=$old/$oldip new=$new/$newip mode=$mode advertise=$(advertise_mode)"
	while [ "$(date +%s)" -lt "$end" ]; do
		t=$(now_ms)
		if port22_ip "$oldip"; then v=up; else v=down; fi
		[ "$v" = "$p22" ] || {
			ev event "$name $old port22-$v"
			p22=$v
		}
		# NEWIP "-": the new machine's name and address come from
		# $SOAK_DIR/new-server once it has been launched.
		if [ "$newip" = - ] && [ -s "$SOAK_DIR/new-server" ]; then
			read -r new newip _ <"$SOAK_DIR/new-server"
			ev event "$name new-machine $new $newip"
		fi
		if [ "$newip" != - ]; then
			if port22_ip "$newip"; then v=up; else v=down; fi
			[ "$v" = "$np22" ] || {
				ev event "$name $new port22-$v"
				np22=$v
			}
		fi
		# the API, on whichever machine is the server now
		if SSH_CT=1 SSH_TIMEOUT=4 on "$SERVER" "curl -fsS -m 2 http://127.0.0.1:4483/healthz" >/dev/null 2>&1; then v=up; else v=down; fi
		[ "$v" = "$api" ] || {
			ev event "$name api-$v"
			api=$v
		}
		case "$phase" in
		wait-down)
			if [ "$p22" = down ] && [ "$api" = down ]; then
				ev event "$name machine-gone"
				phase=wait-new
			fi
			;;
		wait-new)
			if [ "$np22" = up ] && SSH_CT=1 on_ip "$newip" "test -f /var/lib/hm-ready" >/dev/null 2>&1; then
				ev event "$name machine-ready"
				switch_server "$new" "$newip"
				# a fresh login, so the session has the groups user data gave it
				forget_host "$SERVER"
				restore_server >>"$SOAK_DIR/restore.out" 2>&1 &
				rpid=$!
				phase=restoring
			fi
			;;
		restoring)
			if ! kill -0 "$rpid" 2>/dev/null; then
				wait "$rpid" || die "$name: restore failed: $(tail -n 5 "$SOAK_DIR/restore.out")"
				phase=serving
				# the restored server's routing table, from its first seconds
				SSH_TIMEOUT=60 routes_watch 40 &
			fi
			;;
		serving)
			if [ "$api" = up ]; then
				# the operator's steps: the server's address for the workers;
				# the routed hosts' records name the new machine instead of the old
				case "$mode" in
				name) dns_set "$newip" >>"$SOAK_DIR/restore.out" 2>&1 & ;;
				ip) set_server_all "$newip" >>"$SOAK_DIR/restore.out" 2>&1 & ;;
				esac
				mpid=$!
				phase=moving
				restart_loops
				if [ "$SOAK_ROUTES" = 1 ] && [ -n "$OUT_NAME" ]; then
					("$HERE/outside.sh" client-add "$newip" && "$HERE/outside.sh" chall-update) >>"$SOAK_DIR/restore.out" 2>&1 &
				fi
			fi
			;;
		esac
		# while the server is away: do the workers' containers stay as they were?
		if [ "$phase" != moving ] && [ $((k % 5)) -eq 0 ]; then
			v=$(worker_kept "$pre")
			[ "$v" = "$kept" ] || {
				ev event "$name workers-kept ${v:-unchanged}"
				kept=$v
			}
		fi
		if [ "$phase" = moving ] && [ "$api" = up ]; then
			collect "$cur" nodes
			if api_ok "$cur"; then
				for ((i = 1; i < ${#NAMES[@]}; i++)); do
					n=${NAMES[$i]}
					s=$(node_state "$cur" "$(id_of "$n")")
					case " $ready " in *" $n "*) ;; *)
						if [ "$s" = ready ]; then
							ev event "$name $n node-ready"
							ready="$ready $n"
						fi
						;;
					esac
				done
				if check_converged "$cur" 0 >/dev/null; then v=yes; else v=no; fi
				[ "$v" = "$conv" ] || {
					ev event "$name converged-$v"
					conv=$v
				}
				v=$(running "$cur")
				if [ "${v%/*}" = "${v#*/}" ]; then v=yes; else v=no; fi
				[ "$v" = "$allrun" ] || {
					ev event "$name all-running-$v $(running "$cur")"
					allrun=$v
				}
			fi
			d=$(dups "$cur" | tr '\n' ';')
			[ "$d" = "$dupl" ] || {
				ev event "$name dups ${d:-none}"
				dupl=$d
			}
			if [ "$conv" = yes ] && [ "$allrun" = yes ] && [ "$(wc -w <<<"$ready")" -eq $((${#NAMES[@]} - 1)) ] && ! kill -0 "$mpid" 2>/dev/null; then
				break
			fi
		fi
		k=$((k + 1))
		d=$((1000 - ($(now_ms) - t)))
		[ "$d" -le 0 ] || sleep "$((d / 1000)).$(printf '%03d' $((d % 1000)))"
	done
	wait
	if [ "$(date +%s)" -ge "$end" ]; then
		ev event "$name measure-timeout phase=$phase api=$api ready=$ready converged=$conv"
		return 1
	fi
	move_after "$name" "$t0" "$oldip"
}

# move_after NAME T0 OLDIP: after server_move: what the workers kept, the
# new server's journal since T0 (Unix s), its certificate's names, each
# worker's server address, the journals to files; the clients stop asking
# the old machine's router.
move_after() {
	local name=$1 t0=$2 oldip=$3 pre=$SOAK_DIR/pre-$1.tsv sid i
	ev event "$name workers-kept-after $(worker_kept "$pre")"
	sid=$(id_of "$SERVER")
	ev event "$name server-journal lost=$(on "$SERVER" "sudo journalctl -u hivemind --since @$t0 -o cat | grep -c 'node lost'") $(on "$SERVER" "sudo journalctl -u hivemind --since @$t0 -o cat | grep -iE 'resolves|set-server|address changed|advertis' | head -n 3" | tr '\n' ';' | cut -c1-600)"
	ev event "$name server-cert $(on "${NAMES[1]}" "openssl s_client -connect $SERVER_IP:4484 </dev/null 2>/dev/null | openssl x509 -noout -ext subjectAltName 2>/dev/null | tail -n 1" | tr -s ' ')"
	ev event "$name server-node $(hm "$SERVER" nodes --json | jq -c --arg i "$sid" '.[] | select(.id == $i) | {id, address, state}')"
	for ((i = 1; i < ${#NAMES[@]}; i++)); do
		ev event "$name ${NAMES[$i]} node-json server=$(on "${NAMES[$i]}" "sudo jq -r .server $DATA/node.json")"
		on "${NAMES[$i]}" "sudo journalctl -u hivemind --since @$t0 -o short-iso-precise --no-pager" >"$SOAK_DIR/journal-$name-${NAMES[$i]}.txt" 2>/dev/null || true
	done
	on "$SERVER" "sudo journalctl -u hivemind --since @$t0 -o short-iso-precise --no-pager" >"$SOAK_DIR/journal-$name-$SERVER.txt" 2>/dev/null || true
	if [ "$SOAK_ROUTES" = 1 ] && [ -n "$OUT_NAME" ]; then "$HERE/outside.sh" client-drop "$oldip" >>"$SOAK_DIR/restore.out" 2>&1 || true; fi
	rm -f "$SOAK_DIR/new-server"
	ev measure "$name end"
	end_scenario "$name"
}

# setserver_checks T ADDR...: hivemind node set-server on worker T, first
# beside its running service, then, with the service stopped, to each ADDR;
# each must be refused, and node.json must stay as it was. The service is
# started again at the end; the time it was stopped is logged (its lease
# lapses after 15 s, so keep the list short).
setserver_checks() {
	local T=$1 a out rc s0 s t1
	shift
	s0=$(on "$T" "sudo sha256sum $DATA/node.json" | cut -d' ' -f1)
	ev event "set-server-check $T begin node.json=$s0 addrs=$*"
	out=$(on "$T" "sudo $HM node set-server $(server_addr) 2>&1")
	rc=$?
	s=$(on "$T" "sudo sha256sum $DATA/node.json" | cut -d' ' -f1)
	ev event "set-server-check $T running rc=$rc same=$([ "$s" = "$s0" ] && echo yes || echo no) $(tr '\n' ';' <<<"$out" | cut -c1-400)"
	t1=$(now_ms)
	on "$T" "sudo systemctl stop hivemind" || die "stop on $T"
	for a in "$@"; do
		out=$(SSH_TIMEOUT=30 on "$T" "sudo $HM node set-server $a 2>&1")
		rc=$?
		s=$(on "$T" "sudo sha256sum $DATA/node.json" | cut -d' ' -f1)
		ev event "set-server-check $T addr=$a rc=$rc same=$([ "$s" = "$s0" ] && echo yes || echo no) $(tr '\n' ';' <<<"$out" | cut -c1-400)"
	done
	on "$T" "sudo systemctl start hivemind" || die "start on $T"
	ev event "set-server-check $T end stopped_ms=$(($(now_ms) - t1))"
}

# metrics_snap LABEL: the router series of the server's /metrics, to
# $SOAK_DIR/metrics-LABEL.txt with the time; one log line with the number of
# series per node.
read -r -d '' REMOTE_METRICS <<'EOF' || true
tok=$(cat /var/lib/hivemind/admin.token) || exit 1
printf 'header = "Authorization: Bearer %s"\n' "$tok" | curl -sS -K - -m 5 http://127.0.0.1:4483/metrics | grep '^hivemind_router_'
EOF
metrics_nodes() { # metrics_nodes FILE: "name=series ..." for the request counters
	awk -F'node="' '/^hivemind_router_requests_total\{/ { split($2, a, "\""); n[a[1]]++ } END { for (k in n) print k, n[k] }' "$1" |
		while read -r id c; do
			nm=$(name_of_id "$id")
			printf '%s=%s ' "${nm:-$id}" "$c"
		done
}
metrics_snap() {
	local f=$SOAK_DIR/metrics-$1.txt t
	t=$(now_ms)
	on "$SERVER" "sudo bash -s" <<<"$REMOTE_METRICS" >"$f.tmp" || die "metrics on $SERVER"
	{
		echo "# t_ms=$t"
		cat "$f.tmp"
	} >"$f"
	rm -f "$f.tmp"
	ev metrics-snap "$1 t_ms=$t series=$(grep -c '^hivemind_router_' "$f") $(metrics_nodes "$f")"
}

# metrics_watch SECS: every 5 s for SECS, the nodes that have router series
# in the server's /metrics; a line at each change.
metrics_watch() {
	local end last="" f=$SOAK_DIR/metrics-watch.txt v
	end=$(($(date +%s) + ${1:-300}))
	while [ "$(date +%s)" -lt "$end" ]; do
		if on "$SERVER" "sudo bash -s" <<<"$REMOTE_METRICS" >"$f" 2>/dev/null; then
			v=$(metrics_nodes "$f" | tr ' ' '\n' | sed 's/=.*//' | sort | tr '\n' ' ')
		else
			v=fail
		fi
		[ "$v" = "$last" ] || {
			ev event "metrics-nodes $v"
			last=$v
		}
		sleep 5
	done
}

# watch_moves NAME T PRE SECS DONE: once a second, until DONE (a shell
# condition on $moved, $oldrm, $conv) holds or SECS pass, log the first time
# T's movable replicas in PRE run elsewhere, the first time T has none of its
# old containers for them, and each change of convergence and duplicates.
watch_moves() {
	local name=$1 T=$2 pre=$3 secs=$4 done=$5 cur=$SOAK_DIR/measure-$1.tsv
	local id keys end t d v c moved=0 oldrm=0 conv="" dupl="" where
	id=$(id_of "$T")
	keys=$(movable "$pre" "$id" | tr '\n' ' ')
	end=$(($(date +%s) + secs))
	while [ "$(date +%s)" -lt "$end" ]; do
		t=$(now_ms)
		collect "$cur" nodes
		if api_ok "$cur"; then
			if [ "$moved" = 0 ]; then
				where=$(awk -F'\t' -v id="$id" -v keys=" $keys " 'FILENAME == ARGV[1] { split($0, x, " "); nm[x[1]] = x[2]; next }
					$1 == "R" && index(keys, " " $2 "|" $3 " ") > 0 { n++; if ($4 != id && $4 != "" && $5 == "running") { ok++; w = w $2 "|" $3 "@" nm[$4] " " } }
					END { if (ok == n) print w }' "$IDS_FILE" "$cur")
				[ -z "$where" ] || {
					ev event "$name $T replicas-elsewhere $where"
					moved=1
				}
			fi
			if [ "$oldrm" = 0 ] && ! grep -q "^U	$T$" "$cur" && [ -z "$(old_on "$cur" "$pre" "$T" "$keys")" ]; then
				ev event "$name $T old-containers-removed"
				oldrm=1
			fi
			if check_converged "$cur" 0 >/dev/null; then c=yes; else c=no; fi
			[ "$c" = "$conv" ] || {
				ev event "$name converged-$c"
				conv=$c
			}
		fi
		d=$(dups "$cur" | tr '\n' ';')
		[ "$d" = "$dupl" ] || {
			ev event "$name dups ${d:-none}"
			dupl=$d
		}
		eval "$done" && return 0
		d=$((1000 - ($(now_ms) - t)))
		[ "$d" -le 0 ] || sleep "$((d / 1000)).$(printf '%03d' $((d % 1000)))"
	done
	ev event "$name measure-timeout moved=$moved old_removed=$oldrm converged=$conv"
	return 1
}

# rollout [VERSION]: a rolling update of www to the private image's tag
# VERSION (2), measured until no rollout is in progress and the cluster has
# converged with every www replica at the new generation.
rollout() {
	local name=rollout v=${1:-2} cur=$SOAK_DIR/measure-rollout.tsv end t d st r
	begin "$name" "$SERVER"
	ev event "$name www fault deploy $REGISTRY/soak/whoami:$v"
	hm "$SERVER" "deploy -f -" <<<"$(www_spec "$v")" >/dev/null || die "$name: deploy"
	end=$(($(date +%s) + 600))
	r=""
	while [ "$(date +%s)" -lt "$end" ]; do
		t=$(now_ms)
		st=$(hm "$SERVER" status --json 2>/dev/null) || st=""
		v=$(jq -r '.[] | select(.name == "www") | (.rollout // {} | "\(.state // "none") target=\(.target // "-") moving=\(.moving // "-")") + " gens=" + ([.replicas[] | "\(.index):\(.generation):\(.state)"] | join(","))' <<<"$st" 2>/dev/null)
		[ "$v" = "$r" ] || {
			ev event "$name www $v"
			r=$v
		}
		if [[ "$v" == none* ]]; then
			wait_for 120 converged_now && ev event "$name converged-yes"
			break
		fi
		d=$((1000 - ($(now_ms) - t)))
		[ "$d" -le 0 ] || sleep "$((d / 1000)).$(printf '%03d' $((d % 1000)))"
	done
	end_scenario "$name"
}

# drain_node TARGET: hivemind node drain, measured; then eligibility
# --enable, and a check that nothing moved back or ran twice.
drain_node() {
	local T=$1 name=drain-worker id pre=$SOAK_DIR/pre-drain-worker.tsv out
	begin "$name" "$T"
	id=$(id_of "$T")
	ev event "$name $T fault drain"
	out=$(SSH_TIMEOUT=900 hm "$SERVER" node drain "$id" 2>&1)
	ev event "$name $T drain-returned rc=$? $(tr '\n' ';' <<<"$out" | cut -c1-600)"
	watch_moves "$name" "$T" "$pre" 120 '[ "$oldrm" = 1 ] && [ "$conv" = yes ]'
	ev event "$name $T nodes $(hm "$SERVER" nodes --json | jq -c --arg i "$id" '.[] | select(.id == $i) | {state, eligible, replicas, drain: (.drain.state // null)}')"
	hm "$SERVER" node eligibility "$id" --enable >/dev/null || die "$name: eligibility --enable"
	ev event "$name $T enabled"
	sleep 10
	snapshot "$SOAK_DIR/now.tsv" || true
	ev event "$name $T after-enable converged=$(check_converged "$SOAK_DIR/now.tsv" >/dev/null && echo yes || echo no) on-node=$(movable "$SOAK_DIR/now.tsv" "$id" | tr '\n' ' ') eligible=$(hm "$SERVER" nodes --json | jq -r --arg i "$id" '.[] | select(.id == $i) | .eligible')"
	end_scenario "$name"
}

# remove_node TARGET: drain TARGET, then hivemind node remove; check that the
# removed worker's agent stops. The machine is replaced afterwards, outside
# this script, and join-new joins the new one.
remove_node() {
	local T=$1 name=replace-worker id pre=$SOAK_DIR/pre-replace-worker.tsv out
	begin "$name" "$T"
	id=$(id_of "$T")
	ev event "$name $T fault drain-and-remove"
	out=$(SSH_TIMEOUT=900 hm "$SERVER" node drain "$id" 2>&1)
	ev event "$name $T drain-returned rc=$? $(tr '\n' ';' <<<"$out" | cut -c1-600)"
	watch_moves "$name" "$T" "$pre" 120 '[ "$oldrm" = 1 ] && [ "$conv" = yes ]'
	out=$(hm "$SERVER" node remove "$id" 2>&1)
	ev event "$name $T removed rc=$? $(tr '\n' ';' <<<"$out" | cut -c1-300)"
	wait_for 30 on "$T" "! systemctl is-active --quiet hivemind" &&
		ev event "$name $T agent-stopped $(on "$T" "sudo journalctl -u hivemind -n 3 --no-pager -o cat" | tr '\n' ';' | cut -c1-400)"
	ev event "$name $T containers-left $(on "$T" "sudo docker ps -q --filter label=hivemind.app | wc -l")"
	ev event "$name $T nodes $(hm "$SERVER" nodes --json | jq -c '[.[] | {id, state}]')"
}

# join_new TARGET: after the machine was replaced with an empty one at the
# same address, install, prepare and join it with a new token, as a new
# machine; then measure until it is ready and the cluster has converged.
join_new() {
	local T=$1 name=join-worker t0 id end
	ev scenario "$name begin target=$T"
	# Until the address answers as a new machine: Docker installed by its
	# user data, and no hivemind data directory. The old machine may still
	# be up when this starts, and the new one has a new host key.
	end=$(($(date +%s) + 1200))
	until on "$T" "test -f /var/lib/hm-ready && ! sudo test -e $DATA" >/dev/null 2>&1; do
		[ "$(date +%s)" -lt "$end" ] || die "$name: $T is not a new, empty machine after 1200 s"
		forget_host "$T"
		sleep 2
	done
	forget_host "$T"
	ev event "$name $T machine-ready"
	install_bin "$T"
	if [ "$SOAK_ROUTES" = 1 ]; then prepare_node "$T"; fi
	t0=$(now_ms)
	ev event "$name $T join"
	join_worker "$T"
	id=$(on "$T" "sudo jq -r .node_id $DATA/node.json")
	awk -v n="$T" -v i="$id" '$2 == n { $1 = i } { print }' "$IDS_FILE" >"$IDS_FILE.tmp" && mv "$IDS_FILE.tmp" "$IDS_FILE"
	ev ids "$(tr '\n' ' ' <"$IDS_FILE")"
	wait_for 120 node_is "$id" ready && ev event "$name $T node-ready id=$id after_ms=$(($(now_ms) - t0))"
	wait_for 300 converged_now && ev event "$name converged-yes"
	ev event "$name $T machine $(on "$T" "uname -r; docker version --format '{{.Server.Version}}'" | tr '\n' ' ')"
	end_scenario "$name"
}

node_is() { [ "$(hm "$SERVER" nodes --json | jq -r --arg i "$1" '.[] | select(.id == $i) | .state')" = "$2" ]; }

cmd=${1:-}
shift || true
case "$cmd" in
fault) ev event "$1 $2 fault ${*:3}" ;;
rollout) rollout "$@" ;;
drain) drain_node "$@" ;;
remove) remove_node "$@" ;;
join-new) join_new "$@" ;;
place-spread) place_spread ;;
probe-start) bg_start probe "$HERE/probe.sh" ;;
probe-stop) bg_stop probe ;;
watch-start) bg_start watch "$HERE/watch.sh" ;;
watch-stop) bg_stop watch ;;
mark) ev mark "$*" ;;
begin) begin "$@" ;;
measure) measure "$@" ;;
reboot) reboot_node "$@" ;;
partition) partition "$@" ;;
server-backup) server_backup ;;
server-replace) server_replace "$@" ;;
server-move) server_move "$@" ;;
move-after) move_after "$@" ;;
dns) dns_set "$@" ;;
set-server-all) set_server_all "$@" ;;
set-server-checks) setserver_checks "$@" ;;
metrics-snap) metrics_snap "$@" ;;
metrics-watch) metrics_watch "$@" ;;
kv-check) kv_check "$@" ;;
upgrade) upgrade ;;
end) end_scenario "$@" ;;
*)
	sed -n '2,38p' "$0"
	exit 2
	;;
esac
