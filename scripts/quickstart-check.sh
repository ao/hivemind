#!/usr/bin/env bash
# Runs the quick start from README.md against a real one-node cluster.
#
# The commands in the first sh block under "## Quick start" are read from the
# README in order and run as written. The script itself handles four things:
#
#   hivemind init    checks that the token it prints is the one in admin.token;
#   hivemind serve   runs in the background, after which the script checks
#                    that the API answers, that the token is not in the log,
#                    and that no listener is wildcard;
#   deploy, scale    wait until the running replicas match the desired count;
#   delete           waits until the app is gone and no container carries its
#                    label.
#
# The check starts docker:dind with --privileged and its API on a unix socket
# in a named volume, as scripts/integration-docker.sh does. It publishes no
# ports. The commands run in a golang container that shares the daemon's
# network namespace, so the loopback ports of the quick start are reachable.
# That container builds hivemind from this checkout, puts it on PATH, and runs
# the commands.
#
# On exit it removes the containers, network and volumes it created. The Go
# build cache volume (hivemind-cache) is kept.
#
# Needs Docker on the host. Run from anywhere in the repository.
set -euo pipefail

inside() {
	cd /src
	CGO_ENABLED=0 go build -trimpath -o /usr/local/bin/hivemind ./cmd/hivemind
	# The quick start uses the default Docker socket path.
	ln -sf /var/run/docker/docker.sock /var/run/docker.sock

	work=$(mktemp -d)
	serve_pid=""
	token=""
	api=http://127.0.0.1:4483

	fail() {
		echo "quickstart-check: $*" >&2
		if [ -s "$work/serve.log" ]; then
			echo "quickstart-check: last lines of serve.log:" >&2
			tail -n 30 "$work/serve.log" >&2
		fi
		exit 1
	}

	stop_serve() {
		if [ -n "$serve_pid" ] && kill -0 "$serve_pid" 2>/dev/null; then
			kill -TERM "$serve_pid" 2>/dev/null || true
			wait "$serve_pid" 2>/dev/null || true
		fi
	}
	trap 'stop_serve; rm -rf "$work"' EXIT

	awk '/^## Quick start/ {s = 1; next} /^## / {s = 0} s' /src/README.md |
		awk '/^```sh$/ {f = 1; next} /^```$/ {f = 0; next} f' > "$work/commands"
	grep -qx 'hivemind init' "$work/commands" || fail "README.md has no 'hivemind init' line in the quick start"
	grep -qx 'hivemind serve' "$work/commands" || fail "README.md has no 'hivemind serve' line in the quick start"
	echo "quickstart-check: $(grep -vc '^\(#\|$\)' "$work/commands") commands from README.md"

	wait_for_api() {
		for _ in $(seq 1 60); do
			if curl -fsS "$api/healthz" >/dev/null 2>&1; then
				return 0
			fi
			kill -0 "$serve_pid" 2>/dev/null || fail "hivemind serve exited before the API answered"
			sleep 1
		done
		fail "the API did not answer /healthz within 60 seconds"
	}

	# Listening sockets in the daemon's namespace. A wildcard listener would be
	# reachable from the private network, so it fails the check.
	check_listeners() {
		listening=$(cat /proc/net/tcp /proc/net/tcp6 | awk '$4 == "0A" { print $2 }')
		echo "quickstart-check: listening TCP sockets (hex address:port): ${listening:-none}"
		if printf '%s\n' "$listening" | grep -Eq '^(00000000|0{32}):'; then
			fail "a wildcard TCP listener exists after hivemind serve started"
		fi
	}

	# status_field prints a top-level number from `hivemind status NAME --json`.
	# The top-level fields come before the replica list, so the first match is
	# the app's own.
	status_field() {
		hivemind status "$1" --json 2>/dev/null | sed -n "s/^  \"$2\": \([0-9]*\),\{0,1\}$/\1/p" | head -n 1
	}

	wait_replicas() {
		local app=$1 desired running
		for _ in $(seq 1 120); do
			desired=$(status_field "$app" desired)
			running=$(status_field "$app" running)
			if [ -n "$desired" ] && [ "$desired" = "$running" ]; then
				echo "quickstart-check: $app has $running of $desired replicas running"
				return 0
			fi
			sleep 1
		done
		hivemind status "$app" >&2 || true
		fail "$app did not reach its desired replica count within 120 seconds"
	}

	# containers_with_label counts every container, running or not, that
	# carries the hivemind.app label of the app, as the daemon reports them.
	containers_with_label() {
		curl -fsS -G --unix-socket /var/run/docker/docker.sock \
			--data-urlencode 'all=1' \
			--data-urlencode "filters={\"label\":[\"hivemind.app=$1\"]}" \
			http://localhost/v1.44/containers/json | grep -c '"Id"' || true
	}

	wait_gone() {
		local app=$1 left=1
		# The store hides a deleted app at once, but its containers stay until
		# the next pass, so wait on the daemon, not on the status.
		for _ in $(seq 1 120); do
			left=$(containers_with_label "$app")
			if [ "$left" = 0 ]; then
				break
			fi
			sleep 1
		done
		[ "$left" = 0 ] || fail "$left container(s) with label hivemind.app=$app remain 120 seconds after delete"
		# Once no node reports a container, the store drops the app's rows.
		for _ in $(seq 1 30); do
			hivemind status "$app" >/dev/null 2>&1 || break
			sleep 1
		done
		hivemind status "$app" >/dev/null 2>&1 && fail "$app is still listed after its containers are gone"
		echo "quickstart-check: $app is gone and no container carries its label"
	}

	while IFS= read -r cmd; do
		case "$cmd" in
		"" | "#"*) continue ;;
		esac
		echo "quickstart-check: \$ $cmd"
		case "$cmd" in
		"hivemind init")
			hivemind init | tee "$work/init.out"
			token=$(grep -o 'hm_[A-Za-z0-9_-]*' "$work/init.out" | head -n 1 || true)
			[ "${#token}" = 46 ] || fail "init printed no admin token"
			[ "$(cat /var/lib/hivemind/admin.token)" = "$token" ] || fail "the token init printed is not the one in admin.token"
			;;
		"hivemind serve")
			hivemind serve </dev/null >"$work/serve.log" 2>&1 &
			serve_pid=$!
			wait_for_api
			check_listeners
			code=$(curl -s -o /dev/null -w '%{http_code}' "$api/v1/status")
			[ "$code" = 401 ] || fail "GET /v1/status without a token returned $code, want 401"
			code=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $token" "$api/v1/status")
			[ "$code" = 200 ] || fail "GET /v1/status with the init token returned $code, want 200"
			echo "quickstart-check: the API answers 401 without a token, and 200 with the token from init"
			;;
		"hivemind deploy "*)
			eval "$cmd"
			app=$(printf '%s\n' "$cmd" | sed -n 's/.*--name \([a-z0-9-]*\).*/\1/p')
			wait_replicas "$app"
			;;
		"hivemind scale "*)
			read -r _ _ app count <<<"$cmd"
			eval "$cmd"
			wait_replicas "$app"
			[ "$(status_field "$app" desired)" = "$count" ] || fail "$app does not have $count desired replicas"
			;;
		"hivemind delete "*)
			read -r _ _ app <<<"$cmd"
			eval "$cmd"
			wait_gone "$app"
			;;
		"hivemind logs "*)
			eval "$cmd" >"$work/logs.out"
			grep -q 'GET / ' "$work/logs.out" || fail "the log does not show the GET / request made by the quick start"
			echo "quickstart-check: the log shows the request"
			;;
		*)
			eval "$cmd"
			;;
		esac
	done <"$work/commands"

	[ -n "$serve_pid" ] || fail "the quick start never started hivemind serve"
	rc=0
	kill -TERM "$serve_pid" 2>/dev/null || true
	wait "$serve_pid" || rc=$?
	serve_pid=""
	[ "$rc" = 0 ] || fail "hivemind serve exited with $rc on SIGTERM, want 0"
	grep -q 'stopped' "$work/serve.log" || fail "serve.log has no 'stopped' line after SIGTERM"
	if grep -q "$token" "$work/serve.log"; then
		fail "the admin token appears in serve.log"
	fi
	echo "quickstart-check: serve stopped with exit 0 and the token is not in its log"
	echo "quickstart-check: ok"
}

if [ "${1:-}" = "--inside" ]; then
	inside
	exit 0
fi

repo=$(cd "$(dirname "$0")/.." && pwd)
DIND_IMAGE=${DIND_IMAGE:-docker:dind}
GO_IMAGE=${GO_IMAGE:-golang:1.27}
CACHE_VOLUME=${CACHE_VOLUME:-hivemind-cache}

suffix=$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
run="hivemind-qs-$suffix"
dind="$run-dind"
gocont="$run-go"
net="$run-net"
sockvol="$run-sock"
datavol="$run-data"
sock=/var/run/docker/docker.sock

cleanup() {
	docker rm -f "$gocont" >/dev/null 2>&1 || true
	docker rm -fv "$dind" >/dev/null 2>&1 || true
	docker network rm "$net" >/dev/null 2>&1 || true
	docker volume rm "$sockvol" "$datavol" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "quickstart-check: daemon $dind, go container $gocont"
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
	echo "quickstart-check: the daemon in $dind did not start" >&2
	docker logs "$dind" >&2 || true
	exit 1
fi

rc=0
docker run --rm --name "$gocont" --network "container:$dind" \
	-v "$repo:/src:ro" \
	-v "$CACHE_VOLUME:/cache" \
	-v "$sockvol:/var/run/docker" \
	-e GOMODCACHE=/cache/mod \
	-e GOCACHE=/cache/build \
	-e GOFLAGS=-buildvcs=false \
	-e GOPROXY=direct \
	-e GOSUMDB=off \
	-e GOTOOLCHAIN=local \
	"$GO_IMAGE" \
	bash /src/scripts/quickstart-check.sh --inside || rc=$?

if [ "$rc" = 0 ]; then
	echo "quickstart-check: passed"
else
	echo "quickstart-check: failed with exit code $rc" >&2
fi
exit "$rc"
