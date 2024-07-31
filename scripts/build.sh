#!/usr/bin/env bash

cd "$(dirname "${BASH_SOURCE[0]}")"
set -euxo pipefail

# Set CI to false if not set
CI=${CI:-false}

# Get current architecture
current=$(go env GOARCH)
# Arhcitectures to build for
archs=(amd64 arm64 arm)

# build for all architectures
for arch in "${archs[@]}"; do
    echo "Building for $arch"
    GOARCH=$arch GOOS=linux CGO_ENABLED=0 go build -ldflags "-s -w" -o ./coder-logstream-kube-"$arch" ../
done

# We have to use docker buildx to tag multiple images with
# platforms tragically, so we have to create a builder.
BUILDER_NAME="coder-logstream-kube"
BUILDER_EXISTS=$(docker buildx ls | grep $BUILDER_NAME || true)

# If builder doesn't exist, create it
if [ -z "$BUILDER_EXISTS" ]; then
    echo "Creating dockerx builder $BUILDER_NAME..."
    docker buildx create --use --platform=linux/arm64,linux/amd64,linux/arm/v7 --name $BUILDER_NAME
else
    echo "Builder $BUILDER_NAME already exists. Using it."
fi

# Ensure the builder is bootstrapped and ready to use
docker buildx inspect --bootstrap &>/dev/null

# Build and push the image
if [ "$CI" = "false" ]; then
    docker buildx build --platform linux/"$current" -t coder-logstream-kube --load .
else
    VERSION=$(../scripts/version.sh)
    BASE=ghcr.io/coder/coder-logstream-kube
    IMAGE=$BASE:$VERSION
    # if version contains "rc" skip pushing to latest
    if [[ $VERSION == *"rc"* ]]; then
        docker buildx build --platform linux/amd64,linux/arm64,linux/arm/v7 -t "$IMAGE" --push .
    else
        docker buildx build --platform linux/amd64,linux/arm64,linux/arm/v7 -t "$IMAGE" -t $BASE:latest --push .
    fi
fi
