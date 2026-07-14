# Runbook — Panne de la base PostgreSQL interne

> Références : PRD §7.5, §16.4 (RPO ≤ 24 h, RTO ≤ 2 h), §22.1, INV-007, INV-013 ; spec deployment-engine §2.3–2.5 (leases, reprise par inspection) ; data dictionary §9.5–9.6 (`database_backup_plans`, `backup_executions`), §11.8 (`jobs`).

## Symptômes

- UI/API en erreur 5xx ; `GET /api/v1/health` échoue ou signale la base injoignable.
- `docker compose ps` : service `postgres` `Exited`/`Restarting`, ou `AkerDock` en crash-loop avec erreurs de connexion dans les logs.
- Notifications arrêtées, déploiements figés, webhooks non traités.
- **Les applications déployées continuent de servir le trafic** : le control plane n'est pas dans le chemin des requêtes (INV-007). Si les apps sont aussi down, ce n'est pas (que) ce runbook.

## Impact

- Aucune action de pilotage possible (deploy, rollback, terminal) tant que la base est down.
- Les jobs `leased`/`running` s'interrompent ; ils seront repris automatiquement (INV-013).
- En cas de restore : **perte de tout ce qui suit le backup** (RPO ≤ 24 h avec backup quotidien, §16.4).

## Diagnostic

```sh
cd /data/akerdock
docker compose ps
docker compose logs --tail 200 postgres
docker compose exec postgres pg_isready -U AkerDock || echo "PG DOWN"
df -h /data/akerdock                          # disque plein = cause n°1
docker compose exec postgres psql -U AkerDock AkerDock -c "SELECT 1;"   # si PG répond
```

Classifier :

1. **Transitoire** (OOM kill, disque plein, redémarrage machine) → résolution A.
2. **Corruption / perte de données** (`invalid page`, `could not read block`, volume perdu) → résolution B (restore).

Localiser le dernier backup exploitable — si la base répond encore partiellement :

```sql
SELECT be.uuid, be.status, be.filename, be.size_bytes, be.checksum_sha256,
       be.uploaded_to_s3, be.finished_at
FROM backup_executions be
JOIN database_backup_plans p ON p.id = be.backup_plan_id
WHERE p.is_instance_backup AND be.status IN ('succeeded','partial')
ORDER BY be.finished_at DESC LIMIT 5;
```

Sinon : fichiers locaux `/data/akerdock/backups/…` (§7.2) et/ou bucket S3 du plan (`aws s3 ls s3://<bucket>/<prefix>/ --endpoint-url <endpoint>`).

## Résolution pas à pas

### A. Panne transitoire

1. Libérer la cause (disque : purger `backups/` anciens, logs Docker `docker system df` ; RAM : vérifier l'OOM killer `dmesg | grep -i oom`).
2. `docker compose up -d` puis surveiller `docker compose logs -f postgres` (recovery WAL automatique au démarrage).
3. Passer directement à **Vérification** — pas de restore si la recovery aboutit.

### B. Restore depuis backup

1. **Arrêter le control plane**, conserver l'état corrompu :
   ```sh
   docker compose stop AkerDock
   docker compose stop postgres
   mv postgres postgres.corrupted-$(date -u +%Y%m%d)    # ne PAS supprimer (forensic/récupération partielle)
   mkdir postgres
   ```
2. Récupérer le dump (local ou S3) et **vérifier son intégrité** :
   ```sh
   sha256sum backups/akerdock-instance-….dump    # comparer à backup_executions.checksum_sha256 si connue
   ```
3. Redémarrer PostgreSQL seul, restaurer :
   ```sh
   docker compose up -d postgres
   # attendre pg_isready, puis :
   docker compose exec -T postgres pg_restore -U AkerDock -d AkerDock --no-owner --exit-on-error \
     < backups/akerdock-instance-….dump
   ```
   ⚠️ **Point de non-retour** : restaurer dans une base **vide** uniquement. Un restore vers une base non vide exige la confirmation renforcée prévue au §20.5 — en manuel, ne le faites simplement pas.
4. Vérifier que la **clé maître courante** correspond aux données restaurées : si une rotation a eu lieu **après** le backup, le fichier `master.key` doit encore contenir l'ancienne version de clé (voir [key-rotation.md](key-rotation.md) — c'est pour cela qu'on ne supprime jamais une version trop tôt).
5. Redémarrer le control plane : `docker compose up -d AkerDock`.

### C. Reprise des jobs après restore (INV-013, spec §2.3/§2.5)

Rien à forcer : le **scan des leases expirés** (toutes les 30 s) remet en `queued` les jobs `leased`/`running` dont la lease est morte, avec marqueur `recovered = true`. Chaque job de déploiement récupéré commence par une **inspection distante** (image labellisée `akerdock.deployment_uuid`, containers `<uuid>-next`/`<uuid>`, checksum du fichier proxy) avant de reprendre, compenser ou terminer — **ne jamais rejouer manuellement à l'aveugle** (§22.1).

Surveiller la reprise :

```sql
SELECT queue, status, count(*) FROM jobs GROUP BY 1,2 ORDER BY 1,2;
SELECT uuid, status, updated_at FROM deployments
WHERE status NOT IN ('succeeded','failed','cancelled','superseded')
ORDER BY updated_at;
```

Les déploiements non repris proprement finissent en `failed`/`dead_letter` → [queue-dead-letter.md](queue-dead-letter.md) et [orphaned-deployment.md](orphaned-deployment.md).

### D. Réconciliation control plane ↔ serveurs (fenêtre RPO)

Tout ce qui s'est passé **entre le backup et la panne** est absent de la base. Conséquences à traiter :

1. **Statuts observés périmés** : la réconciliation converge seule ; l'UI affiche « inconnu/stale » plutôt qu'un faux `running` (§19.2) — attendre un cycle avant de conclure.
2. **Déploiements réussis après le backup** : la base croit qu'une version plus ancienne tourne. Inventorier la réalité sur chaque serveur :
   ```sh
   ssh <user>@<serveur> "docker ps --filter label=akerdock.managed=true \
     --format '{{.Names}}\t{{.Label \"akerdock.deployment_uuid\"}}\t{{.Label \"akerdock.commit_sha\"}}'"
   ```
   Croiser avec `deployments.uuid` en base ; un `deployment_uuid` inconnu = déploiement perdu dans la fenêtre RPO. **Ne pas « corriger » en arrêtant le container** : re-déclencher un déploiement normal de la ressource pour réaligner base et réalité (le snapshot/SHA courant reprend la main).
3. **Livraisons webhook perdues** : la déduplication `(provider, delivery_id)` a perdu sa mémoire ; les pushes survenus pendant la panne n'ont déclenché personne. Redéployer manuellement les applications à auto-deploy dont le repo a bougé (`POST /applications/{uuid}/deploy`).
4. **Objets créés après le backup** (tokens, clés, ressources) : à re-créer ; les tokens API recréés changent de valeur.

## Vérification

- `curl -fsS http://localhost:8080/api/v1/health` → 200 ; login UI OK.
- Queue vivante : jobs récents `succeeded` ; plus de `leased` avec `lease_expires_at < now()`.
- Un déploiement de test de bout en bout réussit.
- Dashboard : serveurs `ready`, statuts observés rafraîchis (`observed_at` récents), pas d'alerte « actions prioritaires » inexpliquée.
- Relancer immédiatement un « Backup Now » du plan d'instance et vérifier `succeeded`.

## Prévention

- Plan de backup d'instance **quotidien minimum** avec destination **S3** et vérification d'upload (§7.2, §7.5) ; le statut `partial` (succès local, échec S3) est une alerte à traiter, pas un succès (§20.5).
- **Restore drills** automatiques (§20.5, §22.3) : un backup jamais restauré n'est pas fiable.
- Alerte disque sur l'hôte de l'instance (cause n°1 de panne PG) ; dimensionner `/data/akerdock`.
- Rétention des backups locaux ≠ 0 même avec S3 (restore plus rapide) ; RTO documenté ≤ 2 h (§16.4) — chronométrer le drill.
