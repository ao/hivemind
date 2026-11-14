#!/bin/sh
# Builds a static linux/amd64 hivemind in a golang container and copies it into
# an empty image (FROM scratch: no libc, no shell, no CA bundle). The image runs
# with its working directory at /, and the script checks that:
#
#   - version and --help run;
#   - init creates a cluster in a data volume;
#   - serve, without a Docker daemon, exits 1 with "cannot reach Docker". serve
#     needs Docker, and it does not start its API without it;
#   - serve, with a docker:dind daemon, answers 200 on GET /healthz.
#
# serve needs Docker, so the check runs a docker:dind daemon with --privileged
# and its API on a unix socket in a named volume, as scripts/integration-docker.sh
# does. The scratch container shares the daemon's network namespace, so serve's
# API on 127.0.0.1:4483 is reachable from the daemon container only. Nothing is
# published to the host. The Docker-less check uses the same image without the
# socket.
#
# Needs Docker. Run from anywhere in the repository. GOPROXY is passed through
# from the environment and defaults to the public proxy.
set -eu

repo=$(cd "$(dirname "$0")/.." && pwd)
GO_IMAGE=${GO_IMAGE:-golang:1.27}
DIND_IMAGE=${DIND_IMAGE:-docker:dind}
GOPROXY_VALUE=${GOPROXY:-https://proxy.golang.org,direct}

mkdir -p "$repo/.scratch"
out=$(mktemp -d "$repo/.scratch/scratch.XXXXXX")
suffix=$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
run="hivemind-scratch-$suffix"
dind="$run-dind"
serve="$run-serve"
libvol="$run-lib"
sockvol="$run-sock"
datavol="$run-data"
sock=/var/run/docker/docker.sock
tag="hivemind-scratch-smoke:$suffix"

cleanup() {
	docker rm -f "$serve" >/dev/null 2>&1 || true
	docker rm -fv "$dind" >/dev/null 2>&1 || true
	docker volume rm "$libvol" "$sockvol" "$datavol" >/dev/null 2>&1 || true
	docker image rm "$tag" >/dev/null 2>&1 || true
	rm -rf "$out"
}
trap cleanup EXIT

fail() {
	echo "scratch smoke: $*" >&2
	exit 1
}

docker run --rm -v "$repo":/src -v "$out":/out -w /src \
	-e GOTOOLCHAIN=local -e GOFLAGS=-buildvcs=false \
	-e CGO_ENABLED=0 -e GOOS=linux -e GOARCH=amd64 \
	-e GOPROXY="$GOPROXY_VALUE" \
	"$GO_IMAGE" go build -trimpath -ldflags "-s -w" -o /out/hivemind ./cmd/hivemind
cp "$repo/test/scratch/Dockerfile" "$out/Dockerfile"
docker build -q -t "$tag" "$out" >/dev/null

docker run --rm "$tag" version
docker run --rm "$tag" --help >/dev/null
echo "scratch smoke: version and --help ok"

# init writes the data directory. The client config goes to the data volume,
# so the image needs no HOME.
docker volume create "$datavol" >/dev/null
init_out=$(docker run --rm -e HIVEMIND_DATA_DIR=/data -e HIVEMIND_CLIENT_CONFIG=/data/client.yaml \
	-v "$datavol":/data "$tag" init) || fail "init failed: $init_out"
case "$init_out" in
*"Created a cluster"*) ;;
*) fail "init printed no cluster line: $init_out" ;;
esac
echo "scratch smoke: init ok"

# Without a daemon, serve must fail before it listens.
rc=0
nodaemon=$(docker run --rm -e HIVEMIND_DATA_DIR=/data -v "$datavol":/data "$tag" serve 2>&1) || rc=$?
[ "$rc" = 1 ] || fail "serve without Docker exited $rc, want 1: $nodaemon"
case "$nodaemon" in
*"cannot reach Docker"*) ;;
*) fail "serve without Docker gave no 'cannot reach Docker' error: $nodaemon" ;;
esac
echo "scratch smoke: serve without Docker exits 1: cannot reach Docker"

# The daemon, with its API on a unix socket only.
docker volume create "$libvol" >/dev/null
docker volume create "$sockvol" >/dev/null
docker run -d --name "$dind" --privileged \
	-v "$libvol":/var/lib/docker \
	-v "$sockvol":/var/run/docker \
	"$DIND_IMAGE" dockerd -H "unix://$sock" >/dev/null
ready=""
for _ in $(seq 1 90); do
	if docker exec -e "DOCKER_HOST=unix://$sock" "$dind" docker info >/dev/null 2>&1; then
		ready=1
		break
	fi
	sleep 1
done
[ -n "$ready" ] || { docker logs "$dind" >&2 || true; fail "the daemon in $dind did not start"; }

docker run -d --name "$serve" --network "container:$dind" \
	-e HIVEMIND_DATA_DIR=/data \
	-e HIVEMIND_DOCKER_HOST="unix://$sock" \
	-v "$datavol":/data \
	-v "$sockvol":/var/run/docker \
	"$tag" serve >/dev/null

# The check runs in the daemon's namespace, where wget is busybox's.
code=""
for _ in $(seq 1 60); do
	code=$(docker exec "$dind" wget -S -O /dev/null http://127.0.0.1:4483/healthz 2>&1 |
		awk '/HTTP\//{c = $2} END{print c}') || true
	[ "$code" = 200 ] && break
	if [ "$(docker inspect -f '{{.State.Running}}' "$serve")" != true ]; then
		docker logs "$serve" >&2 || true
		fail "serve exited before /healthz answered"
	fi
	sleep 1
done
[ "$code" = 200 ] || fail "GET /healthz returned '${code:-no response}', want 200"
echo "scratch smoke: serve answers 200 on GET /healthz"

# No wildcard TCP listener in the daemon's namespace.
listening=$(docker exec "$dind" sh -c 'cat /proc/net/tcp /proc/net/tcp6' | awk '$4 == "0A" { print $2 }')
if printf '%s\n' "$listening" | grep -Eq '^(00000000|0{32}):'; then
	fail "a wildcard TCP listener exists while serve runs"
fi

docker stop -t 20 "$serve" >/dev/null
rc=$(docker inspect -f '{{.State.ExitCode}}' "$serve")
[ "$rc" = 0 ] || fail "serve exited $rc on SIGTERM, want 0"
echo "scratch smoke: serve stops with exit 0 on SIGTERM"
echo "scratch smoke: ok"
