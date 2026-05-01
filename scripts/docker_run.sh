#!/usr/bin/env bash
set -euo pipefail

# Mode 1 (default): fetch gzipped CSV from the CDN and compile prefix.mmdb.
# Set LICENSE_KEY to your token for licensed (higher quota) access.
docker run -it --rm \
  -p 8080:8080 \
  -v "$(pwd)/db:/data:rw" \
  -e LISTEN_ADDR=0.0.0.0:8080 \
  letstool/http2prefix:latest

# Mode 2 (peer): download prefix.mmdb from another running http2prefix instance.
# Uncomment and set prefix_DB_URL to use this mode:
#
# docker run -it --rm \
#   -p 8080:8080 \
#   -v "$(pwd)/db:/data:rw" \
#   -e LISTEN_ADDR=0.0.0.0:8080 \
#   -e PREFIX_DB_URL=http://upstream-host:8080 \
#   letstool/http2prefix:latest
