# Runbook — Instance installation

> References: PRD §14.1–14.2, §10.2, §7.5; ADR-021 (2-service compose distribution); ADR-003 (master key); data dictionary §11.7 (`instance_settings`), §6.3 (`private_keys`).

## Symptoms

Not applicable — planned operation. This runbook covers the initial installation of an AkerDock instance on a blank machine.

## Impact

No existing workload is affected: the instance only does UI + SSH deployments + monitoring (PRD §3.3, INV-007).

## Prerequisites

- **Machine**: Linux AMD64 or ARM64, minimum **2 vCPU / 2 GB RAM / 30 GB disk** (§14.1 — AkerDock commits to staying responsive on this footprint, §16.1(6)).
- **Docker Engine ≥ 24 + Compose v2** (snap not supported, §3.1). Check:
  ```sh
  docker version --format '{{.Server.Version}}'
  docker compose version
  ```
- **PostgreSQL 15+**: the compose's PostgreSQL image MUST be ≥ 15 (`UNIQUE NULLS NOT DISTINCT`, data dictionary §2) — use the pinned tag from the AkerDock release.
- **Network**: a single exposed port for the control plane (§27.1) — **8080 (normative: spec [instance-config](../specs/instance-config.md) §2)**; 80/443 only if the `localhost` server also acts as a target server with proxy. A DNS record for the instance FQDN (recommended, §14.2).
- Root access on the machine.

## Step-by-step resolution (installation procedure)

### 1. Create the instance directory tree

```sh
mkdir -p /var/lib/akerdock/keys /var/lib/akerdock/postgres /var/lib/akerdock/backups
chmod 0700 /var/lib/akerdock/keys
```

### 2. Generate the encryption master key (ADR-003, §23.2, §27.3)

One line per key version in the format `<version>:<base64 32-byte key>` **(normative: spec [instance-config](../specs/instance-config.md) §3)**. The file is mounted read-only in the `akerdock` container, which runs as **nonroot distroless (uid 65532)**: it MUST be owned by that uid, otherwise startup fails with `master key file … permission denied`:

```sh
umask 077
printf '1:%s\n' "$(openssl rand -base64 32)" > /var/lib/akerdock/keys/master.key
chown 65532:65532 /var/lib/akerdock/keys/master.key
chmod 0600 /var/lib/akerdock/keys/master.key
```

(root reads the file regardless of its permissions — the off-machine backup remains possible.)

⚠️ **Deferred point of no return**: from the moment the first secret is stored, **losing this file renders all secrets unrecoverable** (ADR-003). Immediately copy `master.key` to a safe location **off the machine** (team password manager, vault), **separate from the database backups** (an attacker who gets both reads everything, §23.1).

### 3. Write the configuration

`/var/lib/akerdock/.env` — variable names **(normative: spec [instance-config](../specs/instance-config.md) §2)**:

```sh
cat > /var/lib/akerdock/.env <<'EOF'
AKERDOCK_TAG=v1.0.0                  # explicit image tag, never "latest"
AKERDOCK_PORT=8080
POSTGRES_PASSWORD=<generated: openssl rand -hex 24>
# Non-interactive bootstrap of the first root user (§10.2) — strict email/name/password validation
AKERDOCK_ROOT_EMAIL=admin@example.com
AKERDOCK_ROOT_NAME=Admin
AKERDOCK_ROOT_PASSWORD=<strong password>
EOF
chmod 0600 /var/lib/akerdock/.env
```

`/var/lib/akerdock/docker-compose.yml` **(normative: spec [instance-config](../specs/instance-config.md) §4 — 2 services, a single exposed port, compliant with ADR-021; the spec's reference file uses the lowercase identifiers `akerdock` and named volumes)**:

```yaml
services:
  AkerDock:
    image: ghcr.io/deepteams/akerdock:${AKERDOCK_TAG}
    command: ["all-in-one"]                       # all-in-one/api/worker modes (§18.2)
    restart: unless-stopped
    ports:
      - "${AKERDOCK_PORT}:8080"
    environment:
      AKERDOCK_DATABASE_URL: postgres://AkerDock:${POSTGRES_PASSWORD}@postgres:5432/AkerDock?sslmode=disable
      AKERDOCK_MASTER_KEY_FILE: /run/secrets/master.key
      AKERDOCK_ROOT_EMAIL: ${AKERDOCK_ROOT_EMAIL}
      AKERDOCK_ROOT_NAME: ${AKERDOCK_ROOT_NAME}
      AKERDOCK_ROOT_PASSWORD: ${AKERDOCK_ROOT_PASSWORD}
    volumes:
      - ./keys/master.key:/run/secrets/master.key:ro
    depends_on:
      postgres:
        condition: service_healthy

  postgres:
    image: postgres:18                            # tag pinned by the release
    restart: unless-stopped
    environment:
      POSTGRES_USER: AkerDock
      POSTGRES_DB: AkerDock
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}
      PGDATA: /var/lib/postgresql/data   # PG18 moved the default PGDATA: pinning it keeps the data in the mounted volume
    volumes:
      - ./postgres:/var/lib/postgresql/data
      - ./backups:/backups
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U AkerDock"]
      interval: 5s
      timeout: 3s
      retries: 10
```

### 4. Start

```sh
cd /var/lib/akerdock
docker compose up -d
```

On first startup, the binary applies the **versioned SQL migrations** (ADR-025) then creates the **first root user** from the bootstrap variables (§10.2) — creation fails explicitly if email/name/password do not pass strict validation. Once the root is created, remove `AKERDOCK_ROOT_PASSWORD` from the `.env` **(normative: spec [instance-config](../specs/instance-config.md) §6 — the bootstrap variables are only read if no user exists, and consumed only once)**.

As soon as the first team exists, the bootstrap also pre-registers the **`localhost`** server (the host machine, reached over SSH via `host.docker.internal` with the instance key — spec instance-config §6.2). `install.sh` automatically authorizes the instance public key for the installing user, and the scheduler retries validation every ~5 minutes (for 24 h): the server moves to `ready` on its own, with no action. Prerequisite: an active SSH server on the host. Manual installation (without `install.sh`): add `/var/lib/akerdock/ssh/instance_ed25519.pub` to the `authorized_keys` of `AKERDOCK_LOCALHOST_USER`. Once deleted, this server is never recreated.

### 5. Onboarding and SSH keys

1. Log in to the UI (`http://<host>:8080`), follow the guided onboarding (§14.2): first team, first server, first resource. Enable the root's **TOTP 2FA** immediately (§10.2).
2. Generate an SSH key for the target servers — keys are scoped **per team** (§23.2, `private_keys` table) and it is recommended to use **one key per server** (separability in case of compromise, §23.1):
   ```sh
   ssh-keygen -t ed25519 -N '' -C akerdock-server-01 -f /tmp/akerdock-server-01
   ```
   Register via the UI (Private Keys) or the API:
   ```sh
   curl -sS -X POST "$AKD/private-keys" \
     -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
     -d "{\"name\":\"server-01\",\"private_key\":\"$(awk '{printf "%s\\n",$0}' /tmp/akerdock-server-01)\"}"
   ```
   Then **delete the temporary files** (`shred -u /tmp/akerdock-server-01*`): the key encrypted in the database (`private_keys.private_key_enc`) becomes the only copy.
3. Deposit the public key on the target server (the SSH user's `~/.ssh/authorized_keys`), then add and validate the server (UI or `POST /servers` + `POST /servers/{uuid}/validate`).
4. Configure without delay: the **instance FQDN** and transactional email (§14.2), and the **instance database backup plan** with an S3 destination (`database_backup_plans.is_instance_backup = true`, §7.5) — see [postgres-failure.md](postgres-failure.md).

## Post-install verification

```sh
cd /var/lib/akerdock
docker compose ps                                    # 2 services Up, postgres healthy
docker compose logs --tail 50 AkerDock              # migrations OK, no error at boot
curl -fsS http://localhost:8080/api/v1/health        # unauthenticated healthcheck (§12)
curl -fsS -H "Authorization: Bearer $TOKEN" "$AKD/version"   # if API enabled
docker compose exec postgres psql -U AkerDock AkerDock \
  -c "SELECT fqdn, timezone, registration_enabled, api_enabled FROM instance_settings;"
```

Checklist:

- [ ] `GET /health` responds 200;
- [ ] root login + 2FA working; public registration **disabled** (`registration_enabled = false`, default);
- [ ] `master.key` backed up off the machine, permissions `0600 root:root`;
- [ ] first server `ready` after validation;
- [ ] instance backup plan created and one "Backup Now" execution succeeded (`backup_executions.status = 'succeeded'`).

## Prevention

- **Always** pin an explicit image tag; upgrades go through [upgrade-downgrade.md](upgrade-downgrade.md).
- Back up the `docker-compose.yml` + `.env` + `master.key` triplet off the machine as soon as installation is done: it is exactly what [control-plane-restore.md](control-plane-restore.md) requires.
- Close the control plane port behind the proxied FQDN as soon as possible (§14.2, §27.1); OS hardening and the cloud firewall remain your responsibility (§10.4 — Docker bypasses UFW).
