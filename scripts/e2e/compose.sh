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

# --- backups of a stack's internal database (compose-spec §10) -------------------
# E2E-COMPOSE-BK-01..04. The db component was classified postgresql by image
# detection: it is a valid backup-plan target. The dump runs inside ITS
# container, credentials from its environment, never in a log (INV-003).
say "backing up the stack's internal database, then restoring lost data into it"
DBCOMP=$(echo "$COMPS" | jsonq "[c['uuid'] for c in d['data'] if c['name']=='db'][0]")
WEBCOMP=$(echo "$COMPS" | jsonq "[c['uuid'] for c in d['data'] if c['name']=='web'][0]")
# A non-database component is refused with the reason — never accepted and
# then failing at the first backup.
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $ROOT_TOKEN" \
  -H 'Content-Type: application/json' -d '{"frequency":"daily"}' "$B/service-components/$WEBCOMP/backups")
[ "$CODE" = "422" ] || die "a plan on a non-database component must be refused (got $CODE)"

# Seed real data inside the component, through its own container.
docker exec "$DIND_CTR" docker exec "$CU-db" \
  psql -U postgres -c "CREATE TABLE stock (v text); INSERT INTO stock VALUES ('precious')" >/dev/null || die "could not seed the internal database"

CPLAN=$(api POST "/service-components/$DBCOMP/backups" '{"frequency":"daily","drill_enabled":true}' | jsonq "d['uuid']")
CEJ=$(api POST "/service-components/$DBCOMP/backups/$CPLAN/execute")
[ "$(wait_job "$(echo "$CEJ" | jsonq "d['job_uuid']")" 180)" = "succeeded" ] || die "component backup failed"
CEXEC=$(api GET "/service-components/$DBCOMP/backups/$CPLAN/executions" | jsonq "json.dumps(d['data'][0])")
echo "$CEXEC" | jsonq "d['status']" | grep -q succeeded || die "component backup not succeeded: $CEXEC"
CEXEC_UUID=$(echo "$CEXEC" | jsonq "d['uuid']")

# Destroy the data, then restore it — into the SAME component container.
docker exec "$DIND_CTR" docker exec "$CU-db" psql -U postgres -c "DROP TABLE stock" >/dev/null || die "could not drop the table"
[ "$(wait_job "$(api POST "/service-components/$DBCOMP/backups/$CPLAN/executions/$CEXEC_UUID/restore" '{"confirm":true}' | jsonq "d['job_uuid']")" 180)" = "succeeded" ] || die "component restore failed"
docker exec "$DIND_CTR" docker exec "$CU-db" psql -U postgres -tAc "SELECT v FROM stock" | grep -q precious \
  || die "the component restore did not bring the data back"

# The drill fires from the SCHEDULER (drill_enabled + never drilled = due
# immediately): waiting for it proves component plans are drillable without
# any human — the same guarantee the managed databases have (ADR-014). It
# restores into a disposable instance with the component's role, recounts,
# and destroys it; production is never touched.
CDRILL="{}"
for _ in $(seq 1 180); do
  CDRILL=$(api GET "/service-components/$DBCOMP/backups/$CPLAN/drills" \
    | jsonq "json.dumps(d['data'][0]) if d['data'] else '{}'")
  case "$CDRILL" in *'"succeeded"'*|*'"failed"'*) break;; esac
  sleep 1
done
echo "$CDRILL" | jsonq "d['status']" | grep -q succeeded || die "the scheduled component drill did not succeed: $CDRILL"
[ "$(echo "$CDRILL" | jsonq "d['tables_restored']")" -ge 1 ] || die "component drill restored nothing"
docker exec "$DIND_CTR" docker ps -a --format '{{.Names}}' | grep -q akerdock-drill && die "the drill container was left behind"
ok "internal database backed up, restored in place, and drilled by the scheduler in a disposable copy (§10, ADR-014)"

# --- stack hooks (§10, x-akerdock) ----------------------------------------------
# E2E-COMPOSE-HK-01/02. pre runs in the EXISTING container before anything is
# mutated; post runs in the HEALTHY CANDIDATE before its switch. Order is
# proven by the step sequence, the guarantee by the failing-post case below.
say "stack hooks: pre before any mutation, post in the candidate before the switch"
docker exec "$DIND_CTR" sh -c '
  cd /srv/crepo && sed -i s/compose-v2/compose-v3/ web/Dockerfile
  sed -i "s|^    build: ./web\$|    build: ./web\n    x-akerdock:\n      pre_deployment_command: \"echo pre-ok\"\n      post_deployment_command: \"wget -q -O /dev/null http://127.0.0.1/\"|" docker-compose.yml
  grep -q x-akerdock docker-compose.yml || exit 1
  git add -A && git commit -q -m v3-hooks
' || die "hook fixture update failed"
CDU3=$(api POST "/applications/$CU/deploy" | jsonq "d['deployment_uuid']")
[ "$(wait_deployment "$CDU3" 300)" = "succeeded" ] || die_deployment "$CDU3" "the hooked deployment failed"
HSTEPS=$(docker exec "$PG_CTR" psql -U postgres -d akerdock -tAc \
  "SELECT string_agg(ds.name || ':' || ds.status, ' ' ORDER BY ds.seq) FROM deployment_steps ds JOIN deployments dep ON dep.id = ds.deployment_id WHERE dep.uuid = '$CDU3'")
echo "$HSTEPS" | grep -q "pre_deployment_web:succeeded" || die "pre hook did not run: $HSTEPS"
echo "$HSTEPS" | grep -q "post_deployment_web:succeeded" || die "post hook did not run: $HSTEPS"
# pre before the candidate exists; post after the candidate, before the switch.
echo "$HSTEPS" | grep -qE "pre_deployment_web:succeeded.*start_candidate_web:succeeded.*post_deployment_web:succeeded.*switch_web:succeeded" \
  || die "hook ordering violated (§10): $HSTEPS"

# --- crash resume by per-service inspection (§2.5, compose-spec §8.2) ------------
# E2E-COMPOSE-CR-01. Reproduce the aftermath of a worker dying mid-switch of
# the web service: the candidate exists and is healthy, the stable name is
# gone, the promotion never happened. The resume must FINISH it — never
# replay the whole switch, never fail the deployment (INV-004/005).
say "a compose deployment that crashed mid-switch is resumed by per-service inspection"
docker exec "$DIND_CTR" docker rename "$CU-web" "$CU-web-next" || die "could not stage the compose crash scenario"
CR3=$(docker exec "$PG_CTR" psql -U postgres -d akerdock -tAc \
  "SELECT uuid FROM deployments WHERE resource_id = (SELECT id FROM resources WHERE uuid = '$CU') ORDER BY id DESC LIMIT 1" | tr -d ' ')
docker exec "$PG_CTR" psql -U postgres -d akerdock -q \
  -c "UPDATE deployments SET status = 'starting' WHERE uuid = '$CR3'" \
  -c "UPDATE jobs SET status = 'leased', attempt = 1, lease_expires_at = now() - interval '5 minutes',
      leased_by = 'dead-worker'
      WHERE job_type = 'deployment.run' AND payload->>'deployment_id' =
        (SELECT id::text FROM deployments WHERE uuid = '$CR3')"
for _ in $(seq 90); do
  ST=$(api GET "/deployments/$CR3" | jsonq "d['status']")
  [ "$ST" = "succeeded" ] && break
  [ "$ST" = "failed" ] && die "the compose resume failed instead of finishing the switch: $(api GET "/deployments/$CR3/logs?limit=100" | tr -d '\n' | tail -c 400)"
  sleep 2
done
[ "$ST" = "succeeded" ] || die "the crashed compose deployment was not resumed (status: $ST)"
CR3STEPS=$(docker exec "$PG_CTR" psql -U postgres -d akerdock -tAc \
  "SELECT string_agg(ds.name || ':' || ds.status, ' ' ORDER BY ds.seq) FROM deployment_steps ds JOIN deployments dep ON dep.id = ds.deployment_id WHERE dep.uuid = '$CR3'")
echo "$CR3STEPS" | grep -q "resume_web" || die "the resume did not record its per-service inspection: $CR3STEPS"
[ "$(docker exec "$DIND_CTR" docker ps -q --filter "name=^${CU}-web$" | wc -l | tr -d ' ')" = "1" ] || die "the promoted web container is missing after the resume"
docker exec "$DIND_CTR" docker inspect "$CU-web-next" >/dev/null 2>&1 && die "a candidate survived the compose resume"
docker exec "$DIND_CTR" curl -sk --resolve "$WEBFQDN:443:127.0.0.1" "https://$WEBFQDN/" | grep -q 'compose-v3' || die "web not serving after the resume"
ok "crashed compose switch inspected per service and finished, no double switch (INV-004)"

# --- failing post hook: the old container keeps serving (§10, INV-005) -----------
# E2E-COMPOSE-HK-03. The whole reason the post hook exists: its failure must
# leave the OLD container routed and remove the candidate — never a switch.
say "a failing post hook fails the deployment without switching the service"
docker exec "$DIND_CTR" sh -c '
  cd /srv/crepo && sed -i s/compose-v3/compose-v4/ web/Dockerfile
  sed -i "s|wget -q -O /dev/null http://127.0.0.1/|false|" docker-compose.yml
  git add -A && git commit -q -m v4-failing-post
' || die "failing-post fixture update failed"
CDU4=$(api POST "/applications/$CU/deploy" | jsonq "d['deployment_uuid']")
[ "$(wait_deployment "$CDU4" 300)" = "failed" ] || die "a failing post hook must fail the deployment"
docker exec "$DIND_CTR" curl -sk --resolve "$WEBFQDN:443:127.0.0.1" "https://$WEBFQDN/" | grep -q 'compose-v3' \
  || die "the old container is not serving after the failed post hook (INV-005)"
docker exec "$DIND_CTR" docker inspect "$CU-web-next" >/dev/null 2>&1 && die "the failed candidate was not removed (C2)"
ok "failing post hook: deployment failed, candidate removed, old container still routed (§10)"

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
