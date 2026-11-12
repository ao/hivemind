#!/bin/sh
# Builds a static linux/amd64 hivemind in a golang container, copies it into an
# empty image, and runs it from / to prove it has no runtime dependencies.
# Needs Docker. Run from the repository root.
set -eu
out=$(mktemp -d "$PWD/.scratch/scratch.XXXXXX")
trap 'rm -rf "$out"' EXIT
docker run --rm -v "$PWD":/src -v "$out":/out -w /src \
  -e GOTOOLCHAIN=local -e GOFLAGS=-buildvcs=false \
  -e CGO_ENABLED=0 -e GOOS=linux -e GOARCH=amd64 \
  -e GOPROXY="${GOPROXY:-https://proxy.golang.org,direct}" \
  golang:1.27 go build -trimpath -ldflags "-s -w" -o /out/hivemind ./cmd/hivemind
cp test/scratch/Dockerfile "$out/Dockerfile"
tag="hivemind-scratch-smoke:$(basename "$out" | tr -d .)"
docker build -q -t "$tag" "$out" >/dev/null
docker run --rm "$tag" version
docker run --rm "$tag" --help >/dev/null
docker image rm "$tag" >/dev/null
echo "scratch smoke: ok"
