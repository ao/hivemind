# shellcheck shell=bash
# The scenarios of the soak of several servers (docs/ha.md), sourced by
# scenario.sh, which runs them:
#
#   scenario.sh leader-kill          the leader's machine powered off; a new
#                                    leader; the old one's machine started
#                                    again, back as a follower
#   scenario.sh leader-partition [SECS]  the leader cut off from the other
#                                    servers (TCP 4484 and 4486, both ways)
#                                    for SECS (60) after a new leader is
#                                    elected; healed
#   scenario.sh quorum-loss          a majority of the servers powered off (two
#                                    of three: the followers, or $QL_OFF); no
#                                    leader; one back: a leader; then the rest
#   scenario.sh service-restart [follower]  the leader's (or a follower's)
#                                    hivemind restarted (with one
#                                    server, the server's)
#   scenario.sh rollout-leader       a rolling update of every app, the
#                                    leader's hivemind restarted during it
#   scenario.sh router-noleader [GAP]  the leader's hivemind restarted while
#                                    no server leads (the two others paused
#                                    with SIGSTOP); GAP s (1) after its start
#                                    one of them resumed, then the other
#   scenario.sh steady [SECS]        the steady state (3600 s), with the
#                                    write-load deleting as it goes
#   scenario.sh servers [LABEL]      each server's view, one log line
#   scenario.sh ha-selftest          the parsers on canned replies (no machines)
#
# Set-up for them (setup.sh): SOAK_SERVERS=3 with 7 machines, 3 servers and 3
# workers in $NODES_FILE and the outside machine; SOAK_ROUTES=1 SOAK_PATHS=1
# SOAK_GRPC=1 as in the seventh soak; SOAK_API_TLS=1 (the default with more
# than one server). See the end of this comment for the commands.
#
# Each scenario: wait for convergence; start the write-load (wload.sh,
# WLOAD=1, the default with an outside machine) and let it run $WL_WARM s
# (20); the fault; once a second the observations below, each logged as an
# "event" line at its first change; the return; then the write-load stopped,
# checked against every server's store, and its apps deleted (WL_CLEAN=1).
#   - port 22 of each server;
#   - each server's view, hivemind servers --json on it: the leader it
#     names, every server's role, and the note it gives without a leader
#     ("view S leader=X roles=A:leader,B:follower,... note=...");
#   - the leader a majority of the servers names ("leader X", or "leader
#     none"), and the first new one after the fault ("new-leader X
#     ms_after_fault=N");
#   - through any server that answers: each node's state ("node N STATE"),
#     convergence (the fault suite's check, and the same allowing a volume
#     replica that waits for its node), every replica running, and any
#     (app, replica) running on two nodes ("dups");
#   - in leader-partition and quorum-loss, GET /v1/nodes on the cut-off or
#     surviving servers' own APIs, with the admin token ("api S code=503
#     err=<the body>"): the documented 503 and its reason.
# The outside machine meanwhile runs the HTTP and gRPC clients through every
# router (outside.sh), and wload.sh's probe: a read every 0.5 s through the
# CLI with every server in HIVEMIND_SERVER, and each server's API directly.
# ha-report.sh computes the numbers.
#
# HA_FAULT=power (the default) powers a machine off from inside (sysrq o, as
# soak-ec2.sh poweroff does), and asks the operator to start it again with a
# "note" line: "start NAME's machine now" (SOAK_PREFIX=... soak-ec2.sh start
# NAME); the harness waits for its port 22. HA_FAULT=service kills the
# server's hivemind instead (SIGKILL, kept stopped) and starts it again
# itself; the machine and its containers stay up.
#
# The full run, on the control machine (the build host), once the HA build is
# in $SOAK_DIR/hivemind:
#
#   export SOAK_DIR=$PWD/.scratch/run SOAK_SERVERS=3 SOAK_ROUTES=1 SOAK_PATHS=1 SOAK_GRPC=1
#   printf 'n-1 10.0.0.1\nn-2 10.0.0.2\nn-3 10.0.0.3\nn-4 10.0.0.4\nn-5 10.0.0.5\nn-6 10.0.0.6\n' >"$SOAK_DIR/nodes"  # 3 servers first
#   printf 'n-7 10.0.0.7\n' >"$SOAK_DIR/outside"
#   scripts/soak/outside.sh setup
#   scripts/soak/setup.sh                      # checks 4483/4486 first; joins 2 servers, 3 workers
#   scripts/soak/outside.sh client-start
#   scripts/soak/wload.sh probe-start
#   scripts/soak/scenario.sh probe-start
#   scripts/soak/scenario.sh watch-start
#   scripts/soak/scenario.sh leader-kill       # start the machine when the log says so
#   scripts/soak/scenario.sh leader-partition
#   scripts/soak/scenario.sh quorum-loss       # start each machine when the log says so
#   scripts/soak/scenario.sh service-restart
#   scripts/soak/scenario.sh watch-stop
#   scripts/soak/scenario.sh steady 3600
#   scripts/soak/wload.sh probe-stop; scripts/soak/wload.sh fetch; scripts/soak/outside.sh fetch
#   scripts/soak/ha-report.sh "$SOAK_DIR/soak.log" "$SOAK_DIR/wl"
#   scripts/soak/client-report.sh "$SOAK_DIR/soak.log" "$SOAK_DIR/client"

HA_FAULT=${HA_FAULT:-power}
WL_WARM=${WL_WARM:-20}
WL_CLEAN=${WL_CLEAN:-1}
if [ -z "${WLOAD:-}" ]; then
	if [ "$SOAK_API_TLS" = 1 ] && [ -n "${WL_CLIENT:-$OUT_NAME}" ]; then WLOAD=1; else WLOAD=0; fi
fi
HA_T0=0
# the write-load's tag of the scenario that runs: its name, or NAME-r2, ... for a rerun
WL_TAG=""

# majority: how many servers make a majority.
majority() { echo $((SOAK_SERVERS / 2 + 1)); }

# role_in VIEW NAME: NAME's role in a view's "roles=" list, or empty.
role_in() {
	local r
	r=$(sed -n 's/.*roles=\([^ ]*\).*/\1/p' <<<"$1" | tr ',' '\n' | grep "^$2:" | head -n 1)
	echo "${r#*:}"
}
# leader_of_view VIEW: the leader a view names, "none" when it names none,
# "unknown" when there is no view (the server did not answer).
leader_of_view() {
	local l
	l=$(sed -n 's/^leader=\([^ ]*\).*/\1/p' <<<"$1")
	echo "${l:-unknown}"
}

# api_answer NAME: what NAME's own API answers to GET /v1/nodes with the
# admin token: "code=C" and, for a code other than 200, "err=<body>".
read -r -d '' REMOTE_API_ANSWER <<'EOF' || true
u=$1 tf=$2 ca=$3
co=()
[ "$ca" = - ] || co=(--cacert "$ca")
b=$(mktemp)
c=$(printf 'header = "Authorization: Bearer %s"\n' "$(cat "$tf")" | curl -s -K - "${co[@]}" -m 3 -o "$b" -w '%{http_code}' "$u/v1/nodes")
if [ "$c" = 200 ]; then echo "code=200"; else echo "code=$c err=$(head -c 300 "$b" | tr -s ' \t\n' '___')"; fi
rm -f "$b"
EOF
api_answer() {
	local v
	if port22 "$1" && v=$(SSH_CT=1 SSH_TIMEOUT=6 on "$1" "sudo bash -s $(api_args "$1")" <<<"$REMOTE_API_ANSWER" 2>/dev/null); then
		echo "$v"
	else
		echo "code=ssh-fail"
	fi
}

# The observations' last values, for logging changes only.
declare -A HP22 HV HNODE
HLEAD="" HAPI="" HCONV="" HHELD="" HRUN="" HDUP="" HSURGE=""

# ha_observe NAME: one round of observations (above), about a second.
ha_observe() {
	local name=$1 cur=$SOAK_DIR/measure-$1.tsv i s v n id st pids=() maj l c
	declare -A cnt
	for i in "${!SRV_NAMES[@]}"; do
		s=${SRV_NAMES[$i]}
		if port22 "$s"; then v=up; else v=down; fi
		[ "${HP22[$s]:-}" = "$v" ] || {
			ev event "$name $s port22-$v"
			HP22[$s]=$v
		}
		rm -f "$SOAK_DIR/view.$i"
		if [ "$SOAK_SERVERS" -gt 1 ] && [ "$v" = up ]; then
			(
				j=$(SSH_TIMEOUT=4 servers_json "$s") && w=$(view_of "$j") && [ -n "$w" ] || w=api=fail
				echo "$w" >"$SOAK_DIR/view.$i"
			) &
			pids+=($!)
		elif [ "$SOAK_SERVERS" -gt 1 ]; then
			echo "down" >"$SOAK_DIR/view.$i"
		fi
	done
	collect "$cur" nodes
	[ ${#pids[@]} -eq 0 ] || wait "${pids[@]}"
	if [ "$SOAK_SERVERS" -gt 1 ]; then
		for i in "${!SRV_NAMES[@]}"; do
			s=${SRV_NAMES[$i]}
			v=$(cat "$SOAK_DIR/view.$i" 2>/dev/null || echo api=fail)
			[ "${HV[$s]:-}" = "$v" ] || {
				ev event "$name view $s $v"
				HV[$s]=$v
			}
			l=$(leader_of_view "$v")
			case "$l" in none | unknown) ;; *) cnt[$l]=$((${cnt[$l]:-0} + 1)) ;; esac
		done
		maj=$(majority) v=none
		for l in "${!cnt[@]}"; do [ "${cnt[$l]}" -ge "$maj" ] && v=$l; done
		[ "$v" = "$HLEAD" ] || {
			ev event "$name leader $v"
			HLEAD=$v
		}
	fi
	if api_ok "$cur"; then v=ok; else v=fail; fi
	[ "$v" = "$HAPI" ] || {
		ev event "$name api-$v"
		HAPI=$v
	}
	if [ "$HAPI" = ok ]; then
		while read -r id st; do
			n=$(name_of_id "$id")
			n=${n:-$id}
			[ "${HNODE[$n]:-}" = "$st" ] || {
				ev event "$name node $n $st"
				HNODE[$n]=$st
			}
		done < <(awk -F'\t' '$1 == "L" { print $2, $3 }' "$cur")
		if check_converged "$cur" 0 >/dev/null; then c=yes; else c=no; fi
		[ "$c" = "$HCONV" ] || {
			ev event "$name converged-$c"
			HCONV=$c
		}
		if check_converged "$cur" 1 >/dev/null; then c=yes; else c=no; fi
		[ "$c" = "$HHELD" ] || {
			ev event "$name converged-held-$c"
			HHELD=$c
		}
		v=$(running "$cur")
		if [ "${v%/*}" = "${v#*/}" ]; then c=yes; else c=no; fi
		[ "$c" = "$HRUN" ] || {
			ev event "$name all-running-$c $v"
			HRUN=$c
		}
	fi
	# the same copy (app, replica, generation) running on two nodes; and, as
	# "surge", any two copies of a replica, old and new generations included,
	# which a rolling update runs side by side by design
	v=$(dups_gen "$cur" | tr '\n' ';')
	[ "$v" = "$HDUP" ] || {
		ev event "$name dups ${v:-none}"
		HDUP=$v
	}
	v=$(dups "$cur" | tr '\n' ';')
	[ "$v" = "$HSURGE" ] || {
		ev event "$name surge ${v:-none}"
		HSURGE=$v
	}
}

# ha_tick T_MS: sleep the rest of the second that began at T_MS.
ha_tick() {
	local d=$((1000 - ($(now_ms) - $1)))
	[ "$d" -le 0 ] || sleep "$((d / 1000)).$(printf '%03d' $((d % 1000)))"
}

ha_reset() {
	HP22=() HV=() HNODE=()
	HLEAD="" HAPI="" HCONV="" HHELD="" HRUN="" HDUP="" HSURGE=""
}

# ha_begin NAME: converged; the state before, the servers' views, the
# routed containers' ports; the write-load started and warmed up.
ha_begin() {
	local name=$1 pre=$SOAK_DIR/pre-$1.tsv
	ha_reset
	HA_T0=$(date +%s)
	ev scenario "$name begin servers=${SRV_NAMES[*]} fault=$HA_FAULT"
	wait_for 600 converged_now || die "$name: not converged before the fault: $(check_converged "$SOAK_DIR/now.tsv" | tr '\n' ';')"
	snapshot "$pre" || die "$name: no status"
	ev scenario "$name ready servers $(servers_line)"
	ev scenario "$name placement $(awk -F'\t' 'FILENAME == ARGV[1] { split($0, x, " "); nm[x[1]] = x[2]; next } $1 == "R" && index($2, "wl-") != 1 { printf "%s|%s@%s ", $2, $3, nm[$4] }' "$IDS_FILE" "$pre")"
	ports_log "$name before"
	if [ "$WLOAD" = 1 ]; then
		WL_TAG=$("$HERE/wload.sh" tag "$name") && "$HERE/wload.sh" start "$WL_TAG" || die "$name: the write-load did not start"
		sleep "$WL_WARM"
	fi
}

# ha_end NAME: the write-load stopped, checked and cleaned; the servers'
# journals since the scenario began (journal-TAG-SERVER.txt, the run's tag, so
# that a scenario run twice keeps both); the end line.
ha_end() {
	local name=$1 s f wl=none
	if [ "$WLOAD" = 1 ]; then
		"$HERE/wload.sh" stop || true
		if "$HERE/wload.sh" check "$WL_TAG"; then wl=pass; else wl=FAIL; fi
		if [ "$WL_CLEAN" = 1 ]; then "$HERE/wload.sh" clean "$WL_TAG" || wl="$wl,clean-FAIL"; fi
	fi
	for s in "${SRV_NAMES[@]}"; do
		f=$SOAK_DIR/journal-${WL_TAG:-$name}-$s.txt
		SSH_TIMEOUT=60 on "$s" "sudo journalctl -u hivemind --since @$HA_T0 -o short-iso-precise --no-pager" >"$f" 2>/dev/null || true
		ev event "$name journal $s lines=$(wc -l <"$f") errors=$(grep -c 'level=ERROR' "$f" || true) warns=$(grep -c 'level=WARN' "$f" || true) lost=$(grep -c 'node lost' "$f" || true) leader_lines=$(grep -ci 'leader' "$f" || true)"
	done
	ev event "$name servers $(servers_line)"
	end_scenario "$name"
	ev event "$name write-check $wl"
}

# ha_fault NAME T: T's machine powered off (HA_FAULT=power), or its
# hivemind killed and kept stopped (service).
ha_fault() {
	local name=$1 T=$2
	if [ "$HA_FAULT" = service ]; then
		ev event "$name $T fault kill-service"
		SSH_TIMEOUT=60 on "$T" "sudo systemctl kill -s KILL hivemind.service; sudo systemctl stop hivemind.service" || ev event "$name $T fault-failed"
	else
		ev event "$name $T fault poweroff"
		SSH_TIMEOUT=6 on "$T" "sudo sh -c 'echo 1 > /proc/sys/kernel/sysrq; echo o > /proc/sysrq-trigger'" >/dev/null 2>&1 || true
		forget_host "$T"
	fi
}

# ha_back NAME T: start T again: ask the operator to start its machine, or
# start its hivemind.
ha_back() {
	local name=$1 T=$2
	if [ "$HA_FAULT" = service ]; then
		ev event "$name $T fault start-service"
		SSH_TIMEOUT=60 on "$T" "sudo systemctl start hivemind.service" || ev event "$name $T start-failed"
	else
		ev event "$name $T return-asked"
		note "$name: start $T's machine now (SOAK_PREFIX=<prefix> soak-ec2.sh start $T); then: scenario.sh fault $name $T start"
	fi
}

# rejoined NAME T: T is a follower in the view of the leader a majority
# names, and T's own view names that leader.
rejoined() {
	[ "$HLEAD" != none ] && [ -n "$HLEAD" ] && [ "$HLEAD" != "$2" ] &&
		[ "$(role_in "${HV[$HLEAD]:-}" "$2")" = follower ] && [ "$(leader_of_view "${HV[$2]:-}")" = "$HLEAD" ]
}

# kept_on PRE NAMES...: the containers running in PRE on NAMES still run
# (same ID and start time), and no new one runs: "kept", or what changed.
kept_on() {
	local pre=$1 cur=$SOAK_DIR/kept.tsv n
	shift
	: >"$cur"
	for n in "$@"; do
		SSH_CT=1 container_rows_of "$n" >>"$cur" 2>/dev/null || printf 'U\t%s\n' "$n" >>"$cur"
	done
	awk -F'\t' -v w=" $* " '
		FILENAME == ARGV[1] { if ($1 == "C" && $4 == "running" && index(w, " " $2 " ")) pre[$2 "|" $3 "|" $5] = $6 "|" $7; next }
		$1 == "U" { out = out "unreachable:" $2 ","; next }
		$1 == "C" && $4 == "running" { k = $2 "|" $3 "|" $5; now[k] = 1; if (!(k in pre)) out = out "new:" $2 ":" $3 ":" $6 "|" $7 "," }
		END { for (k in pre) if (!(k in now)) { split(k, a, "|"); out = out "gone:" a[1] ":" a[2] ":" pre[k] "," } print (out == "" ? "kept" : out) }' "$pre" "$cur"
}

need_servers() {
	[ "$SOAK_SERVERS" -ge 3 ] || die "$1 needs a cluster of 3 or more servers (SOAK_SERVERS=3)"
}

# leader_kill: the leader's machine powered off. Measures the time to a new
# leader, the cluster converging without it (its node waits for the new
# leader's start grace, then is lost and its replicas run elsewhere), and
# once the operator has started it again, its return as a follower.
leader_kill() {
	local name=leader-kill L tf t now newl="" asked=0 back=0 done_at=0 end down=${LK_DOWN:-600}
	need_servers "$name"
	ha_begin "$name"
	L=$(leader) || die "$name: no leader"
	ev event "$name target $L"
	tf=$(now_ms)
	ha_fault "$name" "$L"
	end=$(($(date +%s) + ${LK_SECS:-2400}))
	while [ "$(date +%s)" -lt "$end" ]; do
		t=$(now_ms)
		now=$(date +%s)
		ha_observe "$name"
		if [ -z "$newl" ] && [ "$HLEAD" != none ] && [ -n "$HLEAD" ] && [ "$HLEAD" != "$L" ]; then
			newl=$HLEAD
			ev event "$name new-leader $newl ms_after_fault=$(($(now_ms) - tf))"
		fi
		# ask for the return once a new leader leads and the cluster has gone on
		# without L: its node lost (the grace) and converged, or after LK_DOWN s
		if [ "$asked" = 0 ] && [ -n "$newl" ] &&
			{ { [ "${HNODE[$L]:-}" = lost ] && [ "$HHELD" = yes ]; } || [ "$now" -ge $((tf / 1000 + down)) ]; }; then
			ev event "$name down-phase-done node=${HNODE[$L]:-?} converged_held=$HHELD"
			ha_back "$name" "$L"
			asked=1
		fi
		if [ "$asked" = 1 ] && [ "$back" = 0 ] && rejoined "$name" "$L"; then
			ev event "$name $L rejoined-follower leader=$HLEAD"
			back=1
		fi
		if [ "$back" = 1 ] && [ "$HCONV" = yes ] && [ "${HNODE[$L]:-}" = ready ]; then
			[ "$done_at" != 0 ] || done_at=$now
			[ "$now" -lt $((done_at + 10)) ] || break
		fi
		ha_tick "$t"
	done
	[ "$(date +%s)" -lt "$end" ] || ev event "$name measure-timeout new_leader=${newl:-none} asked=$asked back=$back converged=$HCONV"
	ha_end "$name"
}

# leader_partition [SECS]: the leader cut off from the other servers, on
# the node port (4484) and the server port (4486), both ways, by iptables
# on the leader; the clients still reach its router and its API. It must
# step down, the others elect a leader, and its API answer 503. Healed SECS
# (60) after the new leader; it must come back as a follower.
leader_partition() {
	local name=leader-partition secs=${1:-60} L tf t now newl="" newl_at=0 down=0 healed=0 back=0 done_at=0 end o ip p rules="" v lastv=""
	need_servers "$name"
	ha_begin "$name"
	L=$(leader) || die "$name: no leader"
	ev event "$name target $L"
	for o in "${SRV_NAMES[@]}"; do
		[ "$o" = "$L" ] && continue
		ip=$(ip_of "$o")
		for p in 4484 4486; do
			rules+="iptables -I INPUT -s $ip -p tcp --dport $p -j DROP && iptables -I OUTPUT -d $ip -p tcp --dport $p -j DROP && "
		done
	done
	tf=$(now_ms)
	on "$L" "sudo sh -c '${rules}true'" || die "$name: iptables on $L"
	ev event "$name $L fault partition from the other servers, tcp 4484 and 4486 both ways"
	end=$(($(date +%s) + ${LP_SECS:-1200}))
	while [ "$(date +%s)" -lt "$end" ]; do
		t=$(now_ms)
		now=$(date +%s)
		ha_observe "$name"
		v=$(api_answer "$L")
		[ "$v" = "$lastv" ] || {
			ev event "$name api $L $v"
			lastv=$v
		}
		v=$(leader_of_view "${HV[$L]:-}")
		if [ "$down" = 0 ] && [ "$v" != "$L" ] && [ "$v" != unknown ]; then
			ev event "$name $L stepped-down ms_after_fault=$(($(now_ms) - tf)) view=${HV[$L]:-}"
			down=1
		fi
		if [ -z "$newl" ] && [ "$HLEAD" != none ] && [ -n "$HLEAD" ] && [ "$HLEAD" != "$L" ]; then
			newl=$HLEAD newl_at=$now
			ev event "$name new-leader $newl ms_after_fault=$(($(now_ms) - tf))"
		fi
		if [ "$healed" = 0 ] && { { [ -n "$newl" ] && [ "$now" -ge $((newl_at + secs)) ]; } || [ "$now" -ge $((tf / 1000 + 300)) ]; }; then
			on "$L" "sudo sh -c '$(sed 's/ -I / -D /g' <<<"$rules")true'" || ev event "$name $L heal-failed"
			ev event "$name $L heal"
			healed=1
		fi
		if [ "$healed" = 1 ] && [ "$back" = 0 ] && rejoined "$name" "$L"; then
			ev event "$name $L rejoined-follower leader=$HLEAD"
			back=1
		fi
		if [ "$back" = 1 ] && [ "$HCONV" = yes ]; then
			[ "$done_at" != 0 ] || done_at=$now
			[ "$now" -lt $((done_at + 10)) ] || break
		fi
		ha_tick "$t"
	done
	[ "$healed" = 1 ] || on "$L" "sudo sh -c '$(sed 's/ -I / -D /g' <<<"$rules")true'" || true
	[ "$(date +%s)" -lt "$end" ] || ev event "$name measure-timeout new_leader=${newl:-none} stepped_down=$down healed=$healed back=$back"
	ha_end "$name"
}

# quorum_loss: a majority of the servers powered off ($QL_OFF, default the
# followers, as many as it takes). The API of each server left must answer
# 503 with the reason, the containers on the machines left keep running, and
# the routers keep serving (the clients). After $QL_DOWN s (60) without a
# leader the first is started again: a leader again; $QL_GAP s (30) later the
# rest, each back as a follower.
quorum_loss() {
	local name=quorum-loss L tf t now k s off=() left=() nol=0 nol_at=0 again="" back_at=0 back1=0 rest=0 done_at=0 end v pre kept="" kt=0
	declare -A last_api
	need_servers "$name"
	ha_begin "$name"
	L=$(leader) || die "$name: no leader"
	if [ -n "${QL_OFF:-}" ]; then
		read -r -a off <<<"$QL_OFF"
	else
		k=$((SOAK_SERVERS - $(majority) + 1))
		for s in "${SRV_NAMES[@]}"; do [ "$s" != "$L" ] && [ ${#off[@]} -lt "$k" ] && off+=("$s"); done
	fi
	for s in "${SRV_NAMES[@]}"; do case " ${off[*]} " in *" $s "*) ;; *) left+=("$s") ;; esac; done
	ev event "$name target ${off[*]} leader=$L left=${left[*]}"
	pre=$SOAK_DIR/pre-$name.tsv
	tf=$(now_ms)
	for s in "${off[@]}"; do ha_fault "$name" "$s" & done
	wait
	end=$(($(date +%s) + ${QL_SECS:-2400}))
	while [ "$(date +%s)" -lt "$end" ]; do
		t=$(now_ms)
		now=$(date +%s)
		ha_observe "$name"
		for s in "${left[@]}"; do
			v=$(api_answer "$s")
			[ "$v" = "${last_api[$s]:-}" ] || {
				ev event "$name api $s $v"
				last_api[$s]=$v
			}
		done
		if [ "$nol" = 0 ] && [ "$HLEAD" = none ]; then
			ev event "$name no-leader ms_after_fault=$(($(now_ms) - tf))"
			nol=1 nol_at=$now
		fi
		# the containers of the machines left, every 10 s while no server leads
		if [ "$nol" = 1 ] && [ "$back1" = 0 ] && [ "$now" -ge $((kt + 10)) ]; then
			v=$(kept_on "$pre" "${left[@]}" "${NAMES[@]:SOAK_SERVERS}")
			[ "$v" = "$kept" ] || {
				ev event "$name containers $v"
				kept=$v
			}
			kt=$now
		fi
		if [ "$nol" = 1 ] && [ "$back1" = 0 ] && [ "$now" -ge $((nol_at + ${QL_DOWN:-60})) ]; then
			ha_back "$name" "${off[0]}"
			back1=1
		fi
		if [ "$back1" = 1 ] && [ -z "$again" ] && [ "$HLEAD" != none ] && [ -n "$HLEAD" ]; then
			again=$HLEAD back_at=$now
			ev event "$name leader-again $again"
		fi
		if [ -n "$again" ] && [ "$rest" = 0 ] && [ "$now" -ge $((back_at + ${QL_GAP:-30})) ]; then
			for s in "${off[@]:1}"; do ha_back "$name" "$s"; done
			rest=1
		fi
		if [ "$rest" = 1 ]; then
			k=1
			for s in "${off[@]}"; do
				[ "$s" = "$HLEAD" ] || rejoined "$name" "$s" || k=0
			done
			if [ "$k" = 1 ] && [ "$HCONV" = yes ]; then
				[ "$done_at" != 0 ] || {
					done_at=$now
					ev event "$name all-back leader=$HLEAD"
				}
				[ "$now" -lt $((done_at + 10)) ] || break
			fi
		fi
		ha_tick "$t"
	done
	[ "$(date +%s)" -lt "$end" ] || ev event "$name measure-timeout no_leader=$nol leader_again=${again:-none} rest=$rest"
	ha_end "$name"
}

# service_restart [leader|follower]: the leader's hivemind restarted
# (systemctl restart; with one server, the server's), or with "follower" a
# follower's, the scenario service-restart-follower. The API is down while
# the leader restarts; with several servers, a new leader may be elected.
# Ends converged, 30 s after the API answered again at the earliest; for a
# follower, once it is a follower again in the leader's view.
service_restart() {
	local who=${1:-leader} name=service-restart T tf t now up_at=0 went=0 end L s v lastv="" back=0
	case "$who" in leader) ;; follower) name=service-restart-follower ;; *) die "service-restart: leader or follower, not $who" ;; esac
	ha_begin "$name"
	if [ "$SOAK_SERVERS" -gt 1 ]; then
		L=$(leader) || die "$name: no leader"
		T=$L
		if [ "$who" = follower ]; then
			T=""
			for s in "${SRV_NAMES[@]}"; do [ "$s" != "$L" ] && [ -z "$T" ] && T=$s; done
		fi
	else
		[ "$who" = leader ] || die "$name needs several servers"
		T=$SERVER
	fi
	ev event "$name target $T"
	tf=$(now_ms)
	ev event "$name $T fault restart"
	# in the background, so the loop sees the API go: the stop waits up to 10 s
	# for the router's held streams (docs/routing.md), so 60 s for the command
	(SSH_TIMEOUT=60 on "$T" "sudo systemctl restart hivemind.service" || ev event "$name $T restart-failed") &
	end=$(($(date +%s) + 900))
	while [ "$(date +%s)" -lt "$end" ]; do
		t=$(now_ms)
		now=$(date +%s)
		ha_observe "$name"
		[ "$HAPI" != fail ] || went=1
		# back: answering after it was seen down (or 60 s on, if it never was)
		[ "$up_at" != 0 ] || [ "$HAPI" != ok ] || { [ "$went" = 0 ] && [ "$now" -lt $((tf / 1000 + 60)) ]; } || {
			up_at=$now
			ev event "$name api-back ms_after_fault=$(($(now_ms) - tf)) seen_down=$went"
		}
		if [ "$who" = follower ]; then
			# the follower's own API, and its return as a follower
			v=$(api_answer "$T")
			[ "$v" = "${lastv:-}" ] || {
				ev event "$name api $T $v"
				lastv=$v
			}
			if [ "${back:-0}" = 0 ] && [ "$now" -ge $((tf / 1000 + 5)) ] && rejoined "$name" "$T"; then
				ev event "$name $T rejoined-follower leader=$HLEAD"
				back=1
			fi
			[ "${back:-0}" = 1 ] && [ "$HCONV" = yes ] && [ "$now" -ge $((tf / 1000 + 40)) ] && break
		else
			[ "$up_at" != 0 ] && [ "$HCONV" = yes ] && [ "$now" -ge $((up_at + 30)) ] && break
		fi
		ha_tick "$t"
	done
	ha_end "$name"
}

# hm_signal NAME SIG: SIG to the main process of NAME's hivemind unit.
hm_signal() {
	SSH_TIMEOUT=20 on "$1" "p=\$(systemctl show -p MainPID --value hivemind.service); [ \"\${p:-0}\" != 0 ] && sudo kill -$2 \$p"
}

# router_noleader [GAP]: a server's router that starts while no server leads
# (the eighth soak's third finding). The two followers' hivemind paused
# (SIGSTOP: their sockets stay open, nothing answers), so the leader steps
# down; 2 s later its hivemind stopped and started (systemctl), so its
# router starts with no leader; GAP s (1) after the start one of the others
# resumed (SIGCONT), so the two elect a leader; RN_REST s (15) later the
# last one. The clients through the restarted server's router (outside.sh)
# count what failed once its listener was open; its journal says when it
# listened and read the table. Ends once every server is a voter in the
# view of the leader a majority names and the cluster has converged, plus
# 10 s.
router_noleader() {
	local name=router-noleader gap=${1:-1} T s P=() tf t now end fpid newl="" done_at=0 ok gone=0
	need_servers "$name"
	ha_begin "$name"
	T=$(leader) || die "$name: no leader"
	for s in "${SRV_NAMES[@]}"; do [ "$s" = "$T" ] || P+=("$s"); done
	ev event "$name target $T paused=${P[*]} gap=$gap"
	tf=$(now_ms)
	(
		for s in "${P[@]}"; do
			ev event "$name $s fault pause"
			hm_signal "$s" STOP || ev event "$name $s pause-failed"
		done
		sleep 2
		ev event "$name $T fault stop"
		SSH_TIMEOUT=60 on "$T" "sudo systemctl stop hivemind.service" || ev event "$name $T stop-failed"
		ev event "$name $T stopped"
		SSH_TIMEOUT=60 on "$T" "sudo systemctl start hivemind.service" || ev event "$name $T start-failed"
		ev event "$name $T started"
		sleep "$gap"
		ev event "$name ${P[0]} fault resume"
		hm_signal "${P[0]}" CONT || ev event "$name ${P[0]} resume-failed"
		sleep "${RN_REST:-15}"
		ev event "$name ${P[1]} fault resume"
		hm_signal "${P[1]}" CONT || ev event "$name ${P[1]} resume-failed"
		ev event "$name resumed-all"
	) &
	fpid=$!
	end=$(($(date +%s) + 600))
	while [ "$(date +%s)" -lt "$end" ]; do
		t=$(now_ms)
		now=$(date +%s)
		ha_observe "$name"
		[ "$HLEAD" != none ] || gone=1
		if [ -z "$newl" ] && [ "$gone" = 1 ] && [ "$HLEAD" != none ] && [ -n "$HLEAD" ]; then
			newl=$HLEAD
			ev event "$name leader-again $newl ms_after_fault=$(($(now_ms) - tf))"
		fi
		if [ -n "$newl" ] && ! kill -0 "$fpid" 2>/dev/null && [ "$HCONV" = yes ]; then
			ok=1
			for s in "${SRV_NAMES[@]}"; do
				[ "$s" = "$HLEAD" ] || rejoined "$name" "$s" || ok=0
			done
			if [ "$ok" = 1 ]; then
				[ "$done_at" != 0 ] || {
					done_at=$now
					ev event "$name all-back leader=$HLEAD"
				}
				[ "$now" -lt $((done_at + 10)) ] || break
			fi
		fi
		ha_tick "$t"
	done
	[ "$(date +%s)" -lt "$end" ] || ev event "$name measure-timeout leader=${newl:-none} converged=$HCONV"
	wait "$fpid" 2>/dev/null || true
	# never leave a server paused
	for s in "${P[@]}"; do hm_signal "$s" CONT >/dev/null 2>&1 || true; done
	ev event "$name $T router $(on "$T" "sudo journalctl -u hivemind --since @$((tf / 1000)) -o short-iso-precise --no-pager | grep -E 'router listening|routing table|managing certificates|this server leads|shutting down|handed the leadership|stopped' | cut -c1-260" 2>/dev/null | tr -s ' \n' ' ' | cut -c1-2000)"
	ha_end "$name"
}

# The status sampler of rollout-leader, on the client machine through the
# client that knows every server (wload.sh's CLI and env): hivemind status
# --json every 0.5 s; one line per sample, "T sample N" and each app below
# its count, or "T error ..." (the CLI's first line), as rollout-all's.
read -r -d '' REMOTE_SAMPLER_CL <<'EOF' || true
f=$1
d=/var/lib/soak-client/wl
. $d/env
rm -f "$f" "$f.stop" "$f.below"
while [ ! -e "$f.stop" ]; do
	t=$(date -u +%s.%3N)
	if o=$(timeout 70 $d/hivemind status --json 2>&1); then
		l=$(printf '%s\n' "$o" | jq -r --arg t "$t" '"\($t) sample \(length)" + ([.[] | select((.name | startswith("wl-") | not) and (.running < .desired or .ready < .desired)) | " \(.name):desired=\(.desired),running=\(.running),ready=\(.ready),gen=\(.generation)"] | join(""))' 2>&1)
		echo "$l $(date -u +%s.%3N)" >>"$f"
		case "$l" in *"sample "*" "*) printf '== %s\n%s\n' "$t" "$(printf '%s' "$o" | jq -c '.[] | select(.name | startswith("wl-") | not) | {name, desired, running, ready, generation, rollout, replicas: [.replicas[] | {index, node_id, state, ready, generation, desired}]}')" >>"$f.below" ;; esac
	else
		echo "$t error $(printf '%s' "$o" | head -c 200 | tr '\n' ' ') $(date -u +%s.%3N)" >>"$f"
	fi
	sleep 0.5
done
EOF

# rollout_leader: a rolling update of every app (the image apps to version
# $RL_VERSION, 2 by default, a new ROLL in every app's environment, and the
# compose project again), through any server, and RL_AFTER s (3) after the
# PUTs, while rollouts are in progress, the leader's hivemind restarted
# (systemctl restart), so the leader changes during the rollouts. Once a
# second the HA observations, and each change of the rollouts' states; the
# status sampler on the client machine (above); ends when no rollout is in
# progress, a leader leads, the old one is back and the cluster has
# converged, plus 10 s. Logs the samples below a count, and each app's
# replicas' generations against the app's at the end.
rollout_leader() {
	local name=rollout-leader L tf t now end a v pids=() st busy seen=0 s="" rs_done=0 newl="" gone=0 back=0 done_at=0 f=/var/tmp/soak-rl-samples.txt cl
	local apps="www api spread kv proot papi grpc"
	need_servers "$name"
	cl=${WL_CLIENT:-$OUT_NAME}
	ha_begin "$name"
	L=$(leader) || die "$name: no leader"
	ev event "$name target $L"
	on "$cl" "cat >/var/tmp/soak-rl-sampler.sh" <<<"$REMOTE_SAMPLER_CL" || die "$name: sampler to $cl"
	on "$cl" "sudo setsid nohup bash /var/tmp/soak-rl-sampler.sh $f >/dev/null 2>&1 </dev/null &" || die "$name: sampler on $cl"
	sleep 2
	# the image apps to RL_VERSION (2; setup deployed version 1)
	v=${RL_VERSION:-2}
	ROLL="rl-$(date +%s)"
	export ROLL
	for a in $apps; do
		hma_in "deploy -f -" <<<"$(app_spec "$a" "$v")" >/dev/null &
		pids+=($!)
	done
	hma_in "compose deploy -" < <(shop_compose) >/dev/null &
	pids+=($!)
	for a in "${pids[@]}"; do wait "$a" || die "$name: a PUT failed"; done
	unset ROLL
	ev event "$name deploy every app version=$v"
	tf=$(($(now_ms) + ${RL_AFTER:-3} * 1000))
	end=$(($(date +%s) + 900))
	while [ "$(date +%s)" -lt "$end" ]; do
		t=$(now_ms)
		now=$(date +%s)
		if [ "$rs_done" = 0 ] && [ "$t" -ge "$tf" ]; then
			tf=$(now_ms)
			ev event "$name $L fault restart"
			(SSH_TIMEOUT=60 on "$L" "sudo systemctl restart hivemind.service" || ev event "$name $L restart-failed") &
			rs_done=1
		fi
		ha_observe "$name"
		st=$(SSH_TIMEOUT=8 hma "status --json" 2>/dev/null) || st=""
		if [ -n "$st" ]; then
			busy=$(jq -r '[.[] | select(.rollout.state? == "in_progress")] | length' <<<"$st" 2>/dev/null)
			[ "${busy:-0}" = 0 ] || seen=1
			a=$(jq -r '[.[] | select(.rollout != null and (.name | startswith("wl-") | not)) | "\(.name):\(.rollout.state // "-")"] | join(",")' <<<"$st" 2>/dev/null)
			[ "$a" = "$s" ] || {
				ev event "$name rollouts ${a:-none}"
				s=$a
			}
		else
			busy=""
		fi
		# the leadership left L: no leader, or another one, after the fault
		if [ "$rs_done" = 1 ] && [ "${gone:-0}" = 0 ] && { [ "$HLEAD" = none ] || { [ -n "$HLEAD" ] && [ "$HLEAD" != "$L" ]; }; }; then
			gone=1
			ev event "$name leadership-left $L leader=$HLEAD ms_after_fault=$(($(now_ms) - tf))"
		fi
		if [ "${gone:-0}" = 1 ] && [ -z "$newl" ] && [ "$HLEAD" != none ] && [ -n "$HLEAD" ]; then
			newl=$HLEAD
			ev event "$name new-leader $newl ms_after_fault=$(($(now_ms) - tf))"
		fi
		if [ -n "$newl" ] && [ "$back" = 0 ] && { rejoined "$name" "$L" || [ "$HLEAD" = "$L" ]; }; then
			ev event "$name $L back role=$(role_in "${HV[$HLEAD]:-}" "$L") leader=$HLEAD"
			back=1
		fi
		if [ "$back" = 1 ] && [ "$seen" = 1 ] && [ "$busy" = 0 ] && [ "$HCONV" = yes ]; then
			[ "$done_at" != 0 ] || {
				done_at=$now
				ev event "$name rollouts-done $((now - tf / 1000)) s after the fault"
			}
			[ "$now" -lt $((done_at + 10)) ] || break
		fi
		ha_tick "$t"
	done
	[ "$(date +%s)" -lt "$end" ] || ev event "$name measure-timeout new_leader=${newl:-none} back=$back seen=$seen busy=${busy:-?} converged=$HCONV"
	on "$cl" "touch $f.stop"
	sleep 3
	on "$cl" "cat $f" >"$SOAK_DIR/rollout-leader-samples.txt" 2>/dev/null || true
	on "$cl" "cat $f.below 2>/dev/null" >"$SOAK_DIR/rollout-leader-below.txt" 2>/dev/null || true
	f=$SOAK_DIR/rollout-leader-samples.txt
	ev event "$name sampler samples=$(grep -c ' sample ' "$f") errors=$(grep -c ' error ' "$f") below=$(grep -cE ' sample [0-9]+ [a-z]' "$f") below_not_kv=$(grep -E ' sample [0-9]+ [a-z]' "$f" | grep -cE ' (www|api|spread|proot|papi|grpc|front|cache):') first=$(grep -m1 -E ' sample [0-9]+ [a-z]' "$f" | cut -c1-300)"
	st=$(hma "status --json" 2>/dev/null) || st="[]"
	ev event "$name generations $(jq -r '[.[] | select(.name | startswith("wl-") | not) | "\(.name):\(.generation)=\([.replicas[] | "\(.generation)\(if .state == "running" then "" else "/" + .state end)"] | join("+"))"] | join(" ")' <<<"$st")"
	ha_end "$name"
}

# steady_journals SINCE: each server's journal since SINCE (Unix s), and one
# line with its elections and leader changes: "this server leads the
# cluster", raft's "entering candidate state" and "heartbeat timeout",
# errors and warnings.
steady_journals() {
	local s f
	for s in "${SRV_NAMES[@]}"; do
		f=$SOAK_DIR/journal-steady-$s.txt
		SSH_TIMEOUT=60 on "$s" "sudo journalctl -u hivemind --since @$1 -o short-iso-precise --no-pager" >"$f" 2>/dev/null || true
		ev event "steady journal $s lines=$(wc -l <"$f") leads=$(grep -c 'this server leads the cluster' "$f" || true) no_longer_leads=$(grep -c 'no longer leads' "$f" || true) candidate=$(grep -c 'entering candidate state' "$f" || true) heartbeat_timeout=$(grep -c 'heartbeat timeout' "$f" || true) errors=$(grep -c 'level=ERROR' "$f" || true) warns=$(grep -c 'level=WARN' "$f" || true)"
	done
}

# raft_sample: on each server, the Raft log's last index and, after the
# first sample, the entries appended since the one before: how many, the
# bytes of their data, and how many carry a statement that writes a pending
# replica's reason (the bytes "pending_reason"; the eighth soak's fourth
# finding). One "steady raft" line per server. steady takes one every
# STEADY_RAFT_EVERY s (300), so no entry is compacted away between two.
declare -A RAFT_IDX
raft_sample() {
	local s r q i n b p
	for s in "${SRV_NAMES[@]}"; do
		if [ -z "${RAFT_IDX[$s]:-}" ]; then
			q="SELECT (SELECT max(idx) FROM logs), 0, 0, 0"
		else
			q="SELECT (SELECT max(idx) FROM logs), count(*), coalesce(sum(length(data)), 0), coalesce(sum(instr(data, X'70656e64696e675f726561736f6e') > 0), 0) FROM logs WHERE idx > ${RAFT_IDX[$s]}"
		fi
		r=$(SSH_TIMEOUT=30 on "$s" "sudo sqlite3 -separator ' ' $DATA/raft/raft.db \"$q\"" 2>/dev/null) || r=""
		read -r i n b p <<<"$r" || true
		if [[ "$i" =~ ^[0-9]+$ ]] && [[ "$p" =~ ^[0-9]+$ ]]; then
			ev event "steady raft $s idx=$i entries=$n bytes=$b reasons=$p"
			RAFT_IDX[$s]=$i
		else
			ev event "steady raft $s fail=$(tr -s ' \n' '__' <<<"$r" | cut -c1-200)"
		fi
	done
}

# steady [SECS]: the steady state, as before (mark steady begin/end, the
# routers' metrics at both ends), for SECS (3600); with the write-load, which
# deletes the app it deployed $STEADY_KEEP (100) cycles before, so the store
# holds about that many.
steady() {
	local secs=${1:-3600} wl=none t0
	local tag=steady
	if [ "$WLOAD" = 1 ]; then
		tag=$("$HERE/wload.sh" tag steady) && WL_KEEP=${STEADY_KEEP:-100} "$HERE/wload.sh" start "$tag" || die "steady: the write-load did not start"
	fi
	ev mark "steady begin"
	t0=$(date +%s)
	ev event "steady servers $(servers_line)"
	metrics_snap steady-begin
	raft_sample
	local rest=$secs step=${STEADY_RAFT_EVERY:-300} d
	while [ "$rest" -gt 0 ]; do
		d=$step
		[ "$d" -le "$rest" ] || d=$rest
		sleep "$d"
		rest=$((rest - d))
		raft_sample
	done
	metrics_snap steady-end
	ev mark "steady end"
	ev event "steady servers $(servers_line)"
	steady_journals "$t0"
	if [ "$WLOAD" = 1 ]; then
		"$HERE/wload.sh" stop || true
		if "$HERE/wload.sh" check "$tag"; then wl=pass; else wl=FAIL; fi
		[ "$WL_CLEAN" != 1 ] || "$HERE/wload.sh" clean "$tag" || wl="$wl,clean-FAIL"
	fi
	ev event "steady write-check $wl"
}

# ha_selftest: the parsers on canned replies, without machines.
ha_selftest() {
	local j v fails=0
	j='{"leader":"n2id","answered_by":"n1id","servers":[{"id":"n1id","name":"ip-1","advertise":"'${IPS[0]}'","node_port":4484,"role":"follower"},{"id":"n2id","advertise":"'${IPS[1]:-10.9.9.2}'","role":"leader"},{"id":"n3id","advertise":"10.9.9.3:4484","role":"nonvoter"}]}'
	v=$(view_of "$j")
	echo "view: $v"
	[ "$(leader_of_view "$v")" = "${NAMES[1]:-10.9.9.2}" ] || { echo "FAIL leader_of_view"; fails=$((fails + 1)); }
	[ "$(leader_in "$j")" = "${NAMES[1]:-10.9.9.2}" ] || { echo "FAIL leader_in"; fails=$((fails + 1)); }
	[ "$(role_in "$v" "${NAMES[0]}")" = follower ] || { echo "FAIL role_in"; fails=$((fails + 1)); }
	[ "$(role_in "$v" 10.9.9.3)" = nonvoter ] || { echo "FAIL role_in with a port"; fails=$((fails + 1)); }
	j='{"leader":"","answered_by":"n1id","servers":[{"id":"n1id","advertise":"'${IPS[0]}'","role":"follower"}],"note":"no leader: this server reaches 0 of the 2 others; a write needs 2 of 3"}'
	v=$(view_of "$j")
	echo "view: $v"
	[ "$(leader_of_view "$v")" = none ] || { echo "FAIL no leader"; fails=$((fails + 1)); }
	[ "$(leader_of_view api=fail)" = unknown ] || { echo "FAIL no view"; fails=$((fails + 1)); }
	case "$v" in *"note=no_leader:_this_server"*) ;; *)
		echo "FAIL note"
		fails=$((fails + 1))
		;;
	esac
	[ "$(view_of 'not json')" = "" ] || { echo "FAIL bad json"; fails=$((fails + 1)); }
	# a surge (web 0, generations 1 and 2 on two nodes) is not a copy twice;
	# api 1, generation 3 on two nodes, is; an exited copy counts for neither
	local f=$SOAK_DIR/selftest-dups.tsv
	printf 'C\tn-1\taaa\trunning\tt\tweb\t0\t1\nC\tn-2\tbbb\trunning\tt\tweb\t0\t2\nC\tn-3\tccc\trunning\tt\tapi\t1\t3\nC\tn-4\tddd\trunning\tt\tapi\t1\t3\nC\tn-4\teee\texited\tt\tweb\t1\t2\nC\tn-1\tfff\trunning\tt\tweb\t1\t2\n' >"$f"
	echo "dups: $(dups "$f" | tr '\n' ';') dups_gen: $(dups_gen "$f" | tr '\n' ';')"
	[ "$(dups "$f" | wc -l)" = 2 ] || { echo "FAIL dups"; fails=$((fails + 1)); }
	[ "$(dups_gen "$f")" = "api|1|g3 n-3,n-4" ] || { echo "FAIL dups_gen"; fails=$((fails + 1)); }
	echo "ha-selftest: $fails failed"
	[ "$fails" = 0 ]
}
