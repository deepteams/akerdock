# The SMOKE shard: the minimal end-to-end proof that the assembled product
# works — one stack, one server, one application, from registration to safe
# deletion. It is the per-commit E2E gate (e2e-test-plan §2: "smoke à chaque
# commit, catalogue complet nightly"); everything else the full shards cover
# is either proven by unit/integration tests or belongs to the nightly run.
#
# A check earns its place here only if the assembled stack is the ONLY way to
# prove it (real SSH, real proxy, real containers). Anything computable in Go
# belongs in `go test` — see CONTRIBUTING.md "Tests".

# The lib.sh socle has already counted three checks by the time this runs:
# target server ready, boot sequence complete, server validated with the
# Traefik proxy bootstrapped. What follows is the application lifecycle.

# --- deploy a docker_image application behind the proxy ------------------------
say "deploying a docker_image application behind the proxy"
AU=$(api POST /applications "{\"source_type\":\"docker_image\",\"name\":\"web\",\"project_uuid\":\"$PU\",\"environment_uuid\":\"$EU\",\"server_uuid\":\"$S\",\"docker_image\":\"nginx\",\"docker_image_tag\":\"alpine\",\"domains\":[\"web.e2e.test\"],\"ports_exposes\":\"80\"}" | jsonq "d['uuid']")
api POST "/applications/$AU/envs" '{"key":"APP_MODE","value":"production"}' >/dev/null
DU=$(api POST "/applications/$AU/deploy" | jsonq "d['deployment_uuid']")
[ "$(wait_deployment "$DU" 180)" = "succeeded" ] || die_deployment "$DU" "image deployment failed"
docker exec "$DIND_CTR" docker exec "$AU" sh -c '[ "$APP_MODE" = production ]' || die "env var missing in container"
wait_route web.e2e.test 301
docker exec "$DIND_CTR" curl -sk --resolve web.e2e.test:443:127.0.0.1 https://web.e2e.test/ | grep -q 'Welcome to nginx' || die "HTTPS routing failed"
ok "image app deployed, env injected, HTTPS routed through Traefik"

# --- deployment logs (JSON + SSE) ----------------------------------------------
say "checking deployment logs"
api GET "/deployments/$DU/logs?limit=5" | jsonq "len(d['data'])" | grep -q 5 || die "JSON logs missing"
curl -sf -N -H "Authorization: Bearer $ROOT_TOKEN" -H 'Accept: text/event-stream' --max-time 5 "$B/deployments/$DU/logs" | grep -q 'event: end' || die "SSE end event missing"
ok "logs available in JSON and SSE"

# --- application PATCH: immediate routing regeneration --------------------------
say "application PATCH + immediate routing regeneration"
V=$(api GET "/applications/$AU" | jsonq "d['version']")
curl -sf -X PATCH -H "Authorization: Bearer $ROOT_TOKEN" -H "If-Match: \"$V\"" -H 'Content-Type: application/json' \
  -d '{"domains":["renamed.e2e.test"]}' "$B/applications/$AU" >/dev/null || die "PATCH failed"
wait_route renamed.e2e.test 301
OLD=$(docker exec "$DIND_CTR" curl -s -o /dev/null -w '%{http_code}' -H 'Host: web.e2e.test' http://127.0.0.1:80/)
[ "$OLD" = "404" ] || die "old domain still routed (got $OLD)"
ok "PATCH regenerated routing immediately"

# --- health check + zero-downtime rolling update (INV-005) ----------------------
# The one invariant no unit or integration test can prove: while a redeploy
# switches containers, a real client hammering the real proxy loses nothing.
say "health check gates a rolling, zero-downtime switch"
HC_BODY=$(python3 - "$PU" "$EU" "$S" <<'PYEOF'
import json, sys
print(json.dumps({
    "source_type": "docker_image", "name": "rolling",
    "project_uuid": sys.argv[1], "environment_uuid": sys.argv[2], "server_uuid": sys.argv[3],
    "docker_image": "nginx", "docker_image_tag": "alpine",
    "domains": ["rolling.e2e.test"], "ports_exposes": "80",
    "health_check": {"enabled": True, "path": "/", "interval_seconds": 2,
                     "timeout_seconds": 2, "retries": 3, "start_period_seconds": 1},
}))
PYEOF
)
AU5=$(api POST /applications "$HC_BODY" | jsonq "d['uuid']")
D7=$(api POST "/applications/$AU5/deploy" | jsonq "d['deployment_uuid']")
[ "$(wait_deployment "$D7" 240)" = "succeeded" ] || die_deployment "$D7" "first rolling deployment failed"
wait_route rolling.e2e.test 301
docker exec "$DIND_CTR" sh -c '
  rm -f /tmp/hits /tmp/misses
  ( for i in $(seq 1 400); do
      if curl -sk -o /dev/null --max-time 2 --resolve rolling.e2e.test:443:127.0.0.1 https://rolling.e2e.test/; then
        echo x >> /tmp/hits
      else
        echo x >> /tmp/misses
      fi
      sleep 0.15
    done ) &
  echo $! > /tmp/hammer.pid'
D8=$(api POST "/applications/$AU5/deploy" | jsonq "d['deployment_uuid']")
[ "$(wait_deployment "$D8" 240)" = "succeeded" ] || die_deployment "$D8" "rolling redeploy failed"
sleep 3
docker exec "$DIND_CTR" sh -c 'kill $(cat /tmp/hammer.pid) 2>/dev/null; true'
MISSES=$(docker exec "$DIND_CTR" sh -c 'wc -l < /tmp/misses 2>/dev/null || echo 0' | tr -d ' ')
HITS=$(docker exec "$DIND_CTR" sh -c 'wc -l < /tmp/hits 2>/dev/null || echo 0' | tr -d ' ')
[ "$HITS" -gt 20 ] || die "the traffic probe did not run ($HITS hits)"
[ "$MISSES" -eq 0 ] || die "zero-downtime violated: $MISSES failed requests during the rolling switch"
ok "rolling switch: $HITS requests served, 0 dropped during the redeploy"

# --- authentication is actually enforced ----------------------------------------
say "the API refuses anonymous and bad-token calls"
[ "$(curl -s -o /dev/null -w '%{http_code}' "$B/projects")" = "401" ] || die "anonymous call was not refused"
[ "$(curl -s -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer akd_bogus' "$B/projects")" = "401" ] || die "a bogus token was not refused"
ok "anonymous and invalid tokens refused with 401"

# --- safe deletion ---------------------------------------------------------------
say "deleting the application cleanly"
[ "$(wait_job "$(api DELETE "/applications/$AU" | jsonq "d['job_uuid']")" 120)" = "succeeded" ] || die "deletion failed"
docker exec "$DIND_CTR" docker inspect "$AU" >/dev/null 2>&1 && die "the container survived the deletion"
GONE=$(docker exec "$DIND_CTR" curl -s -o /dev/null -w '%{http_code}' -H 'Host: renamed.e2e.test' http://127.0.0.1:80/)
[ "$GONE" = "404" ] || die "the route survived the deletion (got $GONE)"
ok "application deleted: container and route gone"
