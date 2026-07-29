#!/bin/sh
# Install or update AkerDock from a source checkout.
#
#   git clone https://github.com/deepteams/akerdock.git && cd akerdock && ./install.sh
#
# This is the reference two-service distribution (ADR-021, docker-compose.yml)
# with one difference: the image is built locally from ./Dockerfile instead of
# pulled from ghcr.io, so a clone of the sources is all an operator needs.
#
# First run:  generates keys/master.key and .env, builds the image from the
#             working tree and starts the stack.
# Later runs: (after `git pull`) rebuild the image from the current checkout
#             and roll the stack forward — migrations apply at boot (ADR-025),
#             state lives in the named volumes so nothing is lost.
#
# Optional environment overrides, read on the FIRST run only (they seed .env,
# which is the source of truth afterwards):
#   AKERDOCK_PORT           published control-plane port (default 8080)
#   AKERDOCK_INSTANCE_FQDN  public FQDN of the instance
#   AKERDOCK_ACME_EMAIL     Let's Encrypt contact address
#   AKERDOCK_ROOT_EMAIL     first root user (default admin@example.com)
#   AKERDOCK_ROOT_NAME      first root user display name (default Admin)
#   AKERDOCK_ROOT_PASSWORD  first root user password (default: generated)

set -eu
cd "$(dirname "$0")"

say()  { printf '\033[1;34m== %s\033[0m\n' "$*"; }
note() { printf '   %s\n' "$*"; }
warn() { printf '   \033[33m%s\033[0m\n' "$*"; }
die()  { printf '\033[1;31mERROR: %s\033[0m\n' "$*" >&2; exit 1; }

## Prerequisites (runbook install.md): Docker Engine + Compose v2, openssl for
## the secrets, and a complete source checkout to build from.
command -v docker >/dev/null 2>&1 || die "docker is required (Docker Engine >= 24, see docs/runbooks/install.md)"
docker info >/dev/null 2>&1 || die "the Docker daemon is not reachable — is it running, and can this user talk to it?"
docker compose version >/dev/null 2>&1 || die "Docker Compose v2 is required ('docker compose', the v1 'docker-compose' binary is not supported)"
command -v openssl >/dev/null 2>&1 || die "openssl is required to generate the master key and passwords"
[ -f docker-compose.yml ] && [ -f Dockerfile ] && [ -f go.mod ] || die "run this script from the root of an AkerDock source checkout"

# The image tag doubles as the reported version (-ldflags in the Dockerfile).
# Built from a tarball rather than a clone, fall back to a timestamp the
# caller provides implicitly by running now.
VERSION=$(git describe --tags --always --dirty 2>/dev/null || true)
[ -n "$VERSION" ] || VERSION="src-$(date +%Y%m%d%H%M%S)"

FIRST_INSTALL=0
[ -f .env ] || FIRST_INSTALL=1

# Whether a previous install is up and healthy — decided BEFORE touching
# anything, because a healthy boot means the root user bootstrap already ran
# and its variables in .env are dead weight (instance-config §6).
WAS_HEALTHY=0
if [ "$FIRST_INSTALL" = 0 ]; then
  cid=$(docker compose ps -q akerdock 2>/dev/null || true)
  if [ -n "$cid" ] && [ "$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{end}}' "$cid" 2>/dev/null)" = "healthy" ]; then
    WAS_HEALTHY=1
  fi
fi

## Master encryption key (ADR-003, instance-config §3). Never regenerated:
## losing it makes every stored secret unrecoverable.
umask 077
mkdir -p keys backups
[ -d keys/master.key ] && die "keys/master.key is a directory (a compose run created the bind-mount source) — remove it and re-run"
if [ ! -f keys/master.key ]; then
  say "generating the master encryption key (ADR-003)"
  printf '1:%s\n' "$(openssl rand -base64 32)" > keys/master.key
  chmod 600 keys/master.key
  warn "keys/master.key created — back it up OFF this machine now, separately from"
  warn "the database backups: once the first secret is stored, losing this file"
  warn "makes every secret unrecoverable."
else
  chmod 600 keys/master.key 2>/dev/null || true
fi
[ -s keys/master.key ] || die "keys/master.key exists but is empty — restore it from your backup"

## The akerdock service runs as distroless nonroot (uid 65532) and reads the
## key over a read-only bind mount: the file must belong to that uid or the
## boot dies with "permission denied" — better to fail here, where the
## operator is looking, with the exact command to run.
owner=$(stat -c %u keys/master.key 2>/dev/null || stat -f %u keys/master.key 2>/dev/null || echo '?')
if [ "$owner" != 65532 ]; then
  say "granting the container user (uid 65532, distroless nonroot) read access to keys/master.key"
  chown 65532:65532 keys/master.key 2>/dev/null \
    || sudo chown 65532:65532 keys/master.key \
    || die "keys/master.key is unreadable by the container (distroless nonroot, uid 65532) — run: sudo chown 65532:65532 keys/master.key  and re-run this script"
fi

## Instance configuration (.env, instance-config §4). Generated once, then
## only the image tag is maintained by this script — everything else is yours.
GENERATED_ROOT_PASSWORD=""
if [ "$FIRST_INSTALL" = 1 ]; then
  say "writing .env (first install)"
  ROOT_EMAIL=${AKERDOCK_ROOT_EMAIL:-admin@example.com}
  ROOT_NAME=${AKERDOCK_ROOT_NAME:-Admin}
  if [ -n "${AKERDOCK_ROOT_PASSWORD:-}" ]; then
    ROOT_PASSWORD=$AKERDOCK_ROOT_PASSWORD
  else
    ROOT_PASSWORD=$(openssl rand -hex 20)
    GENERATED_ROOT_PASSWORD=$ROOT_PASSWORD
  fi
  cat > .env <<EOF
# Generated by install.sh — never commit this file.
# AKERDOCK_TAG is maintained by install.sh; edit everything else freely.
AKERDOCK_TAG=${VERSION}
POSTGRES_PASSWORD=$(openssl rand -hex 24)
AKERDOCK_PORT=${AKERDOCK_PORT:-8080}
AKERDOCK_INSTANCE_FQDN=${AKERDOCK_INSTANCE_FQDN:-}
AKERDOCK_ACME_EMAIL=${AKERDOCK_ACME_EMAIL:-}
# First root user (§10.2): read only while no user exists, consumed once.
AKERDOCK_ROOT_EMAIL=${ROOT_EMAIL}
AKERDOCK_ROOT_NAME=${ROOT_NAME}
AKERDOCK_ROOT_PASSWORD=${ROOT_PASSWORD}
# SSH user of the pre-registered localhost server (instance-config §6.2):
# the user running this install — install.sh authorizes the instance key
# for it below. Read at first bootstrap only.
AKERDOCK_LOCALHOST_USER=$(id -un)
# The instance's own proxy routes its FQDN to the published port
# (00-control-plane, proxy-contract §5.7), so every request arrives through
# the compose network's gateway. Without this, the address recorded for a
# caller is that gateway — audit trail, auth rate limiter and a token's CIDR
# allowlist all lose the real client. \`gateway\` is resolved at each start
# rather than written down here: the network does not exist yet at install
# time, and a custom subnet in the override would move the address.
AKERDOCK_TRUSTED_PROXIES=gateway
EOF
  chmod 600 .env
else
  say "updating to ${VERSION}"
  if grep -q '^AKERDOCK_TAG=' .env; then
    sed -i.bak "s|^AKERDOCK_TAG=.*|AKERDOCK_TAG=${VERSION}|" .env && rm -f .env.bak
  else
    printf 'AKERDOCK_TAG=%s\n' "$VERSION" >> .env
  fi
  # Instances installed before the localhost server existed (§6.2) seed it at
  # their next boot: give that seed the installing user, like a fresh install
  # would. A variable already present — even empty — is the operator's choice.
  if ! grep -q '^AKERDOCK_LOCALHOST_USER=' .env; then
    printf 'AKERDOCK_LOCALHOST_USER=%s\n' "$(id -un)" >> .env
  fi
  # Instances installed before this variable existed record the proxy's address
  # for every caller — one address in the whole audit trail, one rate-limit
  # bucket for the internet, a token CIDR allowlist that admits whoever the
  # proxy admits. Seed it like a fresh install would; a variable already
  # present — even empty — is the operator's choice.
  if ! grep -q '^AKERDOCK_TRUSTED_PROXIES=' .env; then
    note "recording AKERDOCK_TRUSTED_PROXIES=gateway in .env — caller addresses were the proxy's until now"
    printf 'AKERDOCK_TRUSTED_PROXIES=gateway\n' >> .env
  fi
  # The bootstrap variables are read only while no user exists (§6.3). Once a
  # previous boot went healthy the root user exists, so stop keeping the
  # password around in cleartext (runbook install.md §4). They form an
  # all-or-nothing trio (config validation), so all three go together.
  if [ "$WAS_HEALTHY" = 1 ] && grep -q '^AKERDOCK_ROOT_PASSWORD=..*' .env; then
    note "clearing the AKERDOCK_ROOT_* bootstrap variables from .env (consumed at first boot)"
    sed -i.bak \
      -e 's|^AKERDOCK_ROOT_EMAIL=.*|AKERDOCK_ROOT_EMAIL=|' \
      -e 's|^AKERDOCK_ROOT_NAME=.*|AKERDOCK_ROOT_NAME=|' \
      -e 's|^AKERDOCK_ROOT_PASSWORD=.*|AKERDOCK_ROOT_PASSWORD=|' .env && rm -f .env.bak
  fi
fi

## Compose override: point the reference compose at the locally built image.
## docker-compose.override.yml is the designated place for local changes
## (§4.2) — this script only owns it while it carries the marker below.
OVERRIDE=docker-compose.override.yml
MARKER="Generated by install.sh"
if [ -f "$OVERRIDE" ] && ! grep -q "$MARKER" "$OVERRIDE"; then
  die "$OVERRIDE exists but was not generated by this script — merge the 'image:' and 'build:' keys below into it, or move your customisations elsewhere and re-run"
fi
cat > "$OVERRIDE" <<EOF
# ${MARKER} — rewritten on every run, put local customisations in .env or a
# separate compose file. Builds the image from this checkout instead of
# pulling ghcr.io (source-only install).
services:
  akerdock:
    image: akerdock:\${AKERDOCK_TAG}
    build:
      context: .
      args:
        VERSION: \${AKERDOCK_TAG}
        # Bake this build's own image ref in, so scale-to-zero (ADR-036) deploys
        # the waker from the exact same locally-built image — no registry needed.
        IMAGE: akerdock:\${AKERDOCK_TAG}
    environment:
      # Override the base compose default (ghcr.io): this install builds the
      # image locally, so the waker must be deployed from the local tag.
      AKERDOCK_IMAGE: akerdock:\${AKERDOCK_TAG}
EOF

say "building akerdock:${VERSION} from sources (first build downloads the Go toolchain image — this can take a few minutes)"
docker compose build akerdock

## PostgreSQL major-version guard (ADR-039). An existing data volume from an
## older major would make the pinned postgres image crash-loop ("database files
## are incompatible") — catch it HERE, with the upgrade path, instead of after a
## confusing boot failure. Fresh installs (no volume yet) skip this untouched.
pgvol=$(docker volume ls -q --filter "name=akerdock_pgdata" | head -1)
if [ -n "$pgvol" ]; then
  data_major=$(docker run --rm -v "$pgvol":/d:ro busybox cat /d/PG_VERSION 2>/dev/null | tr -d '[:space:]' || true)
  target_major=$(sed -n 's/.*image: *postgres:\([0-9][0-9]*\).*/\1/p' docker-compose.yml | head -1)
  if [ -n "$data_major" ] && [ -n "$target_major" ] && [ "$data_major" != "$target_major" ]; then
    die "your PostgreSQL data is major $data_major but the compose pins postgres:$target_major — starting now would crash-loop. Upgrade the database in place first (backup-first, ADR-039):  ./scripts/pg-upgrade.sh   then re-run ./install.sh"
  fi
fi

say "starting the stack"
docker compose up -d

say "waiting for the control plane to become healthy"
cid=$(docker compose ps -q akerdock)
[ -n "$cid" ] || die "the akerdock container is not there after 'docker compose up' — check 'docker compose ps'"
state=starting
for _ in $(seq 1 90); do
  state=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}starting{{end}}' "$cid" 2>/dev/null || echo starting)
  [ "$state" = "healthy" ] && break
  sleep 2
done
if [ "$state" != "healthy" ]; then
  docker compose logs --tail 30 akerdock || true
  die "akerdock did not become healthy in time (state: $state) — full logs: docker compose logs akerdock"
fi

## Localhost server (instance-config §6.2): the bootstrap pre-registered this
## machine as the server "localhost", reached over SSH with the instance key.
## Authorize that key for the configured user so validation can succeed —
## idempotent, and skipped when the key is not for the user running this
## script (a custom AKERDOCK_LOCALHOST_USER is then the operator's business).
LOCALHOST_USER=$(sed -n 's/^AKERDOCK_LOCALHOST_USER=//p' .env)
if [ -n "$LOCALHOST_USER" ] && [ "$LOCALHOST_USER" = "$(id -un)" ]; then
  pubkey=$(docker compose cp akerdock:/var/lib/akerdock/ssh/instance_ed25519.pub - 2>/dev/null | tar -xO 2>/dev/null || true)
  if [ -n "$pubkey" ]; then
    mkdir -p "$HOME/.ssh" && chmod 700 "$HOME/.ssh"
    touch "$HOME/.ssh/authorized_keys" && chmod 600 "$HOME/.ssh/authorized_keys"
    if ! grep -qF "$pubkey" "$HOME/.ssh/authorized_keys"; then
      say "authorizing the instance SSH key for $(id -un) (localhost server, §6.2)"
      printf '%s\n' "$pubkey" >> "$HOME/.ssh/authorized_keys"
    fi
  else
    warn "could not read the instance public key from the container — authorize"
    warn "/var/lib/akerdock/ssh/instance_ed25519.pub manually to validate the localhost server."
  fi

  ## The engine writes the proxy layout of every server under /var/lib/akerdock,
  ## over SSH, as the server's user (deployment-engine §5.1) — on this machine
  ## that is $LOCALHOST_USER, who typically cannot create a directory at /.
  ## Prepare it here, once, instead of letting the first proxy start die on
  ## "mkdir: permission denied" (§20.1).
  if [ ! -d /var/lib/akerdock ] || [ "$(stat -c %U /var/lib/akerdock 2>/dev/null || stat -f %Su /var/lib/akerdock 2>/dev/null)" != "$LOCALHOST_USER" ]; then
    say "preparing /var/lib/akerdock for $LOCALHOST_USER (proxy and deployment layout)"
    if ! { mkdir -p /var/lib/akerdock && chown "$LOCALHOST_USER": /var/lib/akerdock; } 2>/dev/null; then
      if ! sudo sh -c "mkdir -p /var/lib/akerdock && chown '$LOCALHOST_USER': /var/lib/akerdock" 2>/dev/null; then
        warn "could not prepare /var/lib/akerdock — the first proxy start on the localhost"
        warn "server will fail until you run:  sudo mkdir -p /var/lib/akerdock && sudo chown $LOCALHOST_USER: /var/lib/akerdock"
      fi
    fi
  fi
fi

PORT=$(sed -n 's/^AKERDOCK_PORT=//p' .env)
PORT=${PORT:-8080}
say "AkerDock ${VERSION} is up"
note "dashboard & API:  http://localhost:${PORT}"
note "localhost server: pre-registered — validated automatically within a few"
note "                  minutes (requires an SSH server on this machine)."
if [ "$FIRST_INSTALL" = 1 ]; then
  ROOT_EMAIL=$(sed -n 's/^AKERDOCK_ROOT_EMAIL=//p' .env)
  note "root login:       ${ROOT_EMAIL}"
  if [ -n "$GENERATED_ROOT_PASSWORD" ]; then
    note "root password:    ${GENERATED_ROOT_PASSWORD}"
    warn "store these credentials now — the password also sits in .env until the"
    warn "next update run, then it is cleared as the runbook requires."
  fi
  warn "back up keys/master.key off this machine (see docs/runbooks/install.md §2)."
fi
note "update later with: git pull && ./install.sh"
note "logs:              docker compose logs -f akerdock"
