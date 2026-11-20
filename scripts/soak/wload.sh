#!/usr/bin/env bash
# The write-load of the soak of several servers (ha.sh): API writes through
# a client that knows every server, and a check that no write the API
# acknowledged is missing from any server's store.
#
#   wload.sh client-setup      the CLI, the API's CA and the admin token on the
#                              client machine ($WL_CLIENT, default the outside
#                              machine), and a call through it
#   wload.sh start TAG         the write loop, apps $WL_PREFIX<TAG>-N
#   wload.sh stop              stop it
#   wload.sh probe-start       the API probe: a read through the same client,
#                              and each server's API directly, every 0.5 s
#   wload.sh probe-stop
#   wload.sh status            writes and probes so far
#   wload.sh fetch [TAG]       the logs to $SOAK_DIR/wl/
#   wload.sh check TAG         every server's store against the writes the API
#                              acknowledged; fails if one is missing anywhere
#   wload.sh clean TAG         delete TAG's apps, through the client; then check
#                              that every server's store has none
#
# Needs SOAK_API_TLS=1 (lib.sh): the client reaches each server's API on its
# private address, port 4483, and trusts the run's API CA. The client is
# hivemind's own CLI with HIVEMIND_SERVER naming every server
# (https://a:4483,https://b:4483,...; with one server, the one URL), the
# token from a 0600 file in its client config.
#
# The writes, one at a time, every $WL_EVERY_MS ms (default 1000), cycle
# through:
#   deploy  a new app wl-TAG-N: busybox:stable, one replica, WL_SEQ=N in its
#           environment, and $WL_CPU (64) CPU cores, which no node has, so its
#           replicas wait as pending ("no node has 64 CPUs free") and never
#           start a container: the writes go through the store and its
#           replication, and leave the containers, the routers and the
#           convergence check (which accepts a pending replica with a reason)
#           alone;
#   scale   the app the write before deployed, to 2 replicas;
#   delete  with WL_KEEP=K > 0 (the steady state), the app deployed K cycles
#           before, so the apps do not pile up. With 0 (the default), an app
#           is never written again after its scale, so the check sees the
#           effect of every acknowledged write, and none is covered by a later
#           one.
# A write is acknowledged when the CLI exits 0, which it does only for a 2xx.
# One line per write in TAG.log on the client, fetched to $SOAK_DIR/wl:
#   <start, Unix s.us> <end> <op> <app> <want> rc=<exit> gen=<generation>
#   seq=<WL_SEQ> replicas=<n> err=<first line of standard error, _ for spaces>
# gen, seq and replicas are read from the reply (--json).
#
# The probe, every $WL_PROBE_MS ms (500): hivemind nodes --json through the
# client, one line of probe.log, "<start> <end> rc=<exit> err=<...>"; and GET
# /v1/nodes of each server's API directly with curl, one line of direct.log,
# "<Unix s.us> <addr:port>=<http code>[:<body>] ...".
#
# The check reads every server's own store: hivemind backup on the server
# (the data directory, with SQLite's VACUUM INTO; a follower would pass an API
# request to the leader, so hivemind apps against each server reads the
# leader's) and sqlite3 on the copy, the apps table's rows of TAG. It also
# reads hivemind apps --json through the API. For each source:
#   - every acknowledged deploy's app is there with its WL_SEQ, unless an
#     acknowledged delete followed;
#   - every acknowledged scale's app has 2 replicas, unless a delete followed;
#   - every acknowledged delete's app is gone;
#   - no app has another app's WL_SEQ;
# and the stores of all servers have the same rows. A write that was not
# acknowledged may or may not be there. From the replies alone: an
# acknowledged deploy's reply has generation 1 and its WL_SEQ, an
# acknowledged scale's names the WL_SEQ of its app's deploy, and a scale or
# delete of an app whose deploy was acknowledged must not be "not found".
# A source that fails is read again every 3 s for $WL_CHECK_WAIT s (30), for
# a follower that applies the last entries late. One "wl" line per source and
# a result line; the details in $SOAK_DIR/wl/TAG.check.
set -euo pipefail
. "$(dirname "$0")/lib.sh"

WLH=${WL_CLIENT:-$OUT_NAME}
WDIR=/var/lib/soak-client/wl
WL_EVERY_MS=${WL_EVERY_MS:-1000}
WL_KEEP=${WL_KEEP:-0}
WL_CPU=${WL_CPU:-64}
WL_PROBE_MS=${WL_PROBE_MS:-500}
WL_CHECK_WAIT=${WL_CHECK_WAIT:-30}
LDIR=$SOAK_DIR/wl
mkdir -p "$LDIR"

[ -n "$WLH" ] || die "wload: no client machine (WL_CLIENT, or an outside machine in $OUTSIDE_FILE)"
[ "$SOAK_API_TLS" = 1 ] || die "wload: needs SOAK_API_TLS=1, the API on each server's address"
cl() { on "$WLH" "$@"; }
tag_ok() { [[ "$1" =~ ^[a-z0-9][a-z0-9-]*$ ]] || die "wload: a tag is lower-case letters, digits and -: $1"; }

read -r -d '' WL_LOOP <<'EOF' || true
#!/bin/bash
# wl.sh TAG EVERY_MS KEEP CPU: the write loop (scripts/soak/wload.sh).
d=/var/lib/soak-client/wl
tag=$1 every=$2 keep=$3 cpu=$4
. $d/env
f=$d/$tag.log
cyc=2
[ "$keep" -gt 0 ] && cyc=3
n=0
# wload.sh stop makes $d/wl.stop: the loop ends between two writes, so
# every write it made is in the log
while [ ! -e $d/wl.stop ]; do
	t=$EPOCHREALTIME
	n=$((n + 1))
	in=""
	case $(((n - 1) % cyc)) in
	0)
		op=deploy app=wl-$tag-$n want=seq=$n
		in='{"name":"'$app'","image":"busybox:stable","replicas":1,"command":["sleep","86400"],"env":{"WL_SEQ":"'$n'"},"resources":{"cpu":'$cpu'}}'
		args=(deploy -f - --json)
		;;
	1)
		op=scale app=wl-$tag-$((n - 1)) want=replicas=2
		args=(scale "$app" 2 --json)
		;;
	2)
		k=$((n - 2 - cyc * keep))
		op=delete app=wl-$tag-$k want=gone
		args=(delete "$app" --json)
		[ "$k" -ge 1 ] || op=""
		;;
	esac
	if [ -n "$op" ]; then
		t0=$EPOCHREALTIME
		out=$(printf '%s' "$in" | timeout 90 $d/hivemind "${args[@]}" 2>$d/err.$tag)
		rc=$?
		t1=$EPOCHREALTIME
		err=$(head -n 1 $d/err.$tag | cut -c1-300 | tr -s ' \t' '__')
		g=- s=- r=-
		[ -z "$out" ] || read -r g s r < <(jq -r '(if .app then .app else . end) | "\(.generation // "-") \(.spec.env.WL_SEQ // "-") \(.replicas // "-")"' <<<"$out" 2>/dev/null)
		echo "$t0 $t1 $op $app $want rc=$rc gen=${g:--} seq=${s:--} replicas=${r:--} err=${err:--}" >>"$f"
	fi
	us=$((${EPOCHREALTIME/./} - ${t/./}))
	s=$((every * 1000 - us))
	[ "$s" -le 0 ] || sleep "$((s / 1000000)).$(printf '%06d' $((s % 1000000)))"
done
EOF

read -r -d '' WL_PROBE <<'EOF' || true
#!/bin/bash
# wl-probe.sh EVERY_MS: the API as a client that knows every server sees it
# (scripts/soak/wload.sh).
d=/var/lib/soak-client/wl
every=$1
. $d/env
while :; do
	t=$EPOCHREALTIME
	timeout 70 $d/hivemind nodes --json >/dev/null 2>$d/err.probe
	rc=$?
	echo "$t $EPOCHREALTIME rc=$rc err=$(head -n 1 $d/err.probe | cut -c1-300 | tr -s ' \t' '__')" >>$d/probe.log
	line=$EPOCHREALTIME
	for u in ${HIVEMIND_SERVER//,/ }; do
		c=$(curl -s -o $d/direct.body -w '%{http_code}' -m 2 --cacert $d/api-ca.pem -K $d/curlrc "$u/v1/nodes")
		e=""
		[ "$c" = 200 ] || e=":$(head -c 200 $d/direct.body 2>/dev/null | tr -s ' \t\n' '___')"
		line+=" ${u#https://}=$c$e"
	done
	echo "$line" >>$d/direct.log
	us=$((${EPOCHREALTIME/./} - ${t/./}))
	s=$((every * 1000 - us))
	[ "$s" -le 0 ] || sleep "$((s / 1000000)).$(printf '%06d' $((s % 1000000)))"
done
EOF

read -r -d '' WL_CLEAN <<'EOF' || true
#!/bin/bash
# wl-clean.sh TAG: delete every app of TAG, through the client; one line per
# delete in TAG.clean.log, as the loop's. Exits 1 if the list cannot be read.
d=/var/lib/soak-client/wl
tag=$1
. $d/env
f=$d/$tag.clean.log
for try in 1 2 3 4 5 6 7 8 9 10; do
	l=$(timeout 70 $d/hivemind apps --json 2>/dev/null) && break
	sleep 3
done
[ -n "$l" ] || exit 1
for app in $(jq -r --arg p "wl-$tag-" '(if type == "array" then . else (.apps // []) end)[] | .name | select(startswith($p))' <<<"$l"); do
	for try in 1 2 3; do
		t0=$EPOCHREALTIME
		out=$(timeout 90 $d/hivemind delete "$app" --json 2>$d/err.$tag)
		rc=$?
		err=$(head -n 1 $d/err.$tag | cut -c1-300 | tr -s ' \t' '__')
		echo "$t0 $EPOCHREALTIME delete $app gone rc=$rc gen=- seq=- replicas=- err=${err:--}" >>"$f"
		[ "$rc" = 0 ] && break
		sleep 1
	done
done
EOF

# The rows of TAG's apps in one server's own store: "#method M", then
# "name generation replicas deleted WL_SEQ" per app.
read -r -d '' REMOTE_STORE <<'EOF' || true
tag=$1
t=$(mktemp -d /var/tmp/soak-wl.XXXXXX) || exit 1
trap 'rm -rf "$t"' EXIT
m=backup
if ! { /usr/local/bin/hivemind backup "$t/b.tar" >"$t/out" 2>&1 && tar -C "$t" -xf "$t/b.tar" hivemind.db; }; then
	m="live:$(head -n 1 "$t/out" | tr -s ' \t' '__' | cut -c1-160)"
	sqlite3 'file:/var/lib/hivemind/hivemind.db?mode=ro' ".backup '$t/hivemind.db'" || { echo "#method none"; exit 1; }
fi
echo "#method $m"
sqlite3 -separator ' ' "$t/hivemind.db" "SELECT name, generation, replicas, deleted, COALESCE(json_extract(spec, '\$.env.WL_SEQ'), '-') FROM apps WHERE name LIKE 'wl-$tag-%' ORDER BY name"
EOF

client_setup() {
	local want got
	want=$(sha256sum "$BIN" | cut -d' ' -f1)
	SSH_TIMEOUT=120 cl "sudo install -d -m 0700 $WDIR && cat >/tmp/hivemind.wl && sudo install -m 0755 /tmp/hivemind.wl $WDIR/hivemind && rm -f /tmp/hivemind.wl" <"$BIN" ||
		die "wload: the CLI to $WLH"
	got=$(cl "sudo sha256sum $WDIR/hivemind" | cut -d' ' -f1)
	[ "$got" = "$want" ] || die "wload: $WLH: CLI digest $got, want $want"
	on "$SERVER" "sudo cat $DATA/admin.token" | cl "sudo sh -c 'umask 077; cat > $WDIR/admin.token'" || die "wload: the admin token to $WLH"
	cl "sudo tee $WDIR/api-ca.pem >/dev/null" <"$API_TLS_DIR/ca.crt" || die "wload: the API's CA to $WLH"
	cl "sudo bash -s $WDIR '$(api_urls)'" <<'EOF' || die "wload: the client's config on $WLH"
set -e
d=$1
umask 077
printf 'token-file: %s\nca-cert: %s\n' "$d/admin.token" "$d/api-ca.pem" >"$d/client.yaml"
printf 'export HIVEMIND_SERVER=%s\nexport HIVEMIND_CLIENT_CONFIG=%s\n' "$2" "$d/client.yaml" >"$d/env"
printf 'header = "Authorization: Bearer %s"\n' "$(cat "$d/admin.token")" >"$d/curlrc"
EOF
	cl "sudo tee $WDIR/wl.sh >/dev/null && sudo chmod 755 $WDIR/wl.sh" <<<"$WL_LOOP"
	cl "sudo tee $WDIR/wl-probe.sh >/dev/null && sudo chmod 755 $WDIR/wl-probe.sh" <<<"$WL_PROBE"
	cl "sudo tee $WDIR/wl-clean.sh >/dev/null && sudo chmod 755 $WDIR/wl-clean.sh" <<<"$WL_CLEAN"
	local out
	out=$(cl "sudo bash -c '. $WDIR/env && $WDIR/hivemind version && $WDIR/hivemind nodes --json | jq length'" 2>&1) || die "wload: a call through the client on $WLH failed: $(tr '\n' ' ' <<<"$out")"
	ev wl "client $WLH servers=$(api_urls) cli=\"$(head -n 1 <<<"$out")\" nodes=$(tail -n 1 <<<"$out")"
}

start() {
	local tag=$1
	tag_ok "$tag"
	cl "sudo test ! -e $WDIR/$tag.log" || die "wload: $tag has a log already; use another tag"
	cl "! pgrep -f '$WDIR/[w]l.sh ' >/dev/null" || die "wload: a write loop runs already; wload.sh stop first"
	cl "sudo rm -f $WDIR/wl.stop && sudo setsid nohup $WDIR/wl.sh $tag $WL_EVERY_MS $WL_KEEP $WL_CPU >/dev/null 2>&1 </dev/null &" || die "wload: start on $WLH"
	ev wl "start $tag every_ms=$WL_EVERY_MS keep=$WL_KEEP cpu=$WL_CPU servers=$(api_urls)"
}

# stop: the loop ends after the write in flight (a CLI call gives up after
# 60 s; the loop's timeout is 90 s), so that every write is in its log.
stop() {
	SSH_TIMEOUT=120 cl "sudo touch $WDIR/wl.stop; for i in \$(seq 1 500); do pgrep -f '$WDIR/[w]l.sh ' >/dev/null || exit 0; sleep 0.2; done; sudo pkill -f '$WDIR/[w]l.sh '; exit 1" ||
		ev wl "stop: the loop did not end within 100 s; killed, its last write may be missing from the log"
	ev wl "stop"
}

probe_start() {
	cl "pgrep -f '$WDIR/[w]l-probe.sh' >/dev/null" && return 0
	cl "sudo setsid nohup $WDIR/wl-probe.sh $WL_PROBE_MS >/dev/null 2>&1 </dev/null &" || die "wload: probe on $WLH"
	ev wl "probe start every_ms=$WL_PROBE_MS servers=$(api_urls)"
}

probe_stop() {
	cl "sudo pkill -f '$WDIR/[w]l-probe.sh'; true"
	ev wl "probe stop"
}

status() {
	cl "sudo bash -s $WDIR" <<'EOF'
cd "$1" || exit 1
for f in *.log; do
	[ -e "$f" ] || continue
	awk -v f="$f" '{ n++; if ($6 == "rc=0" || $3 == "rc=0") ok++ } END { printf "%s lines=%d ok=%d\n", f, n, ok }' "$f"
done
tail -n 1 direct.log 2>/dev/null
EOF
}

fetch() {
	local tag=${1:-}
	SSH_TIMEOUT=120 cl "sudo sh -c 'cd $WDIR && tar -czf - \$(ls *.log 2>/dev/null)'" | tar -xzf - -C "$LDIR" || die "wload: fetch from $WLH"
	[ -z "$tag" ] || [ -s "$LDIR/$tag.log" ] || die "wload: no log for $tag"
}

# store_rows NAME TAG FILE: NAME's own store's rows for TAG to FILE; fails,
# with "#method fail" in FILE, when it cannot read them.
store_rows() {
	SSH_TIMEOUT=60 on "$1" "sudo bash -s $2" <<<"$REMOTE_STORE" >"$3" 2>/dev/null || {
		echo "#method fail" >"$3"
		return 1
	}
}
# api_rows TAG FILE: the same through the API (the leader's store).
api_rows() {
	local j
	j=$(SSH_TIMEOUT=60 hma "apps --json") || return 1
	{
		echo "#method api"
		jq -r --arg p "wl-$1-" '(if type == "array" then . else (.apps // []) end)[] | select(.name | startswith($p)) | "\(.name) \(.generation) \(.replicas) 0 \(.spec.env.WL_SEQ // "-")"' <<<"$j" | sort
	} >"$2"
}

# The evaluation: writes (TAG.log and TAG.clean.log), then a source's rows.
# Prints "result pass|fail" and the counts, then one line per problem.
read -r -d '' EVAL <<'EOF' || true
function num(s) { sub(/^.*-/, "", s); return s + 0 }
function val(f,   i) { i = index(f, "="); return substr(f, i + 1) }
FILENAME != rowsf {
	# <t0> <t1> <op> <app> <want> rc= gen= seq= replicas= err=
	op = $3; app = $4; rc = val($6); g = val($7); s = val($8); r = val($9); e = val($10)
	if (rc == 0) {
		if (op == "deploy") {
			dep[app] = 1; ndep++
			if (g != 1 || s != num(app)) { reply++; prob = prob "reply deploy " app " gen=" g " seq=" s "\n" }
		} else if (op == "scale") {
			sc[app] = 1; nsc++
			if (r != 2 || s != num(app)) { reply++; prob = prob "reply scale " app " replicas=" r " seq=" s "\n" }
		} else if (op == "delete") { del[app] = 1; ndel++ }
	} else {
		fail++
		if (op == "deploy") udep[app] = 1
		if ((op == "scale" || op == "delete") && (app in dep) && !(app in del) && tolower(e) ~ /not.found|404|no_such|no_app/) {
			lostr++; prob = prob "lost " app ": its deploy was acknowledged, then " op " said " e "\n"
		}
	}
	next
}
/^#method / { method = $2; next }
NF >= 5 { if ($4 == 0) { have[$1] = 1; G[$1] = $2; R[$1] = $3; S[$1] = $5; rows++ } }
END {
	for (a in dep) if (!(a in del)) {
		if (!(a in have)) { miss++; prob = prob "missing deploy " a "\n" }
		else if (S[a] != num(a)) { wrong++; prob = prob "wrong " a " seq=" S[a] "\n" }
	}
	for (a in sc) if (!(a in del) && (a in have) && R[a] != 2) { miss++; prob = prob "missing scale " a " replicas=" R[a] "\n" }
	for (a in del) if (a in have) { miss++; prob = prob "missing delete " a " (still there)\n" }
	for (a in have) {
		if (S[a] != num(a)) { wrong++; prob = prob "corrupt " a " seq=" S[a] "\n" }
		if ((a in udep) && !(a in dep)) applied++
	}
	bad = miss + wrong + reply + lostr
	printf "result %s method=%s acked_deploy=%d acked_scale=%d acked_delete=%d failed=%d rows=%d missing=%d wrong=%d reply_odd=%d lost_by_reply=%d unacked_deploys_present=%d\n",
		(bad ? "fail" : "pass"), method, ndep, nsc, ndel, fail, rows, miss, wrong, reply, lostr, applied
	printf "%s", prob
}
EOF

# evaluate TAG ROWS: the evaluation of one source.
evaluate() {
	local w=("$LDIR/$1.log")
	[ -s "$LDIR/$1.clean.log" ] && w+=("$LDIR/$1.clean.log")
	awk -v rowsf="$2" "$EVAL" "${w[@]}" "$2"
}

# rows_key FILE: the rows that are there (not deleted), for comparing stores.
rows_key() { awk '!/^#/ && NF >= 5 && $4 == 0' "$1" | sort; }

check() {
	local tag=$1 out=$LDIR/$1.check end s f r ok consistent first="" k
	tag_ok "$tag"
	fetch "$tag"
	end=$(($(date +%s) + WL_CHECK_WAIT))
	: >"$out"
	declare -A res
	while :; do
		ok=1
		for s in "${SRV_NAMES[@]}" api; do
			[ -n "${res[$s]:-}" ] && continue
			f=$LDIR/$tag.rows.$s
			if [ "$s" = api ]; then api_rows "$tag" "$f" || echo "#method fail" >"$f"; else store_rows "$s" "$tag" "$f" || true; fi
			[ -s "$f" ] || echo "#method fail" >"$f"
			r=$(evaluate "$tag" "$f")
			if head -n 1 <<<"$r" | grep -q '^result pass' && ! grep -q '^#method \(fail\|none\)' "$f"; then
				res[$s]=pass
				ev wl "check $tag $s $(head -n 1 <<<"$r" | cut -d' ' -f2-)"
				printf '== %s\n%s\n' "$s" "$r" >>"$out"
			else
				ok=0
				[ "$(date +%s)" -lt "$end" ] && continue
				res[$s]=fail
				ev wl "check $tag $s $(head -n 1 <<<"$r" | cut -d' ' -f2-) first=$(sed -n 2p <<<"$r" | tr ' ' '_' | cut -c1-200)"
				printf '== %s\n%s\n' "$s" "$r" >>"$out"
			fi
		done
		[ "$ok" = 1 ] || [ "$(date +%s)" -ge "$end" ] || { sleep 3; continue; }
		break
	done
	# every server's store has the same rows
	consistent=yes
	for s in "${SRV_NAMES[@]}"; do
		k=$(rows_key "$LDIR/$tag.rows.$s" | sha256sum | cut -c1-12)
		if [ -z "$first" ]; then first=$k; elif [ "$k" != "$first" ]; then consistent=no; fi
		printf '== %s rows %s %s\n' "$s" "$(rows_key "$LDIR/$tag.rows.$s" | wc -l)" "$k" >>"$out"
	done
	r=pass
	for s in "${SRV_NAMES[@]}" api; do [ "${res[$s]}" = pass ] || r=fail; done
	[ "$consistent" = yes ] || r=fail
	ev wl "check $tag result=$r servers_same_rows=$consistent sources=$(for s in "${SRV_NAMES[@]}" api; do printf '%s:%s,' "$s" "${res[$s]}"; done)"
	[ "$r" = pass ]
}

clean() {
	local tag=$1 s f left=0 n
	tag_ok "$tag"
	SSH_TIMEOUT=1800 cl "sudo $WDIR/wl-clean.sh $tag" || die "wload: clean $tag: the apps could not be listed"
	fetch "$tag"
	n=$(awk '$6 == "rc=0"' "$LDIR/$tag.clean.log" 2>/dev/null | wc -l)
	# the deletes finish in the store when the replicas are gone; pending ones have none
	sleep 5
	for s in "${SRV_NAMES[@]}"; do
		f=$LDIR/$tag.rows-clean.$s
		store_rows "$s" "$tag" "$f" || echo "#method fail" >"$f"
		left=$((left + $(rows_key "$f" | wc -l)))
	done
	ev wl "clean $tag deleted=$n left_in_stores=$left"
	[ "$left" = 0 ]
}

cmd=${1:-}
shift || true
case "$cmd" in
client-setup) client_setup ;;
start) start "${1:?tag}" ;;
stop) stop ;;
probe-start) probe_start ;;
probe-stop) probe_stop ;;
status) status ;;
fetch) fetch "${1:-}" ;;
check) check "${1:?tag}" ;;
clean) clean "${1:?tag}" ;;
eval) evaluate "${1:?tag}" "${2:?rows file}" ;;
*)
	sed -n '2,21p' "$0"
	exit 2
	;;
esac
