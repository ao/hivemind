#!/usr/bin/env bash
# The scale run: 100 apps and 500 replicas on the soak machines, deployed and
# changed through the server's API. It runs on the control machine, like the
# rest of scripts/soak, after setup.sh with SETUP_APPS=0, and logs to the same
# log (lib.sh).
#
#   scale.sh specs              write the app specs and compose files to $SOAK_DIR/specs
#   scale.sh pull               pull the three images on every node, timed
#   scale.sh deploy seq|par N   send every app to the server, one after another or N at
#                               once, then sample once a second until every replica runs
#   scale.sh delete             delete every app; sample until no container is left
#   scale.sh kv-write           write the value the probes read back from kv
#   scale.sh sample NAME SECS   sample the status once a second for SECS
#   scale.sh lose NAME...       after NAMEs were powered off: sample until every replica
#                               without a volume runs on another node
#   scale.sh return NAME...     after they were started: sample until they are ready, their
#                               old containers are gone and no replica runs twice
#   scale.sh rollout N [PHASE]  change an env value of N apps at once; sample until every
#                               rollout has ended, or every one left is paused
#   scale.sh drain NAME         hivemind node drain NAME; sample until it returns
#   scale.sh remove NAME        hivemind node remove NAME (a lost node); sample
#   scale.sh scale FACTOR       set every portless app to FACTOR times its base count (1 is
#                               back); sample until the containers match
#   scale.sh restart-server     systemctl restart on the server; sample until converged
#   scale.sh lat N              N requests each of GET /v1/status, /v1/nodes, /v1/apps, /v1/routes
#   scale.sh routes             the routed apps' endpoints in GET /v1/routes
#   scale.sh served             through every router: each host, each path route, each h2c route
#   scale.sh h2c-start|h2c-stop|h2c-status
#                               unary calls to an h2c route through each router in turn
#   scale.sh metrics [N]        N timed reads of the server's /metrics, with the router series
#   scale.sh perf-start SECS [NAME] | perf-fetch [NAME] | perf-report DIR
#                               perf record of hivemind on NAME (the server), copied here, read
#   scale.sh listeners          every node's TCP listeners; fail on an app port on a wildcard
#   scale.sh on NAME CMD        run CMD on NAME over the harness's ssh connection
#
# Every request to the API is made on the server machine, by curl or the CLI,
# and timed there (curl's time_total). Every other time is this machine's
# clock, as in the rest of the harness. The status samples are one
# GET /v1/status a second, made over the one ssh connection to the server.
#
# Log lines (besides lib.sh's):
#   req    phase name kind code seconds      one API request
#   ss     phase code s bytes apps desired running ready pending nonode below notready rollouts paused
#   mark   phase begin|end ...               the window of a phase
#   event  phase ...                         the first time a condition holds
set -uo pipefail
. "$(dirname "$0")/lib.sh"

SPECS=$SOAK_DIR/specs
NG=${NGINX_IMAGE:-nginx:alpine}
BB=${BUSYBOX_IMAGE:-busybox:stable}
RD=${REDIS_IMAGE:-redis:7-alpine}
RDIR=/var/tmp/hivemind-scale
SCALE_MIX=${SCALE_MIX:-1}
# The apps with a volume, for lib.sh and scenario.sh.
export VOLUME_APPS="kv vol-1 vol-2 vol-3 vol-4 vol-5 vol-6 vol-7 vol-8 vol-9"

# ---------------------------------------------------------------- the apps

# The 100 apps (500 replicas). Kinds, in $SPECS/apps.tsv as "name kind replicas portless":
#   web     nginx, 3 replicas, 127.0.0.1:8080 (the probes curl it)
#   hp-NN   18 nginx apps, 3 replicas each, 127.0.0.1:90NN
#   kv      redis with a volume and 127.0.0.1:6379 (the probes read a value back)
#   vol-N   9 redis apps with a volume, 1 replica each, no port
#   hc-NN   30 nginx apps, 5 replicas each, an HTTP health check on port 80
#   rt-NN   10 nginx apps, 3 replicas each, routed as rt-NN.scale.test
#   cmpN    5 compose files with one service each, 4 replicas: busybox sleep (1-3), nginx (4-5)
#   bb-NN   16 busybox sleep apps, 9 replicas each (bb-16: 8)
#   ng-NN   10 nginx apps, 9 replicas each
# So 20 apps publish a host port, 10 have a volume, 30 a health check, 10 are
# routed and 5 come from compose files; the 71 without a host port or a volume
# are the portless ones that "scale" changes.
#
# With SCALE_MIX=2 (the second scale run), ng-06 to ng-10 are replaced by
# five routed apps of 9 replicas each, so the totals stay 100 and 500:
#   pr-01..03  nginx on the path /api of rt-01.scale.test to rt-03.scale.test,
#              with a page at /api/ that says the app's name
#   gr-01..02  test/h2ctest, a gRPC-style server that speaks only h2c, routed
#              as h2c-01.scale.test and h2c-02.scale.test with protocol h2c,
#              a TCP health check; the image is $H2C_IMAGE, imported on each
#              node from $SOAK_DIR/h2ctest (import_h2c in lib.sh)
specs() {
	local i n
	rm -rf "$SPECS"
	mkdir -p "$SPECS/json" "$SPECS/compose"
	: >"$SPECS/apps.tsv"
	app() { # app NAME KIND REPLICAS PORTLESS JSON
		printf '%s\n' "$5" >"$SPECS/json/$1.json"
		printf '%s\t%s\t%s\t%s\n' "$1" "$2" "$3" "$4" >>"$SPECS/apps.tsv"
	}
	app web hostport 3 0 "{\"name\":\"web\",\"image\":\"$NG\",\"replicas\":3,\"ports\":[{\"host\":8080,\"container\":80,\"bind\":\"127.0.0.1\"}]}"
	for i in $(seq -w 1 18); do
		app "hp-$i" hostport 3 0 "{\"name\":\"hp-$i\",\"image\":\"$NG\",\"replicas\":3,\"ports\":[{\"host\":$((9000 + 10#$i)),\"container\":80,\"bind\":\"127.0.0.1\"}]}"
	done
	app kv volume 1 0 "{\"name\":\"kv\",\"image\":\"$RD\",\"command\":[\"redis-server\",\"--appendonly\",\"yes\"],\"ports\":[{\"host\":6379,\"container\":6379,\"bind\":\"127.0.0.1\"}],\"volumes\":[{\"name\":\"data\",\"path\":\"/data\"}]}"
	for i in $(seq -w 1 9); do
		app "vol-$i" volume 1 0 "{\"name\":\"vol-$i\",\"image\":\"$RD\",\"command\":[\"redis-server\",\"--appendonly\",\"yes\"],\"volumes\":[{\"name\":\"data\",\"path\":\"/data\"}]}"
	done
	for i in $(seq -w 1 30); do
		app "hc-$i" health 5 1 "{\"name\":\"hc-$i\",\"image\":\"$NG\",\"replicas\":5,\"health\":{\"http\":{\"path\":\"/\",\"port\":80}}}"
	done
	for i in $(seq -w 1 10); do
		app "rt-$i" routed 3 1 "{\"name\":\"rt-$i\",\"image\":\"$NG\",\"replicas\":3,\"routes\":[{\"host\":\"rt-$i.scale.test\",\"port\":80}]}"
	done
	for i in $(seq -w 1 16); do
		n=9
		[ "$i" = 16 ] && n=8
		app "bb-$i" plain "$n" 1 "{\"name\":\"bb-$i\",\"image\":\"$BB\",\"replicas\":$n,\"command\":[\"sleep\",\"86400\"]}"
	done
	if [ "$SCALE_MIX" = 2 ]; then
		for i in $(seq -f %02g 1 5); do
			app "ng-$i" plain 9 1 "{\"name\":\"ng-$i\",\"image\":\"$NG\",\"replicas\":9}"
		done
		# /api/ of rt-01 to rt-03: nginx with a page there that names the app
		for i in 1 2 3; do
			app "pr-0$i" routed 9 1 "{\"name\":\"pr-0$i\",\"image\":\"$NG\",\"replicas\":9,\"command\":[\"sh\",\"-c\",\"mkdir -p /usr/share/nginx/html/api && echo pr-0$i >/usr/share/nginx/html/api/index.html && exec nginx -g 'daemon off;'\"],\"routes\":[{\"host\":\"rt-0$i.scale.test\",\"path\":\"/api\",\"port\":80}]}"
		done
		for i in 1 2; do
			app "gr-0$i" routed 9 1 "{\"name\":\"gr-0$i\",\"image\":\"$H2C_IMAGE\",\"replicas\":9,\"command\":[\"serve\",\"-addr\",\":50051\",\"-name\",\"gr-0$i\"],\"routes\":[{\"host\":\"h2c-0$i.scale.test\",\"port\":50051,\"protocol\":\"h2c\"}],\"health\":{\"tcp\":{\"port\":50051}}}"
		done
	else
		for i in $(seq -w 1 10); do
			app "ng-$i" plain 9 1 "{\"name\":\"ng-$i\",\"image\":\"$NG\",\"replicas\":9}"
		done
	fi
	for i in 1 2 3 4 5; do
		if [ "$i" -le 3 ]; then
			printf 'name: cmp%s\nservices:\n  cmp%s:\n    image: %s\n    command: ["sleep", "86400"]\n    deploy:\n      replicas: 4\n' "$i" "$i" "$BB"
		else
			printf 'name: cmp%s\nservices:\n  cmp%s:\n    image: %s\n    deploy:\n      replicas: 4\n' "$i" "$i" "$NG"
		fi >"$SPECS/compose/cmp$i.yaml"
		printf 'cmp%s\tcompose\t4\t1\n' "$i" >>"$SPECS/apps.tsv"
	done
	# One fixed order that mixes the kinds: by a digest of the name.
	while IFS=$'\t' read -r n _; do
		printf '%s %s\n' "$(printf '%s' "$n" | sha256sum | cut -c1-12)" "$n"
	done <"$SPECS/apps.tsv" | sort | cut -d' ' -f2 >"$SPECS/order"
	ev specs "apps=$(wc -l <"$SPECS/apps.tsv") replicas=$(awk -F'\t' '{ s += $3 } END { print s }' "$SPECS/apps.tsv") portless=$(awk -F'\t' '$4 == 1' "$SPECS/apps.tsv" | wc -l) $(cut -f2 "$SPECS/apps.tsv" | sort | uniq -c | awk '{ printf "%s=%s ", $2, $1 }')"
}

kind_of() { awk -F'\t' -v n="$1" '$1 == n { print $2 }' "$SPECS/apps.tsv"; }
total_replicas() { awk -F'\t' '{ s += $3 } END { print s }' "$SPECS/apps.tsv"; }

# ---------------------------------------------------------------- the server side

# put.sh, on the server: one request per argument. A JSON file is PUT to
# /v1/apps/<name>, a YAML file goes through hivemind compose deploy, and a
# line "scale NAME N", "delete NAME" or "env NAME" is that request. One
# output line per request: "<name> <kind> <code> <seconds>".
read -r -d '' REMOTE_PUT <<'EOF' || true
#!/bin/bash
D=/var/tmp/hivemind-scale
a=$1
req() { curl -sS -K $D/curlrc -o "$D/out/$1.$2" -w '%{http_code} %{time_total}' "${@:3}" 2>>"$D/out/curl.err" || echo "000 0"; }
case "$a" in
*.json)
	n=$(basename "$a" .json)
	echo "$n put $(req "$n" put -X PUT -H 'Content-Type: application/json' --data-binary @"$a" "http://127.0.0.1:4483/v1/apps/$n")"
	;;
*.yaml)
	n=$(basename "$a" .yaml)
	t0=$(date +%s%N)
	if /usr/local/bin/hivemind compose deploy "$a" >"$D/out/$n.compose" 2>&1; then c=200; else c=fail; fi
	t1=$(date +%s%N)
	echo "$n compose $c $(awk -v d=$((t1 - t0)) 'BEGIN { printf "%.6f", d / 1e9 }')"
	;;
scale:*)
	set -- ${a//:/ }
	echo "$2 scale $(req "$2" scale -X POST -H 'Content-Type: application/json' --data-binary "{\"replicas\":$3}" "http://127.0.0.1:4483/v1/apps/$2/scale")"
	;;
delete:*)
	n=${a#delete:}
	echo "$n delete $(req "$n" delete -X DELETE "http://127.0.0.1:4483/v1/apps/$n")"
	;;
esac
EOF

# server_prepare: the token in a root-only curl config, put.sh and the specs on the server.
server_prepare() {
	on "$SERVER" "sudo install -d -m 0700 $RDIR $RDIR/out $RDIR/specs && sudo sh -c 'umask 077; printf \"header = \\\"Authorization: Bearer %s\\\"\\n\" \"\$(cat $DATA/admin.token)\" > $RDIR/curlrc'" || die "preparing $RDIR on $SERVER"
	on "$SERVER" "sudo tee $RDIR/put.sh >/dev/null && sudo chmod 0700 $RDIR/put.sh" <<<"$REMOTE_PUT" || die "put.sh on $SERVER"
	tar -C "$SPECS" -cf - json compose | SSH_TIMEOUT=60 on "$SERVER" "sudo tar -C $RDIR/specs -xf -" || die "copying the specs"
}

# requests PHASE N LIST: run put.sh on the server for each line of LIST, N at
# once, and log one req line for each. Prints the output file.
requests() {
	local phase=$1 n=$2 list=$3 out=$SOAK_DIR/req-$1.txt
	SSH_TIMEOUT=1800 on "$SERVER" "sudo xargs -P $n -n 1 $RDIR/put.sh" <"$list" >"$out" 2>>"$SOAK_DIR/req-$phase.err"
	while read -r name kind code secs; do
		ev req "phase=$phase name=$name kind=$kind code=$code s=$secs"
	done <"$out"
}

# ---------------------------------------------------------------- sampling

TOKEN=
token() { [ -n "$TOKEN" ] || TOKEN=$(on "$SERVER" "sudo cat $DATA/admin.token") || die "no admin token"; }

# The volume apps, as a jq array.
VOL_JSON=$(printf '%s\n' $VOLUME_APPS | jq -R . | jq -sc .)

# sample PHASE [nodes]: one GET /v1/status (and, with "nodes", GET
# /v1/nodes) on the server. Logs an ss line, keeps the replies in
# $SOAK_DIR/status.json and nodes.json, and sets S_OK, S_DESIRED, S_RUNNING,
# S_READY, S_BELOW, S_ROLLOUTS, S_ROLLING (the apps with a rollout) and S_LOST.
sample() {
	local phase=$1 want_nodes=${2:-} out st nd sline nline
	token
	out=$(SSH_TIMEOUT=25 on "$SERVER" "read -r h; get() { printf '%s\n' \"\$h\" | curl -sS -K - -m 20 -w '\n#%{http_code} %{time_total} %{size_download}\n' http://127.0.0.1:4483/v1/\$1 2>/dev/null || printf '\n#000 0 0\n'; }; get status; [ -z '$want_nodes' ] || get nodes" \
		<<<"header = \"Authorization: Bearer $TOKEN\"" 2>/dev/null) || out=$'\n#000 0 0'
	st=$(awk '/^#/ { exit } { print }' <<<"$out")
	sline=$(awk '/^#/ { print substr($0, 2); exit }' <<<"$out")
	S_OK=0 S_DESIRED=0 S_RUNNING=0 S_READY=0 S_BELOW=- S_ROLLOUTS=0 S_ROLLING='' S_LOST=''
	if [ "${sline%% *}" = 200 ] && printf '%s\n' "$st" >"$SOAK_DIR/status.json" &&
		read -r S_APPS S_DESIRED S_RUNNING S_READY S_PENDING S_NONODE S_BELOW S_NOTREADY S_ROLLOUTS S_PAUSED S_ROLLING < <(jq -r --argjson vol "$VOL_JSON" '
			[length, (map(.desired) | add // 0), (map(.running) | add // 0), (map(.ready) | add // 0),
			 ([.[].replicas[] | select(.desired == "running" and .state == "pending")] | length),
			 ([.[].replicas[] | select(.desired == "running" and (.node_id // "") == "")] | length),
			 ([.[] | select(.running < .desired) | .name] | if length == 0 then "-" else join(",") end),
			 ([.[] | select(.ready < .desired) | .name] | length),
			 ([.[] | select(.rollout)] | length),
			 ([.[] | select(.rollout.state == "paused") | .name] | if length == 0 then "-" else join(",") end),
			 ([.[] | select(.rollout) | .name] | if length == 0 then "-" else join(",") end)] | @tsv' "$SOAK_DIR/status.json" | tr '\t' ' '); then
		S_OK=1
	fi
	if [ -n "$want_nodes" ]; then
		nd=$(awk 'f && /^#/ { exit } f { print } /^#/ { f = 1 }' <<<"$out")
		nline=$(awk '/^#/ { n++; if (n == 2) { print substr($0, 2); exit } }' <<<"$out")
		if [ "${nline%% *}" = 200 ] && printf '%s\n' "$nd" >"$SOAK_DIR/nodes.json"; then
			S_LOST=$(jq -r '[.[] | select(.state != "ready") | .name + ":" + .state] | join(",")' "$SOAK_DIR/nodes.json")
		fi
	fi
	local code secs bytes
	read -r code secs bytes <<<"$sline"
	if [ "$S_OK" = 1 ]; then
		ev ss "phase=$phase code=$code s=$secs bytes=$bytes apps=$S_APPS desired=$S_DESIRED running=$S_RUNNING ready=$S_READY pending=$S_PENDING nonode=$S_NONODE notready=$S_NOTREADY rollouts=$S_ROLLOUTS paused=$S_PAUSED below=$(cut -c1-300 <<<"$S_BELOW")${want_nodes:+ notready_nodes=${S_LOST:--}}"
	else
		ev ss "phase=$phase code=${code:-000} s=${secs:-0} fail"
	fi
}

# containers_on_nodes: "name count" for each node, the hivemind containers
# Docker has, exited ones included; "name -" for a node that does not answer.
containers_on_nodes() {
	local i n pids=()
	for i in "${!NAMES[@]}"; do
		n=${NAMES[$i]}
		(c=$(SSH_CT=1 on "$n" "sudo docker ps -aq --filter label=hivemind.app | wc -l" 2>/dev/null) || c=-
			echo "$n $c" >"$SOAK_DIR/cc.$i") &
		pids+=($!)
	done
	wait "${pids[@]}"
	for i in "${!NAMES[@]}"; do cat "$SOAK_DIR/cc.$i"; rm -f "$SOAK_DIR/cc.$i"; done
}

# tick T0: sleep to the next whole second after T0 (ms).
tick() {
	local d=$((1000 - ($(now_ms) - $1)))
	[ "$d" -le 0 ] || sleep "$((d / 1000)).$(printf '%03d' $((d % 1000)))"
}

# first PHASE KEY TEXT: an event line the first time KEY is seen in PHASE.
declare -A SEEN
first() {
	[ -n "${SEEN[$1|$2]:-}" ] && return 0
	SEEN[$1|$2]=1
	ev event "$1 $2 $3"
}

# below_track PHASE: log each change of the set of apps below their count.
BELOW_LAST=-
below_track() {
	[ "$S_OK" = 1 ] || return 0
	[ "$S_BELOW" = "$BELOW_LAST" ] && return 0
	ev below "phase=$1 apps=$S_BELOW"
	BELOW_LAST=$S_BELOW
}

# ---------------------------------------------------------------- phases

pull() {
	local n pids=()
	ev mark "pull begin"
	for n in "${NAMES[@]}"; do
		(t0=$(now_ms)
			SSH_TIMEOUT=300 on "$n" "for i in $NG $BB $RD; do sudo docker pull -q \$i >/dev/null || exit 1; done" && r=ok || r=fail
			if [ "$SCALE_MIX" = 2 ]; then import_h2c "$n" || r=fail; fi
			ev event "pull $n $r ms=$(($(now_ms) - t0))") &
		pids+=($!)
	done
	wait "${pids[@]}"
	ev mark "pull end"
}

# wait_running PHASE SECS WANT: sample until WANT replicas are wanted and run,
# and every node's containers number WANT; events for running and ready.
wait_running() {
	local phase=$1 secs=$2 want=$3 end t c k=0 total
	end=$(($(date +%s) + secs))
	while [ "$(date +%s)" -lt "$end" ]; do
		t=$(now_ms)
		sample "$phase"
		below_track "$phase"
		if [ "$S_OK" = 1 ] && [ "$S_DESIRED" = "$want" ]; then
			[ "$S_RUNNING" = "$want" ] && first "$phase" all-running "running=$S_RUNNING"
			[ "$S_READY" = "$want" ] && first "$phase" all-ready "ready=$S_READY"
			if [ "$S_RUNNING" = "$want" ] && [ $((k % 3)) -eq 0 ]; then
				total=$(containers_on_nodes | awk '$2 == "-" { bad = 1 } { s += $2 } END { print (bad ? "-" : s) }')
				ev containers "phase=$phase total=$total"
				if [ "$total" = "$want" ]; then
					first "$phase" containers-match "total=$total"
					[ "$S_READY" = "$want" ] && return 0
				fi
			fi
		fi
		k=$((k + 1))
		tick "$t"
	done
	ev event "$phase timeout running=$S_RUNNING ready=$S_READY desired=$S_DESIRED"
	return 1
}

deploy() {
	local mode=$1 n=${2:-1} phase list want rpid
	[ "$mode" = seq ] && n=1
	phase=deploy-$mode
	want=$(total_replicas)
	server_prepare
	list=$SOAK_DIR/list-$phase
	while read -r a; do
		case "$(kind_of "$a")" in
		compose) echo "$RDIR/specs/compose/$a.yaml" ;;
		*) echo "$RDIR/specs/json/$a.json" ;;
		esac
	done <"$SPECS/order" >"$list"
	ev mark "$phase begin apps=$(wc -l <"$list") replicas=$want concurrency=$n"
	requests "$phase" "$n" "$list" &
	rpid=$!
	wait_running "$phase" 1800 "$want" &
	local wpid=$!
	wait "$rpid"
	ev event "$phase requests-done"
	wait "$wpid"
	ev mark "$phase end"
}

delete_all() {
	local phase=delete list=$SOAK_DIR/list-delete end t total
	server_prepare
	sed 's/^/delete:/' "$SPECS/order" >"$list"
	ev mark "$phase begin"
	requests "$phase" 1 "$list"
	ev event "$phase requests-done"
	end=$(($(date +%s) + 1800))
	while [ "$(date +%s)" -lt "$end" ]; do
		t=$(now_ms)
		sample "$phase"
		[ "$S_OK" = 1 ] && [ "$S_APPS" = 0 ] && first "$phase" status-empty ""
		total=$(containers_on_nodes | awk '$2 == "-" { bad = 1 } { s += $2 } END { print (bad ? "-" : s) }')
		ev containers "phase=$phase total=$total"
		[ "$total" = 0 ] && [ "$S_OK" = 1 ] && [ "$S_APPS" = 0 ] && { first "$phase" no-containers ""; break; }
		tick "$t"
	done
	ev mark "$phase end"
}

kv_write() {
	local st id n val
	st=$(hm "$SERVER" status --json) || die "no status"
	id=$(jq -r '.[] | select(.name == "kv") | .replicas[0].node_id // ""' <<<"$st")
	n=$(name_of_id "$id") || die "kv has no node"
	val="written-$(date -u +%FT%TZ)"
	[ "$(kv_set "$n" "$val")" = "+OK" ] || die "could not write the kv value on $n"
	echo "$val" >"$SOAK_DIR/kv-value"
	ev kv "write node=$n key=soak:canary value=$val"
}

sample_for() {
	local phase=$1 secs=$2 end t
	end=$(($(date +%s) + secs))
	ev mark "$phase begin"
	while [ "$(date +%s)" -lt "$end" ]; do
		t=$(now_ms)
		sample "$phase"
		below_track "$phase"
		tick "$t"
	done
	ev mark "$phase end"
}

# ids_of NAMES...: the node IDs, as a jq array.
ids_of() {
	local n
	for n in "$@"; do id_of "$n"; done | jq -R . | jq -sc .
}

# off_count IDS: the replicas that should run, have no volume, and are not
# running on a node outside IDS.
off_count() {
	jq -r --argjson ids "$1" --argjson vol "$VOL_JSON" '[.[] | .name as $a | select(($vol | index($a)) == null) | .replicas[]
		| select(.desired == "running") | select((.state != "running") or ((.node_id // "") as $n | $ids | index($n) != null))] | length' "$SOAK_DIR/status.json"
}

lose() {
	local phase=lose ids end t k=0 off lostn ns
	ids=$(ids_of "$@")
	ev mark "$phase begin targets=$* ids=$ids"
	end=$(($(date +%s) + 900))
	while [ "$(date +%s)" -lt "$end" ]; do
		t=$(now_ms)
		sample "$phase" nodes
		below_track "$phase"
		for n in "$@"; do port22 "$n" || first "$phase" "$n-port22-down" ""; done
		if [ "$S_OK" = 1 ]; then
			lostn=$(jq -r --argjson ids "$ids" '[.[] | select((.id as $i | $ids | index($i)) != null and .state == "lost")] | length' "$SOAK_DIR/nodes.json" 2>/dev/null)
			for n in "$@"; do
				ns=$(jq -r --arg i "$(id_of "$n")" '.[] | select(.id == $i) | .state' "$SOAK_DIR/nodes.json" 2>/dev/null)
				[ "$ns" = lost ] && first "$phase" "$n-lost" ""
			done
			off=$(off_count "$ids")
			ev lose "lost=$lostn off=$off running=$S_RUNNING desired=$S_DESIRED"
			if [ "$lostn" = "$#" ] && [ "$off" = 0 ]; then
				first "$phase" replicas-elsewhere "running=$S_RUNNING desired=$S_DESIRED"
				break
			fi
		fi
		k=$((k + 1))
		tick "$t"
	done
	ev mark "$phase end"
}

# old_left FILE IDS: containers on the target nodes (C rows) whose replica the server
# now assigns to another node, or no longer has.
old_left() {
	awk -F'\t' -v names=" $2 " '
		$1 == "N" { id[$3] = $2; next }
		$1 == "R" { at[$2 "|" $3] = $4; next }
		$1 == "C" && index(names, " " $2 " ") > 0 { k = $6 "|" $7; if (!(k in at) || at[k] != id[$2]) n++ }
		END { print n + 0 }' "$1"
}

return_nodes() {
	local phase=return end t f=$SOAK_DIR/return.tsv n ready d old dupc conv
	ev mark "$phase begin targets=$*"
	end=$(($(date +%s) + 900))
	while [ "$(date +%s)" -lt "$end" ]; do
		t=$(now_ms)
		collect "$f" nodes
		if api_ok "$f"; then
			ready=0
			for n in "$@"; do
				[ "$(node_state "$f" "$(id_of "$n")")" = ready ] && { ready=$((ready + 1)); first "$phase" "$n-ready" ""; }
			done
			d=$(dups "$f" | tr '\n' ';')
			dupc=$(dups "$f" | wc -l | tr -d ' ')
			old=$(old_left "$f" "$*")
			if check_converged "$f" >/dev/null; then conv=yes; else conv=no; fi
			[ -z "$d" ] || ev dup "phase=$phase $d"
			ev return "ready=$ready old=$old dups=$dupc converged=$conv running=$(running "$f") unreachable=$(unreachable "$f")"
			[ "$old" = 0 ] && [ "$ready" = "$#" ] && first "$phase" old-containers-gone ""
			if [ "$ready" = "$#" ] && [ "$old" = 0 ] && [ "$dupc" = 0 ] && [ "$conv" = yes ]; then
				first "$phase" converged ""
				break
			fi
		fi
		tick "$t"
	done
	ev mark "$phase end"
}

# ROLL_APPS in the environment replaces the list.
ROLL_APPS=${ROLL_APPS:-"hc-01 hc-02 hc-03 hc-04 hc-05 hc-06 hc-07 hc-08 hc-09 hc-10 hc-11 hc-12 hc-13 hc-14 hc-15
hc-16 hc-17 hc-18 hc-19 hc-20 hc-21 hc-22 hc-23 hc-24 hc-25 hc-26 hc-27 hc-28 hc-29 hc-30
bb-01 bb-02 bb-03 bb-04 bb-05 bb-06 bb-07 bb-08 bb-09 bb-10 bb-11 bb-12 bb-13 bb-14 bb-15 bb-16
ng-01 ng-02 ng-03 ng-04"}

rollout() {
	local n=${1:-50} phase=${2:-rollout} list val apps a end t prev=- cur rpid npaused
	list=$SOAK_DIR/list-$phase
	val=$(date -u +%H%M%S)
	apps=$(printf '%s\n' $ROLL_APPS | head -n "$n")
	mkdir -p "$SPECS/json-roll"
	for a in $apps; do
		jq -c --arg v "$val" '.env = ((.env // {}) + {SCALE_ROLL: $v})' "$SPECS/json/$a.json" >"$SPECS/json-roll/$a.json"
	done
	tar -C "$SPECS" -cf - json-roll | on "$SERVER" "sudo tar -C $RDIR/specs -xf -" || die "copying the specs"
	for a in $apps; do echo "$RDIR/specs/json-roll/$a.json"; done >"$list"
	ev mark "$phase begin apps=$n value=$val"
	sample "$phase"
	requests "$phase" "$n" "$list" &
	rpid=$!
	end=$(($(date +%s) + 1800))
	while [ "$(date +%s)" -lt "$end" ]; do
		t=$(now_ms)
		sample "$phase"
		below_track "$phase"
		if [ "$S_OK" = 1 ]; then
			cur=$S_ROLLING
			[ "$cur" = "$prev" ] || ev rolling "phase=$phase n=$S_ROLLOUTS apps=$cur"
			prev=$cur
			[ "$S_ROLLOUTS" -gt 0 ] && first "$phase" rollouts-seen "n=$S_ROLLOUTS"
			npaused=0
			[ "$S_PAUSED" != - ] && npaused=$(tr ',' '\n' <<<"$S_PAUSED" | wc -l | tr -d ' ')
			[ "$S_PAUSED" != - ] && first "$phase" "paused-$npaused" "$S_PAUSED"
			# A paused rollout stays paused until another deploy: stop once
			# every rollout left is paused.
			if [ "$S_ROLLOUTS" -gt 0 ] && [ "$npaused" = "$S_ROLLOUTS" ] && ! kill -0 "$rpid" 2>/dev/null; then
				first "$phase" all-left-paused "n=$npaused reasons=$(jq -r '[.[] | select(.rollout.state == "paused") | .rollout.reason] | unique | join(";")' "$SOAK_DIR/status.json")"
				break
			fi
			if [ -n "${SEEN[$phase|rollouts-seen]:-}" ] && [ "$S_ROLLOUTS" = 0 ] && ! kill -0 "$rpid" 2>/dev/null; then
				# every app at the new generation
				if [ "$(jq -r --arg v "$val" '[.[] | select(.replicas | any(.desired == "running" and .state != "running"))] | length' "$SOAK_DIR/status.json")" = 0 ]; then
					first "$phase" all-done "running=$S_RUNNING desired=$S_DESIRED"
					break
				fi
			fi
		fi
		tick "$t"
	done
	wait "$rpid"
	ev mark "$phase end"
}

# busiest: the worker with the most replicas, from GET /v1/nodes.
busiest() {
	sample busiest nodes >/dev/null
	jq -r '[.[] | select(.server | not)] | max_by(.replicas) | .id' "$SOAK_DIR/nodes.json" | while read -r id; do name_of_id "$id"; done
}

drain() {
	local T=${1:-$(busiest)} phase=drain id end t dpid out=$SOAK_DIR/drain.out
	id=$(id_of "$T")
	sample "$phase" nodes
	ev mark "$phase begin target=$T id=$id replicas=$(jq -r --arg i "$id" '.[] | select(.id == $i) | .replicas' "$SOAK_DIR/nodes.json")"
	(t0=$(now_ms)
		SSH_TIMEOUT=1200 on "$SERVER" "sudo $HM node drain $id --deadline 10m" >"$out" 2>&1
		ev event "$phase command-returned rc=$? ms=$(($(now_ms) - t0))") &
	dpid=$!
	end=$(($(date +%s) + 1200))
	while [ "$(date +%s)" -lt "$end" ] && kill -0 "$dpid" 2>/dev/null; do
		t=$(now_ms)
		sample "$phase"
		below_track "$phase"
		tick "$t"
	done
	wait "$dpid"
	# Read the drain; "node drain" again would start a new one on a finished drain.
	token
	on "$SERVER" "read -r h; printf '%s\n' \"\$h\" | curl -sS -K - http://127.0.0.1:4483/v1/nodes/$id/drain" \
		<<<"header = \"Authorization: Bearer $TOKEN\"" >"$SOAK_DIR/drain.json" 2>&1 || true
	ev drain "result $(tr '\n' ' ' <"$SOAK_DIR/drain.json" | cut -c1-1500)"
	ev drain "output $(tail -n 15 "$out" | tr '\n' ';' | cut -c1-1500)"
	ev mark "$phase end"
}

remove_node() {
	local T=$1 phase=remove id t0 out end t ids
	id=$(id_of "$T")
	ids=$(ids_of "$T")
	ev mark "$phase begin target=$T id=$id"
	t0=$(now_ms)
	token
	out=$(on "$SERVER" "read -r h; printf '%s\n' \"\$h\" | curl -sS -K - -X DELETE -w '\n#%{http_code} %{time_total}' http://127.0.0.1:4483/v1/nodes/$id" <<<"header = \"Authorization: Bearer $TOKEN\"" 2>&1)
	ev req "phase=$phase name=$id kind=remove code=$(sed -n 's/^#//p' <<<"$out" | tr ' ' '\n' | head -n 1) s=$(sed -n 's/^#//p' <<<"$out" | cut -d' ' -f2) reply=$(head -n 1 <<<"$out" | tr -d ' ')"
	end=$(($(date +%s) + 600))
	while [ "$(date +%s)" -lt "$end" ]; do
		t=$(now_ms)
		sample "$phase" nodes
		below_track "$phase"
		if [ "$S_OK" = 1 ] && [ "$(off_count "$ids")" = 0 ]; then
			first "$phase" replicas-elsewhere "running=$S_RUNNING desired=$S_DESIRED"
			break
		fi
		tick "$t"
	done
	ev mark "$phase end"
}

scale_apps() {
	local f=${1:?factor} phase list want
	phase=scale-x$f
	list=$SOAK_DIR/list-$phase
	awk -F'\t' -v f="$f" '$4 == 1 { printf "scale:%s:%d\n", $1, $3 * f }' "$SPECS/apps.tsv" >"$list"
	want=$(awk -F'\t' -v f="$f" '{ s += ($4 == 1 ? $3 * f : $3) } END { print s }' "$SPECS/apps.tsv")
	server_prepare
	ev mark "$phase begin apps=$(wc -l <"$list") want=$want"
	requests "$phase" 1 "$list" &
	local rpid=$!
	wait_running "$phase" 1800 "$want"
	wait "$rpid"
	ev mark "$phase end"
}

restart_server() {
	local phase=restart-server pre=$SOAK_DIR/pre-restart.tsv f=$SOAK_DIR/restart.tsv end t api=down k ready changed
	snapshot "$pre" || die "no status before the restart"
	ev mark "$phase begin running=$(running "$pre")"
	ev event "$phase restart-command"
	on "$SERVER" "sudo systemctl restart hivemind" || die "restart"
	ev event "$phase restart-returned"
	end=$(($(date +%s) + 600))
	while [ "$(date +%s)" -lt "$end" ]; do
		t=$(now_ms)
		if [ "$api" = down ] && on "$SERVER" "curl -fsS -m 1 http://127.0.0.1:4483/healthz" >/dev/null 2>&1; then
			api=up
			first "$phase" api-up ""
		fi
		if [ "$api" = up ]; then
			collect "$f" nodes
			if api_ok "$f"; then
				ready=$(awk -F'\t' '$1 == "L" && $3 == "ready"' "$f" | wc -l | tr -d ' ')
				ev restart "nodes_ready=$ready running=$(running "$f")"
				[ "$ready" = "${#NAMES[@]}" ] && first "$phase" all-nodes-ready ""
				v=$(running "$f")
				[ "${v%/*}" = "${v#*/}" ] && first "$phase" all-running "$v"
				if [ "$ready" = "${#NAMES[@]}" ] && check_converged "$f" >/dev/null; then
					first "$phase" converged ""
					break
				fi
			fi
		fi
		tick "$t"
	done
	# containers that changed: ID or start time
	changed=$(awk -F'\t' 'FILENAME == ARGV[1] { if ($1 == "C" && $4 == "running") pre[$2 "|" $3 "|" $5] = 1; next }
		$1 == "C" && $4 == "running" { if (!(($2 "|" $3 "|" $5) in pre)) n++; now[$2 "|" $3 "|" $5] = 1 }
		END { for (k in pre) if (!(k in now)) g++; printf "new=%d gone=%d", n, g }' "$pre" "$f")
	ev event "$phase containers $changed"
	ev mark "$phase end"
}

lat() {
	local n=${1:-20} out r
	token
	ev mark "lat begin n=$n"
	out=$(SSH_TIMEOUT=600 on "$SERVER" "read -r h; for r in status nodes apps routes; do for i in \$(seq 1 $n); do printf '%s\n' \"\$h\" | curl -sS -K - -o /dev/null -m 20 -w \"\$r %{http_code} %{time_total} %{size_download}\n\" http://127.0.0.1:4483/v1/\$r || echo \"\$r 000 0 0\"; done; done" <<<"header = \"Authorization: Bearer $TOKEN\"")
	while read -r r c s b; do ev lat "code=$c s=$s route=/v1/$r bytes=$b"; done <<<"$out"
	ev mark "lat end"
	awk '{ k = $1; v[k] = v[k] " " $3; b[k] = $4 } END { for (k in v) { n = split(v[k], x, " "); asort(x); printf "%s n=%d p50=%.1fms p99=%.1fms max=%.1fms bytes=%s\n", k, n, x[int(n * 0.5 + 0.5)] * 1000, x[int(n * 0.99 + 0.999)] * 1000, x[n] * 1000, b[k] } }' <<<"$out"
}

routes() {
	local out
	token
	out=$(on "$SERVER" "read -r h; printf '%s\n' \"\$h\" | curl -sS -K - http://127.0.0.1:4483/v1/routes" <<<"header = \"Authorization: Bearer $TOKEN\"") || die "GET /v1/routes"
	jq -r '.routes[] | "\(.host)\(if (.path // "/") == "/" then "" else .path end) app=\(.app) port=\(.port) protocol=\(.protocol // "http") endpoints=\(.endpoints | length) \(.endpoints | join(","))"' <<<"$out" | while read -r l; do ev routes "$l"; echo "$l"; done
}

# ---------------------------------------------------------------- routers (SCALE_MIX=2, SOAK_ROUTES=1)
#
# The clients run on the outside machine ($OUTSIDE_FILE), which trusts
# Pebble's root (outside.sh root). The h2c client is test/h2ctest, copied there
# from $SOAK_DIR/h2ctest.

# routed_hosts: the hosts of the routed apps, one per line.
routed_hosts() { jq -r '.routes[]?.host' "$SPECS"/json/*.json | sort -u; }

# served: through every router, a GET of / on every routed host but the h2c
# ones (200), of /api/ on the hosts with a path route (200, the page names
# pr-0N), and a unary call to each h2c host (from gr-0N). One "served" line per
# router and a summary; fails when one is wrong.
served() {
	local cmd="" ip h i out bad
	out_h2c_bin
	for ip in "${IPS[@]}"; do
		for h in $(routed_hosts | grep -v '^h2c-'); do
			cmd+="printf '%s %s ' $ip $h/; curl -s -o /dev/null -m 5 --cacert /var/lib/soak-client/root.pem --resolve $h:443:$ip -w '%{http_code}\n' https://$h/; "
		done
		for i in 1 2 3; do
			h=rt-0$i.scale.test
			cmd+="printf '%s %s ' $ip $h/api/; curl -s -m 5 --cacert /var/lib/soak-client/root.pem --resolve $h:443:$ip -w ' %{http_code}\n' https://$h/api/ | tr '\n' ' '; echo; "
		done
		for i in 1 2; do
			h=h2c-0$i.scale.test
			cmd+="printf '%s %s ' $ip $h; timeout 10 /var/lib/soak-client/h2ctest call -url https://$h:443 -connect $ip:443 -ca /var/lib/soak-client/root.pem unary 2>&1 | tr '\n' ' '; echo \" rc=\${PIPESTATUS[0]}\"; "
		done
	done
	out=$(SSH_TIMEOUT=300 on "$OUT_NAME" "$cmd") || die "served: the checks did not run on $OUT_NAME"
	bad=$(awk '($2 ~ /\/$/ && $2 !~ /\/api\/$/ && $3 != 200) || ($2 ~ /\/api\/$/ && !($3 ~ /^pr-0/ && $4 == 200)) || ($2 ~ /^h2c-/ && $NF != "rc=0") { n++ } END { print n + 0 }' <<<"$out")
	while read -r l; do ev served "$l"; done <<<"$out"
	ev served "summary checks=$(wc -l <<<"$out") bad=$bad"
	echo "checks=$(wc -l <<<"$out") bad=$bad"
	[ "$bad" = 0 ]
}

# out_h2c_bin: test/h2ctest on the outside machine.
out_h2c_bin() {
	on "$OUT_NAME" "test -x /var/lib/soak-client/h2ctest" 2>/dev/null && return 0
	[ -s "$SOAK_DIR/h2ctest" ] || die "no $SOAK_DIR/h2ctest"
	SSH_TIMEOUT=60 on "$OUT_NAME" "sudo install -d -m 0755 /var/lib/soak-client && sudo tee /var/lib/soak-client/h2ctest >/dev/null && sudo chmod 755 /var/lib/soak-client/h2ctest" <"$SOAK_DIR/h2ctest" || die "h2ctest to $OUT_NAME"
}

# On the outside machine: a unary call to h2c-01.scale.test through each
# router in turn, then a 1 s pause, until h2c.stop exists. One line per call:
# "<Unix s.ms> <router> <rc> <seconds> <reply or error>".
read -r -d '' H2C_LOOP <<'EOF' || true
#!/bin/bash
d=/var/lib/soak-client
while [ ! -e $d/h2c.stop ]; do
	for ip in $(cat $d/routers); do
		t0=$(date +%s%N)
		r=$(timeout 10 $d/h2ctest call -url https://h2c-01.scale.test:443 -connect $ip:443 -ca $d/root.pem unary 2>&1)
		rc=$?
		r=$(printf '%s' "$r" | tr '\n' ' ')
		t1=$(date +%s%N)
		echo "$((t0 / 1000000)) $ip $rc $(((t1 - t0) / 1000000)) $r"
	done >>$d/h2c.log
	sleep 1
done
EOF

h2c_start() {
	out_h2c_bin
	on "$OUT_NAME" "sudo tee /var/lib/soak-client/h2c-loop.sh >/dev/null && sudo chmod 755 /var/lib/soak-client/h2c-loop.sh" <<<"$H2C_LOOP"
	on "$OUT_NAME" "sudo rm -f /var/lib/soak-client/h2c.stop; sudo setsid nohup /var/lib/soak-client/h2c-loop.sh >/dev/null 2>&1 </dev/null &"
	ev client "h2c loop start"
}
h2c_stop() {
	on "$OUT_NAME" "sudo touch /var/lib/soak-client/h2c.stop"
	ev client "h2c loop stop"
}
# h2c_status: calls and failures so far, per router.
h2c_status() {
	on "$OUT_NAME" "awk '{ n[\$2]++; if (\$3 != 0) f[\$2]++ } END { for (k in n) printf \"%s calls=%d failed=%d\n\", k, n[k], f[k] }' /var/lib/soak-client/h2c.log | sort"
}

# metrics [N]: N reads of the server's /metrics, timed: bytes, lines,
# go_goroutines, the router series and the nodes they name. Prints a summary.
metrics() {
	local n=${1:-5} out
	token
	out=$(on "$SERVER" "read -r h; for i in \$(seq 1 $n); do printf '%s\n' \"\$h\" | curl -sS -K - -m 20 -o \$HOME/hm-metrics.txt -w '%{http_code} %{time_total} %{size_download} ' http://127.0.0.1:4483/metrics; awk '\$1 == \"go_goroutines\" { g = \$2 } /^hivemind_router_/ { r++; if (match(\$0, /node=\"[^\"]+\"/)) nd[substr(\$0, RSTART, RLENGTH)] = 1 } /^[a-z]/ { l++ } END { c = 0; for (k in nd) c++; printf \"lines=%d goroutines=%s router_series=%d router_nodes=%d\n\", l, g, r, c }' \$HOME/hm-metrics.txt; done" <<<"header = \"Authorization: Bearer $TOKEN\"") || die "GET /metrics"
	while read -r c s b rest; do ev metrics "code=$c s=$s bytes=$b $rest"; done <<<"$out"
	on "$SERVER" "cat \$HOME/hm-metrics.txt" >"$SOAK_DIR/metrics-$(date -u +%H%M%S).txt" 2>/dev/null || true
	awk '{ print $1, $2 * 1000 "ms", $3 "B", $4, $5, $6, $7 }' <<<"$out"
}

# perf-start SECS [NAME]: perf record of the hivemind process on NAME (the
# server) for SECS, in the background on that machine, at 99 Hz with call
# graphs. perf-fetch [NAME] copies the data and the binary here; perf-report
# FILE reads it in a throwaway container on this machine.
perf_start() {
	local secs=${1:?seconds} n=${2:-$SERVER}
	on "$n" "command -v perf >/dev/null || sudo dnf install -y -q perf >/dev/null 2>&1" || die "no perf on $n"
	on "$n" "pid=\$(systemctl show -p MainPID --value hivemind.service); sudo rm -f /var/tmp/hm-perf.data; sudo setsid nohup perf record -F 99 -g -p \$pid -o /var/tmp/hm-perf.data -- sleep $secs >/var/tmp/hm-perf.log 2>&1 </dev/null &" || die "perf record on $n"
	ev perf "start node=$n secs=$secs"
}
perf_fetch() {
	local n=${1:-$SERVER} dst
	dst=$SOAK_DIR/perf-$(date -u +%H%M%S)
	mkdir -p "$dst/symfs/usr/local/bin"
	SSH_TIMEOUT=300 on "$n" "sudo cat /var/tmp/hm-perf.data" >"$dst/perf.data" || die "copying perf.data"
	cp "$BIN" "$dst/symfs/usr/local/bin/hivemind"
	ev perf "fetched node=$n dir=$dst bytes=$(stat -c %s "$dst/perf.data") log=$(on "$n" "tail -n 2 /var/tmp/hm-perf.log" | tr '\n' ';')"
	echo "$dst"
}
perf_report() {
	local dir=${1:?dir}
	docker run --rm -v "$dir:/p" -w /p amazonlinux:2023 bash -c 'dnf install -y -q perf >/dev/null 2>&1 &&
		perf report -f -i perf.data --symfs /p/symfs --stdio --children --sort symbol -g none --percent-limit 0.3 >report-children.txt 2>report.err;
		perf report -f -i perf.data --symfs /p/symfs --stdio --no-children --sort symbol -g none --percent-limit 0.3 >report-self.txt 2>>report.err;
		perf script -f -i perf.data --symfs /p/symfs -F time,ip,sym --no-inline >script.txt 2>>report.err; ls -l'
}

listeners() {
	local n l bad=0
	for n in "${NAMES[@]}"; do
		l=$(on "$n" "sudo ss -ltnpH" | awk '{ print $4, $6 }')
		ev listeners "$n $(awk '{ p = "-"; if (match($2, /"[^"]+"/)) p = substr($2, RSTART + 1, RLENGTH - 2); printf "%s(%s);", $1, p }' <<<"$l" | cut -c1-3000)"
		# hivemind's ports, and every port docker-proxy holds for an app
		if awk '{ split($1, a, ":"); port = a[length(a)]; addr = substr($1, 1, length($1) - length(port) - 1) }
			(port ~ /^448[3-5]$/ || $2 ~ /docker-proxy/) && (addr == "0.0.0.0" || addr == "*" || addr == "[::]") { f = 1; print }
			END { exit !f }' <<<"$l"; then
			note "$n: a hivemind or app port on a wildcard address"
			bad=1
		fi
	done
	return $bad
}

cmd=${1:-}
shift || true
case "$cmd" in
specs) specs ;;
pull) pull ;;
deploy) deploy "$@" ;;
delete) delete_all ;;
kv-write) kv_write ;;
sample) sample_for "$@" ;;
lose) lose "$@" ;;
return) return_nodes "$@" ;;
rollout) rollout "$@" ;;
drain) drain "$@" ;;
remove) remove_node "$@" ;;
scale) scale_apps "$@" ;;
restart-server) restart_server ;;
lat) lat "$@" ;;
routes) routes ;;
served) served ;;
h2c-start) h2c_start ;;
h2c-stop) h2c_stop ;;
h2c-status) h2c_status ;;
metrics) metrics "$@" ;;
perf-start) perf_start "$@" ;;
perf-fetch) perf_fetch "$@" ;;
perf-report) perf_report "$@" ;;
listeners) listeners ;;
on) on "$@" ;;
*)
	sed -n '2,39p' "$0"
	exit 2
	;;
esac
