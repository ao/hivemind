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
#   scenario.sh rollout [VERSION [APP...]]    rolling update of each APP (www) to VERSION
#   scenario.sh old-back OLD OLDIP W          the old server's machine back on after a
#                                             server-move; W sent to it for a while
#   scenario.sh cert-restart T [LEFT]         restart T once a certificate its router
#                                             serves is in its renewal window
#   scenario.sh ca-rotate [NAME]              a CA rotation, start, renew, finish, each
#                                             once the one before it is done; measured
#   scenario.sh ca-offline T                  a rotation with worker T powered off,
#                                             finish --force, T back cut off, removed
#                                             and joined again
#   scenario.sh ca-abort                      a rotation aborted in phase renew, then
#                                             the turned rotation finished
#   scenario.sh ca-restore-begin              start, renew, then a backup, for a
#                                             server-move during a rotation
#   scenario.sh ca-finish NAME                finish the rotation a restore brought back
#   scenario.sh ca-watch | ca-views NAME LABEL  the rotation's state, on change or once
#   scenario.sh start-hold                    the server's machine stopped and started
#                                             with every worker cut off from it, past
#                                             its start grace; then the workers back
#                                             (HEAL_LAGS="W:0 W:5 W:20": each at its lag)
#   scenario.sh ca-text NAME LABEL            hivemind ca status and status, as text
#   scenario.sh reboot-cutoff W               worker W's machine powered off and started
#                                             while the other workers' routers serve
#                                             their last table (the server's hivemind
#                                             stopped, then started; the others cut off)
#   scenario.sh rollout-all [ROUNDS]          ROUNDS (3) rolling updates of every app back
#                                             to back, a status sampler every 0.5 s
#   scenario.sh ports LABEL                   each node's routed containers and host ports
#   scenario.sh end NAME
#   scenario.sh leader-kill | leader-partition [SECS] | quorum-loss |
#               service-restart | steady [SECS] | servers [LABEL] | ha-selftest
#                                             the scenarios of several servers,
#                                             with the write-load: see ha.sh
#
# The scenarios above but the ha.sh ones assume one server (SOAK_SERVERS=1)
# with its API on loopback (SOAK_API_TLS=0).
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
# The scenarios of several servers (leader-kill, leader-partition, quorum-loss, ...).
. "$HERE/ha.sh"

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
	ports_log "$name before"
}

end_scenario() {
	local name=$1
	snapshot "$SOAK_DIR/now.tsv" || true
	ports_log "$name after"
	ev scenario "$name end converged=$(check_converged "$SOAK_DIR/now.tsv" >/dev/null && echo yes || echo no) placement $(awk -F'\t' 'FILENAME == ARGV[1] { split($0, x, " "); nm[x[1]] = x[2]; next } $1 == "R" { printf "%s|%s@%s ", $2, $3, nm[$4] }' "$IDS_FILE" "$SOAK_DIR/now.tsv")"
}

# ports_log LABEL: with SOAK_ROUTES=1, one "ports" line per node: its routed
# containers, stopped ones included, with their host ports (routed_ports in
# lib.sh), or "unreachable". The seventh soak compares a scenario's "before"
# and "after" lines: a routed container keeps its host ports for its life.
ports_log() {
	local n v
	[ "$SOAK_ROUTES" = 1 ] || return 0
	for n in "${NAMES[@]}"; do
		v=$(SSH_CT=2 routed_ports "$n" 2>/dev/null) || v=unreachable
		ev ports "$1 $n ${v:-none}"
	done
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
	local f=/var/tmp/hivemind-soak.tar out
	out=$(on "$SERVER" "sudo rm -f $f && sudo $HM backup $f 2>/dev/null") || die "hivemind backup on $SERVER"
	ev event "lose-server backup-said $(tr '\n' ';' <<<"$out" | cut -c1-400)"
	(umask 077 && SSH_TIMEOUT=60 on "$SERVER" "sudo cat $f" >"$SOAK_DIR/backup.tar") || die "copying the backup"
	chmod 600 "$SOAK_DIR/backup.tar"
	ev event "lose-server backup bytes=$(stat -c %s "$SOAK_DIR/backup.tar") sha256=$(sha256sum "$SOAK_DIR/backup.tar" | cut -d' ' -f1) entries=$(tar -tf "$SOAK_DIR/backup.tar" | tr '\n' ',')"
}

# restore_server: on the new, empty server machine, as docs/backup.md says.
restore_server() {
	local out
	install_bin "$SERVER" || return 1
	# The machine set-up that is not hivemind's state: hosts, trust, certs.d.
	if [ "$SOAK_ROUTES" = 1 ]; then prepare_node "$SERVER" || return 1; fi
	(SSH_TIMEOUT=60 on "$SERVER" "umask 077; cat > /var/tmp/hivemind-soak.tar" <"$SOAK_DIR/backup.tar") || return 1
	# restore's own lines (no secret in them): the epoch, and the CA rotation's phase
	out=$(on "$SERVER" "sudo $HM restore /var/tmp/hivemind-soak.tar --data-dir $DATA 2>&1 && rm -f /var/tmp/hivemind-soak.tar") || {
		echo "$out"
		return 1
	}
	ev event "lose-server restored $(tr '\n' ';' <<<"$out" | cut -c1-900)"
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
if [ "$SOAK_PATHS" = 1 ]; then ROUTED_APPS=${ROUTED_APPS:-www api proot papi}; else ROUTED_APPS=${ROUTED_APPS:-www api}; fi
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
		jq -r '[.routes[] | "\(.host)\(.path // "")=\(.endpoints | join(","))"] | join(" ")' 2>/dev/null) || r=down
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
# Until every worker is ready, the cluster has converged and every replica
# runs. NAME (default lose-server-MODE) names the scenario; DNS_DELAY
# delays the record's change, in seconds.
server_move() {
	local new=$1 newip=$2 mode=$3 name=${4:-lose-server-$3}
	local pre=$SOAK_DIR/pre-$name.tsv cur=$SOAK_DIR/measure-$name.tsv
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
				# DNS_DELAY: seconds between the API up and the record's change,
				# as a record with a TTL would take to reach the workers
				case "$mode" in
				name) (sleep "${DNS_DELAY:-0}" && dns_set "$newip") >>"$SOAK_DIR/restore.out" 2>&1 & ;;
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
	ev event "$name server-node $(hm "$SERVER" nodes --json | jq -c --arg i "$sid" '.[] | select(.id == $i) | {id, address, advertise, state}')"
	# what hivemind nodes shows the operator: the server's row names what the workers dial
	ev event "$name nodes-table $(hm "$SERVER" nodes 2>&1 | tr -s ' \n' ' ' | cut -c1-900)"
	ev event "$name nodes-dials $(hm "$SERVER" nodes --json | jq -c '[.[] | {id, state, dials}]')"
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

# The old server's own view, on its machine: whether its API answers, the
# state of each node in its store, its routing table (host/path=endpoint
# addresses), and the hivemind containers that run there.
read -r -d '' REMOTE_OLD <<'EOF' || true
h=$(curl -fsS -m 2 http://127.0.0.1:4483/healthz >/dev/null 2>&1 && echo up || echo down)
n=$(/usr/local/bin/hivemind nodes --json 2>/dev/null | jq -r '[.[] | "\(.id)=\(.state)"] | join(",")')
tok=$(cat /var/lib/hivemind/admin.token 2>/dev/null)
r=$(printf 'header = "Authorization: Bearer %s"\n' "$tok" | curl -sf -K - -m 1 http://127.0.0.1:4483/v1/routes 2>/dev/null |
	jq -r '[.routes[] | "\(.host)\(.path // "/")=\(.endpoints | map(split(":")[0]) | join("+"))"] | join(",")' 2>/dev/null)
c=$(docker ps --filter label=hivemind.app --format '{{.Label "hivemind.app"}}' | sort | uniq -c | awk '{ printf "%s:%s,", $2, $1 }')
echo "api=$h nodes=${n:--} routes=${r:--} containers=${c:--}"
EOF

# old_view LINE: the node IDs of a REMOTE_OLD line as names; the restored
# server's ID is "self" there, since the old server has the same node ID.
# Addresses stay addresses.
old_view() {
	local l=$1 id nm sid
	sid=$(id_of "$SERVER")
	l=${l//$sid=/self=}
	while read -r id nm; do l=${l//$id=/$nm=}; done <"$IDS_FILE"
	echo "$l"
}

# old_back OLD OLDIP W: the old server's machine OLD, at OLDIP, lost and
# restored elsewhere with server-move, is powered on again by the operator
# with its data directory and its unit enabled, which docs/backup.md warns
# against. Measured once a second until OLD_SECS (300) after its API
# answers: its port 22 and API, and its own view (REMOTE_OLD) at each change;
# the restored server's view of W, convergence and duplicates. From the time
# OLD answers on port 22 the clients send their streams to OLD's router too,
# as clients whose DNS answer still has its address would.
# REDIRECT_AFTER (30) s after OLD's API answers, worker W is sent to OLD, as
# a resolver with the name's old record would send it: W's line for the name
# names OLDIP, and W's connections to the restored server are reset
# (iptables REJECT with tcp-reset on W's output to $SERVER_IP:4484), so that
# W dials the name again. REDIRECT_SECS (30) later W's line names the
# restored server again and the rule is removed; the log says when W is
# ready on the restored server again. At OLD_SECS the old server's unit is
# disabled and stopped (systemctl disable --now), as docs/backup.md says to
# do, and the measure goes on until W is ready again and the cluster has
# converged (or 300 s). Then the journals of OLD, W and the server, and the
# epoch lines from each. The operator powers OLD off afterwards.
old_back() {
	local old=$1 oldip=$2 W=$3 name=old-server-back cur=$SOAK_DIR/measure-old-server-back.tsv
	local end t d v p22=down oview="" up_at=0 redir=0 stopped=0 wst="" conv="" dupl="" t0 wid now stop_at=0
	local ra=${REDIRECT_AFTER:-30} rs=${REDIRECT_SECS:-30} os=${OLD_SECS:-300}
	t0=$(date -u +%s)
	wid=$(id_of "$W")
	ev scenario "$name begin target=$old/$oldip redirect=$W"
	end=$(($(date +%s) + 1800))
	while [ "$(date +%s)" -lt "$end" ]; do
		t=$(now_ms)
		now=$(date +%s)
		if port22_ip "$oldip"; then v=up; else v=down; fi
		[ "$v" = "$p22" ] || {
			ev event "$name $old port22-$v"
			p22=$v
			if [ "$v" = up ] && [ "$stopped" = 0 ] && [ "$SOAK_ROUTES" = 1 ] && [ -n "$OUT_NAME" ]; then
				"$HERE/outside.sh" client-add "$oldip" >>"$SOAK_DIR/old.out" 2>&1 &
			fi
		}
		if [ "$p22" = up ]; then
			v=$(SSH_CT=1 SSH_TIMEOUT=4 on_ip "$oldip" "sudo bash -s" <<<"$REMOTE_OLD" 2>/dev/null) || v="api=unknown ssh=fail"
			v=$(old_view "$v")
			[ "$v" = "$oview" ] || {
				ev event "$name old-view $v"
				oview=$v
			}
			if [ "$up_at" = 0 ] && [[ "$v" == api=up* ]]; then
				up_at=$now
				ev event "$name old-api-up"
			fi
		fi
		# W to the old server, and back
		if [ "$up_at" != 0 ] && [ "$redir" = 0 ] && [ "$now" -ge $((up_at + ra)) ]; then
			dns_line "$W" "$oldip"
			on "$W" "sudo iptables -I OUTPUT -p tcp -d $SERVER_IP --dport 4484 -j REJECT --reject-with tcp-reset"
			ev event "$name $W fault redirect to $oldip"
			redir=1
		elif [ "$redir" = 1 ] && [ "$now" -ge $((up_at + ra + rs)) ]; then
			dns_line "$W" "$SERVER_IP"
			on "$W" "sudo iptables -D OUTPUT -p tcp -d $SERVER_IP --dport 4484 -j REJECT --reject-with tcp-reset"
			ev event "$name $W heal: name and connections to $SERVER_IP again"
			redir=2
		fi
		if [ "$up_at" != 0 ] && [ "$stopped" = 0 ] && [ "$now" -ge $((up_at + os)) ]; then
			on_ip "$oldip" "sudo systemctl disable --now hivemind" >/dev/null 2>&1
			ev event "$name $old stopped: systemctl disable --now hivemind"
			stopped=1 stop_at=$now
		fi
		# the restored server's view
		collect "$cur" nodes
		if api_ok "$cur"; then
			v=$(node_state "$cur" "$wid")
			[ "$v" = "$wst" ] || {
				ev event "$name $W node-$v (restored server)"
				wst=$v
			}
			if check_converged "$cur" 0 >/dev/null; then v=yes; else v=no; fi
			[ "$v" = "$conv" ] || {
				ev event "$name converged-$v $(running "$cur")"
				conv=$v
			}
		fi
		d=$(dups "$cur" | tr '\n' ';')
		[ "$d" = "$dupl" ] || {
			ev event "$name dups ${d:-none}"
			dupl=$d
		}
		if [ "$stopped" = 1 ] && { { [ "$wst" = ready ] && [ "$conv" = yes ] && [ "$redir" = 2 ]; } || [ "$now" -ge $((stop_at + 300)) ]; }; then
			break
		fi
		d=$((1000 - ($(now_ms) - t)))
		[ "$d" -le 0 ] || sleep "$((d / 1000)).$(printf '%03d' $((d % 1000)))"
	done
	wait
	if [ "$SOAK_ROUTES" = 1 ] && [ -n "$OUT_NAME" ]; then "$HERE/outside.sh" client-drop "$oldip" >>"$SOAK_DIR/old.out" 2>&1 || true; fi
	on_ip "$oldip" "sudo journalctl -u hivemind -b --no-pager -o short-iso-precise" >"$SOAK_DIR/journal-$name-$old.txt" 2>/dev/null || true
	on "$W" "sudo journalctl -u hivemind --since @$t0 --no-pager -o short-iso-precise" >"$SOAK_DIR/journal-$name-$W.txt" 2>/dev/null || true
	on "$SERVER" "sudo journalctl -u hivemind --since @$t0 --no-pager -o short-iso-precise" >"$SOAK_DIR/journal-$name-$SERVER.txt" 2>/dev/null || true
	ev event "$name old-journal $(grep -E 'epoch=|newer copy|start grace|wait|node lost' "$SOAK_DIR/journal-$name-$old.txt" | head -n 8 | cut -c1-400 | tr '\n' ';')"
	ev event "$name worker-journal $(grep -E 'before a restore|answers at this|epoch' "$SOAK_DIR/journal-$name-$W.txt" | head -n 6 | cut -c1-500 | tr '\n' ';')"
	ev event "$name server-journal lost=$(grep -c 'node lost' "$SOAK_DIR/journal-$name-$SERVER.txt") $(grep -E 'node lost|epoch' "$SOAK_DIR/journal-$name-$SERVER.txt" | head -n 4 | cut -c1-400 | tr '\n' ';')"
	ev event "$name $W cluster-epoch=$(on "$W" "sudo cat $DATA/cluster-epoch" 2>&1 | tr -d '\n') node.json-server=$(on "$W" "sudo jq -r .server $DATA/node.json")"
	end_scenario "$name"
}

# The certificate a router serves for each host, read from the outside
# machine: "host serial notAfter(Unix s)" lines.
read -r -d '' REMOTE_CERTS <<'EOF' || true
ip=$1
shift
for h in "$@"; do
	r=$(timeout 5 openssl s_client -connect "$ip:443" -servername "$h" -CAfile /var/lib/soak-client/root.pem </dev/null 2>/dev/null | openssl x509 -noout -serial -enddate 2>/dev/null)
	s=$(sed -n 's/^serial=//p' <<<"$r")
	e=$(sed -n 's/^notAfter=//p' <<<"$r")
	[ -n "$e" ] && e=$(date -d "$e" +%s)
	echo "$h ${s:-none} ${e:-0}"
done
EOF
if [ "$SOAK_PATHS" = 1 ]; then CERT_HOSTS=${CERT_HOSTS:-www.soak.test api.soak.test $PATH_HOST}; else CERT_HOSTS=${CERT_HOSTS:-www.soak.test api.soak.test}; fi
served_certs() { on "$OUT_NAME" "bash -s $(ip_of "$1") $CERT_HOSTS" <<<"$REMOTE_CERTS"; }

# cert_restart T [LEFT]: restart T's service once a certificate its router
# serves has LEFT seconds (2985) or less of its lifetime left, so that the
# router starts with a certificate in its renewal window: these one-hour
# certificates are renewed once 50 minutes are left. The certificates T
# serves are read every 5 s, for at most 40 minutes. Then for 90 s the
# serials T serves, at each change; and T's journal lines about certificates
# since the restart, and Pebble's issuances.
cert_restart() {
	local T=$1 left=${2:-2985} name=cert-restart end now c m t1 s last=""
	ev scenario "$name begin target=$T left_max=$left"
	end=$(($(date +%s) + 2400))
	while :; do
		now=$(date +%s)
		c=$(served_certs "$T" 2>/dev/null)
		m=$(awk -v now="$now" '$3 > 0 { l = $3 - now; if (m == "" || l < m) m = l } END { print (m == "" ? 99999 : m) }' <<<"$c")
		[ "$m" -le "$left" ] && break
		[ "$now" -lt "$end" ] || die "$name: no certificate on $T with $left s left in 40 minutes: $(tr '\n' ';' <<<"$c")"
		sleep 5
	done
	ev event "$name $T certs-before $(awk -v now="$now" '{ printf "%s:%s:left=%ds ", $1, substr($2, 1, 12), $3 - now }' <<<"$c")"
	t1=$(date -u +%s)
	ev event "$name $T fault restart"
	on "$T" "sudo systemctl restart hivemind" || die "$name: restart on $T"
	end=$(($(date +%s) + 90))
	while [ "$(date +%s)" -lt "$end" ]; do
		now=$(date +%s)
		s=$(served_certs "$T" 2>/dev/null | awk -v now="$now" '{ printf "%s:%s:left=%ds ", $1, substr($2, 1, 12), $3 - now }')
		[ "$s" = "$last" ] || {
			ev event "$name $T certs $s"
			last=$s
		}
		sleep 5
	done
	ev event "$name $T journal $(on "$T" "sudo journalctl -u hivemind --since @$t1 -o cat --no-pager" | grep -iE 'renew|certif|obtain|acme|router ready' | cut -c1-300 | head -n 12 | tr '\n' ';')"
	ev event "$name pebble $(on "$OUT_NAME" "sudo docker logs --since $t1 soak-pebble 2>&1" | grep -E 'Issued certificate|POST /finalize|validat' | cut -c1-200 | head -n 12 | tr '\n' ';')"
	on "$T" "sudo journalctl -u hivemind --since @$t1 --no-pager -o short-iso-precise" >"$SOAK_DIR/journal-$name-$T.txt" 2>/dev/null || true
	ev scenario "$name end"
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
# args: the API URL, the token file and the CA (api_args in lib.sh)
u=${1:-http://127.0.0.1:4483} tf=${2:-/var/lib/hivemind/admin.token} ca=${3:--}
co=()
[ "$ca" = - ] || co=(--cacert "$ca")
tok=$(cat "$tf") || exit 1
printf 'header = "Authorization: Bearer %s"\n' "$tok" | curl -sS -K - "${co[@]}" -m 5 "$u/metrics" | grep '^hivemind_router_'
EOF
metrics_nodes() { # metrics_nodes FILE: "name=series ..." for the request counters
	awk -F'node="' '/^hivemind_router_requests_total\{/ { split($2, a, "\""); n[a[1]]++ } END { for (k in n) print k, n[k] }' "$1" |
		while read -r id c; do
			nm=$(name_of_id "$id")
			printf '%s=%s ' "${nm:-$id}" "$c"
		done
}
metrics_snap() {
	local f=$SOAK_DIR/metrics-$1.txt t s
	t=$(now_ms)
	s=$(api_server) && on "$s" "sudo bash -s $(api_args "$s")" <<<"$REMOTE_METRICS" >"$f.tmp" || die "metrics on ${s:-no server}"
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
	local end last="" f=$SOAK_DIR/metrics-watch.txt v s
	end=$(($(date +%s) + ${1:-300}))
	while [ "$(date +%s)" -lt "$end" ]; do
		if s=$(api_server) && on "$s" "sudo bash -s $(api_args "$s")" <<<"$REMOTE_METRICS" >"$f" 2>/dev/null; then
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

# rollout [VERSION [APP...]]: a rolling update of each APP (www) to the
# private image's tag VERSION (2), all deployed at once, measured until no
# rollout is in progress and the cluster has converged. The scenario is
# "rollout" for www alone, else "rollout-APP+APP...".
rollout() {
	local v=${1:-2} apps name a end t d st r="" s done
	shift || true
	apps=${*:-www}
	if [ "$apps" = www ]; then name=rollout; else name=rollout-${apps// /+}; fi
	begin "$name" "$SERVER"
	for a in $apps; do
		ev event "$name $a fault deploy $(app_spec "$a" "$v" | jq -r .image)"
		hm "$SERVER" "deploy -f -" <<<"$(app_spec "$a" "$v")" >/dev/null || die "$name: deploy $a"
	done
	end=$(($(date +%s) + 600))
	while [ "$(date +%s)" -lt "$end" ]; do
		t=$(now_ms)
		st=$(hm "$SERVER" status --json 2>/dev/null) || st=""
		done=1 s=""
		for a in $apps; do
			v=$(jq -r --arg a "$a" '.[] | select(.name == $a) | (.rollout // {} | "\(.state // "none") target=\(.target // "-") moving=\(.moving // "-")") + " gens=" + ([.replicas[] | "\(.index):\(.generation):\(.state)"] | join(","))' <<<"$st" 2>/dev/null)
			[[ "$v" == none* ]] || done=0
			s+="$a $v;"
		done
		[ "$s" = "$r" ] || {
			ev event "$name $s"
			r=$s
		}
		if [ "$done" = 1 ]; then
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

# The fifth soak: rotations of the cluster CA (docs/ca-rotation.md), and the
# start grace's hold.
#
# A CA is shown by the end of its name: the CA from init is "hivemind cluster
# CA" (shown as ca0); one a rotation makes is "hivemind cluster CA 1a2b3c4d",
# the first 4 bytes of its fingerprint (shown as 1a2b3c4d).

# On the server: the rotation as hivemind ca status --json has it (the CAs,
# phase, direction, whether the server's certificate has moved, the next step
# and the nodes it waits for, and per node live or down, T if it trusts the
# CA the rotation goes to, R if it has renewed under it, O for an old build),
# the CA files in the data directory, and the certificate the join listener
# presents: the node listener presents the same one, and a handshake there
# without a client certificate would be a TLS error in the server's log.
read -r -d '' REMOTE_CA_SERVER <<'EOF' || true
addr=$1
d=/var/lib/hivemind
cn() { sed -n 's/^.*CN *= *hivemind cluster CA *//p' | sed 's/^$/ca0/'; }
s=$(/usr/local/bin/hivemind ca status --json 2>/dev/null | jq -r '
	def tag: if . == null then "-" else (ltrimstr("hivemind cluster CA") | ltrimstr(" ") | if . == "" then "ca0" else . end) end;
	.current.fingerprint as $c | (.next.fingerprint // "") as $n |
	"cur=\(.current.name | tag) next=\(.next.name | tag) listener=\(if .listener == $c then (.current.name | tag) elif .listener == $n then (.next.name | tag) else .listener[0:8] end) " +
	(if .rotation == null then "phase=none" else
		"phase=\(.rotation.phase) toward=\(.rotation.toward) moved=\(.rotation.listener_moved) step=\(.rotation.next_step) waiting=\([.rotation.waiting[] | .node] | join(",") | if . == "" then "-" else . end) nodes=\([(.nodes // [])[] | "\(.id)=\(if .live then "live" else "down" end):\(if .trusts then "T" else "-" end)\(if .renewed then "R" else "-" end)\(if .old_build then "O" else "" end)"] | join(","))"
	end)' 2>/dev/null) || s=""
f=$(cd $d && for x in ca.crt ca-next.crt; do [ -f $x ] && printf '%s:%s,' $x "$(openssl x509 -in $x -noout -subject | cn)"; done; for x in ca.key ca-next.key; do [ -f $x ] && printf '%s,' $x; done)
l=$(timeout 3 openssl s_client -connect $addr:4485 </dev/null 2>/dev/null | openssl x509 -noout -issuer -serial 2>/dev/null)
li=$(sed -n '/^issuer/p' <<<"$l" | cn)
ls=$(sed -n 's/^serial=//p' <<<"$l")
echo "${s:-status=fail} files=${f:--} served=${li:-none}:${ls: -8}"
EOF

# On a worker, its own view: the CAs in ca.crt, in order; the issuer, serial
# and expiry of node.crt; whether the server's certificate, from the join
# listener at NAME (what the worker dials), verifies against ca.crt for NAME,
# and its issuer; and whether a ca.crt.next is there.
read -r -d '' REMOTE_CA_WORKER <<'EOF' || true
name=$1
d=/var/lib/hivemind
cn() { sed -n 's/^.*CN *= *hivemind cluster CA *//p' | sed 's/^$/ca0/'; }
[ -f $d/ca.crt ] || { echo "ca.crt=none"; exit 0; }
cas=$(openssl crl2pkcs7 -nocrl -certfile $d/ca.crt 2>/dev/null | openssl pkcs7 -print_certs -noout 2>/dev/null | grep '^subject' | cn | tr '\n' '+')
n=$(openssl x509 -in $d/node.crt -noout -issuer -serial -enddate 2>/dev/null)
ni=$(sed -n '/^issuer/p' <<<"$n" | cn)
ns=$(sed -n 's/^serial=//p' <<<"$n")
ne=$(date -u -d "$(sed -n 's/^notAfter=//p' <<<"$n")" +%FT%TZ 2>/dev/null)
# No -verify_return_error: an aborted handshake is a TLS error in the
# server's log ("bad record MAC"); s_client reports the result either way.
v=$(timeout 3 openssl s_client -connect $name:4485 -verify_hostname $name -CAfile $d/ca.crt </dev/null 2>&1)
vc=$(sed -n 's/^ *Verify return code: \([0-9]*\).*/\1/p' <<<"$v" | tail -n 1)
si=$(openssl x509 -noout -issuer 2>/dev/null <<<"$v" | cn)
echo "ca.crt=${cas%+} node.crt=${ni:-none}:${ns: -8}:${ne:-?} server=${si:-none}:verify=${vc:-fail} next=$([ -e $d/ca.crt.next ] && echo yes || echo no)"
EOF

# with_names TEXT: the node IDs in TEXT as soak names.
with_names() {
	local l=$1 id nm
	while read -r id nm; do l=${l//$id/$nm}; done <"$IDS_FILE"
	echo "$l"
}

ca_server_view() { SSH_CT=${SSH_CT:-2} SSH_TIMEOUT=8 on "$SERVER" "sudo bash -s $SERVER_IP" <<<"$REMOTE_CA_SERVER" 2>/dev/null || echo "ssh=fail"; }
ca_worker_view() { SSH_CT=${SSH_CT:-2} SSH_TIMEOUT=8 on "$1" "sudo bash -s $(server_addr)" <<<"$REMOTE_CA_WORKER" 2>/dev/null || echo "ssh=fail"; }

# ca_watch: once a second the server's view, every 2 s each worker's, one
# "ca server" or "ca disk NAME" line at each change. Runs until killed
# (bg_start cawatch); it reads the node list when it starts.
ca_watch() {
	local k=0 t d v i n f=$SOAK_DIR/ca-watch
	declare -A last
	trap 'exit 0' TERM INT
	while :; do
		t=$(now_ms)
		if [ $((k % 2)) -eq 0 ]; then
			for ((i = 1; i < ${#NAMES[@]}; i++)); do
				(SSH_CT=1 ca_worker_view "${NAMES[$i]}" >"$f.$i") &
			done
		fi
		v=$(with_names "$(SSH_CT=1 ca_server_view)")
		[ "$v" = "${last[server]:-}" ] || {
			ev ca "server $v"
			last[server]=$v
		}
		wait
		if [ $((k % 2)) -eq 0 ]; then
			for ((i = 1; i < ${#NAMES[@]}; i++)); do
				n=${NAMES[$i]}
				v=$(cat "$f.$i" 2>/dev/null)
				rm -f "$f.$i"
				[ "$v" = "${last[$n]:-}" ] || {
					ev ca "disk $n $v"
					last[$n]=$v
				}
			done
		fi
		k=$((k + 1))
		d=$((1000 - ($(now_ms) - t)))
		[ "$d" -le 0 ] || sleep "$((d / 1000)).$(printf '%03d' $((d % 1000)))"
	done
}

# ca_views NAME LABEL: the server's view and every worker's, as events, once.
ca_views() {
	local i
	ev event "$1 ca-$2 $SERVER $(with_names "$(ca_server_view)")"
	for ((i = 1; i < ${#NAMES[@]}; i++)); do ev event "$1 ca-$2 ${NAMES[$i]} $(ca_worker_view "${NAMES[$i]}")"; done
}

# ca_text NAME LABEL: what the operator reads: hivemind ca status as text,
# and the lines of hivemind status that speak of the rotation.
ca_text() {
	ev event "$1 ca-status-$2 $(with_names "$(hm "$SERVER" ca status 2>&1 | tr -s ' ' | tr '\n' ';')")"
	ev event "$1 status-$2 $(with_names "$(hm "$SERVER" status 2>&1 | grep -iE 'rotat|cluster CA' | tr -s ' ' | tr '\n' ';')")"
}

# ca_step NAME STEP [--force]: one step on the server, as the operator runs
# it; its reply and exit code, and how long it took.
ca_step() {
	local name=$1 step=$2 force=${3:-} out rc t
	t=$(now_ms)
	ev event "$name $SERVER fault ca-$step${force:+ $force}"
	out=$(hm "$SERVER" "ca rotate $step $force" 2>&1)
	rc=$?
	ev event "$name ca-$step${force:+-force}-done rc=$rc ms=$(($(now_ms) - t)) $(with_names "$(tr '\n' ';' <<<"$out" | tr -s ' ' | cut -c1-1200)")"
	return $rc
}

C_TRUST='.rotation.next_step == "renew" and (.rotation.waiting | length) == 0'
C_RENEWED='.rotation.next_step == "finish" and (.rotation.waiting | length) == 0'
C_MOVED='.rotation.listener_moved == true'

# ca_wait NAME LABEL SECS COND: until hivemind ca status --json meets the jq
# condition COND, polled every 0.5 s; an event when it does.
ca_wait() {
	local name=$1 label=$2 secs=$3 cond=$4 t0 end
	t0=$(now_ms)
	end=$(($(date +%s) + secs))
	while [ "$(date +%s)" -lt "$end" ]; do
		if hm "$SERVER" "ca status --json" 2>/dev/null | jq -e "$cond" >/dev/null 2>&1; then
			ev event "$name $label after_ms=$(($(now_ms) - t0))"
			return 0
		fi
		sleep 0.5
	done
	ev event "$name $label-timeout $(with_names "$(ca_server_view)")"
	return 1
}

# ca_tag: the server's current CA, as shown.
ca_tag() {
	hm "$SERVER" "ca status --json" | jq -r '.current.name | ltrimstr("hivemind cluster CA") | ltrimstr(" ") | if . == "" then "ca0" else . end'
}

# ca_wait_disk NAME LABEL SECS TAG [SKIP]: until every worker but SKIP has
# only TAG in ca.crt and a node certificate from TAG, polled every second.
ca_wait_disk() {
	local name=$1 label=$2 secs=$3 tag=$4 skip=${5:-} t0 end i n ok v
	t0=$(now_ms)
	end=$(($(date +%s) + secs))
	while [ "$(date +%s)" -lt "$end" ]; do
		ok=1
		for ((i = 1; i < ${#NAMES[@]}; i++)); do
			n=${NAMES[$i]}
			[ "$n" = "$skip" ] && continue
			v=$(ca_worker_view "$n")
			[[ "$v" == "ca.crt=$tag node.crt=$tag:"* ]] || {
				ok=0
				break
			}
		done
		if [ "$ok" = 1 ]; then
			ev event "$name $label tag=$tag after_ms=$(($(now_ms) - t0))"
			return 0
		fi
		sleep 1
	done
	ev event "$name $label-timeout tag=$tag"
	return 1
}

# all_kept PRE [SKIP]: like worker_kept, over every node of the list now,
# the server too, but SKIP.
all_kept() {
	local cur=$SOAK_DIR/kept.tsv n
	: >"$cur"
	for n in "${NAMES[@]}"; do
		[ "$n" = "${2:-}" ] && continue
		container_rows_of "$n" >>"$cur" 2>/dev/null || printf 'U\t%s\n' "$n" >>"$cur"
	done
	awk -F'\t' -v w=" ${NAMES[*]} " -v skip="${2:-}" '
		FILENAME == ARGV[1] { if ($1 == "C" && $4 == "running" && index(w, " " $2 " ") && $2 != skip) pre[$2 "|" $3 "|" $5] = $6 "|" $7; next }
		$1 == "U" { out = out "unreachable:" $2 ","; next }
		$1 == "C" && $4 == "running" { k = $2 "|" $3 "|" $5; now[k] = 1; if (!(k in pre)) out = out "new:" $2 ":" $3 ":" $6 "|" $7 "," }
		END { for (k in pre) if (!(k in now)) { split(k, a, "|"); out = out "gone:" a[1] ":" a[2] ":" pre[k] "," } print (out == "" ? "unchanged" : out) }' "$1" "$cur"
}

# ids_check: the node IDs in hivemind nodes are those of the ID map, and each
# worker's node.json has its ID: no node joined again.
ids_check() {
	local want got i n bad=""
	want=$(awk '{ print $1 }' "$IDS_FILE" | sort | tr '\n' ' ')
	got=$(hm "$SERVER" nodes --json | jq -r '.[].id' | sort | tr '\n' ' ')
	[ "$want" = "$got" ] || bad="nodes=[$got] map=[$want] "
	for ((i = 1; i < ${#NAMES[@]}; i++)); do
		n=${NAMES[$i]}
		[ "$(on "$n" "sudo jq -r .node_id $DATA/node.json")" = "$(id_of "$n")" ] || bad+="$n:node.json-differs "
	done
	echo "${bad:-same}"
}

# ca_after NAME T0 PRE [SKIP]: after a rotation: the containers (all_kept),
# the node IDs, the TLS handshake errors and the lines that name a node
# whose handshake was refused (cut_off_lines, which replace the first for
# such a node) and refusals in the server's log
# since T0 (Unix s), the TLS and x509 lines in each worker's, the joins, the
# node certificates' expiry the server records, and the journals to files.
ca_after() {
	local name=$1 t0=$2 pre=$3 skip=${4:-} i n
	ev event "$name kept $(all_kept "$pre" "$skip")"
	ev event "$name ids $(ids_check)"
	on "$SERVER" "sudo journalctl -u hivemind --since @$t0 -o short-iso-precise --no-pager" >"$SOAK_DIR/journal-$name-$SERVER.txt" 2>/dev/null || true
	ev event "$name server-log tls_handshake_errors=$(grep -c 'TLS handshake error' "$SOAK_DIR/journal-$name-$SERVER.txt") cut_off_lines=$(grep -c 'certificate from a CA this cluster does not trust, and was refused\|a node refused this server.s certificate' "$SOAK_DIR/journal-$name-$SERVER.txt") refused=$(grep -c 'request refused' "$SOAK_DIR/journal-$name-$SERVER.txt") joined=$(grep -c 'node joined' "$SOAK_DIR/journal-$name-$SERVER.txt") errors=$(grep -c 'level=ERROR' "$SOAK_DIR/journal-$name-$SERVER.txt") warns=$(grep -c 'level=WARN' "$SOAK_DIR/journal-$name-$SERVER.txt") toward_new=$(grep -c 'the CA the rotation goes to, the new one' "$SOAK_DIR/journal-$name-$SERVER.txt") toward_old=$(grep -c 'the CA the rotation goes back to, the old one' "$SOAK_DIR/journal-$name-$SERVER.txt") says_new_cluster_ca=$(grep -c 'the new cluster CA' "$SOAK_DIR/journal-$name-$SERVER.txt")"
	for ((i = 1; i < ${#NAMES[@]}; i++)); do
		n=${NAMES[$i]}
		on "$n" "sudo journalctl -u hivemind --since @$t0 -o short-iso-precise --no-pager" >"$SOAK_DIR/journal-$name-$n.txt" 2>/dev/null || true
		ev event "$name $n worker-log tls_x509_lines=$(grep -ciE 'x509|tls:|handshake' "$SOAK_DIR/journal-$name-$n.txt") errors=$(grep -c 'level=ERROR' "$SOAK_DIR/journal-$name-$n.txt") warns=$(grep -c 'level=WARN' "$SOAK_DIR/journal-$name-$n.txt") ca_lines=$(grep -cE 'cluster CA|renew' "$SOAK_DIR/journal-$name-$n.txt") renewals=$(grep 'renewing the node certificate' "$SOAK_DIR/journal-$name-$n.txt" | sed -n 's/.*\(ca=[0-9a-f]*\).*\(from=[0-9a-f]*\).*/\1:\2/p' | tr '\n' ',')"
	done
	ev event "$name node-cert-expiry $(with_names "$(on "$SERVER" "sudo bash -s" <<<"$REMOTE_CERT_EXPIRY" 2>/dev/null | tr '\n' ' ')")"
}

read -r -d '' REMOTE_CERT_EXPIRY <<'EOF' || true
tok=$(cat /var/lib/hivemind/admin.token) || exit 1
printf 'header = "Authorization: Bearer %s"\n' "$tok" | curl -sS -K - -m 5 http://127.0.0.1:4483/metrics |
	sed -n 's/^hivemind_node_cert_expiry_timestamp_seconds{node="\([^"]*\)"} \(.*\)$/\1 \2/p' |
	while read -r n v; do echo "$n=$(date -u -d @"$(printf '%.0f' "$v")" +%FT%TZ)"; done
EOF

# ca_rotate: a rotation under the clients, each step once the one before
# it is done: start; renew once every node trusts the new CA; finish once
# every node has renewed and the server's certificate has moved; then until
# every worker holds the new CA alone.
ca_rotate() {
	local name=${1:-ca-rotate} pre t0
	pre=$SOAK_DIR/pre-$name.tsv
	begin "$name" "$SERVER"
	t0=$(date -u +%s)
	bg_start cawatch "$HERE/scenario.sh" ca-watch
	ca_views "$name" before
	ca_step "$name" start || die "$name: start"
	ca_wait "$name" trust-all 300 "$C_TRUST" || die "$name: not every node trusts the new CA"
	ca_views "$name" trust
	ca_step "$name" renew || die "$name: renew"
	ca_wait "$name" renewed-all 300 "$C_RENEWED" || die "$name: not every node renewed"
	ca_wait "$name" listener-moved 300 "$C_MOVED" || die "$name: the server's certificate did not move"
	ca_views "$name" renew
	ca_text "$name" renew
	ca_step "$name" finish || die "$name: finish"
	ca_wait_disk "$name" workers-one-ca 120 "$(ca_tag)"
	sleep 5
	ca_views "$name" after
	bg_stop cawatch
	ca_after "$name" "$t0" "$pre"
	end_scenario "$name"
}

port22_down() { ! port22 "$1"; }

# ca_offline T: a rotation with worker T powered off. The operator powers T
# off when the log says "ca-offline: power ... off now", and on again when it
# says "... on now"; this waits for each. With T lost, its replicas
# elsewhere: start; renew, which T does not hold back; finish, which must
# refuse naming T; finish --force. T then comes back cut off: the line its
# log says it with, its containers, then hivemind node remove and a join
# from an empty data directory (setup.sh rejoin), until converged.
ca_offline() {
	local T=$1 name=ca-offline pre=$SOAK_DIR/pre-ca-offline.tsv id t0 t1 end v line=""
	begin "$name" "$T"
	id=$(id_of "$T")
	t0=$(date -u +%s)
	bg_start cawatch "$HERE/scenario.sh" ca-watch
	note "$name: power $T off now"
	wait_for 900 port22_down "$T" || die "$name: $T did not go down"
	measure "$name" "$T" loss 300 || die "$name: $T was not lost"
	ca_views "$name" before
	ca_step "$name" start || die "$name: start"
	ca_wait "$name" trust-live 300 "$C_TRUST" || die "$name: not every live node trusts the new CA"
	ca_views "$name" trust
	ca_step "$name" renew || die "$name: renew"
	ca_wait "$name" renewed-live 300 "([.nodes[] | select(.live and (.renewed | not))] | length) == 0 and ([.rotation.waiting[].node] == [\"$id\"])" ||
		die "$name: the live nodes did not renew"
	sleep 5
	ev event "$name status-before-finish $(with_names "$(ca_server_view)")"
	ca_views "$name" renew
	ca_step "$name" finish && die "$name: finish did not refuse with $T down"
	ca_step "$name" finish --force || die "$name: finish --force"
	ca_wait_disk "$name" workers-one-ca 120 "$(ca_tag)" "$T"
	ca_views "$name" after
	note "$name: power $T on now"
	wait_for 900 port22 "$T" || die "$name: $T did not come back"
	t1=$(date -u +%s)
	forget_host "$T"
	ev event "$name $T back"
	end=$(($(date +%s) + 300))
	while [ "$(date +%s)" -lt "$end" ]; do
		line=$(on "$T" "sudo journalctl -u hivemind -b -o short-iso-precise --no-pager | grep -m1 -E 'does not trust|unknown authority'" 2>/dev/null) && [ -n "$line" ] && break
		sleep 2
	done
	ev event "$name $T cut-off-line ${line:-none}"
	sleep 30
	ev event "$name $T after-30s $(ca_worker_view "$T") containers=$(on "$T" "sudo docker ps --filter label=hivemind.app --format '{{.Label \"hivemind.app\"}}|{{.Label \"hivemind.replica\"}}'" | sort | tr '\n' ',') state=$(hm "$SERVER" nodes --json | jq -r --arg i "$id" '.[] | select(.id == $i) | .state')"
	snapshot "$SOAK_DIR/now.tsv" && ev event "$name dups $(dups "$SOAK_DIR/now.tsv" | tr '\n' ';')"
	on "$T" "sudo journalctl -u hivemind -b -o short-iso-precise --no-pager" >"$SOAK_DIR/journal-$name-$T-cutoff.txt" 2>/dev/null || true
	ev event "$name $T cut-off-lines $(grep -cE 'does not trust|unknown authority' "$SOAK_DIR/journal-$name-$T-cutoff.txt") first-and-last $(grep -E 'does not trust|unknown authority' "$SOAK_DIR/journal-$name-$T-cutoff.txt" | sed -n '1s/ .*//p;$s/ .*//p' | tr '\n' ' ')"
	ev event "$name $T fault node-remove"
	v=$(hm "$SERVER" node remove "$id" 2>&1)
	ev event "$name $T removed rc=$? $(tr '\n' ';' <<<"$v" | cut -c1-400)"
	ev event "$name $T fault rejoin"
	"$HERE/setup.sh" rejoin "$T" >>"$SOAK_DIR/rejoin.out" 2>&1 || die "$name: rejoin $T"
	wait_for 300 converged_now && ev event "$name converged-yes"
	ca_views "$name" rejoined
	bg_stop cawatch
	ca_after "$name" "$t0" "$pre" "$T"
	end_scenario "$name"
}

# On the server: renew, then abort as soon as at least one node has renewed
# and not every one has (or MAX ms after renew returned), polling GET /v1/ca
# every 50 ms. Server times, Unix ms.
read -r -d '' REMOTE_ABORT <<'EOF' || true
max=$1
HM=/usr/local/bin/hivemind
tok=$(cat /var/lib/hivemind/admin.token)
st() { printf 'header = "Authorization: Bearer %s"\n' "$tok" | curl -sf -K - -m 2 http://127.0.0.1:4483/v1/ca; }
t0=$(date +%s%3N)
out=$($HM ca rotate renew 2>&1); rc=$?
t1=$(date +%s%3N)
echo "renew rc=$rc server_ms=$t1 took_ms=$((t1 - t0))"
[ $rc = 0 ] || { echo "$out" | tr '\n' ';'; exit 1; }
while :; do
	s=$(st)
	now=$(date +%s%3N)
	r=$(jq -r '[.nodes[] | select(.renewed)] | length' <<<"$s")
	a=$(jq -r '.nodes | length' <<<"$s")
	[ "$r" -ge 1 ] && [ "$r" -lt "$a" ] && break
	[ $((now - t1)) -ge "$max" ] && break
	sleep 0.05
done
echo "abort-at server_ms=$now after_renew_ms=$((now - t1)) renewed=$r/$a moved=$(jq -r .rotation.listener_moved <<<"$s") nodes=$(jq -r '[.nodes[] | "\(.id)=\(if .renewed then "R" else "-" end)"] | join(",")' <<<"$s")"
out=$($HM ca rotate abort 2>&1); rc=$?
echo "abort rc=$rc server_ms=$(date +%s%3N) $(echo "$out" | tr '\n' ';' | tr -s ' ' | cut -c1-900)"
EOF

# ca_abort: start; renew once every node trusts the new CA; abort in phase
# renew (REMOTE_ABORT); then the turned rotation, back to the CA it came
# from, until every node has renewed back and the server's certificate is
# back, and finish.
ca_abort() {
	local name=ca-abort pre=$SOAK_DIR/pre-ca-abort.tsv t0 from out l
	begin "$name" "$SERVER"
	t0=$(date -u +%s)
	bg_start cawatch "$HERE/scenario.sh" ca-watch
	from=$(ca_tag)
	ca_views "$name" before
	ca_step "$name" start || die "$name: start"
	ca_wait "$name" trust-all 300 "$C_TRUST" || die "$name: not every node trusts the new CA"
	ca_views "$name" trust
	ev event "$name $SERVER fault ca-renew-then-abort"
	out=$(SSH_TIMEOUT=60 on "$SERVER" "sudo bash -s ${ABORT_MAX_MS:-10000}" <<<"$REMOTE_ABORT" 2>&1)
	while read -r l; do ev event "$name $(with_names "$l")"; done <<<"$out"
	ca_text "$name" after-abort
	ca_wait "$name" turned-back 300 ".rotation.toward == \"current\" and $C_RENEWED and $C_MOVED" || die "$name: the turned rotation did not come back"
	ca_views "$name" turned
	ca_text "$name" turned
	ca_step "$name" finish || die "$name: finish"
	ca_wait_disk "$name" workers-one-ca 120 "$from"
	sleep 5
	ca_views "$name" after
	bg_stop cawatch
	ca_after "$name" "$t0" "$pre"
	end_scenario "$name"
}

# ca_restore_begin: the first half of a restore during a rotation: start,
# renew, every node renewed and the server's certificate moved, then the
# backup (server-backup), in phase renew. Then the operator loses the
# server and server-move restores it (scenario ca-restore), and ca-finish
# ends the rotation on the restored server.
ca_restore_begin() {
	local name=ca-restore
	begin "$name" "$SERVER"
	date -u +%s >"$SOAK_DIR/ca-restore.t0"
	bg_start cawatch "$HERE/scenario.sh" ca-watch
	ca_views "$name" before
	ca_step "$name" start || die "$name: start"
	ca_wait "$name" trust-all 300 "$C_TRUST" || die "$name: not every node trusts the new CA"
	ca_step "$name" renew || die "$name: renew"
	ca_wait "$name" renewed-all 300 "$C_RENEWED" || die "$name: not every node renewed"
	ca_wait "$name" listener-moved 300 "$C_MOVED" || die "$name: the server's certificate did not move"
	ca_views "$name" renew
	server_backup
	ca_views "$name" backup
	bg_stop cawatch
	note "$name: backup taken in phase renew; power the server off now"
}

# ca_finish NAME: on the restored server, the rotation as it came back, until
# every node has renewed (the restored server learns it from their
# requests), then finish, until every worker holds the new CA alone.
ca_finish() {
	local name=${1:-ca-finish} t0
	ev scenario "$name begin target=$SERVER"
	t0=$(date -u +%s)
	bg_start cawatch "$HERE/scenario.sh" ca-watch
	ca_views "$name" restored
	ca_wait "$name" renewed-all 300 "$C_RENEWED and $C_MOVED"
	ca_step "$name" finish
	ca_wait_disk "$name" workers-one-ca 120 "$(ca_tag)"
	sleep 5
	ca_views "$name" after
	bg_stop cawatch
	ev event "$name workers-kept-since-backup $(worker_kept "$SOAK_DIR/pre-ca-restore.tsv")"
	ca_after "$name" "$t0" "$SOAK_DIR/pre-ca-restore.tsv" "$SERVER"
	end_scenario "$name"
}

# start_hold: the server's machine is stopped by the operator (EC2 stop)
# when the log says "start-hold: stop ...", and every worker's packets to
# the server's port 4484 are dropped (iptables on each worker) once it is
# down; the operator then starts it. Once a second until HOLD_AFTER (90) s
# past the end of its start grace (START_GRACE, 120): its API, the state of
# each worker in its hivemind nodes (+held for start_held), the containers on
# its own machine, its status line, and its error line. Then the rules come
# off, and until every worker is ready and the cluster has converged. With
# HEAL_LATE=W (scenario start-hold-late), W's rule comes off HEAL_LAG (12) s
# after the others', so that the hold ends with W not yet reported. With
# HEAL_LAGS="W1:0 W2:5 W3:20" (scenario start-hold-stagger, the sixth soak),
# each listed worker's rule comes off that many seconds after the heal
# begins (a worker not listed: at once), each from its own timer. HOLD_NAME
# names the scenario; a negative HOLD_AFTER heals inside the start grace,
# with no hold (restart-cutoff: the server's machine restarted while its
# workers cannot reach it, for the routers' stale tables).
start_hold() {
	local name=start-hold pre=$SOAK_DIR/pre-start-hold.tsv cur=$SOAK_DIR/measure-start-hold.tsv
	local grace=${START_GRACE:-120} after=${HOLD_AFTER:-90} t0 t now d v i n s api=down p22=down up_at=0 heal_at=0
	local lastn="" lastc="" lastp="" lasts="" errl="" ready="" conv="" dupl="" end late=${HEAL_LATE:-} lag=${HEAL_LAG:-12} late_done=0
	local lags=${HEAL_LAGS:-} maxlag=0 p w
	declare -A lag_of
	[ -z "$late" ] || name=start-hold-late
	if [ -n "$lags" ]; then
		name=start-hold-stagger late=""
		for p in $lags; do
			lag_of[${p%%:*}]=${p#*:}
			[ "${p#*:}" -le "$maxlag" ] || maxlag=${p#*:}
		done
	fi
	name=${HOLD_NAME:-$name}
	pre=$SOAK_DIR/pre-$name.tsv cur=$SOAK_DIR/measure-$name.tsv
	begin "$name" "$SERVER"
	t0=$(date -u +%s)
	note "$name: stop the server's machine now"
	wait_for 900 port22_down "$SERVER" || die "$name: the server's machine did not stop"
	ev event "$name $SERVER port22-down"
	for ((i = 1; i < ${#NAMES[@]}; i++)); do
		n=${NAMES[$i]}
		on "$n" "sudo iptables -I OUTPUT -p tcp -d $SERVER_IP --dport 4484 -j DROP" || die "$name: iptables on $n"
		ev event "$name $n fault drop to $SERVER_IP:4484"
	done
	forget_host "$SERVER"
	note "$name: start the server's machine now"
	end=$(($(date +%s) + 1800))
	while [ "$(date +%s)" -lt "$end" ]; do
		t=$(now_ms)
		now=$(date +%s)
		if port22 "$SERVER"; then v=up; else v=down; fi
		[ "$v" = "$p22" ] || {
			ev event "$name $SERVER port22-$v"
			p22=$v
		}
		if [ "$p22" = up ] && SSH_CT=1 SSH_TIMEOUT=4 on "$SERVER" "curl -fsS -m 2 http://127.0.0.1:4483/healthz" >/dev/null 2>&1; then v=up; else v=down; fi
		[ "$v" = "$api" ] || {
			ev event "$name api-$v"
			api=$v
			[ "$v" = up ] && [ "$up_at" = 0 ] && up_at=$now
		}
		if [ "$api" = up ]; then
			v=$(with_names "$(hm "$SERVER" nodes --json 2>/dev/null | jq -r '[.[] | select(.server | not) | "\(.id)=\(.state)\(if .start_held then "+held" else "" end)\(if .start_hold_ended then "+hold-ended" else "" end)"] | join(",")')")
			[ "$v" = "$lastn" ] || {
				ev event "$name nodes $v"
				lastn=$v
			}
			v=$(on "$SERVER" "sudo docker ps --filter label=hivemind.app --format '{{.Label \"hivemind.app\"}}|{{.Label \"hivemind.replica\"}}'" 2>/dev/null | sort | tr '\n' ',')
			[ "$v" = "$lastc" ] || {
				ev event "$name server-containers ${v:-none}"
				lastc=$v
			}
			# the server's routed containers and their host ports, at each change
			v=$(SSH_CT=1 routed_ports "$SERVER" 2>/dev/null) || v=fail
			[ "$v" = "$lastp" ] || {
				ev event "$name server-ports ${v:-none}"
				lastp=$v
			}
			v=$(hm "$SERVER" status 2>/dev/null | grep -iE 'waiting|grace|report' | tr '\n' ';' | cut -c1-600)
			[ "$v" = "$lasts" ] || {
				ev event "$name status-line ${v:-none}"
				lasts=$v
			}
			if [ -z "$errl" ] && [ "$now" -ge $((up_at + grace)) ]; then
				errl=$(on "$SERVER" "sudo journalctl -u hivemind -b -o short-iso-precise --no-pager | grep -m1 'no worker has reported'" 2>/dev/null | cut -c1-900)
				[ -z "$errl" ] || ev event "$name error-line $(with_names "$errl")"
			fi
		fi
		if [ "$heal_at" = 0 ] && [ "$up_at" != 0 ] && [ "$now" -ge $((up_at + grace + after)) ] && [ -n "$lags" ]; then
			ev event "$name heal: begins; lags $lags"
			# the routing table at each change, every 200 ms, through the heal
			SSH_TIMEOUT=$((maxlag + 80)) routes_watch $((maxlag + 60)) &
			for ((i = 1; i < ${#NAMES[@]}; i++)); do
				w=${NAMES[$i]}
				(
					[ "${lag_of[$w]:-0}" = 0 ] || sleep "${lag_of[$w]}"
					on "$w" "sudo iptables -D OUTPUT -p tcp -d $SERVER_IP --dport 4484 -j DROP" || ev event "$name $w heal-failed"
					ev event "$name heal: $w reaches $SERVER_IP:4484 again, lag ${lag_of[$w]:-0}s"
				) &
			done
			heal_at=$now
		fi
		if [ "$heal_at" = 0 ] && [ "$up_at" != 0 ] && [ "$now" -ge $((up_at + grace + after)) ]; then
			for ((i = 1; i < ${#NAMES[@]}; i++)); do
				[ "${NAMES[$i]}" = "$late" ] && continue
				on "${NAMES[$i]}" "sudo iptables -D OUTPUT -p tcp -d $SERVER_IP --dport 4484 -j DROP" || ev event "$name ${NAMES[$i]} heal-failed"
			done
			ev event "$name heal: the workers ${late:+but $late }reach $SERVER_IP:4484 again"
			heal_at=$now
		fi
		if [ -n "$late" ] && [ "$heal_at" != 0 ] && [ "$late_done" = 0 ] && [ "$now" -ge $((heal_at + lag)) ]; then
			on "$late" "sudo iptables -D OUTPUT -p tcp -d $SERVER_IP --dport 4484 -j DROP" || ev event "$name $late heal-failed"
			ev event "$name heal: $late reaches $SERVER_IP:4484 again, ${lag}s after the others"
			late_done=1
		fi
		if [ "$heal_at" != 0 ]; then
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
					ev event "$name converged-$v $(running "$cur")"
					conv=$v
				}
			fi
			d=$(dups "$cur" | tr '\n' ';')
			[ "$d" = "$dupl" ] || {
				ev event "$name dups ${d:-none}"
				dupl=$d
			}
			if { [ "$conv" = yes ] && [ "$(wc -w <<<"$ready")" -eq $((${#NAMES[@]} - 1)) ] && { [ -z "$late" ] || [ "$late_done" = 1 ]; } && [ "$now" -ge $((heal_at + maxlag + 3)) ]; } || [ "$now" -ge $((heal_at + 300)) ]; then
				break
			fi
		fi
		d=$((1000 - ($(now_ms) - t)))
		[ "$d" -le 0 ] || sleep "$((d / 1000)).$(printf '%03d' $((d % 1000)))"
	done
	on "$SERVER" "sudo journalctl -u hivemind -b -o short-iso-precise --no-pager" >"$SOAK_DIR/journal-$name-$SERVER.txt" 2>/dev/null || true
	for ((i = 1; i < ${#NAMES[@]}; i++)); do
		on "${NAMES[$i]}" "sudo journalctl -u hivemind --since @$t0 -o short-iso-precise --no-pager" >"$SOAK_DIR/journal-$name-${NAMES[$i]}.txt" 2>/dev/null || true
	done
	ev event "$name server-journal lost=$(grep -c 'node lost' "$SOAK_DIR/journal-$name-$SERVER.txt") errors=$(grep -c 'level=ERROR' "$SOAK_DIR/journal-$name-$SERVER.txt") $(with_names "$(grep -E 'start grace|has reported|waiting=' "$SOAK_DIR/journal-$name-$SERVER.txt" | cut -c1-500 | tr '\n' ';')")"
	ev event "$name workers-kept $(worker_kept "$pre")"
	ev event "$name ids $(ids_check)"
	end_scenario "$name"
}

# router_lines NAME FILE: in a journal of NAME, the router's lines about the
# table, counted: "reading the routing table; serving the last one" and
# "reading the routing table again".
router_lines() {
	printf '%s last=%s again=%s' "$1" "$(grep -c 'serving the last one' "$2")" "$(grep -c 'reading the routing table again' "$2")"
}

# reboot_cutoff W: a worker's machine restarted while the other workers'
# routers serve their last table (the seventh soak). The server's hivemind
# is stopped (its machine and containers stay up), so no router can read
# the table; the other workers are cut off from its cluster port; the
# operator powers W off when the log says so, and starts its machine; once
# W's port 22 opens the server's hivemind is started again, so its start
# grace begins. W reaches the server, its agent starts its routed containers
# again, and the other workers' routers, still cut off, send requests to
# W's replicas at the addresses of their table from before W went down.
# OBSERVE seconds (45) after every routed container of W runs again, or
# HEAL_BEFORE seconds (20) before the start grace ends, whichever is first,
# the other workers are healed. Logged: W's routed containers and their
# ports at each change, the table at each change (every 200 ms) from the
# server's start, the nodes, and at the end each node's journal.
reboot_cutoff() {
	local W=${1:?worker} name=${HOLD_NAME:-reboot-cutoff} grace=${START_GRACE:-120} observe=${OBSERVE:-45} before=${HEAL_BEFORE:-20}
	local pre cur t0 t now d v i n s api=down w22=down srv=stopped up_at=0 back_at=0 heal_at=0 end wb nb lastn="" lastw="" ready="" conv="" dupl="" others=() rw=""
	pre=$SOAK_DIR/pre-$name.tsv cur=$SOAK_DIR/measure-$name.tsv
	[ "$W" != "$SERVER" ] || die "$name: W must be a worker"
	for ((i = 1; i < ${#NAMES[@]}; i++)); do [ "${NAMES[$i]}" = "$W" ] || others+=("${NAMES[$i]}"); done
	begin "$name" "$W"
	t0=$(date -u +%s)
	wb=$(routed_ports "$W") || die "$name: no routed containers read on $W"
	nb=$(wc -w <<<"$wb")
	ev event "$name $W routed-before n=$nb $wb"
	ev event "$name routes-before $(hm "$SERVER" routes 2>&1 | tr -s ' \n' ' ')"
	SSH_TIMEOUT=60 on "$SERVER" "sudo systemctl stop hivemind" || die "$name: stop hivemind on $SERVER"
	ev event "$name $SERVER fault stop hivemind"
	for n in "${others[@]}"; do
		on "$n" "sudo iptables -I OUTPUT -p tcp -d $SERVER_IP --dport 4484 -j DROP" || die "$name: iptables on $n"
		ev event "$name $n fault drop to $SERVER_IP:4484"
	done
	note "$name: power $W off now"
	wait_for 600 port22_down "$W" || die "$name: $W did not go down"
	ev event "$name $W port22-down"
	forget_host "$W"
	note "$name: start $W's machine now"
	end=$(($(date +%s) + 900))
	while [ "$(date +%s)" -lt "$end" ]; do
		t=$(now_ms)
		now=$(date +%s)
		if port22 "$W"; then v=up; else v=down; fi
		[ "$v" = "$w22" ] || {
			ev event "$name $W port22-$v"
			w22=$v
		}
		# The server's hivemind starts once W's machine is booting, so its
		# start grace, which the other workers' cut-off needs, does not run
		# while the machine stops and starts.
		if [ "$srv" = stopped ] && [ "$w22" = up ]; then
			SSH_TIMEOUT=60 on "$SERVER" "sudo systemctl start hivemind" || die "$name: start hivemind on $SERVER"
			ev event "$name $SERVER fault start hivemind"
			srv=started
		fi
		if SSH_CT=1 SSH_TIMEOUT=4 on "$SERVER" "curl -fsS -m 2 http://127.0.0.1:4483/healthz" >/dev/null 2>&1; then v=up; else v=down; fi
		[ "$v" = "$api" ] || {
			ev event "$name api-$v"
			api=$v
			if [ "$v" = up ] && [ "$up_at" = 0 ]; then
				up_at=$now
				SSH_TIMEOUT=$((grace + 120)) routes_watch $((grace + 100)) &
				rw=$!
			fi
		}
		if [ "$api" = up ]; then
			v=$(with_names "$(hm "$SERVER" nodes --json 2>/dev/null | jq -r '[.[] | select(.server | not) | "\(.id)=\(.state)\(if .start_held then "+held" else "" end)\(if .start_hold_ended then "+hold-ended" else "" end)"] | join(",")')")
			[ "$v" = "$lastn" ] || {
				ev event "$name nodes $v"
				lastn=$v
			}
		fi
		if [ "$w22" = up ]; then
			v=$(SSH_CT=1 SSH_TIMEOUT=4 routed_ports "$W" 2>/dev/null) || v=fail
			[ "$v" = "$lastw" ] || {
				ev event "$name $W routed $v"
				lastw=$v
			}
			# back: as many routed containers as before, all running
			if [ "$back_at" = 0 ] && [ "$v" != fail ] && [ "$(wc -w <<<"$v")" = "$nb" ] && ! grep -qE '\|(exited|created|restarting|dead|paused)\|' <<<"$v"; then
				back_at=$now
				ev event "$name $W routed-back $(if [ "$(ports_key "$v")" = "$(ports_key "$wb")" ]; then echo same-ports; else echo "ports-changed before=$(ports_key "$wb") after=$(ports_key "$v")"; fi)"
			fi
		fi
		if [ "$heal_at" = 0 ] && [ "$up_at" != 0 ] && { { [ "$back_at" != 0 ] && [ "$now" -ge $((back_at + observe)) ]; } || [ "$now" -ge $((up_at + grace - before)) ]; }; then
			for n in "${others[@]}"; do
				on "$n" "sudo iptables -D OUTPUT -p tcp -d $SERVER_IP --dport 4484 -j DROP" || ev event "$name $n heal-failed"
			done
			ev event "$name heal: ${others[*]} reach $SERVER_IP:4484 again, $((now - up_at)) s after the API answered$([ "$back_at" = 0 ] && echo ", before $W's containers were back")"
			heal_at=$now
		fi
		if [ "$heal_at" != 0 ]; then
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
					ev event "$name converged-$v $(running "$cur")"
					conv=$v
				}
			fi
			d=$(dups "$cur" | tr '\n' ';')
			[ "$d" = "$dupl" ] || {
				ev event "$name dups ${d:-none}"
				dupl=$d
			}
			if { [ "$conv" = yes ] && [ "$(wc -w <<<"$ready")" -eq $((${#NAMES[@]} - 1)) ] && [ "$now" -ge $((heal_at + 3)) ]; } || [ "$now" -ge $((heal_at + 300)) ]; then
				break
			fi
		fi
		d=$((1000 - ($(now_ms) - t)))
		[ "$d" -le 0 ] || sleep "$((d / 1000)).$(printf '%03d' $((d % 1000)))"
	done
	[ -z "$rw" ] || kill "$rw" 2>/dev/null
	on "$SERVER" "sudo journalctl -u hivemind --since @$t0 -o short-iso-precise --no-pager" >"$SOAK_DIR/journal-$name-$SERVER.txt" 2>/dev/null || true
	v=""
	for ((i = 1; i < ${#NAMES[@]}; i++)); do
		n=${NAMES[$i]}
		on "$n" "sudo journalctl -u hivemind --since @$t0 -o short-iso-precise --no-pager" >"$SOAK_DIR/journal-$name-$n.txt" 2>/dev/null || true
		v+="$(router_lines "$n" "$SOAK_DIR/journal-$name-$n.txt") "
	done
	ev event "$name router-lines $v"
	ev event "$name server-journal lost=$(grep -c 'node lost' "$SOAK_DIR/journal-$name-$SERVER.txt") errors=$(grep -c 'level=ERROR' "$SOAK_DIR/journal-$name-$SERVER.txt") $(with_names "$(grep -E 'start grace|has reported|waiting=' "$SOAK_DIR/journal-$name-$SERVER.txt" | cut -c1-500 | tr '\n' ';')")"
	ev event "$name $W journal restarted=$(grep -c 'restarted exited container' "$SOAK_DIR/journal-$name-$W.txt") created=$(grep -c 'created container' "$SOAK_DIR/journal-$name-$W.txt") removed=$(grep -c 'removed container' "$SOAK_DIR/journal-$name-$W.txt")"
	ev event "$name workers-kept $(worker_kept "$pre")"
	ev event "$name ids $(ids_check)"
	end_scenario "$name"
}

# The sampler of rollout-all, run on the server: hivemind status --json
# every 0.5 s until FILE.stop exists. One line per sample in FILE:
#   <Unix s.ms> sample <apps> [APP:desired=D,running=R,ready=Y,gen=G ...]
# listing each app whose running or ready count is below its desired count;
# the whole status of such a sample goes to FILE.below.
read -r -d '' REMOTE_SAMPLER <<'EOF' || true
f=$1
rm -f "$f" "$f.stop" "$f.below"
while [ ! -e "$f.stop" ]; do
	t=$(date -u +%s.%3N)
	if o=$(sudo /usr/local/bin/hivemind status --json 2>&1); then
		l=$(printf '%s\n' "$o" | jq -r --arg t "$t" '"\($t) sample \(length)" + ([.[] | select(.running < .desired or .ready < .desired) | " \(.name):desired=\(.desired),running=\(.running),ready=\(.ready),gen=\(.generation)"] | join(""))' 2>&1)
		echo "$l" >>"$f"
		case "$l" in *"sample "*" "*) printf '== %s\n%s\n' "$t" "$(printf '%s' "$o" | jq -c '.[] | {name, desired, running, ready, generation, rollout, replicas: [.replicas[] | {index, node_id, state, ready, generation, desired}]}')" >>"$f.below" ;; esac
	else
		echo "$t error $(printf '%s' "$o" | head -c 200 | tr '\n' ' ')" >>"$f"
	fi
	sleep 0.5
done
EOF

# rollout_all [ROUNDS]: ROUNDS (3) rolling updates of every app back to
# back (the seventh soak; fixes11's consistent read): each round PUTs every
# app at once with a new ROLL in its environment (lib.sh), the image apps
# at version 2 in odd rounds and 1 in even ones, and the compose project
# shop again; the next round starts as soon as no app has a rollout in
# progress. The sampler on the server reads hivemind status --json every
# 0.5 s from before the first PUT until the cluster has converged after the
# last round (the old copies gone), and the scenario logs the samples in
# which an app was below its count. kv has a volume, so its replica moves in
# place and is down while its new container starts (docs/concepts.md); the
# others surge.
rollout_all() {
	local rounds=${1:-3} name=rollout-all r a v t end st s busy seen pids f=/var/tmp/soak-samples.txt t0 tr
	local apps="www api spread kv proot papi grpc"
	begin "$name" "$SERVER"
	on "$SERVER" "cat >/var/tmp/soak-sampler.sh" <<<"$REMOTE_SAMPLER" || die "$name: sampler to $SERVER"
	on "$SERVER" "setsid nohup bash /var/tmp/soak-sampler.sh $f >/dev/null 2>&1 </dev/null &" || die "$name: sampler on $SERVER"
	sleep 2
	t0=$(date +%s)
	for ((r = 1; r <= rounds; r++)); do
		v=$((r % 2 == 1 ? 2 : 1))
		export ROLL="r$r-$t0"
		pids=()
		for a in $apps; do
			hm "$SERVER" "deploy -f -" <<<"$(app_spec "$a" "$v")" >/dev/null &
			pids+=($!)
		done
		hm "$SERVER" "compose deploy -" < <(shop_compose) >/dev/null &
		pids+=($!)
		for a in "${pids[@]}"; do wait "$a" || die "$name: a PUT of round $r failed"; done
		ev event "$name round $r fault deploy ROLL=$ROLL version=$v"
		tr=$(date +%s)
		end=$((tr + 600))
		s="" seen=0
		while [ "$(date +%s)" -lt "$end" ]; do
			t=$(now_ms)
			st=$(hm "$SERVER" status --json 2>/dev/null) || st=""
			busy=$(jq -r '[.[] | select(.rollout.state? == "in_progress")] | length' <<<"$st" 2>/dev/null)
			[ "${busy:-0}" = 0 ] || seen=1
			v=$(jq -r '[.[] | select(.rollout != null) | "\(.name):\(.rollout.state // "-")"] | join(",")' <<<"$st" 2>/dev/null)
			[ "$v" = "$s" ] || {
				ev event "$name round $r rollouts ${v:-none}"
				s=$v
			}
			# done: no rollout in progress (a paused one is logged above), once one
			# was seen, or 10 s after the PUTs if none ever was
			if [ -n "$st" ] && [ "$busy" = 0 ] && { [ "$seen" = 1 ] || [ "$(date +%s)" -ge $((tr + 10)) ]; }; then
				ev event "$name round $r done $(($(date +%s) - tr)) s"
				break
			fi
			d=$((1000 - ($(now_ms) - t)))
			[ "$d" -le 0 ] || sleep "$((d / 1000)).$(printf '%03d' $((d % 1000)))"
		done
		[ "$(date +%s)" -lt "$end" ] || ev event "$name round $r timeout"
	done
	unset ROLL
	wait_for 300 converged_now && ev event "$name converged-yes"
	on "$SERVER" "touch $f.stop"
	sleep 2
	on "$SERVER" "cat $f" >"$SOAK_DIR/rollout-all-samples.txt" 2>/dev/null || true
	on "$SERVER" "cat $f.below 2>/dev/null" >"$SOAK_DIR/rollout-all-below.txt" 2>/dev/null || true
	f=$SOAK_DIR/rollout-all-samples.txt
	ev event "$name sampler samples=$(grep -c ' sample ' "$f") errors=$(grep -c ' error ' "$f") seconds=$(($(date +%s) - t0)) below=$(grep -cE ' sample [0-9]+ ' "$f") below_not_kv=$(grep -E ' sample [0-9]+ ' "$f" | grep -cE ' (www|api|spread|proot|papi|grpc|front|cache):') first=$(grep -m1 -E ' sample [0-9]+ ' "$f" | cut -c1-300)"
	end_scenario "$name"
}

cmd=${1:-}
shift || true
case "$cmd" in
fault) ev event "$1 $2 fault ${*:3}" ;;
ports) ports_log "${*:-now}" ;;
reboot-cutoff) reboot_cutoff "$@" ;;
rollout-all) rollout_all "$@" ;;
rollout) rollout "$@" ;;
old-back) old_back "$@" ;;
cert-restart) cert_restart "$@" ;;
ca-watch) ca_watch ;;
ca-views) ca_views "${1:-ca}" "${2:-now}" ;;
ca-text) ca_text "${1:-ca}" "${2:-now}" ;;
ca-rotate) ca_rotate "$@" ;;
ca-offline) ca_offline "$@" ;;
ca-abort) ca_abort ;;
ca-restore-begin) ca_restore_begin ;;
ca-finish) ca_finish "$@" ;;
start-hold) start_hold ;;
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
leader-kill) leader_kill ;;
leader-partition) leader_partition "$@" ;;
quorum-loss) quorum_loss ;;
service-restart) service_restart ;;
steady) steady "$@" ;;
servers) ev servers "${1:-now} $(servers_line)" ;;
ha-selftest) ha_selftest ;;
*)
	sed -n '2,48p' "$0"
	exit 2
	;;
esac
