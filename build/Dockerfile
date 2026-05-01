# syntax=docker/dockerfile:1.4

##############################
#  Stage 1: Build (golang)
##############################
FROM golang:1.24-alpine AS builder

# Go build environment variables
ENV CGO_ENABLED=0 \
    GOOS=linux \
    GOARCH=amd64 \
    GO111MODULE=on

WORKDIR /src

# Download dependencies (cached as a separate layer)
COPY go.mod go.sum ./
RUN go mod download

# Copy the full source tree
COPY . .

# Compile the static binary
# The output path is fixed: /out/http2prefix
RUN go build \
    -trimpath \
    -ldflags="-extldflags -static -s -w" \
    -tags netgo \
    -o /out/http2prefix ./cmd/http2prefix

##############################
#  Stage 2: Runtime (scratch)
##############################
FROM scratch AS runtime

# Copy only the binary from the builder stage
COPY --from=builder /out/http2prefix /http2prefix

# OCI image metadata
LABEL org.opencontainers.image.title="http2prefix" \
      org.opencontainers.image.description="HTTP Server → prefix (Go)" \
      org.opencontainers.image.source="https://github.com/letstool/http2prefix" \
      org.opencontainers.image.authors="LetsTool.net" \
      org.opencontainers.image.licenses="MIT"

USER 65534:65534

# Entry point
ENTRYPOINT ["/http2prefix"]
