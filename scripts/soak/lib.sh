# shellcheck shell=bash
# Shared helpers for the soak harness (scripts/soak/*.sh). It runs on a
# control machine that reaches every node over ssh, and touches the nodes only
# through ssh. Sourced, not run.
#
# Settings, from the environment:
#   SOAK_DIR    run directory: the log, the node list, state files (required)
#   NODES_FILE  "name private-ip" lines; the first line is the server
#               (default $SOAK_DIR/nodes)
#   SSH_KEY     private key for the nodes (default ~/.ssh/soak)
#   SSH_USER    login on the nodes, with passwordless sudo (default ec2-user)
#   SOAK_LOG    the log (default $SOAK_DIR/soak.log)
#
# Every probe and event goes to $SOAK_LOG as one line:
#   <UTC time, ms> <Unix time, ms> <kind> <fields...>
# The times come from this machine's clock (CLOCK_REALTIME via date(1)).

SOAK_REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
SOAK_DIR=${SOAK_DIR:?set SOAK_DIR to the run directory}
NODES_FILE=${NODES_FILE:-$SOAK_DIR/nodes}
LOG=${SOAK_LOG:-$SOAK_DIR/soak.log}
SSH_KEY=${SSH_KEY:-$HOME/.ssh/soak}
SSH_USER=${SSH_USER:-ec2-user}
HM=/usr/local/bin/hivemind
DATA=/var/lib/hivemind
BIN=${BIN:-$SOAK_DIR/hivemind}

mkdir -p "$SOAK_DIR"

NAMES=()
IPS=()
while read -r n ip _; do
	case "$n" in '' | '#'*) continue ;; esac
	NAMES+=("$n")
	IPS+=("$ip")
done <"$NODES_FILE"
[ ${#NAMES[@]} -ge 1 ] || {
	echo "soak: no nodes in $NODES_FILE" >&2
	exit 2
}
SERVER=${NAMES[0]}
SERVER_IP=${IPS[0]}

# Several servers (docs/ha.md; the HA soak, ha.sh). The first $SOAK_SERVERS
# lines of $NODES_FILE are servers: the first is made by init, the others
# join it with hivemind join --role server; the lines after them are
# workers. With 1, the default, the cluster is the one server of the earlier
# soaks, and nothing below changes them.
SOAK_SERVERS=${SOAK_SERVERS:-1}
if ! [[ "$SOAK_SERVERS" =~ ^[0-9]+$ ]] || [ "$SOAK_SERVERS" -lt 1 ] || [ "$SOAK_SERVERS" -gt ${#NAMES[@]} ]; then
	echo "soak: SOAK_SERVERS=$SOAK_SERVERS, but $NODES_FILE has ${#NAMES[@]} nodes" >&2
	exit 2
fi
SRV_NAMES=("${NAMES[@]:0:SOAK_SERVERS}")
SRV_IPS=("${IPS[@]:0:SOAK_SERVERS}")
is_server() {
	local s
	for s in "${SRV_NAMES[@]}"; do [ "$s" = "$1" ] && return 0; done
	return 1
}
# The flag that asks join-token for a server's token (docs/ha.md#membership).
SERVER_TOKEN_FLAGS=${SERVER_TOKEN_FLAGS:---role server}
# More flags for hivemind join --role server, if the build wants any.
JOIN_SERVER_FLAGS=${JOIN_SERVER_FLAGS:-}

# The API off loopback. With SOAK_API_TLS=1 (the default with more than one
# server) each server serves the API on its private address, port 4483, with
# TLS (docs/api.md#tls): a certificate for that address from a CA made for
# the run (api_certs), so that a client on another machine can know every
# server (HIVEMIND_SERVER=https://a:4483,https://b:4483,...). On each server,
# $SOAK_ETC holds the certificate and key, the CA, a copy of the admin token
# (mode 0600; a joined server has none of its own, docs/ha.md) and a client
# config naming its own API; the harness's CLI calls use that config.
SOAK_API_TLS=${SOAK_API_TLS:-$([ "$SOAK_SERVERS" -gt 1 ] && echo 1 || echo 0)}
API_PORT=${API_PORT:-4483}
SOAK_ETC=/etc/hivemind-soak
SOAK_CC=$SOAK_ETC/client.yaml
if [ "$SOAK_API_TLS" = 1 ]; then
	HMC="sudo env HIVEMIND_CLIENT_CONFIG=$SOAK_CC $HM"
	API_TOKEN_FILE=$SOAK_ETC/admin.token
else
	HMC="sudo $HM"
	API_TOKEN_FILE=$DATA/admin.token
fi
# api_url IP: the URL of the API of the server at IP.
api_url() { if [ "$SOAK_API_TLS" = 1 ]; then echo "https://$1:$API_PORT"; else echo "http://127.0.0.1:$API_PORT"; fi; }
# api_urls: every server's API, comma separated, for a client that knows them all.
api_urls() {
	local ip s=""
	for ip in "${SRV_IPS[@]}"; do s+=${s:+,}$(api_url "$ip"); done
	echo "$s"
}
# api_args NAME: "URL TOKEN-FILE CA" for a remote script that calls NAME's
# own API with curl (CA "-" for plain HTTP on loopback).
api_args() {
	local ip
	ip=$(ip_of "$1") || return 1
	if [ "$SOAK_API_TLS" = 1 ]; then echo "$(api_url "$ip") $API_TOKEN_FILE $SOAK_ETC/api-ca.pem"; else echo "$(api_url "$ip") $API_TOKEN_FILE -"; fi
}
# api_flags IP: serve's flags for the API at IP, with SOAK_API_TLS=1.
api_flags() {
	[ "$SOAK_API_TLS" = 1 ] || return 0
	printf ' --api-addr %s:%s --api-tls-cert %s/api.crt --api-tls-key %s/api.key' "$1" "$API_PORT" "$SOAK_ETC" "$SOAK_ETC"
}
# The throwaway apps of the write-load (wload.sh) are named $WL_PREFIX<tag>-<n>.
WL_PREFIX=wl-

# The outside machine, for the routing soak: Pebble, its DNS, a private
# registry and the HTTPS clients. One "name private-ip" line in
# $OUTSIDE_FILE (default $SOAK_DIR/outside); it is not a cluster node.
OUTSIDE_FILE=${OUTSIDE_FILE:-$SOAK_DIR/outside}
OUT_NAME="" OUT_IP=""
[ -f "$OUTSIDE_FILE" ] && read -r OUT_NAME OUT_IP _ <"$OUTSIDE_FILE"

# Routing: with SOAK_ROUTES=1 every node runs a router on its own private
# address, with certificates from the ACME CA at $ACME_CA (Pebble on the
# outside machine, which /etc/hosts on each node names "pebble").
SOAK_ROUTES=${SOAK_ROUTES:-0}
ACME_CA=${ACME_CA:-https://pebble:14000/dir}
ACME_EMAIL=${ACME_EMAIL:-ops@soak.test}
REGISTRY=${REGISTRY:-registry.soak.test}
# The files prepare_node installs on each node, fetched by outside.sh.
OUT_FILES=$SOAK_DIR/outside-files
# OUTSIDE_REGISTRY=0: no private registry on the outside machine (the scale
# run, whose images are public; its outside machine is one of the nodes, whose
# router has port 443).
OUTSIDE_REGISTRY=${OUTSIDE_REGISTRY:-1}

# The h2c test server (test/h2ctest), built by the operator into
# $SOAK_DIR/h2ctest; import_h2c NAME imports it on NAME as the image
# $H2C_IMAGE, which no registry has. It does nothing without the file, or
# when NAME has the image.
H2C_IMAGE=${H2C_IMAGE:-hivemind-h2ctest:scale}
import_h2c() {
	[ -s "$SOAK_DIR/h2ctest" ] || return 0
	on "$1" "sudo docker image inspect $H2C_IMAGE >/dev/null 2>&1" && return 0
	SSH_TIMEOUT=120 on "$1" "d=\$(mktemp -d) && cat >\$d/h2ctest && chmod 755 \$d/h2ctest && tar -C \$d -cf - h2ctest | sudo docker import --change 'ENTRYPOINT [\"/h2ctest\"]' - $H2C_IMAGE >/dev/null; rc=\$?; rm -rf \$d; exit \$rc" <"$SOAK_DIR/h2ctest"
}

# The server's DNS name (the third soak). With SERVER_NAME set, the server
# runs serve --advertise $SERVER_NAME --cluster-addr <its private address>,
# and the workers join the name. One line in /etc/hosts on every machine,
# "IP NAME # soak-server", stands in for the name's DNS record; the IP it
# names is in $DNS_FILE (the server's address until dns_set changes it).
# "ip" in $ADV_FILE makes the server advertise its address instead, for the
# IP-joined half of that soak.
SERVER_NAME=${SERVER_NAME:-}
if [ -n "$SERVER_NAME" ] && [ "$SOAK_SERVERS" -gt 1 ]; then
	echo "soak: SERVER_NAME is for a cluster of one server; unset it with SOAK_SERVERS=$SOAK_SERVERS" >&2
	exit 2
fi
DNS_FILE=$SOAK_DIR/server-dns
ADV_FILE=$SOAK_DIR/advertise
advertise_mode() {
	if [ -n "$SERVER_NAME" ] && [ "$(cat "$ADV_FILE" 2>/dev/null)" != ip ]; then echo name; else echo ip; fi
}
# server_addr: the address a worker joins, or set-server is given.
server_addr() { if [ "$(advertise_mode)" = name ]; then echo "$SERVER_NAME"; else echo "$SERVER_IP"; fi; }
dns_ip() { cat "$DNS_FILE" 2>/dev/null || echo "$SERVER_IP"; }
# dns_line NAME IP: the server name's hosts line on machine NAME.
dns_line() {
	on "$1" "sudo sed -i '/ # soak-server\$/d' /etc/hosts && echo '$2 $SERVER_NAME # soak-server' | sudo tee -a /etc/hosts >/dev/null"
}

# The connection to each node is shared (ControlMaster), so a probe costs one
# round trip. ServerAlive gives up on a dead master after about 4 s.
SSH_OPTS=(-i "$SSH_KEY" -o IdentitiesOnly=yes -o BatchMode=yes -o StrictHostKeyChecking=accept-new
	-o UserKnownHostsFile="$SOAK_DIR/known_hosts" -o ServerAliveInterval=2
	-o ServerAliveCountMax=2 -o ControlMaster=auto -o ControlPath="$SOAK_DIR/cm-%C" -o ControlPersist=120
	-o LogLevel=ERROR)

ip_of() {
	local i
	for i in "${!NAMES[@]}"; do
		[ "${NAMES[$i]}" = "$1" ] && {
			echo "${IPS[$i]}"
			return 0
		}
	done
	[ -n "$OUT_NAME" ] && [ "$1" = "$OUT_NAME" ] && {
		echo "$OUT_IP"
		return 0
	}
	echo "soak: unknown node $1" >&2
	return 1
}

# on NAME COMMAND: run COMMAND (one shell string) on NAME, for at most
# SSH_TIMEOUT seconds (default 10). A new connection gives up after SSH_CT
# seconds (default 3); collect uses 1 s, so a dead node costs it 1 s.
on() {
	local ip
	ip=$(ip_of "$1") || return 1
	shift
	on_ip "$ip" "$@"
}
# on_ip IP COMMAND: the same, for a machine by its address.
on_ip() {
	local ip=$1
	shift
	timeout "${SSH_TIMEOUT:-10}" ssh "${SSH_OPTS[@]}" -o ConnectTimeout="${SSH_CT:-3}" "$SSH_USER@$ip" "$@"
}

# port22_ip IP: port22 for a machine by its address.
port22_ip() { timeout 1 bash -c "exec 3<>/dev/tcp/$1/22" 2>/dev/null; }

# forget_host NAME: drop NAME's host key and shared connection, for a machine
# that was replaced.
forget_host() {
	local ip
	ip=$(ip_of "$1") || return 1
	ssh -O exit "${SSH_OPTS[@]}" "$SSH_USER@$ip" >/dev/null 2>&1 || true
	ssh-keygen -R "$ip" -f "$SOAK_DIR/known_hosts" >/dev/null 2>&1 || true
}

# port22 NAME: does NAME accept a TCP connection on port 22 within 1 s? A new
# connection each time, so a dead machine shows at once.
port22() {
	local ip
	ip=$(ip_of "$1") || return 1
	timeout 1 bash -c "exec 3<>/dev/tcp/$ip/22" 2>/dev/null
}

now_ms() { date -u +%s%3N; }

# ev KIND FIELDS...: one log line.
ev() {
	local t
	t=$(date -u +'%FT%T.%3NZ %s%3N')
	printf '%s %s\n' "$t" "$*" >>"$LOG"
}

# note TEXT: to stderr and the log.
note() {
	echo "soak: $*" >&2
	ev note "$*"
}

die() {
	echo "soak: FAIL: $*" >&2
	ev fail "$*"
	exit 1
}

# wait_for SECONDS COMMAND...: retry COMMAND once a second until it succeeds.
wait_for() {
	local end=$(($(date +%s) + $1))
	shift
	while :; do
		"$@" >/dev/null 2>&1 && return 0
		[ "$(date +%s)" -ge "$end" ] && return 1
		sleep 1
	done
}

# hm NAME ARGS...: the hivemind CLI as root on NAME. The CLI reads root's
# client config, which init and restore write; with SOAK_API_TLS=1, the
# soak's client config in $SOAK_ETC, which names NAME's API on its address.
hm() {
	local n=$1
	shift
	on "$n" "$HMC $*"
}

# With several servers any one answers the API: a follower passes the
# request to the leader (docs/ha.md#the-api). API_PREF names the server that
# answered last, which is asked first next time.
API_PREF=$SOAK_DIR/api-server
# api_order: the servers, the one that answered last first.
api_order() {
	local last s
	last=$(cat "$API_PREF" 2>/dev/null || true)
	if [ -n "$last" ] && is_server "$last"; then echo "$last"; else last=""; fi
	for s in "${SRV_NAMES[@]}"; do [ "$s" = "$last" ] || echo "$s"; done
}
api_used() { [ "$(cat "$API_PREF" 2>/dev/null)" = "$1" ] || echo "$1" >"$API_PREF"; }
# hma ARGS...: hm on the first server, in api_order, whose machine answers
# on port 22 and whose CLI succeeds; with one server, hm on it. Its stdin is
# /dev/null; hma_in passes its own stdin to each try.
hma() {
	local s
	if [ "$SOAK_SERVERS" = 1 ]; then
		hm "$SERVER" "$@" </dev/null
		return
	fi
	for s in $(api_order); do
		port22 "$s" || continue
		if hm "$s" "$@" </dev/null; then
			api_used "$s"
			return 0
		fi
	done
	return 1
}
hma_in() {
	local s in
	in=$(cat)
	if [ "$SOAK_SERVERS" = 1 ]; then
		hm "$SERVER" "$@" <<<"$in"
		return
	fi
	for s in $(api_order); do
		port22 "$s" || continue
		if hm "$s" "$@" <<<"$in"; then
			api_used "$s"
			return 0
		fi
	done
	return 1
}
# api_server: a server whose machine answers, in api_order (for a remote
# script that calls the API itself).
api_server() {
	local s
	for s in $(api_order); do
		if port22 "$s"; then
			echo "$s"
			return 0
		fi
	done
	return 1
}

# name_of_ip IP: the soak name of the machine at IP, or IP.
name_of_ip() {
	local i
	for i in "${!IPS[@]}"; do
		[ "${IPS[$i]}" = "$1" ] && {
			echo "${NAMES[$i]}"
			return 0
		}
	done
	echo "$1"
}

# servers_json NAME: hivemind servers --json as NAME's API answers it: a
# follower passes it to the leader, and without a leader answers it from its
# own copy of the configuration (docs/ha.md#membership).
servers_json() { SSH_CT=${SSH_CT:-1} SSH_TIMEOUT=${SSH_TIMEOUT:-5} hm "$1" "servers --json" 2>/dev/null </dev/null; }
# The fields of GET /v1/servers that the harness reads: leader (an ID, or
# "" without one), answered_by, note, and for each server id, advertise and
# role (leader, follower or nonvoter). JQ_SRV_NAME maps an advertise
# address, with any :port taken off, to the machine's soak name.
jq_srv_names() {
	local i m="{"
	for i in "${!IPS[@]}"; do m+="\"${IPS[$i]}\":\"${NAMES[$i]}\","; done
	echo "${m%,}}"
}
# view_of JSON: one word list of a servers reply: "leader=NAME|none
# roles=NAME:ROLE,... [note=TEXT]", the note's spaces as _.
view_of() {
	jq -r --argjson nm "$(jq_srv_names)" '
		def nm: (.advertise // "" | sub(":[0-9]+$"; "")) as $a | ($nm[$a] // $a);
		. as $r
		| ([.servers[] | select(.id == $r.leader and $r.leader != "") | nm] | first // "none") as $l
		| "leader=\($l) roles=\([.servers[] | "\(nm):\(.role)"] | sort | join(","))"
		  + (if ($r.note // "") != "" then " note=\($r.note | gsub("[[:space:]]+"; "_") | .[0:200])" else "" end)' <<<"$1" 2>/dev/null
}
# leader_in JSON: the leader's soak name in a servers reply, or "none".
leader_in() {
	local v
	v=$(view_of "$1") || v=""
	v=${v#leader=}
	echo "${v%% *}"
}
# leader: the leader, as the first server that answers names it.
leader() {
	local s j l
	for s in $(api_order); do
		port22 "$s" || continue
		j=$(servers_json "$s") || continue
		l=$(leader_in "$j")
		if [ -n "$l" ] && [ "$l" != none ]; then
			api_used "$s"
			echo "$l"
			return 0
		fi
	done
	return 1
}
# server_role NAME: NAME's role (leader, follower, nonvoter) as the first
# server that answers lists it; empty when it is not listed.
server_role() {
	local j ip
	ip=$(ip_of "$1") || return 1
	j=$(hma "servers --json" 2>/dev/null) || return 1
	jq -r --arg ip "$ip" '.servers[] | select((.advertise // "" | sub(":[0-9]+$"; "")) == $ip) | .role' <<<"$j" | head -n 1
}
# server_voter NAME: NAME is a voting server (leader or follower).
server_voter() { case "$(server_role "$1")" in leader | follower) return 0 ;; *) return 1 ;; esac; }
server_listed() { [ -n "$(server_role "$1")" ]; }
# servers_line: each server's view, "NAME[leader=... roles=...]", for a log line.
servers_line() {
	local s j v out=""
	for s in "${SRV_NAMES[@]}"; do
		if port22 "$s" && j=$(servers_json "$s"); then v=$(view_of "$j"); else v="api=fail"; fi
		out+="${s}[${v:-api=fail}] "
	done
	echo "$out"
}

# The node ID of each node, as "id name" lines, written by setup.sh.
IDS_FILE=$SOAK_DIR/ids
id_of() { awk -v n="$1" '$2 == n { print $1 }' "$IDS_FILE"; }
name_of_id() { awk -v i="$1" '$1 == i { print $2 }' "$IDS_FILE"; }

# One "C" row per hivemind container on a node, exited ones included: the
# full ID, state, StartedAt, app, replica index and generation, | separated.
read -r -d '' REMOTE_ROWS <<'EOF' || true
ids=$(sudo docker ps -aq --no-trunc --filter label=hivemind.app) || exit 1
[ -z "$ids" ] || sudo docker inspect -f '{{.Id}}|{{.State.Status}}|{{.State.StartedAt}}|{{index .Config.Labels "hivemind.app"}}|{{index .Config.Labels "hivemind.replica"}}|{{index .Config.Labels "hivemind.generation"}}' $ids
EOF

# container_rows_of NAME: the C rows of one node, or failure if it does not answer.
container_rows_of() {
	local out
	out=$(on "$1" "$REMOTE_ROWS") || return 1
	[ -z "$out" ] && return 0
	awk -F'|' -v n="$1" 'BEGIN { OFS = "\t" } { print "C", n, substr($1, 1, 12), $2, $3, $4, $5, $6 }' <<<"$out"
}

# status_rows JSON: the A and R rows of the server's status, as fault-suite.sh writes them.
status_rows() {
	jq -r '.[] | . as $a | (["A", $a.name, ($a.desired|tostring)] | @tsv), (.replicas[] | ["R", $a.name, (.index|tostring), (.node_id // ""), .state, (.container_id // ""), (.pending_reason // ""), (.generation|tostring), .desired] | @tsv)'
}

# collect FILE [nodes]: the server's status (A, R), the nodes (N), and the
# containers of every node that answers (C), in the format of
# scripts/lib/converged.awk. More rows: "S ok" or "S fail" for the status
# call; "U name" for each node that did not answer; with "nodes", one
# "L id state lease-age" row per node from hivemind nodes. The status and
# the nodes come from one ssh call; the nodes are asked in parallel.
collect() {
	local f=$1 want_nodes=${2:-} tmp=$1.tmp out i n s cmd pids=()
	# The reply is split with sed, not ${out%%#nodes*}: bash's pattern
	# removal is quadratic in the length, 24 s for a 115 KB status.
	cmd="$HMC status --json && echo '#nodes' && { [ -z '$want_nodes' ] || $HMC nodes --json; }"
	out=""
	if [ "$SOAK_SERVERS" = 1 ]; then
		out=$(on "$SERVER" "$cmd" 2>/dev/null) && grep -qx '#nodes' <<<"$out" || out=
	else
		# with several servers, the first that answers (the API is the leader's
		# through any of them)
		for s in $(api_order); do
			port22 "$s" || continue
			if out=$(on "$s" "$cmd" 2>/dev/null) && grep -qx '#nodes' <<<"$out"; then
				api_used "$s"
				break
			fi
			out=""
		done
	fi
	if [ -n "$out" ] && sed '/^#nodes$/,$d' <<<"$out" | status_rows >"$tmp" 2>/dev/null; then
		echo "S	ok" >>"$tmp"
	else
		echo "S	fail" >"$tmp"
	fi
	if [ -n "$want_nodes" ] && [ -n "$out" ]; then
		sed '1,/^#nodes$/d' <<<"$out" | jq -r '.[] | ["L", .id, .state, (.lease_age_seconds|tostring)] | @tsv' >>"$tmp" 2>/dev/null || true
	fi
	awk '{ printf "N\t%s\t%s\n", $1, $2 }' "$IDS_FILE" >>"$tmp"
	for i in "${!NAMES[@]}"; do
		n=${NAMES[$i]}
		(SSH_CT=1 container_rows_of "$n" >"$tmp.$i" 2>/dev/null || printf 'U\t%s\n' "$n" >"$tmp.$i") &
		pids+=($!)
	done
	wait "${pids[@]}"
	for i in "${!NAMES[@]}"; do
		cat "$tmp.$i" >>"$tmp"
		rm -f "$tmp.$i"
	done
	mv "$tmp" "$f"
}

api_ok() { grep -q '^S	ok$' "$1"; }

# snapshot FILE: collect, and fail when the server's status did not answer.
snapshot() { collect "$1" && api_ok "$1"; }

# node_state FILE ID: ready, lost, or empty, from the L rows.
node_state() { awk -F'\t' -v i="$2" '$1 == "L" && $2 == i { print $3 }' "$1"; }

# converged FILE [allow_held]: the fault suite's convergence check on FILE.
check_converged() {
	awk -F'\t' -v allow_held="${2:-0}" -f "$SOAK_REPO/scripts/lib/converged.awk" "$1"
}

# dups FILE: "app|replica node,node" for each pair running on more than one
# node. Empty when there is none.
dups() {
	awk -F'\t' '$1 == "C" && $4 == "running" { k = $6 "|" $7; n[k]++; on[k] = on[k] (on[k] == "" ? "" : ",") $2 }
		END { for (k in n) if (n[k] > 1) print k, on[k] }' "$1" | sort
}

# unreachable FILE: the nodes that did not answer, comma separated.
unreachable() { awk -F'\t' '$1 == "U" { s = s (s == "" ? "" : ",") $2 } END { print s }' "$1"; }

# bg_start NAME COMMAND...: run COMMAND detached, output to $SOAK_DIR/NAME.out.
bg_start() {
	local name=$1
	shift
	if [ -f "$SOAK_DIR/$name.pid" ] && kill -0 "$(cat "$SOAK_DIR/$name.pid")" 2>/dev/null; then
		echo "soak: $name already runs as $(cat "$SOAK_DIR/$name.pid")" >&2
		return 0
	fi
	setsid nohup "$@" >>"$SOAK_DIR/$name.out" 2>&1 </dev/null &
	echo $! >"$SOAK_DIR/$name.pid"
	ev bg "start $name pid=$!"
	# not a job of this shell: a later bare wait must not wait for it
	disown "$!" 2>/dev/null || true
}

bg_stop() {
	local pid
	[ -f "$SOAK_DIR/$1.pid" ] || return 0
	pid=$(cat "$SOAK_DIR/$1.pid")
	kill -- -"$pid" 2>/dev/null || kill "$pid" 2>/dev/null || true
	rm -f "$SOAK_DIR/$1.pid"
	ev bg "stop $1 pid=$pid"
}

# The unit files, as docs/systemd.md gives them. One unit name on every node.
server_unit() {
	cat <<EOF
[Unit]
Description=hivemind server
Documentation=https://github.com/ao/hivemind
Wants=network-online.target docker.service
After=network-online.target docker.service

[Service]
ExecStart=/usr/local/bin/hivemind serve $(advertise_flags "$1")$(api_flags "$1")$(route_flags "$1")
Restart=on-failure
RestartPreventExitStatus=3
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
}

# advertise_flags IP: serve's address flags for a server at IP.
advertise_flags() {
	if [ "$(advertise_mode)" = name ]; then
		printf -- '--advertise %s --cluster-addr %s' "$SERVER_NAME" "$1"
	else
		printf -- '--advertise %s' "$1"
	fi
}

# route_flags IP: the router's flags for a node at IP, with SOAK_ROUTES=1.
# The router listens on the node's own private address, on ports 80 and 443.
route_flags() {
	[ "$SOAK_ROUTES" = 1 ] || return 0
	printf ' --route-addr %s --acme-email %s --acme-ca %s' "$1" "$ACME_EMAIL" "$ACME_CA"
}

# worker_unit [IP]: the worker's unit as docs/systemd.md gives it, with no
# flags: join wrote the router's flags to $DATA/hivemind.yaml, and serve
# --role worker reads them there. With WORKER_UNIT_FLAGS=1 the router's
# flags go in ExecStart too (IP is the worker's private address), as the
# docs say for a worker joined by a build before 2.2; the fifth soak and
# earlier ran that way.
worker_unit() {
	local flags=""
	[ "${WORKER_UNIT_FLAGS:-0}" = 1 ] && flags=$(route_flags "${1:-}")
	cat <<EOF
[Unit]
Description=hivemind worker
Documentation=https://github.com/ao/hivemind
Wants=network-online.target docker.service
After=network-online.target docker.service

[Service]
ExecStart=/usr/local/bin/hivemind serve --role worker$flags
Restart=on-failure
RestartPreventExitStatus=3
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
}

# install_bin NAME: copy the binary to NAME and check its digest.
install_bin() {
	local want got
	want=$(sha256sum "$BIN" | cut -d' ' -f1)
	SSH_TIMEOUT=120 on "$1" "cat > /tmp/hivemind.new && sudo install -m 0755 /tmp/hivemind.new $HM.new && sudo mv -f $HM.new $HM && rm -f /tmp/hivemind.new" <"$BIN"
	got=$(on "$1" "sha256sum $HM" | cut -d' ' -f1)
	[ "$got" = "$want" ] || die "$1: binary digest $got, want $want"
}

# install_unit NAME TEXT: write /etc/systemd/system/hivemind.service, enable and start it.
install_unit() {
	on "$1" "sudo tee /etc/systemd/system/hivemind.service >/dev/null && sudo systemctl daemon-reload && sudo systemctl enable --now hivemind.service" <<<"$2"
}

# kv_get NAME: the kv value, read on NAME from 127.0.0.1:6379 (redis inline protocol).
read -r -d '' REMOTE_KV_GET <<'EOK' || true
timeout 3 bash -c 'exec 3<>/dev/tcp/127.0.0.1/6379 || exit 1; printf "GET soak:canary\r\n" >&3; IFS= read -r -t 2 a <&3; IFS= read -r -t 2 b <&3; printf "%s\n" "${b%$(printf "\r")}"'
EOK
kv_get() { on "$1" "$REMOTE_KV_GET"; }

# kv_set NAME VALUE: write the kv value on NAME over the published port, with
# the redis inline protocol. Prints the reply, +OK on success.
KV_SET='exec 3<>/dev/tcp/127.0.0.1/6379 || exit 1; printf "SET soak:canary %s\r\n" "$1" >&3; IFS= read -r -t 3 r <&3; printf "%s\n" "${r%$(printf "\r")}"'
kv_set() { on "$1" "bash -c '$KV_SET' sh '$2'"; }

VOLUME_APPS=${VOLUME_APPS:-kv}

# running FILE: "running/desired": replicas the server reports running, of
# those that should run. The convergence check also accepts a replica that is
# pending with a placement reason; this count does not.
# The write-load's throwaway apps (wload.sh), whose replicas never run, are
# not counted.
running() { awk -F'\t' -v wl="$WL_PREFIX" '$1 == "R" && $9 == "running" && index($2, wl) != 1 { n++; if ($5 == "running") r++ } END { printf "%d/%d", r, n }' "$1"; }

# prepare_node NAME: the machine set-up a routed, private-registry cluster
# needs on each node besides hivemind, as an operator would do it: the
# outside machine's names in /etc/hosts (Pebble's ACME API and the
# registry), Pebble's TLS root in the system trust store (the routers' ACME
# client uses the system roots), and the registry's CA for Docker in
# /etc/docker/certs.d. Needs $OUT_FILES from outside.sh setup.
prepare_node() {
	local n=$1
	[ -n "$OUT_IP" ] || die "prepare_node: no outside machine in $OUTSIDE_FILE"
	[ -s "$OUT_FILES/pebble-minica.pem" ] || die "prepare_node: run outside.sh setup first"
	[ "$OUTSIDE_REGISTRY" = 0 ] || [ -s "$OUT_FILES/registry-ca.crt" ] || die "prepare_node: run outside.sh setup first"
	on "$n" "sudo sed -i '/ # soak-outside\$/d' /etc/hosts && echo '$OUT_IP pebble $REGISTRY # soak-outside' | sudo tee -a /etc/hosts >/dev/null" || return 1
	# The server's name resolves to what the record says now, which on a new
	# server machine is still the old address.
	if [ -n "$SERVER_NAME" ]; then
		[ -s "$DNS_FILE" ] || echo "$SERVER_IP" >"$DNS_FILE"
		dns_line "$n" "$(dns_ip)" || return 1
	fi
	on "$n" "sudo tee /etc/pki/ca-trust/source/anchors/pebble-minica.pem >/dev/null && sudo update-ca-trust" <"$OUT_FILES/pebble-minica.pem" || return 1
	if [ "$OUTSIDE_REGISTRY" != 0 ]; then
		on "$n" "sudo install -d /etc/docker/certs.d/$REGISTRY && sudo tee /etc/docker/certs.d/$REGISTRY/ca.crt >/dev/null" <"$OUT_FILES/registry-ca.crt" || return 1
	fi
	import_h2c "$n" || return 1
	on "$n" "curl -fsS -m 5 -o /dev/null https://pebble:14000/dir" || die "$n: Pebble's ACME directory does not answer, or is not trusted"
	ev prepare "$n hosts+trust+certs.d done"
}

# www_spec VERSION: the routed app on the private image; scenario.sh deploys
# version 2 for the rolling update. WHOAMI_NAME makes every reply name the
# app ("Name: www"), so its streams can check who answered (the seventh soak).
www_spec() {
	cat <<EOF
{"name":"www","image":"$REGISTRY/soak/whoami:$1","replicas":3,"env":{"WHOAMI_NAME":"www"$(roll_env)},
 "routes":[{"host":"www.soak.test","port":80}],
 "health":{"http":{"port":80,"path":"/health"},"interval":"2s","timeout":"1s"}}
EOF
}

# The path-routed pair (the fourth soak). With SOAK_PATHS=1 two apps share
# the host $PATH_HOST: proot takes the whole host and papi the prefix /api.
# Both run traefik/whoami from the private registry with WHOAMI_NAME set to
# the app's name, so every reply says which app answered it ("Name: papi").
SOAK_PATHS=${SOAK_PATHS:-0}
PATH_HOST=${PATH_HOST:-paths.soak.test}
# path_spec NAME PATH VERSION: PATH "/" is the whole host.
path_spec() {
	local route
	if [ "$2" = / ]; then
		route="{\"host\":\"$PATH_HOST\",\"port\":80}"
	else
		route="{\"host\":\"$PATH_HOST\",\"path\":\"$2\",\"port\":80}"
	fi
	cat <<EOF
{"name":"$1","image":"$REGISTRY/soak/whoami:$3","replicas":2,"env":{"WHOAMI_NAME":"$1"$(roll_env)},
 "routes":[$route],
 "health":{"http":{"port":80,"path":"/health"},"interval":"2s","timeout":"1s"}}
EOF
}

# The gRPC app (the sixth soak). With SOAK_GRPC=1, grpc runs test/h2ctest's
# server, a server of gRPC's wire format that speaks only HTTP/2 without
# TLS, from the private image $REGISTRY/soak/h2ctest (outside.sh pushes
# versions 1 and 2, the same binary), under $GRPC_PATH of $PATH_HOST, an h2c
# route: so the host has http routes (proot, papi) and an h2c one. Its
# health check is TCP, as docs/routing.md says for a server that speaks only
# h2c. V in its environment is the version, which every reply names
# ("grpc/2@<container>:ping").
SOAK_GRPC=${SOAK_GRPC:-0}
GRPC_PATH=${GRPC_PATH:-/h2ctest.Echo}
grpc_spec() {
	cat <<EOF
{"name":"grpc","image":"$REGISTRY/soak/h2ctest:$1","replicas":2,"env":{"V":"$1"$(roll_env)},
 "command":["serve","-addr",":50051","-name","grpc"],
 "routes":[{"host":"$PATH_HOST","path":"$GRPC_PATH","port":50051,"protocol":"h2c"}],
 "health":{"tcp":{"port":50051},"interval":"2s","timeout":"1s"}}
EOF
}

# ROLL, when set, goes in every app's environment as ROLL, so that each
# value is a new spec and a rolling update (scenario.sh rollout-all, the
# seventh soak). roll_env: ',"ROLL":"N"' after an app's other variables;
# roll_env_field: '"env":{"ROLL":"N"},' for an app with none. Both print
# nothing without ROLL, which gives the specs of the earlier soaks.
ROLL=${ROLL:-}
roll_env() { [ -z "$ROLL" ] || printf ',"ROLL":"%s"' "$ROLL"; }
roll_env_field() { [ -z "$ROLL" ] || printf '"env":{"ROLL":"%s"},' "$ROLL"; }

# The other apps of the routing soak, as setup.sh deploys them.
WEB_IMAGE=${WEB_IMAGE:-nginx:alpine}
SPREAD_IMAGE=${SPREAD_IMAGE:-busybox:stable}
KV_IMAGE=${KV_IMAGE:-redis:7-alpine}
api_spec() {
	echo "{\"name\":\"api\",\"image\":\"$WEB_IMAGE\",\"replicas\":2,$(roll_env_field)\"routes\":[{\"host\":\"api.soak.test\",\"port\":80}],\"health\":{\"http\":{\"port\":80,\"path\":\"/\"},\"interval\":\"2s\",\"timeout\":\"1s\"}}"
}
spread_spec() {
	echo "{\"name\":\"spread\",\"image\":\"$SPREAD_IMAGE\",\"replicas\":6,$(roll_env_field)\"command\":[\"sleep\",\"86400\"]}"
}
kv_spec() {
	echo "{\"name\":\"kv\",\"image\":\"$KV_IMAGE\",$(roll_env_field)\"command\":[\"redis-server\",\"--appendonly\",\"yes\"],\"ports\":[{\"host\":6379,\"container\":6379,\"bind\":\"127.0.0.1\"}],\"volumes\":[{\"name\":\"data\",\"path\":\"/data\"}]}"
}
# shop_compose: compose-shop.yaml, with ROLL in each service's environment
# when it is set.
shop_compose() {
	if [ -z "$ROLL" ]; then
		cat "$SOAK_REPO/scripts/soak/compose-shop.yaml"
	else
		sed "s/^    image: \(.*\)$/    image: \1\n    environment:\n      ROLL: \"$ROLL\"/" "$SOAK_REPO/scripts/soak/compose-shop.yaml"
	fi
}

# routed_ports NAME: one word per routed container on NAME, stopped ones
# included, sorted: app/replica|state|label|published|container, label the
# host ports as the hivemind.route-ports label records them
# (container:host, comma separated), published the address:port Docker
# publishes for a running container, container its ID (12). A replica keeps
# its host ports for the life of its container (docs/routing.md#route-host-ports).
read -r -d '' REMOTE_ROUTED_PORTS <<'EOF' || true
ids=$(sudo docker ps -aq --filter label=hivemind.routed=true) || exit 1
[ -z "$ids" ] || sudo docker inspect -f '{{index .Config.Labels "hivemind.app"}}/{{index .Config.Labels "hivemind.replica"}}|{{.State.Status}}|{{index .Config.Labels "hivemind.route-ports"}}|{{range $p, $b := .NetworkSettings.Ports}}{{range $b}}{{.HostIp}}:{{.HostPort}},{{end}}{{end}}|{{printf "%.12s" .Id}}' $ids | sort | tr '\n' ' '
EOF
routed_ports() { on "$1" "$REMOTE_ROUTED_PORTS"; }
# ports_key WORDS: routed_ports' words without the state, published ports
# and ID: "app/replica|label" each, sorted, for comparing two reads.
ports_key() { tr ' ' '\n' <<<"$1" | grep . | cut -d'|' -f1,3 | sort | tr '\n' ' '; }

# app_spec APP VERSION: an app at a version (of the private image, for the
# routed apps that run one; the others have one version).
app_spec() {
	case "$1" in
	www) www_spec "$2" ;;
	proot) path_spec proot / "$2" ;;
	papi) path_spec papi /api "$2" ;;
	grpc) grpc_spec "$2" ;;
	api) api_spec ;;
	spread) spread_spec ;;
	kv) kv_spec ;;
	*)
		echo "soak: no spec for $1" >&2
		return 1
		;;
	esac
}

# join_worker NAME: join with a one-hour token, then hand over to systemd.
join_worker() {
	local n=$1 tok ip
	ip=$(ip_of "$n")
	tok=$(hma join-token --ttl 1h 2>/dev/null) || die "join-token on $SERVER"
	note "joining $n"
	# The token goes on stdin, so it is in no sudo log; join takes it only as a flag.
	on "$n" "sudo sh -c 'read -r T; nohup $HM join $(server_addr) --token \"\$T\"$(route_flags "$ip") >/var/log/hivemind-join.log 2>&1 </dev/null &'" <<<"$tok"
	wait_for 60 on "$n" "sudo test -s $DATA/node.json" || die "$n did not join: $(on "$n" 'sudo tail -n 5 /var/log/hivemind-join.log' 2>&1)"
	# join keeps running as the agent. Stop it, and let systemd run the worker.
	on "$n" "sudo pkill -TERM -f '^$HM join' ; for i in \$(seq 1 20); do pgrep -f '^$HM join' >/dev/null || exit 0; sleep 0.5; done; exit 1" ||
		die "$n: the foreground join did not stop"
	# join writes the worker flags it was given to hivemind.yaml (docs/systemd.md),
	# which the unit's plain serve --role worker reads. Its keys, mode and owner:
	ev setup "$n hivemind.yaml $(on "$n" "sudo stat -c 'mode=%a owner=%U' $DATA/hivemind.yaml 2>/dev/null || echo missing") $(on "$n" "sudo cat $DATA/hivemind.yaml 2>/dev/null | grep -v '^#' | tr -s ' \n' ' '")"
	install_unit "$n" "$(worker_unit "$ip")"
	ev setup "$n unit ExecStart=$(on "$n" "systemctl show -p ExecStart --value hivemind.service | sed -n 's/.*argv\[\]=\([^;]*\);.*/\1/p'")"
	if [ "$SOAK_ROUTES" = 1 ]; then
		wait_for 60 router_up "$n" || die "$n: no router after the unit started: $(hma nodes 2>&1 | tr -s ' \n' ' ')"
		ev setup "$n router up: ROUTER yes in hivemind nodes; listening $(on "$n" "sudo ss -ltnH '( sport = :80 or sport = :443 )'" | awk '{ printf "%s ", $4 }')"
	fi
}

# router_up NAME: hivemind nodes has router true for NAME's node (its
# router read the routing table in the last minute), and NAME listens on 443.
router_up() {
	local id
	id=$(on "$1" "sudo jq -r .node_id $DATA/node.json") || return 1
	[ "$(hma nodes --json | jq -r --arg i "$id" '.[] | select(.id == $i) | .router')" = true ] &&
		on "$1" "sudo ss -ltnH '( sport = :443 )' | grep -q ."
}

# node_id_on NAME: NAME's node ID from its data directory: node.json on a
# worker, server.json on a server that joined (docs/ha.md), or for the
# server init made, the server node of hivemind nodes whose host name is
# NAME's (with one server, the one server node).
node_id_on() {
	local id h
	id=$(on "$1" "sudo sh -c 'if [ -s $DATA/node.json ]; then jq -r .node_id $DATA/node.json; elif [ -s $DATA/server.json ]; then jq -r .node_id $DATA/server.json; fi'" 2>/dev/null) || id=""
	if [ -z "$id" ] || [ "$id" = null ]; then
		h=$(on "$1" hostname 2>/dev/null) || return 1
		id=$(hma nodes --json 2>/dev/null | jq -r --arg h "$h" '[.[] | select(.server)] as $s | if ($s | length) == 1 then $s[0].id else ($s[] | select(.name == $h) | .id) end' | head -n 1)
	fi
	[ -n "$id" ] && [ "$id" != null ] && echo "$id"
}

# router_up_id NAME: router_up for any node, a server's too.
router_up_id() {
	local id
	id=$(node_id_on "$1") || return 1
	[ "$(hma nodes --json | jq -r --arg i "$id" '.[] | select(.id == $i) | .router')" = true ] &&
		on "$1" "sudo ss -ltnH '( sport = :443 )' | grep -q ."
}

# api_certs: with SOAK_API_TLS=1, a CA made for the run and a certificate
# for each server's private address (ECDSA P-256, 7 days), made with openssl
# on this machine in $SOAK_DIR/api-tls (mode 0700); then on each server the
# certificate, its key (0600) and the CA in $SOAK_ETC. The clients trust that CA.
API_TLS_DIR=$SOAK_DIR/api-tls
api_certs() {
	local ip n i
	[ "$SOAK_API_TLS" = 1 ] || return 0
	(
		umask 077
		mkdir -p "$API_TLS_DIR"
		cd "$API_TLS_DIR" || exit 1
		[ -s ca.crt ] || openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 7 -subj '/CN=soak API CA' \
			-addext basicConstraints=critical,CA:TRUE -addext keyUsage=critical,keyCertSign,cRLSign -keyout ca.key -out ca.crt 2>/dev/null || exit 1
		for ip in "${SRV_IPS[@]}"; do
			[ -s "$ip.crt" ] && continue
			openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -subj "/CN=$ip" -keyout "$ip.key" -out "$ip.csr" 2>/dev/null || exit 1
			printf 'subjectAltName=IP:%s\nextendedKeyUsage=serverAuth\nbasicConstraints=CA:FALSE\nkeyUsage=critical,digitalSignature\n' "$ip" >"$ip.ext"
			openssl x509 -req -in "$ip.csr" -CA ca.crt -CAkey ca.key -CAcreateserial -days 7 -extfile "$ip.ext" -out "$ip.crt" 2>/dev/null || exit 1
		done
	) || die "api_certs: openssl"
	for i in "${!SRV_NAMES[@]}"; do
		n=${SRV_NAMES[$i]} ip=${SRV_IPS[$i]}
		on "$n" "sudo install -d -m 0700 $SOAK_ETC && sudo tee $SOAK_ETC/api-ca.pem >/dev/null" <"$API_TLS_DIR/ca.crt" || die "$n: the API's CA"
		on "$n" "sudo tee $SOAK_ETC/api.crt >/dev/null" <"$API_TLS_DIR/$ip.crt" || die "$n: the API's certificate"
		on "$n" "sudo sh -c 'umask 077; cat > $SOAK_ETC/api.key'" <"$API_TLS_DIR/$ip.key" || die "$n: the API's key"
		on "$n" "printf 'server: %s\ntoken-file: %s\nca-cert: %s\n' '$(api_url "$ip")' '$API_TOKEN_FILE' '$SOAK_ETC/api-ca.pem' | sudo sh -c 'umask 077; cat > $SOAK_CC'" || die "$n: the client config"
	done
	ev setup "api tls: CA $(openssl x509 -noout -fingerprint -sha256 -in "$API_TLS_DIR/ca.crt" | cut -d= -f2 | cut -c1-23)...; a certificate for each server's address; APIs $(api_urls)"
}

# token_to NAME: the admin token, from the server init made, to NAME's
# $SOAK_ETC/admin.token (0600), for the CLI there (docs/ha.md: a joined server
# writes no admin.token). The token goes through a pipe, not a file here.
token_to() {
	[ "$SOAK_API_TLS" = 1 ] || return 0
	on "$SERVER" "sudo cat $DATA/admin.token" | on "$1" "sudo install -d -m 0700 $SOAK_ETC && sudo sh -c 'umask 077; cat > $SOAK_ETC/admin.token'" ||
		die "$1: the admin token"
}

# join_server NAME: a server joined with a server token (docs/ha.md#membership):
# hivemind join <first server> --role server --token T --advertise <its
# address> in the foreground until the servers list it as a voter, then the
# unit, serve --advertise <its address> with the API's and the router's
# flags, as the first server's.
join_server() {
	local n=$1 tok ip
	ip=$(ip_of "$n")
	# shellcheck disable=SC2086 # SERVER_TOKEN_FLAGS is words
	tok=$(hma join-token --ttl 1h $SERVER_TOKEN_FLAGS 2>/dev/null) ||
		die "join-token --ttl 1h $SERVER_TOKEN_FLAGS: this build gives no server join token (docs/ha.md): $(hma join-token --ttl 1h $SERVER_TOKEN_FLAGS 2>&1 >/dev/null | head -n 2 | tr '\n' ' ')"
	token_to "$n"
	note "joining $n as a server"
	on "$n" "sudo sh -c 'read -r T; nohup $HM join $SERVER_IP --role server --token \"\$T\" --advertise $ip$JOIN_SERVER_FLAGS >/var/log/hivemind-join.log 2>&1 </dev/null &'" <<<"$tok"
	wait_for 180 server_voter "$n" || die "$n is not a voting server 180 s after its join: role=$(server_role "$n") $(on "$n" 'sudo tail -n 5 /var/log/hivemind-join.log' 2>&1 | tr '\n' ' ')"
	ev setup "$n joined as a server: role $(server_role "$n"); data $(on "$n" "sudo ls $DATA | tr '\n' ' '")"
	on "$n" "sudo pkill -TERM -f '^$HM join' ; for i in \$(seq 1 40); do pgrep -f '^$HM join' >/dev/null || exit 0; sleep 0.5; done; exit 1" ||
		die "$n: the foreground join did not stop"
	install_unit "$n" "$(server_unit "$ip")"
	ev setup "$n unit ExecStart=$(on "$n" "systemctl show -p ExecStart --value hivemind.service | sed -n 's/.*argv\[\]=\([^;]*\);.*/\1/p'")"
	wait_for 120 server_voter "$n" || die "$n is not a voting server after its unit started: role=$(server_role "$n")"
	wait_for 60 hm "$n" status || die "$n: its API does not answer"
	if [ "$SOAK_ROUTES" = 1 ]; then
		wait_for 90 router_up_id "$n" || die "$n: no router after the unit started: $(hma nodes 2>&1 | tr -s ' \n' ' ')"
		ev setup "$n router up; listening $(on "$n" "sudo ss -ltnH '( sport = :80 or sport = :443 )'" | awk '{ printf "%s ", $4 }')"
	fi
	ev setup "servers $(servers_line)"
}

# ports_check: before hivemind runs, whether the security group lets the
# machines reach what this run needs: with several servers the server port,
# 4486, from every other server; with SOAK_API_TLS=1 the API, 4483, from
# the client machine ($WL_CLIENT, default the outside machine) and the
# servers. A python3 listener on the first server's private address for
# 40 s, and a new TCP connection from each other machine to each port, 2 s
# at most. One "ports-check" line per machine; it fails when a needed path
# is closed, and names the rule to add. PORTS_CHECK=0 skips it.
read -r -d '' REMOTE_LISTEN <<'EOF' || true
import socket, sys, threading
ip = sys.argv[1]
socks = []
for p in sys.argv[2:]:
    s = socket.socket()
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    s.bind((ip, int(p)))
    s.listen(64)
    socks.append(s)
def serve(s):
    while True:
        c, _ = s.accept()
        c.close()
for s in socks:
    threading.Thread(target=serve, args=(s,), daemon=True).start()
threading.Event().wait()
EOF
ports_check() {
	local ports="" p n res line bad="" client=${WL_CLIENT:-$OUT_NAME} m
	[ "${PORTS_CHECK:-1}" = 1 ] || return 0
	[ "$SOAK_SERVERS" -gt 1 ] && ports+=" 4486"
	[ "$SOAK_API_TLS" = 1 ] && ports+=" $API_PORT"
	[ -n "$ports" ] || return 0
	on "$SERVER" "cat > /tmp/soak-listen.py" <<<"$REMOTE_LISTEN" || die "ports_check: listener to $SERVER"
	on "$SERVER" "setsid nohup timeout 40 python3 /tmp/soak-listen.py $SERVER_IP$ports >/tmp/soak-listen.log 2>&1 </dev/null &" || die "ports_check: listener on $SERVER"
	sleep 2
	for n in "${NAMES[@]:1}" $OUT_NAME; do
		line=""
		for p in $ports; do
			if on "$n" "timeout 2 bash -c 'exec 3<>/dev/tcp/$SERVER_IP/$p'" >/dev/null 2>&1; then
				res=open
			else
				case $? in 124) res=dropped ;; *) res=refused ;; esac
			fi
			line+=" $p=$res"
			[ "$res" = open ] && continue
			# which closed paths matter
			if { [ "$p" = 4486 ] && is_server "$n"; } || { [ "$p" = "$API_PORT" ] && { [ "$n" = "$client" ] || is_server "$n"; }; }; then
				bad+=" $n->$SERVER_IP:$p"
			fi
		done
		ev ports-check "$n to $SERVER ($SERVER_IP):$line"
	done
	m=$(on "$SERVER" "cat /tmp/soak-listen.log; pkill -f '[s]oak-listen.py'; rm -f /tmp/soak-listen.py /tmp/soak-listen.log" 2>/dev/null | tr '\n' ' ')
	[ -z "$m" ] || ev ports-check "listener said: $m"
	[ -z "$bad" ] || die "ports_check: closed:$bad. The soak security group must allow TCP 4486 and $API_PORT from itself (aws ec2 authorize-security-group-ingress --group-id <hivemind-soak-sg> --ip-permissions 'IpProtocol=tcp,FromPort=4486,ToPort=4486,UserIdGroupPairs=[{GroupId=<hivemind-soak-sg>}]' 'IpProtocol=tcp,FromPort=$API_PORT,ToPort=$API_PORT,UserIdGroupPairs=[{GroupId=<hivemind-soak-sg>}]')"
	ev ports-check "ok:$ports"
}
