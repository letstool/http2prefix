#!/bin/bash

go build \
    -trimpath \
    -ldflags="-extldflags -static -s -w" \
    -o ./out/http2prefix ./cmd/http2prefix
