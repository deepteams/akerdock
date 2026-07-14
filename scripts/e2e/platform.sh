# --- base application (fixture) ------------------------------------------------
# Deployed here rather than in the library: only the shards that observe a
# running application pay for it.
base_app

# --- realtime SSE event stream (ADR-024) --------------------------------------------
say "SSE event stream fed by the transactional outbox"
# Listen while a deployment runs: the status transitions must arrive live.
curl -sN -H "Authorization: Bearer $ROOT_TOKEN" -H 'Accept: text/event-stream' --max-time 45 \
  "$B/events" > "$WORKDIR/events.txt" 2>/dev/null &
SSE_PID=$!
sleep 1
D9=$(api POST "/applications/$AU/deploy" | jsonq "d['deployment_uuid']")
[ "$(wait_deployment "$D9" 180)" = "succeeded" ] || die "deployment for the SSE test failed"
sleep 2
kill $SSE_PID 2>/dev/null || true
grep -q 'event: deployment.succeeded.v1' "$WORKDIR/events.txt" || die "no deployment.succeeded.v1 event received live"
grep -q 'event: deployment.building.v1' "$WORKDIR/events.txt" || die "intermediate transitions missing from the stream"
FIRST_ID=$(grep -m1 '^id: ' "$WORKDIR/events.txt" | cut -d' ' -f2)
# Resume from Last-Event-ID: the missed events are replayed
curl -sN -H "Authorization: Bearer $ROOT_TOKEN" -H 'Accept: text/event-stream' \
  -H "Last-Event-ID: $FIRST_ID" --max-time 5 "$B/events" > "$WORKDIR/replay.txt" 2>/dev/null || true
REPLAYED=$(grep -c '^id: ' "$WORKDIR/replay.txt" || echo 0)
[ "$REPLAYED" -ge 3 ] || die "Last-Event-ID resume replayed only $REPLAYED events"
docker exec "$PG_CTR" psql -U postgres -d akerdock -tAc "SELECT count(*) FROM outbox_events WHERE published_at IS NULL" | grep -q '^0$' || die "outbox events left unpublished"
ok "live events streamed, Last-Event-ID replayed $REPLAYED events, outbox fully drained"

# --- notifications (§11, ADR-019) ----------------------------------------------------
say "routing a deployment event to a webhook channel"
CH=$(api POST /notification-channels "{\"kind\":\"webhook\",\"name\":\"ops\",\"url\":\"http://127.0.0.1:${SINK_PORT}/hook\"}")
CHU=$(echo "$CH" | jsonq "d['uuid']")
grep -q "${SINK_PORT}" <<<"$CH" && die "the channel URL leaked into the response (INV-003)"
# A channel is validated for the transport it CLAIMS to be. A telegram channel
# carrying a webhook URL and no chat_id would be accepted, stored, and would
# then fail at the only moment it matters — so it is refused here.
BADCH=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $ROOT_TOKEN" -H 'Content-Type: application/json' \
  -d '{"kind":"telegram","name":"tg","url":"https://api.telegram.org/x"}' "$B/notification-channels")
[ "$BADCH" = "422" ] || die "a telegram channel without bot_token/chat_id must be refused (got $BADCH)"

# --- SMTP: a real mail server receives a real alert (amendement n°18) ----------
# mailpit accepts mail on 1025 and exposes what it received on its HTTP API:
# the assertion is that the alert LANDED, not that the code did not error.
MAIL_CTR=akerdock-e2e-mail-$SHARD
docker rm -f "$MAIL_CTR" >/dev/null 2>&1 || true
docker run -d --rm --name "$MAIL_CTR" -p "${MAIL_SMTP_PORT}:1025" -p "${MAIL_HTTP_PORT}:8025" axllent/mailpit >/dev/null
for _ in $(seq 1 30); do curl -sf "http://127.0.0.1:${MAIL_HTTP_PORT}/api/v1/messages" >/dev/null 2>&1 && break; sleep 1; done

# encryption=none is asked for explicitly: this relay is local and offers no TLS.
# Inferring it from the port would mean silently sending credentials in clear.
MAIL_CH=$(api POST /notification-channels "$(python3 - "$MAIL_SMTP_PORT" <<'PYEOF'
import json, sys
print(json.dumps({"kind": "smtp", "name": "mail", "smtp": {
    "host": "127.0.0.1", "port": int(sys.argv[1]), "from": "akerdock@e2e.test",
    "to": ["ops@e2e.test"], "encryption": "none"}}))
PYEOF
)" | jsonq "d['uuid']")
[ "$(api POST "/notification-channels/$MAIL_CH/test" | jsonq "d['delivered']")" = "True" ] || die "the SMTP channel test did not deliver"
MAILS=$(curl -sf "http://127.0.0.1:${MAIL_HTTP_PORT}/api/v1/messages" | jsonq "d['total']")
[ "$MAILS" -ge 1 ] || die "the SMTP channel reported success but no mail arrived"
curl -sf "http://127.0.0.1:${MAIL_HTTP_PORT}/api/v1/messages" | jsonq "d['messages'][0]['Subject']" | grep -q akerdock \
  || die "the alert subject does not identify akerdock"

# --- transactional email + invitations (§14.2, amendement n°20) -----------------------
# The instance relay is verified BEFORE it is accepted: a relay that cannot be
# reached is refused here, where an operator is looking, and not at the first
# invitation — where the only symptom would be a mail that never arrives.
BADMAIL=$(curl -s -o /dev/null -w '%{http_code}' -X PUT -H "Authorization: Bearer $ROOT_TOKEN" -H 'Content-Type: application/json' \
  -d '{"kind":"smtp","smtp":{"host":"127.0.0.1","port":1,"from":"a@b.c","to":["d@e.f"],"encryption":"none"}}' "$B/system/email")
[ "$BADMAIL" = "422" ] || die "an unreachable transactional relay must be refused (got $BADMAIL)"

api PUT /system/email "$(python3 - "$MAIL_SMTP_PORT" <<'PYEOF'
import json, sys
print(json.dumps({"kind": "smtp", "smtp": {
    "host": "127.0.0.1", "port": int(sys.argv[1]), "from": "akerdock@e2e.test",
    "to": ["placeholder@e2e.test"], "encryption": "none"}}))
PYEOF
)" >/dev/null
# The credentials are never rendered back — not even to root.
api GET /system/email | grep -q placeholder && die "the transactional email configuration was returned by the API (INV-003)"
api GET /system/email | jsonq "d['configured']" | grep -qi true || die "the transactional email is not reported as configured"

TEAM_UUID=$(api GET /teams | jsonq "d['data'][0]['uuid']")
INVITE=$(api POST "/teams/$TEAM_UUID/invitations" '{"email":"invitee@e2e.test","role":"member"}')
# The link stays in the response: the mail is an ADDITION. An instance whose
# relay hiccups must still be able to hand the invitation over.
echo "$INVITE" | jsonq "d['invite_url']" | grep -q 'token=' || die "the invitation link is no longer returned"
for _ in $(seq 1 20); do
  TO=$(curl -sf "http://127.0.0.1:${MAIL_HTTP_PORT}/api/v1/messages" | jsonq "json.dumps(d['messages'])")
  printf '%s' "$TO" | grep -q 'invitee@e2e.test' && break
  sleep 1
done
printf '%s' "$TO" | grep -q 'invitee@e2e.test' || die "the invitation mail never reached the invitee"
docker rm -f "$MAIL_CTR" >/dev/null 2>&1 || true
ok "SMTP channel delivered a real mail; the instance relay is verified before acceptance and carries invitations"
say "routing a deployment event to a webhook channel"
# The test endpoint proves the configuration now, not at the first outage.
[ "$(api POST "/notification-channels/$CHU/test" | jsonq "d['delivered']")" = "True" ] || die "the channel test did not deliver"

# Route deployment failures of this team to the channel, and info events too.
api POST "/notification-channels/$CHU/rules" '{"event_type":"deployment.succeeded.v1","min_severity":"info"}' >/dev/null
api POST "/notification-channels/$CHU/rules" '{"event_type":"deployment.failed.v1","min_severity":"warning"}' >/dev/null
[ "$(api GET "/notification-channels/$CHU/rules" | jsonq "len(d['data'])")" = "2" ] || die "the rules were not recorded"
# The same event and scope twice is a conflict, not a duplicate rule.
DUP=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $ROOT_TOKEN" -H 'Content-Type: application/json' \
  -d '{"event_type":"deployment.succeeded.v1"}' "$B/notification-channels/$CHU/rules")
[ "$DUP" = "409" ] || die "a duplicate rule must conflict (got $DUP)"

# Deploy: the dispatcher must pick the event up from the outbox and post it.
DU8=$(api POST "/applications/$AU/deploy" | jsonq "d['deployment_uuid']")
[ "$(wait_deployment "$DU8" 240)" = "succeeded" ] || die "the notified deployment failed"
GOT=""
for _ in $(seq 60); do
  GOT=$(grep -c 'deployment.succeeded.v1' "$WORKDIR/hooks.jsonl" 2>/dev/null || echo 0)
  [ "$GOT" -gt 0 ] && break
  sleep 2
done
[ "${GOT:-0}" -gt 0 ] || die "no deployment.succeeded notification reached the webhook"
# The payload is structured, carries the severity, and never the channel secret.
python3 - "$WORKDIR/hooks.jsonl" <<'PYEOF' || die "the notification payload is wrong"
import json, sys
events = [json.loads(l) for l in open(sys.argv[1]) if l.strip()]
deploys = [e for e in events if e.get("event_type") == "deployment.succeeded.v1"]
assert deploys, "no deployment event"
e = deploys[-1]
assert e["severity"] == "info", e["severity"]
assert e.get("team_uuid"), "the event carries no team"
assert e.get("resource_uuid"), "the event carries no resource"
PYEOF

# Expiring certificates alert through the same pipeline (§4.3): the scheduler
# emits certificate.expiring.v1, the rules route it. A certificate expiring in
# 5 days is seeded directly — the sandbox has no public DNS to get a real one.
api POST "/notification-channels/$CHU/rules" '{"event_type":"certificate.expiring.v1","min_severity":"warning"}' >/dev/null
docker exec "$PG_CTR" psql -U postgres -d akerdock -q -c \
  "INSERT INTO certificates (server_id, kind, main_domain, issuer, not_before, not_after, status)
   SELECT id, 'acme_http01', 'expiring.e2e.test', 'E2E CA', now() - interval '85 days', now() + interval '5 days', 'issued'
   FROM servers LIMIT 1"
EXPIRING=""
for _ in $(seq 40); do
  EXPIRING=$(grep -c 'certificate.expiring.v1' "$WORKDIR/hooks.jsonl" 2>/dev/null || echo 0)
  [ "$EXPIRING" -gt 0 ] && break
  sleep 2
done
[ "${EXPIRING:-0}" -gt 0 ] || die "no certificate.expiring notification reached the webhook"
# The alert fires once per threshold, not on every scheduler pass.
sleep 10
AGAIN=$(grep -c 'certificate.expiring.v1' "$WORKDIR/hooks.jsonl" 2>/dev/null || echo 0)
[ "$AGAIN" = "$EXPIRING" ] || die "the expiry alert repeated itself ($EXPIRING then $AGAIN)"
# It carries the warning severity — so quiet hours may hold it, but a debounce
# window must not swallow it silently.
python3 - "$WORKDIR/hooks.jsonl" <<'PYEOF' || die "the expiry alert payload is wrong"
import json, sys
events = [json.loads(l) for l in open(sys.argv[1]) if l.strip()]
certs = [e for e in events if e.get("event_type") == "certificate.expiring.v1"]
assert certs, "no certificate event"
e = certs[-1]
assert e["severity"] == "warning", e["severity"]
assert e["payload"]["main_domain"] == "expiring.e2e.test", e["payload"]
assert 4 <= e["payload"]["days_left"] <= 5, e["payload"]["days_left"]
PYEOF
ok "webhook channel tested, deployment event routed, expiring certificate alerted once at its threshold"

# --- deferred digest (ADR-019 §4) ----------------------------------------------------
say "non-critical events are grouped into a digest instead of one message each"
CH2=$(api POST /notification-channels "{\"kind\":\"webhook\",\"name\":\"digest\",\"url\":\"http://127.0.0.1:${SINK_PORT}/digest\"}" | jsonq "d['uuid']")
# A digest rule with a 1-minute window: info events wait instead of firing one
# by one. deployment.succeeded is exactly the kind of event nobody wants a
# message for, one per deploy.
api POST "/notification-channels/$CH2/rules" \
  '{"event_type":"deployment.succeeded.v1","min_severity":"info","digest_enabled":true,"digest_interval_minutes":1}' >/dev/null

for _ in 1 2 3; do
  [ "$(wait_deployment "$(api POST "/applications/$AU/deploy" | jsonq "d['deployment_uuid']")" 240)" = "succeeded" ] || die "a deployment of the digest test failed"
done

# Nothing individual must reach the digest channel, even after a dispatch pass.
# 8s is several scheduler ticks: if the events were going to be sent one by one,
# they would have been by now.
sleep 8
SOLO=$(grep -c '"event_type": *"deployment.succeeded.v1"' "$WORKDIR/hooks-digest.jsonl" 2>/dev/null || true)
[ "${SOLO:-0}" -eq 0 ] || die "a digest rule must not send events one by one (got $SOLO)"

# Once the window elapses, one grouped message arrives — saying how many events
# it stands for.
DIGEST=0
for _ in $(seq 60); do
  DIGEST=$(grep -c 'notification.digest.v1' "$WORKDIR/hooks-digest.jsonl" 2>/dev/null || echo 0)
  [ "$DIGEST" -gt 0 ] && break
  sleep 3
done
[ "$DIGEST" -gt 0 ] || die "the digest was never flushed"
python3 - "$WORKDIR/hooks-digest.jsonl" <<'PYEOF' || die "the digest payload is wrong"
import json, sys
events = [json.loads(l) for l in open(sys.argv[1]) if l.strip()]
digests = [e for e in events if e.get("event_type") == "notification.digest.v1"]
assert digests, "no digest"
d = digests[-1]
assert d["payload"]["total"] >= 3, d["payload"]
assert d["payload"]["events"].get("deployment.succeeded.v1", 0) >= 3, d["payload"]
assert d["severity"] == "info", d["severity"]
PYEOF
ok "digest rule held the info events back and sent one grouped message"

# --- lifecycle -----------------------------------------------------------------
say "lifecycle stop/start"
[ "$(wait_job "$(api POST "/applications/$AU/stop" | jsonq "d['job_uuid']")")" = "succeeded" ] || die "stop failed"
[ "$(docker exec "$DIND_CTR" docker inspect --format '{{.State.Status}}' "$AU")" = "exited" ] || die "container not stopped"
[ "$(wait_job "$(api POST "/applications/$AU/start" | jsonq "d['job_uuid']")")" = "succeeded" ] || die "start failed"
[ "$(docker exec "$DIND_CTR" docker inspect --format '{{.State.Status}}' "$AU")" = "running" ] || die "container not restarted"
ok "stop/start converge desired and remote state"

# --- web terminal (§5.7, §24.4, ADR-024) ---------------------------------------------
# E2E-TERM-01/02. The suite speaks curl, and curl does not speak WebSocket —
# and no shell can assert on a PTY. wsprobe (scripts/e2e/wsprobe) is the
# client: it types, reads what the terminal printed, and reports how the
# server said the session ended.
say "terminal: a PTY in the container, bounded, audited, and killed with the socket"
WS_BASE="ws://127.0.0.1:${API_PORT}"
# Built once (like akerdock itself, and from the same working directory): `go
# run` on every call would pay the compile on each assertion.
go build -o "$WORKDIR/wsprobe" ./scripts/e2e/wsprobe || die "the terminal probe does not build"
wsprobe() { "$WORKDIR/wsprobe" "$@"; }

# The token is single-use and short-lived: it is minted by an authenticated,
# team-scoped operation and redeemed once on the socket.
TS=$(api POST "/applications/$AU/terminal-sessions")
TS_TOKEN=$(echo "$TS" | jsonq "d['token']")
TS_UUID=$(echo "$TS" | jsonq "d['uuid']")
[ "$(echo "$TS" | jsonq "d['target_kind']")" = "container" ] || die "the application terminal must target a container"
[ "$(echo "$TS" | jsonq "d['websocket_path']")" = "/terminal/ws" ] || die "unexpected websocket_path"

# A real PTY, not a pipe: `test -t 0` is a shell builtin, so it answers in
# busybox as in bash — and it answers the only question that matters here.
# nginx:alpine has no bash: the server falls back to sh, which is the point.
#
# The markers are SPLIT in the typed command (PTY"_"YES) but whole in the
# output: a pty echoes what you type, so a marker typed verbatim would match
# its own echo and the assertion would pass without the command ever running.
# That is exactly what happened the first time this test was written.
OUT=$(wsprobe -url "$WS_BASE/terminal/ws?token=$TS_TOKEN&cols=120&rows=40" \
  -send 'test -t 0 && echo PTY"_"YES || echo PTY"_"NO; echo TERMINAL"_"OK' \
  -expect 'TERMINAL_OK' -timeout 30s) \
  || die "the terminal session did not produce output"
grep -q 'PTY_YES' <<<"$OUT" || die "the remote side is not a pty (stdin is not a terminal): $OUT"
grep -q 'end: user_close' <<<"$OUT" || die "the server did not announce a clean close: $OUT"

# The row is closed with its reason, and BOTH open and close are audited
# (§23.4) — the keystrokes are not (§24.4).
for _ in $(seq 1 10); do
  TS_ROW=$(docker exec "$PG_CTR" psql -U postgres -d akerdock -tAc \
    "SELECT coalesce(end_reason::text,'') FROM terminal_sessions WHERE uuid = '$TS_UUID'")
  [ -n "$TS_ROW" ] && break
  sleep 1
done
[ "$TS_ROW" = "user_close" ] || die "the terminal session row was not closed with its reason (got '$TS_ROW')"
docker exec "$PG_CTR" psql -U postgres -d akerdock -tAc \
  "SELECT count(*) FROM audit_events WHERE action = 'terminal.open' AND target_uuid = '$TS_UUID'" | grep -q '^1$' \
  || die "the terminal opening was not audited"
docker exec "$PG_CTR" psql -U postgres -d akerdock -tAc \
  "SELECT count(*) FROM audit_events WHERE action = 'terminal.close' AND target_uuid = '$TS_UUID'" | grep -q '^1$' \
  || die "the terminal closing was not audited"
docker exec "$PG_CTR" psql -U postgres -d akerdock -tAc \
  "SELECT count(*) FROM audit_events WHERE coalesce(diff_redacted::text,'') LIKE '%TERMINAL_OK%'" | grep -q '^0$' \
  || die "keystrokes leaked into the audit trail (§24.4: they are never recorded)"
ok "PTY in the container, command executed, clean close audited with its reason"

# A token is single-use: replaying it authenticates nothing (§24.4).
REPLAY=$(curl -s -o /dev/null -w '%{http_code}' \
  -H 'Connection: Upgrade' -H 'Upgrade: websocket' -H 'Sec-WebSocket-Version: 13' \
  -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' \
  "http://127.0.0.1:${API_PORT}/terminal/ws?token=$TS_TOKEN")
[ "$REPLAY" = "401" ] || die "a replayed terminal token must be refused (got $REPLAY)"
NOTOKEN=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${API_PORT}/terminal/ws?token=akdt_nope")
[ "$NOTOKEN" = "401" ] || die "an unknown terminal token must be refused (got $NOTOKEN)"
ok "the attach token is single-use: a replay and a forged token are both refused"

# Idle timeout (AKERDOCK_TERMINAL_IDLE_TIMEOUT=8s here): a session nobody types
# in is closed by the SERVER, and the pty dies with it.
TS2_TOKEN=$(api POST "/applications/$AU/terminal-sessions" | jsonq "d['token']")
IDLE_OUT=$(wsprobe -url "$WS_BASE/terminal/ws?token=$TS2_TOKEN" -idle -timeout 30s) \
  || die "the idle session never ended on its own"
grep -q 'end: idle_timeout' <<<"$IDLE_OUT" || die "the idle session did not end with idle_timeout: $IDLE_OUT"
ok "an idle session is closed by the server (idle timeout), not left open"

# Guaranteed kill: the socket is yanked without a goodbye. The server must
# notice and reap the row — a session that survives its socket is a shell
# nobody is watching.
TS3=$(api POST "/applications/$AU/terminal-sessions")
TS3_TOKEN=$(echo "$TS3" | jsonq "d['token']")
TS3_UUID=$(echo "$TS3" | jsonq "d['uuid']")
wsprobe -url "$WS_BASE/terminal/ws?token=$TS3_TOKEN" -send 'echo DROP"_"ME' -expect 'DROP_ME' -close drop -timeout 30s >/dev/null \
  || die "the session to be dropped never opened"
for _ in $(seq 1 15); do
  TS3_END=$(docker exec "$PG_CTR" psql -U postgres -d akerdock -tAc \
    "SELECT coalesce(end_reason::text,'') FROM terminal_sessions WHERE uuid = '$TS3_UUID'")
  [ -n "$TS3_END" ] && break
  sleep 1
done
[ -n "$TS3_END" ] || die "a dropped connection left the session open forever"
ok "a dropped connection ends the session on the server ($TS3_END) — the pty never outlives its socket"

# E2E-TERM-02 — bounded to the active team (INV-002): another team's token sees
# a 404, exactly like a missing application. A read-only token cannot open one
# at all, and a server terminal is a ROOT terminal (rbac-matrix §5).
OTHER_TEAM_TOKEN="akd_$(openssl rand -hex 24)"
docker exec "$PG_CTR" psql -U postgres -d akerdock -c \
  "INSERT INTO teams (name) VALUES ('other-team')" >/dev/null
docker exec "$PG_CTR" psql -U postgres -d akerdock -c \
  "INSERT INTO api_tokens (team_id, name, token_prefix, token_hash, permissions) SELECT id, 'other', left('$OTHER_TEAM_TOKEN',10), encode(digest('$OTHER_TEAM_TOKEN','sha256'),'hex'), '{write}' FROM teams WHERE name = 'other-team'" >/dev/null
XTEAM=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $OTHER_TEAM_TOKEN" \
  "$B/applications/$AU/terminal-sessions")
[ "$XTEAM" = "404" ] || die "another team must get 404 on a terminal session, never 403 (got $XTEAM)"

RO_TOKEN="akd_$(openssl rand -hex 24)"
docker exec "$PG_CTR" psql -U postgres -d akerdock -c \
  "INSERT INTO api_tokens (team_id, name, token_prefix, token_hash, permissions) SELECT id, 'ro-term', left('$RO_TOKEN',10), encode(digest('$RO_TOKEN','sha256'),'hex'), '{read}' FROM teams WHERE name <> 'other-team' LIMIT 1" >/dev/null
RO=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $RO_TOKEN" \
  "$B/applications/$AU/terminal-sessions")
[ "$RO" = "403" ] || die "a read-only token must not open a terminal (got $RO)"

WRITE_TOKEN="akd_$(openssl rand -hex 24)"
docker exec "$PG_CTR" psql -U postgres -d akerdock -c \
  "INSERT INTO api_tokens (team_id, name, token_prefix, token_hash, permissions) SELECT id, 'w-term', left('$WRITE_TOKEN',10), encode(digest('$WRITE_TOKEN','sha256'),'hex'), '{write}' FROM teams WHERE name <> 'other-team' LIMIT 1" >/dev/null
SRV_TERM=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $WRITE_TOKEN" \
  "$B/servers/$S/terminal-sessions")
[ "$SRV_TERM" = "403" ] || die "a server terminal is a root terminal: a write token must be refused (got $SRV_TERM)"
ok "terminal bounded to the team (404), refused read-only (403), server shell gated on root (403)"

# The server shell itself: a root token passes the double control, and the
# session lands on the SERVER, not in a container.
SRV_TS=$(api POST "/servers/$S/terminal-sessions")
SRV_TOKEN=$(echo "$SRV_TS" | jsonq "d['token']")
[ "$(echo "$SRV_TS" | jsonq "d['target_kind']")" = "server" ] || die "the server terminal must target the server"
# It really is the docker HOST: the application's container is visible from
# there — which no application container could say of itself.
SRV_OUT=$(wsprobe -url "$WS_BASE/terminal/ws?token=$SRV_TOKEN" \
  -send "docker ps --format '{{.Names}}' | grep -q $AU && echo HOST\"_\"YES || echo HOST\"_\"NO" \
  -expect 'HOST_YES' -timeout 30s) \
  || die "the server shell did not see the application container — is it really on the host?"
grep -q 'end: user_close' <<<"$SRV_OUT" || die "the server shell did not close cleanly: $SRV_OUT"
ok "the server shell runs on the host (root terminal, double control satisfied)"

# --- pre/post-deployment commands (§10) ----------------------------------------------
say "pre/post-deployment hooks run in the right container, and a failing post never switches"
HOOK_BODY=$(python3 - "$PU" "$EU" "$S" <<'PYEOF'
import json, sys
print(json.dumps({
    "source_type": "docker_image", "name": "hookapp",
    "project_uuid": sys.argv[1], "environment_uuid": sys.argv[2], "server_uuid": sys.argv[3],
    "docker_image": "nginx", "docker_image_tag": "alpine", "ports_exposes": "80",
    # A health check is what makes a deployment ROLLING (§7.3) — that is the
    # path §10 is written for: a failing post hook must not switch, and the old
    # container must keep serving (INV-005). Without one, the deployment is a
    # stop-then-start replace and there is no candidate to compensate.
    "domains": ["hook.e2e.test"],
    "health_check": {"enabled": True, "path": "/", "interval_seconds": 2,
                     "timeout_seconds": 2, "retries": 3, "start_period_seconds": 1},
    # Both hooks from the start: the pre one must be SKIPPED on this first
    # deployment (nothing is running yet), which is what the check below asserts.
    # Quotes and metacharacters on purpose: the command is quoted, not sanitized.
    "pre_deployment_command": "touch /tmp/pre.txt",
    "post_deployment_command": "echo 'post ran' > /tmp/post.txt && echo \"$(hostname)\" >> /tmp/post.txt",
}))
PYEOF
)
AU9=$(api POST /applications "$HOOK_BODY" | jsonq "d['uuid']")
D_H1=$(api POST "/applications/$AU9/deploy" | jsonq "d['deployment_uuid']")
[ "$(wait_deployment "$D_H1" 240)" = "succeeded" ] || die "the deployment with a post hook failed"
# The post hook ran inside the container that is now serving.
docker exec "$DIND_CTR" docker exec "$AU9" cat /tmp/post.txt | grep -q 'post ran' || die "the post-deployment command did not run in the candidate"
# The pre hook was skipped on the first deployment: there was no container yet.
H1LOGS=$(api GET "/deployments/$D_H1/logs")
grep -q 'no running container' <<<"$H1LOGS" || die "the pre hook must be skipped (and traced) when nothing is running"

# Now the pre hook DOES have a container to run in, and the post hook FAILS.
HV=$(api GET "/applications/$AU9" | jsonq "d['version']")
curl -sf -X PATCH -H "Authorization: Bearer $ROOT_TOKEN" -H "If-Match: \"$HV\"" -H 'Content-Type: application/json' \
  -d '{"post_deployment_command":"exit 3"}' \
  "$B/applications/$AU9" >/dev/null || die "could not set the failing post hook"
OLD_CID=$(docker exec "$DIND_CTR" docker inspect --format '{{.Id}}' "$AU9")
D_H2=$(api POST "/applications/$AU9/deploy" | jsonq "d['deployment_uuid']")
[ "$(wait_deployment "$D_H2" 240)" = "failed" ] || die "a failing post-deployment command must fail the deployment"
# The old container is untouched and still serving (INV-005): no switch happened.
[ "$(docker exec "$DIND_CTR" docker inspect --format '{{.Id}}' "$AU9")" = "$OLD_CID" ] || die "the old container was replaced despite the failed post hook"
[ "$(docker exec "$DIND_CTR" docker inspect --format '{{.State.Running}}' "$AU9")" = "true" ] || die "the old container is not running any more (INV-005)"
# ...and the candidate was cleaned up (compensation C2).
docker exec "$DIND_CTR" docker inspect "${AU9}-next" >/dev/null 2>&1 && die "the candidate was left behind after the failed post hook"
# The pre hook DID run this time, in the existing container.
docker exec "$DIND_CTR" docker exec "$AU9" test -f /tmp/pre.txt || die "the pre-deployment command did not run in the existing container"
# The §10 guarantee needs a candidate, hence a health check: the combination
# "post hook without health check" is refused rather than silently degraded.
NOHC=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $ROOT_TOKEN" -H 'Content-Type: application/json' \
  -d "{\"source_type\":\"docker_image\",\"name\":\"nohc\",\"project_uuid\":\"$PU\",\"environment_uuid\":\"$EU\",\"server_uuid\":\"$S\",\"docker_image\":\"nginx\",\"docker_image_tag\":\"alpine\",\"ports_exposes\":\"80\",\"post_deployment_command\":\"true\"}" \
  "$B/applications")
[ "$NOHC" = "422" ] || die "a post-deployment command without a health check must be refused (got $NOHC)"
# ...and it cannot be introduced by a PATCH either (removing the health check
# under an existing hook would break the same guarantee).
HV2=$(api GET "/applications/$AU9" | jsonq "d['version']")
NOHC2=$(curl -s -o /dev/null -w '%{http_code}' -X PATCH -H "Authorization: Bearer $ROOT_TOKEN" -H "If-Match: \"$HV2\"" \
  -H 'Content-Type: application/json' -d '{"health_check":{"enabled":false}}' "$B/applications/$AU9")
[ "$NOHC2" = "422" ] || die "removing the health check under a post hook must be refused (got $NOHC2)"
ok "pre hook ran in the old container, failing post hook failed the deploy without switching (INV-005); the hook requires a health check"

# --- scheduled tasks (§192) ----------------------------------------------------------
say "a cron runs inside the container, and an occurrence that does NOT run says so"
TASK_BODY='{"name":"beat","command":"echo ran >> /tmp/beat.txt","cron_expression":"every_minute"}'
TU=$(api POST "/applications/$AU9/scheduled-tasks" "$TASK_BODY" | jsonq "d['uuid']")

# A manual run takes the same path as the cron trigger.
TJ=$(api POST "/scheduled-tasks/$TU/run" | jsonq "d['job_uuid']")
[ "$(wait_job "$TJ" 60)" = "succeeded" ] || die "the manual run of the scheduled task failed"
docker exec "$DIND_CTR" docker exec "$AU9" cat /tmp/beat.txt | grep -q ran || die "the command did not run inside the container"
api GET "/scheduled-tasks/$TU/executions" | jsonq "d['data'][0]['status']" | grep -q succeeded || die "the execution was not recorded as succeeded"
[ "$(api GET "/scheduled-tasks/$TU/executions" | jsonq "d['data'][0]['exit_code']")" = "0" ] || die "the exit code was not recorded"

# The scheduler fires it on its own — no API call involved. This is the whole
# point of a scheduled task, so it is proven, not assumed.
BEFORE_RUNS=$(api GET "/scheduled-tasks/$TU/executions" | jsonq "len(d['data'])")
FIRED=0
for _ in $(seq 1 75); do
  NOW_RUNS=$(api GET "/scheduled-tasks/$TU/executions" | jsonq "len(d['data'])")
  [ "$NOW_RUNS" -gt "$BEFORE_RUNS" ] && { FIRED=1; break; }
  sleep 1
done
[ "$FIRED" = "1" ] || die "the cron never fired on its own within 75s (every_minute)"
api GET "/scheduled-tasks/$TU" | jsonq "d['last_run_at']" | grep -q 20 || die "last_run_at was not recorded"

# A command that fails is a RESULT, not a job to retry: the history records the
# exit code, and the failure is announced (§290).
TU2=$(api POST "/applications/$AU9/scheduled-tasks" '{"name":"broken","command":"exit 3","cron_expression":"yearly"}' | jsonq "d['uuid']")
TJ2=$(api POST "/scheduled-tasks/$TU2/run" | jsonq "d['job_uuid']")
wait_job "$TJ2" 60 >/dev/null
FAILED=$(api GET "/scheduled-tasks/$TU2/executions" | jsonq "d['data'][0]")
printf '%s' "$FAILED" | grep -q "'status': 'failed'" || die "a failing command must be recorded as failed: $FAILED"
printf '%s' "$FAILED" | grep -q "'exit_code': 3" || die "the exit code 3 was not recorded: $FAILED"
docker exec "$PG_CTR" psql -U postgres -d akerdock -tAc \
  "SELECT count(*) FROM outbox_events WHERE event_type = 'scheduled_task.failed.v1'" | grep -qv '^0$' \
  || die "a failing scheduled task published no event — nobody would ever know"

# The overlap policy is enforced on the manual path too: a second way to start
# an overlapping run would defeat the policy that exists to prevent it.
TU3=$(api POST "/applications/$AU9/scheduled-tasks" '{"name":"slow","command":"sleep 30","cron_expression":"yearly"}' | jsonq "d['uuid']")
api POST "/scheduled-tasks/$TU3/run" >/dev/null
OVERLAP=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $ROOT_TOKEN" -H "Idempotency-Key: $(openssl rand -hex 8)" "$B/scheduled-tasks/$TU3/run")
[ "$OVERLAP" = "409" ] || die "a second run of an already-running task returned $OVERLAP, expected 409 (overlap_policy=skip)"

api DELETE "/scheduled-tasks/$TU" >/dev/null
[ "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $ROOT_TOKEN" "$B/scheduled-tasks/$TU")" = "404" ] || die "the deleted task is still readable"
ok "scheduled task executed by the scheduler, failure recorded and announced, overlap refused"

# --- telemetry (ADR-008) -------------------------------------------------------------
say "traces and metrics are exported over OTLP"
TR=0; ME=0
for _ in $(seq 30); do
  read -r TR ME < "$WORKDIR/otlp.count" 2>/dev/null || true
  [ "${TR:-0}" -gt 0 ] && [ "${ME:-0}" -gt 0 ] && break
  sleep 2
done
[ "${TR:-0}" -gt 0 ] || die "no trace was exported to the OTLP endpoint"
[ "${ME:-0}" -gt 0 ] || die "no metric was exported to the OTLP endpoint"
ok "OTLP export live ($TR trace batches, $ME metric batches)"

# The same instruments are also scrapable — one meter provider, two readers.
# The body is captured first: piping curl into `grep -q` under `set -o pipefail`
# fails even on a match, because grep closes the pipe and curl dies of SIGPIPE.
MCODE=$(curl -s -o "$WORKDIR/metrics.txt" -w '%{http_code}' "http://127.0.0.1:${API_PORT}/metrics")
[ "$MCODE" = "200" ] || die "/metrics answered HTTP $MCODE"
# grep on the FILE, never through a pipe: under `set -o pipefail`, `grep -q`
# closes the pipe on its first match and the writer dies of EPIPE, so a
# successful match reads as a failed pipeline.
grep -q 'akerdock_jobs_completed_total' "$WORKDIR/metrics.txt" || \
  die "/metrics does not expose the job counter — exposed: $(grep '^# TYPE' "$WORKDIR/metrics.txt" | tr '\n' ' ')"
grep -q 'job_type=' "$WORKDIR/metrics.txt" || die "/metrics lost the job type dimension"

# The audit trail says WHAT changed — without ever storing a secret (§23.4).
DIFF=$(docker exec "$PG_CTR" psql -U postgres -d akerdock -tAc \
  "SELECT diff_redacted FROM audit_events WHERE action = 'application.update' AND diff_redacted IS NOT NULL LIMIT 1")
[ -n "$DIFF" ] || die "no redacted diff was recorded on an application update"
grep -q '"from"' <<<"$DIFF" || die "the audit diff records no before/after"
# And no secret value ever reaches the audit table.
LEAK=$(docker exec "$PG_CTR" psql -U postgres -d akerdock -tAc \
  "SELECT count(*) FROM audit_events WHERE diff_redacted::text ILIKE '%BEGIN KEY%' OR diff_redacted::text ILIKE '%production%'")
[ "$LEAK" = "0" ] || die "a secret value leaked into the audit diff ($LEAK rows)"
ok "metrics scrapable on /metrics; audit diffs record what changed, never the secret"

# --- browser authentication (§698, INV-003) ------------------------------------------
say "the dashboard signs in with a cookie, and a cookie alone cannot be used to forge a write"
JAR="$WORKDIR/cookies.txt"; rm -f "$JAR"

# A wrong password is rejected — and the message must not distinguish it from an
# unknown email, or this endpoint becomes an account-enumeration oracle.
STATUS=$(curl -s -o "$WORKDIR/badlogin.json" -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
  -d '{"email":"e2e@example.com","password":"wrong"}' "http://127.0.0.1:$API_PORT/auth/login")
[ "$STATUS" = "401" ] || die "a wrong password should be refused with 401, got $STATUS"
UNKNOWN=$(curl -s -X POST -H 'Content-Type: application/json' \
  -d '{"email":"nobody@example.com","password":"wrong"}' "http://127.0.0.1:$API_PORT/auth/login" | jsonq "d['message']")
KNOWN=$(jsonq "d['message']" < "$WORKDIR/badlogin.json")
[ "$UNKNOWN" = "$KNOWN" ] || die "an unknown email answers differently from a wrong password — accounts are enumerable"

# The real login. The session lands in an HttpOnly cookie the page cannot read;
# the CSRF token lands in one it can.
curl -sf -c "$JAR" -X POST -H 'Content-Type: application/json' \
  -d '{"email":"e2e@example.com","password":"a-very-long-password"}' \
  "http://127.0.0.1:$API_PORT/auth/login" > "$WORKDIR/login.json" || die "the login with valid credentials failed"
grep -q 'HttpOnly_.*akerdock_session' "$JAR" || die "the session cookie is not HttpOnly — a single XSS would hand it over"
CSRF=$(awk '$6=="akerdock_csrf" {print $7}' "$JAR")
[ -n "$CSRF" ] || die "no CSRF cookie was set — the page has nothing to echo"

# The cookie authenticates a read of the same v1 API the tokens use.
curl -sf -b "$JAR" "$B/applications" >/dev/null || die "the session cookie does not authenticate an API read"

# A cookie rides along on requests OTHER sites trigger too. Without the echoed
# token, a write must be refused — that is the whole point of double-submit.
NOCSRF=$(curl -s -o /dev/null -w '%{http_code}' -b "$JAR" -X POST -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $(openssl rand -hex 8)" -d '{"name":"csrf-probe","type":"docker"}' "$B/projects")
[ "$NOCSRF" = "403" ] || die "a cookie-authenticated write without a CSRF token returned $NOCSRF, expected 403"

# A forged token is no better than none: the server compares against the cookie.
FORGED=$(curl -s -o /dev/null -w '%{http_code}' -b "$JAR" -X POST -H 'Content-Type: application/json' \
  -H "X-CSRF-Token: forged" -H "Idempotency-Key: $(openssl rand -hex 8)" \
  -d '{"name":"csrf-probe","type":"docker"}' "$B/projects")
[ "$FORGED" = "403" ] || die "a forged CSRF token was accepted ($FORGED)"

# With the token the page read from its own cookie, the write goes through.
OKW=$(curl -s -o /dev/null -w '%{http_code}' -b "$JAR" -X POST -H 'Content-Type: application/json' \
  -H "X-CSRF-Token: $CSRF" -H "Idempotency-Key: $(openssl rand -hex 8)" \
  -d '{"name":"session-project","description":"created from a browser session"}' "$B/projects")
[ "$OKW" = "201" ] || die "a cookie write with a valid CSRF token returned $OKW, expected 201"
# The dashboard is a first-class client of the same API: what it wrote through a
# cookie is what a bearer token reads back.
api GET /projects | grep -q 'session-project' || die "the project created from a browser session is invisible to the API"

# Logout revokes SERVER-SIDE. Replaying the very same cookie afterwards must
# fail: a logout that only clears the browser leaves a live session behind.
curl -sf -b "$JAR" -X POST -H "X-CSRF-Token: $CSRF" "http://127.0.0.1:$API_PORT/auth/logout" -o /dev/null \
  || die "the logout failed"
REPLAY=$(curl -s -o /dev/null -w '%{http_code}' -b "$JAR" "$B/applications")
[ "$REPLAY" = "401" ] || die "the session cookie still works after logout ($REPLAY) — the session was not revoked"

# Guessing is bounded: after MaxFailedLogins the account locks, so an online
# brute force runs out of attempts long before it runs out of passwords.
LOCKED=""
for _ in 1 2 3 4 5 6; do
  LOCKED=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
    -d '{"email":"e2e@example.com","password":"nope"}' "http://127.0.0.1:$API_PORT/auth/login")
done
[ "$LOCKED" = "429" ] || die "the account never locked after 6 failed logins (last status $LOCKED)"
# And the lockout is real: even the RIGHT password is refused while it holds.
STILL=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
  -d '{"email":"e2e@example.com","password":"a-very-long-password"}' "http://127.0.0.1:$API_PORT/auth/login")
[ "$STILL" = "429" ] || die "a locked account still accepted the correct password ($STILL)"
docker exec "$PG_CTR" psql -U postgres -d akerdock -q -c "UPDATE users SET failed_login_count = 0, locked_until = NULL"
ok "session login, HttpOnly cookie, double-submit CSRF, server-side logout and account lockout"

# --- rolling upgrade N-1 / N (§18.2, ADR-021) ----------------------------------------
say "the schema of version N still serves the binary of version N-1"
# An upgrade is a tag change: the new binary migrates, while the OLD one may
# still be running for a while. The migrations are expand-only (enforced by
# go test ./db), so the previous binary must keep working against the new
# schema. Proven here by running the PREVIOUS commit's binary against the
# database that the current migrations produced.
PREV=$(git -C "$ROOT_DIR" rev-parse HEAD~1 2>/dev/null || echo "")
if [ -n "$PREV" ] && git -C "$ROOT_DIR" cat-file -e "$PREV:cmd/akerdock/main.go" 2>/dev/null; then
  OLDSRC="$WORKDIR/old-src"
  rm -rf "$OLDSRC" && mkdir -p "$OLDSRC"
  git -C "$ROOT_DIR" archive "$PREV" | tar -x -C "$OLDSRC"
  if (cd "$OLDSRC" && go build -o "$WORKDIR/akerdock-old" ./cmd/akerdock 2>/dev/null); then
    kill -TERM "$API_PID" 2>/dev/null; sleep 2
    AKERDOCK_DATABASE_URL="postgres://postgres:test@127.0.0.1:${PG_PORT}/akerdock?sslmode=disable" \
    AKERDOCK_MASTER_KEY_FILE="$WORKDIR/master.key" AKERDOCK_DATA_DIR="$WORKDIR/data" \
    AKERDOCK_PORT="$API_PORT" AKERDOCK_LOG_FORMAT=text "$WORKDIR/akerdock-old" >> "$WORKDIR/api-old.log" 2>&1 &
    OLD_PID=$!
    OLD_OK=0
    for _ in $(seq 1 30); do curl -sf "$B/health" >/dev/null 2>&1 && { OLD_OK=1; break; }; sleep 1; done
    if [ "$OLD_OK" = "1" ]; then
      # It must not merely boot: it must READ the data written by version N.
      api GET /applications >/dev/null || die "the N-1 binary cannot read applications from the N schema"
      api GET /servers >/dev/null || die "the N-1 binary cannot read servers from the N schema"
      api GET /jobs >/dev/null || die "the N-1 binary cannot read jobs from the N schema"
      ok "the previous binary boots and serves against the migrated schema (expand-only upgrade)"
    else
      die "the N-1 binary did not become healthy against the N schema — the upgrade is not rolling-safe"
    fi
    kill -TERM "$OLD_PID" 2>/dev/null; sleep 2
    start_akerdock
  else
    say "skipping: the previous commit does not build (nothing to compare against yet)"
  fi
else
  say "skipping the N-1 upgrade check: no previous commit"
fi

ok "audit events recorded ($AUDIT_COUNT rows), outbox events published"
