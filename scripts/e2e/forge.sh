# E2E shard: PR/MR previews through the per-application manual webhooks —
# GitLab and Gitea (git-webhook-protocols §4/§6, §20.4, ADR-011) — against
# HTTP stubs of both forge APIs. The GitHub App path has its own shard
# (`github`); this one proves the PARITY: same lifecycle, same policies,
# degraded feedback by commit statuses + one upserted comment, and the
# opt-in trigger controls (§20.4.7).
#
# What the shard proves:
#   1. GitLab MR opened → protected preview (basic auth, noindex), commit
#      statuses (running→success, name AkerDock/preview) and ONE note,
#      updated in place — via the API token stored on the git source.
#   2. preview_require_label: an MR without the label deploys NOTHING.
#   3. preview_cancel_obsolete_builds: a new commit supersedes/cancels the
#      in-flight preview build; the newest commit wins.
#   4. Comment commands (opt-in): /destroy refused for an author without
#      write access (rights checked server-side), executed for a maintainer;
#      /deploy revives the destroyed preview.
#   5. MR merged → the preview is destroyed, route gone.
#   6. Gitea PR opened on a COMPOSE application → ephemeral stack per PR
#      (§20.4.1): preview-scoped containers/network/volumes, magic
#      SERVICE_URL resolved to the preview's own URL, protected route;
#      commit status + upserted comment; PR closed → everything destroyed.

GITLAB_PORT=$((18600 + IDX * 10))
GITEA_PORT=$((18601 + IDX * 10))

# --- server wildcard (preview FQDNs of the compose stack need one) -------------
say "giving the server a wildcard domain"
DNSC=$(api POST /dns-credentials '{"name":"cf","provider":"cloudflare","config":{"CF_DNS_API_TOKEN":"tok-e2e"}}' | jsonq "d['uuid']")
SV=$(api GET "/servers/$S" | jsonq "d['version']")
curl -sf -X PATCH -H "Authorization: Bearer $ROOT_TOKEN" -H "Content-Type: application/json" \
  -H "If-Match: \"$SV\"" \
  -d "{\"wildcard_domain\":\"e2e.test\",\"dns_credential_uuid\":\"$DNSC\"}" "$B/servers/$S" >/dev/null \
  || die "server wildcard patch failed"
ok "server carries *.e2e.test"

# patch_app UUID JSON — PATCH with the current version (If-Match, INV-014).
patch_app() {
  local uuid=$1 body=$2 v
  v=$(api GET "/applications/$uuid" | jsonq "d['version']")
  curl -sf -X PATCH -H "Authorization: Bearer $ROOT_TOKEN" -H "Content-Type: application/json" \
    -H "If-Match: \"$v\"" -d "$body" "$B/applications/$uuid" >/dev/null || die "PATCH $uuid failed: $body"
}

# --- the forge API stubs (plain HTTP: api_url is pinned per git source) --------
say "starting the GitLab and Gitea API stubs"
cat > "$WORKDIR/forge-stub.py" <<'PYSTUB'
import http.server, json, sys

workdir, mode, port = sys.argv[1], sys.argv[2], int(sys.argv[3])
P = "gl-" if mode == "gitlab" else "gt-"

def stored_comment():
    try:
        line = open(workdir + "/" + P + "comments.jsonl").read().strip().split("\n")[0]
        return [{"id": 41, "body": json.loads(line)["body"]}] if line else []
    except FileNotFoundError:
        return []

class H(http.server.BaseHTTPRequestHandler):
    def _json(self, code, obj):
        raw = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def _record(self, name, body):
        with open(workdir + "/" + P + name + ".jsonl", "a") as f:
            f.write(body.decode() + "\n")

    def _auth_ok(self):
        if mode == "gitlab":
            return self.headers.get("PRIVATE-TOKEN") == "glpat-e2e"
        return self.headers.get("Authorization") == "token gtpat-e2e"

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("content-length", 0)))
        if not self._auth_ok():
            return self._json(401, {"message": "bad token"})
        p = self.path
        if "/statuses/" in p:
            self._record("statuses", body)
            return self._json(201, {"id": 1})
        if p.endswith("/notes") or p.endswith("/comments"):
            self._record("comments", body)
            return self._json(201, {"id": 41})
        return self._json(404, {"message": "unexpected " + p})

    def do_PUT(self):
        body = self.rfile.read(int(self.headers.get("content-length", 0)))
        if "/notes/41" in self.path:
            self._record("comment-updates", body)
            return self._json(200, {"id": 41})
        return self._json(404, {"message": "unexpected " + self.path})

    def do_PATCH(self):
        body = self.rfile.read(int(self.headers.get("content-length", 0)))
        if "/comments/41" in self.path:
            self._record("comment-updates", body)
            return self._json(200, {"id": 41})
        return self._json(404, {"message": "unexpected " + self.path})

    def do_GET(self):
        p = self.path.split("?")[0]
        if not self._auth_ok():
            return self._json(401, {"message": "bad token"})
        if p.endswith("/notes") or p.endswith("/comments"):
            return self._json(200, stored_comment())
        # GitLab rights check (protocols §4.3): member 900 is a maintainer,
        # anyone else is not a member at all.
        if "/members/all/" in p:
            uid = p.rsplit("/", 1)[1]
            if uid == "900":
                return self._json(200, {"id": 900, "access_level": 40})
            return self._json(404, {"message": "not a member"})
        # Gitea rights check (protocols §6.3).
        if "/collaborators/" in p and p.endswith("/permission"):
            user = p.split("/collaborators/")[1].split("/")[0]
            perm = "write" if user == "maintainer" else "read"
            return self._json(200, {"permission": perm})
        return self._json(404, {"message": "unexpected " + p})

    def log_message(self, *a):
        pass

http.server.HTTPServer(("127.0.0.1", port), H).serve_forever()
PYSTUB
python3 "$WORKDIR/forge-stub.py" "$WORKDIR" gitlab "$GITLAB_PORT" &
GITLAB_STUB_PID=$!
python3 "$WORKDIR/forge-stub.py" "$WORKDIR" gitea "$GITEA_PORT" &
GITEA_STUB_PID=$!
for _ in $(seq 1 20); do
  curl -s -o /dev/null "http://127.0.0.1:${GITLAB_PORT}/x" 2>/dev/null &&
    curl -s -o /dev/null "http://127.0.0.1:${GITEA_PORT}/x" 2>/dev/null && break
  sleep 0.5
done
ok "stubs up on :$GITLAB_PORT (gitlab) and :$GITEA_PORT (gitea)"

# --- git fixtures ---------------------------------------------------------------
say "serving the two repositories (dockerfile app, compose stack)"
docker exec "$DIND_CTR" sh -c '
  apk add --no-cache git git-daemon >/dev/null 2>&1
  rm -rf /srv/glrepo.git && mkdir -p /srv/glrepo.git && cd /srv/glrepo.git
  git init -q && git config user.email e2e@example.com && git config user.name e2e
  printf "FROM nginx:alpine\nRUN echo gl-v1 > /usr/share/nginx/html/index.html\nHEALTHCHECK --interval=2s --retries=5 CMD wget -q -O /dev/null http://127.0.0.1/ || exit 1\n" > Dockerfile
  git add -A && git commit -q -m v1
  git checkout -q -b mr14 && sed -i s/gl-v1/gl-pr/ Dockerfile && git add -A && git commit -q -m pr
  git checkout -q master && git update-ref refs/merge-requests/14/head refs/heads/mr14 && git branch -q -D mr14

  rm -rf /srv/gtrepo.git && mkdir -p /srv/gtrepo.git && cd /srv/gtrepo.git
  git init -q && git config user.email e2e@example.com && git config user.name e2e
  printf "FROM nginx:alpine\nRUN echo gt-pr > /usr/share/nginx/html/index.html\nHEALTHCHECK --interval=2s --retries=5 CMD wget -q -O /dev/null http://127.0.0.1/ || exit 1\n" > Dockerfile
  cat > docker-compose.yml <<COMPOSE
services:
  web:
    build: .
    expose:
      - "80"
    environment:
      PREVIEW_URL: \${SERVICE_URL_WEB}
  helper:
    image: alpine:3.20
    command: ["sleep", "3600"]
    volumes:
      - data:/data
volumes:
  data: {}
COMPOSE
  git add -A && git commit -q -m v1
  git checkout -q -b pr21 && sed -i s/gt-pr/gt-pr21/ Dockerfile && git add -A && git commit -q -m pr21
  git checkout -q master && git update-ref refs/pull/21/head refs/heads/pr21 && git branch -q -D pr21
  git daemon --base-path=/srv --export-all --enable=receive-pack --reuseaddr --detach /srv 2>/dev/null || true
' || die "git fixtures failed"
GIT_HOST=$(docker exec "$DIND_CTR" hostname -i | awk '{print $1}')
MRSHA=$(docker exec "$DIND_CTR" sh -c 'cd /srv/glrepo.git && git rev-parse refs/merge-requests/14/head' | tr -d ' \n')
PRSHA=$(docker exec "$DIND_CTR" sh -c 'cd /srv/gtrepo.git && git rev-parse refs/pull/21/head' | tr -d ' \n')
ok "repositories served from git://$GIT_HOST/"

# --- the two applications --------------------------------------------------------
say "creating the applications (GitLab dockerfile, Gitea compose) with API tokens"
GLAPP=$(python3 - "$PU" "$EU" "$S" "$GIT_HOST" <<'PYEOF'
import json, sys
print(json.dumps({
    "source_type": "git", "name": "glapp",
    "project_uuid": sys.argv[1], "environment_uuid": sys.argv[2], "server_uuid": sys.argv[3],
    "git_repository": "git://%s/glrepo.git" % sys.argv[4],
    "git_branch": "master", "build_pack": "dockerfile",
    "domains": ["glapp.e2e.test"], "ports_exposes": "80",
}))
PYEOF
)
GLU=$(api POST /applications "$GLAPP" | jsonq "d['uuid']")
patch_app "$GLU" "{\"previews_enabled\":true,\"preview_protection\":\"basic_auth\",\"git_api_token\":\"glpat-e2e\",\"git_api_url\":\"http://127.0.0.1:${GITLAB_PORT}\"}"
[ "$(api GET "/applications/$GLU" | jsonq "d['git_api_token_set']")" = "True" ] || die "git_api_token_set must be true after the patch"
GLWH=$(api POST "/applications/$GLU/webhook-endpoint" '{"provider":"gitlab"}')
GLSECRET=$(echo "$GLWH" | jsonq "d['secret']")
GLEP=$(echo "$GLWH" | jsonq "d['uuid']")

GTAPP=$(python3 - "$PU" "$EU" "$S" "$GIT_HOST" <<'PYEOF'
import json, sys
print(json.dumps({
    "source_type": "git", "name": "gtapp",
    "project_uuid": sys.argv[1], "environment_uuid": sys.argv[2], "server_uuid": sys.argv[3],
    "git_repository": "git://%s/gtrepo.git" % sys.argv[4],
    "git_branch": "master", "build_pack": "compose",
}))
PYEOF
)
GTU=$(api POST /applications "$GTAPP" | jsonq "d['uuid']")
patch_app "$GTU" "{\"previews_enabled\":true,\"preview_protection\":\"basic_auth\",\"git_api_token\":\"gtpat-e2e\",\"git_api_url\":\"http://127.0.0.1:${GITEA_PORT}\"}"
GTWH=$(api POST "/applications/$GTU/webhook-endpoint" '{"provider":"gitea"}')
GTSECRET=$(echo "$GTWH" | jsonq "d['secret']")
GTEP=$(echo "$GTWH" | jsonq "d['uuid']")
ok "applications created, tokens stored on the git sources, endpoints issued"

# send_gitlab_mr ACTION DELIVERY SHA [EXTRA_JSON] — a Merge Request Hook.
send_gitlab_mr() {
  python3 - "$API_PORT" "$GLEP" "$GLSECRET" "$1" "$2" "$3" "${4:-}" <<'PYEOF'
import json, sys, urllib.request
port, ep, secret, action, delivery, sha, extra = sys.argv[1:8]
attrs = {
    "iid": 14, "action": action, "source_branch": "mr14",
    "source_project_id": 777, "target_project_id": 777,
    "last_commit": {"id": sha},
}
body = {"object_kind": "merge_request", "project": {"id": 777},
        "object_attributes": attrs, "labels": []}
if extra:
    body.update(json.loads(extra))
raw = json.dumps(body).encode()
req = urllib.request.Request(f"http://127.0.0.1:{port}/webhooks/gitlab/{ep}", data=raw, method="POST")
req.add_header("Content-Type", "application/json")
req.add_header("X-Gitlab-Event", "Merge Request Hook")
req.add_header("X-Gitlab-Event-UUID", delivery)
req.add_header("X-Gitlab-Token", secret)
print(urllib.request.urlopen(req).status)
PYEOF
}

# send_gitlab_note DELIVERY NOTE USER_ID — a Note Hook (comment command).
send_gitlab_note() {
  python3 - "$API_PORT" "$GLEP" "$GLSECRET" "$1" "$2" "$3" <<'PYEOF'
import json, sys, urllib.request
port, ep, secret, delivery, note, uid = sys.argv[1:7]
body = json.dumps({
    "object_kind": "note",
    "user": {"id": int(uid), "username": "user%s" % uid},
    "project": {"id": 777},
    "object_attributes": {"note": note, "noteable_type": "MergeRequest"},
    "merge_request": {"iid": 14},
}).encode()
req = urllib.request.Request(f"http://127.0.0.1:{port}/webhooks/gitlab/{ep}", data=body, method="POST")
req.add_header("Content-Type", "application/json")
req.add_header("X-Gitlab-Event", "Note Hook")
req.add_header("X-Gitlab-Event-UUID", delivery)
req.add_header("X-Gitlab-Token", secret)
print(urllib.request.urlopen(req).status)
PYEOF
}

wait_preview() { # wait_preview APP_UUID PR_ID STATUS TRIES — polls the previews list
  local app=$1 pr=$2 want=$3 tries=$4 st=""
  for _ in $(seq 1 "$tries"); do
    st=$(api GET "/applications/$app/previews" | jsonq "([p['status'] for p in d['data'] if p['pr_id']==$pr] or ['absent'])[0]")
    # A destroyed preview leaves the live listing: absent IS destroyed.
    [ "$want" = "destroyed" ] && [ "$st" = "absent" ] && { echo destroyed; return 0; }
    [ "$st" = "$want" ] && { echo "$st"; return 0; }
    [ "$st" = "failed" ] && { echo failed; return 0; }
    sleep 3
  done
  echo "$st"
}

# --- 1. GitLab MR opened → protected preview + degraded feedback -----------------
say "GitLab MR opened: preview active, statuses + single upserted note"
send_gitlab_mr open mr-delivery-1 "$MRSHA" >/dev/null
[ "$(wait_preview "$GLU" 14 active 60)" = "active" ] || die "the GitLab preview did not become active: $(api GET "/applications/$GLU/previews")"
GLFQDN=$(api GET "/applications/$GLU/previews" | jsonq "[p['fqdn'] for p in d['data'] if p['pr_id']==14][0]")
CODE=$(docker exec "$DIND_CTR" curl -sk -o /dev/null -w '%{http_code}' --resolve "$GLFQDN:443:127.0.0.1" "https://$GLFQDN/")
[ "$CODE" = "401" ] || die "preview must be protected (got $CODE)"
GLCRED=$(api GET "/applications/$GLU/envs?preview=true&limit=100" | jsonq "[v['value'] for v in d['data'] if v['key']=='AKERDOCK_PREVIEW_BASIC_AUTH'][0]")
docker exec "$DIND_CTR" curl -sk -u "$GLCRED" --resolve "$GLFQDN:443:127.0.0.1" "https://$GLFQDN/" | grep -q gl-pr || die "preview not serving the MR content"
grep -q '"state": "running"' "$WORKDIR/gl-statuses.jsonl" || grep -q '"state":"running"' "$WORKDIR/gl-statuses.jsonl" || die "running commit status missing: $(cat "$WORKDIR/gl-statuses.jsonl" 2>/dev/null)"
grep -q '"success"' "$WORKDIR/gl-statuses.jsonl" || die "success commit status missing"
grep -q 'AkerDock/preview' "$WORKDIR/gl-statuses.jsonl" || die "status name must be AkerDock/preview"
[ "$(wc -l < "$WORKDIR/gl-comments.jsonl" | tr -d ' ')" = "1" ] || die "exactly ONE note must be created — got $(wc -l < "$WORKDIR/gl-comments.jsonl")"
grep -q "Preview ready" "$WORKDIR/gl-comment-updates.jsonl" || die "the note was not updated in place with the URL"
ok "MR preview active at $GLFQDN — 401/200, statuses named AkerDock/preview, one note updated in place"

# --- 2. label opt-in: an MR without the label deploys NOTHING --------------------
say "preview_require_label: MR 15 without the label is ignored"
patch_app "$GLU" '{"preview_require_label":"preview"}'
python3 - "$API_PORT" "$GLEP" "$GLSECRET" "$MRSHA" <<'PYEOF'
import json, sys, urllib.request
port, ep, secret, sha = sys.argv[1:5]
body = json.dumps({
    "object_kind": "merge_request", "project": {"id": 777},
    "object_attributes": {"iid": 15, "action": "open", "source_branch": "mr15",
                          "source_project_id": 777, "target_project_id": 777,
                          "last_commit": {"id": sha}},
    "labels": [{"title": "bug"}],
}).encode()
req = urllib.request.Request(f"http://127.0.0.1:{port}/webhooks/gitlab/{ep}", data=body, method="POST")
req.add_header("Content-Type", "application/json")
req.add_header("X-Gitlab-Event", "Merge Request Hook")
req.add_header("X-Gitlab-Event-UUID", "mr15-delivery-1")
req.add_header("X-Gitlab-Token", secret)
print(urllib.request.urlopen(req).status)
PYEOF
sleep 5
api GET "/applications/$GLU/previews" | grep -qE '"pr_id":\s*15' && die "an MR without the required label must deploy nothing"
patch_app "$GLU" '{"preview_require_label":null}'
ok "MR without the required label ignored, no preview row"

# --- 3. cancel-obsolete: the newest commit wins ----------------------------------
say "preview_cancel_obsolete_builds: a new commit cancels the in-flight build"
patch_app "$GLU" '{"preview_cancel_obsolete_builds":true}'
docker exec "$DIND_CTR" sh -c 'cd /srv/glrepo.git && git checkout -q -b mr14b refs/merge-requests/14/head && sed -i s/gl-pr/gl-pr2/ Dockerfile && git add -A && git commit -q -m pr2 && git update-ref refs/merge-requests/14/head refs/heads/mr14b && git checkout -q master && git branch -q -D mr14b'
SHA2=$(docker exec "$DIND_CTR" sh -c 'cd /srv/glrepo.git && git rev-parse refs/merge-requests/14/head' | tr -d ' \n')
send_gitlab_mr update mr-delivery-2 "$SHA2" '{"object_attributes":{"iid":14,"action":"update","oldrev":"'$MRSHA'","source_branch":"mr14","source_project_id":777,"target_project_id":777,"last_commit":{"id":"'$SHA2'"}}}' >/dev/null
sleep 2
docker exec "$DIND_CTR" sh -c 'cd /srv/glrepo.git && git checkout -q -b mr14c refs/merge-requests/14/head && sed -i s/gl-pr2/gl-pr3/ Dockerfile && git add -A && git commit -q -m pr3 && git update-ref refs/merge-requests/14/head refs/heads/mr14c && git checkout -q master && git branch -q -D mr14c'
SHA3=$(docker exec "$DIND_CTR" sh -c 'cd /srv/glrepo.git && git rev-parse refs/merge-requests/14/head' | tr -d ' \n')
send_gitlab_mr update mr-delivery-3 "$SHA3" '{"object_attributes":{"iid":14,"action":"update","oldrev":"'$SHA2'","source_branch":"mr14","source_project_id":777,"target_project_id":777,"last_commit":{"id":"'$SHA3'"}}}' >/dev/null
for _ in $(seq 1 60); do
  docker exec "$DIND_CTR" curl -sk -u "$GLCRED" --resolve "$GLFQDN:443:127.0.0.1" "https://$GLFQDN/" 2>/dev/null | grep -q gl-pr3 && break
  sleep 3
done
docker exec "$DIND_CTR" curl -sk -u "$GLCRED" --resolve "$GLFQDN:443:127.0.0.1" "https://$GLFQDN/" | grep -q gl-pr3 || die "the newest commit did not end up serving"
OBSOLETE=$(api GET "/applications/$GLU/deployments?limit=50" | jsonq "len([x for x in d['data'] if x['status'] in ('cancelled','superseded')])")
[ "$OBSOLETE" -ge 1 ] || die "the obsolete preview build was neither superseded nor cancelled"
# The content switches a few seconds before the deployment finishes and marks
# the preview active again — the command tests below assert on that status.
[ "$(wait_preview "$GLU" 14 active 40)" = "active" ] || die "preview did not settle active after the redeploys"
ok "obsolete build cancelled/superseded, newest commit serving"

# --- 4. comment commands: rights checked server-side -----------------------------
say "comment commands: /destroy needs write access, /deploy revives"
send_gitlab_note note-delivery-0 "/destroy" 901 >/dev/null
sleep 5
ST=$(api GET "/applications/$GLU/previews" | jsonq "[p['status'] for p in d['data'] if p['pr_id']==14][0]")
[ "$ST" = "active" ] || die "commands disabled: /destroy must have done nothing (got $ST)"
patch_app "$GLU" '{"preview_comment_commands_enabled":true}'
# Author 901 is NOT a member (stub answers 404): the command is refused.
send_gitlab_note note-delivery-1 "/destroy" 901 >/dev/null
sleep 5
ST=$(api GET "/applications/$GLU/previews" | jsonq "[p['status'] for p in d['data'] if p['pr_id']==14][0]")
[ "$ST" = "active" ] || die "/destroy from a non-member must be refused (got $ST)"
# Author 900 is a maintainer: the command executes.
send_gitlab_note note-delivery-2 "/destroy" 900 >/dev/null
[ "$(wait_preview "$GLU" 14 destroyed 40)" = "destroyed" ] || die "/destroy from a maintainer did not destroy the preview"
# /deploy revives the destroyed preview at the recorded SHA.
send_gitlab_note note-delivery-3 "/deploy" 900 >/dev/null
[ "$(wait_preview "$GLU" 14 active 60)" = "active" ] || die "/deploy did not revive the preview"
docker exec "$DIND_CTR" curl -sk -u "$GLCRED" --resolve "$GLFQDN:443:127.0.0.1" "https://$GLFQDN/" | grep -q gl-pr3 || die "revived preview not serving"
ok "commands opt-in, rights enforced via the API, /destroy then /deploy honoured"

# --- 5. MR merged → destroyed ----------------------------------------------------
say "MR merged: the preview is destroyed"
GLPV=$(api GET "/applications/$GLU/previews" | jsonq "[p['uuid'] for p in d['data'] if p['pr_id']==14][0]")
send_gitlab_mr merge mr-delivery-4 "$SHA3" >/dev/null
[ "$(wait_preview "$GLU" 14 destroyed 40)" = "destroyed" ] || die "the merged MR's preview was not destroyed"
docker exec "$DIND_CTR" docker inspect "$GLPV" >/dev/null 2>&1 && die "preview container survived the merge"
GONE=$(docker exec "$DIND_CTR" curl -sk -o /dev/null -w '%{http_code}' --resolve "$GLFQDN:443:127.0.0.1" "https://$GLFQDN/")
[ "$GONE" = "404" ] || die "preview route survived (got $GONE)"
ok "merge destroyed the preview: container and route gone"

# --- 6. Gitea PR on a COMPOSE application → ephemeral stack ----------------------
say "Gitea PR on a compose application: ephemeral stack, magic URL, protection"
python3 - "$API_PORT" "$GTEP" "$GTSECRET" "$PRSHA" opened pr21-delivery-1 <<'PYEOF'
import hashlib, hmac, json, sys, urllib.request
port, ep, secret, sha, action, delivery = sys.argv[1:7]
body = json.dumps({
    "action": action, "number": 21,
    "pull_request": {
        "number": 21, "title": "pr21",
        "merged": action == "closed",
        "head": {"ref": "pr21", "sha": sha, "repo": {"id": 555}},
        "base": {"repo": {"id": 555, "full_name": "e2e/gtapp"}},
    },
    "repository": {"id": 555, "full_name": "e2e/gtapp"},
}).encode()
sig = hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
req = urllib.request.Request(f"http://127.0.0.1:{port}/webhooks/gitea/{ep}", data=body, method="POST")
req.add_header("Content-Type", "application/json")
req.add_header("X-Gitea-Event", "pull_request")
req.add_header("X-Gitea-Delivery", delivery)
req.add_header("X-Gitea-Signature", sig)
print(urllib.request.urlopen(req).status)
PYEOF
[ "$(wait_preview "$GTU" 21 active 80)" = "active" ] || die "the compose preview did not become active: $(api GET "/applications/$GTU/previews")"
GTPV=$(api GET "/applications/$GTU/previews" | jsonq "[p['uuid'] for p in d['data'] if p['pr_id']==21][0]")
GTFQDN=$(api GET "/applications/$GTU/previews" | jsonq "[p['fqdn'] for p in d['data'] if p['pr_id']==21][0]")

# The stack is preview-scoped (INV-011): containers, network and volume all
# derive from the preview uuid — nothing collides with production.
docker exec "$DIND_CTR" docker inspect --format '{{.State.Status}}' "$GTPV-web" | grep -q running || die "preview web container missing"
docker exec "$DIND_CTR" docker inspect --format '{{.State.Status}}' "$GTPV-helper" | grep -q running || die "preview helper container missing"
docker exec "$DIND_CTR" docker network inspect "$GTPV" >/dev/null 2>&1 || die "preview stack network missing"
docker exec "$DIND_CTR" docker volume ls -q | grep -q "${GTPV}_data" || die "preview-prefixed volume missing"

# Protected route serving the PR's content; magic URL resolved per instance.
CODE=$(docker exec "$DIND_CTR" curl -sk -o /dev/null -w '%{http_code}' --resolve "$GTFQDN:443:127.0.0.1" "https://$GTFQDN/")
[ "$CODE" = "401" ] || die "compose preview must be protected (got $CODE)"
GTCRED=$(api GET "/applications/$GTU/envs?preview=true&limit=100" | jsonq "[v['value'] for v in d['data'] if v['key']=='AKERDOCK_PREVIEW_BASIC_AUTH'][0]")
docker exec "$DIND_CTR" curl -sk -u "$GTCRED" --resolve "$GTFQDN:443:127.0.0.1" "https://$GTFQDN/" | grep -q gt-pr21 || die "compose preview not serving the PR content"
docker exec "$DIND_CTR" docker exec "$GTPV-web" sh -c 'echo $PREVIEW_URL' | grep -q "https://$GTFQDN" || die "SERVICE_URL magic did not resolve to the preview URL"
grep -q '"success"' "$WORKDIR/gt-statuses.jsonl" || die "Gitea commit status missing: $(cat "$WORKDIR/gt-statuses.jsonl" 2>/dev/null)"
[ "$(wc -l < "$WORKDIR/gt-comments.jsonl" | tr -d ' ')" = "1" ] || die "exactly ONE Gitea comment must be created"
grep -q "Preview ready" "$WORKDIR/gt-comment-updates.jsonl" || die "the Gitea comment was not updated in place"
ok "compose preview: scoped stack (containers, network, volume), magic URL, 401/200, status + one comment"

# --- 7. PR closed → the whole stack is destroyed ---------------------------------
say "Gitea PR closed: the ephemeral stack is destroyed integrally"
python3 - "$API_PORT" "$GTEP" "$GTSECRET" "$PRSHA" closed pr21-delivery-2 <<'PYEOF'
import hashlib, hmac, json, sys, urllib.request
port, ep, secret, sha, action, delivery = sys.argv[1:7]
body = json.dumps({
    "action": action, "number": 21,
    "pull_request": {
        "number": 21, "title": "pr21", "merged": True,
        "head": {"ref": "pr21", "sha": sha, "repo": {"id": 555}},
        "base": {"repo": {"id": 555, "full_name": "e2e/gtapp"}},
    },
    "repository": {"id": 555, "full_name": "e2e/gtapp"},
}).encode()
sig = hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
req = urllib.request.Request(f"http://127.0.0.1:{port}/webhooks/gitea/{ep}", data=body, method="POST")
req.add_header("Content-Type", "application/json")
req.add_header("X-Gitea-Event", "pull_request")
req.add_header("X-Gitea-Delivery", delivery)
req.add_header("X-Gitea-Signature", sig)
print(urllib.request.urlopen(req).status)
PYEOF
[ "$(wait_preview "$GTU" 21 destroyed 40)" = "destroyed" ] || die "the closed PR's compose preview was not destroyed"
docker exec "$DIND_CTR" docker inspect "$GTPV-web" >/dev/null 2>&1 && die "web container survived the close"
docker exec "$DIND_CTR" docker inspect "$GTPV-helper" >/dev/null 2>&1 && die "helper container survived the close"
docker exec "$DIND_CTR" docker network inspect "$GTPV" >/dev/null 2>&1 && die "stack network survived the close"
docker exec "$DIND_CTR" docker volume ls -q | grep -q "${GTPV}_data" && die "stack volume survived the close"
GONE=$(docker exec "$DIND_CTR" curl -sk -o /dev/null -w '%{http_code}' --resolve "$GTFQDN:443:127.0.0.1" "https://$GTFQDN/")
[ "$GONE" = "404" ] || die "compose preview route survived (got $GONE)"
ok "compose preview destroyed integrally: containers, network, volume and route gone"
