# hivemind Makefile. Every build is static: CGO_ENABLED=0, -trimpath.

BINARY  := hivemind
PKG     := ./cmd/hivemind
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  := $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)
GOBUILD := CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)"

# The six release platforms: the node runs on Linux; the other platforms get the client CLI.
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

.PHONY: all build build-all smoke test vet fmt clean help

all: vet test build

build:
	@mkdir -p bin
	$(GOBUILD) -o bin/$(BINARY) $(PKG)

# Cross-compile all six release binaries into dist/, named as the release names them.
build-all:
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=""; \
		[ "$$os" = windows ] && ext=.exe; \
		GOOS=$$os GOARCH=$$arch $(GOBUILD) -o dist/$(BINARY)-$$os-$$arch$$ext $(PKG) || exit 1; \
	done

# Run the version and help commands from an empty directory, as a user would.
smoke: build
	@d=$$(mktemp -d) && cd "$$d" && "$(CURDIR)/bin/$(BINARY)" version && "$(CURDIR)/bin/$(BINARY)" --help >/dev/null && rm -rf "$$d"

test:
	go test -race -count=1 ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

clean:
	rm -rf bin dist

help:
	@echo "build      build bin/hivemind for this machine"
	@echo "build-all  cross-compile the six release binaries into dist/"
	@echo "smoke      run version and --help from an empty directory"
	@echo "test       go test -race ./..."
	@echo "vet        go vet ./..."
	@echo "fmt        gofmt -w ."
	@echo "clean      remove bin/ and dist/"
