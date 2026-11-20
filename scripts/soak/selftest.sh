#!/usr/bin/env bash
# The harness's own checks, on canned data, without machines: the parsers of
# hivemind servers --json (ha.sh, lib.sh) and the write-load's evaluation
# (wload.sh), each case with the result it must give.
#
#   scripts/soak/selftest.sh [DIR]     DIR for the canned files (default a new temporary one)
#
# Needs bash, jq and awk, as the harness does.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
T=${1:-$(mktemp -d)}
mkdir -p "$T/wl"
printf 'n-1 10.1.0.1\nn-2 10.1.0.2\nn-3 10.1.0.3\nn-4 10.1.0.4\n' >"$T/nodes"
printf 'n-7 10.1.0.7\n' >"$T/outside"
export SOAK_DIR=$T SOAK_SERVERS=3 SOAK_API_TLS=1 SOAK_LOG=$T/selftest.log
fails=0

"$HERE/scenario.sh" ha-selftest || fails=$((fails + 1))

# expect TAG ROWS RESULT [PROBLEM-PATTERN]: the evaluation's first line says
# RESULT, and a problem line matches the pattern.
expect() {
	local out
	out=$("$HERE/wload.sh" eval "$1" "$T/wl/$2")
	if ! head -n 1 <<<"$out" | grep -q "^result $3 " || { [ -n "${4:-}" ] && ! grep -qE "$4" <<<"$out"; }; then
		echo "FAIL $1 $2: want $3 ${4:-}, got: $(tr '\n' ';' <<<"$out")"
		fails=$((fails + 1))
	else
		echo "ok   $1 $2: $(head -n 1 <<<"$out")${4:+ / $(grep -E "$4" <<<"$out" | head -n 1)}"
	fi
}

# acknowledged deploys and scales, failures during an outage, a deploy that
# failed but was applied
cat >"$T/wl/t1.log" <<'EOF'
1.0 1.1 deploy wl-t1-1 seq=1 rc=0 gen=1 seq=1 replicas=1 err=warning:_replica_0_is_pending
2.0 2.1 scale wl-t1-1 replicas=2 rc=0 gen=1 seq=1 replicas=2 err=-
3.0 3.1 deploy wl-t1-3 seq=3 rc=0 gen=1 seq=3 replicas=1 err=-
4.0 9.1 scale wl-t1-3 replicas=2 rc=1 gen=- seq=- replicas=- err=503_no_leader
10.0 10.1 deploy wl-t1-5 seq=5 rc=1 gen=- seq=- replicas=- err=503
11.0 11.1 scale wl-t1-5 replicas=2 rc=1 gen=- seq=- replicas=- err=app_"wl-t1-5"_not_found
12.0 12.1 deploy wl-t1-7 seq=7 rc=0 gen=1 seq=7 replicas=1 err=-
13.0 13.1 scale wl-t1-7 replicas=2 rc=0 gen=1 seq=7 replicas=2 err=-
EOF
printf '#method backup\nwl-t1-1 1 2 0 1\nwl-t1-3 1 1 0 3\nwl-t1-7 1 2 0 7\n' >"$T/wl/t1-good"
printf '#method backup\nwl-t1-1 1 2 0 1\nwl-t1-3 1 2 0 3\nwl-t1-5 1 2 0 5\nwl-t1-7 1 2 0 7\n' >"$T/wl/t1-unacked-applied"
printf '#method backup\nwl-t1-1 1 2 0 1\nwl-t1-3 1 1 0 3\nwl-t1-7 1 1 0 7\n' >"$T/wl/t1-lost-scale"
printf '#method backup\nwl-t1-1 1 2 0 1\nwl-t1-3 1 1 0 3\n' >"$T/wl/t1-lost-deploy"
printf '#method backup\nwl-t1-1 1 2 0 1\nwl-t1-3 1 1 0 3\nwl-t1-7 1 2 0 6\n' >"$T/wl/t1-wrong"
printf '#method backup\nwl-t1-1 1 2 0 1\nwl-t1-3 1 1 0 3\nwl-t1-7 1 2 1 7\n' >"$T/wl/t1-deleted"
expect t1 t1-good pass
expect t1 t1-unacked-applied pass
expect t1 t1-lost-scale fail 'missing scale wl-t1-7'
expect t1 t1-lost-deploy fail 'missing deploy wl-t1-7'
expect t1 t1-wrong fail 'wrong wl-t1-7'
expect t1 t1-deleted fail 'missing deploy wl-t1-7'

# a deploy acknowledged, and then the leader says the app is not there
cat >"$T/wl/t2.log" <<'EOF'
1.0 1.1 deploy wl-t2-1 seq=1 rc=0 gen=1 seq=1 replicas=1 err=-
2.0 2.1 scale wl-t2-1 replicas=2 rc=1 gen=- seq=- replicas=- err=Error:_app_"wl-t2-1"_not_found
EOF
printf '#method backup\nwl-t2-1 1 1 0 1\n' >"$T/wl/t2-there"
expect t2 t2-there fail 'lost wl-t2-1'
# a scale's reply that names another deploy
cat >"$T/wl/t4.log" <<'EOF'
1.0 1.1 deploy wl-t4-1 seq=1 rc=0 gen=1 seq=1 replicas=1 err=-
2.0 2.1 scale wl-t4-1 replicas=2 rc=0 gen=2 seq=9 replicas=2 err=-
EOF
printf '#method backup\nwl-t4-1 1 2 0 1\n' >"$T/wl/t4-rows"
expect t4 t4-rows fail 'reply scale wl-t4-1'

# the steady state: deletes as it goes; then the clean
cat >"$T/wl/t3.log" <<'EOF'
1.0 1.1 deploy wl-t3-1 seq=1 rc=0 gen=1 seq=1 replicas=1 err=-
2.0 2.1 scale wl-t3-1 replicas=2 rc=0 gen=1 seq=1 replicas=2 err=-
4.0 4.1 deploy wl-t3-4 seq=4 rc=0 gen=1 seq=4 replicas=1 err=-
5.0 5.1 scale wl-t3-4 replicas=2 rc=0 gen=1 seq=4 replicas=2 err=-
6.0 6.1 delete wl-t3-1 gone rc=0 gen=- seq=- replicas=- err=-
EOF
printf '#method backup\nwl-t3-4 1 2 0 4\n' >"$T/wl/t3-good"
printf '#method backup\nwl-t3-1 1 2 1 1\nwl-t3-4 1 2 0 4\n' >"$T/wl/t3-deleting"
printf '#method backup\nwl-t3-1 1 2 0 1\nwl-t3-4 1 2 0 4\n' >"$T/wl/t3-lost-delete"
printf '#method backup\n' >"$T/wl/t3-empty"
expect t3 t3-good pass
expect t3 t3-deleting pass
expect t3 t3-lost-delete fail 'missing delete wl-t3-1'
echo "1.0 1.1 delete wl-t3-4 gone rc=0 gen=- seq=- replicas=- err=-" >"$T/wl/t3.clean.log"
expect t3 t3-empty pass
expect t3 t3-good fail 'missing delete wl-t3-4'

# a source that could not be read is not a pass of its own (check() refuses
# "#method fail"); the evaluation still names what is missing
printf '#method fail\n' >"$T/wl/t1-fail"
expect t1 t1-fail fail 'missing deploy'

echo "selftest: $fails failed"
[ "$fails" = 0 ]
