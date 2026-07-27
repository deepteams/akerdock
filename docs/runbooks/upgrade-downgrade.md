# Runbook — Release upgrade / downgrade and PostgreSQL major upgrade

> References: PRD §14.3, §22.4, §26.3(6); ADR-021 (upgrade by tag change); ADR-025 (versioned SQL migrations).

## Symptoms

Not applicable — planned operation. Use cases: new AkerDock release, rollback of a faulty release, major version change of the internal PostgreSQL.

## Impact

- While the control plane restarts: UI/API unavailable for seconds to minutes, deployments suspended. **Workloads and proxies on the target servers keep running** (INV-007).
- Jobs `leased`/`running` at shutdown time are resumed automatically after lease expiry (90 s), with a remote inspection before replay (deployment-engine spec §2.5) — no blind replay.

## Diagnosis (state before intervention)

```sh
cd /var/lib/akerdock
curl -fsS -H "Authorization: Bearer $TOKEN" "$AKD/version"      # current version
docker compose ps
# Deployments in progress (prefer waiting for them to finish):
docker compose exec postgres psql -U AkerDock AkerDock -c "
  SELECT count(*) FROM deployments
  WHERE status NOT IN ('succeeded','failed','cancelled','superseded','queued');"
# Active jobs:
docker compose exec postgres psql -U AkerDock AkerDock -c "
  SELECT queue, status, count(*) FROM jobs
  WHERE status IN ('leased','running') GROUP BY 1,2;"
```

Read the release notes: included migrations, backward compatibility (the API guarantees compatibility within a minor version, §22.4).

## Step-by-step resolution

### A. Release upgrade (order: backup → pull → migrations → verify)

1. **Prior backup** (mandatory — it is the rollback's point of return):
   ```sh
   cd /var/lib/akerdock
   docker compose exec -T postgres pg_dump -U AkerDock -Fc AkerDock \
     > backups/pre-upgrade-$(date -u +%Y%m%dT%H%M%SZ).dump
   cp keys/master.key backups/   # with the same access precautions (0600)
   ```
   Verify the size is non-zero and copy the dump off the machine.
2. **Wait for calm**: ideally zero deployments in progress (query above). Otherwise, accept that active jobs will be resumed after lease expiry (90 s).
3. **Change the tag** in `/var/lib/akerdock/.env`:
   ```sh
   sed -i 's/^AKERDOCK_TAG=.*/AKERDOCK_TAG=v1.1.0/' .env
   ```
4. **Pull then recreate**:
   ```sh
   docker compose pull AkerDock
   docker compose up -d AkerDock
   ```
   The binary applies the **up migrations** at startup **(normative: [instance-config](../specs/instance-config.md) spec §6 — automatic migrations at boot; migrations are designed to be rolling-upgrade compatible, §18.2)**. In multi-instance `api`/`worker` mode, update one instance at a time.
5. **Follow the migrations**:
   ```sh
   docker compose logs -f AkerDock   # until "migrations applied" + HTTP listening
   ```

⚠️ **Point of no return**: as soon as a **non-backward-compatible** migration is applied (flagged in the release notes), going back to the old tag requires down migrations or a dump restore — no longer a simple tag change.

### B. Release rollback (downgrade)

Three levels, from least to most destructive:

1. **Previous tag alone** — if the release notes confirm that the faulty version's migrations are backward compatible (additive):
   ```sh
   sed -i 's/^AKERDOCK_TAG=.*/AKERDOCK_TAG=v1.0.0/' .env
   docker compose up -d AkerDock
   ```
2. **Down migrations then previous tag** — each release ships its down migration or a rollback procedure (§26.3(6)). The distroless image has no shell: run the binary's migration mode **(future CLI candidate — `AkerDock migrate down --to <version>`; proposed default: subcommand of the binary launched via a one-shot run)**:
   ```sh
   docker compose run --rm AkerDock migrate down --to <target_schema_version>
   sed -i 's/^AKERDOCK_TAG=.*/AKERDOCK_TAG=v1.0.0/' .env
   docker compose up -d AkerDock
   ```
3. **Restore the pre-upgrade dump** — if the down migrations are impossible/faulty:
   ⚠️ **Point of no return**: everything created/modified since the backup (deployments, tokens, webhook deliveries, audit) is lost. Follow [postgres-failure.md](postgres-failure.md) §"Restore", with the `pre-upgrade-*` dump, then go back to the old tag.

### C. Major upgrade of the internal PostgreSQL (§14.3, §22.4, ADR-021, ADR-039)

A Postgres major is **not in-place compatible**: after a tag bump, the pinned image refuses to start on a volume from an earlier major (`database files are incompatible`). The upgrade is therefore **explicit and opt-in** (ADR-039) — never automatic. `install.sh` detects the version gap and stops, pointing here, rather than letting the container crash-loop.

**Recommended path — tooled, in-place, backup-first (ADR-039)**

Once the tag is bumped in `docker-compose.yml` (stay within the **range tested** by the release, §22.4):

```sh
./scripts/pg-upgrade.sh          # interactive confirmation
# or, in scripted maintenance:
./scripts/pg-upgrade.sh --yes
```

The script: detects the volume's major vs the target → **full copy of the data volume** under `backups/` (rollback) → migrates in-place via `pgautoupgrade` (one-shot) → restarts the stack on the **official** `postgres:<major>` image → checks health. ⚠️ **Point of no return**: only delete the `backups/pgdata-pre-upgrade-*.tar.gz` copy after several days of verified operation.

**Fallback — manual dump/restore**

If `pgautoupgrade` is not acceptable (third-party image refused, special case), the logical path remains valid. The service, user and database are `akerdock` (lowercase); the data lives in the **named volume** `akerdock_pgdata`, not in a bind-mounted folder:

1. Full backup (step A.1).
2. Dump, with the control plane stopped but PostgreSQL kept:
   ```sh
   docker compose stop akerdock
   docker compose exec -T postgres pg_dump -U akerdock -Fc akerdock > backups/pg-major-upgrade.dump
   docker compose down
   ```
3. Set the current volume aside (**do not delete**), create a fresh one:
   ```sh
   docker volume rename akerdock_akerdock_pgdata akerdock_akerdock_pgdata.old
   docker volume create akerdock_akerdock_pgdata
   ```
4. Bump the PostgreSQL tag in `docker-compose.yml`, then restore into the fresh volume:
   ```sh
   docker compose up -d postgres
   docker compose exec -T postgres pg_restore -U akerdock -d akerdock --no-owner < backups/pg-major-upgrade.dump
   docker compose up -d akerdock
   ```
5. ⚠️ **Point of no return**: deleting the `akerdock_akerdock_pgdata.old` volume — only after several verified days.

## Verification

```sh
curl -fsS http://localhost:8080/api/v1/health
curl -fsS -H "Authorization: Bearer $TOKEN" "$AKD/version"      # expected new version
docker compose logs --tail 100 AkerDock | grep -iE 'error|panic' || echo OK
# The queue is running: recent jobs processed
docker compose exec postgres psql -U AkerDock AkerDock -c "
  SELECT status, count(*) FROM jobs
  WHERE created_at > now() - interval '1 hour' GROUP BY 1;"
```

Then an end-to-end **test deployment** (small app or redeploy of a non-critical resource) and a dashboard tour (servers `ready`, observed statuses refreshing — no widespread `stale`).

## Prevention

- Always an explicit tag, never `latest`; read the release notes **beforehand** (non-backward-compatible migrations flagged).
- Systematic pre-upgrade backup (the instance auto-update cron, §14.3, must be preceded by the scheduled backup — check the relative timing of the two crons).
- Test the upgrade on a staging instance if you have one; otherwise, an announced maintenance window.
- Auto-update (§14.3): leave it enabled only if the daily backup is in place and verified (restore drills, §20.5).
