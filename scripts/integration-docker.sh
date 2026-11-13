#!/usr/bin/env bash
# Runs the integration tests against a real Docker daemon: the agent's
# single-node reconcile tests first, then the Docker runtime tests. The runtime
# tests end by killing the daemon, so they go last.
#
# It starts docker:dind with --privileged, with its API on a unix socket in a
# named volume. It publishes no ports, and it listens on no TCP socket. The
# tests run in a golang container that shares the daemon's network namespace,
# so a container port published on 127.0.0.1 inside the daemon can be reached.
# On exit it removes the containers, network and volumes it created. The Go
# build cache volume (hivemind-cache) is kept.
#
# Needs Docker on the host. Run from anywhere in the repository.
set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
DIND_IMAGE=${DIND_IMAGE:-docker:dind}
GO_IMAGE=${GO_IMAGE:-golang:1.27}
CACHE_VOLUME=${CACHE_VOLUME:-hivemind-cache}

suffix=$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
run="hivemind-it-$suffix"
dind="$run-dind"
gocont="$run-go"
net="$run-net"
sockvol="$run-sock"
datavol="$run-data"
sock=/var/run/docker/docker.sock
marker=/tmp/hivemind-it-daemon-stop
stopper=""

cleanup() {
	if [ -n "$stopper" ]; then
		kill "$stopper" 2>/dev/null || true
	fi
	docker rm -f "$gocont" >/dev/null 2>&1 || true
	docker rm -fv "$dind" >/dev/null 2>&1 || true
	docker network rm "$net" >/dev/null 2>&1 || true
	docker volume rm "$sockvol" "$datavol" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "integration-docker: daemon $dind, go container $gocont"
docker network create "$net" >/dev/null
docker volume create "$sockvol" >/dev/null
docker volume create "$datavol" >/dev/null

# The first argument is "dockerd", so the image's entrypoint adds no TCP
# listeners. Only the unix socket is served.
docker run -d --name "$dind" --privileged --network "$net" \
	-v "$datavol:/var/lib/docker" \
	-v "$sockvol:/var/run/docker" \
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

docker run -d --name "$gocont" --network "container:$dind" \
	-v "$repo:/src" \
	-v "$CACHE_VOLUME:/cache" \
	-v "$sockvol:/var/run/docker" \
	-w /src \
	-e GOMODCACHE=/cache/mod \
	-e GOCACHE=/cache/build \
	-e GOFLAGS=-buildvcs=false \
	-e GOPROXY=direct \
	-e GOSUMDB=off \
	-e GOTOOLCHAIN=local \
	-e "DOCKER_HOST=unix://$sock" \
	"$GO_IMAGE" \
	bash -c 'rc=0
		go test -race -count=1 -tags integration -v ./internal/agent/... || rc=1
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
if [ "$rc" = 0 ]; then
	echo "integration-docker: tests passed"
else
	echo "integration-docker: tests failed with exit code $rc" >&2
fi
exit "$rc"
