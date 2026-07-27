---
name: verify
description: Run AkerDock locally against a throwaway Postgres and drive the /auth API or the embedded UI to verify a change under real conditions.
---

# Verifying AkerDock locally

## Run the app (all-in-one)

```bash
# Throwaway Postgres (the official image ships citext)
docker run -d --rm --name akd-verify -e POSTGRES_USER=akerdock \
  -e POSTGRES_PASSWORD=verify -e POSTGRES_DB=akerdock -p 15477:5432 postgres:18-alpine

export AKERDOCK_DATABASE_URL="postgres://akerdock:verify@localhost:15477/akerdock?sslmode=disable"
export AKERDOCK_MASTER_KEY="1:$(openssl rand -base64 32)"
export AKERDOCK_ROOT_EMAIL=root@example.com AKERDOCK_ROOT_NAME=Root \
  AKERDOCK_ROOT_PASSWORD="a-very-long-verify-password"
export AKERDOCK_PORT=18475
export AKERDOCK_DATA_DIR=$(mktemp -d)   # otherwise fatal: /var/lib/akerdock not creatable

go build -o /tmp/akerdock ./cmd/akerdock && /tmp/akerdock
```

Goose migrations apply at startup; the bootstrap creates the root user from
the `AKERDOCK_ROOT_*` variables. The `server.validate` job for the localhost
server fails in a loop (no SSH) — expected noise, not an outage.

## Pitfalls

- **The embedded UI comes from `internal/web/dist`** (go:embed), NOT from
  `web/dist`. After a UI change: `npm --prefix web run build` then
  `cp -r web/dist/akerdock-web/browser/. internal/web/dist/` (the
  `make web` target), then rebuild the binary. Without this you are driving
  the old UI. `internal/web/dist` is tracked by git and gets committed along
  with UI changes.
- **`/auth` rate limit**: 30 req/min per IP — space out probes or sleep 30 s.
- **Lockout**: 5 failures (login or MFA code) lock the account for 15 min.
  To unlock: `docker exec akd-verify psql -U akerdock -d akerdock -c
  "UPDATE users SET failed_login_count=0, locked_until=NULL;"`.

## Drive it

- **API**: `POST /auth/login` (JSON email/password) → cookies + `csrf_token`;
  `/auth/*` mutations require the `X-CSRF-Token` header. The v1 contract lives
  under `/api/v1` (Bearer).
- **UI**: Playwright with the system Chrome, without downloading a browser:
  `chromium.launch({ channel: 'chrome', headless: true })` (install the
  `playwright` package alone in a temporary folder). Useful routes: `/sign-in`,
  `/security`, `/applications`.
- **TOTP**: generate codes with an independent implementation (python3:
  `hmac` + `base32decode`, SHA-1, 6 digits, 30 s step) — proves
  interoperability with real apps. Anti-replay burns the current step:
  for an immediate second code, take the next step (+1).
- **OAuth/OIDC**: fake IdP in Go stdlib on `http://localhost:9091`
  (discovery + JWKS + auto-approved authorize + token signing a real RS256
  JWT, PKCE **verified** on the IdP side, `POST /control` to change sub/email
  between scenarios) — `ValidateIssuer` tolerates http on localhost only.
  Configure via `PUT /api/v1/system/oauth-providers/oidc` (root session +
  `X-CSRF-Token`). `registration_enabled` is `false` by default: enable it
  in SQL and wait ~4 s (settings cache TTL).
