# Runbook — Upgrade / downgrade de release et upgrade majeur PostgreSQL

> Références : PRD §14.3, §22.4, §26.3(6) ; ADR-021 (upgrade par changement de tag) ; ADR-025 (migrations SQL versionnées).

## Symptômes

Sans objet — opération planifiée. Cas d'usage : nouvelle release AkerDock, rollback d'une release défectueuse, changement de version majeure du PostgreSQL interne.

## Impact

- Pendant le redémarrage du control plane : UI/API indisponibles quelques secondes à minutes, déploiements suspendus. **Les workloads et proxies des serveurs cibles continuent de tourner** (INV-007).
- Les jobs `leased`/`running` au moment de l'arrêt sont repris automatiquement après expiration de lease (90 s), avec inspection distante avant rejeu (spec deployment-engine §2.5) — aucun rejeu à l'aveugle.

## Diagnostic (état avant intervention)

```sh
cd /var/lib/akerdock
curl -fsS -H "Authorization: Bearer $TOKEN" "$AKD/version"      # version courante
docker compose ps
# Déploiements en cours (préférer attendre qu'ils se terminent) :
docker compose exec postgres psql -U AkerDock AkerDock -c "
  SELECT count(*) FROM deployments
  WHERE status NOT IN ('succeeded','failed','cancelled','superseded','queued');"
# Jobs actifs :
docker compose exec postgres psql -U AkerDock AkerDock -c "
  SELECT queue, status, count(*) FROM jobs
  WHERE status IN ('leased','running') GROUP BY 1,2;"
```

Lire les notes de release : migrations incluses, compatibilité descendante (l'API garantit la compat sur une version mineure, §22.4).

## Résolution pas à pas

### A. Upgrade de release (ordre : backup → pull → migrations → vérif)

1. **Backup préalable** (obligatoire — c'est le point de retour du rollback) :
   ```sh
   cd /var/lib/akerdock
   docker compose exec -T postgres pg_dump -U AkerDock -Fc AkerDock \
     > backups/pre-upgrade-$(date -u +%Y%m%dT%H%M%SZ).dump
   cp keys/master.key backups/   # avec les mêmes précautions d'accès (0600)
   ```
   Vérifier la taille non nulle et copier le dump hors machine.
2. **Attendre le calme** : idéalement zéro déploiement en cours (requête ci-dessus). Sinon, accepter que les jobs actifs seront repris après lease (90 s).
3. **Changer le tag** dans `/var/lib/akerdock/.env` :
   ```sh
   sed -i 's/^AKERDOCK_TAG=.*/AKERDOCK_TAG=v1.1.0/' .env
   ```
4. **Pull puis recréation** :
   ```sh
   docker compose pull AkerDock
   docker compose up -d AkerDock
   ```
   Le binaire applique les **migrations up** au démarrage **(normatif : spec [instance-config](../specs/instance-config.md) §6 — migrations automatiques au boot ; les migrations sont conçues compatibles rolling upgrade, §18.2)**. En mode multi-instance `api`/`worker`, mettre à jour une instance à la fois.
5. **Suivre les migrations** :
   ```sh
   docker compose logs -f AkerDock   # jusqu'à "migrations applied" + écoute HTTP
   ```

⚠️ **Point de non-retour** : dès qu'une migration **non rétrocompatible** est appliquée (indiquée dans les notes de release), le retour à l'ancien tag exige migrations down ou restore du dump — plus un simple changement de tag.

### B. Rollback de release (downgrade)

Trois niveaux, du moins au plus destructif :

1. **Tag précédent seul** — si les notes de release confirment que les migrations de la version fautive sont rétrocompatibles (additives) :
   ```sh
   sed -i 's/^AKERDOCK_TAG=.*/AKERDOCK_TAG=v1.0.0/' .env
   docker compose up -d AkerDock
   ```
2. **Migrations down puis tag précédent** — chaque release livre sa migration down ou une procédure de rollback (§26.3(6)). L'image distroless n'a pas de shell : exécuter le mode migration du binaire **(candidat CLI futur — `AkerDock migrate down --to <version>` ; défaut proposé : sous-commande du binaire lancée via un run one-shot)** :
   ```sh
   docker compose run --rm AkerDock migrate down --to <schema_version_cible>
   sed -i 's/^AKERDOCK_TAG=.*/AKERDOCK_TAG=v1.0.0/' .env
   docker compose up -d AkerDock
   ```
3. **Restore du dump pré-upgrade** — si les migrations down sont impossibles/défectueuses :
   ⚠️ **Point de non-retour** : tout ce qui a été créé/modifié depuis le backup (déploiements, tokens, livraisons webhook, audit) est perdu. Suivre [postgres-failure.md](postgres-failure.md) §« Restore », avec le dump `pre-upgrade-*`, puis revenir à l'ancien tag.

### C. Upgrade majeur du PostgreSQL interne (§14.3, §22.4, ADR-021)

Procédure **séparée** des releases AkerDock, par dump/restore (pas de `pg_upgrade` in-place entre volumes de containers) :

1. Backup complet (étape A.1) — c'est le fichier qui sera restauré.
2. Arrêter le control plane, garder PostgreSQL :
   ```sh
   docker compose stop AkerDock
   docker compose exec -T postgres pg_dump -U AkerDock -Fc AkerDock > backups/pg-major-upgrade.dump
   docker compose stop postgres
   ```
3. Mettre l'ancien répertoire de données de côté (**ne pas supprimer**) :
   ```sh
   mv postgres postgres.old-pg16
   mkdir postgres
   ```
4. Changer le tag PostgreSQL dans `docker-compose.yml` (ex. `postgres:16` → `postgres:17` — rester dans la **plage de versions testée** par la release, §22.4), puis :
   ```sh
   docker compose up -d postgres
   docker compose exec -T postgres pg_restore -U AkerDock -d AkerDock --no-owner < backups/pg-major-upgrade.dump
   docker compose up -d AkerDock
   ```
5. ⚠️ **Point de non-retour** : la suppression de `postgres.old-pg16/` — ne la faire qu'après plusieurs jours de fonctionnement vérifié.

## Vérification

```sh
curl -fsS http://localhost:8080/api/v1/health
curl -fsS -H "Authorization: Bearer $TOKEN" "$AKD/version"      # nouvelle version attendue
docker compose logs --tail 100 AkerDock | grep -iE 'error|panic' || echo OK
# La queue tourne : jobs récents traités
docker compose exec postgres psql -U AkerDock AkerDock -c "
  SELECT status, count(*) FROM jobs
  WHERE created_at > now() - interval '1 hour' GROUP BY 1;"
```

Puis un **déploiement de test** de bout en bout (petite app ou redeploy d'une ressource non critique) et un tour du dashboard (serveurs `ready`, statuts observés qui se rafraîchissent — pas de `stale` généralisé).

## Prévention

- Toujours un tag explicite, jamais `latest` ; lire les notes de release **avant** (migrations non rétrocompatibles signalées).
- Backup pré-upgrade systématique (le cron d'auto-update de l'instance, §14.3, doit être précédé du backup planifié — vérifier l'horaire relatif des deux crons).
- Tester l'upgrade sur une instance de staging si vous en avez une ; sinon, fenêtre de maintenance annoncée.
- Auto-update (§14.3) : le laisser activé seulement si le backup quotidien est en place et vérifié (restore drills, §20.5).
