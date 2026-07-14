# --- git build pack ------------------------------------------------------------
say "deploying a git application (shallow fetch at exact SHA + remote build)"
# Serve a tiny git repo over git:// from inside the DinD network so the
# target can clone it without external access.
docker exec "$DIND_CTR" sh -c '
  set -e
  apk add --no-cache git git-daemon >/dev/null 2>&1
  rm -rf /srv/repo && mkdir -p /srv/repo && cd /srv/repo && git init -q
  git config user.email e2e@example.com && git config user.name e2e
  printf "FROM nginx:alpine\nRUN echo git-built-app > /usr/share/nginx/html/index.html\n" > Dockerfile
  git add -A && git commit -q -m init
  git daemon --base-path=/srv --export-all --reuseaddr --detach /srv
' || die "git repo setup failed"
GIT_HOST=$(docker exec "$DIND_CTR" hostname -i | awk '{print $1}')
GIT_BODY=$(python3 - "$PU" "$EU" "$S" "$GIT_HOST" <<'PYEOF'
import json, sys
print(json.dumps({
    "source_type": "git", "name": "gitapp",
    "project_uuid": sys.argv[1], "environment_uuid": sys.argv[2], "server_uuid": sys.argv[3],
    "git_repository": f"git://{sys.argv[4]}/repo", "git_branch": "master", "build_pack": "dockerfile",
    "domains": ["git.e2e.test"], "ports_exposes": "80",
}))
PYEOF
)
AU3=$(api POST /applications "$GIT_BODY" | jsonq "d['uuid']")
DU3=$(api POST "/applications/$AU3/deploy" | jsonq "d['deployment_uuid']")
[ "$(wait_deployment "$DU3" 240)" = "succeeded" ] || die "git deployment failed"
SHA=$(api GET "/deployments/$DU3" | jsonq "d.get('commit_sha') or ''")
[ -n "$SHA" ] || die "commit SHA was not resolved"
wait_route git.e2e.test 301
docker exec "$DIND_CTR" curl -sk --resolve git.e2e.test:443:127.0.0.1 https://git.e2e.test/ | grep -q 'git-built-app' || die "git app not serving its built content"
ok "git app cloned at resolved SHA ${SHA:0:12}, built remotely and routed"

# --- private repository via deploy key (§5.1) ---------------------------------------
say "cloning a private repository with a deploy key"
# A bare repo reachable only over SSH, authorized by a key AkerDock holds.
ssh-keygen -t ed25519 -N '' -C deploy -f "$WORKDIR/deploykey" -q
DEPLOY_PUB=$(cat "$WORKDIR/deploykey.pub")
docker exec "$DIND_CTR" sh -c "
  set -e
  apk add --no-cache openssh-client >/dev/null 2>&1
  printf '%s\n' '$DEPLOY_PUB' >> /root/.ssh/authorized_keys
  rm -rf /srv/private.git /tmp/priv && mkdir -p /tmp/priv && cd /tmp/priv && git init -q
  git config user.email e2e@example.com && git config user.name e2e
  printf 'FROM nginx:alpine\nRUN echo private-repo-app > /usr/share/nginx/html/index.html\n' > Dockerfile
  git add -A && git commit -q -m init
  git clone -q --bare /tmp/priv /srv/private.git
" || die "private repo setup failed"

# The repository is genuinely unreachable without the key: anonymous git:// is
# not serving it (git-daemon exports /srv, but the clone below goes over SSH).
DKEY_JSON=$(python3 -c "import json,sys; print(json.dumps(open(sys.argv[1]).read()))" "$WORKDIR/deploykey")
DK=$(api POST /private-keys "{\"name\":\"deploy\",\"private_key\":$DKEY_JSON}" | jsonq "d['uuid']")

# An SSH URL without a deploy key must be refused, not attempted (INV-003).
NOKEY=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $ROOT_TOKEN" -H 'Content-Type: application/json' \
  -d "{\"source_type\":\"git\",\"name\":\"nokey\",\"project_uuid\":\"$PU\",\"environment_uuid\":\"$EU\",\"server_uuid\":\"$S\",\"git_repository\":\"ssh://root@$GIT_HOST/srv/private.git\",\"git_branch\":\"master\",\"build_pack\":\"dockerfile\",\"ports_exposes\":\"80\"}" \
  "$B/applications")
[ "$NOKEY" = "422" ] || die "an SSH repository without a deploy key must be rejected (got $NOKEY)"

PRIV_BODY=$(python3 - "$PU" "$EU" "$S" "$GIT_HOST" "$DK" <<'PYEOF'
import json, sys
print(json.dumps({
    "source_type": "git", "name": "privapp",
    "project_uuid": sys.argv[1], "environment_uuid": sys.argv[2], "server_uuid": sys.argv[3],
    "git_repository": f"ssh://root@{sys.argv[4]}/srv/private.git", "git_branch": "master",
    "build_pack": "dockerfile", "private_key_uuid": sys.argv[5],
    "domains": ["priv.e2e.test"], "ports_exposes": "80",
}))
PYEOF
)
AU6=$(api POST /applications "$PRIV_BODY" | jsonq "d['uuid']")
[ "$(api GET "/applications/$AU6" | jsonq "d['private_key_uuid']")" = "$DK" ] || die "the deploy key is not reported on the application"
DU6=$(api POST "/applications/$AU6/deploy" | jsonq "d['deployment_uuid']")
[ "$(wait_deployment "$DU6" 240)" = "succeeded" ] || die "private repository deployment failed"
wait_route priv.e2e.test 301
docker exec "$DIND_CTR" curl -sk --resolve priv.e2e.test:443:127.0.0.1 https://priv.e2e.test/ | grep -q 'private-repo-app' || die "the private app is not serving its built content"

# The key never lingers on the build server (INV-003).
LEFTOVER=$(docker exec "$DIND_CTR" sh -c "ls /data/akerdock/applications/$AU6/keys 2>/dev/null | wc -l" | tr -d ' ')
[ "$LEFTOVER" = "0" ] || die "the deploy key was left on the server after the clone ($LEFTOVER files)"
[ "$(api GET "/private-keys/$DK" | jsonq "d['in_use']")" = "True" ] || die "a deploy key in use must report in_use"
# A key in use cannot be deleted (ON DELETE RESTRICT, §19.2).
KEYDEL=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE -H "Authorization: Bearer $ROOT_TOKEN" "$B/private-keys/$DK")
[ "$KEYDEL" = "409" ] || die "a deploy key still used by an application must not be deletable (got $KEYDEL)"
ok "private repo cloned with a deploy key, key removed after the clone, key deletion refused while in use"

# --- static build pack (§5.2) --------------------------------------------------------
say "deploying a static site (generated nginx image, SPA routing)"
docker exec "$DIND_CTR" sh -c '
  set -e
  rm -rf /srv/static && mkdir -p /srv/static && cd /srv/static && git init -q
  git config user.email e2e@example.com && git config user.name e2e
  mkdir -p dist
  echo "<h1>static-site</h1>" > dist/index.html
  echo "asset" > dist/app.js
  git add -A && git commit -q -m init
' || die "static repo setup failed"

STATIC_BODY=$(python3 - "$PU" "$EU" "$S" "$GIT_HOST" <<'PYEOF'
import json, sys
print(json.dumps({
    "source_type": "git", "name": "staticapp",
    "project_uuid": sys.argv[1], "environment_uuid": sys.argv[2], "server_uuid": sys.argv[3],
    "git_repository": f"git://{sys.argv[4]}/static", "git_branch": "master",
    "build_pack": "static", "publish_directory": "dist",
    "domains": ["static.e2e.test"], "ports_exposes": "80",
}))
PYEOF
)
AU7=$(api POST /applications "$STATIC_BODY" | jsonq "d['uuid']")
DU7=$(api POST "/applications/$AU7/deploy" | jsonq "d['deployment_uuid']")
[ "$(wait_deployment "$DU7" 240)" = "succeeded" ] || die "the static deployment failed"
wait_route static.e2e.test 301
docker exec "$DIND_CTR" curl -sk --resolve static.e2e.test:443:127.0.0.1 https://static.e2e.test/ | grep -q 'static-site' || die "the static site is not served"
docker exec "$DIND_CTR" curl -sk --resolve static.e2e.test:443:127.0.0.1 https://static.e2e.test/app.js | grep -q 'asset' || die "the published assets are not served"
# A deep link is a client route, not a file: it must fall back to index.html, not 404.
SPA=$(docker exec "$DIND_CTR" curl -sk -o /dev/null -w '%{http_code}' --resolve static.e2e.test:443:127.0.0.1 https://static.e2e.test/users/42)
[ "$SPA" = "200" ] || die "the SPA fallback did not serve index.html (got $SPA)"
# A build pack that is not implemented is refused explicitly, never accepted
# and then silently treated as something else.
RAIL=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $ROOT_TOKEN" -H 'Content-Type: application/json' \
  -d "{\"source_type\":\"git\",\"name\":\"rail\",\"project_uuid\":\"$PU\",\"environment_uuid\":\"$EU\",\"server_uuid\":\"$S\",\"git_repository\":\"git://$GIT_HOST/static\",\"git_branch\":\"master\",\"build_pack\":\"railpack\",\"ports_exposes\":\"3000\"}" \
  "$B/applications")
[ "$RAIL" = "422" ] || die "an unimplemented build pack must be refused explicitly (got $RAIL)"
ok "static site built into an nginx image, assets and SPA deep links served"

# --- nixpacks build pack (§5.5) ------------------------------------------------------
say "deploying a Node app with nixpacks (no Dockerfile in the repository)"
# nixpacks was provisioned at onboarding — the validation step must say so.
api GET "/servers/$S" >/dev/null
docker exec "$DIND_CTR" /data/akerdock/bin/nixpacks --version | grep -q "$NIXPACKS_VERSION" \
  || die "nixpacks $NIXPACKS_VERSION was not provisioned on the server"

docker exec "$DIND_CTR" sh -c '
  set -e
  rm -rf /srv/node && mkdir -p /srv/node && cd /srv/node && git init -q
  git config user.email e2e@example.com && git config user.name e2e
  cat > package.json <<JSON
{"name":"e2e-node","version":"1.0.0","scripts":{"start":"node index.js"}}
JSON
  cat > index.js <<JS
const http = require("http");
http.createServer((_, res) => res.end("nixpacks-built-app\n")).listen(3000);
JS
  git add -A && git commit -q -m init
' || die "node repo setup failed"

NIX_BODY=$(python3 - "$PU" "$EU" "$S" "$GIT_HOST" <<'PYEOF'
import json, sys
print(json.dumps({
    "source_type": "git", "name": "nodeapp",
    "project_uuid": sys.argv[1], "environment_uuid": sys.argv[2], "server_uuid": sys.argv[3],
    "git_repository": f"git://{sys.argv[4]}/node", "git_branch": "master",
    "build_pack": "nixpacks",
    "domains": ["node.e2e.test"], "ports_exposes": "3000",
}))
PYEOF
)
AU8=$(api POST /applications "$NIX_BODY" | jsonq "d['uuid']")
DU9=$(api POST "/applications/$AU8/deploy" | jsonq "d['deployment_uuid']")
# A nixpacks build downloads a toolchain: it is legitimately slow the first time.
[ "$(wait_deployment "$DU9" 900)" = "succeeded" ] || die "the nixpacks deployment failed"
# The plan is traced in the build logs — it is the only way to know afterwards
# why the builder picked a given runtime (§5.5).
# Captured first: piping a large body into `grep -q` under `set -o pipefail`
# fails on a MATCH — grep closes the pipe and curl dies of SIGPIPE.
NIXLOGS=$(api GET "/deployments/$DU9/logs?limit=100")
grep -qi 'nixpacks_plan' <<<"$NIXLOGS" || die "the nixpacks plan was not traced in the deployment steps"
wait_route node.e2e.test 301
docker exec "$DIND_CTR" curl -sk --resolve node.e2e.test:443:127.0.0.1 https://node.e2e.test/ | grep -q 'nixpacks-built-app' \
  || die "the nixpacks app is not serving"
ok "node app built by nixpacks (no Dockerfile), plan traced, routed through the proxy"

# --- nixpacks in static mode (§5.5) --------------------------------------------------
say "a site generator built by nixpacks ships as nginx, not as its toolchain"
# A repository whose build produces files, not a server. Deploying the nixpacks
# image itself would ship the whole Node toolchain — and would serve nothing:
# the build command exits, and a container whose command exits is down.
docker exec "$DIND_CTR" sh -c '
  set -e
  rm -rf /srv/site && mkdir -p /srv/site && cd /srv/site && git init -q
  git config user.email e2e@example.com && git config user.name e2e
  cat > package.json <<JSON
{"name":"e2e-site","version":"1.0.0","scripts":{"build":"mkdir -p dist && cp index.html dist/"}}
JSON
  echo "<h1>generated-by-nixpacks</h1>" > index.html
  git add -A && git commit -q -m init
' || die "static site repo setup failed"

SITE_BODY=$(python3 - "$PU" "$EU" "$S" "$GIT_HOST" <<'PYEOF'
import json, sys
print(json.dumps({
    "source_type": "git", "name": "sitegen",
    "project_uuid": sys.argv[1], "environment_uuid": sys.argv[2], "server_uuid": sys.argv[3],
    "git_repository": f"git://{sys.argv[4]}/site", "git_branch": "master",
    "build_pack": "nixpacks", "publish_directory": "dist",
    "domains": ["site.e2e.test"], "ports_exposes": "80",
}))
PYEOF
)
AU13=$(api POST /applications "$SITE_BODY" | jsonq "d['uuid']")
D_SITE=$(api POST "/applications/$AU13/deploy" | jsonq "d['deployment_uuid']")
[ "$(wait_deployment "$D_SITE" 900)" = "succeeded" ] || die_deployment "$D_SITE" "the nixpacks static deployment failed"
wait_route site.e2e.test 301
docker exec "$DIND_CTR" curl -sk --resolve site.e2e.test:443:127.0.0.1 https://site.e2e.test/ | grep -q 'generated-by-nixpacks' \
  || die "the generated site is not served"
# What ships is nginx, not the builder: the toolchain must not be in the image
# that runs in production.
docker exec "$DIND_CTR" docker exec "$AU13" sh -c '! command -v node' \
  || die "the deployed image still carries the Node toolchain — the builder was shipped instead of its output"
# And the intermediate builder image is not left behind on the server.
docker exec "$DIND_CTR" docker images --format '{{.Repository}}:{{.Tag}}' | grep -q -- '-builder' \
  && die "the intermediate builder image was left on the server"
ok "nixpacks static mode: assets built, served by nginx, toolchain absent from the shipped image"

# --- rollback (ADR-006) ----------------------------------------------------------
say "rollback to the previous artifact without a rebuild"
# v2 of the git app: change the served content, redeploy, then roll back.
docker exec "$DIND_CTR" sh -c '
  set -e
  cd /srv/repo
  printf "FROM nginx:alpine\nRUN echo git-built-v2 > /usr/share/nginx/html/index.html\n" > Dockerfile
  git add -A && git commit -q -m v2
' || die "second commit failed"
DU4=$(api POST "/applications/$AU3/deploy" | jsonq "d['deployment_uuid']")
[ "$(wait_deployment "$DU4" 240)" = "succeeded" ] || die "v2 deployment failed"
docker exec "$DIND_CTR" curl -sk --resolve git.e2e.test:443:127.0.0.1 https://git.e2e.test/ | grep -q 'git-built-v2' || die "v2 content not served"

RB=$(api POST "/applications/$AU3/rollback" | jsonq "d['deployment_uuid']")
[ "$(wait_deployment "$RB" 180)" = "succeeded" ] || die "rollback deployment failed"
api GET "/deployments/$RB" | jsonq "d['is_rollback']" | grep -qi true || die "deployment not flagged as rollback"
docker exec "$PG_CTR" psql -U postgres -d akerdock -tAc "SELECT count(*) FROM deployment_steps s JOIN deployments d ON d.id=s.deployment_id WHERE d.uuid='$RB' AND s.name = 'build'" | grep -q '^0$' || die "rollback must not rebuild"
sleep 2
docker exec "$DIND_CTR" curl -sk --resolve git.e2e.test:443:127.0.0.1 https://git.e2e.test/ | grep -q 'git-built-app' || die "rollback did not restore the previous version"
ok "rollback redeployed the previous artifact (no rebuild) and restored v1"

# --- deploy webhook + coalescing --------------------------------------------------
say "deploy webhook (CI) by uuid and by tag, with push coalescing"
TAG_BODY=$(python3 - "$PU" "$EU" "$S" <<'PYEOF'
import json, sys
print(json.dumps({
    "source_type": "docker_image", "name": "tagged",
    "project_uuid": sys.argv[1], "environment_uuid": sys.argv[2], "server_uuid": sys.argv[3],
    "docker_image": "nginx", "docker_image_tag": "alpine", "tags": ["ci", "prod"],
}))
PYEOF
)
AU4=$(api POST /applications "$TAG_BODY" | jsonq "d['uuid']")
api GET "/applications/$AU4" | jsonq "sorted(d['tags'])" | grep -q "ci" || die "tags not persisted"

# by uuid
api POST "/deploy?uuid=$AU4" | jsonq "len(d['deployments'])" | grep -q '^1$' || die "webhook by uuid failed"
# by tag — targets every application carrying it
api POST "/deploy?tag=ci" | jsonq "d['deployments'][0]['resource_uuid']" | grep -q "$AU4" || die "webhook by tag failed"
# neither parameter → 400
STATUS=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $ROOT_TOKEN" "$B/deploy")
[ "$STATUS" = "400" ] || die "webhook without uuid/tag must 400 (got $STATUS)"

# coalescing: rapid pushes leave a single queued deployment, older ones superseded
api POST "/deploy?uuid=$AU4" >/dev/null
LAST=$(api POST "/deploy?uuid=$AU4" | jsonq "d['deployments'][0]['deployment_uuid']")
SUPERSEDED=$(docker exec "$PG_CTR" psql -U postgres -d akerdock -tAc "SELECT count(*) FROM deployments WHERE status = 'superseded'")
[ "$SUPERSEDED" -ge 1 ] || die "expected superseded deployments from coalescing, got $SUPERSEDED"
[ "$(wait_deployment "$LAST" 240)" = "succeeded" ] || die "the surviving coalesced deployment failed"
ok "webhook deploys by uuid and tag, older queued pushes superseded ($SUPERSEDED)"

# --- signed Git webhooks (INV-009) ---------------------------------------------------
say "a signed GitHub push deploys; a replay, a bad signature and [skip ci] do not"
WH=$(api POST "/applications/$AU3/webhook-endpoint" '{"provider":"github"}')
WHU=$(echo "$WH" | jsonq "d['uuid']")
WHSECRET=$(echo "$WH" | jsonq "d['secret']")
[ ${#WHSECRET} -eq 64 ] || die "no webhook secret was returned at creation"
# The secret is returned once and never again — it lives encrypted at rest.
APPBODY=$(api GET "/applications/$AU3")
grep -q "$WHSECRET" <<<"$APPBODY" && die "the webhook secret is readable after creation (INV-003)"

# The git app (AU3) tracks branch master; its repo lives at /srv/repo in the DinD.
HEAD_SHA=$(docker exec "$DIND_CTR" git -C /srv/repo rev-parse HEAD)
push_body() {  # push_body <branch> <message> [files_json]
  python3 - "$1" "$2" "$HEAD_SHA" "${3:-[\"Dockerfile\"]}" <<'PYEOF'
import json, sys
print(json.dumps({
    "ref": f"refs/heads/{sys.argv[1]}",
    "after": sys.argv[3],
    "head_commit": {"message": sys.argv[2]},
    "commits": [{"added": [], "removed": [], "modified": json.loads(sys.argv[4])}],
}))
PYEOF
}
sign() { python3 -c "
import hashlib, hmac, sys
print(hmac.new(sys.argv[1].encode(), sys.stdin.buffer.read(), hashlib.sha256).hexdigest())
" "$WHSECRET"; }
deliver() {  # deliver <body> <delivery_id> <signature>
  curl -s -o /dev/null -w '%{http_code}' -X POST \
    -H "X-GitHub-Event: push" -H "X-GitHub-Delivery: $2" -H "X-Hub-Signature-256: sha256=$3" \
    -H 'Content-Type: application/json' -d "$1" \
    "http://127.0.0.1:${API_PORT}/webhooks/github/$WHU"
}

BEFORE=$(api GET "/applications/$AU3/deployments" | jsonq "len(d['data'])")

# 1. A forged signature triggers nothing and must not leak why (401).
BODY=$(push_body master "feat: real push")
CODE=$(deliver "$BODY" "d-forged" "$(printf 'deadbeef%.0s' 1 2 3 4 5 6 7 8)")
[ "$CODE" = "401" ] || die "a forged signature must be refused (got $CODE)"

# 2. A properly signed push deploys.
SIG=$(printf '%s' "$BODY" | sign)
CODE=$(deliver "$BODY" "d-1" "$SIG")
[ "$CODE" = "200" ] || die "a signed push must be accepted (got $CODE)"

# 3. The same delivery id replayed must NOT deploy a second time (INV-009).
CODE=$(deliver "$BODY" "d-1" "$SIG")
[ "$CODE" = "200" ] || die "a replay must still answer 200 (got $CODE)"

# 4. [skip ci] is honoured — the author asked for no deployment.
SKIP_BODY=$(push_body master "chore: docs [skip ci]")
CODE=$(deliver "$SKIP_BODY" "d-2" "$(printf '%s' "$SKIP_BODY" | sign)")
[ "$CODE" = "200" ] || die "a [skip ci] push must answer 200 (got $CODE)"

# 5. A push on another branch is not this application's business.
OTHER_BODY=$(push_body feature-x "feat: elsewhere")
CODE=$(deliver "$OTHER_BODY" "d-3" "$(printf '%s' "$OTHER_BODY" | sign)")
[ "$CODE" = "200" ] || die "a push on another branch must answer 200 (got $CODE)"

# Exactly ONE deployment came out of those five deliveries.
NEW=0
for _ in $(seq 40); do
  NEW=$(( $(api GET "/applications/$AU3/deployments" | jsonq "len(d['data'])") - BEFORE ))
  [ "$NEW" -ge 1 ] && break
  sleep 2
done
sleep 5  # give the ignored deliveries every chance to wrongly produce a deployment
NEW=$(( $(api GET "/applications/$AU3/deployments" | jsonq "len(d['data'])") - BEFORE ))
[ "$NEW" = "1" ] || die "expected exactly 1 deployment from the signed push, got $NEW"
DW=$(api GET "/applications/$AU3/deployments" | jsonq "d['data'][0]['uuid']")
[ "$(wait_deployment "$DW" 300)" = "succeeded" ] || die "the webhook-triggered deployment failed"
ok "signed push deployed once; replay, forged signature, [skip ci] and other branches ignored"

# --- persistent volumes (§8, INV-008) --------------------------------------------
say "persistent volume: data survives redeploys, and deletion by default"
VOL_BODY='{"kind":"volume","name":"data","mount_path":"/data"}'
SU=$(api POST "/applications/$AU4/storages" "$VOL_BODY" | jsonq "d['uuid']")
VOL_NAME=$(api GET "/applications/$AU4/storages" | jsonq "d['data'][0]['docker_volume_name']")
[ "$VOL_NAME" = "${AU4}_data" ] || die "unexpected docker volume name: $VOL_NAME"
STATUS=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $ROOT_TOKEN" -H 'Content-Type: application/json' -d "$VOL_BODY" "$B/applications/$AU4/storages")
[ "$STATUS" = "409" ] || die "duplicate mount_path must conflict (got $STATUS)"

D5=$(api POST "/applications/$AU4/deploy" | jsonq "d['deployment_uuid']")
[ "$(wait_deployment "$D5" 180)" = "succeeded" ] || die "deployment with volume failed"
docker exec "$DIND_CTR" docker volume inspect "$VOL_NAME" >/dev/null 2>&1 || die "volume not created on the server"
docker exec "$DIND_CTR" docker exec "$AU4" sh -c 'echo persisted-payload > /data/state.txt' || die "cannot write into the volume"

# redeploy: the container is replaced, the data must survive
D6=$(api POST "/applications/$AU4/deploy" | jsonq "d['deployment_uuid']")
[ "$(wait_deployment "$D6" 180)" = "succeeded" ] || die "redeploy with volume failed"
docker exec "$DIND_CTR" docker exec "$AU4" cat /data/state.txt | grep -q persisted-payload || die "volume data lost across redeploy"

# Deleting the application does NOT delete its data (INV-008): destroying a
# volume nobody asked to destroy is the one mistake that cannot be undone. The
# assertion lives here, next to the volume it is about.
[ "$(wait_job "$(api DELETE "/applications/$AU4" | jsonq "d['job_uuid']")")" = "succeeded" ] || die "deletion of the volume application failed"
docker exec "$DIND_CTR" docker volume inspect "$VOL_NAME" >/dev/null 2>&1 \
  || die "the volume was destroyed without an explicit request (INV-008)"
ok "volume mounted, created idempotently, data survived a redeploy and the deletion of its application (INV-008)"

# --- certificates: observed reflection (§18.3) ---------------------------------------
say "certificates are reflected from the server, renewal is a job"
CERTS=$(api GET "/servers/$S/certificates")
echo "$CERTS" | jsonq "'data' in d" | grep -q True || die "the certificates endpoint is malformed"
# the sync job must have run (no public DNS here, so ACME issues nothing —
# an empty, healthy reflection is the expected result)
sleep 3
docker exec "$PG_CTR" psql -U postgres -d akerdock -tAc "SELECT count(*) FROM jobs WHERE job_type = 'certificate.sync' AND status = 'succeeded'" | grep -qv '^0$' || die "the certificate sync job did not run"
api GET "/servers/$S/certificates?expiring_within_days=30" >/dev/null || die "the expiry filter failed"
ok "certificate reflection synchronized from the server (0 issued: no public DNS in the sandbox)"

# --- DNS-01 for wildcards (proxy-contract §7.2) --------------------------------------
say "a wildcard needs DNS-01: the credential is materialized 0600 and never in a config"
# A wildcard cannot be validated over HTTP-01 — the CA has no single host to ask.
# Accepting one anyway would leave every preview URL on the self-signed fallback,
# forever, with nothing saying why.
NOWILD=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $ROOT_TOKEN" -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $(openssl rand -hex 8)" \
  -d "{\"name\":\"wild\",\"host\":\"127.0.0.1\",\"port\":${SSH_PORT},\"private_key_uuid\":\"$K\",\"wildcard_domain\":\"e2e.test\"}" "$B/servers")
[ "$NOWILD" = "422" ] || die "a wildcard domain without a DNS-01 credential must be refused (got $NOWILD)"

DNSC=$(api POST /dns-credentials '{"name":"cf","provider":"cloudflare","config":{"CF_DNS_API_TOKEN":"tok-s3cr3t"}}' | jsonq "d['uuid']")
# A value with a newline would end the env-file line and start a second variable.
BADVAL=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $ROOT_TOKEN" -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $(openssl rand -hex 8)" \
  -d '{"name":"bad","provider":"cloudflare","config":{"CF_DNS_API_TOKEN":"a\nEVIL=1"}}' "$B/dns-credentials")
[ "$BADVAL" = "422" ] || die "a credential value containing a newline must be refused (got $BADVAL)"
api GET "/dns-credentials/$DNSC" | grep -q 'tok-s3cr3t' && die "the DNS credential content was returned by the API (INV-003)"

# Attach it to the existing server, with a wildcard, and re-validate: the proxy
# is recreated with the credential injected by --env-file.
SV=$(api GET "/servers/$S" | jsonq "d['version']")
curl -sf -X PATCH -H "Authorization: Bearer $ROOT_TOKEN" -H "If-Match: \"$SV\"" -H 'Content-Type: application/json' \
  -d "{\"wildcard_domain\":\"e2e.test\",\"dns_credential_uuid\":\"$DNSC\"}" "$B/servers/$S" >/dev/null || die "attaching the DNS credential failed"
docker exec "$DIND_CTR" docker rm -f akerdock-proxy >/dev/null 2>&1 || true
[ "$(wait_job "$(api POST "/servers/$S/validate" | jsonq "d['job_uuid']")" 240)" = "succeeded" ] || die "the re-validation with DNS-01 failed"

# The resolver is declared…
docker exec "$DIND_CTR" grep -q 'dns01-cloudflare' /data/akerdock/proxy/traefik.yaml || die "the DNS-01 resolver is not declared in the static config"
# …the credential is NOT in it (traefik.yaml is checksummed, stored as a revision
# and read back: a secret there would be a second copy of that secret in the DB).
docker exec "$DIND_CTR" grep -q 'tok-s3cr3t' /data/akerdock/proxy/traefik.yaml && die "the DNS credential leaked into the static config (INV-003)"
docker exec "$PG_CTR" psql -U postgres -d akerdock -tAc "SELECT count(*) FROM proxy_config_revisions WHERE content LIKE '%tok-s3cr3t%'" | grep -q '^0$' \
  || die "the DNS credential leaked into a proxy revision (INV-003)"
# …it lives in a 0600 env-file, injected into the proxy.
[ "$(docker exec "$DIND_CTR" stat -c '%a' /data/akerdock/proxy/acme.env)" = "600" ] || die "acme.env must be 0600"
docker exec "$DIND_CTR" grep -q 'CF_DNS_API_TOKEN=tok-s3cr3t' /data/akerdock/proxy/acme.env || die "the credential was not materialized"
docker exec "$DIND_CTR" docker inspect akerdock-proxy --format '{{range .Config.Env}}{{println .}}{{end}}' | grep -q 'CF_DNS_API_TOKEN' \
  || die "the proxy container did not receive the DNS credential"
# A credential a server depends on cannot be deleted.
INUSE=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE -H "Authorization: Bearer $ROOT_TOKEN" "$B/dns-credentials/$DNSC")
[ "$INUSE" = "409" ] || die "deleting an in-use DNS credential returned $INUSE, expected 409"
ok "DNS-01 resolver declared, credential materialized 0600 and injected, never in a config or a revision"

# --- SSH host key pinning (§20.1) ----------------------------------------------------
say "a server that changes its SSH host key is refused, not silently trusted"
PINNED=$(docker exec "$PG_CTR" psql -U postgres -d akerdock -tAc "SELECT host_key_fingerprint FROM servers LIMIT 1")
case "$PINNED" in SHA256:*) ;; *) die "the host key was not pinned at validation (got '$PINNED')";; esac

# Regenerate the server's host keys: same host, same port, different identity —
# exactly what a man-in-the-middle looks like.
docker exec "$DIND_CTR" sh -c 'pkill sshd; rm -f /etc/ssh/ssh_host_*; ssh-keygen -A >/dev/null 2>&1; /usr/sbin/sshd' \
  || die "could not rotate the host keys of the target"
sleep 2

# Any operational job must now refuse to connect.
REVAL=$(api POST "/servers/$S/validate" | jsonq "d['job_uuid']")
[ "$(wait_job "$REVAL" 120)" = "dead_letter" ] || die "a changed host key must fail the validation, not be re-pinned"
api GET "/jobs/$REVAL" | jsonq "str(d['steps'])" | grep -qi 'host key' || die "the failure must name the host key change"

# Restore the original identity: the pin matches again and the server is usable.
docker exec -i "$DIND_CTR" sh -c 'pkill sshd; rm -f /etc/ssh/ssh_host_*; tar -xzf - -C / && /usr/sbin/sshd' < "$WORKDIR/hostkeys.tgz" \
  || die "could not restore the original host keys"
sleep 2
[ "$(wait_job "$(api POST "/servers/$S/validate" | jsonq "d['job_uuid']")" 180)" = "succeeded" ] || die "the server must be usable again once it presents the pinned key"
ok "host key pinned at onboarding; a changed key is refused and named, never auto-repinned"

# --- private registry (§5.1, INV-003) ------------------------------------------------
say "an image pulled from a private registry, with the password never in argv"
# A real private registry inside the DinD, with htpasswd auth. Anonymous pulls
# are refused, so the deployment below only works if the login actually happened.
docker exec "$DIND_CTR" sh -c 'mkdir -p /auth && docker run --rm --entrypoint htpasswd httpd:2-alpine -Bbn akerdock s3cr3t-token > /auth/htpasswd' 2>/dev/null
docker exec "$DIND_CTR" docker run -d --name registry --network host \
  -e REGISTRY_AUTH=htpasswd -e REGISTRY_AUTH_HTPASSWD_REALM=akerdock \
  -e REGISTRY_AUTH_HTPASSWD_PATH=/auth/htpasswd -v /auth:/auth registry:2 >/dev/null
for _ in $(seq 1 30); do docker exec "$DIND_CTR" sh -c 'wget -qO- http://127.0.0.1:5000/v2/ 2>&1 | grep -q . && exit 0; exit 1' && break; sleep 1; done

# Push a known image into it (the push is authenticated too).
docker exec "$DIND_CTR" sh -c 'echo s3cr3t-token | docker login 127.0.0.1:5000 -u akerdock --password-stdin' >/dev/null 2>&1 \
  || die "cannot log in to the test registry"
docker exec "$DIND_CTR" docker tag nginx:alpine 127.0.0.1:5000/private/web:v1 >/dev/null
docker exec "$DIND_CTR" docker push 127.0.0.1:5000/private/web:v1 >/dev/null 2>&1 || die "cannot push to the test registry"
# Forget the credentials on the server AND drop the local image: the deployment
# must pull it back, which is only possible if akerdock logs in itself.
docker exec "$DIND_CTR" docker logout 127.0.0.1:5000 >/dev/null 2>&1
docker exec "$DIND_CTR" docker rmi 127.0.0.1:5000/private/web:v1 >/dev/null 2>&1

# Without a credential, the pull must fail — otherwise the test below proves nothing.
ANON=$(api POST /applications "$(python3 - "$PU" "$EU" "$S" <<'PYEOF'
import json, sys
print(json.dumps({"source_type": "docker_image", "name": "anon", "project_uuid": sys.argv[1],
    "environment_uuid": sys.argv[2], "server_uuid": sys.argv[3],
    "docker_image": "127.0.0.1:5000/private/web", "docker_image_tag": "v1", "ports_exposes": "80"}))
PYEOF
)" | jsonq "d['uuid']")
D_ANON=$(api POST "/applications/$ANON/deploy" | jsonq "d['deployment_uuid']")
[ "$(wait_deployment "$D_ANON" 120)" = "failed" ] || die "a private image pulled without credentials must NOT succeed"

# The credential itself. The password is write-only: no permission reads it back.
RC=$(api POST /registry-credentials '{"name":"local","registry_url":"127.0.0.1:5000","username":"akerdock","password":"s3cr3t-token"}' | jsonq "d['uuid']")
api GET "/registry-credentials/$RC" | grep -q s3cr3t && die "the registry password was returned by the API (INV-003)"
BADURL=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $ROOT_TOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"bad","registry_url":"evil.io; rm -rf /","username":"u","password":"p"}' "$B/registry-credentials")
[ "$BADURL" = "422" ] || die "a registry_url with a shell metacharacter must be refused (got $BADURL)"

REG_BODY=$(python3 - "$PU" "$EU" "$S" "$RC" <<'PYEOF'
import json, sys
print(json.dumps({"source_type": "docker_image", "name": "private-web", "project_uuid": sys.argv[1],
    "environment_uuid": sys.argv[2], "server_uuid": sys.argv[3],
    "docker_image": "127.0.0.1:5000/private/web", "docker_image_tag": "v1",
    "registry_credential_uuid": sys.argv[4], "ports_exposes": "80"}))
PYEOF
)
AUR=$(api POST /applications "$REG_BODY" | jsonq "d['uuid']")
D_REG=$(api POST "/applications/$AUR/deploy" | jsonq "d['deployment_uuid']")
[ "$(wait_deployment "$D_REG" 240)" = "succeeded" ] || die "the deployment from the private registry failed"

# The password never appeared in a command line, and the server is logged out
# again: an authenticated ~/.docker/config.json left behind would let anything
# else on that host pull from the registry.
api GET "/deployments/$D_REG" | grep -q s3cr3t && die "the registry password leaked into the deployment log (INV-003)"
docker exec "$DIND_CTR" sh -c 'cat /root/.docker/config.json 2>/dev/null | grep -q "127.0.0.1:5000"' \
  && die "the server is still logged in to the registry after the deployment"

# A credential an application depends on cannot be deleted (§19.2).
INUSE=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE -H "Authorization: Bearer $ROOT_TOKEN" "$B/registry-credentials/$RC")
[ "$INUSE" = "409" ] || die "deleting an in-use registry credential returned $INUSE, expected 409"
ok "private registry: anonymous pull refused, authenticated pull succeeded, password never in argv, logout enforced"

# --- build server (§3.4) -------------------------------------------------------------
say "an application built on a dedicated build server, pulled back by digest"
# A SECOND machine, which hosts nothing. It trusts the registry served by the
# first one — over plain HTTP, which is why both daemons were told about it at
# startup rather than after the fact.
docker run -d --rm --privileged --name "$BUILD_CTR" --network "$NET_CTR" --ip "$BUILD_IP" \
  -p "${BUILD_SSH_PORT}:22" -e DOCKER_TLS_CERTDIR="" \
  docker:27-dind --insecure-registry "${DIND_IP}:5000" >/dev/null
for _ in $(seq 1 60); do docker exec "$BUILD_CTR" docker info >/dev/null 2>&1 && break; sleep 2; done
docker exec "$BUILD_CTR" docker info >/dev/null 2>&1 || die "dockerd did not start in the build server"
BS_ERR=""
for attempt in 1 2 3; do
  if BS_ERR=$(docker exec "$BUILD_CTR" sh -c "
      apk upgrade --no-cache >/dev/null 2>&1
      apk add --no-cache openssh-server curl git
      ssh-keygen -A >/dev/null 2>&1
      mkdir -p /root/.ssh && echo '$(cat "$WORKDIR/serverkey.pub")' > /root/.ssh/authorized_keys
      /usr/sbin/sshd" 2>&1); then
    BS_ERR=""
    break
  fi
  sleep $((attempt * 3))
done
[ -z "$BS_ERR" ] || die "sshd setup failed in the build server: $(printf '%s' "$BS_ERR" | tail -3 | tr '\n' ' ')"

BS=$(api POST /servers "{\"name\":\"builder\",\"host\":\"127.0.0.1\",\"port\":${BUILD_SSH_PORT},\"private_key_uuid\":\"$K\",\"is_build_server\":true}" | jsonq "d['uuid']")
[ "$(wait_job "$(api POST "/servers/$BS/validate" | jsonq "d['job_uuid']")" 240)" = "succeeded" ] || die "the build server did not validate"
# A build server hosts nothing, so it routes nothing: giving it a proxy would
# bind 80/443 on a machine that has no reason to listen.
docker exec "$BUILD_CTR" docker ps --format '{{.Names}}' | grep -q akerdock-proxy && die "a proxy was started on the build server"
# And it cannot be used as a deployment target.
ONBUILD=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $ROOT_TOKEN" -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $(openssl rand -hex 8)" \
  -d "{\"source_type\":\"docker_image\",\"name\":\"nope\",\"project_uuid\":\"$PU\",\"environment_uuid\":\"$EU\",\"server_uuid\":\"$BS\",\"docker_image\":\"nginx\",\"ports_exposes\":\"80\"}" "$B/applications")
[ "$ONBUILD" = "422" ] || die "a build server must not be a deployment target (got $ONBUILD)"

# The push registry is the one served by the target DinD, reachable from BOTH.
PRC=$(api POST /registry-credentials "{\"name\":\"push\",\"registry_url\":\"${DIND_IP}:5000\",\"username\":\"akerdock\",\"password\":\"s3cr3t-token\"}" | jsonq "d['uuid']")
# Without a push registry, building elsewhere produces an image nobody can pull.
NOPUSH=$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $ROOT_TOKEN" -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $(openssl rand -hex 8)" \
  -d "{\"source_type\":\"git\",\"name\":\"nopush\",\"project_uuid\":\"$PU\",\"environment_uuid\":\"$EU\",\"server_uuid\":\"$S\",\"git_repository\":\"git://${GIT_HOST}/repo\",\"git_branch\":\"master\",\"build_pack\":\"dockerfile\",\"use_build_server\":true,\"ports_exposes\":\"80\"}" "$B/applications")
[ "$NOPUSH" = "422" ] || die "use_build_server without a push registry must be refused (got $NOPUSH)"

BSRV_BODY=$(python3 - "$PU" "$EU" "$S" "$GIT_HOST" "$PRC" <<'PYEOF'
import json, sys
print(json.dumps({
    "source_type": "git", "name": "remote-built",
    "project_uuid": sys.argv[1], "environment_uuid": sys.argv[2], "server_uuid": sys.argv[3],
    "git_repository": f"git://{sys.argv[4]}/repo", "git_branch": "master",
    "build_pack": "dockerfile", "use_build_server": True,
    "push_registry_credential_uuid": sys.argv[5],
    "domains": ["built.e2e.test"], "ports_exposes": "80",
}))
PYEOF
)
AU14=$(api POST /applications "$BSRV_BODY" | jsonq "d['uuid']")
D_BS=$(api POST "/applications/$AU14/deploy" | jsonq "d['deployment_uuid']")
[ "$(wait_deployment "$D_BS" 600)" = "succeeded" ] || die_deployment "$D_BS" "the deployment on a build server failed"

# The build happened on the OTHER machine: the sources are there, and not here.
docker exec "$BUILD_CTR" sh -c "ls /data/akerdock/applications/$AU14/source" >/dev/null 2>&1 \
  || die "the build server has no sources — the build did not happen there"
docker exec "$DIND_CTR" sh -c "! ls /data/akerdock/applications/$AU14/source" >/dev/null 2>&1 \
  || die "the deployment server cloned the sources — it built the image itself"
# What runs came from the registry, by digest: that is what makes the running
# image and the built image provably the same, and what a rollback replays.
# The recorded digest names the registry AND pins the content: it is what makes
# the running image and the built image provably the same, and what a rollback
# replays without a rebuild (ADR-006).
DIGEST=$(api GET "/deployments/$D_BS" | jsonq "d['image_digest']")
case "$DIGEST" in "${DIND_IP}:5000/"*"@sha256:"*) ;; *) die "the deployment did not record a registry digest: $DIGEST";; esac
wait_route built.e2e.test 301
docker exec "$DIND_CTR" curl -sk --resolve built.e2e.test:443:127.0.0.1 https://built.e2e.test/ | grep -q . || die "the remotely built app is not serving"
ok "built on a dedicated build server, pushed to a registry, pulled back by digest and served"
