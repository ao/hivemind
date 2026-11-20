#!/usr/bin/env bash
# The numbers of the scenarios of several servers (ha.sh), from the soak log
# and the write-load's logs.
#
#   scripts/soak/ha-report.sh LOG WL_DIR
#
# WL_DIR has what wload.sh fetch copies: probe.log, direct.log and TAG.log for
# each scenario. Needs gawk.
#
# Per scenario window (its "scenario NAME begin" line to its "end" line, the
# fault at the first "event NAME ... fault" line):
#   - the times after the fault of its events: port 22 down and up, a new
#     leader, the old leader stepping down, no leader, a leader again, the
#     return asked for, the old leader back as a follower, convergence lost
#     and regained, and any replica running on two nodes;
#   - the API as the client that knows every server saw it (probe.log): reads,
#     failed reads with their errors, and the longest time without a
#     successful read, from the end of the last success before it to the end
#     of the first after it;
#   - each server's own API (direct.log): the codes it answered, and the
#     first body of a 503;
#   - the writes (TAG.log): made, acknowledged, failed with their errors, the
#     latency of the acknowledged ones (p50, p99, max), the longest time
#     between two acknowledged writes, and the first and last failure after
#     the fault;
#   - the write-load's check of every server's store, and the journals.
# Times in the log are the control machine's clock; the write-load's the
# client machine's; both are synchronised by chrony.
set -euo pipefail
LOG=${1:?log}
WL=${2:?write-load dir}

# name start end fault (Unix seconds)
gawk '
# a scenario run again: the last run counts
$3 == "scenario" && $5 == "begin" { if (!($4 in s)) order[++n] = $4; s[$4] = $2 / 1000; f[$4] = 0; delete e[$4] }
$3 == "event" && ($4 in s) && !($4 in e) && f[$4] == 0 && / fault / { f[$4] = $2 / 1000 }
$3 == "scenario" && $5 == "end" && ($4 in s) && !($4 in e) { e[$4] = $2 / 1000 }
END { for (i = 1; i <= n; i++) { w = order[i]; if (w in e) printf "%s %.3f %.3f %.3f\n", w, s[w], e[w], f[w] } }' "$LOG" |
	while read -r name ws we wf; do
		case "$name" in leader-kill | leader-partition | quorum-loss | service-restart) ;; *) continue ;; esac
		echo "### $name"
		echo
		echo "From $(date -u -d @"${ws%.*}" +%T) to $(date -u -d @"${we%.*}" +%T) UTC; fault at $(date -u -d @"${wf%.*}" +%T.%3N)."
		echo
		echo "Events, seconds after the fault:"
		echo
		gawk -v nm="$name" -v ws="$ws" -v we="$we" -v wf="$wf" '
			$3 != "event" || $4 != nm { next }
			{ t = $2 / 1000; if (t < ws || t > we) next; d = sprintf("%+.1f", t - wf); line = $0; sub(/^[^ ]+ [^ ]+ event [^ ]+ /, "", line) }
			line ~ / fault / || line ~ /^target / { print "- " d " s: " line; next }
			line ~ /port22-(down|up)$/ || line ~ /^(new-leader|leader-again|no-leader|down-phase-done|all-back|api-back|measure-timeout)/ ||
				line ~ / (stepped-down|rejoined-follower|return-asked|heal|heal-failed)/ { print "- " d " s: " substr(line, 1, 200); next }
			line ~ /^leader / { print "- " d " s: the leader a majority names: " $6; next }
			line ~ /^api [^ ]+ code=/ { print "- " d " s: " substr(line, 1, 240); next }
			line ~ /^converged-(yes|no)$/ { print "- " d " s: " line; next }
			line ~ /^dups / && $6 != "none" { print "- " d " s: TWICE: " line; next }
			line ~ /^node / { print "- " d " s: " line; next }
			line ~ /^containers / { print "- " d " s: " substr(line, 1, 240); next }
			line ~ /^write-check / { print "- write check: " $6; next }
		' "$LOG"
		echo
		if [ -s "$WL/probe.log" ]; then
			gawk -v ws="$ws" -v we="$we" -v wf="$wf" '
				$2 + 0 < ws || $2 + 0 > we { next }
				{
					n++; rc = $3; sub(/^rc=/, "", rc)
					if (rc == 0) {
						if (failing) { g = $2 - lastok; if (g > gap) { gap = g; ga = lastok; gb = $2 }; failing = 0 }
						lastok = $2 + 0
					} else {
						bad++; failing = 1; e = $4; sub(/^err=/, "", e); e = substr(e, 1, 90); ec[e]++
						if (!fb) fb = $1; lb = $1
					}
				}
				END {
					# still failing at the end of the window
					if (failing && lastok && we - lastok > gap) { gap = we - lastok; ga = lastok }
					printf "API through the client that knows every server (a read every 0.5 s): %d reads, %d failed", n, bad
					if (bad) printf ", first %+.1f s, last %+.1f s after the fault", fb - wf, lb - wf
					if (gap) printf "; longest without a successful read %.1f s, from %+.1f s", gap, ga - wf
					printf "\n\n"
					for (k in ec) printf "- %d x %s\n", ec[k], k
					if (bad) printf "\n"
				}' "$WL/probe.log"
		fi
		if [ -s "$WL/direct.log" ]; then
			gawk -v ws="$ws" -v we="$we" -v wf="$wf" '
				$1 + 0 < ws || $1 + 0 > we { next }
				{ for (i = 2; i <= NF; i++) { split($i, a, "="); s = a[1]; c = substr(a[2], 1, 3); k[s]++; cnt[s, c]++; codes[c] = 1
					if (c == 503 && !(s in f503)) { f503[s] = $1 - wf; b = $i; sub(/^[^:]*:[^:]*:/, "", b); body[s] = substr(b, 1, 200) } } }
				END {
					print "Each server'\''s own API, GET /v1/nodes every 0.5 s:"; print ""
					for (s in k) {
						printf "- %s:", s
						for (c in codes) if ((s, c) in cnt) printf " %s x %d", c, cnt[s, c]
						if (s in f503) printf "; first 503 at %+.1f s: %s", f503[s], body[s]
						printf "\n"
					}
					print ""
				}' "$WL/direct.log"
		fi
		# the write-load's tag in this window: the scenario's name, or NAME-r2, ... for a rerun
		tag=$(gawk -v ws="$ws" -v we="$we" '$3 == "wl" && $4 == "start" && $2 / 1000 >= ws && $2 / 1000 <= we { t = $5 } END { print t }' "$LOG")
		tag=${tag:-$name}
		if [ -s "$WL/$tag.log" ]; then
			gawk -v wf="$wf" '
				function pct(p,   i) { i = int(p * na + 0.999999); if (i < 1) i = 1; if (i > na) i = na; return sprintf("%.0f", lat[i] * 1000) }
				{
					n++; op[$3]++; rc = $6; sub(/^rc=/, "", rc)
					if (rc == 0) { ok++; okop[$3]++; l[++na] = $2 - $1; if (lastok) { g = $2 - lastok; if (g > gap) { gap = g; ga = lastok } }; lastok = $2 + 0 }
					else { bad++; e = $10; sub(/^err=/, "", e); ec[$3 " " substr(e, 1, 90)]++; if ($1 + 0 >= wf) { if (!fb) fb = $1; lb = $1 } }
				}
				END {
					asort(l, lat)
					printf "Writes: %d (%d deploys, %d scales, %d deletes), %d acknowledged (%d, %d, %d), %d failed", n, op["deploy"], op["scale"], op["delete"], ok, okop["deploy"], okop["scale"], okop["delete"], bad
					if (fb) printf ", first %+.1f s, last %+.1f s after the fault", fb - wf, lb - wf
					printf ".\n"
					if (na) printf "Acknowledged: p50 %s ms, p99 %s ms, max %s ms; longest between two %.1f s, from %+.1f s.\n", pct(0.5), pct(0.99), pct(1), gap, ga - wf
					printf "\n"
					for (k in ec) printf "- %d x %s\n", ec[k], k
					if (bad) printf "\n"
				}' "$WL/$tag.log"
		fi
		echo "Write check:"
		echo
		grep -E " wl check $tag |\bwl clean $tag " "$LOG" | sed 's/^[^ ]* [^ ]* wl /- /' | cut -c1-300 || echo "- none"
		echo
		grep -E " event $name journal " "$LOG" | sed 's/^[^ ]* [^ ]* event [^ ]* /- /' || true
		echo
	done
