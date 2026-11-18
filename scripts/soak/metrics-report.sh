#!/usr/bin/env bash
# The routers' request counters in the server's /metrics, against the
# clients' own logs of the same requests.
#
#   scripts/soak/metrics-report.sh SNAP1 SNAP2 CLIENT_DIR
#
# SNAP1 and SNAP2 are two files from "scenario.sh metrics-snap", each the
# hivemind_router_* lines of the server's /metrics and the time it was read
# ("# t_ms=..."). CLIENT_DIR has the stream logs HOST@ROUTER.log (outside.sh
# fetch). The node IDs are mapped to names with the run directory's ids file
# and to router addresses with its nodes file. Needs gawk.
#
# For each node and host: the increase of hivemind_router_requests_total
# with code 200 between the two reads, and the clients' 200s through that
# router for that host with an end time between the two reads, and the same
# 15 s earlier. A worker posts its counters every 15 s, so its increase
# should lie between those two counts, give or take the requests of the
# window's edges; the server's own router is read directly and should match
# the first.
set -euo pipefail
S1=${1:?first snapshot}
S2=${2:?second snapshot}
DIR=${3:?client dir}
RUN=$(dirname "$S1")
LAG=${LAG:-15}

echo "| Node | Host | Counter increase, code 200 | Clients' 200s between the reads | The same, $LAG s earlier | Counter minus clients |"
echo "|---|---|---|---|---|---|"
# The counters are by host, with no path label, so the streams of every path
# of a host through a router (HOST@IP.log, HOST+P@IP.log) are counted together.
LIST=$(for f in "$DIR"/*@*.log; do
	b=$(basename "$f" .log)
	h=${b%@*}
	printf '%s|%s|%s;' "${h%%+*}" "${b##*@}" "$f"
done)
gawk -v lag="$LAG" -v list="$LIST" '
BEGIN {
	n = split(list, L, ";")
	for (i = 1; i <= n; i++) if (split(L[i], p, "|") == 3) files[p[1] SUBSEP p[2]] = files[p[1] SUBSEP p[2]] (files[p[1] SUBSEP p[2]] == "" ? "" : "\t") p[3]
}
function labels(s, out,   n, a, i, kv) {
	delete out
	n = split(s, a, ",")
	for (i = 1; i <= n; i++) { split(a[i], kv, "="); gsub(/"/, "", kv[2]); out[kv[1]] = kv[2] }
}
FILENAME == ARGV[1] { name[$1] = $2; next }            # ids: id name
FILENAME == ARGV[2] { ip[$1] = $2; next }              # nodes: name ip
/^# t_ms=/ { t[FILENAME] = substr($2, 6) / 1000; next }
/^hivemind_router_requests_total\{/ {
	s = $1; sub(/^[^{]*\{/, "", s); sub(/\}$/, "", s)
	labels(s, L)
	if (L["code"] != "200") next
	k = L["node"] SUBSEP L["host"]
	if (FILENAME == ARGV[3]) v1[k] = $2; else { v2[k] = $2; keys[k] = 1 }
}
END {
	t1 = t[ARGV[3]]; t2 = t[ARGV[4]]
	printf "Reads at %s and %s UTC, %.0f s apart.\n\n", strftime("%H:%M:%S", int(t1), 1), strftime("%H:%M:%S", int(t2), 1), t2 - t1 > "/dev/stderr"
	for (k in keys) {
		split(k, p, SUBSEP); id = p[1]; host = p[2]
		nm = (id in name ? name[id] : id); addr = (nm in ip ? ip[nm] : "")
		d = v2[k] - (k in v1 ? v1[k] : 0)
		a = 0; b = 0
		nf = (addr != "" ? split(files[host SUBSEP addr], F, "\t") : 0)
		for (j = 1; j <= nf; j++) {
			f = F[j]
			while ((getline line < f) > 0) {
				n = split(line, x, " ")
				if (n < 8 || x[2] != "200" || x[3] != "0") continue
				e = x[1] + 0
				if (e > t1 && e <= t2) a++
				if (e > t1 - lag && e <= t2 - lag) b++
			}
			close(f)
		}
		printf "| %s | %s | %d | %s | %s | %s |\n", nm, host, d, (nf ? a : "-"), (nf ? b : "-"), (nf ? sprintf("%+d", d - a) : "-")
	}
}' "$RUN/ids" "$RUN/nodes" "$S1" "$S2" | sort
