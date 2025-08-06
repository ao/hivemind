# Build stage
FROM golang:1.23-alpine AS builder

# Install git and ca-certificates (needed for go modules and HTTPS)
RUN apk add --no-cache git ca-certificates tzdata

# Set working directory
WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY . .

# Build arguments for version information
ARG VERSION=dev
ARG BUILD_TIME
ARG GIT_COMMIT

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-X 'main.Version=${VERSION}' -X 'main.BuildTime=${BUILD_TIME}' -X 'main.GitCommit=${GIT_COMMIT}' -s -w" \
    -a -installsuffix cgo \
    -o hivemind \
    ./cmd/hivemind

# Final stage
FROM alpine:latest

# Install ca-certificates for HTTPS requests
RUN apk --no-cache add ca-certificates tzdata

# Create non-root user
RUN addgroup -g 1001 -S hivemind && \
    adduser -u 1001 -S hivemind -G hivemind

# Set working directory
WORKDIR /app

# Copy binary from builder stage
COPY --from=builder /app/hivemind .

# Copy templates and static files if they exist
COPY --from=builder /app/templates/ ./templates/

# Change ownership to non-root user
RUN chown -R hivemind:hivemind /app

# Switch to non-root user
USER hivemind

# Expose port (adjust as needed)
EXPOSE 8080

# Health check
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD ./hivemind health || exit 1

# Run the binary
ENTRYPOINT ["./hivemind"]