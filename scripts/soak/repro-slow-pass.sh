#!/usr/bin/env bash
# Reproduction: a reconcile pass longer than the 15 s lease makes a worker
# that is up and reachable "lost", and its replicas are placed again.
#
#   SOAK_DIR=... scripts/soak/repro-slow-pass.sh
#
# The agent renews its node's lease only when a pass reports, at the end of
# the pass (internal/agent/agent.go, Reconcile). A pass removes extra
# containers one after another, each with docker stop and a 10 s grace
# period. A container whose PID 1 ignores SIGTERM, such as busybox sleep,
# takes the full 10 s. So a worker that has three such containers to remove
# spends about 30 s in one pass without renewing its lease, and the server's
# sweep marks it lost after 15 to 20 s, although nothing is wrong with it.
#
# The script deploys app slowstop (busybox sleep, 3 replicas per node), waits
# for it to run, deletes it, and watches hivemind nodes for 60 s. It exits 1
# if any worker was marked lost (the bug), 0 if none was. Then it waits for
# the cluster to converge again. Run it on a converged cluster with nothing
# else in flight; it moves other apps' replicas when it reproduces.
set -uo pipefail
. "$(dirname "$0")/lib.sh"

N=${REPLICAS:-$((${#NAMES[@]} * 3))}
converged_now() { snapshot "$SOAK_DIR/repro.tsv" && check_converged "$SOAK_DIR/repro.tsv" >/dev/null; }

ev repro "slow-pass begin replicas=$N"
hm "$SERVER" "deploy -f -" >/dev/null <<EOF || die "deploy slowstop"
{"name":"slowstop","image":"busybox:stable","replicas":$N,"command":["sleep","86400"]}
EOF
wait_for 300 converged_now || die "slowstop did not converge"
ev repro "slow-pass deployed: $(awk -F'\t' '$1 == "R" && $2 == "slowstop" { c[$4]++ } END { for (n in c) printf "%s=%d ", n, c[n] }' "$SOAK_DIR/repro.tsv")"
sleep 5
hm "$SERVER" delete slowstop >/dev/null || die "delete slowstop"
t0=$(now_ms)
ev repro "slow-pass deleted slowstop"
declare -A seen
lost=""
while [ $(($(now_ms) - t0)) -lt 60000 ]; do
	while read -r id st age; do
		[ "${seen[$id]:-}" = "$st" ] && continue
		seen[$id]=$st
		n=$(name_of_id "$id")
		ms=$(($(now_ms) - t0))
		ev repro "slow-pass $((ms / 1000)).$(((ms % 1000) / 100)) s after delete: ${n:-$id} $st lease_age=$age"
		[ "$st" = lost ] && lost="$lost ${n:-$id}"
	done < <(hm "$SERVER" nodes --json 2>/dev/null | jq -r '.[] | "\(.id) \(.state) \(.lease_age_seconds)"')
	sleep 1
done
ev repro "slow-pass result: workers marked lost:${lost:- none}"
wait_for 600 converged_now || ev repro "slow-pass: not converged 600 s later: $(check_converged "$SOAK_DIR/repro.tsv" | tr '\n' ';')"
ev repro "slow-pass end"
[ -z "$lost" ]
