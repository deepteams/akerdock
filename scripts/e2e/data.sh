# --- base application (fixture) ------------------------------------------------
# Deployed here rather than in the library: only the shards that observe a
# running application pay for it.
base_app

# --- managed PostgreSQL database (§6) -----------------------------------------------
say "managed PostgreSQL: provisioning, credentials, real connection, persistence"
DB_BODY=$(python3 - "$PU" "$EU" "$S" <<'PYEOF'
import json, sys
print(json.dumps({
    "name": "maindb", "project_uuid": sys.argv[1], "environment_uuid": sys.argv[2],
    "server_uuid": sys.argv[3], "image": "postgres:16-alpine",
    "postgres_user": "app", "postgres_db": "appdb", "instant_start": True,
    # Public: the external URL only exists for a database that is actually
    # reachable from outside, and the check below connects THROUGH it.
    "is_public": True,
}))
PYEOF
)
DBU=$(api POST /databases/postgresql "$DB_BODY" | jsonq "d['uuid']")
# the password is generated and never returned without read:sensitive
api GET "/databases/$DBU" | jsonq "d['is_redacted']" | grep -qi false || die "root token must see the credentials"
DBPASS=$(api GET "/databases/$DBU" | jsonq "d['postgres_password']")
[ ${#DBPASS} -ge 32 ] || die "the generated password is too short (${#DBPASS})"
api GET "/databases/$DBU" | jsonq "d['internal_url']" | grep -q "postgres://app:" || die "internal_url malformed"
# a token without read:sensitive gets nothing
RW_TOKEN="akd_$(openssl rand -hex 24)"
docker exec "$PG_CTR" psql -U postgres -d akerdock -q -c \
  "INSERT INTO api_tokens (team_id, name, token_prefix, token_hash, permissions) SELECT id, 'rw2', left('$RW_TOKEN',10), encode(digest('$RW_TOKEN','sha256'),'hex'), '{read,write}' FROM teams LIMIT 1"
LEAK=$(curl -s -H "Authorization: Bearer $RW_TOKEN" "$B/databases/$DBU" | jsonq "d.get('postgres_password') or d.get('internal_url') or 'absent'")
[ "$LEAK" = "absent" ] || die "credentials leaked without read:sensitive (INV-003)"
curl -s -H "Authorization: Bearer $RW_TOKEN" "$B/databases/$DBU" | jsonq "d['is_redacted']" | grep -qi true || die "is_redacted must be true without read:sensitive"

# wait for the provisioning job, then connect for real from inside the network
for _ in $(seq 1 90); do
  ST=$(api GET "/databases/$DBU" | jsonq "d['observed_status']")
  [ "$ST" = "healthy" ] && break
  sleep 2
done
[ "$ST" = "healthy" ] || die "the database never became healthy (status $ST)"
NET=$(docker exec "$DIND_CTR" docker inspect --format '{{range $k,$v := .NetworkSettings.Networks}}{{$k}}{{end}}' "$DBU")
docker exec "$DIND_CTR" docker run --rm --network "$NET" -e PGPASSWORD="$DBPASS" postgres:16-alpine \
  psql -h "$DBU" -U app -d appdb -c 'CREATE TABLE t (v text); INSERT INTO t VALUES (%s);' >/dev/null 2>&1 || \
docker exec "$DIND_CTR" docker run --rm --network "$NET" -e PGPASSWORD="$DBPASS" postgres:16-alpine \
  psql -h "$DBU" -U app -d appdb -c "CREATE TABLE t (v text)" -c "INSERT INTO t VALUES ('persisted')" >/dev/null || die "cannot connect to the managed database"

# The external URL is not decoration: it must connect. A published connection
# string that cannot connect is worse than none — it looks usable (§6.2).
EXT=$(api GET "/databases/$DBU" | jsonq "d['external_url']")
case "$EXT" in postgres://app:*) ;; *) die "external_url missing or malformed on a public database: $EXT";; esac
EXT_PORT=$(printf '%s' "$EXT" | sed -E 's|.*:([0-9]+)/.*|\1|')
docker exec "$DIND_CTR" docker run --rm --network host -e PGPASSWORD="$DBPASS" postgres:16-alpine \
  psql -h 127.0.0.1 -p "$EXT_PORT" -U app -d appdb -tAc "SELECT 1" | grep -q '^1$' \
  || die "the external_url advertises 127.0.0.1:$EXT_PORT, and nothing answers there"

# restart re-provisions the container: the data volume must survive
[ "$(wait_job "$(api POST "/databases/$DBU/restart" | jsonq "d['job_uuid']")" 180)" = "succeeded" ] || die "database restart failed"
sleep 3
docker exec "$DIND_CTR" docker run --rm --network "$NET" -e PGPASSWORD="$DBPASS" postgres:16-alpine \
  psql -h "$DBU" -U app -d appdb -tAc "SELECT v FROM t" | grep -q persisted || die "database data lost across a restart"

# stop/start converge the observed status
[ "$(wait_job "$(api POST "/databases/$DBU/stop" | jsonq "d['job_uuid']")")" = "succeeded" ] || die "database stop failed"
[ "$(docker exec "$DIND_CTR" docker inspect --format '{{.State.Status}}' "$DBU")" = "exited" ] || die "the database container is still running"
[ "$(wait_job "$(api POST "/databases/$DBU/start" | jsonq "d['job_uuid']")")" = "succeeded" ] || die "database start failed"
ok "database provisioned, credentials redacted per INV-003, data survived a restart"

# --- TCP proxy for a public database (§6.2, §2.6) ------------------------------------
say "a database exposed through the proxy never publishes a port of its own"
TCP_PORT=15433
TCPDB_BODY=$(python3 - "$PU" "$EU" "$S" "$TCP_PORT" <<'PYEOF'
import json, sys
print(json.dumps({
    "name": "tcpdb", "project_uuid": sys.argv[1], "environment_uuid": sys.argv[2],
    "server_uuid": sys.argv[3], "image": "postgres:16-alpine",
    "postgres_user": "tcp", "postgres_db": "tcpdb", "instant_start": True,
    "is_public": True, "public_access_mode": "tcp_proxy", "public_port": int(sys.argv[4]),
}))
PYEOF
)
TCPDB=$(api POST /databases/postgresql "$TCPDB_BODY" | jsonq "d['uuid']")
for _ in $(seq 1 90); do
  ST=$(api GET "/databases/$TCPDB" | jsonq "d['observed_status']")
  [ "$ST" = "healthy" ] && break
  sleep 2
done
[ "$ST" = "healthy" ] || die "the tcp-proxied database never became healthy ($ST)"

# The whole point: the database container publishes NOTHING. Its port lives on
# the proxy, so changing it never restarts the database and never drops a
# connection.
# `Ports` lists what the IMAGE exposes; what matters is whether anything is
# BOUND to a host port — that is what a restart would be needed to change.
BOUND=$(docker exec "$DIND_CTR" docker inspect --format '{{range $p, $conf := .NetworkSettings.Ports}}{{if $conf}}{{$p}}{{end}}{{end}}' "$TCPDB")
[ -z "$BOUND" ] || die "the tcp-proxied database bound a host port on its own container: $BOUND"
docker exec "$DIND_CTR" test -f "/data/akerdock/proxy/dynamic/${TCPDB}.yaml" || die "the TCP route file was not written"
docker exec "$DIND_CTR" grep -q "tcp-${TCP_PORT}" /data/akerdock/proxy/traefik.yaml \
  || die "the TCP entrypoint is not declared in the static config"
docker exec "$DIND_CTR" docker inspect akerdock-proxy --format '{{.NetworkSettings.Ports}}' | grep -q "${TCP_PORT}" \
  || die "the proxy does not listen on the TCP port"

# And it actually serves: a connection through the proxy reaches the database.
TCPPASS=$(api GET "/databases/$TCPDB" | jsonq "d['postgres_password']")
docker exec "$DIND_CTR" docker run --rm --network host -e PGPASSWORD="$TCPPASS" postgres:16-alpine \
  psql -h 127.0.0.1 -p "$TCP_PORT" -U tcp -d tcpdb -tAc "SELECT 1" | grep -q '^1$' \
  || die "nothing answers through the TCP proxy on port $TCP_PORT"
ok "database routed through the TCP proxy: no port on its container, reachable through the proxy"

# --- database TLS with the server CA (§6.3) ------------------------------------------
say "a database served over TLS, with a certificate a client can actually verify"
# Before any TLS database, the server has no CA: that is a state, not a failure.
api GET "/servers/$S/ca" | jsonq "str(d['ca_cert'])" | grep -q None || die "the server already had a CA before any TLS database"

SSLDB_BODY=$(python3 - "$PU" "$EU" "$S" <<'PYEOF'
import json, sys
print(json.dumps({
    "name": "ssldb", "project_uuid": sys.argv[1], "environment_uuid": sys.argv[2],
    "server_uuid": sys.argv[3], "image": "postgres:16-alpine",
    "postgres_user": "sec", "postgres_db": "secdb", "instant_start": True,
    "ssl_enabled": True, "ssl_mode": "verify-ca",
}))
PYEOF
)
SSLDB=$(api POST /databases/postgresql "$SSLDB_BODY" | jsonq "d['uuid']")
for _ in $(seq 1 90); do
  ST=$(api GET "/databases/$SSLDB" | jsonq "d['observed_status']")
  [ "$ST" = "healthy" ] && break
  sleep 2
done
[ "$ST" = "healthy" ] || die "the TLS database never became healthy ($ST)"

# The CA now exists and is readable: it is what a client needs to VERIFY. Its
# private key is another matter — it never leaves the control plane.
api GET "/servers/$S/ca" | jsonq "d['ca_cert']" | grep -q "BEGIN CERTIFICATE" || die "the server CA is not exposed"
api GET "/servers/$S/ca" | grep -q "PRIVATE KEY" && die "the CA private key was returned by the API (INV-003)"
docker exec "$PG_CTR" psql -U postgres -d akerdock -tAc "SELECT count(*) FROM servers WHERE ca_key_enc IS NOT NULL" | grep -qv '^0$' \
  || die "the CA private key was not stored encrypted"

# And the proof: a client that VERIFIES the chain connects. A TLS nobody
# verifies protects against nothing, so sslmode=require would prove very little.
api GET "/servers/$S/ca" | jsonq "d['ca_cert']" > "$WORKDIR/ca.crt"
# Through stdin, not `docker cp`: the mount below is resolved by the DinD's own
# daemon, and a source path it cannot find is silently created as a DIRECTORY —
# which then fails as "no certificate found", far from the cause.
docker exec -i "$DIND_CTR" sh -c 'cat > /tmp/ca.crt' < "$WORKDIR/ca.crt"
docker exec "$DIND_CTR" grep -q "BEGIN CERTIFICATE" /tmp/ca.crt || die "the CA did not reach the client machine"
SSLPASS=$(api GET "/databases/$SSLDB" | jsonq "d['postgres_password']")
SSLNET=$(docker exec "$DIND_CTR" docker inspect --format '{{range $k,$v := .NetworkSettings.Networks}}{{$k}}{{end}}' "$SSLDB")
SSLOUT=$(docker exec "$DIND_CTR" docker run --rm --network "$SSLNET" -v /tmp/ca.crt:/ca.crt:ro \
  -e PGPASSWORD="$SSLPASS" -e PGSSLMODE=verify-ca -e PGSSLROOTCERT=/ca.crt postgres:16-alpine \
  psql -h "$SSLDB" -U sec -d secdb -tAc "SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()" 2>&1 || true)
printf '%s' "$SSLOUT" | grep -q '^t$' \
  || die "the connection is not TLS, or the certificate does not verify against the server CA: $SSLOUT"
ok "database TLS: certificate signed by the server CA, verified by the client, private key never exposed"

# --- backup and restore (§7, ADR-014) ------------------------------------------------
say "database backup, checksum, and restore of lost data"
PLAN=$(api POST "/databases/$DBU/backups" '{"frequency":"daily","local_retention":{"max_count":3}}' | jsonq "d['uuid']")
BADCRON=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $ROOT_TOKEN" -H 'Content-Type: application/json' -d '{"frequency":"not a cron"}' "$B/databases/$DBU/backups")
[ "$BADCRON" = "422" ] || die "an invalid cron must be rejected (got $BADCRON)"

# the data to protect is the table created during the database check
[ "$(wait_job "$(api POST "/databases/$DBU/backups/$PLAN/execute" | jsonq "d['job_uuid']")" 180)" = "succeeded" ] || die "backup execution failed"
EXEC=$(api GET "/databases/$DBU/backups/$PLAN/executions" | jsonq "json.dumps(d['data'][0])")
echo "$EXEC" | jsonq "d['status']" | grep -q succeeded || die "the backup did not succeed"
CHK=$(echo "$EXEC" | jsonq "d['checksum']"); [ ${#CHK} -eq 64 ] || die "missing backup checksum"
SIZE=$(echo "$EXEC" | jsonq "d['size_bytes']"); [ "$SIZE" -gt 100 ] || die "the dump looks empty ($SIZE bytes)"
EXEC_UUID=$(echo "$EXEC" | jsonq "d['uuid']")
FILE=$(echo "$EXEC" | jsonq "d['filename']")

# destroy the data, then restore it from the backup
docker exec "$DIND_CTR" docker run --rm --network "$NET" -e PGPASSWORD="$DBPASS" postgres:16-alpine \
  psql -h "$DBU" -U app -d appdb -c "DROP TABLE t" >/dev/null || die "could not drop the table"
# The contract (§20.5) requires confirm=true — a bodyless restore is a 422.
CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $ROOT_TOKEN" \
  -H 'Content-Type: application/json' -d '{"confirm":false}' \
  "$B/databases/$DBU/backups/$PLAN/executions/$EXEC_UUID/restore")
[ "$CODE" = "422" ] || die "a restore without confirm=true must be refused (got $CODE)"
[ "$(wait_job "$(api POST "/databases/$DBU/backups/$PLAN/executions/$EXEC_UUID/restore" '{"confirm":true}' | jsonq "d['job_uuid']")" 180)" = "succeeded" ] || die "restore failed"
docker exec "$DIND_CTR" docker run --rm --network "$NET" -e PGPASSWORD="$DBPASS" postgres:16-alpine \
  psql -h "$DBU" -U app -d appdb -tAc "SELECT v FROM t" | grep -q persisted || die "the restore did not bring the data back"

# A restore drill (ADR-014): the dump is restored into a DISPOSABLE database,
# its content is recounted, and the copy is destroyed. A backup that has never
# been restored is a file, not a backup.
DRJ=$(api POST "/databases/$DBU/backups/$PLAN/drill" | jsonq "d['job_uuid']")
[ "$(wait_job "$DRJ" 240)" = "succeeded" ] || die "the restore drill job failed"
DRILL=$(api GET "/databases/$DBU/backups/$PLAN/drills" | jsonq "json.dumps(d['data'][0])")
echo "$DRILL" | jsonq "d['status']" | grep -q succeeded || die "the drill did not succeed: $DRILL"
DT_EXP=$(echo "$DRILL" | jsonq "d['tables_expected']")
DT_GOT=$(echo "$DRILL" | jsonq "d['tables_restored']")
[ "$DT_GOT" = "$DT_EXP" ] && [ "$DT_GOT" -ge 1 ] || die "the drill restored $DT_GOT tables, expected $DT_EXP"
# The disposable database is destroyed — including its data. Leaving it behind
# would leave a copy of production on the server.
docker exec "$DIND_CTR" docker ps -a --format '{{.Names}}' | grep -q akerdock-drill && die "the drill container was left behind"
api GET "/databases/$DBU/backups/$PLAN" | jsonq "d['last_drill_status']" | grep -q succeeded || die "the plan does not record the drill result"

# a corrupted dump is never restored (§20.5)
docker exec "$DIND_CTR" sh -c "echo corruption >> $FILE"
RJ=$(api POST "/databases/$DBU/backups/$PLAN/executions/$EXEC_UUID/restore" '{"confirm":true}' | jsonq "d['job_uuid']")
[ "$(wait_job "$RJ" 120)" = "dead_letter" ] || die "a corrupted dump must not be restored"
api GET "/jobs/$RJ" | jsonq "str(d['steps'])" | grep -qi checksum || die "the failure must name the checksum mismatch"

# And the drill catches it too — LOUDLY. This is the failure mode the whole
# feature exists for: backups that stay green for months and restore into
# nothing. A drill that failed must be recorded AND announced.
DRJ2=$(api POST "/databases/$DBU/backups/$PLAN/drill" | jsonq "d['job_uuid']")
wait_job "$DRJ2" 240 >/dev/null
api GET "/databases/$DBU/backups/$PLAN/drills" | jsonq "d['data'][0]['status']" | grep -q failed \
  || die "a drill on a corrupted dump must fail"
api GET "/databases/$DBU/backups/$PLAN" | jsonq "d['last_drill_status']" | grep -q failed || die "the plan does not record the failed drill"
docker exec "$PG_CTR" psql -U postgres -d akerdock -tAc \
  "SELECT count(*) FROM outbox_events WHERE event_type = 'backup.drill_failed.v1'" | grep -qv '^0$' \
  || die "a failed drill published no event — a backup nobody can restore would stay green"
ok "backup created (${SIZE}B, checksum verified), data restored, corrupted dump refused, restore drill run and its failure announced"

# --- scheduled backups: the cron actually fires (§7.1) -------------------------------
say "the scheduler fires a backup plan on its cron occurrence"
# An unknown timezone must never reach the scheduler, where it would stall the plan.
BADTZ=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $ROOT_TOKEN" -H 'Content-Type: application/json' \
  -d '{"frequency":"daily","timezone":"Middle/Earth"}' "$B/databases/$DBU/backups")
[ "$BADTZ" = "422" ] || die "an unknown timezone must be rejected (got $BADTZ)"

CRONPLAN=$(api POST "/databases/$DBU/backups" '{"frequency":"every_minute","local_retention":{"max_count":2}}' | jsonq "d['uuid']")
# The scheduler owns next_run_at: it seeds it on its first pass (30s tick),
# without firing — the first backup is the first occurrence.
NEXT=""
for _ in $(seq 40); do
  NEXT=$(api GET "/databases/$DBU/backups/$CRONPLAN" | jsonq "d.get('next_run_at') or ''")
  [ -n "$NEXT" ] && break
  sleep 2
done
[ -n "$NEXT" ] || die "the scheduler never computed next_run_at for the plan"

# Then the occurrence passes and an execution appears — nobody called /execute.
AUTOEXEC=""
for _ in $(seq 60); do
  AUTOEXEC=$(api GET "/databases/$DBU/backups/$CRONPLAN/executions" | jsonq "d['data'][0]['status'] if d['data'] else ''")
  [ "$AUTOEXEC" = "succeeded" ] && break
  sleep 3
done
[ "$AUTOEXEC" = "succeeded" ] || die "the cron occurrence never produced a backup (status: ${AUTOEXEC:-none})"
api GET "/databases/$DBU/backups/$CRONPLAN" | jsonq "d['next_run_at']" >/dev/null || die "the window was not advanced"
# Leave no every-minute plan behind: it would keep firing under the rest of
# the suite.
api DELETE "/databases/$DBU/backups/$CRONPLAN" >/dev/null
ok "scheduled plan: next_run_at seeded, cron occurrence fired a backup unattended"

# --- backups to S3 (§7.2) ------------------------------------------------------------
say "uploading a backup to an S3 bucket and restoring from it"
docker exec "$DIND_CTR" sh -c '
  set -e
  docker run -d --name minio -p 9000:9000 \
    -e MINIO_ROOT_USER=akerdock -e MINIO_ROOT_PASSWORD=akerdock-secret \
    minio/minio:latest server /data >/dev/null
  for _ in $(seq 30); do curl -sf http://127.0.0.1:9000/minio/health/live >/dev/null 2>&1 && break; sleep 1; done
  docker run --rm --network host --entrypoint sh minio/mc:latest -c "
    mc alias set m http://127.0.0.1:9000 akerdock akerdock-secret >/dev/null &&
    mc mb -p m/backups >/dev/null"
' >/dev/null 2>&1 || die "MinIO setup failed"

# The same endpoint answers from the host (where akerdock signs) and from the
# DinD (where curl uploads) — see the port publishing above. A SigV4 signature
# covers the Host header, so one bucket must have one address.
S3=$(api POST /s3-storages "{\"name\":\"minio\",\"endpoint\":\"http://127.0.0.1:${MINIO_PORT}\",\"bucket\":\"backups\",\"path_prefix\":\"akerdock\",\"access_key\":\"akerdock\",\"secret_key\":\"akerdock-secret\"}")
S3U=$(echo "$S3" | jsonq "d['uuid']")
[ "$(echo "$S3" | jsonq "d['is_usable']")" = "True" ] || die "the write/read/delete round trip did not pass: $(echo "$S3" | jsonq "d.get('last_check_error')")"
echo "$S3" | grep -qi 'akerdock-secret' && die "the secret key leaked into the response (INV-003)"

# Wrong credentials are recorded as unusable with a reason — never accepted.
BAD=$(api POST /s3-storages "{\"name\":\"broken\",\"endpoint\":\"http://127.0.0.1:${MINIO_PORT}\",\"bucket\":\"backups\",\"access_key\":\"wrong\",\"secret_key\":\"wrongsecret\"}")
BADU=$(echo "$BAD" | jsonq "d['uuid']")
[ "$(echo "$BAD" | jsonq "d['is_usable']")" = "False" ] || die "a storage with wrong credentials must not be usable"
[ -n "$(echo "$BAD" | jsonq "d['last_check_error'] or ''")" ] || die "an unusable storage must carry the reason"
BADPLAN=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $ROOT_TOKEN" -H 'Content-Type: application/json' \
  -d "{\"frequency\":\"daily\",\"save_s3\":true,\"s3_storage_uuid\":\"$BADU\"}" "$B/databases/$DBU/backups")
[ "$BADPLAN" = "422" ] || die "a plan must not reference an unusable storage (got $BADPLAN)"
api DELETE "/s3-storages/$BADU" >/dev/null

# s3_only: the dump is uploaded, then dropped from the server.
S3PLAN=$(api POST "/databases/$DBU/backups" "{\"frequency\":\"daily\",\"save_s3\":true,\"s3_only\":true,\"s3_storage_uuid\":\"$S3U\"}" | jsonq "d['uuid']")
[ "$(wait_job "$(api POST "/databases/$DBU/backups/$S3PLAN/execute" | jsonq "d['job_uuid']")" 180)" = "succeeded" ] || die "the S3 backup failed"
S3EXEC=$(api GET "/databases/$DBU/backups/$S3PLAN/executions" | jsonq "json.dumps(d['data'][0])")
[ "$(echo "$S3EXEC" | jsonq "d['status']")" = "succeeded" ] || die "the S3 backup is not succeeded: $(echo "$S3EXEC" | jsonq "d.get('message')")"
[ "$(echo "$S3EXEC" | jsonq "d['s3_uploaded']")" = "True" ] || die "the backup was not marked as uploaded"
[ "$(echo "$S3EXEC" | jsonq "d['local_available']")" = "False" ] || die "s3_only must not keep a local copy"
S3EXEC_UUID=$(echo "$S3EXEC" | jsonq "d['uuid']")
docker exec "$DIND_CTR" docker run --rm --network host --entrypoint sh minio/mc:latest -c \
  "mc alias set m http://127.0.0.1:9000 akerdock akerdock-secret >/dev/null && mc ls --recursive m/backups/akerdock" | grep -q 'sql.gz' \
  || die "no dump found in the bucket"

# Restoring pulls the object back down, verifies its checksum, and replays it.
docker exec "$DIND_CTR" docker run --rm --network "$NET" -e PGPASSWORD="$DBPASS" postgres:16-alpine \
  psql -h "$DBU" -U app -d appdb -c "DROP TABLE t" >/dev/null || die "could not drop the table"
[ "$(wait_job "$(api POST "/databases/$DBU/backups/$S3PLAN/executions/$S3EXEC_UUID/restore" '{"confirm":true}' | jsonq "d['job_uuid']")" 180)" = "succeeded" ] || die "the restore from S3 failed"
docker exec "$DIND_CTR" docker run --rm --network "$NET" -e PGPASSWORD="$DBPASS" postgres:16-alpine \
  psql -h "$DBU" -U app -d appdb -tAc "SELECT v FROM t" | grep -q persisted || die "the S3 restore did not bring the data back"

# Retention purges the bucket too: with max_count=1, a second backup must
# leave exactly one object behind — and never the last successful one.
RPLAN=$(api POST "/databases/$DBU/backups" "{\"frequency\":\"daily\",\"save_s3\":true,\"s3_storage_uuid\":\"$S3U\",\"s3_retention\":{\"max_count\":1}}" | jsonq "d['uuid']")
for _ in 1 2; do
  [ "$(wait_job "$(api POST "/databases/$DBU/backups/$RPLAN/execute" | jsonq "d['job_uuid']")" 180)" = "succeeded" ] || die "a backup of the retention plan failed"
  sleep 1  # the object key carries a second-resolution timestamp
done
OBJECTS=$(docker exec "$DIND_CTR" docker run --rm --network host --entrypoint sh minio/mc:latest -c \
  "mc alias set m http://127.0.0.1:9000 akerdock akerdock-secret >/dev/null && mc ls --recursive m/backups/akerdock/$DBU" | grep -c 'sql.gz' || true)
# 1 kept by this plan + 1 from the s3_only plan above.
[ "$OBJECTS" = "2" ] || die "S3 retention kept $OBJECTS objects, expected 2"

# A storage still referenced by a plan cannot be deleted (§19.2).
S3DEL=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE -H "Authorization: Bearer $ROOT_TOKEN" "$B/s3-storages/$S3U")
[ "$S3DEL" = "409" ] || die "an S3 storage used by a plan must not be deletable (got $S3DEL)"
ok "backup uploaded to S3, local copy dropped (s3_only), restored from the bucket, retention purged the old object"

# --- invitations, server inventory, encryption rotation ------------------------------
say "invitations, server inventory, and master key rotation"
TEAM_UUID=$(api GET /teams | jsonq "d['data'][0]['uuid']")
INV=$(api POST "/teams/$TEAM_UUID/invitations" '{"email":"guest@example.com","role":"member"}')
echo "$INV" | jsonq "d['status']" | grep -q pending || die "the invitation is not pending"
echo "$INV" | jsonq "d['invite_url']" | grep -q 'token=' || die "the invite link must be returned when email is not configured"
INV_UUID=$(echo "$INV" | jsonq "d['uuid']")
# the link is a credential: only its hash is stored
docker exec "$PG_CTR" psql -U postgres -d akerdock -tAc "SELECT length(token_hash) FROM invitations LIMIT 1" | grep -q '^64$' || die "the invite token must be stored hashed"
DUP=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $ROOT_TOKEN" -H 'Content-Type: application/json' -d '{"email":"guest@example.com"}' "$B/teams/$TEAM_UUID/invitations")
[ "$DUP" = "409" ] || die "a duplicate active invitation must conflict (got $DUP)"
[ "$(curl -s -o /dev/null -w '%{http_code}' -X DELETE -H "Authorization: Bearer $ROOT_TOKEN" "$B/teams/$TEAM_UUID/invitations/$INV_UUID")" = "204" ] || die "revoke failed"
api GET "/teams/$TEAM_UUID/invitations" | jsonq "d['data'][0]['status']" | grep -q revoked || die "the invitation is not revoked"

# server inventory: managed resources and routed domains
api GET "/servers/$S/resources" | jsonq "len(d['data'])" | grep -qv '^0$' || die "the server inventory is empty"
api GET "/servers/$S/domains" | jsonq "str(d['data'])" | grep -q 'e2e.test' || die "the routed domains are missing"

# master key rotation: add a v2 key, restart, rotate, and check convergence
BEFORE=$(api GET /system/encryption | jsonq "d['active_key_version']")
[ "$BEFORE" = "1" ] || die "the active key version should be 1 (got $BEFORE)"
python3 -c "import base64,os; print('2:'+base64.b64encode(os.urandom(32)).decode())" >> "$WORKDIR/master.key"
kill -TERM "$API_PID" 2>/dev/null; sleep 2
start_akerdock
[ "$(api GET /system/encryption | jsonq "d['active_key_version']")" = "2" ] || die "the new key version is not active"
# rows still carry v1 before the rotation
api GET /system/encryption | jsonq "[v['key_version'] for v in d['key_versions']]" | grep -q 1 || die "no rows on the old version?"
[ "$(wait_job "$(api POST /system/encryption/rotate | jsonq "d['job_uuid']")" 180)" = "succeeded" ] || die "the rotation job failed"
VERSIONS=$(api GET /system/encryption | jsonq "sorted(v['key_version'] for v in d['key_versions'])")
[ "$VERSIONS" = "[2]" ] || die "the rotation did not converge (versions still referenced: $VERSIONS)"
# and the re-encrypted secrets are still readable with the new key
AFTER_PASS=$(api GET "/databases/$DBU" | jsonq "d['postgres_password']")
[ "$AFTER_PASS" = "$DBPASS" ] || die "a secret became unreadable after the rotation"
ok "invitation hashed and revoked, server inventory served, key rotated to v2 (all rows converged)"

# --- safe deletion --------------------------------------------------------------
say "safe deletion (routing first, then workloads)"
STATUS=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE -H "Authorization: Bearer $ROOT_TOKEN" "$B/projects/$PU/environments/$EU")
[ "$STATUS" = "409" ] || die "environment with resources must refuse deletion (got $STATUS)"
# Every application the suite created, read back from the API rather than from a
# hand-kept list: the check below asserts that NO application container is left,
# so a list that drifts by one app fails far from its cause.
for app in $(api GET /applications | jsonq "' '.join(a['uuid'] for a in d['data'])"); do
  [ "$(wait_job "$(api DELETE "/applications/$app" | jsonq "d['job_uuid']")")" = "succeeded" ] || die "deletion of $app failed"
done
# A managed database is a resource too: it blocks the environment until removed.
# Read back from the API for the same reason as the applications above.
for db in $(api GET /databases | jsonq "' '.join(x['uuid'] for x in d['data'])"); do
  [ "$(wait_job "$(api DELETE "/databases/$db" | jsonq "d['job_uuid']")" 120)" = "succeeded" ] || die "deletion of database $db failed"
done
docker exec "$DIND_CTR" docker volume inspect "${DBU}_data" >/dev/null 2>&1 || die "the database volume was destroyed without an explicit request (INV-008)"
docker exec "$DIND_CTR" sh -c "! ls /data/akerdock/proxy/dynamic/${AU}.yaml 2>/dev/null" || die "routing file not removed"
[ "$(docker exec "$DIND_CTR" docker ps -q --filter label=akerdock.type=application | wc -l | tr -d ' ')" = "0" ] || die "app containers still present"
STATUS=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE -H "Authorization: Bearer $ROOT_TOKEN" "$B/projects/$PU/environments/$EU")
[ "$STATUS" = "204" ] || die "empty environment must delete (got $STATUS)"
ok "deletion removed routing, workloads and tombstoned resources"
