#!/bin/sh
# Opt-in, in-place PostgreSQL major upgrade of the INSTANCE database (ADR-039).
#
# The reference compose pins the PostgreSQL major (docker-compose.yml). A major
# bump (e.g. 16 -> 17) is not compatible in place with an existing data volume:
# the pinned image would crash-loop with "database files are incompatible".
# This script performs the upgrade deliberately and safely:
#
#   1. detects the data volume's major vs the compose target;
#   2. takes a FULL filesystem copy of the data volume (version-agnostic net);
#   3. runs pgautoupgrade one-shot to migrate the volume in place;
#   4. brings the stack back up on the official postgres image and verifies.
#
# It is NEVER run automatically — an in-place upgrade of the datastore that holds
# all instance state is a human decision (ADR-039). Run it, from the checkout
# root, only when install.sh (or a boot) reports a version mismatch:
#
#   ./scripts/pg-upgrade.sh          # interactive confirmation
#   ./scripts/pg-upgrade.sh --yes    # non-interactive (scripted maintenance)
#
# The pre-upgrade volume copy under backups/ is your rollback: keep it until the
# instance has run verified for several days.

set -eu
cd "$(dirname "$0")/.."

say()  { printf '\033[1;34m== %s\033[0m\n' "$*"; }
note() { printf '   %s\n' "$*"; }
warn() { printf '   \033[33m%s\033[0m\n' "$*"; }
die()  { printf '\033[1;31mERROR: %s\033[0m\n' "$*" >&2; exit 1; }

command -v docker >/dev/null 2>&1 || die "docker is required"
docker compose version >/dev/null 2>&1 || die "Docker Compose v2 is required"
[ -f docker-compose.yml ] || die "run this from the root of an AkerDock checkout"
[ -f .env ] || die ".env not found — this is an installed instance's upgrade tool, run install.sh first"

PROJECT=$(sed -n 's/^name: *//p' docker-compose.yml | head -1)
PROJECT=${PROJECT:-akerdock}
# The compose volume `akerdock_pgdata` becomes `<project>_akerdock_pgdata`.
PGVOL=$(docker volume ls -q --filter "name=${PROJECT}_akerdock_pgdata" | head -1)
[ -n "$PGVOL" ] || die "no PostgreSQL data volume (${PROJECT}_akerdock_pgdata) — nothing to upgrade; fresh installs start on the pinned major directly"

# `busybox` is only used as a tiny, well-known helper to read/copy the volume.
data_major=$(docker run --rm -v "$PGVOL":/d:ro busybox cat /d/PG_VERSION 2>/dev/null | tr -d '[:space:]' || true)
[ -n "$data_major" ] || die "could not read $PGVOL/PG_VERSION — is this really a PostgreSQL data volume?"
target_major=$(sed -n 's/.*image: *postgres:\([0-9][0-9]*\).*/\1/p' docker-compose.yml | head -1)
[ -n "$target_major" ] || die "could not read the target postgres major from docker-compose.yml"

if [ "$data_major" = "$target_major" ]; then
  say "PostgreSQL data is already major $data_major — nothing to do"
  exit 0
fi
if [ "$data_major" -gt "$target_major" ] 2>/dev/null; then
  die "the data is major $data_major, newer than the compose target $target_major — downgrades are not supported"
fi

say "PostgreSQL major upgrade: $data_major -> $target_major"
warn "This migrates the instance database IN PLACE. A full copy of the data"
warn "volume is taken first (backups/) and is your only rollback."

if [ "${1:-}" != "--yes" ]; then
  printf '   Type the target major (%s) to proceed, anything else to abort: ' "$target_major"
  read -r ans
  [ "$ans" = "$target_major" ] || die "aborted — nothing was changed"
fi

POSTGRES_PASSWORD=$(sed -n 's/^POSTGRES_PASSWORD=//p' .env)
[ -n "$POSTGRES_PASSWORD" ] || die "POSTGRES_PASSWORD not found in .env"

# The stack must be idle: nothing may write to the volume during the copy and
# the in-place upgrade.
say "stopping the stack"
docker compose down

ts=$(date +%Y%m%d-%H%M%S)
mkdir -p backups
archive="pgdata-pre-upgrade-${data_major}to${target_major}-${ts}.tar.gz"
say "backing up the data volume to backups/$archive (rollback copy)"
docker run --rm -v "$PGVOL":/from:ro -v "$PWD/backups":/to busybox \
  tar czf "/to/$archive" -C /from .
[ -s "backups/$archive" ] || die "the backup archive is empty — aborting before touching the data"

# The -debian variant is glibc-based like the official postgres image we ship:
# the data dir keeps the same libc collations across the upgrade (a musl/alpine
# tool on a glibc-inited cluster risks index corruption). It tracks the current
# Debian per major, so it exists for every major we might target.
img="pgautoupgrade/pgautoupgrade:${target_major}-debian"
say "running the in-place upgrade with $img"
docker pull "$img" >/dev/null
if ! docker run --rm \
    -e POSTGRES_USER=akerdock -e POSTGRES_DB=akerdock -e POSTGRES_PASSWORD="$POSTGRES_PASSWORD" \
    -e PGAUTO_ONESHOT=yes \
    -e PGDATA=/var/lib/postgresql/data \
    -v "$PGVOL":/var/lib/postgresql/data \
    "$img"; then
  warn "the in-place upgrade FAILED — the data volume may be partially migrated."
  warn "restore the pre-upgrade copy before retrying anything:"
  warn "  docker run --rm -v $PGVOL:/to -v \$PWD/backups:/from busybox \\"
  warn "    sh -c 'rm -rf /to/* /to/..?* /to/.[!.]* 2>/dev/null; tar xzf /from/$archive -C /to'"
  die "aborted after upgrade failure — data volume backup is backups/$archive"
fi

say "starting the stack on postgres:$target_major"
docker compose up -d

say "waiting for the control plane to become healthy"
cid=$(docker compose ps -q akerdock)
[ -n "$cid" ] || die "the akerdock container is not there after 'docker compose up'"
state=starting
for _ in $(seq 1 90); do
  state=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}starting{{end}}' "$cid" 2>/dev/null || echo starting)
  [ "$state" = "healthy" ] && break
  sleep 2
done
if [ "$state" != "healthy" ]; then
  docker compose logs --tail 30 akerdock || true
  warn "akerdock did not become healthy (state: $state)."
  warn "if PostgreSQL is at fault, restore backups/$archive into $PGVOL and re-pin the old major."
  die "upgrade left the stack unhealthy — investigate before deleting the backup"
fi

say "upgrade complete: PostgreSQL $data_major -> $target_major"
note "rollback copy kept at backups/$archive — delete it only after several days of verified operation."
