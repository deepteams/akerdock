# --- project + docker_image application with domain + env var -----------------
say "deploying a docker_image application behind the proxy"
[ "$(api GET "/projects/$PU/environments/$EU" | jsonq "d['resource_count']")" = "0" ] || die "a fresh environment must report 0 resources"
AU=$(api POST /applications "{\"source_type\":\"docker_image\",\"name\":\"web\",\"project_uuid\":\"$PU\",\"environment_uuid\":\"$EU\",\"server_uuid\":\"$S\",\"docker_image\":\"nginx\",\"docker_image_tag\":\"alpine\",\"domains\":[\"web.e2e.test\"],\"ports_exposes\":\"80\"}" | jsonq "d['uuid']")
api POST "/applications/$AU/envs" '{"key":"APP_MODE","value":"production"}' >/dev/null
DU=$(api POST "/applications/$AU/deploy" | jsonq "d['deployment_uuid']")
[ "$(wait_deployment "$DU" 180)" = "succeeded" ] || die "image deployment failed"
docker exec "$DIND_CTR" docker exec "$AU" sh -c '[ "$APP_MODE" = production ]' || die "env var missing in container"
wait_route web.e2e.test 301
docker exec "$DIND_CTR" curl -sk --resolve web.e2e.test:443:127.0.0.1 https://web.e2e.test/ | grep -q 'Welcome to nginx' || die "HTTPS routing failed"
# A multiline secret (a PEM key, a JSON blob) must reach the container intact.
# An env-file cannot carry one — the values are sourced into a shell and passed
# to docker by NAME, so they never appear in argv either (INV-003).
PEM_BODY=$(python3 <<'PYEOF'
import json
value = "-----BEGIN KEY-----\nline with 'quotes' and $(id) and `whoami`\n-----END KEY-----"
print(json.dumps({"key": "PEM_KEY", "value": value}))
PYEOF
)
api POST "/applications/$AU/envs" "$PEM_BODY" >/dev/null
DU_ML=$(api POST "/applications/$AU/deploy" | jsonq "d['deployment_uuid']")
[ "$(wait_deployment "$DU_ML" 240)" = "succeeded" ] || die "the deployment with a multiline variable failed"
GOT=$(docker exec "$DIND_CTR" docker exec "$AU" printenv PEM_KEY)
grep -q 'BEGIN KEY' <<<"$GOT" || die "the multiline variable is missing or truncated"
# The shell metacharacters must arrive literally, not expanded.
grep -q 'whoami' <<<"$GOT" || die "the middle line was lost"
grep -q "\$(id)" <<<"$GOT" || die "the value was expanded by the shell instead of quoted"
[ "$(echo "$GOT" | wc -l | tr -d ' ')" -ge 3 ] || die "the multiline variable arrived on a single line"
[ "$(api GET "/projects/$PU/environments/$EU" | jsonq "d['resource_count']")" -ge 1 ] || die "resource_count did not follow the created application"
ok "image app deployed, env injected (multiline secret intact), HTTPS routed through Traefik"

# --- deployment logs (JSON + SSE) ---------------------------------------------
say "checking deployment logs"
api GET "/deployments/$DU/logs?limit=5" | jsonq "len(d['data'])" | grep -q 5 || die "JSON logs missing"
curl -sf -N -H "Authorization: Bearer $ROOT_TOKEN" -H 'Accept: text/event-stream' --max-time 5 "$B/deployments/$DU/logs" | grep -q 'event: end' || die "SSE end event missing"
ok "logs available in JSON and SSE"

# --- dockerfile application ----------------------------------------------------
say "deploying an inline-Dockerfile application"
DF_BODY=$(python3 - "$PU" "$EU" "$S" <<'PYEOF'
import json, sys
print(json.dumps({
    "source_type": "dockerfile", "name": "built",
    "project_uuid": sys.argv[1], "environment_uuid": sys.argv[2], "server_uuid": sys.argv[3],
    "dockerfile": 'FROM nginx:alpine\nRUN echo "hello from dockerfile build" > /usr/share/nginx/html/index.html\n',
    "domains": ["built.e2e.test"], "ports_exposes": "80",
}))
PYEOF
)
AU2=$(api POST /applications "$DF_BODY" | jsonq "d['uuid']")
DU2=$(api POST "/applications/$AU2/deploy" | jsonq "d['deployment_uuid']")
[ "$(wait_deployment "$DU2" 180)" = "succeeded" ] || die "dockerfile deployment failed"
wait_route built.e2e.test 301
docker exec "$DIND_CTR" curl -sk --resolve built.e2e.test:443:127.0.0.1 https://built.e2e.test/ | grep -q 'hello from dockerfile build' || die "dockerfile app not serving its built content"
ok "dockerfile app built remotely and routed"

# --- build args and BuildKit secrets (§5.2, INV-003) ---------------------------------
say "build args reach the Dockerfile; build secrets never reach the image"
BA_BODY=$(python3 - "$PU" "$EU" "$S" <<'PYEOF'
import json, sys
dockerfile = """FROM nginx:alpine
ARG APP_VERSION
# A build secret is mounted for the lifetime of this RUN only — it is never a
# layer, and never appears in `docker history`.
RUN --mount=type=secret,id=NPM_TOKEN \\
    test -s /run/secrets/NPM_TOKEN && \\
    echo "version=$APP_VERSION" > /usr/share/nginx/html/index.html && \\
    echo "secret_len=$(wc -c < /run/secrets/NPM_TOKEN)" >> /usr/share/nginx/html/index.html
"""
print(json.dumps({
    "source_type": "dockerfile", "name": "buildargapp",
    "project_uuid": sys.argv[1], "environment_uuid": sys.argv[2], "server_uuid": sys.argv[3],
    "dockerfile": dockerfile, "ports_exposes": "80",
}))
PYEOF
)
AU12=$(api POST /applications "$BA_BODY" | jsonq "d['uuid']")
# A plain build arg, and a build secret that must never end up in the image.
api POST "/applications/$AU12/envs" '{"key":"APP_VERSION","value":"1.2.3","is_build_time":true}' >/dev/null
api POST "/applications/$AU12/envs" '{"key":"NPM_TOKEN","value":"npm_supersecret_value","is_build_time":true,"is_secret":true}' >/dev/null
# The flag must round-trip: without it the variable would become a --build-arg,
# and a build arg is written into the image metadata (INV-003).
api GET "/applications/$AU12/envs" | jsonq "str([v['is_secret'] for v in d['data'] if v['key']=='NPM_TOKEN'])" | grep -q True \
  || die "is_secret was not persisted — the variable would leak into docker history"

D_BA=$(api POST "/applications/$AU12/deploy" | jsonq "d['deployment_uuid']")
[ "$(wait_deployment "$D_BA" 240)" = "succeeded" ] || \
  die "the build with args and secrets failed: $(api GET "/deployments/$D_BA" | jsonq "d.get('error_message') or '?'") | $(api GET "/deployments/$D_BA/logs?limit=100" | tr -d '\n' | tail -c 600)"

# The build arg reached the Dockerfile...
docker exec "$DIND_CTR" docker exec "$AU12" cat /usr/share/nginx/html/index.html | grep -q 'version=1.2.3' \
  || die "the build arg did not reach the Dockerfile"
# ...and the secret was readable AT BUILD TIME (the RUN would have failed otherwise).
docker exec "$DIND_CTR" docker exec "$AU12" cat /usr/share/nginx/html/index.html | grep -q 'secret_len=2[0-9]' \
  || die "the build secret was not mounted during the build"

# THE point: the secret is nowhere in the image — not in its history, not in a
# layer, not in the environment (INV-003).
# The deployment exposes the digest, not the reference: take the image the
# running container was created from.
IMG=$(docker exec "$DIND_CTR" docker inspect --format '{{.Config.Image}}' "$AU12")
[ -n "$IMG" ] || die "could not resolve the built image of the application"
# `grep && die` would abort the script under `set -e` when grep finds nothing —
# which is the PASSING case here. So the absence is asserted explicitly.
HIST=$(docker exec "$DIND_CTR" docker history --no-trunc "$IMG" 2>/dev/null || true)
if grep -q 'npm_supersecret_value' <<<"$HIST"; then die "the build secret leaked into docker history (INV-003)"; fi
CFG=$(docker exec "$DIND_CTR" docker inspect "$IMG" 2>/dev/null || true)
if grep -q 'npm_supersecret_value' <<<"$CFG"; then die "the build secret leaked into the image config (INV-003)"; fi
LAYER=$(docker exec "$DIND_CTR" docker run --rm --entrypoint sh "$IMG" -c 'cat /run/secrets/NPM_TOKEN 2>/dev/null' || true)
if grep -q 'npm_supersecret' <<<"$LAYER"; then die "the build secret was baked into a layer (INV-003)"; fi
ok "build arg reached the build; build secret was mounted then left out of the image entirely"

# --- application PATCH: immediate routing regeneration ------------------------
say "application PATCH + immediate routing regeneration"
V=$(api GET "/applications/$AU2" | jsonq "d['version']")
curl -sf -X PATCH -H "Authorization: Bearer $ROOT_TOKEN" -H "If-Match: \"$V\"" -H 'Content-Type: application/json' \
  -d '{"domains":["renamed.e2e.test"]}' "$B/applications/$AU2" >/dev/null || die "PATCH failed"
wait_route renamed.e2e.test 301
OLD=$(docker exec "$DIND_CTR" curl -s -o /dev/null -w '%{http_code}' -H 'Host: built.e2e.test' http://127.0.0.1:80/)
[ "$OLD" = "404" ] || die "old domain still routed (got $OLD)"
STATUS=$(curl -s -o /dev/null -w '%{http_code}' -X PATCH -H "Authorization: Bearer $ROOT_TOKEN" -H "If-Match: \"$V\"" -H 'Content-Type: application/json' -d '{"name":"x"}' "$B/applications/$AU2")
[ "$STATUS" = "409" ] || die "stale If-Match must conflict (got $STATUS)"
ok "PATCH regenerated routing immediately, optimistic locking enforced"

# --- deployment cancellation ----------------------------------------------------
say "deployment cancellation (queued behind the per-app lock)"
D1=$(api POST "/applications/$AU/deploy" | jsonq "d['deployment_uuid']")
D2=$(api POST "/applications/$AU/deploy" | jsonq "d['deployment_uuid']")
api POST "/deployments/$D2/cancel" >/dev/null
[ "$(wait_deployment "$D2" 60)" = "cancelled" ] || die "queued deployment not cancelled"
[ "$(wait_deployment "$D1" 180)" = "succeeded" ] || die "in-flight deployment was affected by the cancel"
STATUS=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $ROOT_TOKEN" "$B/deployments/$D1/cancel")
[ "$STATUS" = "409" ] || die "terminal deployment cancel must 409 (got $STATUS)"
ok "queued deployment cancelled, in-flight one unaffected, terminal one refused"

# --- health check + zero-downtime rolling update (§7.2) --------------------------
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
[ "$(wait_deployment "$D7" 240)" = "succeeded" ] || die "first rolling deployment failed"
docker exec "$DIND_CTR" docker inspect --format '{{.State.Health.Status}}' "$AU5" | grep -q healthy || die "container has no healthy state"
wait_route rolling.e2e.test 301

# Redeploy while hammering the endpoint: the old container must keep serving
# until the candidate is healthy and routed (INV-005 — no downtime window).
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
[ "$(wait_deployment "$D8" 240)" = "succeeded" ] || die "rolling redeploy failed"
sleep 3
docker exec "$DIND_CTR" sh -c 'kill $(cat /tmp/hammer.pid) 2>/dev/null; true'
MISSES=$(docker exec "$DIND_CTR" sh -c 'wc -l < /tmp/misses 2>/dev/null || echo 0' | tr -d ' ')
HITS=$(docker exec "$DIND_CTR" sh -c 'wc -l < /tmp/hits 2>/dev/null || echo 0' | tr -d ' ')
[ "$HITS" -gt 20 ] || die "the traffic probe did not run ($HITS hits)"
[ "$MISSES" -eq 0 ] || die "zero-downtime violated: $MISSES failed requests during the rolling switch"
# the switch went through the candidate IP, then stabilized on the container name
docker exec "$PG_CTR" psql -U postgres -d akerdock -tAc "SELECT count(*) FROM deployment_steps s JOIN deployments d ON d.id=s.deployment_id WHERE d.uuid='$D8' AND s.name IN ('resolve_endpoint','switch_routing')" | grep -q '^2$' || die "rolling switch steps missing"
ok "rolling switch: $HITS requests served, 0 dropped during the redeploy"

# --- idempotency + rate limiting (§24.1) --------------------------------------------
say "Idempotency-Key replay and per-token rate limiting"
IDEM="e2e-$(openssl rand -hex 8)"
BODY='{"name":"idem-token","permissions":["read"]}'
TEAM=$(api GET /teams | jsonq "d['data'][0]['uuid']")
R1=$(curl -s -X POST -H "Authorization: Bearer $ROOT_TOKEN" -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $IDEM" -d "$BODY" "$B/teams/$TEAM/tokens")
R2=$(curl -s -X POST -H "Authorization: Bearer $ROOT_TOKEN" -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $IDEM" -d "$BODY" "$B/teams/$TEAM/tokens")
U1=$(echo "$R1" | jsonq "d['uuid']"); U2=$(echo "$R2" | jsonq "d['uuid']")
[ "$U1" = "$U2" ] || die "replaying an Idempotency-Key must return the original response ($U1 vs $U2)"
CREATED=$(docker exec "$PG_CTR" psql -U postgres -d akerdock -tAc "SELECT count(*) FROM api_tokens WHERE name = 'idem-token'")
[ "$CREATED" -eq 1 ] || die "the operation ran $CREATED times despite the idempotency key"
# same key, different body → 409 idempotency_conflict
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $ROOT_TOKEN" -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $IDEM" -d '{"name":"other","permissions":["read"]}' "$B/teams/$TEAM/tokens")
[ "$CODE" = "409" ] || die "the same key with a different body must conflict (got $CODE)"

# rate limit: burst past the per-token budget and expect a 429 with Retry-After
RL_TOKEN="akd_$(openssl rand -hex 24)"
docker exec "$PG_CTR" psql -U postgres -d akerdock -q -c \
  "INSERT INTO api_tokens (team_id, name, token_prefix, token_hash, permissions) SELECT id, 'rl', left('$RL_TOKEN',10), encode(digest('$RL_TOKEN','sha256'),'hex'), '{read}' FROM teams LIMIT 1"
LIMITED=0
for _ in $(seq 1 230); do
  C=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $RL_TOKEN" "$B/version")
  [ "$C" = "429" ] && { LIMITED=1; break; }
done
[ "$LIMITED" = "1" ] || die "the rate limit never triggered"
RETRY=$(curl -s -o /dev/null -D - -H "Authorization: Bearer $RL_TOKEN" "$B/version" | grep -i '^retry-after' | tr -d '\r')
[ -n "$RETRY" ] || die "429 must carry a Retry-After header"
# the root token, on its own budget, is unaffected
[ "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $ROOT_TOKEN" "$B/version")" = "200" ] || die "another token's budget was consumed"
ok "idempotent replay (single execution), 409 on body mismatch, 429 with $RETRY"

# --- scheduler: proxy drift reconciliation (§6.2.4) ---------------------------------
say "the scheduler detects and repairs a manual edit of a routing file"
grep -q "scheduler elected leader" "$WORKDIR/api.log" || die "the scheduler did not take the advisory lock"
DYN="/data/akerdock/proxy/dynamic/${AU}.yaml"
GOOD=$(docker exec "$DIND_CTR" sha256sum "$DYN" | cut -d' ' -f1)
docker exec "$DIND_CTR" sh -c "echo '# manually tampered' >> $DYN"
docker exec "$DIND_CTR" sh -c "sha256sum $DYN | cut -d' ' -f1" | grep -qv "$GOOD" || die "the tamper did not change the file"
# force a scheduler pass by restarting the process (the cron ticks every 5 min)
kill -TERM "$API_PID" 2>/dev/null; sleep 2
start_akerdock
for _ in $(seq 1 20); do
  NOW=$(docker exec "$DIND_CTR" sha256sum "$DYN" | cut -d' ' -f1)
  [ "$NOW" = "$GOOD" ] && break
  sleep 1
done
[ "$NOW" = "$GOOD" ] || die "the drift was not repaired (checksum still $NOW)"
grep -q "proxy config drift detected" "$WORKDIR/api.log" || die "the drift was not reported"
ok "manual edit detected by checksum and the expected revision re-applied"

# --- crash recovery during the switch (§2.5, INV-004/005) ----------------------------
say "a deployment that crashed mid-switch is inspected, never replayed blindly"
# A rolling app (health check ⇒ candidate container), deployed and serving.
CR_BODY=$(python3 - "$PU" "$EU" "$S" <<'PYEOF'
import json, sys
print(json.dumps({
    "source_type": "docker_image", "name": "crashapp",
    "project_uuid": sys.argv[1], "environment_uuid": sys.argv[2], "server_uuid": sys.argv[3],
    "docker_image": "nginx", "docker_image_tag": "alpine",
    "domains": ["crash.e2e.test"], "ports_exposes": "80",
    "health_check": {"enabled": True, "path": "/", "interval_seconds": 2,
                     "timeout_seconds": 2, "retries": 3, "start_period_seconds": 1},
}))
PYEOF
)
AU10=$(api POST /applications "$CR_BODY" | jsonq "d['uuid']")
[ "$(wait_deployment "$(api POST "/applications/$AU10/deploy" | jsonq "d['deployment_uuid']")" 240)" = "succeeded" ] || die "the crashapp deployment failed"

# Reproduce the exact aftermath of a worker dying mid-switch (§4, case c):
# the candidate is up, the old container is gone, the rename never happened.
docker exec "$DIND_CTR" docker rename "$AU10" "${AU10}-next" || die "could not stage the crash scenario"
CR_DEP=$(docker exec "$PG_CTR" psql -U postgres -d akerdock -tAc \
  "SELECT uuid FROM deployments WHERE resource_id = (SELECT id FROM resources WHERE uuid = '$AU10') ORDER BY id DESC LIMIT 1" | tr -d ' ')
# The deployment is left in `switching`, its job leased by a worker that is now
# dead: exactly what the reaper finds after a crash.
docker exec "$PG_CTR" psql -U postgres -d akerdock -q \
  -c "UPDATE deployments SET status = 'switching' WHERE uuid = '$CR_DEP'" \
  -c "UPDATE jobs SET status = 'leased', attempt = 1, lease_expires_at = now() - interval '5 minutes',
      leased_by = 'dead-worker'
      WHERE job_type = 'deployment.run' AND payload->>'deployment_id' =
        (SELECT id::text FROM deployments WHERE uuid = '$CR_DEP')"

# The reaper hands the job back; the resume must INSPECT and finish the switch.
for _ in $(seq 60); do
  ST=$(api GET "/deployments/$CR_DEP" | jsonq "d['status']")
  [ "$ST" = "succeeded" ] && break
  [ "$ST" = "failed" ] && die "the resume failed instead of completing the interrupted switch: $(api GET "/deployments/$CR_DEP" | jsonq "d.get('error_message') or '?'") — steps: $(api GET "/deployments/$CR_DEP/logs" | tr -d '\n' | tail -c 400)"
  sleep 2
done
[ "$ST" = "succeeded" ] || die "the crashed deployment was not resumed (status: $ST)"
# It resumed at the rename, and did not switch twice: exactly one container, under the final name.
[ "$(docker exec "$DIND_CTR" docker ps -q --filter "name=^${AU10}$" | wc -l | tr -d ' ')" = "1" ] || die "the promoted container is missing after the resume"
docker exec "$DIND_CTR" docker inspect "${AU10}-next" >/dev/null 2>&1 && die "a candidate container survived the resume — the switch was replayed"
# And it took the inspection path, not a blind replay.
# limit=100: the resume steps come after the ~25 steps of the crashed attempt,
# and the default page would cut them off.
CRLOGS=$(api GET "/deployments/$CR_DEP/logs?limit=100")
grep -q 'resume_inspect' <<<"$CRLOGS" || die "the resume did not record its inspection"
docker exec "$DIND_CTR" curl -sk --resolve crash.e2e.test:443:127.0.0.1 https://crash.e2e.test/ | grep -qi 'nginx' || die "the app is not serving after the resume"
ok "crashed switch inspected and completed at the rename, no double switch (INV-004)"

# --- remnants of a failed deletion (§20.6.4) -----------------------------------------
say "a deletion that fails records what it left behind, and forget demands an acknowledgement"
RM_BODY=$(python3 - "$PU" "$EU" "$S" <<'PYEOF'
import json, sys
print(json.dumps({
    "source_type": "docker_image", "name": "remnantapp",
    "project_uuid": sys.argv[1], "environment_uuid": sys.argv[2], "server_uuid": sys.argv[3],
    "docker_image": "nginx", "docker_image_tag": "alpine", "ports_exposes": "80",
}))
PYEOF
)
AU11=$(api POST /applications "$RM_BODY" | jsonq "d['uuid']")
[ "$(wait_deployment "$(api POST "/applications/$AU11/deploy" | jsonq "d['deployment_uuid']")" 240)" = "succeeded" ] || die "the remnantapp deployment failed"

# Make the remote cleanup fail for real: the application directory is turned
# into an immutable-ish obstacle — `rm -rf` on a busy mount point fails, so the
# delete job cannot finish and must record what it could not remove.
docker exec "$DIND_CTR" sh -c "mkdir -p /data/akerdock/applications/$AU11/stuck && mount -t tmpfs none /data/akerdock/applications/$AU11/stuck" \
  || die "could not stage the failing-deletion scenario"

DEL_JOB=$(api DELETE "/applications/$AU11" | jsonq "d['job_uuid']")
[ "$(wait_job "$DEL_JOB" 180)" = "dead_letter" ] || die "the deletion should have failed (the directory is not removable)"

# The remnants were recorded. The cleanup removes the container first and only
# then the files, so what survives here is the DIRECTORY — and that is exactly
# what the inventory must name.
REM=$(docker exec "$PG_CTR" psql -U postgres -d akerdock -tAc \
  "SELECT remnants FROM resources WHERE uuid = '$AU11'")
[ -n "$REM" ] && [ "$REM" != "null" ] || die "the failed deletion recorded no remnants (§20.6.4)"
grep -q "/data/akerdock/applications/$AU11" <<<"$REM" || die "the remnants do not name the leftover files: $REM"

# Forgetting without acknowledging must be refused, WITH the list of leftovers.
FORGET=$(curl -s -w '\n%{http_code}' -X POST -H "Authorization: Bearer $ROOT_TOKEN" -H 'Content-Type: application/json' \
  -d '{}' "$B/jobs/$DEL_JOB/forget")
FCODE=$(tail -1 <<<"$FORGET"); FBODY=$(sed '$d' <<<"$FORGET")
[ "$FCODE" = "409" ] || die "forgetting a job with remnants must be refused (got $FCODE)"
grep -q 'remnants_present' <<<"$FBODY" || die "the refusal must carry the remnants_present code"
grep -q "$AU11" <<<"$FBODY" || die "the refusal must list WHAT is left on the server"

# With an explicit acknowledgement it goes through — and cleans up NOTHING.
api POST "/jobs/$DEL_JOB/forget" '{"acknowledge_remnants":true}' >/dev/null || die "an acknowledged forget must succeed"
[ "$(api GET "/jobs/$DEL_JOB" | jsonq "d['status']")" = "cancelled" ] || die "the forgotten job is not cancelled"
docker exec "$DIND_CTR" test -d "/data/akerdock/applications/$AU11" || die "forget must NOT delete anything remotely — the leftover directory should still be there"

# Clear the obstacle by hand — exactly what the operator has to do — then
# delete again: the retried deletion now succeeds and tombstones the resource.
docker exec "$DIND_CTR" sh -c "umount /data/akerdock/applications/$AU11/stuck" || true
[ "$(wait_job "$(api DELETE "/applications/$AU11" | jsonq "d['job_uuid']")" 120)" = "succeeded" ] \
  || die "the retried deletion should succeed once the obstacle is gone"
ok "failed deletion recorded its remnants; forget refused without acknowledgement, removed nothing; retry then succeeded"

# --- proxy revisions: applied + verified, removal verified -------------------------
say "proxy revisions are checksummed, applied and verified (§6.2-§6.5)"
APPLIED=$(docker exec "$PG_CTR" psql -U postgres -d akerdock -tAc "SELECT count(*) FROM proxy_config_revisions WHERE status = 'applied'")
[ "$APPLIED" -ge 3 ] || die "expected applied proxy revisions, got $APPLIED"
BAD=$(docker exec "$PG_CTR" psql -U postgres -d akerdock -tAc "SELECT count(*) FROM proxy_config_revisions WHERE status IN ('failed','rolled_back')")
[ "$BAD" -eq 0 ] || die "$BAD proxy revisions failed or were rolled back"
# every revision carries a checksum and no secret
docker exec "$PG_CTR" psql -U postgres -d akerdock -tAc "SELECT count(*) FROM proxy_config_revisions WHERE length(checksum_sha256) <> 64" | grep -q '^0$' || die "malformed revision checksum"
ok "proxy revisions recorded and verified ($APPLIED applied, 0 failed)"

# --- audit trail + outbox --------------------------------------------------------
say "audit trail and transactional outbox"
AUDIT_COUNT=$(docker exec "$PG_CTR" psql -U postgres -d akerdock -tAc "SELECT count(*) FROM audit_events")
[ "$AUDIT_COUNT" -ge 4 ] || die "expected audited actions, got $AUDIT_COUNT rows"
docker exec "$PG_CTR" psql -U postgres -d akerdock -tAc "SELECT count(*) FROM audit_events WHERE action = 'deployment.trigger'" | grep -qv '^0$' || die "deployment.trigger not audited"
OUTBOX_COUNT=$(docker exec "$PG_CTR" psql -U postgres -d akerdock -tAc "SELECT count(*) FROM outbox_events WHERE event_type = 'deployment.succeeded.v1'")
[ "$OUTBOX_COUNT" -ge 2 ] || die "expected deployment.succeeded.v1 outbox events, got $OUTBOX_COUNT"

# --- proxy lifecycle (§3) ---------------------------------------------------------
say "proxy lifecycle: stop cuts every route of the server, start brings them back"
# The routed application of this shard answers right now.
UP=$(docker exec "$DIND_CTR" curl -s -o /dev/null -w '%{http_code}' -H 'Host: web.e2e.test' http://127.0.0.1:80/)
[ "$UP" = "301" ] || die "the route is not up before the proxy test (got $UP)"

PJ=$(api POST "/servers/$S/proxy/stop" | jsonq "d['job_uuid']")
[ "$(wait_job "$PJ" 90)" = "succeeded" ] || die "proxy stop failed"
docker exec "$DIND_CTR" docker inspect --format '{{.State.Status}}' akerdock-proxy | grep -q running && die "the proxy is still running after stop"
# The blast radius the UI warns about, verified: nothing answers on 80 anymore.
# curl fails to connect at all (there is nothing listening): the exit code is
# what proves it, and -w prints 000 on a connection it never made.
# `|| true` because a refused connection is the EXPECTED outcome here: curl
# exits non-zero and still prints 000, which is exactly the proof we want.
DOWN=$(docker exec "$DIND_CTR" curl -s -o /dev/null -w '%{http_code}' --max-time 5 -H 'Host: web.e2e.test' http://127.0.0.1:80/ || true)
[ "$DOWN" = "000" ] || die "traffic still flows with the proxy stopped (got $DOWN)"
# The intent is persisted and visible.
[ "$(api GET "/servers/$S" | jsonq "d['proxy_desired_state']")" = "stopped" ] || die "the stop intent was not persisted"
[ "$(api GET "/servers/$S" | jsonq "d['proxy_observed_status']")" = "exited" ] || die "the proxy observed status is wrong"

# A deliberately stopped proxy is NOT "repaired" by the drift reconciler: an
# intent is not an accident. Give the reconciler a full maintenance window.
sleep 8
docker exec "$DIND_CTR" docker inspect --format '{{.State.Status}}' akerdock-proxy | grep -q running && die "the reconciler restarted a proxy the operator stopped"

PJ=$(api POST "/servers/$S/proxy/start" | jsonq "d['job_uuid']")
[ "$(wait_job "$PJ" 120)" = "succeeded" ] || die "proxy start failed"
[ "$(api GET "/servers/$S" | jsonq "d['proxy_desired_state']")" = "running" ] || die "the start intent was not persisted"
wait_route web.e2e.test 301
ok "proxy stopped (all routes down, intent persisted, not auto-repaired) then started (routes back)"

say "proxy logs are readable from the API"
LOGS=$(api GET "/servers/$S/proxy/logs?lines=50")
[ "$(echo "$LOGS" | jsonq "len(d['data']) > 0")" = "True" ] || die "the proxy logs came back empty: $LOGS"
ok "proxy logs served ($(echo "$LOGS" | jsonq "len(d['data'])") lines)"
