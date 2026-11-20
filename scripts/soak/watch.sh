#!/usr/bin/env bash
# The 1 s watcher, for the scenarios. Runs until killed. It logs changes only:
#   watch api up|down              GET /healthz on the server, over ssh
#   watch port22 NAME up|down      a new TCP connection to the machine's port 22
#   watch node NAME STATE          the node's state in hivemind nodes
#   watch replica APP|IDX ...      node, state, generation, container of a replica
# With several servers (SOAK_SERVERS > 1), the API, the nodes and the
# replicas are read through the first server that answers, and for each
# server also:
#   watch server NAME VIEW         its view: the leader it names and every
#                                  server's role (hivemind servers --json on it)
# A poll that takes longer than a second starts the next one at once.
set -uo pipefail
. "$(dirname "$0")/lib.sh"

declare -A P22 NST REP SV
API=

# args: the API URL, the token file and the CA (api_args in lib.sh)
read -r -d '' REMOTE <<EOF || true
u=\${1:-http://127.0.0.1:4483} ca=\${3:--}
co=()
[ "\$ca" = - ] || co=(--cacert "\$ca")
curl -fsS -m 2 "\${co[@]}" "\$u/healthz" >/dev/null 2>&1 && echo api-up || echo api-down
$HMC nodes --json 2>/dev/null | jq -r '.[] | "N \(.id) \(.state) \(.lease_age_seconds)"'
$HMC status --json 2>/dev/null | jq -r '.[] | select(.name | startswith("wl-") | not) | .name as \$a | .replicas[] | "R \(\$a)|\(.index) node=\(.node_id // "-") state=\(.state) gen=\(.generation) cid=\((.container_id // "-")[0:12]) reason=\((.pending_reason // "") | gsub(" "; "_"))"'
EOF

poll() {
	local i n out api line k v id nm s pids=()
	for i in "${!NAMES[@]}"; do
		n=${NAMES[$i]}
		if port22 "$n"; then v=up; else v=down; fi
		[ "${P22[$n]:-}" = "$v" ] || {
			ev watch "port22 $n $v"
			P22[$n]=$v
		}
	done
	if [ "$SOAK_SERVERS" -gt 1 ]; then
		for i in "${!SRV_NAMES[@]}"; do
			s=${SRV_NAMES[$i]}
			rm -f "$SOAK_DIR/wview.$i"
			[ "${P22[$s]}" = up ] || {
				echo down >"$SOAK_DIR/wview.$i"
				continue
			}
			(
				j=$(SSH_TIMEOUT=3 servers_json "$s") && w=$(view_of "$j") && [ -n "$w" ] || w=api=fail
				echo "$w" >"$SOAK_DIR/wview.$i"
			) &
			pids+=($!)
		done
	fi
	s=$SERVER
	[ "$SOAK_SERVERS" = 1 ] || s=$(api_server) || s=""
	if [ -n "$s" ]; then
		out=$(SSH_TIMEOUT=4 on "$s" "bash -s $(api_args "$s")" <<<"$REMOTE" 2>/dev/null) || out="api-down"
	else
		out="api-down"
	fi
	[ ${#pids[@]} -eq 0 ] || wait "${pids[@]}"
	api=${out%%$'\n'*}
	[ "$api" = "$API" ] || {
		ev watch "api ${api#api-}$([ "$SOAK_SERVERS" = 1 ] || echo " via=${s:-none}")"
		API=$api
	}
	if [ "$SOAK_SERVERS" -gt 1 ]; then
		for i in "${!SRV_NAMES[@]}"; do
			s=${SRV_NAMES[$i]}
			v=$(cat "$SOAK_DIR/wview.$i" 2>/dev/null || echo api=fail)
			[ "${SV[$s]:-}" = "$v" ] || {
				ev watch "server $s $v"
				SV[$s]=$v
			}
		done
	fi
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
