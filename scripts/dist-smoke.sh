#!/usr/bin/env bash
# Smoke test of the shipped artefact (ADR-021, instance-config §4): the
# distroless image and the reference compose file, exactly as an operator gets
# them. The E2E suite exercises the code; this one exercises the *delivery* —
# a binary that works but ships in an image that cannot start is not shipped.
#
# Usage: scripts/dist-smoke.sh

set -euo pipefail

PORT=${PORT:-18097}
WORKDIR=$(mktemp -d)
PASS=0

say() { printf '\033[1;34m== %s\033[0m\n' "$*"; }
ok()  { PASS=$((PASS+1)); printf '   \033[32m✔\033[0m %s\n' "$*"; }
die() { printf '   \033[31m✘ %s\033[0m\n' "$*"; exit 1; }

cleanup() {
  status=$?
  if [ $status -ne 0 ]; then
    # The one thing a red run needs and cannot get after teardown: what the
    # container actually said. Without this, "never became healthy" is
    # undiagnosable from CI.
    printf '\n\033[1;33m== container state and logs at failure\033[0m\n'
    (cd "$WORKDIR" && docker compose ps 2>&1; docker compose logs --tail 80 2>&1) || true
  fi
  (cd "$WORKDIR" && docker compose down -v >/dev/null 2>&1) || true
  rm -rf "$WORKDIR"
  if [ $status -eq 0 ]; then
    printf '\n\033[1;32mDIST OK — %d checks passed\033[0m\n' "$PASS"
  else
    printf '\n\033[1;31mDIST FAILED\033[0m\n'
  fi
}
trap cleanup EXIT

ROOT=$(cd "$(dirname "$0")/.." && pwd)

say "building the distroless image"
docker build -q -t akerdock:smoke --build-arg VERSION=smoke "$ROOT" >/dev/null || die "the image does not build"
ok "image built ($(docker images akerdock:smoke --format '{{.Size}}'))"

# The point of distroless: no shell to land in, nothing to pivot from.
docker run --rm --entrypoint /bin/sh akerdock:smoke -c 'echo reachable' 2>/dev/null && die "the image ships a shell"
docker run --rm --entrypoint /bin/busybox akerdock:smoke sh 2>/dev/null && die "the image ships busybox"
# ...and it must not run as root.
docker inspect akerdock:smoke --format '{{.Config.User}}' | grep -q nonroot || die "the image runs as root"
# The version is stamped at build time, not read from git: the tag an operator
# pulls must be able to say what it is.
docker run --rm --entrypoint /akerdock akerdock:smoke version | grep -q smoke || die "the image does not report its build version"
ok "no shell, no busybox, runs as nonroot, reports its version"

say "installing exactly as the runbook says"
cp "$ROOT/docker-compose.yml" "$WORKDIR/"
mkdir -p "$WORKDIR/keys" "$WORKDIR/backups"
printf '1:%s\n' "$(openssl rand -base64 32)" > "$WORKDIR/keys/master.key"
chmod 600 "$WORKDIR/keys/master.key"
# What install.sh does at step 2, for the same reason: the service runs as
# distroless nonroot (uid 65532) and reads the key over a read-only bind
# mount — a 600 file owned by the invoking user is unreadable in the
# container and the boot dies on "permission denied". macOS Docker Desktop
# masks bind-mount ownership, which is exactly why this only bites on Linux.
chown 65532:65532 "$WORKDIR/keys/master.key" 2>/dev/null \
  || sudo -n chown 65532:65532 "$WORKDIR/keys/master.key" \
  || die "cannot chown keys/master.key to uid 65532 (needed for the container to read it)"
# The published image is not built here: point the compose at the local tag.
sed -i.bak 's|ghcr.io/deepteams/akerdock:${AKERDOCK_TAG:?[^}]*}|akerdock:${AKERDOCK_TAG}|' "$WORKDIR/docker-compose.yml"
cat > "$WORKDIR/.env" <<ENV
AKERDOCK_TAG=smoke
POSTGRES_PASSWORD=$(openssl rand -hex 24)
AKERDOCK_INSTANCE_FQDN=akerdock.local
AKERDOCK_ROOT_EMAIL=root@example.com
AKERDOCK_ROOT_NAME=Root
AKERDOCK_ROOT_PASSWORD=$(openssl rand -hex 20)
AKERDOCK_PORT=$PORT
ENV

(cd "$WORKDIR" && docker compose up -d >/dev/null 2>&1) || die "docker compose up failed"

# The compose healthcheck is the binary's own probe: the distroless image has
# no curl to call.
for _ in $(seq 1 60); do
  state=$(docker inspect --format '{{.State.Health.Status}}' akerdock-akerdock-1 2>/dev/null || echo starting)
  [ "$state" = "healthy" ] && break
  sleep 2
done
[ "$state" = "healthy" ] || die "the container never became healthy (state: $state)"
ok "a single 'docker compose up -d' installs; the built-in healthcheck reports healthy"

curl -sf "http://127.0.0.1:${PORT}/api/v1/health" | grep -q '"ok"' || die "the API does not answer on the published port"
LOGS=$(cd "$WORKDIR" && docker compose logs akerdock 2>&1)
echo "$LOGS" | grep -q "migrations applied" || die "migrations did not run at boot"
echo "$LOGS" | grep -q "root user created" || die "the root user was not bootstrapped"
echo "$LOGS" | grep -q "instance ssh key generated" || die "the instance key was not generated"
ok "boot sequence complete behind the single published port"

# PostgreSQL must not be reachable from the host (instance-config §4.1): it is
# why sslmode=disable is acceptable on the private compose network.
docker inspect akerdock-postgres-1 --format '{{.HostConfig.PortBindings}}' | grep -q '5432' && die "PostgreSQL is published on the host"
ok "PostgreSQL stays on the private network, unpublished"

# An upgrade is a tag change, so state must live in the volumes, not the
# container: recreating it keeps the instance identity.
KEY_BEFORE=$(echo "$LOGS" | grep "instance ssh key generated" | head -1)
(cd "$WORKDIR" && docker compose up -d --force-recreate akerdock >/dev/null 2>&1)
for _ in $(seq 1 60); do
  state=$(docker inspect --format '{{.State.Health.Status}}' akerdock-akerdock-1 2>/dev/null || echo starting)
  [ "$state" = "healthy" ] && break
  sleep 2
done
[ "$state" = "healthy" ] || die "the container did not come back healthy after a recreate"
RELOGS=$(cd "$WORKDIR" && docker compose logs akerdock 2>&1)
echo "$RELOGS" | grep -q "root user created" && die "the root user was created twice — bootstrap is not idempotent"
ok "recreating the container keeps the state (no second bootstrap)"
