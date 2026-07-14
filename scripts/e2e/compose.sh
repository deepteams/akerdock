# E2E shard: the compose build pack (compose-spec.md).
#
# One git repository carrying a three-service stack — a built web service, a
# postgres with a named volume and magic credentials, and a one-shot migrate
# job — deployed twice:
#
#   deploy 1  proves the whole pipeline: validation, magic variables (a
#             SERVICE_FQDN reference generates the component's domain from the
#             server wildcard), isolated network, prefixed volume, topological
#             order, one-shot exit 0, per-component routing.
#   deploy 2  proves the §8.2 semantics: the unchanged db is NOT touched (same
#             container), the web service is replaced zero-downtime (candidate
#             switched behind the proxy, new content served).

# --- server wildcard (SERVICE_FQDN needs one) ---------------------------------
say "giving the server a wildcard domain (DNS-01 credential + patch)"
DNSC=$(api POST /dns-credentials '{"name":"cf","provider":"cloudflare","config":{"CF_DNS_API_TOKEN":"tok-e2e"}}' | jsonq "d['uuid']")
SV=$(api GET "/servers/$S" | jsonq "d['version']")
curl -sf -X PATCH -H "Authorization: Bearer $ROOT_TOKEN" -H "Content-Type: application/json" \
  -H "If-Match: \"$SV\"" \
  -d "{\"wildcard_domain\":\"e2e.test\",\"dns_credential_uuid\":\"$DNSC\"}" "$B/servers/$S" >/dev/null \
  || die "server wildcard patch failed"
ok "server carries *.e2e.test"

# --- git fixture: the compose stack --------------------------------------------
say "serving a git repo with a three-service compose stack"
docker exec "$DIND_CTR" sh -c '
  apk add --no-cache git git-daemon >/dev/null 2>&1
  rm -rf /srv/crepo && mkdir -p /srv/crepo/web && cd /srv/crepo
  git init -q && git config user.email e2e@example.com && git config user.name e2e
  cat > web/Dockerfile <<EOF
FROM nginx:alpine
RUN echo compose-v1 > /usr/share/nginx/html/index.html
HEALTHCHECK --interval=2s --timeout=2s --retries=5 CMD wget -q -O /dev/null http://127.0.0.1/ || exit 1
EOF
  cat > docker-compose.yml <<EOF
services:
  web:
    build: ./web
    expose: ["80"]
    environment:
      PUBLIC_FQDN: \${SERVICE_FQDN_WEB}
      DB_PASSWORD: \${SERVICE_PASSWORD_DB}
    depends_on:
      db:
        condition: service_healthy
  db:
    image: postgres:17-alpine
    environment:
      POSTGRES_PASSWORD: \${SERVICE_PASSWORD_DB}
    volumes:
      - pgdata:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U postgres"]
      interval: 2s
      retries: 10
  migrate:
    image: postgres:17-alpine
    restart: "no"
    command: ["psql", "-h", "db", "-U", "postgres", "-c", "SELECT 1"]
    environment:
      PGPASSWORD: \${SERVICE_PASSWORD_DB}
    depends_on:
      db:
        condition: service_healthy
    x-akerdock:
      exclude_from_hc: true
volumes:
  pgdata:
EOF
  git add -A && git commit -q -m stack
  git daemon --base-path=/srv --export-all --enable=receive-pack --reuseaddr --detach /srv
' || die "compose repo setup failed"
GIT_HOST=$(docker exec "$DIND_CTR" hostname -i | awk '{print $1}')

# --- create + first deployment -------------------------------------------------
say "creating the compose application and deploying the stack"
CAPP=$(python3 - "$PU" "$EU" "$S" "$GIT_HOST" <<'PYEOF'
import json, sys
print(json.dumps({
    "source_type": "git", "name": "stack",
    "project_uuid": sys.argv[1], "environment_uuid": sys.argv[2], "server_uuid": sys.argv[3],
    "git_repository": f"git://{sys.argv[4]}/crepo", "git_branch": "master",
    "build_pack": "compose",
}))
PYEOF
)
CU=$(api POST /applications "$CAPP" | jsonq "d['uuid']")
[ "$(api GET "/applications/$CU" | jsonq "d['build_pack']")" = "compose" ] || die "build_pack not persisted"
CDU=$(api POST "/applications/$CU/deploy" | jsonq "d['deployment_uuid']")
[ "$(wait_deployment "$CDU" 300)" = "succeeded" ] || die_deployment "$CDU" "compose deployment failed"
ok "stack deployed"

# --- what the deployment must have produced ------------------------------------
say "checking components, network, volume, one-shot and magic variables"
COMPS=$(api GET "/applications/$CU/components")
[ "$(echo "$COMPS" | jsonq "len(d['data'])")" = "3" ] || die "expected 3 components, got: $COMPS"
for name in web db migrate; do
  echo "$COMPS" | jsonq "','.join(c['name'] for c in d['data'])" | grep -q "$name" || die "component $name missing"
done
[ "$(echo "$COMPS" | jsonq "[c for c in d['data'] if c['name']=='db'][0]['is_database']")" = "True" ] || die "db not detected as a database"
[ "$(echo "$COMPS" | jsonq "[c for c in d['data'] if c['name']=='db'][0]['database_engine']")" = "postgresql" ] || die "wrong engine"
[ "$(echo "$COMPS" | jsonq "[c for c in d['data'] if c['name']=='migrate'][0]['observed_status']")" = "exited" ] || die "one-shot not exited"
[ "$(echo "$COMPS" | jsonq "[c for c in d['data'] if c['name']=='web'][0]['observed_status']")" = "healthy" ] || die "web not healthy"

docker exec "$DIND_CTR" docker network inspect "$CU" >/dev/null 2>&1 || die "isolated stack network missing"
docker exec "$DIND_CTR" docker volume inspect "${CU}_pgdata" >/dev/null 2>&1 || die "prefixed volume missing"
docker exec "$DIND_CTR" docker inspect --format '{{.State.Status}}' "$CU-db" | grep -q running || die "db container not running"
# The one-shot ran at its topological position and exited 0 (§7.3).
[ "$(docker exec "$DIND_CTR" docker inspect --format '{{.State.ExitCode}}' "$CU-migrate")" = "0" ] || die "migrate did not exit 0"

# The magic password was generated once, stored is_generated and secret (§4.3).
GEN=$(docker exec "$PG_CTR" psql -U postgres -d akerdock -tAc \
  "SELECT count(*) FROM environment_variables ev JOIN resources r ON r.id = ev.resource_id WHERE r.uuid = '$CU' AND ev.key = 'SERVICE_PASSWORD_DB' AND ev.is_generated AND ev.is_secret")
[ "$GEN" = "1" ] || die "SERVICE_PASSWORD_DB not persisted as a generated secret"

# SERVICE_FQDN_WEB was a declaration of intent: the domain exists and routes to
# the web component only (§6).
WEBFQDN=$(docker exec "$PG_CTR" psql -U postgres -d akerdock -tAc \
  "SELECT dom.fqdn FROM domains dom JOIN service_components sc ON sc.id = dom.service_component_id WHERE sc.name = 'web'" | tr -d ' ')
[ -n "$WEBFQDN" ] || die "SERVICE_FQDN_WEB did not generate a domain"
wait_route "$WEBFQDN" 301
docker exec "$DIND_CTR" curl -sk --resolve "$WEBFQDN:443:127.0.0.1" "https://$WEBFQDN/" | grep -q 'compose-v1' || die "web component not serving through the proxy"
ok "components synced, one-shot exit 0, magic secret persisted, $WEBFQDN routed to the web component"

# --- second deployment: diff + zero-downtime (§8.2) -----------------------------
say "redeploying with a web change: db untouched, web switched zero-downtime"
DB_CTR_ID=$(docker exec "$DIND_CTR" docker inspect --format '{{.Id}}' "$CU-db")
docker exec "$DIND_CTR" sh -c '
  cd /srv/crepo && sed -i s/compose-v1/compose-v2/ web/Dockerfile
  git add -A && git commit -q -m v2
' || die "fixture update failed"
CDU2=$(api POST "/applications/$CU/deploy" | jsonq "d['deployment_uuid']")
[ "$(wait_deployment "$CDU2" 300)" = "succeeded" ] || die_deployment "$CDU2" "second compose deployment failed"

# The unchanged db was not replaced: same container, same identity (§8.2 step 1).
[ "$(docker exec "$DIND_CTR" docker inspect --format '{{.Id}}' "$CU-db")" = "$DB_CTR_ID" ] || die "unchanged db was replaced"
# The web service was switched: the new content serves, under the final name,
# and no candidate is left behind.
docker exec "$DIND_CTR" curl -sk --resolve "$WEBFQDN:443:127.0.0.1" "https://$WEBFQDN/" | grep -q 'compose-v2' || die "new web content not serving"
docker exec "$DIND_CTR" docker ps -a --format '{{.Names}}' | grep -q -- "$CU-web-next" && die "candidate left behind after the switch"
# The switch left the deployment a per-service story: the zero-downtime steps
# of web must be there, and db must have been SKIPPED as unchanged.
STEPS=$(docker exec "$PG_CTR" psql -U postgres -d akerdock -tAc \
  "SELECT string_agg(ds.name || ':' || ds.status, ' ' ORDER BY ds.seq) FROM deployment_steps ds JOIN deployments dep ON dep.id = ds.deployment_id WHERE dep.uuid = '$CDU2'")
echo "$STEPS" | grep -q "start_candidate_web:succeeded" || die "web was not replaced via a candidate: $STEPS"
echo "$STEPS" | grep -q "switch_web:succeeded" || die "web switch step missing: $STEPS"
echo "$STEPS" | grep -q "start_db:skipped" || die "unchanged db was not skipped: $STEPS"
ok "db untouched, web replaced zero-downtime behind the proxy"

# --- inline stacks: /services (phase B) -----------------------------------------
say "inline stack: refused at save when invalid, deployed, cycled, deleted"
# A file breaking the subset is refused where it is written, with its stable
# code (compose-spec §11) — not discovered at deployment time.
BADBODY=$(python3 - "$PU" "$EU" "$S" <<'PYEOF'
import json, sys
print(json.dumps({
    "name": "bad", "project_uuid": sys.argv[1], "environment_uuid": sys.argv[2], "server_uuid": sys.argv[3],
    "compose_content": "services:\n  app:\n    image: nginx\n    network_mode: host\n",
}))
PYEOF
)
BAD=$(curl -s -X POST -H "Authorization: Bearer $ROOT_TOKEN" -H "Content-Type: application/json" -d "$BADBODY" "$B/services")
echo "$BAD" | grep -q "compose_network_mode_host_rejected" || die "invalid inline stack not refused with its code: $BAD"

# build: has no source to build from in an inline stack.
BUILDBODY=$(python3 - "$PU" "$EU" "$S" <<'PYEOF'
import json, sys
print(json.dumps({
    "name": "buildy", "project_uuid": sys.argv[1], "environment_uuid": sys.argv[2], "server_uuid": sys.argv[3],
    "compose_content": "services:\n  app:\n    build: .\n",
}))
PYEOF
)
BUILDREF=$(curl -s -X POST -H "Authorization: Bearer $ROOT_TOKEN" -H "Content-Type: application/json" -d "$BUILDBODY" "$B/services")
echo "$BUILDREF" | grep -q "compose_build_unsupported" || die "build in an inline stack must be refused: $BUILDREF"

INLINE=$(python3 - "$PU" "$EU" "$S" <<'PYEOF'
import json, sys
compose = """
services:
  app:
    image: nginx:alpine
    expose: ["80"]
    environment:
      PUBLIC_URL: ${SERVICE_URL_APP}
  cache:
    image: redis:7-alpine
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 2s
"""
print(json.dumps({
    "name": "inline", "project_uuid": sys.argv[1], "environment_uuid": sys.argv[2], "server_uuid": sys.argv[3],
    "compose_content": compose,
}))
PYEOF
)
SVCU=$(api POST /services "$INLINE" | jsonq "d['uuid']")
[ "$(api GET "/services/$SVCU" | jsonq "d['observed_status']")" = "unknown" ] || die "fresh stack must be unknown"
SDU=$(api POST "/services/$SVCU/deploy" | jsonq "d['deployment_uuid']")
[ "$(wait_deployment "$SDU" 300)" = "succeeded" ] || die_deployment "$SDU" "inline stack deployment failed"

SCOMPS=$(api GET "/services/$SVCU/components")
[ "$(echo "$SCOMPS" | jsonq "len(d['data'])")" = "2" ] || die "expected 2 components: $SCOMPS"
docker exec "$DIND_CTR" docker inspect --format '{{.State.Status}}' "$SVCU-app" | grep -q running || die "inline app not running"
# The SERVICE_URL magic generated the app's domain from the wildcard and routed it.
APPFQDN=$(docker exec "$PG_CTR" psql -U postgres -d akerdock -tAc \
  "SELECT dom.fqdn FROM domains dom JOIN service_components sc ON sc.id = dom.service_component_id JOIN resources r ON r.id = sc.resource_id WHERE r.uuid = '$SVCU'" | tr -d ' ')
[ -n "$APPFQDN" ] || die "SERVICE_URL_APP did not generate a domain"
wait_route "$APPFQDN" 301

# Lifecycle: stop stops every container of the stack, start brings them back.
[ "$(wait_job "$(api POST "/services/$SVCU/stop" | jsonq "d['job_uuid']")" 120)" = "succeeded" ] || die "stack stop failed"
docker exec "$DIND_CTR" docker inspect --format '{{.State.Status}}' "$SVCU-app" | grep -q running && die "app still running after stop"
docker exec "$DIND_CTR" docker inspect --format '{{.State.Status}}' "$SVCU-cache" | grep -q running && die "cache still running after stop"
[ "$(wait_job "$(api POST "/services/$SVCU/start" | jsonq "d['job_uuid']")" 120)" = "succeeded" ] || die "stack start failed"
docker exec "$DIND_CTR" docker inspect --format '{{.State.Status}}' "$SVCU-app" | grep -q running || die "app not running after start"

# Deletion removes every container of the stack AND its network.
[ "$(wait_job "$(api DELETE "/services/$SVCU" | jsonq "d['job_uuid']")" 120)" = "succeeded" ] || die "stack deletion failed"
docker exec "$DIND_CTR" docker inspect "$SVCU-app" >/dev/null 2>&1 && die "app container survived deletion"
docker exec "$DIND_CTR" docker inspect "$SVCU-cache" >/dev/null 2>&1 && die "cache container survived deletion"
docker exec "$DIND_CTR" docker network inspect "$SVCU" >/dev/null 2>&1 && die "stack network survived deletion"
ok "inline stack: 422 with stable codes, deployed, routed, stopped/started, deleted with its network"
