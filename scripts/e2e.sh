#!/usr/bin/env bash
# End-to-end test of AkerDock (ADR-026: Docker-in-Docker E2E on every commit).
#
# The suite runs as four SHARDS in parallel. Each shard is a complete, isolated
# stack — its own PostgreSQL, its own DinD target server, its own akerdock, its
# own image cache — driving the public API through one slice of the product:
#
#   deploy    the core deployment path: image and Dockerfile builds, routing,
#             zero-downtime switch, crash recovery, remnants, audit
#   build     the build packs (git, deploy keys, static, nixpacks), the Git
#             webhooks, rollback, volumes, certificates, private registries
#   data      managed databases, backups, S3, restore drills, safe deletion
#   platform  realtime, notifications, scheduled tasks, telemetry, browser
#             auth, and the N-1 rolling upgrade
#
# Sharding is what keeps the suite runnable on every commit: the sections do not
# get faster, they get run at the same time. Nothing is shared between shards —
# not a port, not a container name, not a Docker volume — because a shared
# anything would make one shard's verdict depend on another shard's timing.
#
# A fifth shard, `smoke`, is NOT part of the parallel run: it is the minimal
# per-commit gate (server onboarding, deploy, HTTPS routing, zero-downtime
# switch, deletion). The four full shards above are the nightly catalogue.
# See the test pyramid in docs/specs/e2e-test-plan.md §2 and CONTRIBUTING.md.
#
# Usage:
#   scripts/e2e.sh smoke            the minimal per-commit gate
#   scripts/e2e.sh                  every full shard, in parallel (nightly)
#   scripts/e2e.sh deploy           one shard, output live (how you debug)
#   E2E_KEEP=--keep scripts/e2e.sh  leave the containers up afterwards
#   E2E_FRESH=1 scripts/e2e.sh      wipe the image caches first
#
# Requirements: docker, go, curl, python3, ssh-keygen, openssl.

set -uo pipefail

ROOT_DIR=$(cd "$(dirname "$0")/.." && pwd)
SHARDS=(deploy build data platform compose github)
LOG_DIR=$(mktemp -d)

run_shard() { # run_shard NAME — one isolated stack, one slice of the suite
  local shard=$1
  E2E_SHARD="$shard" bash -c "source '$ROOT_DIR/scripts/e2e/lib.sh'; source '$ROOT_DIR/scripts/e2e/$shard.sh'"
}

# One shard in the foreground: the way to debug a failure, and how the
# per-commit gate runs (`scripts/e2e.sh smoke`).
if [ $# -gt 0 ]; then
  case " ${SHARDS[*]} smoke " in
    *" $1 "*) run_shard "$1"; exit $? ;;
    *) printf 'unknown shard %q — expected smoke or one of: %s\n' "$1" "${SHARDS[*]}" >&2; exit 2 ;;
  esac
fi

# All shards at once. The output is captured per shard rather than interleaved:
# four suites talking over each other is unreadable, and what matters on a
# failure is that one shard's story, in its own order.
printf '\033[1m== running %d e2e shards in parallel\033[0m\n' "${#SHARDS[@]}"
START=$(date +%s)
# Parallel arrays, not an associative one: the bash that ships with macOS is
# 3.2, and `declare -A` is a bash 4 feature. A test harness that only runs on
# the maintainer's Linux box is a test harness nobody runs.
PIDS=()
for shard in "${SHARDS[@]}"; do
  run_shard "$shard" > "$LOG_DIR/$shard.log" 2>&1 &
  PIDS+=($!)
  printf '   \033[2m%-9s started\033[0m\n' "$shard"
done

FAILED=()
TOTAL=0
for i in "${!SHARDS[@]}"; do
  shard=${SHARDS[$i]}
  if wait "${PIDS[$i]}"; then
    checks=$(grep -oE 'E2E OK — [0-9]+' "$LOG_DIR/$shard.log" | grep -oE '[0-9]+$' || echo 0)
    TOTAL=$((TOTAL + checks))
    printf '   \033[32m✔\033[0m %-9s %s checks\n' "$shard" "$checks"
  else
    FAILED+=("$shard")
    printf '   \033[31m✘\033[0m %-9s FAILED\n' "$shard"
  fi
done

ELAPSED=$(($(date +%s) - START))

# A failing shard prints its whole log. A summary that only says "build failed"
# sends the reader hunting for a file in a temp directory they were never told
# about.
# `"${FAILED[@]}"` on an empty array is an "unbound variable" under `set -u` in
# bash 3.2 — the green path would have failed on success, which is the one
# failure a test harness must never have.
if [ ${#FAILED[@]} -gt 0 ]; then
  for shard in "${FAILED[@]}"; do
    printf '\n\033[1;31m───── %s ─────\033[0m\n' "$shard"
    cat "$LOG_DIR/$shard.log"
  done
fi

printf '\n\033[2m   logs: %s   wall clock: %ds\033[0m\n' "$LOG_DIR" "$ELAPSED"
if [ ${#FAILED[@]} -gt 0 ]; then
  printf '\033[1;31mE2E FAILED — %s\033[0m\n' "${FAILED[*]}"
  exit 1
fi
printf '\033[1;32mE2E OK — %d checks passed across %d shards in %ds\033[0m\n' "$TOTAL" "${#SHARDS[@]}" "$ELAPSED"
