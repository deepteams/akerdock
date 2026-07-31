#!/usr/bin/env bash
# Shared setup for the single complete product journey (ADR-028): boots a real
# PostgreSQL, a DinD target server with sshd and the AkerDock binary.
#
# Requirements: docker, go, curl, python3, ssh-keygen, openssl.
# Usage: E2E_KEEP=--keep scripts/e2e.sh  (leaves containers running)

set -euo pipefail

SHARD=${E2E_SHARD:-complete}
[ "$SHARD" = "complete" ] || {
  printf 'unknown E2E journey: %s\n' "$SHARD" >&2
  exit 2
}
IDX=0

PG_PORT=$((55500 + IDX * 10))
SSH_PORT=$((22300 + IDX * 10))
API_PORT=$((18100 + IDX * 10))
WORKDIR=$(mktemp -d)
ROOT_DIR=$(cd "$(dirname "$0")/.." && pwd)
KEEP=${E2E_KEEP:-}

RUN_START=$(date +%s)

PG_CTR=akerdock-e2e-pg-$SHARD
DIND_CTR=akerdock-e2e-dind-$SHARD
NET_CTR=akerdock-e2e-net-$SHARD
API_PID=""

B="http://127.0.0.1:${API_PORT}/api/v1"
PASS=0
FAIL=0

# Timing: each section reports how long the PREVIOUS one took, so a slow run
# points at its own cause instead of needing a bisect.
SECTION_START=$(date +%s)
SECTION_NAME=""
say()  {
  local now; now=$(date +%s)
  if [ -n "$SECTION_NAME" ]; then
    printf '\033[2m   (%s: %ds)\033[0m\n' "$SECTION_NAME" "$((now - SECTION_START))"
  fi
  SECTION_NAME="$*"; SECTION_START=$now
  printf '\033[1;34m== [%s] %s\033[0m\n' "$SHARD" "$*"
}
ok()   { PASS=$((PASS+1)); printf '   \033[32m✔\033[0m [%s] %s\n' "$SHARD" "$*"; }
# stderr, not stdout: die is called from inside `$(api ... | jsonq ...)`, and a
# failure message printed on stdout would be swallowed by the pipe and reported
# as a Python parse error instead of the actual HTTP failure.
die()  { printf '   \033[31m✘ [%s] %s\033[0m\n' "$SHARD" "$*" >&2; FAIL=$((FAIL+1)); exit 1; }
# die_deployment DEPLOYMENT_UUID MESSAGE — a failed deployment says WHY. Without
# this, the harness reports "the deployment failed" and the reader has to
# reproduce it by hand to learn anything.
die_deployment() {
  printf '   \033[31m--- deployment %s ---\033[0m\n' "$1" >&2
  api GET "/deployments/$1" >&2 2>/dev/null || true
  printf '\n   \033[31m--- steps ---\033[0m\n' >&2
  # The TAIL of the log: the head is boilerplate, the end is the failure.
  api GET "/deployments/$1/logs?limit=100" 2>/dev/null \
    | python3 -c "import json,sys; [print('  ', l['channel'], l['message'][:300]) for l in json.load(sys.stdin)['data'][-30:]]" >&2 || true
  die "$2"
}
die_job() {
  printf '   \033[31m--- job %s ---\033[0m\n' "$1" >&2
  api GET "/jobs/$1" >&2 2>/dev/null || true
  printf '\n' >&2
  die "$2"
}

jsonq() { python3 -c "import json,sys; d=json.load(sys.stdin); print(eval(sys.argv[1]))" "$1"; }  # `json` is in scope for dumps()

cleanup() {
  status=$?
  if [ -n "$API_PID" ]; then
    kill -TERM "$API_PID" 2>/dev/null || true
    wait "$API_PID" 2>/dev/null || true
  fi
  if [ "$KEEP" != "--keep" ]; then
    docker rm -f "$PG_CTR" "$DIND_CTR" >/dev/null 2>&1 || true
    docker network rm "$NET_CTR" >/dev/null 2>&1 || true
    if [ $status -eq 0 ] && [ $FAIL -eq 0 ]; then
      rm -rf "$WORKDIR"
    fi
  fi
  if [ -n "$SECTION_NAME" ]; then
    printf '\033[2m   (%s: %ds)\033[0m\n' "$SECTION_NAME" "$(($(date +%s) - SECTION_START))"
  fi
  printf '\033[2m   total: %ds\033[0m\n' "$(($(date +%s) - RUN_START))"
  if [ $status -eq 0 ] && [ $FAIL -eq 0 ]; then
    printf '\n\033[1;32m[%s] E2E OK — %d checks passed\033[0m\n' "$SHARD" "$PASS"
  else
    printf '\n\033[1;31m[%s] E2E FAILED (see above) — logs: %s/api.log\033[0m\n' "$SHARD" "$WORKDIR"
    [ -f "$WORKDIR/api.log" ] && tail -20 "$WORKDIR/api.log" || true
  fi
}
trap cleanup EXIT

api() { # api METHOD PATH [JSON_BODY] — authenticated call; a non-2xx status
        # aborts with the response body, instead of dying silently under set -e
  local method=$1 path=$2 body=${3:-} out code
  if [ -n "$body" ]; then
    out=$(curl -s -w '\n%{http_code}' -X "$method" -H "Authorization: Bearer $ROOT_TOKEN" -H 'Content-Type: application/json' -d "$body" "$B$path")
  else
    out=$(curl -s -w '\n%{http_code}' -X "$method" -H "Authorization: Bearer $ROOT_TOKEN" "$B$path")
  fi
  code=${out##*$'\n'}
  out=${out%$'\n'*}
  case "$code" in
    2*) printf '%s' "$out" ;;
    *)  die "$method $path → HTTP $code: $out" ;;
  esac
}

wait_job() { # wait_job JOB_UUID [TIMEOUT_S] — echoes final status
  local ju=$1 timeout=${2:-120} st=""
  for _ in $(seq 1 "$timeout"); do
    st=$(api GET "/jobs/$ju" | jsonq "d['status']")
    case "$st" in succeeded|dead_letter|cancelled) break;; esac
    sleep 1
  done
  echo "$st"
}

wait_deployment() {
  local du=$1 timeout=${2:-120} st=""
  for _ in $(seq 1 "$timeout"); do
    st=$(api GET "/deployments/$du" | jsonq "d['status']")
    case "$st" in succeeded|failed|cancelled) break;; esac
    sleep 1
  done
  echo "$st"
}

wait_route() { # wait_route HOST EXPECTED_CODE — polls the proxy until the
               # file watcher has loaded the route (up to 30 s)
  local host=$1 want=$2 got=""
  for _ in $(seq 1 30); do
    got=$(docker exec "$DIND_CTR" curl -s -o /dev/null -w '%{http_code}' -H "Host: $host" http://127.0.0.1:80/ || true)
    [ "$got" = "$want" ] && return 0
    sleep 1
  done
  die "route $host: expected HTTP $want, got $got"
}

start_akerdock() { # Keep the complete boot environment in one readable place.
  AKERDOCK_DATABASE_URL="postgres://postgres:test@127.0.0.1:${PG_PORT}/akerdock?sslmode=disable" \
  AKERDOCK_MASTER_KEY_FILE="$WORKDIR/master.key" \
  AKERDOCK_DATA_DIR="$WORKDIR/data" \
  AKERDOCK_PORT="$API_PORT" \
  AKERDOCK_ROOT_EMAIL="e2e@example.com" AKERDOCK_ROOT_NAME="E2E" AKERDOCK_ROOT_PASSWORD="a-very-long-password" \
  AKERDOCK_ACME_EMAIL="ops@akerdock.test" \
  AKERDOCK_SCHEDULER_TICK=2s \
  AKERDOCK_RETRY_BASE=1s \
  AKERDOCK_TERMINAL_IDLE_TIMEOUT=8s \
  AKERDOCK_TERMINAL_MAX_DURATION=2m \
  AKERDOCK_IMAGE=akerdock:e2e \
  AKERDOCK_INSTANCE_URL="$INSTANCE_URL" \
  AKERDOCK_LOG_FORMAT=text "$WORKDIR/akerdock" serve all-in-one >> "$WORKDIR/api.log" 2>&1 &
  API_PID=$!
  for _ in $(seq 1 30); do curl -sf "$B/health" >/dev/null 2>&1 && break; sleep 1; done
  curl -sf "$B/health" >/dev/null || die "akerdock did not become healthy"
}

# Two suites running at once fight over the API port and the container names,
# and the loser fails somewhere far from the cause (an earlier instance answers
# /health, so the boot checks pass against the WRONG process). Refuse to start.
if lsof -nP -iTCP:"$API_PORT" -sTCP:LISTEN >/dev/null 2>&1; then
  printf '\033[31m✘ port %s is already in use — another e2e run (or a stray akerdock) is live\033[0m\n' "$API_PORT" >&2
  exit 1
fi

# --- infrastructure ----------------------------------------------------------
say "starting PostgreSQL and the DinD target server"
docker rm -f "$PG_CTR" "$DIND_CTR" >/dev/null 2>&1 || true
docker run -d --rm --name "$PG_CTR" -e POSTGRES_PASSWORD=test -e POSTGRES_DB=akerdock -p "${PG_PORT}:5432" postgres:18-alpine >/dev/null
# The DinD keeps its image store in a named volume: without it, every run
# re-pulls nginx, PostgreSQL and Traefik. E2E_FRESH=1 wipes it.
DIND_CACHE=${DIND_CACHE:-akerdock-e2e-images-$SHARD}
[ "${E2E_FRESH:-0}" = "1" ] && docker volume rm -f "$DIND_CACHE" >/dev/null 2>&1
docker volume create "$DIND_CACHE" >/dev/null
docker network rm "$NET_CTR" >/dev/null 2>&1 || true
docker network create --subnet "10.$((90 + IDX)).0.0/16" "$NET_CTR" >/dev/null
docker run -d --rm --privileged --name "$DIND_CTR" \
  --network "$NET_CTR" \
  -p "${SSH_PORT}:22" \
  -v "$DIND_CACHE:/var/lib/docker" \
  -e DOCKER_TLS_CERTDIR="" docker:27-dind >/dev/null

ssh-keygen -t ed25519 -N '' -C e2e -f "$WORKDIR/serverkey" -q
python3 -c "import base64,os; print('1:'+base64.b64encode(os.urandom(32)).decode())" > "$WORKDIR/master.key"
chmod 600 "$WORKDIR/master.key"

for _ in $(seq 1 60); do docker exec "$DIND_CTR" docker info >/dev/null 2>&1 && break; sleep 2; done
docker exec "$DIND_CTR" docker info >/dev/null 2>&1 || die "dockerd did not start in DinD"

# The cache volume holds the whole docker root — images AND the containers,
# networks and volumes of the previous run. Keep the images (that is the point),
# throw the rest away: a leftover proxy or app container would make this run
# start from someone else's state.
docker exec "$DIND_CTR" sh -c '
  docker rm -f $(docker ps -aq) >/dev/null 2>&1
  docker volume prune -f >/dev/null 2>&1
  docker network prune -f >/dev/null 2>&1
  rm -rf /var/lib/akerdock
  exit 0' || true
# `apk add` reaches the network. Retry transient mirror errors and surface the
# actual failure rather than a generic "sshd setup failed".
SSHD_ERR=""
for attempt in 1 2 3; do
  if SSHD_ERR=$(docker exec "$DIND_CTR" sh -c "
      apk upgrade --no-cache >/dev/null 2>&1
      apk add --no-cache openssh-server curl
      ssh-keygen -A >/dev/null 2>&1
      mkdir -p /root/.ssh && echo '$(cat "$WORKDIR/serverkey.pub")' > /root/.ssh/authorized_keys
      /usr/sbin/sshd" 2>&1); then
    SSHD_ERR=""
    break
  fi
  sleep $((attempt * 3))
done
[ -z "$SSHD_ERR" ] || die "sshd setup failed in DinD after 3 attempts: $(printf '%s' "$SSHD_ERR" | tail -2 | tr '\n' ' ')"
# Keep the original host keys: the pinning test swaps them out and back.
docker exec "$DIND_CTR" tar -czf - -C / etc/ssh 2>/dev/null > "$WORKDIR/hostkeys.tgz"
ok "target server ready (DinD + sshd)"


# --- agent image -------------------------------------------------------------
# Every Docker operation rides the agent channel (ADR-051), and server
# validation refuses to succeed without a provisioned agent (ADR-054): the
# DinD needs an AkerDock image to run it from. A minimal one is built INSIDE
# the DinD from the same source tree — FROM scratch, so nothing is pulled.
say "building the agent image inside the DinD"
DIND_ARCH=$(docker exec "$DIND_CTR" docker version --format '{{.Server.Arch}}')
mkdir -p "$WORKDIR/agent-image"
CGO_ENABLED=0 GOOS=linux GOARCH="$DIND_ARCH" go build -o "$WORKDIR/agent-image/akerdock" ./cmd/akerdock
cat > "$WORKDIR/agent-image/Dockerfile" <<'DOCKERFILE'
FROM scratch
COPY akerdock /akerdock
ENTRYPOINT ["/akerdock"]
DOCKERFILE
tar -C "$WORKDIR/agent-image" -cf - . | docker exec -i "$DIND_CTR" docker build -q -t akerdock:e2e - >/dev/null
ok "agent image ready (akerdock:e2e)"

# The agent dials the control plane BACK from a container nested inside the
# DinD: the reachable address there is the DinD's own default gateway — the
# runner's docker bridge, where the akerdock binary listens. Overridable for
# environments with a VM in between (Docker Desktop): E2E_INSTANCE_URL.
DIND_GW=$(docker exec "$DIND_CTR" sh -c "ip route | awk '/default/ {print \$3; exit}'")
INSTANCE_URL=${E2E_INSTANCE_URL:-http://$DIND_GW:$API_PORT}

# --- akerdock boot -----------------------------------------------------------
say "building and booting akerdock"
go build -o "$WORKDIR/akerdock" ./cmd/akerdock
start_akerdock
grep -q "migrations applied" "$WORKDIR/api.log" || die "migrations marker missing"
grep -q "root user created" "$WORKDIR/api.log" || die "root bootstrap missing"
grep -q "instance ssh key generated" "$WORKDIR/api.log" || die "instance key missing"
# The ACME contact is an explicit setting, never a guess: a wrong address makes
# certificate issuance fail silently (§4.3).
ACME=$(docker exec "$PG_CTR" psql -U postgres -d akerdock -tAc "SELECT acme_email FROM instance_settings" | tr -d ' ')
[ "$ACME" = "ops@akerdock.test" ] || die "the ACME contact was not seeded (got '$ACME')"
ok "boot sequence complete (migrations, root user, instance key)"


# --- seed a root token on the team the root user belongs to --------------------
# The token is issued on the personal team that bootstrap created for the root
# user, NOT on a team of its own: the bearer API and the browser session must
# see the same data, or the auth section below would prove nothing about the
# API the dashboard actually calls.
ROOT_TOKEN="akd_$(openssl rand -hex 24)"
docker exec "$PG_CTR" psql -U postgres -d akerdock -q \
 -c "INSERT INTO api_tokens (team_id, created_by, name, token_prefix, token_hash, permissions) SELECT t.id, u.id, 'e2e', left('$ROOT_TOKEN',10), encode(digest('$ROOT_TOKEN','sha256'),'hex'), '{root}' FROM teams t CROSS JOIN users u WHERE u.is_root" \
 -c "UPDATE instance_settings SET api_enabled = true"
# The instance settings are cached for a few seconds (instance-config §1.1), so
# a row updated behind the API's back is not visible instantly. Wait for the
# cache to converge instead of racing it — that race passed by luck, and the
# first call to lose it would have failed with an unrelated-looking 403.
for _ in $(seq 1 10); do
  [ "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $ROOT_TOKEN" "$B/projects")" = "200" ] && break
  sleep 1
done


# --- server registration, validation and explicit proxy start -----------------
say "registering and validating the server"
KEY_JSON=$(python3 -c "import json,sys; print(json.dumps(open(sys.argv[1]).read()))" "$WORKDIR/serverkey")
K=$(api POST /private-keys "{\"name\":\"e2e\",\"private_key\":$KEY_JSON}" | jsonq "d['uuid']")
S=$(api POST /servers "{\"name\":\"dind\",\"host\":\"127.0.0.1\",\"port\":${SSH_PORT},\"private_key_uuid\":\"$K\"}" | jsonq "d['uuid']")
JU=$(api POST "/servers/$S/validate" | jsonq "d['job_uuid']")
[ "$(wait_job "$JU" 180)" = "succeeded" ] || die_job "$JU" "server validation failed"
[ "$(api GET "/servers/$S" | jsonq "d['status']")" = "ready" ] || die "server not ready"
[ "$(api GET "/servers/$S" | jsonq "d['proxy_desired_state']")" = "stopped" ] ||
  die "a fresh server must keep its proxy stopped until the operator starts it"
PJU=$(api POST "/servers/$S/proxy/start" | jsonq "d['job_uuid']")
[ "$(wait_job "$PJU" 180)" = "succeeded" ] || die_job "$PJU" "explicit proxy start failed"
docker exec "$DIND_CTR" docker inspect --format '{{.State.Status}}' akerdock-proxy | grep -q running || die "proxy not running"
docker exec "$DIND_CTR" grep -q "ops@akerdock.test" /var/lib/akerdock/proxy/traefik.yaml ||
  die "the explicit ACME contact is missing from Traefik configuration"
ok "server validated, explicit Traefik start bootstrapped the proxy"


# --- journey fixtures ---------------------------------------------------------

P=$(api POST /projects '{"name":"E2E"}')
PU=$(echo "$P" | jsonq "d['uuid']")
EU=$(echo "$P" | jsonq "d['environments'][0]['uuid']")
