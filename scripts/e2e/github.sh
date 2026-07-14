# E2E shard: the GitHub App integration (git-webhook-protocols §2) and the PR
# previews it powers (§20.4, ADR-011) — against a STUB of the GitHub API.
#
# The stub serves HTTPS with a self-signed certificate that akerdock trusts
# through AKERDOCK_GITHUB_CA_FILE — exactly how a GitHub Enterprise Server
# with a private CA is trusted (protocols §2.6): nothing test-only enters
# the product.
#
# What the shard proves:
#   1. manifest flow: draft → callback(code+state) → credentials converted,
#      encrypted, git source created; the state is single-use.
#   2. installation (setup redirect) + repository discovery through a REAL
#      installation token exchange (App JWT → token).
#   3. application created from the discovered repository, cloned, deployed.
#   4. push webhook (HMAC-signed) → auto-deploy of the new commit.
#   5. PR opened → protected preview (its own container, basic auth, noindex),
#      single upserted PR comment with the URL, check run.
#   6. PR closed → the preview instance is destroyed.

GITHUB_PORT=$((18300 + IDX * 10))

# --- the GitHub API stub --------------------------------------------------------
say "starting the GitHub API stub (HTTPS, self-signed)"
# A self-signed leaf cannot act as its own trust anchor: Go refuses a parent
# that is not a CA. The stub's certificate is therefore a real CA certificate
# (basicConstraints CA:TRUE, keyCertSign) — which is also what a GHES private
# CA looks like.
cat > "$WORKDIR/gh-openssl.cnf" <<'CNF'
[req]
distinguished_name = dn
x509_extensions = v3_ca
prompt = no
[dn]
CN = github-stub
[v3_ca]
basicConstraints = critical,CA:TRUE
keyUsage = critical,digitalSignature,keyCertSign
subjectAltName = IP:127.0.0.1
CNF
openssl req -x509 -newkey rsa:2048 -keyout "$WORKDIR/gh-key.pem" -out "$WORKDIR/gh-cert.pem" \
  -days 1 -nodes -config "$WORKDIR/gh-openssl.cnf" >/dev/null 2>&1 \
  || die "stub certificate generation failed"
openssl x509 -in "$WORKDIR/gh-cert.pem" -noout -text | grep -q "CA:TRUE" || die "stub certificate is not a CA"
# The App's RSA key (PKCS#1 — LibreSSL's genrsa default): returned by the
# manifest conversion, then used by akerdock to sign REAL RS256 JWTs.
openssl genrsa -out "$WORKDIR/app-key.pem" 2048 >/dev/null 2>&1 || die "app key generation failed"
head -1 "$WORKDIR/app-key.pem" | grep -q "BEGIN" || die "app key is empty"

GIT_HOST_FILE="$WORKDIR/git-host"
cat > "$WORKDIR/github-stub.py" <<'PYSTUB'
import http.server, json, ssl, sys, time, datetime

workdir, port = sys.argv[1], int(sys.argv[2])
app_pem = open(workdir + "/app-key.pem").read()
git_host = open(workdir + "/git-host").read().strip()
WEBHOOK_SECRET = "e2e-webhook-secret"
state = {"tokens": 0, "conversions": 0}

class H(http.server.BaseHTTPRequestHandler):
    def _json(self, code, obj):
        raw = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def _record(self, name, body):
        with open(workdir + "/gh-" + name + ".jsonl", "a") as f:
            f.write(body.decode() + "\n")

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("content-length", 0)))
        p = self.path
        if p.startswith("/api/v3/app-manifests/") and p.endswith("/conversions"):
            state["conversions"] += 1
            code = p.split("/")[4]
            if code != "one-shot-code":
                return self._json(404, {"message": "unknown code"})
            return self._json(201, {
                "id": 4242, "slug": "akerdock-e2e", "name": "akerdock-e2e",
                "client_id": "cid", "client_secret": "csec",
                "webhook_secret": WEBHOOK_SECRET, "pem": app_pem,
                "html_url": "https://127.0.0.1:%d/apps/akerdock-e2e" % port,
            })
        if p == "/api/v3/app/installations/77/access_tokens":
            auth = self.headers.get("Authorization", "")
            if not auth.startswith("Bearer ey"):
                return self._json(401, {"message": "bad app jwt"})
            state["tokens"] += 1
            with open(workdir + "/gh-tokens.count", "w") as f:
                f.write(str(state["tokens"]))
            exp = datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(hours=1)
            return self._json(201, {"token": "ghs_e2e", "expires_at": exp.strftime("%Y-%m-%dT%H:%M:%SZ")})
        if "/check-runs" in p:
            self._record("checks", body)
            return self._json(201, {"id": 9, "name": "check"})
        if "/deployments/5/statuses" in p:
            self._record("deployment-statuses", body)
            return self._json(201, {"id": 6})
        if p.endswith("/deployments"):
            self._record("deployments", body)
            return self._json(201, {"id": 5})
        if "/issues/12/comments" in p:
            self._record("comments", body)
            return self._json(201, {"id": 31})
        return self._json(404, {"message": "unexpected " + p})

    def do_PATCH(self):
        body = self.rfile.read(int(self.headers.get("content-length", 0)))
        if "/issues/comments/31" in self.path:
            self._record("comment-updates", body)
            return self._json(200, {"id": 31})
        if "/check-runs/" in self.path:
            self._record("checks", body)
            return self._json(200, {"id": 9})
        return self._json(404, {"message": "unexpected " + self.path})

    def do_GET(self):
        p = self.path
        if "/installation/repositories" in p:
            if self.headers.get("Authorization") != "Bearer ghs_e2e":
                return self._json(401, {"message": "bad installation token"})
            return self._json(200, {"total_count": 1, "repositories": [{
                "id": 555, "full_name": "e2e/app", "default_branch": "master",
                # The engine derives the clone URL from html_url + ".git": the
                # stub points it at the shard's git daemon.
                "html_url": "git://%s/crepo" % git_host, "private": True,
            }]})
        if "/repos/e2e/app/issues/12/comments" in p:
            existing = []
            try:
                lines = open(workdir + "/gh-comments.jsonl").read().strip().split("\n")
                if lines and lines[0]:
                    existing = [{"id": 31, "body": json.loads(lines[0])["body"]}]
            except FileNotFoundError:
                pass
            return self._json(200, existing)
        return self._json(404, {"message": "unexpected " + p})

    def log_message(self, *a):
        pass

server = http.server.HTTPServer(("127.0.0.1", port), H)
ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
ctx.load_cert_chain(workdir + "/gh-cert.pem", workdir + "/gh-key.pem")
server.socket = ctx.wrap_socket(server.socket, server_side=True)
server.serve_forever()
PYSTUB

# --- git fixture (cloned via the URL the stub advertises) -----------------------
docker exec "$DIND_CTR" sh -c '
  apk add --no-cache git git-daemon >/dev/null 2>&1
  rm -rf /srv/crepo.git && mkdir -p /srv/crepo.git && cd /srv/crepo.git
  git init -q && git config user.email e2e@example.com && git config user.name e2e
  printf "FROM nginx:alpine\nRUN echo gh-v1 > /usr/share/nginx/html/index.html\nHEALTHCHECK --interval=2s --retries=5 CMD wget -q -O /dev/null http://127.0.0.1/ || exit 1\n" > Dockerfile
  git add -A && git commit -q -m v1
  git daemon --base-path=/srv --export-all --enable=receive-pack --reuseaddr --detach /srv
' || die "git fixture failed"
docker exec "$DIND_CTR" hostname -i | awk '{print $1}' > "$GIT_HOST_FILE"

# A stub left behind by a crashed run would hold the port and serve its own
# (now untrusted) certificate — a failure that looks like a TLS bug in the
# product. Refuse to start on top of one.
if lsof -nP -iTCP:"$GITHUB_PORT" -sTCP:LISTEN >/dev/null 2>&1; then
  die "port $GITHUB_PORT is already in use — a stray GitHub stub is still running"
fi
python3 "$WORKDIR/github-stub.py" "$WORKDIR" "$GITHUB_PORT" &
GITHUB_STUB_PID=$!
STUB_UP=0
for _ in $(seq 1 20); do
  curl -sk "https://127.0.0.1:${GITHUB_PORT}/api/v3/x" >/dev/null 2>&1 && { STUB_UP=1; break; }
  sleep 0.5
done
[ "$STUB_UP" = 1 ] || die "the GitHub stub did not come up on $GITHUB_PORT"

# The instance must trust the stub's CA (the GHES-with-private-CA path) and
# needs an FQDN for the manifest URLs. Restart with the trust in place.
say "restarting akerdock trusting the stub CA"
docker exec "$PG_CTR" psql -U postgres -d akerdock -q -c "UPDATE instance_settings SET fqdn = 'akerdock.e2e.test'"
kill -TERM "$API_PID" 2>/dev/null; sleep 1
export AKERDOCK_GITHUB_CA_FILE="$WORKDIR/gh-cert.pem"
start_akerdock
ok "instance up with the stub as its GitHub"

# --- 1. manifest flow -----------------------------------------------------------
say "manifest flow: draft, callback, converted credentials, single-use state"
FLOW=$(api POST /github-apps "{\"api_url\":\"https://127.0.0.1:${GITHUB_PORT}/api/v3\",\"html_url\":\"https://127.0.0.1:${GITHUB_PORT}\"}")
GHU=$(echo "$FLOW" | jsonq "d['github_app']['uuid']")
STATE=$(echo "$FLOW" | jsonq "d['state']")
echo "$FLOW" | jsonq "d['manifest']['hook_attributes']['url']" | grep -q "webhooks/github/apps/$GHU" || die "manifest hook url wrong"

CB=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${API_PORT}/webhooks/github/manifest/callback?code=one-shot-code&state=${STATE}")
[ "$CB" = "302" ] || die "manifest callback failed (got $CB)"
[ "$(api GET "/github-apps/$GHU" | jsonq "d['app_id']")" = "4242" ] || die "credentials not persisted"
# Replay: the state is single-use (anti-CSRF) — a second callback must 404.
CB2=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${API_PORT}/webhooks/github/manifest/callback?code=one-shot-code&state=${STATE}")
[ "$CB2" = "404" ] || die "state replay must be refused (got $CB2)"
ok "app converted (app_id 4242), state single-use"

# --- 2. installation + discovery -------------------------------------------------
say "installation and repository discovery through a real token exchange"
curl -s -o /dev/null "http://127.0.0.1:${API_PORT}/webhooks/github/apps/${GHU}/setup?installation_id=77&setup_action=install"
[ "$(api GET "/github-apps/$GHU" | jsonq "d['is_installed']")" = "True" ] || die "installation not recorded"
REPOS=$(api GET "/github-apps/$GHU/repositories?refresh=true")
[ "$(echo "$REPOS" | jsonq "d['data'][0]['full_name']")" = "e2e/app" ] || die "discovery failed: $REPOS"
[ "$(cat "$WORKDIR/gh-tokens.count")" -ge 1 ] || die "no installation token was minted"
ok "installed (77), repository e2e/app discovered via App JWT → installation token"

# --- 3. application from the discovered repo ------------------------------------
say "application from the discovered repository, cloned and deployed"
GHAPP=$(python3 - "$PU" "$EU" "$S" "$GHU" <<'PYEOF'
import json, sys
print(json.dumps({
    "source_type": "git", "name": "ghapp",
    "project_uuid": sys.argv[1], "environment_uuid": sys.argv[2], "server_uuid": sys.argv[3],
    "git_repository": "ignored", "github_app_uuid": sys.argv[4], "repository_full_name": "e2e/app",
    "git_branch": "master", "build_pack": "dockerfile",
    "domains": ["ghapp.e2e.test"], "ports_exposes": "80",
}))
PYEOF
)
GAU=$(api POST /applications "$GHAPP" | jsonq "d['uuid']")
GDU=$(api POST "/applications/$GAU/deploy" | jsonq "d['deployment_uuid']")
[ "$(wait_deployment "$GDU" 240)" = "succeeded" ] || die_deployment "$GDU" "github app deployment failed"
wait_route ghapp.e2e.test 301
docker exec "$DIND_CTR" curl -sk --resolve ghapp.e2e.test:443:127.0.0.1 https://ghapp.e2e.test/ | grep -q gh-v1 || die "app not serving"
ok "cloned from the discovered repo and routed"

# --- 4. push webhook → auto-deploy ------------------------------------------------
say "signed push webhook: auto-deploy of the new commit"
docker exec "$DIND_CTR" sh -c 'cd /srv/crepo.git && sed -i s/gh-v1/gh-v2/ Dockerfile && git add -A && git commit -q -m v2'
SHA2=$(docker exec "$DIND_CTR" sh -c 'cd /srv/crepo.git && git rev-parse HEAD' | tr -d ' \n')
python3 - "$WORKDIR" "$API_PORT" "$GHU" "$SHA2" <<'PYEOF'
import hmac, hashlib, json, sys, urllib.request
workdir, port, ghu, sha = sys.argv[1:5]
body = json.dumps({
    "ref": "refs/heads/master", "after": sha, "before": "0"*40,
    "repository": {"id": 555, "full_name": "e2e/app"},
    "head_commit": {"id": sha, "message": "v2", "added": [], "modified": ["Dockerfile"], "removed": []},
    "commits": [{"id": sha, "message": "v2", "added": [], "modified": ["Dockerfile"], "removed": []}],
}).encode()
sig = "sha256=" + hmac.new(b"e2e-webhook-secret", body, hashlib.sha256).hexdigest()
req = urllib.request.Request(f"http://127.0.0.1:{port}/webhooks/github/apps/{ghu}", data=body, method="POST")
req.add_header("Content-Type", "application/json")
req.add_header("X-GitHub-Event", "push")
req.add_header("X-GitHub-Delivery", "delivery-push-1")
req.add_header("X-Hub-Signature-256", sig)
print(urllib.request.urlopen(req).status)
PYEOF
for _ in $(seq 1 60); do
  docker exec "$DIND_CTR" curl -sk --resolve ghapp.e2e.test:443:127.0.0.1 https://ghapp.e2e.test/ 2>/dev/null | grep -q gh-v2 && break
  sleep 3
done
docker exec "$DIND_CTR" curl -sk --resolve ghapp.e2e.test:443:127.0.0.1 https://ghapp.e2e.test/ | grep -q gh-v2 || die "push did not auto-deploy v2"
ok "push webhook deployed the new commit"

# --- 5. PR opened → protected preview + upserted comment --------------------------
say "PR opened: protected preview, single comment, check run"
GV=$(api GET "/applications/$GAU" | jsonq "d['version']")
curl -sf -X PATCH -H "Authorization: Bearer $ROOT_TOKEN" -H "Content-Type: application/json" \
  -H "If-Match: \"$GV\"" \
  -d '{"previews_enabled":true,"preview_protection":"basic_auth"}' \
  "$B/applications/$GAU" >/dev/null || die "enabling previews failed"

# GitHub publishes every PR head under refs/pull/<n>/head of the BASE repo —
# including a fork's. The fixture does the same, because that is the ref the
# engine fetches for previews.
docker exec "$DIND_CTR" sh -c 'cd /srv/crepo.git && git checkout -q -b feature && sed -i s/gh-v2/gh-pr/ Dockerfile && git add -A && git commit -q -m pr && git checkout -q master && git update-ref refs/pull/12/head refs/heads/feature && git branch -q -D feature'
PRSHA=$(docker exec "$DIND_CTR" sh -c 'cd /srv/crepo.git && git rev-parse refs/pull/12/head' | tr -d ' \n')
send_pr_event() { # send_pr_event ACTION DELIVERY
  python3 - "$API_PORT" "$GHU" "$PRSHA" "$1" "$2" <<'PYEOF'
import hmac, hashlib, json, sys, urllib.request
port, ghu, sha, action, delivery = sys.argv[1:6]
body = json.dumps({
    "action": action, "number": 12,
    "pull_request": {
        "draft": False, "merged": action == "closed",
        "head": {"ref": "feature", "sha": sha, "repo": {"id": 555}},
        "base": {"repo": {"id": 555}},
    },
}).encode()
sig = "sha256=" + hmac.new(b"e2e-webhook-secret", body, hashlib.sha256).hexdigest()
req = urllib.request.Request(f"http://127.0.0.1:{port}/webhooks/github/apps/{ghu}", data=body, method="POST")
req.add_header("Content-Type", "application/json")
req.add_header("X-GitHub-Event", "pull_request")
req.add_header("X-GitHub-Delivery", delivery)
req.add_header("X-Hub-Signature-256", sig)
print(urllib.request.urlopen(req).status)
PYEOF
}
send_pr_event opened delivery-pr-1 >/dev/null

PVFQDN=""
for _ in $(seq 1 60); do
  ST=$(api GET "/applications/$GAU/previews" | jsonq "(d['data'][0]['status'], d['data'][0]['fqdn']) if d['data'] else ('none','')")
  case "$ST" in "('active',"*) PVFQDN=$(api GET "/applications/$GAU/previews" | jsonq "d['data'][0]['fqdn']"); break;; esac
  sleep 3
done
[ -n "$PVFQDN" ] || die "preview did not become active: $(api GET "/applications/$GAU/previews")"

# Protected by default: anonymous → 401; with the generated credential → 200.
CODE=$(docker exec "$DIND_CTR" curl -sk -o /dev/null -w '%{http_code}' --resolve "$PVFQDN:443:127.0.0.1" "https://$PVFQDN/")
[ "$CODE" = "401" ] || die "preview must be protected (got $CODE)"
# The credential lives in the PREVIEW variable set, not production (INV-010).
PVENVS=$(api GET "/applications/$GAU/envs?preview=true&limit=100")
CRED=$(echo "$PVENVS" | jsonq "[v['value'] for v in d['data'] if v['key']=='AKERDOCK_PREVIEW_BASIC_AUTH'][0]")
[ -n "$CRED" ] || die "the generated preview credential is not readable: $PVENVS"
# And it must NOT pollute the production set.
api GET "/applications/$GAU/envs?limit=100" | grep -q AKERDOCK_PREVIEW_BASIC_AUTH && die "the preview credential leaked into the production set"
BODY=$(docker exec "$DIND_CTR" curl -sk -u "$CRED" --resolve "$PVFQDN:443:127.0.0.1" "https://$PVFQDN/")
echo "$BODY" | grep -q gh-pr || die "preview not serving the PR content: $BODY"
grep -q "Preview ready" "$WORKDIR/gh-comments.jsonl" "$WORKDIR/gh-comment-updates.jsonl" 2>/dev/null || die "PR comment missing"
grep -qE '"conclusion":\s*"success"' "$WORKDIR/gh-checks.jsonl" || die "success check run missing: $(cat "$WORKDIR/gh-checks.jsonl" 2>/dev/null)"
grep -q '"transient_environment":true' "$WORKDIR/gh-deployments.jsonl" || die "GitHub deployment (View deployment) missing: $(cat "$WORKDIR/gh-deployments.jsonl" 2>/dev/null)"
grep -qE '"state":\s*"success"' "$WORKDIR/gh-deployment-statuses.jsonl" || die "deployment status missing"
ok "preview active at $PVFQDN — 401 anonymous, 200 with the generated credential, comment + check + deployment posted"

# --- 6. fork PR: nothing is built without an explicit approval (INV-010) ---------
say "fork PR: ignored by default, deployed only after a maintainer approves"
docker exec "$DIND_CTR" sh -c 'cd /srv/crepo.git && git checkout -q -b forkbranch master && sed -i s/gh-v2/gh-fork/ Dockerfile && git add -A && git commit -q -m fork && git checkout -q master && git update-ref refs/pull/13/head refs/heads/forkbranch && git branch -q -D forkbranch'
FORKSHA=$(docker exec "$DIND_CTR" sh -c 'cd /srv/crepo.git && git rev-parse refs/pull/13/head' | tr -d ' \n')
send_fork_event() { # send_fork_event DELIVERY
  python3 - "$API_PORT" "$GHU" "$FORKSHA" "$1" <<'PYEOF'
import hmac, hashlib, json, sys, urllib.request
port, ghu, sha, delivery = sys.argv[1:5]
body = json.dumps({
    "action": "opened", "number": 13,
    "pull_request": {
        "draft": False, "merged": False,
        # A fork: head repo id differs from the base repo id.
        "head": {"ref": "forkbranch", "sha": sha, "repo": {"id": 999}},
        "base": {"repo": {"id": 555}},
    },
}).encode()
sig = "sha256=" + hmac.new(b"e2e-webhook-secret", body, hashlib.sha256).hexdigest()
req = urllib.request.Request(f"http://127.0.0.1:{port}/webhooks/github/apps/{ghu}", data=body, method="POST")
req.add_header("Content-Type", "application/json")
req.add_header("X-GitHub-Event", "pull_request")
req.add_header("X-GitHub-Delivery", delivery)
req.add_header("X-Hub-Signature-256", sig)
print(urllib.request.urlopen(req).status)
PYEOF
}

# Fork approval disabled (the default): the PR is ignored outright — no row,
# nothing built (INV-010).
send_fork_event delivery-fork-1 >/dev/null
sleep 5
api GET "/applications/$GAU/previews" | grep -qE '"pr_id":\s*13' && die "a fork PR must be ignored while approvals are disabled"

# Enable approvals: the fork PR now creates a preview that WAITS.
GV2=$(api GET "/applications/$GAU" | jsonq "d['version']")
curl -sf -X PATCH -H "Authorization: Bearer $ROOT_TOKEN" -H "Content-Type: application/json" \
  -H "If-Match: \"$GV2\"" -d '{"preview_fork_approval_enabled":true}' "$B/applications/$GAU" >/dev/null \
  || die "enabling fork approvals failed"
send_fork_event delivery-fork-2 >/dev/null
FPV=""
for _ in $(seq 1 20); do
  FPV=$(api GET "/applications/$GAU/previews" | jsonq "[p['uuid'] for p in d['data'] if p['pr_id']==13][0] if [p for p in d['data'] if p['pr_id']==13] else ''")
  [ -n "$FPV" ] && break
  sleep 2
done
[ -n "$FPV" ] || die "the fork preview row was not created"
FSTATUS=$(api GET "/applications/$GAU/previews" | jsonq "[p['status'] for p in d['data'] if p['pr_id']==13][0]")
[ "$FSTATUS" = "queued" ] || die "an unapproved fork must stay queued (got $FSTATUS)"
docker exec "$DIND_CTR" docker inspect "$FPV" >/dev/null 2>&1 && die "an unapproved fork must build NOTHING"

# The maintainer approves: it deploys, fetched through refs/pull/13/head —
# the fork's commit does not exist in the base repo any other way.
api POST "/applications/$GAU/previews/$FPV/approve" >/dev/null || die "approval failed"
FFQDN=""
for _ in $(seq 1 60); do
  FSTATUS=$(api GET "/applications/$GAU/previews" | jsonq "[p['status'] for p in d['data'] if p['pr_id']==13][0]")
  [ "$FSTATUS" = "active" ] && { FFQDN=$(api GET "/applications/$GAU/previews" | jsonq "[p.get('fqdn','') for p in d['data'] if p['pr_id']==13][0]"); break; }
  [ "$FSTATUS" = "failed" ] && die "the approved fork preview failed to deploy"
  sleep 3
done
[ -n "$FFQDN" ] || die "the approved fork preview has no URL (status: $FSTATUS)"
docker exec "$DIND_CTR" curl -sk -u "$CRED" --resolve "$FFQDN:443:127.0.0.1" "https://$FFQDN/" | grep -q gh-fork \
  || die "the fork preview is not serving the fork's commit"
ok "fork ignored by default, queued without building once approvals are on, deployed from refs/pull/13/head after approval"

# --- 7. PR closed → destroyed ------------------------------------------------------
say "PR closed: the preview instance is destroyed"
PVUUID=$(api GET "/applications/$GAU/previews" | jsonq "d['data'][0]['uuid']")
send_pr_event closed delivery-pr-2 >/dev/null
for _ in $(seq 1 40); do
  [ "$(api GET "/applications/$GAU/previews" | jsonq "d['data'][0]['status'] if d['data'] else 'destroyed'")" = "destroyed" ] && break
  sleep 3
done
docker exec "$DIND_CTR" docker inspect "$PVUUID" >/dev/null 2>&1 && die "preview container survived"
GONE=$(docker exec "$DIND_CTR" curl -sk -o /dev/null -w '%{http_code}' --resolve "$PVFQDN:443:127.0.0.1" "https://$PVFQDN/")
[ "$GONE" = "404" ] || die "preview route survived (got $GONE)"
ok "preview destroyed: container and route gone"
