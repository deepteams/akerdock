#!/usr/bin/env bash
# The single product E2E journey (ADR-028).
#
# It boots PostgreSQL, one Docker-in-Docker target server and AkerDock, then
# proves the assembled path that unit/module tests cannot: SSH onboarding,
# Traefik bootstrap, deployment, HTTPS routing, live logs, a zero-downtime
# rolling switch, authentication and safe deletion.
#
# Usage:
#   scripts/e2e.sh                   run the complete journey
#   E2E_KEEP=--keep scripts/e2e.sh   leave containers running for diagnosis
#   E2E_FRESH=1 scripts/e2e.sh       wipe the DinD image cache first
#
# Requirements: docker, go, curl, python3, ssh-keygen, openssl.

set -euo pipefail

if [ $# -ne 0 ]; then
  printf 'the E2E suite has one complete journey and accepts no scenario argument\n' >&2
  exit 2
fi

ROOT_DIR=$(cd "$(dirname "$0")/.." && pwd)
export E2E_SHARD=complete

# shellcheck source=e2e/lib.sh
source "$ROOT_DIR/scripts/e2e/lib.sh"
# shellcheck source=e2e/complete.sh
source "$ROOT_DIR/scripts/e2e/complete.sh"
