#!/usr/bin/env bash
# govulncheck as a gate that stays green against the unfixable: findings in
# scripts/vulncheck-triage.txt (advisories with no fixed release, each with a
# written justification) are tolerated; anything NOT listed fails the build.
# A permanently red gate teaches everyone to ignore it — a triage list keeps
# the gate meaningful while upstream catches up.

set -uo pipefail

ROOT_DIR=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT_DIR"

OUTPUT=$(go run golang.org/x/vuln/cmd/govulncheck@v1.1.4 ./... 2>&1)
STATUS=$?
echo "$OUTPUT"
if [ "$STATUS" -eq 0 ]; then
  exit 0
fi

# The "Vulnerability #N: GO-…" headings list only the findings that affect
# reachable code — the ones that made the run fail. A non-zero exit with no
# such heading is a tool failure (go run flattens govulncheck's exit codes,
# so the output is the only reliable signal).
FOUND=$(echo "$OUTPUT" | awk '/^Vulnerability #/ { sub(/:$/, "", $3); print $3 }' | sort -u)
if [ -z "$FOUND" ]; then
  exit "$STATUS"
fi
NEW=0
for id in $FOUND; do
  if ! grep -q "^${id}	" scripts/vulncheck-triage.txt; then
    echo "::error::untriaged vulnerability ${id} — fix the dependency or triage it in scripts/vulncheck-triage.txt with a justification"
    NEW=1
  fi
done
if [ "$NEW" -eq 0 ]; then
  echo "govulncheck: all findings are triaged in scripts/vulncheck-triage.txt (no fixed release upstream)"
fi
exit "$NEW"
