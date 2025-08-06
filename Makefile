# Hivemind Container Orchestration System
# Makefile for Go implementation

# Variables
BINARY_NAME=hivemind
VERSION=$(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
BUILD_TIME=$(shell date -u '+%Y-%m-%d_%H:%M:%S')
LDFLAGS=-ldflags "-X main.Version=$(VERSION) -X main.BuildTime=$(BUILD_TIME)"
GO=go
GOTEST=$(GO) test
GOVET=$(GO) vet
GOFMT=$(GO) fmt
GOLINT=golangci-lint
DOCKER=docker
DOCKER_IMAGE=hivemind
DOCKER_TAG=$(VERSION)

# Default target
.PHONY: all
all: clean deps fmt lint test build

# Build the application
.PHONY: build
build:
	@echo "Building $(BINARY_NAME)..."
	@mkdir -p bin
	$(GO) build $(LDFLAGS) -o bin/$(BINARY_NAME) ./cmd/hivemind

# Run the application
.PHONY: run
run: build
	@echo "Running $(BINARY_NAME)..."
	./bin/$(BINARY_NAME)

# Clean build artifacts
.PHONY: clean
clean:
	@echo "Cleaning..."
	@rm -rf bin/
	@rm -f $(BINARY_NAME)
	@rm -rf dist/

# Install dependencies
.PHONY: deps
deps:
	@echo "Installing dependencies..."
	$(GO) mod download
	$(GO) mod tidy

# Format code
.PHONY: fmt
fmt:
	@echo "Formatting code..."
	$(GOFMT) ./...

# Lint code
.PHONY: lint
lint:
	@echo "Linting code..."
	$(GOVET) ./...
	@if command -v $(GOLINT) > /dev/null; then \
		$(GOLINT) run ./...; \
	else \
		echo "golangci-lint not installed. Skipping lint."; \
	fi

# Run tests
.PHONY: test
test:
	@echo "Running tests..."
	$(GOTEST) -v ./...

# Run tests with coverage
.PHONY: test-coverage
test-coverage:
	@echo "Running tests with coverage..."
	$(GOTEST) -v -coverprofile=coverage.out ./...
	$(GO) tool cover -html=coverage.out -o coverage.html

# Build Docker image
.PHONY: docker-build
docker-build:
	@echo "Building Docker image..."
	$(DOCKER) build -t $(DOCKER_IMAGE):$(DOCKER_TAG) .

# Push Docker image
.PHONY: docker-push
docker-push: docker-build
	@echo "Pushing Docker image..."
	$(DOCKER) push $(DOCKER_IMAGE):$(DOCKER_TAG)

# Generate Go code from protobuf files (if needed)
.PHONY: proto
proto:
	@echo "Generating protobuf code..."
	@if command -v protoc > /dev/null; then \
		protoc --go_out=. --go-grpc_out=. ./proto/*.proto; \
	else \
		echo "protoc not installed. Skipping proto generation."; \
	fi

# Install development tools
.PHONY: install-tools
install-tools:
	@echo "Installing development tools..."
	go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
	go install github.com/golang/protobuf/protoc-gen-go@latest
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

# Help target
.PHONY: help
help:
	@echo "Hivemind Container Orchestration System - Makefile targets:"
	@echo "  all            - Clean, install dependencies, format, lint, test, and build"
	@echo "  build          - Build the application"
	@echo "  run            - Run the application"
	@echo "  clean          - Remove build artifacts"
	@echo "  deps           - Install dependencies"
	@echo "  fmt            - Format code"
	@echo "  lint           - Lint code"
	@echo "  test           - Run tests"
	@echo "  test-coverage  - Run tests with coverage"
	@echo "  docker-build   - Build Docker image"
	@echo "  docker-push    - Push Docker image"
	@echo "  proto          - Generate Go code from protobuf files"
	@echo "  install-tools  - Install development tools"
	@echo "  help           - Show this help message"