# The convergence check of the fault suite and the soak harness. Input: a
# state file of tab-separated rows (see take_snapshot in fault-suite.sh):
#   N  node-id  node-name
#   A  app  desired-replicas
#   R  app  index  node-id  state  container-id  pending-reason  generation  desired
#   C  node-name  container-id(12)  state  started-at  app  replica  generation
# It prints "converged" and exits 0, or prints up to six reasons and exits 1.
#
# Converged: every replica is either running on the node the server assigns
# it, with that container running, or pending with no node and a placement
# reason; the server's status names the same container IDs the engines report;
# no container is left over; no (app, replica) pair is on two nodes.
#
# With -v allow_held=1, a replica that keeps its node because its volume is
# there, and whose node is unavailable, also counts (the documented state of a
# volume app while its node is lost).
function bad(msg) { if (nbad < 6) reasons[nbad] = msg; nbad++ }
$1 == "N" { name[$2] = $3; next }
$1 == "A" { want[$2] = $3; next }
$1 == "R" {
	k = $2 "|" $3
	rnode[k] = $4; rstate[k] = $5; rcid[k] = $6; rreason[k] = $7; rdes[k] = $9
	next
}
$1 == "C" {
	k = $6 "|" $7
	cstate[$2 "|" $3] = $4
	copies[k]++
	crow[++nc] = $2 "|" $3 "|" k
	next
}
END {
	for (k in rnode) {
		if (rdes[k] != "running") { bad(k " is desired " rdes[k]); continue }
		if (rstate[k] == "running") {
			key = name[rnode[k]] "|" substr(rcid[k], 1, 12)
			if (cstate[key] != "running") bad(k " is reported running as " substr(rcid[k], 1, 12) " on " rnode[k] ", but the engine does not run it")
		} else if (rstate[k] == "pending" && rnode[k] == "" && rreason[k] != "") {
			# expected: no node yet, with a placement reason
		} else if (allow_held && rnode[k] != "" && rreason[k] ~ /^volume (is on|pinned to) node .*unavailable/) {
			# expected with allow_held: a volume replica waits for its lost node
		} else {
			bad(k " state " rstate[k] " node [" rnode[k] "] reason [" rreason[k] "]")
		}
		split(k, p, "|")
		cnt[p[1]]++
	}
	for (a in want) {
		if (cnt[a] + 0 != want[a] + 0) bad("app " a " has " cnt[a] + 0 " replicas in the server, desired " want[a])
	}
	# Every container must be the one the server assigns to its replica on
	# its node: no stray container, and no replica on two nodes.
	for (i = 1; i <= nc; i++) {
		split(crow[i], c, "|")
		k = c[3] "|" c[4]
		if (!(k in rnode)) { bad("stray container " c[2] " on " c[1] " for " k); continue }
		if (name[rnode[k]] != c[1] || substr(rcid[k], 1, 12) != c[2]) bad("container " c[2] " on " c[1] " is not the one assigned to " k)
	}
	for (k in copies) {
		if (copies[k] > 1) bad("duplicate: " k " is on " copies[k] " containers")
	}
	if (nbad == 0) { print "converged"; exit 0 }
	for (i = 0; i < nbad && i < 6; i++) print reasons[i]
	exit 1
}
