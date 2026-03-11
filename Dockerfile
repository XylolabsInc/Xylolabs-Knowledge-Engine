# =============================================================================
# Build stage
# =============================================================================
FROM golang:1.26-alpine AS builder

WORKDIR /build

# Install build dependencies
RUN apk add --no-cache git ca-certificates tzdata

# Download dependencies first (cached layer)
COPY go.mod go.sum* ./
RUN go mod download

# Copy source and build
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w -extldflags '-static'" \
    -o /build/xylolabs-kb ./cmd/xylolabs-kb/

# =============================================================================
# Run stage
# =============================================================================
FROM alpine:latest

# Install runtime dependencies
RUN apk add --no-cache ca-certificates tzdata curl

# Create non-root user
RUN addgroup -g 1001 -S xylolabs && \
    adduser -u 1001 -S xylolabs -G xylolabs

# Create data directories with correct ownership
RUN mkdir -p /data/attachments && \
    chown -R xylolabs:xylolabs /data

WORKDIR /app

# Copy binary from builder
COPY --from=builder /build/xylolabs-kb .
RUN chown xylolabs:xylolabs /app/xylolabs-kb

USER xylolabs

# Expose API port
EXPOSE 8080

# Health check via the /health endpoint
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD curl -f http://localhost:8080/health || exit 1

ENTRYPOINT ["./xylolabs-kb"]
