#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
export PATH="${HOME}/.local/go/bin:${PATH}"
make build
exec ./bin/darwin-node --runtime=fake --standalone --allow-nat-workloads --log-level=debug
