#!/usr/bin/env bash
# Coverage gate for handwritten Go code.
#
# Generated OpenAPI/sqlc packages contain no authored decisions. The HTTP and
# job packages are module boundaries: their current floors stay explicit below
# while deterministic policy continues to move into >=90%-covered packages.

set -euo pipefail

ROOT_DIR=$(cd "$(dirname "$0")/.." && pwd)
PROFILE=$(mktemp "${TMPDIR:-/tmp}/akerdock-cover.XXXXXX")
trap 'rm -f "$PROFILE"' EXIT

cd "$ROOT_DIR"
go test ./internal/... -coverprofile="$PROFILE" -count=1

awk '
  NR == 1 { next }
  {
    file = $1
    sub(/:[0-9].*$/, "", file)
    package = file
    sub(/\/[^\/]+$/, "", package)
    statements[package] += $2
    if ($3 > 0) covered[package] += $2
  }
  END {
    failed = 0
    for (package in statements) {
      if (package ~ /\/internal\/(api|store)$/) {
        continue
      }
      percent = 100 * covered[package] / statements[package]
      minimum = 90
      kind = "unit"
      if (package ~ /\/internal\/handlers$/) {
        minimum = 60
        kind = "module boundary (target 90)"
      } else if (package ~ /\/internal\/jobs$/) {
        minimum = 40
        kind = "module boundary (target 90)"
      }
      short = package
      sub(/^.*\/internal\//, "internal/", short)
      printf "%-34s %6.1f%%  minimum %4.1f%%  %s\n", short, percent, minimum, kind
      if (percent + 0.0001 < minimum) {
        failed = 1
      }
    }
    exit failed
  }
' "$PROFILE" | sort
