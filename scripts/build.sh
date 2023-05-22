#!/usr/bin/env bash

cd $(dirname "${BASH_SOURCE[0]}")
set -euxo pipefail

CGO_ENABLED=0 go build -ldflags "-s -w" -o ./coder-logstream-kube ../
docker build -t coder-logstream-kube:latest .
