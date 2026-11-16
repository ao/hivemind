#!/usr/bin/env bash
# Runs the integration tests against a real Docker daemon: the agent's
# reconcile and health tests first, then the health probes, then the CLI
# test, which builds the hivemind binary and drives every command against a
# one-node cluster, then the rolling update tests, then the Docker runtime
# tests. The runtime tests end by killing the daemon, so they go last.
#
# It starts docker:dind with --privileged, with its API on a unix socket in a
# named volume. It publishes no ports, and it listens on no TCP socket. The
# tests run in a golang container that shares the daemon's network namespace,
# so a container port published on 127.0.0.1 inside the daemon can be reached.
# On exit it removes the containers, network and volumes it created. The Go
# build cache volume (hivemind-cache) is kept.
#
# For the private registry test it runs registry:2 on the same network, with
# htpasswd auth and TLS from a CA made for the run, which the daemon trusts
# through /etc/docker/certs.d. The registry publishes no port either; the
# daemon reaches it by its network alias. It pushes a small image there with
# a random password, and checks at the end that the daemon's log does not
# have the password.
#
# Needs Docker on the host. Run from anywhere in the repository.
set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
DIND_IMAGE=${DIND_IMAGE:-docker:dind}
GO_IMAGE=${GO_IMAGE:-golang:1.27}
REGISTRY_IMAGE=${REGISTRY_IMAGE:-registry:2}
HTPASSWD_IMAGE=${HTPASSWD_IMAGE:-httpd:2-alpine}
CACHE_VOLUME=${CACHE_VOLUME:-hivemind-cache}

suffix=$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
run="hivemind-it-$suffix"
dind="$run-dind"
gocont="$run-go"
regcont="$run-registry"
net="$run-net"
sockvol="$run-sock"
datavol="$run-data"
regvol="$run-registry"
sock=/var/run/docker/docker.sock
marker=/tmp/hivemind-it-daemon-stop
stopper=""

# The private registry. The username and the password are new for each run.
reghost=registry.hivemind.test
registry="$reghost:5000"
reguser="puller-$suffix"
regpw=$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')
regimage="$registry/it/private:1"

cleanup() {
	if [ -n "$stopper" ]; then
		kill "$stopper" 2>/dev/null || true
	fi
	docker rm -f "$gocont" >/dev/null 2>&1 || true
	docker rm -f "$regcont" >/dev/null 2>&1 || true
	docker rm -fv "$dind" >/dev/null 2>&1 || true
	docker network rm "$net" >/dev/null 2>&1 || true
	docker volume rm "$sockvol" "$datavol" "$regvol" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "integration-docker: daemon $dind, go container $gocont, registry $regcont"
docker network create "$net" >/dev/null
docker volume create "$sockvol" >/dev/null
docker volume create "$datavol" >/dev/null
docker volume create "$regvol" >/dev/null

# The first argument is "dockerd", so the image's entrypoint adds no TCP
# listeners. Only the unix socket is served.
docker run -d --name "$dind" --privileged --network "$net" \
	-v "$datavol:/var/lib/docker" \
	-v "$sockvol:/var/run/docker" \
	-v "$regvol:/hm-registry" \
	"$DIND_IMAGE" dockerd -H "unix://$sock" >/dev/null

ready=""
for _ in $(seq 1 90); do
	if docker exec -e "DOCKER_HOST=unix://$sock" "$dind" docker info >/dev/null 2>&1; then
		ready=1
		break
	fi
	sleep 1
done
if [ -z "$ready" ]; then
	echo "integration-docker: the daemon in $dind did not start" >&2
	docker logs "$dind" >&2 || true
	exit 1
fi

# Listening sockets in the daemon's namespace. A wildcard listener (0.0.0.0 or
# [::]) would be reachable from the private network, so it fails the run.
listening=$(docker exec "$dind" sh -c 'cat /proc/net/tcp /proc/net/tcp6' | awk '$4 == "0A" { print $2 }')
echo "integration-docker: listening TCP sockets before the tests (hex address:port): ${listening:-none}"
if printf '%s\n' "$listening" | grep -Eq '^(00000000|0{32}):'; then
	echo "integration-docker: a wildcard TCP listener exists in the daemon namespace" >&2
	exit 1
fi

indind() { docker exec -i -e "DOCKER_HOST=unix://$sock" "$dind" "$@"; }

# A CA and the registry's certificate, made with the daemon container's
# openssl. The daemon trusts the CA for this registry only. The CA key is
# deleted once the certificate is signed.
indind sh -ec "
	cd /hm-registry
	openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 \
		-subj '/CN=hivemind integration CA' -keyout ca.key -out ca.crt 2>/dev/null
	openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
		-subj '/CN=$reghost' -keyout server.key -out server.csr 2>/dev/null
	printf 'subjectAltName=DNS:$reghost\n' >san.cnf
	openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 1 \
		-extfile san.cnf -out server.crt 2>/dev/null
	rm -f ca.key ca.srl server.csr san.cnf
	mkdir -p '/etc/docker/certs.d/$registry'
	cp ca.crt '/etc/docker/certs.d/$registry/ca.crt'
"
# htpasswd reads the password from standard input, so it is on no command line.
printf '%s' "$regpw" | docker run --rm -i --entrypoint htpasswd "$HTPASSWD_IMAGE" -Bin "$reguser" |
	indind sh -c 'cat >/hm-registry/htpasswd'
docker run -d --name "$regcont" --network "$net" --network-alias "$reghost" \
	-v "$regvol:/hm-registry:ro" \
	-e REGISTRY_HTTP_ADDR=0.0.0.0:5000 \
	-e REGISTRY_HTTP_TLS_CERTIFICATE=/hm-registry/server.crt \
	-e REGISTRY_HTTP_TLS_KEY=/hm-registry/server.key \
	-e REGISTRY_AUTH=htpasswd \
	-e REGISTRY_AUTH_HTPASSWD_REALM=hivemind-it \
	-e REGISTRY_AUTH_HTPASSWD_PATH=/hm-registry/htpasswd \
	"$REGISTRY_IMAGE" >/dev/null

# Push a small image, then forget both the image and the login, so that
# only hivemind's credential can pull it.
indind docker pull -q busybox:1.36 >/dev/null
indind docker tag busybox:1.36 "$regimage"
pushed=""
for _ in $(seq 1 30); do
	if printf '%s' "$regpw" | indind docker login "$registry" -u "$reguser" --password-stdin >/dev/null 2>&1; then
		pushed=1
		break
	fi
	sleep 1
done
if [ -z "$pushed" ]; then
	echo "integration-docker: could not log in to the registry at $registry" >&2
	docker logs "$regcont" >&2 || true
	exit 1
fi
indind docker push -q "$regimage" >/dev/null
indind docker logout "$registry" >/dev/null
indind docker rmi "$regimage" >/dev/null
if indind docker pull "$regimage" >/dev/null 2>&1; then
	echo "integration-docker: $regimage pulls without a login" >&2
	exit 1
fi
echo "integration-docker: pushed $regimage to a registry with htpasswd and its own CA"

docker run -d --name "$gocont" --network "container:$dind" \
	-v "$repo:/src" \
	-v "$CACHE_VOLUME:/cache" \
	-v "$sockvol:/var/run/docker" \
	-w /src \
	-e GOMODCACHE=/cache/mod \
	-e GOCACHE=/cache/build \
	-e GOFLAGS=-buildvcs=false \
	-e GOPROXY="${GOPROXY:-https://proxy.golang.org,direct}" \
	-e GOSUMDB="${GOSUMDB:-sum.golang.org}" \
	-e GOTOOLCHAIN=local \
	-e "DOCKER_HOST=unix://$sock" \
	-e "HIVEMIND_IT_REGISTRY=$registry" \
	-e "HIVEMIND_IT_REGISTRY_IMAGE=$regimage" \
	-e "HIVEMIND_IT_REGISTRY_USER=$reguser" \
	-e "HIVEMIND_IT_REGISTRY_PASSWORD=$regpw" \
	"$GO_IMAGE" \
	bash -c 'rc=0
		go test -race -count=1 -tags integration -v ./internal/agent/... || rc=1
		go test -race -count=1 -tags integration -v ./internal/health/... || rc=1
		go test -race -count=1 -tags integration -v ./cmd/hivemind/... || rc=1
		go test -race -count=1 -tags integration -v ./internal/rollout/... || rc=1
		go test -race -count=1 -tags integration -v ./internal/runtime/... || rc=1
		exit $rc' >/dev/null

# The daemon test blocks in a call and then writes $marker. Kill the daemon's
# container then, so the call fails the way a daemon crash would. The loop ends
# when the test container exits.
(
	while [ "$(docker inspect -f '{{.State.Running}}' "$gocont" 2>/dev/null || true)" = true ]; do
		if docker exec "$gocont" test -f "$marker" 2>/dev/null; then
			sleep 2
			docker kill --signal KILL "$dind" >/dev/null
			echo "integration-docker: killed $dind during the daemon-stop test" >&2
			exit 0
		fi
		sleep 1
	done
) &
stopper=$!

docker logs -f "$gocont" || true
rc=$(docker wait "$gocont")
# The private registry test must have run, not been skipped or filtered out.
if [ "$rc" = 0 ] && ! docker logs "$gocont" 2>&1 | grep -q -- '--- PASS: TestCLIPrivateRegistry'; then
	echo "integration-docker: TestCLIPrivateRegistry did not pass" >&2
	rc=1
fi
# The daemon got the password in X-Registry-Auth; its log must not have it.
if docker logs "$dind" 2>&1 | grep -qF -- "$regpw"; then
	echo "integration-docker: the Docker daemon's log has the registry password" >&2
	rc=1
fi
if [ "$rc" = 0 ]; then
	echo "integration-docker: tests passed"
else
	echo "integration-docker: tests failed with exit code $rc" >&2
fi
exit "$rc"
