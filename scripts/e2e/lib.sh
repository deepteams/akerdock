#!/usr/bin/env bash
# End-to-end test of the P0 vertical slice (ADR-026: Docker-in-Docker E2E
# on every commit): boots a real PostgreSQL, a DinD target server with sshd,
# and the akerdock binary, then drives the public API through the full
# lifecycle — server validation (with Traefik bootstrap), docker_image and
# dockerfile deployments, encrypted env variables, HTTPS routing through
# the managed proxy, logs, and safe deletion.
#
# Requirements: docker, go, curl, python3, ssh-keygen, openssl.
# Usage: scripts/e2e.sh [--keep]  (--keep leaves containers running)

set -euo pipefail

# The suite runs as SHARDS: several independent copies of the whole stack (its
# own PostgreSQL, its own DinD server, its own akerdock), executing different
# sections in parallel. Nothing is shared between shards — not a port, not a
# container name, not the DinD image cache — because anything shared would make
# one shard's failure depend on another shard's timing.
SHARD=${E2E_SHARD:?E2E_SHARD must be set (smoke|deploy|build|data|platform|compose|github)}
case "$SHARD" in
  deploy)   IDX=0 ;;
  build)    IDX=1 ;;
  data)     IDX=2 ;;
  platform) IDX=3 ;;
  smoke)    IDX=4 ;;
  compose)  IDX=5 ;;
  github)   IDX=6 ;;
  *) printf 'unknown shard: %s\n' "$SHARD" >&2; exit 2 ;;
esac

PG_PORT=$((55500 + IDX * 10))
SSH_PORT=$((22300 + IDX * 10))
API_PORT=$((18100 + IDX * 10))
SINK_PORT=$((18200 + IDX * 10))
OTLP_PORT=$((14400 + IDX * 10))
BUILD_SSH_PORT=$((22400 + IDX * 10))
MAIL_SMTP_PORT=$((11025 + IDX * 10))
MAIL_HTTP_PORT=$((18025 + IDX * 10))
# MinIO is the one port that cannot be moved: akerdock signs the presigned URL
# against a host:port, and the DinD must reach the bucket at that SAME address
# or the SigV4 signature will not match. Only the shard that runs the S3 tests
# publishes it, so there is nothing to collide with.
MINIO_PORT=9000
# Pinned per release (deployment-engine §5.5) — must match internal/jobs.NixpacksVersion.
NIXPACKS_VERSION=${NIXPACKS_VERSION:-1.38.0}
WORKDIR=$(mktemp -d)
ROOT_DIR=$(cd "$(dirname "$0")/.." && pwd)
KEEP=${E2E_KEEP:-}

RUN_START=$(date +%s)

PG_CTR=akerdock-e2e-pg-$SHARD
DIND_CTR=akerdock-e2e-dind-$SHARD
BUILD_CTR=akerdock-e2e-build-$SHARD
NET_CTR=akerdock-e2e-net-$SHARD
# Fixed addresses, because a private registry served over plain HTTP is only
# usable if BOTH daemons were told to trust it — and they are told at startup,
# before anything has an address to discover.
DIND_IP=10.$((90 + IDX)).0.2
BUILD_IP=10.$((90 + IDX)).0.3
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

jsonq() { python3 -c "import json,sys; d=json.load(sys.stdin); print(eval(sys.argv[1]))" "$1"; }  # `json` is in scope for dumps()

cleanup() {
  status=$?
  [ -n "$API_PID" ] && kill -TERM "$API_PID" 2>/dev/null || true
  # A stub that outlives its shard keeps its port AND its old certificate: the
  # next run would trust a new CA and talk to the old server.
  [ -n "${GITHUB_STUB_PID:-}" ] && kill -TERM "$GITHUB_STUB_PID" 2>/dev/null || true
  [ -n "${SINK_PID:-}" ] && kill -TERM "$SINK_PID" 2>/dev/null || true
  [ -n "${OTLP_PID:-}" ] && kill -TERM "$OTLP_PID" 2>/dev/null || true
  if [ "$KEEP" != "--keep" ]; then
    docker rm -f "$PG_CTR" "$DIND_CTR" "$BUILD_CTR" >/dev/null 2>&1 || true
    docker network rm "$NET_CTR" >/dev/null 2>&1 || true
    rm -rf "$WORKDIR"
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

start_akerdock() { # start_akerdock — boots the instance with the full environment.
                   # A single definition on purpose: the suite restarts the
                   # process several times, and a restart that drops a variable
                   # silently changes what is under test.
  AKERDOCK_DATABASE_URL="postgres://postgres:test@127.0.0.1:${PG_PORT}/akerdock?sslmode=disable" \
  AKERDOCK_MASTER_KEY_FILE="$WORKDIR/master.key" \
  AKERDOCK_DATA_DIR="$WORKDIR/data" \
  AKERDOCK_PORT="$API_PORT" \
  AKERDOCK_ROOT_EMAIL="e2e@example.com" AKERDOCK_ROOT_NAME="E2E" AKERDOCK_ROOT_PASSWORD="a-very-long-password" \
  AKERDOCK_ACME_EMAIL="ops@akerdock.test" \
  AKERDOCK_SCHEDULER_TICK=2s \
  AKERDOCK_RETRY_BASE=1s \
  OTEL_EXPORTER_OTLP_ENDPOINT="http://127.0.0.1:${OTLP_PORT}" \
  OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf \
  OTEL_SERVICE_NAME=akerdock-e2e \
  OTEL_BSP_SCHEDULE_DELAY=1000 \
  OTEL_METRIC_EXPORT_INTERVAL=5000 \
  AKERDOCK_METRICS_ENABLED=true \
  AKERDOCK_LOG_FORMAT=text "$WORKDIR/akerdock" >> "$WORKDIR/api.log" 2>&1 &
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
docker rm -f "$PG_CTR" "$DIND_CTR" "$BUILD_CTR" >/dev/null 2>&1 || true
docker run -d --rm --name "$PG_CTR" -e POSTGRES_PASSWORD=test -e POSTGRES_DB=akerdock -p "${PG_PORT}:5432" postgres:17-alpine >/dev/null
# MinIO's port is published straight through the DinD, so that the SAME URL
# (http://127.0.0.1:9000) is valid from the host — where akerdock signs — and
# from inside the DinD — where curl uploads. A SigV4 signature covers the Host
# header, so two different addresses for one bucket would not work.
# The DinD keeps its image store in a named volume: without it, every run
# re-pulls nginx, postgres, traefik, minio, mc and the whole nixpacks base —
# several minutes of network for images that never change. E2E_FRESH=1 wipes it.
DIND_CACHE=${DIND_CACHE:-akerdock-e2e-images-$SHARD}
[ "${E2E_FRESH:-0}" = "1" ] && docker volume rm -f "$DIND_CACHE" >/dev/null 2>&1
docker volume create "$DIND_CACHE" >/dev/null
# Only the data shard runs MinIO, so only it publishes the port.
MINIO_PUBLISH=""
[ "$SHARD" = "data" ] && MINIO_PUBLISH=1
docker network rm "$NET_CTR" >/dev/null 2>&1 || true
docker network create --subnet "10.$((90 + IDX)).0.0/16" "$NET_CTR" >/dev/null
docker run -d --rm --privileged --name "$DIND_CTR" \
  --network "$NET_CTR" --ip "$DIND_IP" \
  -p "${SSH_PORT}:22" ${MINIO_PUBLISH:+-p "${MINIO_PORT}:9000"} \
  -v "$DIND_CACHE:/var/lib/docker" \
  -e DOCKER_TLS_CERTDIR="" docker:27-dind --insecure-registry "${DIND_IP}:5000" >/dev/null

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
  rm -rf /data/akerdock
  exit 0' || true
# `apk add` reaches the network, and four shards booting at once reach it at the
# same moment: a single attempt turns a transient mirror hiccup into a red
# build. Retried, and the actual apk error is surfaced — "sshd setup failed" on
# its own tells the reader nothing they can act on.
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


# --- webhook sink (notification target, reachable from the instance) ---------
cat > "$WORKDIR/sink.py" <<'PYSINK'
import http.server, json, sys
path = sys.argv[1]
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get('content-length', 0)))
        # One file per channel: the path tells the channels apart, so a test can
        # assert that a given channel received nothing.
        target = path if self.path != '/digest' else path.replace('hooks', 'hooks-digest')
        with open(target, 'a') as f:
            f.write(body.decode() + "\n")
        self.send_response(204)
        self.end_headers()
    def log_message(self, *a):
        pass
http.server.HTTPServer(('127.0.0.1', int(sys.argv[2])), H).serve_forever()
PYSINK
python3 "$WORKDIR/sink.py" "$WORKDIR/hooks.jsonl" "$SINK_PORT" &
SINK_PID=$!


# --- OTLP collector stub (ADR-008) -------------------------------------------
# Counts what the instance exports. It does not decode protobuf: what matters is
# that traces and metrics ARE exported to the standard OTLP endpoints, which is
# exactly what ADR-008 requires.
cat > "$WORKDIR/otlp.py" <<'PYOTLP'
import http.server, sys
counts = {"traces": 0, "metrics": 0}
path = sys.argv[1]
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        self.rfile.read(int(self.headers.get('content-length', 0)))
        for k in counts:
            if self.path.endswith("/v1/" + k):
                counts[k] += 1
        with open(path, 'w') as f:
            f.write("%d %d\n" % (counts["traces"], counts["metrics"]))
        self.send_response(200)
        self.send_header('Content-Type', 'application/x-protobuf')
        self.end_headers()
        self.wfile.write(b'')
    def log_message(self, *a):
        pass
http.server.HTTPServer(('127.0.0.1', int(sys.argv[2])), H).serve_forever()
PYOTLP
python3 "$WORKDIR/otlp.py" "$WORKDIR/otlp.count" "$OTLP_PORT" &
OTLP_PID=$!


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
docker exec "$DIND_CTR" grep -q "ops@akerdock.test" /data/akerdock/proxy/traefik.yaml 2>/dev/null || true
grep -q "telemetry enabled" "$WORKDIR/api.log" || die "OTLP telemetry was not enabled (ADR-008)"
grep -q "prometheus metrics exposed" "$WORKDIR/api.log" || die "the /metrics endpoint was not mounted (AKERDOCK_METRICS_ENABLED)"
ok "boot sequence complete (migrations, root user, instance key, OTLP enabled)"


# --- seed a root token on the team the root user belongs to --------------------
# The token is issued on the personal team that bootstrap created for the root
# user, NOT on a team of its own: the bearer API and the browser session must
# see the same data, or the auth section below would prove nothing about the
# API the dashboard actually calls.
ROOT_TOKEN="akd_$(openssl rand -hex 24)"
docker exec "$PG_CTR" psql -U postgres -d akerdock -q \
 -c "INSERT INTO api_tokens (team_id, name, token_prefix, token_hash, permissions) SELECT id, 'e2e', left('$ROOT_TOKEN',10), encode(digest('$ROOT_TOKEN','sha256'),'hex'), '{root}' FROM teams" \
 -c "UPDATE instance_settings SET api_enabled = true"
# The instance settings are cached for a few seconds (instance-config §1.1), so
# a row updated behind the API's back is not visible instantly. Wait for the
# cache to converge instead of racing it — that race passed by luck, and the
# first call to lose it would have failed with an unrelated-looking 403.
for _ in $(seq 1 10); do
  [ "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $ROOT_TOKEN" "$B/projects")" = "200" ] && break
  sleep 1
done


# --- server registration + validation (proxy bootstrap) -----------------------
say "registering and validating the server"
KEY_JSON=$(python3 -c "import json,sys; print(json.dumps(open(sys.argv[1]).read()))" "$WORKDIR/serverkey")
K=$(api POST /private-keys "{\"name\":\"e2e\",\"private_key\":$KEY_JSON}" | jsonq "d['uuid']")
S=$(api POST /servers "{\"name\":\"dind\",\"host\":\"127.0.0.1\",\"port\":${SSH_PORT},\"private_key_uuid\":\"$K\"}" | jsonq "d['uuid']")
JU=$(api POST "/servers/$S/validate" | jsonq "d['job_uuid']")
[ "$(wait_job "$JU" 180)" = "succeeded" ] || die "server validation failed"
[ "$(api GET "/servers/$S" | jsonq "d['status']")" = "ready" ] || die "server not ready"
docker exec "$DIND_CTR" docker inspect --format '{{.State.Status}}' akerdock-proxy | grep -q running || die "proxy not running"
ok "server validated, Traefik proxy bootstrapped"


# --- shared fixtures ----------------------------------------------------------
# Every shard needs a project to hang its resources off, and several need one
# deployed application to observe. They are fixtures, not tests: they assert
# nothing and count nothing — the sections that follow do that.

P=$(api POST /projects '{"name":"E2E"}')
PU=$(echo "$P" | jsonq "d['uuid']")
EU=$(echo "$P" | jsonq "d['environments'][0]['uuid']")

# base_app deploys the nginx reference application (AU) behind the proxy.
base_app() {
  AU=$(api POST /applications "{\"source_type\":\"docker_image\",\"name\":\"web\",\"project_uuid\":\"$PU\",\"environment_uuid\":\"$EU\",\"server_uuid\":\"$S\",\"docker_image\":\"nginx\",\"docker_image_tag\":\"alpine\",\"domains\":[\"web.e2e.test\"],\"ports_exposes\":\"80\"}" | jsonq "d['uuid']")
  api POST "/applications/$AU/envs" '{"key":"APP_MODE","value":"production"}' >/dev/null
  DU=$(api POST "/applications/$AU/deploy" | jsonq "d['deployment_uuid']")
  [ "$(wait_deployment "$DU" 180)" = "succeeded" ] || die "the base application did not deploy"
}
