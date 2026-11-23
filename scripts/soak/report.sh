#!/usr/bin/env bash
# The numbers, computed from the soak log.
#
#   scripts/soak/report.sh LOG
#
# 1. Durations. Each is "from" to "to": the first line matching FROM after the
#    window's start, then the first line matching TO after that. Times are the
#    log's Unix milliseconds, from the control machine's clock.
# 2. Probe results in each window: a scenario's window runs from its "begin"
#    line to its "end" line; the steady window from "mark steady begin" to
#    "mark steady end".
# 3. Resources in the steady window: the first and last sample of each node,
#    the CPU used over the window, and the least-squares slope of RSS.
set -euo pipefail
LOG=${1:?log file}

# label | window start | from | to   (extended regular expressions)
DURATIONS=$(
	cat <<'EOF'
kill-worker: command sent to port 22 closed|scenario kill-worker begin|mark kill-worker poweroff command|watch port22 hivemind-soak-3 down
kill-worker: port 22 closed to node lost|scenario kill-worker begin|watch port22 hivemind-soak-3 down|watch node hivemind-soak-3 lost
kill-worker: node lost to web/2 running elsewhere|scenario kill-worker begin|watch node hivemind-soak-3 lost|watch replica web.2 node=hivemind-soak-[124] state=running
kill-worker: node lost to spread/3 running elsewhere|scenario kill-worker begin|watch node hivemind-soak-3 lost|watch replica spread.3 node=hivemind-soak-[124] state=running
kill-worker: port 22 closed to converged|scenario kill-worker begin|watch port22 hivemind-soak-3 down|event kill-worker converged-yes
kill-worker: start command to port 22 open|scenario kill-worker begin|mark kill-worker start command|watch port22 hivemind-soak-3 up
kill-worker: start command to node ready|scenario kill-worker begin|mark kill-worker start command|watch node hivemind-soak-3 ready
kill-worker: node ready to old containers removed|scenario kill-worker begin|watch node hivemind-soak-3 ready|event kill-worker hivemind-soak-3 old-containers-removed
kill-worker: start command to converged|scenario kill-worker begin|mark kill-worker start command|event kill-worker converged-yes
reboot-worker: reboot to port 22 closed|scenario reboot-worker begin|event reboot-worker .* fault reboot|watch port22 hivemind-soak-4 down
reboot-worker: reboot to node lost|scenario reboot-worker begin|event reboot-worker .* fault reboot|watch node hivemind-soak-4 lost
reboot-worker: reboot to replicas elsewhere|scenario reboot-worker begin|event reboot-worker .* fault reboot|event reboot-worker .* replicas-elsewhere
reboot-worker: reboot to port 22 open|scenario reboot-worker begin|event reboot-worker .* fault reboot|watch port22 hivemind-soak-4 up
reboot-worker: reboot to node ready|scenario reboot-worker begin|event reboot-worker .* fault reboot|watch node hivemind-soak-4 ready
reboot-worker: reboot to old containers removed|scenario reboot-worker begin|event reboot-worker .* fault reboot|event reboot-worker .* old-containers-removed
reboot-worker: reboot to converged|scenario reboot-worker begin|event reboot-worker .* fault reboot|event reboot-worker converged-yes
reboot-worker: reboot to all replicas running|scenario reboot-worker begin|event reboot-worker .* fault reboot|event reboot-worker all-running-yes
partition-worker: partition to node lost|scenario partition-worker begin|event partition-worker .* fault partition|watch node hivemind-soak-2 lost
partition-worker: partition to replicas elsewhere|scenario partition-worker begin|event partition-worker .* fault partition|event partition-worker .* replicas-elsewhere
partition-worker: partition to first duplicate|scenario partition-worker begin|event partition-worker .* fault partition|event partition-worker dups [^n]
partition-worker: heal to node ready|scenario partition-worker begin|event partition-worker .* heal|watch node hivemind-soak-2 ready
partition-worker: heal to no duplicate|scenario partition-worker begin|event partition-worker .* heal|event partition-worker dups none
partition-worker: heal to old containers removed|scenario partition-worker begin|event partition-worker .* heal|event partition-worker .* old-containers-removed
partition-worker: heal to converged|scenario partition-worker begin|event partition-worker .* heal|event partition-worker converged-yes
partition-worker: partition to all replicas running|scenario partition-worker begin|event partition-worker .* fault partition|event partition-worker all-running-yes
lose-server: replace command to API down|scenario lose-server begin|mark lose-server replace command|event lose-server api-down
lose-server: API down to machine gone (port 22)|scenario lose-server begin|event lose-server api-down|event lose-server machine-gone
lose-server: API down to new machine ready|scenario lose-server begin|event lose-server api-down|event lose-server machine-ready
lose-server: new machine ready to restored|scenario lose-server begin|event lose-server machine-ready|event lose-server restored
lose-server: restored to service started|scenario lose-server begin|event lose-server restored|event lose-server service-started
lose-server: service started to API up|scenario lose-server begin|event lose-server service-started|event lose-server api-up
lose-server: API down to API up (outage)|scenario lose-server begin|event lose-server api-down|event lose-server api-up
lose-server: API up to hivemind-soak-2 ready|scenario lose-server begin|event lose-server api-up|event lose-server hivemind-soak-2 node-ready
lose-server: API up to hivemind-soak-3 ready|scenario lose-server begin|event lose-server api-up|event lose-server hivemind-soak-3 node-ready
lose-server: API up to hivemind-soak-4 ready|scenario lose-server begin|event lose-server api-up|event lose-server hivemind-soak-4 node-ready
lose-server: API up to converged|scenario lose-server begin|event lose-server api-up|event lose-server converged-yes
lose-server: API up to all replicas running|scenario lose-server begin|event lose-server api-up|event lose-server all-running-yes
EOF
)

echo "## Durations"
echo
echo "| Measure | From (UTC) | To (UTC) | Seconds |"
echo "|---|---|---|---|"
awk -v specs="$DURATIONS" '
BEGIN {
	n = split(specs, lines, "\n")
	for (i = 1; i <= n; i++) {
		m = split(lines[i], f, "|")
		lab[i] = f[1]; win[i] = f[2]; from[i] = f[3]; to[i] = f[4]
		stage[i] = 0
	}
	ns = n
}
{
	for (i = 1; i <= ns; i++) {
		if (stage[i] == 0 && $0 ~ win[i]) stage[i] = 1
		if (stage[i] == 1 && $0 ~ from[i]) { stage[i] = 2; t1[i] = $2; s1[i] = $1; continue }
		if (stage[i] == 2 && $0 ~ to[i]) { stage[i] = 3; t2[i] = $2; s2[i] = $1 }
	}
}
END {
	for (i = 1; i <= ns; i++) {
		if (stage[i] == 3) printf "| %s | %s | %s | %.1f |\n", lab[i], substr(s1[i], 12), substr(s2[i], 12), (t2[i] - t1[i]) / 1000
		else if (stage[i] >= 1) printf "| %s | %s | not seen | - |\n", lab[i], (stage[i] == 2 ? substr(s1[i], 12) : "not seen")
	}
}' "$LOG"

echo
echo "## Probes by window"
echo
echo "| Window | Start | Length (s) | Status probes ok | Converged | All replicas running | Duplicates seen | web reach ok | kv reach ok | Fast web reach ok | GET /v1/status p50 / p99 / max (ms) |"
echo "|---|---|---|---|---|---|---|---|---|---|---|"
awk '
function pct(a, b) { return b ? sprintf("%d/%d", a, b) : "-" }
function f(k,   i) { for (i = 4; i <= NF; i++) if (index($i, k "=") == 1) return substr($i, length(k) + 2); return "" }
function start(w, t, s) { if (open[w]) return; open[w] = 1; ws[w] = t; wiso[w] = s; order[++nw] = w }
function stop(w, t) { open[w] = 0; we[w] = t }
function q(w, p,   k, i, j, x, n) {
	n = nl[w]; if (!n) return "-"
	for (i = 1; i <= n; i++) x[i] = lat[w, i]
	for (i = 2; i <= n; i++) { k = x[i]; for (j = i - 1; j >= 1 && x[j] > k; j--) x[j + 1] = x[j]; x[j + 1] = k }
	i = int(p * n + 0.999999); if (i < 1) i = 1; if (i > n) i = n
	return sprintf("%.1f", x[i] * 1000)
}
BEGIN { start("all", 0, "-") }
$3 == "scenario" && $5 == "begin" { start($4, $2, $1) }
$3 == "scenario" && $5 == "end" { stop($4, $2) }
$3 == "mark" && $4 == "steady" && $5 == "begin" { start("steady", $2, $1) }
$3 == "mark" && $4 == "steady" && $5 == "end" { stop("steady", $2) }
{ last = $2; if (!first) first = $2 }
{
	for (w in open) if (open[w]) {
		if ($3 == "probe") { np[w]++; if (f("api") == "ok") ok[w]++; if (f("converged") == "yes") cv[w]++; split(f("running"), rr, "/"); if (rr[1] == rr[2] && rr[2] > 0) ar[w]++; if (f("dups") != "0") dp[w]++ }
		if ($3 == "reach") { if ($4 == "app=web") { rw[w]++; if ($7 == "ok") rwo[w]++ } if ($4 == "app=kv") { rk[w]++; if ($7 == "ok") rko[w]++ } }
		if ($3 == "reach-fast") { rf[w]++; if ($6 == "ok") rfo[w]++ }
		if ($3 == "lat" && $4 == "code=200") { sub(/^s=/, "", $5); lat[w, ++nl[w]] = $5 + 0 }
		if ($3 == "lat" && $4 != "code=200") latf[w]++
	}
}
END {
	ws["all"] = first; we["all"] = last
	for (i = 1; i <= nw; i++) {
		w = order[i]; e = (we[w] ? we[w] : last)
		printf "| %s | %s | %.0f | %s | %s | %s | %d | %s | %s | %s | %s / %s / %s%s |\n", w, substr(wiso[w], 12), (e - ws[w]) / 1000,
			pct(ok[w], np[w]), pct(cv[w], np[w]), pct(ar[w], np[w]), dp[w], pct(rwo[w], rw[w]), pct(rko[w], rk[w]), pct(rfo[w], rf[w]),
			q(w, 0.5), q(w, 0.99), q(w, 1), (latf[w] ? sprintf(" (%d failed)", latf[w]) : "")
	}
}' "$LOG"

echo
echo "## Resources in the steady window"
echo
echo "| Node | Samples | RSS first / last / max (MiB) | RSS slope (KiB/h) | Threads first / last | FDs first / last / max | CPU (% of one CPU) | Store first / last (KiB) | WAL first / last (KiB) | Restarts |"
echo "|---|---|---|---|---|---|---|---|---|---|---|"
awk '
function kv(s, k,   i, a, n) { n = split(s, a, " "); for (i = 1; i <= n; i++) if (index(a[i], k "=") == 1) return substr(a[i], length(k) + 2); return "" }
$3 == "mark" && $4 == "steady" && $5 == "begin" { on = 1; next }
$3 == "mark" && $4 == "steady" && $5 == "end" { on = 0; next }
on && $3 == "res" && $5 != "fail" && $5 != "pid=0" {
	n = $4; sub(/^node=/, "", n); if (!(n in cnt)) names[++nn] = n
	t = $2 / 1000; rss = kv($0, "rss_kb"); cpu = (kv($0, "utime") + kv($0, "stime")) / kv($0, "clk_tck")
	pid = kv($0, "pid")
	c = ++cnt[n]
	if (c == 1) { t0[n] = t; rss0[n] = rss; th0[n] = kv($0, "threads"); fd0[n] = kv($0, "fds"); cpu0[n] = cpu; db0[n] = kv($0, "db_bytes"); wal0[n] = kv($0, "db-wal_bytes"); pid0[n] = pid }
	else if (pid != lpid[n]) { restarts[n]++; cpubase[n] += lcpu[n] - cpu0[n]; cpu0[n] = cpu }
	lpid[n] = pid; lcpu[n] = cpu
	t1[n] = t; rss1[n] = rss; th1[n] = kv($0, "threads"); fd1[n] = kv($0, "fds"); cpu1[n] = cpu; db1[n] = kv($0, "db_bytes"); wal1[n] = kv($0, "db-wal_bytes")
	if (rss > mx[n]) mx[n] = rss; if (kv($0, "fds") + 0 > fmx[n]) fmx[n] = kv($0, "fds") + 0
	# least squares of rss (KiB) on time (h)
	x = (t - t0[n]) / 3600; sx[n] += x; sy[n] += rss; sxx[n] += x * x; sxy[n] += x * rss
}
END {
	for (i = 1; i <= nn; i++) {
		n = names[i]; c = cnt[n]
		slope = (c > 1 && (c * sxx[n] - sx[n] * sx[n]) > 0) ? (c * sxy[n] - sx[n] * sy[n]) / (c * sxx[n] - sx[n] * sx[n]) : 0
		el = t1[n] - t0[n]; used = cpubase[n] + cpu1[n] - cpu0[n]
		printf "| %s | %d | %.1f / %.1f / %.1f | %+.0f | %s / %s | %s / %s / %d | %.2f | %s | %s | %d |\n", n, c,
			rss0[n] / 1024, rss1[n] / 1024, mx[n] / 1024, slope, th0[n], th1[n], fd0[n], fd1[n], fmx[n],
			(el > 0 ? 100 * used / el : 0),
			(db0[n] != "" ? sprintf("%.0f / %.0f", db0[n] / 1024, db1[n] / 1024) : "-"),
			(wal0[n] != "" ? sprintf("%.0f / %.0f", wal0[n] / 1024, wal1[n] / 1024) : "-"), restarts[n]
	}
}' "$LOG"

echo
echo "## Duplicates, failures and unexpected events"
echo
awk '$3 == "dup" || $3 == "fail" || ($3 == "reach" && $7 == "fail") || ($3 == "reach-fast" && $6 == "fail") || ($3 == "res" && $5 == "fail") || ($3 == "lat" && $4 != "code=200") || ($3 == "probe-loop" && $4 == "late") || ($3 == "event" && ($5 ~ /^(measure-timeout|kv-value|workers-kept)/ || ($5 == "dups" && $6 != "none")))' "$LOG" | cut -c1-300
