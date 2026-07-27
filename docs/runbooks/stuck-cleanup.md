# Runbook — Docker cleanup stuck or destructive

> References: PRD §3.7 ("never while a deployment is in progress", targets managed resources only), INV-015; deployment-engine spec §6.2 (`akerdock.managed`, `akerdock.retain` labels), §8.2 (protected rollback images); data dictionary §6.1 (cleanup options of `servers`), §10.3 (`deployment_artifacts`), §11.8 (`jobs`).

## Symptoms

- **Stuck**: a cleanup job that never finishes ("Docker cleanup" notification absent/failing, §11), server disk not freeing up despite the threshold being reached, persistent disk alert.
- **Destructive (suspicion)**: a rollback image has disappeared, an expected volume is absent, a container not managed by AkerDock was deleted — potential violation of INV-015.

## Impact

- Stuck: the disk keeps filling up → eventually build failures (`preparing` requires 2 GiB free, spec §4) then workload failures.
- A cleanup running during a deployment (forbidden by §3.7) can delete a candidate image in use; a misconfigured volume/network prune can destroy unmanaged data.

## Diagnosis

1. **Job state** (separate queues, §24.3 — `cleanup` queue):
   ```sql
   SELECT uuid, job_type, status, attempt, leased_by, heartbeat_at, lease_expires_at,
          run_at, last_error
   FROM jobs WHERE queue = 'cleanup'
   ORDER BY created_at DESC LIMIT 10;
   ```
   - `running` with a recent `heartbeat_at`: it is working (a build-cache prune can take a while) — wait.
   - `running`/`leased` with `lease_expires_at < now()`: dead worker; the lease scan (30 s) will recover it on its own.
   - `dead_letter`: see [queue-dead-letter.md](queue-dead-letter.md).
2. **Is there a deployment in progress on this server?** (cleanup must never run at the same time, §3.7):
   ```sql
   SELECT count(*) FROM deployments d JOIN servers s ON s.id = d.server_id
   WHERE s.uuid = '<server_uuid>'
     AND d.status NOT IN ('succeeded','failed','cancelled','superseded','queued');
   ```
   If > 0 **and** the cleanup is running: an anomaly to report (bug), and a reason to suspend the cleanup (resolution 3).
3. **On the server** — is the prune frozen on the Docker side?
   ```sh
   ssh <user>@<server> "ps aux | grep -E 'docker (image|container|volume|network|builder) prune' | grep -v grep"
   ssh <user>@<server> "journalctl -u docker --since '-30 min' --no-pager | tail -50"
   ssh <user>@<server> "df -h /var/lib/akerdock && docker system df"
   ```
   A Docker daemon that no longer responds (`docker ps` hanging) is a dockerd problem, not an AkerDock one.

## Step-by-step resolution

### A. Stuck cleanup

1. If the lease is expired: do nothing — automatic recovery (spec §2.3), the job restarts or ends up in `dead_letter`.
2. If the job holds its lease but is clearly frozen (heartbeat alive, no remote activity for > 30 min): cancel the job from the server UI; failing that **(future CLI candidate, SQL last resort — only on a `queued` job, never `running`)**:
   ```sql
   UPDATE jobs SET status = 'cancelled', finished_at = now(), updated_at = now()
   WHERE uuid = '<job_uuid>' AND queue = 'cleanup' AND status = 'queued';
   ```
   For a frozen `running` job, kill the remote process (`timeout` already wraps long commands, spec §2.6) rather than falsifying its status in the database.
3. **Temporarily disable** the server's cleanup for the duration of the diagnosis (server UI, or `PATCH /servers/{uuid}` with `cleanup_enabled: false`).
4. If dockerd itself is frozen: ⚠️ **restarting dockerd restarts the server's containers** (unless live-restore is enabled) — this is an interruption of all the server's workloads. Do it as a last resort, in an announced window:
   ```sh
   ssh <user>@<server> "systemctl restart docker"
   ```
5. Disk still full after unblocking: run a **manual and targeted** prune, respecting the managed boundaries:
   ```sh
   # build cache (harmless for managed resources):
   ssh <user>@<server> "docker builder prune -f --keep-storage 5GB"
   # unprotected dangling images only:
   ssh <user>@<server> "docker image prune -f --filter label!=akerdock.retain=true"
   ```
   ⚠️ Never `docker system prune -a --volumes` on a managed server: that would violate INV-015 (destruction of unmanaged objects and persistent volumes).

### B. Verify that no managed/persistent resource was touched (INV-015)

1. **Protected rollback images**: cross-check the database and the server:
   ```sql
   SELECT da.image_name, da.image_tag, da.image_digest
   FROM deployment_artifacts da JOIN servers s ON s.id = da.server_id
   WHERE s.uuid = '<server_uuid>' AND da.kind = 'local_image' AND da.protected_from_cleanup;
   ```
   ```sh
   ssh <user>@<server> "docker image inspect <image_name>:<image_tag> --format '{{.Id}} {{index .Config.Labels \"akerdock.retain\"}}'"
   ```
   A missing protected image = an incident: local rollback of that application is no longer possible (INV-006 eroded). Remediation: if a registry is configured, the digest remains retrievable (`docker pull <registry>/<image>@sha256:…`); otherwise, redeploy the resource to rebuild an artifact, and record it.
2. **Managed volumes**: compare declared vs actual:
   ```sh
   ssh <user>@<server> "docker volume ls --filter label=akerdock.managed=true --format '{{.Name}}'"
   ```
   against the `persistent_storages` (kind `volume`) of the server's resources (naming `<app_uuid>_<volume_name>`, spec §6.1). A managed volume missing on a `running` app = potential data loss → restore from a volume/database backup.
3. **Containers**: every expected managed container (`GET /servers/{uuid}/resources` with desired status `running`) must exist; an observed status `missing` that appears right after a cleanup is suspicious.
4. **Unmanaged objects**: if the server team reports the disappearance of containers/volumes **without** the `akerdock.managed` label while the cleanup ran, it is a direct violation of INV-015 → bug to report with the job's logs.

## Verification

- Next cleanup job `succeeded` (relaunch it via its cron or the server action) and status notification received (§11).
- Disk below the threshold (`df -h`), `docker system df` consistent.
- The three inventories in §B show nothing missing.
- Cleanup re-enabled (`cleanup_enabled = true`) if you had suspended it.

## Prevention

- Leave the destructive opt-ins **disabled** unless genuinely needed: `cleanup_prune_volumes` and `cleanup_prune_networks` are `false` by default (§3.7, data dictionary §6.1) — only enable them on servers without precious unmanaged volumes.
- Set `cleanup_disk_threshold_pct` **before** the red zone (e.g. 75%) so the cleanup runs outside emergencies, and outside deployment hours via `cleanup_cron`.
- Monitor disk metrics (Sentinel/OTLP) and the "disk usage threshold" event (§11).
- Size the rollback image retention (3 by default, spec §8.2) according to the available disk.
