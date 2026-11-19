#!/usr/bin/env bash
# The outside machine of the routing soak: a test ACME CA, its DNS, a private
# registry, and the HTTPS clients that measure the routers.
#
#   outside.sh setup          Pebble, pebble-challtestsrv and registry:2 on the
#                             outside machine; the private images pushed; the
#                             files prepare_node needs fetched to $OUT_FILES
#   outside.sh login          hivemind registry login on the server
#   outside.sh wait-served S  until every router serves every host (S seconds)
#   outside.sh client-start   the request streams and the certificate watcher
#   outside.sh client-add IP  streams to a new router at IP
#   outside.sh client-drop IP stop the streams to the router at IP
#   outside.sh chall-update   the routed hosts' DNS answers: every node now listed
#   outside.sh scratch-start  another cluster's server on the outside machine
#   outside.sh scratch-stop
#   outside.sh client-stop
#   outside.sh client-status  requests and failures so far, per stream
#   outside.sh fetch          the client logs, the certificate log and
#                             Pebble's log, to $SOAK_DIR/client/
#
# The outside machine is the one line of $OUTSIDE_FILE. It needs Docker,
# curl (8.6 or later, for -w %time{}) and openssl. Its listeners: Pebble's
# ACME API on its private address, port 14000; the registry on its private
# address, port 443; Pebble's management API and challtestsrv's on
# 127.0.0.1 only. Nothing else is published.
# With OUTSIDE_REGISTRY=0 there is no registry (and nothing on 443), so the
# outside machine can be a node that runs a router, as in the scale run.
#
# Pebble runs with its validation sleeps off, no nonce rejection, no
# authorization reuse (every order is validated again, over HTTP-01 on port
# 80 of a router), and a default certificate lifetime of $CERT_LIFETIME
# seconds, so the routers renew during the run. challtestsrv answers every
# routed host with the private address of every node, so Pebble's
# validation can reach any router. The clients trust Pebble's root, which
# Pebble makes at its start: Pebble must not restart during the run.
#
# Clients: one stream per router and entry of STREAMS (HOST/PATH). Each is
# one curl process making $STREAM_RATE requests a second (default 10/s) to
# https://HOST/PATH through that router,
# over HTTP/1.1 with a new TCP connection and a full TLS handshake each time
# (--http1.1, Connection: close,
# no session reuse: over HTTP/2 curl would send every request on one
# connection), 3 s to connect, 10 s in all. Each request is one line of
# /var/lib/soak-client/HOST@ROUTER.log, or HOST+P+Q@ROUTER.log for the path
# /P/Q:
#   <end, Unix s.us> <http code> <curl exit code> <connect> <TLS done>
#   <first byte> <total> (seconds from the start) <connections> <error...>
# A stream with a NAME (HOST/PATH=NAME) reads each reply, and writes after
# <connections> "name=NAME" when whoami's "Name:" line names the app it
# expects, "misroute=OTHER" when it names another, and "name=-" when the
# reply has no such line (an error page, or no reply).
# The certificate watcher logs, every 30 s, the serial and expiry of the
# certificate each router serves for each host, from a fresh handshake; the
# routers are those in /var/lib/soak-client/routers, which client-add and
# client-drop change.
#
# With SOAK_GRPC=1 (the sixth soak), two gRPC clients per router as well, to
# the grpc app (lib.sh) at https://$PATH_HOST$GRPC_PATH:
#   - a unary call every $GRPC_EVERY_MS ms (200), each one run of
#     test/h2ctest's client ($H2CTEST, copied to the outside machine): a new
#     process, TCP connection and TLS handshake, HTTP/2 with ALPN, and its
#     checks (the message back, HTTP/2, grpc-status 0). One line per call in
#     /var/lib/soak-client/grpc/unary@ROUTER.log:
#       <end, Unix s.us> <exit code> <duration, us> <replica, or the error>
#   - a held server stream: h2ctest's Server method asked for
#     $GRPC_STREAM_N (1000) messages $GRPC_STREAM_MS (1000) ms apart, one
#     HTTP/2 request with curl (--trace-time gives each message's arrival),
#     and the next one when it ends, 1 s later. curl gives up after 10 s
#     with no data (--speed-time). Lines of grpc/stream@ROUTER.log:
#       <Unix s.us> start <seq>
#       <Unix s.us> msg <seq> <index> <replica>
#       <Unix s.us> end <seq> <http code> <curl exit> grpc=<grpc-status> msgs=<n> s=<total> <error...>
set -euo pipefail
. "$(dirname "$0")/lib.sh"

[ -n "$OUT_NAME" ] || die "no outside machine in $OUTSIDE_FILE"
if [ "$SOAK_PATHS" = 1 ]; then
	HOSTS=${ROUTED_HOSTS:-www.soak.test api.soak.test $PATH_HOST}
	# Every stream checks the app that answers (the seventh soak): www and the
	# path apps by whoami's "Name:" line; api's nginx has none, so its
	# replies are "name=-", and one with a "Name:" line came from a whoami app.
	STREAMS=${STREAMS:-www.soak.test/=www api.soak.test/=api $PATH_HOST/=proot $PATH_HOST/api/x=papi}
else
	HOSTS=${ROUTED_HOSTS:-www.soak.test api.soak.test}
fi
# One stream per router for each entry of STREAMS, "HOST/PATH" or
# "HOST/PATH=NAME"; with NAME, each reply must be whoami's from the app NAME.
STREAMS=${STREAMS:-$(printf '%s/ ' $HOSTS)}
CERT_LIFETIME=${CERT_LIFETIME:-3600}
# Requests a second on each stream (curl --rate).
STREAM_RATE=${STREAM_RATE:-10/s}
PEBBLE_IMAGE=${PEBBLE_IMAGE:-ghcr.io/letsencrypt/pebble:latest}
CHALL_IMAGE=${CHALL_IMAGE:-ghcr.io/letsencrypt/pebble-challtestsrv:latest}
REG_IMAGE=${REG_IMAGE:-registry:2}
HTPASSWD_IMAGE=${HTPASSWD_IMAGE:-httpd:2-alpine}
# The private images: NAME=SOURCE, pushed as $REGISTRY/soak/NAME.
PRIVATE_IMAGES=${PRIVATE_IMAGES:-whoami:1=traefik/whoami:v1.10.3 whoami:2=traefik/whoami:v1.11.0}
REG_USER=${REG_USER:-soak}
PW_FILE=$SOAK_DIR/registry.pw
CDIR=/var/lib/soak-client
# test/h2ctest, built for the outside machine and the nodes (linux/arm64):
# the grpc app's image and the gRPC clients.
H2CTEST=${H2CTEST:-$SOAK_DIR/h2ctest}
GRPC_EVERY_MS=${GRPC_EVERY_MS:-200}
GRPC_STREAM_N=${GRPC_STREAM_N:-1000}
GRPC_STREAM_MS=${GRPC_STREAM_MS:-1000}

out() { on "$OUT_NAME" "$@"; }

setup() {
	mkdir -p "$OUT_FILES"
	[ -s "$PW_FILE" ] || (umask 077 && openssl rand -hex 16 >"$PW_FILE")
	ev outside "setup begin $OUT_NAME $OUT_IP cert_lifetime=${CERT_LIFETIME}s hosts=$HOSTS"
	local reg=$OUTSIDE_REGISTRY
	SSH_TIMEOUT=600 out "set -e; for i in $PEBBLE_IMAGE $CHALL_IMAGE $([ "$reg" = 0 ] || echo "$REG_IMAGE $HTPASSWD_IMAGE"); do sudo docker pull -q \$i; done" >/dev/null
	# The registry's own CA and certificate, made here for the run.
	[ "$reg" = 0 ] || SSH_TIMEOUT=60 out "sudo bash -s" <<EOF
set -e
install -d -m 0755 /opt/soak && install -d -m 0700 /opt/soak/certs /opt/soak/auth
cd /opt/soak/certs
[ -s ca.crt ] || {
	openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 7 -subj '/CN=soak registry CA' \
		-addext basicConstraints=critical,CA:TRUE -addext keyUsage=critical,keyCertSign,cRLSign -keyout ca.key -out ca.crt 2>/dev/null
	openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -subj '/CN=$REGISTRY' -keyout reg.key -out reg.csr 2>/dev/null
	printf 'subjectAltName=DNS:$REGISTRY\nextendedKeyUsage=serverAuth\nbasicConstraints=CA:FALSE\n' >ext.cnf
	openssl x509 -req -in reg.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 7 -extfile ext.cnf -out reg.crt 2>/dev/null
}
EOF
	# htpasswd, from the password on stdin.
	[ "$reg" = 0 ] || out "sudo docker run --rm -i --entrypoint htpasswd $HTPASSWD_IMAGE -Bin $REG_USER | sudo tee /opt/soak/auth/htpasswd >/dev/null" <"$PW_FILE"
	# Pebble's config: HTTP-01 on port 80, and a short default lifetime.
	out "sudo install -d -m 0755 /opt/soak && sudo tee /opt/soak/pebble-config.json >/dev/null" <<EOF
{"pebble": {"listenAddress": "0.0.0.0:14000", "managementListenAddress": "0.0.0.0:15000",
 "certificate": "test/certs/localhost/cert.pem", "privateKey": "test/certs/localhost/key.pem",
 "httpPort": 80, "tlsPort": 443, "ocspResponderURL": "", "externalAccountBindingRequired": false,
 "domainBlocklist": ["blocked-domain.example"], "retryAfter": {"authz": 3, "order": 5}, "keyAlgorithm": "ecdsa",
 "profiles": {"default": {"description": "soak: short-lived", "validityPeriod": $CERT_LIFETIME}}}}
EOF
	SSH_TIMEOUT=120 out "set -e
		sudo docker network inspect soak >/dev/null 2>&1 || sudo docker network create soak >/dev/null
		sudo docker rm -f soak-chall soak-pebble soak-registry >/dev/null 2>&1 || true
		sudo docker run -d --name soak-chall --network soak -p 127.0.0.1:8055:8055 $CHALL_IMAGE \
			-defaultIPv4 $SERVER_IP -defaultIPv6 '' -http01 '' -https01 '' -tlsalpn01 '' -doh '' >/dev/null
		cip=\$(sudo docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' soak-chall)
		sudo docker run -d --name soak-pebble --network soak -p $OUT_IP:14000:14000 -p 127.0.0.1:15000:15000 \
			-e PEBBLE_VA_NOSLEEP=1 -e PEBBLE_WFE_NONCEREJECT=0 -e PEBBLE_AUTHZREUSE=0 \
			-v /opt/soak/pebble-config.json:/soak/pebble-config.json:ro $PEBBLE_IMAGE \
			-config /soak/pebble-config.json -dnsserver \$cip:8053 >/dev/null
		[ \"$reg\" = 0 ] || sudo docker run -d --name soak-registry --restart unless-stopped -p $OUT_IP:443:5000 \
			-v /opt/soak/certs:/certs:ro -v /opt/soak/auth:/auth:ro -v soak-registry:/var/lib/registry \
			-e REGISTRY_AUTH=htpasswd -e REGISTRY_AUTH_HTPASSWD_REALM=soak -e REGISTRY_AUTH_HTPASSWD_PATH=/auth/htpasswd \
			-e REGISTRY_HTTP_TLS_CERTIFICATE=/certs/reg.crt -e REGISTRY_HTTP_TLS_KEY=/certs/reg.key $REG_IMAGE >/dev/null"
	# The outside machine pushes to its own registry by name, as a node pulls.
	out "sudo sed -i '/ # soak-outside\$/d' /etc/hosts && echo '$OUT_IP pebble $REGISTRY # soak-outside' | sudo tee -a /etc/hosts >/dev/null"
	if [ "$reg" != 0 ]; then
		out "sudo install -d /etc/docker/certs.d/$REGISTRY && sudo cp /opt/soak/certs/ca.crt /etc/docker/certs.d/$REGISTRY/ca.crt"
		SSH_TIMEOUT=60 out "for i in \$(seq 1 30); do curl -s -o /dev/null --cacert /etc/docker/certs.d/$REGISTRY/ca.crt https://$REGISTRY/v2/ && exit 0; sleep 1; done; exit 1" ||
			die "the registry does not answer"
	fi
	SSH_TIMEOUT=60 out "sudo docker cp soak-pebble:/test/certs/pebble.minica.pem /opt/soak/pebble-minica.pem && sudo cat /opt/soak/pebble-minica.pem" >"$OUT_FILES/pebble-minica.pem"
	if [ "$reg" != 0 ]; then
		out "sudo cat /opt/soak/certs/ca.crt" >"$OUT_FILES/registry-ca.crt"
		push_images
	fi
	local h addrs
	addrs=$(printf '"%s",' "${IPS[@]}")
	for h in $HOSTS; do
		out "curl -fsS -d '{\"host\":\"$h\",\"addresses\":[${addrs%,}]}' http://127.0.0.1:8055/add-a" >/dev/null || die "challtestsrv did not take $h"
	done
	out "for i in \$(seq 1 30); do curl -fsS -o /dev/null --cacert /opt/soak/pebble-minica.pem https://127.0.0.1:15000/roots/0 && exit 0; sleep 1; done; exit 1" ||
		die "Pebble's management API does not answer"
	ev outside "pebble image=$(out "sudo docker inspect -f '{{.Image}} {{index .Config.Labels \"org.opencontainers.image.revision\"}}' soak-pebble") chall=$(out "sudo docker inspect -f '{{.Image}}' soak-chall") registry=$([ "$reg" = 0 ] && echo none || out "sudo docker inspect -f '{{.Image}}' soak-registry")"
	ev outside "listeners $(out "sudo ss -ltnH" | awk '{ printf "%s;", $4 }')"
	ev outside "setup end"
}

push_images() {
	local p name src i
	# The daemon reads /etc/hosts again at most every 5 s, so the first try
	# right after setup can miss the registry's name.
	for i in 1 2 3 4 5 6 7 8 9 10; do
		out "sudo docker login $REGISTRY -u $REG_USER --password-stdin >/dev/null 2>&1" <"$PW_FILE" && break
		[ "$i" = 10 ] && die "docker login to $REGISTRY"
		sleep 3
	done
	for p in $PRIVATE_IMAGES; do
		name=${p%%=*} src=${p#*=}
		SSH_TIMEOUT=300 out "set -e; sudo docker pull -q $src >/dev/null; sudo docker tag $src $REGISTRY/soak/$name; sudo docker push -q $REGISTRY/soak/$name >/dev/null" ||
			die "push $REGISTRY/soak/$name"
		ev outside "pushed $REGISTRY/soak/$name from $src $(out "sudo docker inspect -f '{{index .RepoDigests 0}}' $src")"
	done
	out "sudo docker logout $REGISTRY >/dev/null; sudo docker rmi $(for p in $PRIVATE_IMAGES; do printf '%s ' "$REGISTRY/soak/${p%%=*}"; done) >/dev/null"
	[ "$SOAK_GRPC" = 1 ] && push_h2ctest
	return 0
}

# push_h2ctest: test/h2ctest as an image, as docs/routing.md's test builds
# it (the static binary alone, ENTRYPOINT /h2ctest, docker import), pushed
# as $REGISTRY/soak/h2ctest:1 and :2, which differ only in a label; and the
# binary in $CDIR for the gRPC clients.
push_h2ctest() {
	local v
	[ -s "$H2CTEST" ] || die "no h2ctest binary at $H2CTEST"
	SSH_TIMEOUT=120 out "sudo install -d -m 0755 /opt/soak/h2c $CDIR $CDIR/grpc && cat >/tmp/h2ctest.new && sudo install -m 0755 /tmp/h2ctest.new /opt/soak/h2c/h2ctest && sudo install -m 0755 /tmp/h2ctest.new $CDIR/h2ctest && rm -f /tmp/h2ctest.new" <"$H2CTEST" ||
		die "copy h2ctest to $OUT_NAME"
	out "sudo docker login $REGISTRY -u $REG_USER --password-stdin >/dev/null 2>&1" <"$PW_FILE" || die "docker login to $REGISTRY"
	for v in 1 2; do
		SSH_TIMEOUT=120 out "set -e; sudo tar -C /opt/soak/h2c -cf - h2ctest | sudo docker import --change 'ENTRYPOINT [\"/h2ctest\"]' --change 'LABEL soak.h2ctest=$v' - $REGISTRY/soak/h2ctest:$v >/dev/null; sudo docker push -q $REGISTRY/soak/h2ctest:$v >/dev/null" ||
			die "push $REGISTRY/soak/h2ctest:$v"
		ev outside "pushed $REGISTRY/soak/h2ctest:$v from test/h2ctest sha256=$(sha256sum "$H2CTEST" | cut -d' ' -f1) $(out "sudo docker inspect -f '{{index .RepoDigests 0}}' $REGISTRY/soak/h2ctest:$v")"
	done
	out "sudo docker logout $REGISTRY >/dev/null; sudo docker rmi $REGISTRY/soak/h2ctest:1 $REGISTRY/soak/h2ctest:2 >/dev/null"
}

# login: the registry credential, stored on the server, as docs/registries.md says.
login() {
	hm "$SERVER" "registry login $REGISTRY --username $REG_USER --password-stdin" <"$PW_FILE" >/dev/null || die "registry login"
	ev outside "registry login $REGISTRY done: $(hm "$SERVER" registry list | tr -s ' \n' ' ')"
}

# root: Pebble's root, which the clients trust.
root() {
	out "sudo install -d -m 0755 $CDIR && curl -fsS --cacert /opt/soak/pebble-minica.pem https://127.0.0.1:15000/roots/0 | sudo tee $CDIR/root.pem >/dev/null"
}

# served_all: every router answers 200 for every host.
served_all() {
	local cmd="" ip h
	for ip in "${IPS[@]}"; do
		for h in $HOSTS; do
			cmd+="[ \"\$(curl -s -o /dev/null -w '%{http_code}' -m 5 --cacert $CDIR/root.pem --resolve $h:443:$ip https://$h/)\" = 200 ] || exit 1; "
		done
	done
	out "$cmd"
}

wait_served() {
	local t0
	t0=$(now_ms)
	root
	wait_for "${1:-300}" served_all || die "not every router serves every host after ${1:-300} s"
	ev outside "served: every router serves $HOSTS, $(($(now_ms) - t0)) ms after the wait began"
}

read -r -d '' STREAM <<'EOF' || true
#!/bin/bash
# stream.sh ROUTER_IP HOST [PATH [NAME]]: a request every 100 ms to
# https://HOST/PATH through ROUTER_IP; with NAME, each reply's "Name:" line is checked.
ip=$1 host=$2 path=${3:-/} name=${4:-} d=/var/lib/soak-client
tag=$(printf '%s' "${path#/}" | tr / +)
f=$d/$host${tag:++$tag}@$ip.log
w='%time{%s.%f} %{http_code} %{exitcode} %{time_connect} %{time_appconnect} %{time_starttransfer} %{time_total} %{num_connects}'
while :; do
	if [ -z "$name" ]; then
		curl -s --http1.1 --rate "${RATE:-10/s}" --no-sessionid -H 'Connection: close' --connect-timeout 3 -m 10 \
			--cacert $d/root.pem --resolve "$host:443:$ip" -o /dev/null -w "$w %{errormsg}\n" \
			"https://$host$path?n=[1-36000]" >>"$f" 2>/dev/null
	else
		# The body goes to awk, which keeps the "Name:" line and writes one line
		# per request when curl's marker line comes.
		curl -s --http1.1 --rate "${RATE:-10/s}" --no-sessionid -H 'Connection: close' --connect-timeout 3 -m 10 \
			--cacert $d/root.pem --resolve "$host:443:$ip" -w "\n@@soak@@ $w @@ %{errormsg}\n" \
			"https://$host$path?n=[1-36000]" 2>/dev/null |
			awk -v want="$name" '
			/^Name: / { got = $2; next }
			/^@@soak@@ / {
				sub(/^@@soak@@ /, ""); i = index($0, " @@ ")
				tag = (got == "" ? "name=-" : (got == want ? "name=" got : "misroute=" got))
				print substr($0, 1, i - 1), tag, substr($0, i + 4); fflush(); got = ""
			}' >>"$f"
	fi
done
EOF

read -r -d '' CERTWATCH <<'EOF' || true
#!/bin/bash
# certwatch.sh HOSTS: every 30 s, the certificate each router in
# /var/lib/soak-client/routers serves for each host.
d=/var/lib/soak-client
while :; do
	for ip in $(cat $d/routers); do
		for h in $1; do
			r=$(timeout 5 openssl s_client -connect "$ip:443" -servername "$h" -CAfile $d/root.pem -verify_return_error </dev/null 2>/dev/null |
				openssl x509 -noout -serial -enddate 2>/dev/null | tr '\n' ' ')
			echo "$(date -u +%s.%3N) $h@$ip ${r:-fail}"
		done
	done >>$d/certs.log
	sleep 30
done
EOF

read -r -d '' GRPC_UNARY <<'EOF' || true
#!/bin/bash
# grpc-unary.sh ROUTER_IP HOST EVERY_MS: a unary call to grpc every EVERY_MS
# ms through ROUTER_IP, each one run of h2ctest call (checked; a new
# connection); one line per call: <end, Unix s.us> <exit> <us> <replica or error>.
ip=$1 host=$2 every=$3 d=/var/lib/soak-client
f=$d/grpc/unary@$ip.log
while :; do
	t0=$EPOCHREALTIME
	o=$($d/h2ctest call -url "https://$host" -connect "$ip:443" -ca $d/root.pem unary 2>&1)
	rc=$?
	t1=$EPOCHREALTIME
	us=$((${t1/./} - ${t0/./}))
	echo "$t1 $rc $us ${o//$'\n'/ }" >>"$f"
	s=$((every * 1000 - us))
	[ "$s" -le 0 ] || sleep "$((s / 1000000)).$(printf '%06d' $((s % 1000000)))"
done
EOF

read -r -d '' GRPC_STREAM <<'EOF' || true
#!/bin/bash
# grpc-stream.sh ROUTER_IP HOST N MS: a server stream held through
# ROUTER_IP: h2ctest's Server method, N messages MS ms apart, one HTTP/2
# request with curl; the next one 1 s after it ends. curl's trace gives each
# message's arrival time and the trailers; awk writes one line for each.
ip=$1 host=$2 n=$3 ms=$4 d=/var/lib/soak-client
f=$d/grpc/stream@$ip.log
m="$n $ms"
fr=$d/grpc/server-$n-$ms.frame
# gRPC's framing: not compressed, a 4-byte big-endian length (under 256 here), the message.
[ -s "$fr" ] || printf "\\0\\0\\0\\0\\x$(printf %02x ${#m})%s" "$m" >"$fr"
seq=0
while :; do
	seq=$((seq + 1))
	echo "$EPOCHREALTIME start $seq" >>"$f"
	curl -sS --http2 --connect-timeout 3 --speed-limit 1 --speed-time 10 --cacert $d/root.pem --resolve "$host:443:$ip" \
		-H 'content-type: application/grpc' -H 'te: trailers' --data-binary @"$fr" \
		--trace-ascii - --trace-time -o /dev/null \
		-w '\n@@soak@@ %time{%s.%f} %{http_code} %{exitcode} %{time_total} @@ %{errormsg}\n' \
		"https://$host/h2ctest.Echo/Server" 2>/dev/null |
		awk -v seq="$seq" '
		# HH:MM:SS.us of the trace (UTC) to Unix time, on the day of the stream
		function ut(s,   a, t) { split(s, a, ":"); t = base + a[1] * 3600 + a[2] * 60 + a[3]; if (t < last - 43200) { base += 86400; t += 86400 } last = t; return t }
		BEGIN { base = systime() - systime() % 86400; last = 0 }
		/ <= Recv data, / { td = ut($1); data = 1; next }
		/ <= Recv header, / { data = 0; next }
		/^[0-9]+:[0-9]+:[0-9]+\.[0-9]+ / { data = 0; next }
		/^[0-9a-f][0-9a-f][0-9a-f][0-9a-f]: grpc-status: / { gs = $3; next }
		data && /^[0-9a-f][0-9a-f][0-9a-f][0-9a-f]: / {
			s = substr($0, 7)
			while (match(s, /[a-z0-9]+\/[0-9A-Za-z]*@[0-9A-Za-z_-]+:[0-9]+/)) {
				x = substr(s, RSTART, RLENGTH); s = substr(s, RSTART + RLENGTH)
				i = x; sub(/^.*:/, "", i); r = x; sub(/:[0-9]+$/, "", r)
				printf "%.6f msg %d %s %s\n", td, seq, i, r; fflush(); nm++
			}
			next
		}
		/^@@soak@@ / {
			e = $0; sub(/^.* @@ ?/, "", e)
			printf "%s end %d %s %s grpc=%s msgs=%d s=%s %s\n", $2, seq, $3, $4, (gs == "" ? "-" : gs), nm, $5, e; fflush()
		}' >>"$f"
	sleep 1
done
EOF

# streams_to IP: start the streams of STREAMS to the router at IP, and with
# SOAK_GRPC=1 its two gRPC clients.
streams_to() {
	local ip=$1 s hp host path name
	for s in $STREAMS; do
		hp=${s%%=*} name=""
		[ "$hp" = "$s" ] || name=${s#*=}
		host=${hp%%/*} path=/${hp#*/}
		out "sudo env RATE=$STREAM_RATE setsid nohup $CDIR/stream.sh $ip $host '$path' $name >/dev/null 2>&1 </dev/null &"
	done
	if [ "$SOAK_GRPC" = 1 ]; then
		out "sudo setsid nohup $CDIR/grpc-unary.sh $ip $PATH_HOST $GRPC_EVERY_MS >/dev/null 2>&1 </dev/null &"
		out "sudo setsid nohup $CDIR/grpc-stream.sh $ip $PATH_HOST $GRPC_STREAM_N $GRPC_STREAM_MS >/dev/null 2>&1 </dev/null &"
	fi
}

client_start() {
	local ip
	root
	out "sudo tee $CDIR/stream.sh >/dev/null && sudo chmod 755 $CDIR/stream.sh" <<<"$STREAM"
	out "sudo tee $CDIR/certwatch.sh >/dev/null && sudo chmod 755 $CDIR/certwatch.sh" <<<"$CERTWATCH"
	if [ "$SOAK_GRPC" = 1 ]; then
		out "test -x $CDIR/h2ctest" || die "no $CDIR/h2ctest on $OUT_NAME: run outside.sh setup with SOAK_GRPC=1"
		out "sudo install -d -m 0755 $CDIR/grpc && sudo tee $CDIR/grpc-unary.sh >/dev/null && sudo chmod 755 $CDIR/grpc-unary.sh" <<<"$GRPC_UNARY"
		out "sudo tee $CDIR/grpc-stream.sh >/dev/null && sudo chmod 755 $CDIR/grpc-stream.sh" <<<"$GRPC_STREAM"
	fi
	out "echo '${IPS[*]}' | sudo tee $CDIR/routers >/dev/null"
	for ip in "${IPS[@]}"; do streams_to "$ip"; done
	out "sudo setsid nohup $CDIR/certwatch.sh '$HOSTS' >/dev/null 2>&1 </dev/null &"
	ev client "start streams=$((${#IPS[@]} * $(wc -w <<<"$STREAMS"))) hosts=$HOSTS streams_per_router=$(tr ' ' ',' <<<"$STREAMS") routers=${IPS[*]} curl=\"$(out 'curl --version | head -n 1')\" curl_features=\"$(out 'curl --version | sed -n "s/^Features: //p"')\""
	[ "$SOAK_GRPC" = 1 ] && ev client "grpc per_router=unary-every-${GRPC_EVERY_MS}ms,server-stream-${GRPC_STREAM_N}x${GRPC_STREAM_MS}ms url=https://$PATH_HOST$GRPC_PATH routers=${IPS[*]}"
	return 0
}

# client_add IP: streams to the router at IP (a new machine, or an old one
# that clients may still be sent to), and IP in the certificate watcher's list.
client_add() {
	local ip=$1
	streams_to "$ip"
	out "grep -qw '$ip' $CDIR/routers || { echo \"\$(cat $CDIR/routers) $ip\" | sudo tee $CDIR/routers.new >/dev/null && sudo mv $CDIR/routers.new $CDIR/routers; }"
	ev client "add router=$ip streams=$(wc -w <<<"$STREAMS") watched=$(out "cat $CDIR/routers" | tr ' ' ',')"
}

# client_drop IP: stop the streams to the router at IP (a machine that is
# gone), and take it off the certificate watcher's list.
client_drop() {
	local ip=$1
	out "sudo pkill -f '/var/lib/soak-client/[s]tream.sh $ip '; sudo pkill -f '/var/lib/soak-client/[g]rpc-(unary|stream).sh $ip '; sleep 0.5; sudo pkill -f '[-]-resolve [^ ]*:443:$ip '; sudo pkill -f '[h]2ctest call .*-connect $ip:443 '; true"
	out "tr ' ' '\n' <$CDIR/routers | grep -vx '$ip' | grep . | tr '\n' ' ' | sudo tee $CDIR/routers.new >/dev/null && sudo mv $CDIR/routers.new $CDIR/routers"
	ev client "drop router=$ip watched=$(out "cat $CDIR/routers" | tr ' ' ',')"
}

# chall_update: challtestsrv answers each routed host with the nodes of the
# list now, as the hosts' DNS records would be changed for a new machine.
# challtestsrv's add-a appends to a host's addresses, and Pebble validates
# against the first one, so each host is cleared first (in the third soak and
# the first run of the fourth, the dead old server stayed first, and every
# renewal after a server move failed). The default answer, for any other
# name, is the server's address.
chall_update() {
	local h addrs
	addrs=$(printf '"%s",' "${IPS[@]}")
	for h in $HOSTS; do
		out "curl -fsS -d '{\"host\":\"$h\"}' http://127.0.0.1:8055/clear-a && curl -fsS -d '{\"host\":\"$h\",\"addresses\":[${addrs%,}]}' http://127.0.0.1:8055/add-a" >/dev/null || die "challtestsrv did not take $h"
	done
	out "curl -fsS -d '{\"ip\":\"$SERVER_IP\"}' http://127.0.0.1:8055/set-default-ipv4" >/dev/null || die "challtestsrv did not take the default address"
	ev outside "challtestsrv $HOSTS -> ${IPS[*]} (cleared first); default $SERVER_IP"
}

# scratch_start: another cluster's server on the outside machine, for
# set-server to be refused by: init in its own data directory, serve on the
# outside machine's private address, the API on loopback, no router.
SCRATCH=/var/lib/hivemind-scratch
scratch_start() {
	install_bin "$OUT_NAME"
	out "sudo rm -rf $SCRATCH && sudo $HM init --data-dir $SCRATCH >/dev/null &&
		sudo sh -c 'nohup $HM serve --data-dir $SCRATCH --advertise $OUT_IP >/var/log/hivemind-scratch.log 2>&1 </dev/null &'" ||
		die "scratch init/serve on $OUT_NAME"
	wait_for 30 out "timeout 1 bash -c 'exec 3<>/dev/tcp/$OUT_IP/4484'" || die "the scratch server does not listen on $OUT_IP:4484: $(out "tail -n 5 /var/log/hivemind-scratch.log")"
	ev outside "scratch cluster serving on $OUT_IP:4484 ($(out "sudo ss -ltnH | awk '{ print \$4 }' | grep -E ':448[345]\$' | tr '\n' ' '"))"
}

scratch_stop() {
	out "sudo pkill -TERM -f '^$HM serve --data-dir $SCRATCH'; sleep 2; ids=\$(sudo docker ps -aq --filter label=hivemind.app); [ -z \"\$ids\" ] || sudo docker rm -fv \$ids >/dev/null; sudo rm -rf $SCRATCH"
	ev outside "scratch cluster stopped and removed"
}

client_stop() {
	# The brackets keep each pattern from matching the shell that runs pkill.
	out "sudo pkill -f '/var/lib/soak-client/[s]tream.sh|/var/lib/soak-client/[c]ertwatch.sh|/var/lib/soak-client/[g]rpc-(unary|stream).sh'; sleep 0.5; sudo pkill -f '[-]-cacert /var/lib/soak-client/root.pem'; sudo pkill -f '[h]2ctest call '; true"
	ev client "stop"
}

client_status() {
	out "cd $CDIR && for f in *@*.log; do awk -v f=\"\$f\" 'NF >= 8 { n++; if (\$2 != 200 || \$3 != 0) b++ } END { printf \"%s requests=%d failed=%d\n\", f, n, b }' \"\$f\"; done; tail -n 2 certs.log"
	[ "$SOAK_GRPC" = 1 ] || return 0
	out "cd $CDIR/grpc && for f in unary@*.log; do awk -v f=\"\$f\" 'NF >= 4 { n++; if (\$2 != 0) b++ } END { printf \"%s calls=%d failed=%d\n\", f, n, b }' \"\$f\"; done;
		for f in stream@*.log; do awk -v f=\"\$f\" '\$2 == \"start\" { s++ } \$2 == \"msg\" { m++ } \$2 == \"end\" { e++; if (!(\$4 == 200 && \$5 == 0 && \$6 == \"grpc=0\")) b++ } END { printf \"%s streams=%d ended=%d broken=%d msgs=%d\n\", f, s, e, b, m }' \"\$f\"; tail -n 1 \"\$f\"; done"
}

fetch() {
	mkdir -p "$SOAK_DIR/client"
	# The stream logs grow while they are read: tar exits 1 for that, which is fine.
	SSH_TIMEOUT=600 out "cd $CDIR && { tar --warning=no-file-changed -czf - *.log \$(ls grpc/*.log 2>/dev/null) || [ \$? = 1 ]; }" | tar -xzf - -C "$SOAK_DIR/client"
	SSH_TIMEOUT=60 out "sudo docker logs soak-pebble 2>&1" >"$SOAK_DIR/client/pebble.log"
	ev client "fetched $(ls "$SOAK_DIR/client" | wc -l) files"
}

cmd=${1:-}
shift || true
case "$cmd" in
setup) setup ;;
login) login ;;
wait-served) wait_served "$@" ;;
client-start) client_start ;;
client-add) client_add "$@" ;;
client-drop) client_drop "$@" ;;
chall-update) chall_update ;;
scratch-start) scratch_start ;;
scratch-stop) scratch_stop ;;
client-stop) client_stop ;;
client-status) client_status ;;
fetch) fetch ;;
*)
	sed -n '2,20p' "$0"
	exit 2
	;;
esac
