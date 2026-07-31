#!/usr/bin/env bash
# Coverage gate for handwritten Go code.
#
# Generated OpenAPI/sqlc packages contain no authored decisions and are not
# gated. The default floor is 90%; packages below it hold an explicit ratchet
# floor in scripts/coverage-floors.txt (policy lives there, mechanism here).
#
# With COVERPROFILE set, the gate analyses that existing profile instead of
# running the tests itself — CI produces one `go test -race -coverprofile`
# run and feeds it to this script, so the suite executes once, not twice.

set -euo pipefail

ROOT_DIR=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT_DIR"

if [ -n "${COVERPROFILE:-}" ]; then
  PROFILE=$COVERPROFILE
else
  PROFILE=$(mktemp "${TMPDIR:-/tmp}/akerdock-cover.XXXXXX")
  trap 'rm -f "$PROFILE"' EXIT
  go test ./internal/... -coverprofile="$PROFILE" -count=1
fi

awk -F'\t' '
  NR == FNR {
    # +0 forces the floor to a number: a string floor would make the
    # comparison below lexical on some awks and fail exact-floor packages.
    if ($0 !~ /^#/ && NF == 2) ratchet[$1] = $2 + 0
    next
  }
  FNR == 1 { next }
  {
    split($0, cols, " ")
    file = cols[1]
    sub(/:[0-9].*$/, "", file)
    package = file
    sub(/\/[^\/]+$/, "", package)
    statements[package] += cols[2]
    if (cols[3] > 0) covered[package] += cols[2]
  }
  END {
    failed = 0
    for (package in statements) {
      # Only handwritten internal packages are gated; generated ones are not.
      if (package !~ /\/internal\//) continue
      if (package ~ /\/internal\/(api|store)$/) continue
      percent = 100 * covered[package] / statements[package]
      short = package
      sub(/^.*\/internal\//, "internal/", short)
      minimum = 90
      kind = "unit"
      if (short in ratchet) {
        minimum = ratchet[short]
        kind = "ratchet (target 90)"
      }
      printf "%-34s %6.1f%%  minimum %4.1f%%  %s\n", short, percent, minimum, kind
      if (percent + 0.0001 < minimum) {
        failed = 1
      }
    }
    exit failed
  }
' scripts/coverage-floors.txt "$PROFILE" | sort
