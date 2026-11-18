#!/usr/bin/env bash
# The numbers of the HTTPS clients (outside.sh), by window of the soak log.
#
#   scripts/soak/client-report.sh LOG CLIENT_DIR
#
# CLIENT_DIR has the stream logs HOST@ROUTER.log, certs.log and pebble.log,
# as outside.sh fetch copies them. Needs gawk.
#
# Windows: each scenario of LOG, from its "scenario NAME begin" line to its
# "scenario NAME end" line and 60 s more (less if the next window starts
# sooner), with its fault at the first "event NAME ... fault"
# line inside it; and the steady window, "mark steady begin" to "mark steady
# end". A request belongs to the window its end time falls in. The client
# times are the outside machine's clock, the log's the control machine's;
# both are synchronised by chrony.
#
# Per window and stream: requests; failed (not a 200, or a curl error), with
# the codes ("000/28" is curl's timeout, "000/7" a refused connection,
# "000/35" a failed TLS handshake);
# misrouted, a 200 from another app than a name-checked stream expects
# (outside.sh, HOST/PATH=NAME), with the app that answered; the successful
# requests that took 1.5 s or more (a router's 2 s dial timeout to a dead
# replica, then another replica);
# the longest gap between two successful requests (or from the window's
# first request, or to its last), and when it began; the
# first and last failure, in seconds after the fault; and the total time of
# the successful requests, p50, p99 and max, in ms. A stream with no request
# in the window has no row. For the steady window, the same over every
# stream, and per host and path over the routers.
set -euo pipefail
LOG=${1:?log}
DIR=${2:?client dir}
NODES_FILE=${NODES_FILE:-$(dirname "$LOG")/nodes}
W=$(mktemp)
trap 'rm -f "$W"' EXIT

# name start end fault (Unix seconds; fault 0 when there is none)
awk '
$3 == "scenario" && $5 == "begin" { s[$4] = $2 / 1000; order[++n] = $4; f[$4] = 0 }
$3 == "event" && ($4 in s) && !($4 in e) && f[$4] == 0 && / fault / { f[$4] = $2 / 1000 }
$3 == "scenario" && $5 == "end" && ($4 in s) && !($4 in e) { e[$4] = $2 / 1000 }
$3 == "mark" && $4 == "steady" && $5 == "begin" { s["steady"] = $2 / 1000; order[++n] = "steady"; f["steady"] = 0 }
$3 == "mark" && $4 == "steady" && $5 == "end" { e["steady"] = $2 / 1000 }
END {
	for (i = 1; i <= n; i++) {
		w = order[i]; if (!(w in e)) continue
		# 60 s past the end, for failures that outlast the measure, but not into the next window
		x = e[w] + 60; if (i < n && s[order[i + 1]] < x) x = s[order[i + 1]]; if (w == "steady") x = e[w]
		printf "%s %.3f %.3f %.3f\n", w, s[w], x, f[w]
	}
}' "$LOG" >"$W"

label() { # label FILE: host/path@node-name (a log HOST+P+Q@IP.log is the path /P/Q)
	local b ip host
	b=$(basename "$1" .log)
	host=${b%@*} ip=${b#*@}
	host=${host//+//}
	[[ "$host" == */* ]] || host=$host/
	echo "$host@$(awk -v ip="$ip" '$2 == ip { print $1 }' "$NODES_FILE" | sed 's/^.*-\([0-9]*\)$/\1/;s/^/node-/')"
}

while read -r name ws we wf; do
	echo "### $name"
	echo
	echo "From $(date -u -d @"${ws%.*}" +%T) to $(date -u -d @"${we%.*}" +%T) UTC, $(awk -v a="$ws" -v b="$we" 'BEGIN { printf "%.0f", b - a }') s$( [ "$wf" != 0.000 ] && echo ", fault at $(date -u -d @"${wf%.*}" +%T)")."
	echo
	echo "| Stream (host/path@router) | Requests | Failed | Codes of the failures | Misrouted | Over 1.5 s | Longest gap (s) | Gap began | First / last failure (s after the fault) | p50 / p99 / max (ms) |"
	echo "|---|---|---|---|---|---|---|---|---|---|"
	for f in "$DIR"/*@*.log; do
		gawk -v ws="$ws" -v we="$we" -v wf="$wf" -v lab="$(label "$f")" '
		function pct(p,   i) { i = int(p * nl + 0.999999); if (i < 1) i = 1; if (i > nl) i = nl; return sprintf("%.0f", lat[i] * 1000) }
		NF < 8 || $2 !~ /^[0-9][0-9][0-9]$/ || $1 + 0 < ws || $1 + 0 > we { next }
		{
			t = $1 + 0; n++; if (!t0) t0 = t - $7; tl = t
			# a 200 from another app than the stream expects (name-checked streams)
			if ($9 ~ /^misroute=/) { mis++; mn[substr($9, 10)]++ }
			if ($2 == "200" && $3 == "0" && $7 + 0 >= 1.5) slow++
			if ($2 == "200" && $3 == "0") {
				if (lastok) { g = t - lastok } else { g = t - t0 }
				if (g > gap) { gap = g; gapat = (lastok ? lastok : t0) }
				lastok = t; lat[++nl] = $7 + 0
			} else {
				bad++; codes[$2 "/" $3]++
				if (!ff) ff = t - $7
				lf = t
			}
		}
		END {
			if (!n) exit
			# A gap still open at the end of the window counts up to the last request.
			if (n && lf > lastok) { g = tl - (lastok ? lastok : t0); if (g > gap) { gap = g; gapat = (lastok ? lastok : t0) } }
			c = ""; for (k in codes) c = c (c == "" ? "" : ", ") k " x" codes[k]
			m = ""; for (k in mn) m = m (m == "" ? "" : ", ") k " x" mn[k]
			if (nl) asort(lat)
			ref = (wf > 0 ? wf : ws)
			printf "| %s | %d | %d | %s | %s | %d | %.2f | %s | %s | %s |\n", lab, n, bad, (c == "" ? "-" : c), (mis ? mis " (" m ")" : "0"), slow, gap,
				(gapat ? strftime("%H:%M:%S", int(gapat), 1) : "-"),
				(bad ? sprintf("%+.1f / %+.1f", ff - ref, lf - ref) : "-"),
				(nl ? pct(0.5) " / " pct(0.99) " / " pct(1) : "-")
		}' "$f"
	done
	echo
done <"$W"

# The steady window over every stream: success rate and latency.
read -r _ ss se _ < <(grep '^steady ' "$W" || echo "- 0 0 0")
if [ "$ss" != 0 ]; then
	echo "### steady, all streams"
	echo
	# awk 1 ends each file with a newline, so a last line that curl had not
	# finished writing does not run into the next file's first.
	awk 1 "$DIR"/*@*.log | gawk -v ws="$ss" -v we="$se" '
	function pct(a, n, p,   i) { i = int(p * n + 0.999999); if (i < 1) i = 1; if (i > n) i = n; return sprintf("%.1f", a[i] * 1000) }
	NF < 8 || $2 !~ /^[0-9][0-9][0-9]$/ { partial++; next }
	$1 + 0 < ws || $1 + 0 > we { next }
	$9 ~ /^misroute=/ { n++; mis++; next }
	{ n++; if ($2 == "200" && $3 == "0") { ok++; tt[ok] = $7 + 0; px[ok] = $6 - $5; tl[ok] = $5 - $4 } }
	END {
		asort(tt); asort(px); asort(tl)
		printf "| Requests | Succeeded | Success rate | Misrouted (a 200 from another app) |\n|---|---|---|---|\n| %d | %d | %.5f %% | %d |\n\n", n, ok, (n ? 100 * ok / n : 0), mis
		printf "Lines skipped as incomplete, in all the logs (the last line of a log that curl had not finished writing when it was read or stopped): %d.\n\n", partial
		printf "| Time, ms | p50 | p90 | p99 | p99.9 | max |\n|---|---|---|---|---|---|\n"
		printf "| Total (TCP + TLS handshake + request) | %s | %s | %s | %s | %s |\n", pct(tt, ok, .5), pct(tt, ok, .9), pct(tt, ok, .99), pct(tt, ok, .999), pct(tt, ok, 1)
		printf "| TLS handshake | %s | %s | %s | %s | %s |\n", pct(tl, ok, .5), pct(tl, ok, .9), pct(tl, ok, .99), pct(tl, ok, .999), pct(tl, ok, 1)
		printf "| TLS done to first byte (router and replica) | %s | %s | %s | %s | %s |\n", pct(px, ok, .5), pct(px, ok, .9), pct(px, ok, .99), pct(px, ok, .999), pct(px, ok, 1)
	}'
	echo
	# The same per host and path, over the routers.
	echo "| Host and path | Streams | Requests | Succeeded | Success rate | Misrouted | Over 1.5 s | p50 / p99 / p99.9 / max (ms) |"
	echo "|---|---|---|---|---|---|---|---|"
	for p in $(for f in "$DIR"/*@*.log; do b=$(basename "$f" .log); echo "${b%@*}"; done | sort -u); do
		awk 1 "$DIR/$p"@*.log | gawk -v ws="$ss" -v we="$se" -v lab="$(tr + / <<<"$p" | sed 's|^[^/]*$|&/|')" -v k="$(ls "$DIR/$p"@*.log | wc -l)" '
		function pct(p,   i) { i = int(p * ok + 0.999999); if (i < 1) i = 1; if (i > ok) i = ok; return sprintf("%.1f", tt[i] * 1000) }
		NF < 8 || $2 !~ /^[0-9][0-9][0-9]$/ { next }
		$1 + 0 < ws || $1 + 0 > we { next }
		$9 ~ /^misroute=/ { n++; mis++; next }
		{ n++; if ($2 == "200" && $3 == "0") { ok++; tt[ok] = $7 + 0; if ($7 + 0 >= 1.5) slow++ } }
		END {
			if (!n) exit
			asort(tt)
			printf "| %s | %d | %d | %d | %.5f %% | %d | %d | %s / %s / %s / %s |\n", lab, k, n, ok, 100 * ok / n, mis, slow, pct(.5), pct(.99), pct(.999), pct(1)
		}'
	done
	echo
fi

# Certificates: each issuance by Pebble, and when each router served it.
if [ -f "$DIR/pebble.log" ] && [ -f "$DIR/certs.log" ]; then
	echo "### Certificates"
	echo
	echo "Pebble's issuances (its log, UTC), and when the certificate watcher first saw each router serve the new serial:"
	echo
	echo "| Issued (Pebble) | Host | Serial | First served by each router (s after issuance) |"
	echo "|---|---|---|---|"
	# The host of each serial is the one the watcher saw it served for.
	gawk -v names="$(awk '{ printf "%s=%s ", $2, $1 }' "$NODES_FILE")" '
	BEGIN { n = split(names, a, " "); for (i = 1; i <= n; i++) { split(a[i], b, "="); nm[b[1]] = b[2]; sub(/^.*-/, "node-", nm[b[1]]) } }
	FILENAME ~ /pebble.log$/ && /Issued certificate serial/ {
		match($0, /([0-9]{4})\/([0-9]{2})\/([0-9]{2}) ([0-9]{2}):([0-9]{2}):([0-9]{2})/, d)
		t = mktime(d[1] " " d[2] " " d[3] " " d[4] " " d[5] " " d[6], 1)
		match($0, /serial ([0-9a-f]+)/, s)
		sr = toupper(s[1]); sub(/^0+/, "", sr)
		ni++; it[ni] = t; is[ni] = sr; next
	}
	FILENAME ~ /certs.log$/ && $3 ~ /^serial=/ {
		sr = substr($3, 8); sub(/^0+/, "", sr); split($2, hp, "@"); rt[hp[2]] = 1; host[sr] = hp[1]
		k = sr "|" hp[2]
		if (!(k in seen)) seen[k] = $1 + 0
	}
	END {
		for (i = 1; i <= ni; i++) {
			sr = is[i]; w = ""
			for (r in rt) { k = sr "|" r; w = w (w == "" ? "" : ", ") (r in nm ? nm[r] : r) " " ((k in seen) ? sprintf("%+.0f", seen[k] - it[i]) : "never") }
			printf "| %s | %s | %s | %s |\n", strftime("%H:%M:%S", it[i], 1), (sr in host ? host[sr] : "never served"), substr(sr, 1, 12), w
		}
	}' "$DIR/pebble.log" "$DIR/certs.log"
	echo
	echo "Certificate checks that failed (no answer, or a chain that does not verify): $(awk '$3 == "fail"' "$DIR/certs.log" | wc -l) of $(wc -l <"$DIR/certs.log")."
fi
