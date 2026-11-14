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
#   scenario.sh kv-check NAME                 read the kv value where kv runs
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
	awk -F'\t' -v s="$SERVER" '
		FILENAME == ARGV[1] { if ($1 == "C" && $4 == "running" && $2 != s) pre[$2 "|" $3 "|" $5] = $6 "|" $7; next }
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

kv_check() {
	local st id n v
	st=$(hm "$SERVER" status --json) || die "no status"
	id=$(jq -r '.[] | select(.name == "kv") | .replicas[0].node_id // ""' <<<"$st")
	n=$(name_of_id "$id")
	v=$(kv_get "$n" 2>/dev/null) || v=""
	if [ "$v" = "$(cat "$SOAK_DIR/kv-value")" ]; then ev event "${1:-kv} kv-value-match node=$n"; else ev event "${1:-kv} kv-value-mismatch node=$n got=${v:-none}"; fi
}

cmd=${1:-}
shift || true
case "$cmd" in
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
kv-check) kv_check "$@" ;;
end) end_scenario "$@" ;;
*)
	sed -n '2,27p' "$0"
	exit 2
	;;
esac
