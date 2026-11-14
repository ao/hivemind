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
	timeout "${SSH_TIMEOUT:-10}" ssh "${SSH_OPTS[@]}" -o ConnectTimeout="${SSH_CT:-3}" "$SSH_USER@$ip" "$@"
}

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
# client config, which init and restore write.
hm() {
	local n=$1
	shift
	on "$n" "sudo $HM $*"
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
	local f=$1 want_nodes=${2:-} tmp=$1.tmp out i n pids=()
	out=$(on "$SERVER" "sudo $HM status --json && echo '#nodes' && { [ -z '$want_nodes' ] || sudo $HM nodes --json; }" 2>/dev/null) &&
		[[ "$out" == *'#nodes'* ]] || out=
	if [ -n "$out" ] && printf '%s\n' "${out%%#nodes*}" | status_rows >"$tmp" 2>/dev/null; then
		echo "S	ok" >>"$tmp"
	else
		echo "S	fail" >"$tmp"
	fi
	if [ -n "$want_nodes" ] && [ -n "$out" ]; then
		printf '%s\n' "${out#*#nodes}" | jq -r '.[] | ["L", .id, .state, (.lease_age_seconds|tostring)] | @tsv' >>"$tmp" 2>/dev/null || true
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
ExecStart=/usr/local/bin/hivemind serve --advertise $1
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
}

worker_unit() {
	cat <<'EOF'
[Unit]
Description=hivemind worker
Documentation=https://github.com/ao/hivemind
Wants=network-online.target docker.service
After=network-online.target docker.service

[Service]
ExecStart=/usr/local/bin/hivemind serve --role worker
Restart=on-failure
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

VOLUME_APPS=${VOLUME_APPS:-kv}

# running FILE: "running/desired": replicas the server reports running, of
# those that should run. The convergence check also accepts a replica that is
# pending with a placement reason; this count does not.
running() { awk -F'\t' '$1 == "R" && $9 == "running" { n++; if ($5 == "running") r++ } END { printf "%d/%d", r, n }' "$1"; }
