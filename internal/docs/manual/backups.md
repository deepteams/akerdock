---
id: backups
title: Backups and restore
icon: archive
group: Run and debug
summary: Backup plans, retention, restoring, and the drill that proves it works.
order: 4
permission: backups:read
gates:
  backup-plans: backups:manage
  restoring: backups:restore
---

## Backup plans

A plan is a schedule, a destination and a retention policy, attached to a database (or to the internal database of a compose stack).

- **Schedule** — a cron expression or an alias (`hourly`, `daily`, `weekly`…), plus a **Backup now** button.
- **Timezone** — the cron is read in the zone attached to the plan. A nightly backup written as `0 2 * * *` in `UTC` drifts by an hour across daylight saving where you live; pick the zone you reason in and it stays where you put it.
- **Destination** — local on the server, an S3 storage, or both; "S3 only" deletes the local file after upload.
- **Retention** — max count, max age and max total size, applied separately to local and S3.
- Each run is recorded with its status, size and upload result, and can be downloaded or deleted.

The same two questions from your shell:

```
akerdock db backups list main          # the plans and their executions
akerdock db backups run main           # back up now
akerdock db backups run main --plan nightly
```

> **Note** — The CLI stops there: **no `restore`** — overwriting a production database is not an act for a one-line terminal confirmation, so it keeps the dashboard’s context — and no download, because no endpoint serves the file.

## Restoring

Restore a database from any recorded execution, straight from its backup list. It is a destructive operation on the target — the dashboard asks you to confirm what you are overwriting, and it is the only surface that offers it: the CLI has no `restore`, by decision.

## Restore drills

A drill restores a backup into a throwaway instance and reports whether it worked, without touching production. The history is kept next to the plan. A backup nobody has ever restored is not a backup — the drill is what turns the assumption into a fact.
