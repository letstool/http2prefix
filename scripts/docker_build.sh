#!/bin/bash

IMAGE_TAG=letstool/http2prefix:latest

docker build \
        -t "$IMAGE_TAG" \
       -f build/Dockerfile \
       .
