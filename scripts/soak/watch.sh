#!/usr/bin/env bash
# The 1 s watcher, for the scenarios. Runs until killed. It logs changes only:
#   watch api up|down              GET /healthz on the server, over ssh
#   watch port22 NAME up|down      a new TCP connection to the machine's port 22
#   watch node NAME STATE          the node's state in hivemind nodes
#   watch replica APP|IDX ...      node, state, generation, container of a replica
# A poll that takes longer than a second starts the next one at once.
set -uo pipefail
. "$(dirname "$0")/lib.sh"

declare -A P22 NST REP
API=

read -r -d '' REMOTE <<EOF || true
curl -fsS -m 2 http://127.0.0.1:4483/healthz >/dev/null 2>&1 && echo api-up || echo api-down
sudo $HM nodes --json 2>/dev/null | jq -r '.[] | "N \(.id) \(.state) \(.lease_age_seconds)"'
sudo $HM status --json 2>/dev/null | jq -r '.[] | .name as \$a | .replicas[] | "R \(\$a)|\(.index) node=\(.node_id // "-") state=\(.state) gen=\(.generation) cid=\((.container_id // "-")[0:12]) reason=\((.pending_reason // "") | gsub(" "; "_"))"'
EOF

poll() {
	local i n out api line k v id nm
	for i in "${!NAMES[@]}"; do
		n=${NAMES[$i]}
		if port22 "$n"; then v=up; else v=down; fi
		[ "${P22[$n]:-}" = "$v" ] || {
			ev watch "port22 $n $v"
			P22[$n]=$v
		}
	done
	out=$(SSH_TIMEOUT=4 on "$SERVER" "$REMOTE" 2>/dev/null) || out="api-down"
	api=${out%%$'\n'*}
	[ "$api" = "$API" ] || {
		ev watch "api ${api#api-}"
		API=$api
	}
	while read -r k id v rest; do
		case "$k" in
		N)
			nm=$(name_of_id "$id")
			[ "${NST[$id]:-}" = "$v" ] || {
				ev watch "node ${nm:-$id} $v lease_age=$rest"
				NST[$id]=$v
			}
			;;
		R)
			line="$v $rest"
			# show node names instead of IDs
			nid=${v#node=}
			nm=$(name_of_id "$nid")
			[ -n "$nm" ] && line="node=$nm $rest"
			[ "${REP[$id]:-}" = "$line" ] || {
				ev watch "replica $id $line"
				REP[$id]=$line
			}
			;;
		esac
	done <<<"$out"
}

trap 'ev watch-loop "stop"; exit 0' TERM INT
ev watch-loop "start"
while :; do
	t=$(now_ms)
	poll
	d=$((1000 - ($(now_ms) - t)))
	[ "$d" -le 0 ] || sleep "$((d / 1000)).$(printf "%03d" $((d % 1000)))"
done
