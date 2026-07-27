# Operator runbooks — AkerDock

> Artifact §29.10 of the PRD (`docs/PRD.md`). These runbooks rely exclusively on the actual distribution (2-service compose — ADR-021), the data dictionary tables (`docs/specs/data-dictionary.md`), the OpenAPI endpoints (`docs/specs/openapi-v1.yaml`) and the deployment engine specification (`docs/specs/deployment-engine.md`). When a dedicated CLI command would be ideal but does not exist yet, the equivalent SQL/API request is given and marked **(future CLI candidate)**. Values not fixed by the specs are marked **(proposed default)**.

## Index

| Runbook | When to use it | Criticality |
|---|---|---|
| [install.md](install.md) | Installation of a new instance (2-service compose), master key, SSH keys, first root user | Medium (planned operation) |
| [upgrade-downgrade.md](upgrade-downgrade.md) | Release update by image tag, release rollback, major upgrade of the internal PostgreSQL | High (maintenance window) |
| [key-rotation.md](key-rotation.md) | Rotation of the master key, of a server SSH key, of webhook/OAuth secrets; emergency revocation of API tokens | High to critical (depending on context) |
| [postgres-failure.md](postgres-failure.md) | Internal PostgreSQL database down or corrupted; restore from backup; job recovery | **Critical** |
| [control-plane-restore.md](control-plane-restore.md) | Total loss of the machine hosting the instance; full restore on a fresh machine | **Critical** |
| [compromised-server.md](compromised-server.md) | Suspected or confirmed compromise of a target server | **Critical** (security incident) |
| [stuck-cleanup.md](stuck-cleanup.md) | Docker cleanup stuck, or suspicion that it touched a managed/persistent resource | Medium to high |
| [orphaned-deployment.md](orphaned-deployment.md) | Frozen deployment: dead worker, expired lease, lingering `-next` container, unreleased lock | High |
| [queue-dead-letter.md](queue-dead-letter.md) | Dead-lettered jobs: triage, retry/forget, recurring causes | Medium |
| [proxy-outage.md](proxy-outage.md) | Server proxy down or corrupted dynamic configuration | **Critical** (inbound traffic cut off) |
| [certificates.md](certificates.md) | ACME failures, active self-signed fallback, expired certificates, custom certs, DNS-01 wildcard | High |

## Conventions common to all runbooks

### Anatomy of the instance (ADR-021, §27.21)

The instance = **2 Docker Compose services**: the `AkerDock` image (static Go binary, distroless image, `all-in-one`/`api`/`worker` modes) + PostgreSQL. Directory tree on the host machine **(proposed default)**:

```text
/var/lib/akerdock/                  # instance root on the control plane host
├── docker-compose.yml            # definition of the 2 services
├── .env                          # non-secret configuration (image tag, port…)
├── keys/master.key               # envelope encryption master key (0600, root) — ADR-003
├── postgres/                     # PostgreSQL data directory (bind mount)
└── backups/                      # local backups of the internal database (§7.2)
```

> Not to be confused with `/var/lib/akerdock/` **on the target servers** (normative directory tree §5.1 of the deployment-engine spec: `applications/`, `proxy/`, `backups/`, `tmp/`). If the `localhost` server is used, both coexist — the instance then lives in a dedicated subdirectory, e.g. `/var/lib/akerdock/instance/` **(proposed default)**.

### Tool access

- **All `docker compose` commands** are run from `/var/lib/akerdock/` on the control plane host.
- **The AkerDock image is distroless** (ADR-021): no shell in the container. All diagnostics go through the logs (`docker compose logs AkerDock`), the API and `psql` executed inside the PostgreSQL container.
- **psql**:
  ```sh
  cd /var/lib/akerdock
  docker compose exec postgres psql -U AkerDock AkerDock
  ```
- **API**: base `https://<instance-fqdn>/api/v1`, auth `Authorization: Bearer $TOKEN`. Reminder: the API is **disabled by default** (§10.3) — enable it in the settings before an incident, or go through the UI/SQL. In the examples: `export AKD=https://akerdock.example.com/api/v1` and `export TOKEN=akd_…`.
- Direct SQL mutations are a **last resort**: they bypass the audit (§23.4) and the optimistic lock. Each runbook flags them as such.

### Symbols

- ⚠️ **Point of no return**: beyond this step, there is no going back without a restore/loss.
- **(proposed default)**: value or name not fixed by the PRD/specs; to be confirmed at implementation time.
- **(future CLI candidate)**: operation that deserves a dedicated `AkerDock` command; SQL/API in the meantime.
