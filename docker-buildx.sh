#!/bin/bash

release=$(git describe --abbrev=0 --tags)

docker buildx build --push --no-cache \
	--build-arg RELEASE="${release}" \
	-t "qdrant/prom-label-proxy:${release}" \
	--platform=linux/amd64 \
	-f Dockerfile.dev .

docker buildx prune -f
