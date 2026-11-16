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
#
# Pebble runs with its validation sleeps off, no nonce rejection, no
# authorization reuse (every order is validated again, over HTTP-01 on port
# 80 of a router), and a default certificate lifetime of $CERT_LIFETIME
# seconds, so the routers renew during the run. challtestsrv answers every
# routed host with the private address of every node, so Pebble's
# validation can reach any router. The clients trust Pebble's root, which
# Pebble makes at its start: Pebble must not restart during the run.
#
# Clients: one stream per router and host. Each is one curl process making a
# request every 100 ms (--rate 10/s) to https://HOST/ through that router,
# over HTTP/1.1 with a new TCP connection and a full TLS handshake each time
# (--http1.1, Connection: close,
# no session reuse: over HTTP/2 curl would send every request on one
# connection), 3 s to connect, 10 s in all. Each request is one line of
# /var/lib/soak-client/HOST@ROUTER.log:
#   <end, Unix s.us> <http code> <curl exit code> <connect> <TLS done>
#   <first byte> <total> (seconds from the start) <connections> <error...>
# The certificate watcher logs, every 30 s, the serial and expiry of the
# certificate each router serves for each host, from a fresh handshake.
set -euo pipefail
. "$(dirname "$0")/lib.sh"

[ -n "$OUT_NAME" ] || die "no outside machine in $OUTSIDE_FILE"
HOSTS=${ROUTED_HOSTS:-www.soak.test api.soak.test}
CERT_LIFETIME=${CERT_LIFETIME:-3600}
PEBBLE_IMAGE=${PEBBLE_IMAGE:-ghcr.io/letsencrypt/pebble:latest}
CHALL_IMAGE=${CHALL_IMAGE:-ghcr.io/letsencrypt/pebble-challtestsrv:latest}
REG_IMAGE=${REG_IMAGE:-registry:2}
HTPASSWD_IMAGE=${HTPASSWD_IMAGE:-httpd:2-alpine}
# The private images: NAME=SOURCE, pushed as $REGISTRY/soak/NAME.
PRIVATE_IMAGES=${PRIVATE_IMAGES:-whoami:1=traefik/whoami:v1.10.3 whoami:2=traefik/whoami:v1.11.0}
REG_USER=${REG_USER:-soak}
PW_FILE=$SOAK_DIR/registry.pw
CDIR=/var/lib/soak-client

out() { on "$OUT_NAME" "$@"; }

setup() {
	mkdir -p "$OUT_FILES"
	[ -s "$PW_FILE" ] || (umask 077 && openssl rand -hex 16 >"$PW_FILE")
	ev outside "setup begin $OUT_NAME $OUT_IP cert_lifetime=${CERT_LIFETIME}s hosts=$HOSTS"
	SSH_TIMEOUT=600 out "set -e; for i in $PEBBLE_IMAGE $CHALL_IMAGE $REG_IMAGE $HTPASSWD_IMAGE; do sudo docker pull -q \$i; done" >/dev/null
	# The registry's own CA and certificate, made here for the run.
	SSH_TIMEOUT=60 out "sudo bash -s" <<EOF
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
	out "sudo docker run --rm -i --entrypoint htpasswd $HTPASSWD_IMAGE -Bin $REG_USER | sudo tee /opt/soak/auth/htpasswd >/dev/null" <"$PW_FILE"
	# Pebble's config: HTTP-01 on port 80, and a short default lifetime.
	out "sudo tee /opt/soak/pebble-config.json >/dev/null" <<EOF
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
		sudo docker run -d --name soak-registry --restart unless-stopped -p $OUT_IP:443:5000 \
			-v /opt/soak/certs:/certs:ro -v /opt/soak/auth:/auth:ro -v soak-registry:/var/lib/registry \
			-e REGISTRY_AUTH=htpasswd -e REGISTRY_AUTH_HTPASSWD_REALM=soak -e REGISTRY_AUTH_HTPASSWD_PATH=/auth/htpasswd \
			-e REGISTRY_HTTP_TLS_CERTIFICATE=/certs/reg.crt -e REGISTRY_HTTP_TLS_KEY=/certs/reg.key $REG_IMAGE >/dev/null"
	# The outside machine pushes to its own registry by name, as a node pulls.
	out "sudo sed -i '/ # soak-outside\$/d' /etc/hosts && echo '$OUT_IP pebble $REGISTRY # soak-outside' | sudo tee -a /etc/hosts >/dev/null &&
		sudo install -d /etc/docker/certs.d/$REGISTRY && sudo cp /opt/soak/certs/ca.crt /etc/docker/certs.d/$REGISTRY/ca.crt"
	SSH_TIMEOUT=60 out "for i in \$(seq 1 30); do curl -s -o /dev/null --cacert /etc/docker/certs.d/$REGISTRY/ca.crt https://$REGISTRY/v2/ && exit 0; sleep 1; done; exit 1" ||
		die "the registry does not answer"
	SSH_TIMEOUT=60 out "sudo docker cp soak-pebble:/test/certs/pebble.minica.pem /opt/soak/pebble-minica.pem && sudo cat /opt/soak/pebble-minica.pem" >"$OUT_FILES/pebble-minica.pem"
	out "sudo cat /opt/soak/certs/ca.crt" >"$OUT_FILES/registry-ca.crt"
	push_images
	local h addrs
	addrs=$(printf '"%s",' "${IPS[@]}")
	for h in $HOSTS; do
		out "curl -fsS -d '{\"host\":\"$h\",\"addresses\":[${addrs%,}]}' http://127.0.0.1:8055/add-a" >/dev/null || die "challtestsrv did not take $h"
	done
	out "for i in \$(seq 1 30); do curl -fsS -o /dev/null --cacert /opt/soak/pebble-minica.pem https://127.0.0.1:15000/roots/0 && exit 0; sleep 1; done; exit 1" ||
		die "Pebble's management API does not answer"
	ev outside "pebble image=$(out "sudo docker inspect -f '{{.Image}} {{index .Config.Labels \"org.opencontainers.image.revision\"}}' soak-pebble") chall=$(out "sudo docker inspect -f '{{.Image}}' soak-chall") registry=$(out "sudo docker inspect -f '{{.Image}}' soak-registry")"
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
# stream.sh ROUTER_IP HOST: a request every 100 ms to https://HOST/ through ROUTER_IP.
ip=$1 host=$2 d=/var/lib/soak-client
while :; do
	curl -s --http1.1 --rate 10/s --no-sessionid -H 'Connection: close' --connect-timeout 3 -m 10 \
		--cacert $d/root.pem --resolve "$host:443:$ip" -o /dev/null \
		-w '%time{%s.%f} %{http_code} %{exitcode} %{time_connect} %{time_appconnect} %{time_starttransfer} %{time_total} %{num_connects} %{errormsg}\n' \
		"https://$host/?n=[1-36000]" >>"$d/$host@$ip.log" 2>/dev/null
done
EOF

read -r -d '' CERTWATCH <<'EOF' || true
#!/bin/bash
# certwatch.sh HOSTS IPS: every 30 s, the certificate each router serves for each host.
d=/var/lib/soak-client
while :; do
	for ip in $2; do
		for h in $1; do
			r=$(timeout 5 openssl s_client -connect "$ip:443" -servername "$h" -CAfile $d/root.pem -verify_return_error </dev/null 2>/dev/null |
				openssl x509 -noout -serial -enddate 2>/dev/null | tr '\n' ' ')
			echo "$(date -u +%s.%3N) $h@$ip ${r:-fail}"
		done
	done >>$d/certs.log
	sleep 30
done
EOF

client_start() {
	local ip h
	root
	out "sudo tee $CDIR/stream.sh >/dev/null && sudo chmod 755 $CDIR/stream.sh" <<<"$STREAM"
	out "sudo tee $CDIR/certwatch.sh >/dev/null && sudo chmod 755 $CDIR/certwatch.sh" <<<"$CERTWATCH"
	for ip in "${IPS[@]}"; do
		for h in $HOSTS; do
			out "sudo setsid nohup $CDIR/stream.sh $ip $h >/dev/null 2>&1 </dev/null &"
		done
	done
	out "sudo setsid nohup $CDIR/certwatch.sh '$HOSTS' '${IPS[*]}' >/dev/null 2>&1 </dev/null &"
	ev client "start streams=$((${#IPS[@]} * $(wc -w <<<"$HOSTS"))) hosts=$HOSTS routers=${IPS[*]} curl=\"$(out 'curl --version | head -n 1')\""
}

client_stop() {
	# The brackets keep each pattern from matching the shell that runs pkill.
	out "sudo pkill -f '/var/lib/soak-client/[s]tream.sh|/var/lib/soak-client/[c]ertwatch.sh'; sleep 0.5; sudo pkill -f '[-]-cacert /var/lib/soak-client/root.pem'; true"
	ev client "stop"
}

client_status() {
	out "cd $CDIR && for f in *@*.log; do awk -v f=\"\$f\" 'NF >= 8 { n++; if (\$2 != 200 || \$3 != 0) b++ } END { printf \"%s requests=%d failed=%d\n\", f, n, b }' \"\$f\"; done; tail -n 2 certs.log"
}

fetch() {
	mkdir -p "$SOAK_DIR/client"
	# The stream logs grow while they are read: tar exits 1 for that, which is fine.
	SSH_TIMEOUT=600 out "cd $CDIR && { tar --warning=no-file-changed -czf - *.log || [ \$? = 1 ]; }" | tar -xzf - -C "$SOAK_DIR/client"
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
client-stop) client_stop ;;
client-status) client_status ;;
fetch) fetch ;;
*)
	sed -n '2,15p' "$0"
	exit 2
	;;
esac
