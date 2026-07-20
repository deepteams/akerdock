# Runbook — Cleanup Docker bloqué ou destructeur

> Références : PRD §3.7 (« jamais pendant un déploiement en cours », cible uniquement les ressources gérées), INV-015 ; spec deployment-engine §6.2 (labels `akerdock.managed`, `akerdock.retain`), §8.2 (images de rollback protégées) ; data dictionary §6.1 (options cleanup de `servers`), §10.3 (`deployment_artifacts`), §11.8 (`jobs`).

## Symptômes

- **Bloqué** : job de cleanup qui ne se termine pas (notification « Docker cleanup » absente/en échec, §11), disque du serveur qui ne se libère pas malgré le seuil atteint, alerte disque persistante.
- **Destructeur (suspicion)** : une image de rollback a disparu, un volume attendu est absent, un container non géré par AkerDock a été supprimé — violation potentielle d'INV-015.

## Impact

- Bloqué : le disque continue de se remplir → à terme, échecs de builds (`preparing` exige 2 GiB libres, spec §4) puis de workloads.
- Un cleanup qui tourne pendant un déploiement (interdit par §3.7) peut supprimer une image candidate en cours d'utilisation ; un prune volumes/réseaux mal configuré peut détruire des données non gérées.

## Diagnostic

1. **État du job** (files séparées, §24.3 — queue `cleanup`) :
   ```sql
   SELECT uuid, job_type, status, attempt, leased_by, heartbeat_at, lease_expires_at,
          run_at, last_error
   FROM jobs WHERE queue = 'cleanup'
   ORDER BY created_at DESC LIMIT 10;
   ```
   - `running` avec `heartbeat_at` récent : il travaille (un prune de build cache peut être long) — patienter.
   - `running`/`leased` avec `lease_expires_at < now()` : worker mort ; le scan des leases (30 s) va le récupérer seul.
   - `dead_letter` : voir [queue-dead-letter.md](queue-dead-letter.md).
2. **Y a-t-il un déploiement en cours sur ce serveur ?** (le cleanup ne doit jamais s'exécuter en même temps, §3.7) :
   ```sql
   SELECT count(*) FROM deployments d JOIN servers s ON s.id = d.server_id
   WHERE s.uuid = '<server_uuid>'
     AND d.status NOT IN ('succeeded','failed','cancelled','superseded','queued');
   ```
   Si > 0 **et** que le cleanup tourne : anomalie à signaler (bug), et raison de suspendre le cleanup (résolution 3).
3. **Sur le serveur** — le prune est-il gelé côté Docker ?
   ```sh
   ssh <user>@<serveur> "ps aux | grep -E 'docker (image|container|volume|network|builder) prune' | grep -v grep"
   ssh <user>@<serveur> "journalctl -u docker --since '-30 min' --no-pager | tail -50"
   ssh <user>@<serveur> "df -h /var/lib/akerdock && docker system df"
   ```
   Un daemon Docker qui ne répond plus (`docker ps` qui pend) est un problème dockerd, pas AkerDock.

## Résolution pas à pas

### A. Cleanup bloqué

1. Si la lease est expirée : ne rien faire — récupération automatique (spec §2.3), le job repart ou finit en `dead_letter`.
2. Si le job tient sa lease mais est manifestement gelé (heartbeat vivant, aucune activité distante depuis > 30 min) : annuler le job depuis l'UI du serveur ; à défaut **(candidat CLI futur, dernier recours SQL — uniquement sur un job `queued`, jamais `running`)** :
   ```sql
   UPDATE jobs SET status = 'cancelled', finished_at = now(), updated_at = now()
   WHERE uuid = '<job_uuid>' AND queue = 'cleanup' AND status = 'queued';
   ```
   Pour un job `running` gelé, tuer le processus distant (`timeout` encadre déjà les commandes longues, spec §2.6) plutôt que de falsifier son statut en base.
3. **Désactiver temporairement** le cleanup du serveur le temps du diagnostic (UI serveur, ou `PATCH /servers/{uuid}` avec `cleanup_enabled: false`).
4. Si dockerd lui-même est gelé : ⚠️ **redémarrer dockerd redémarre les containers du serveur** (sauf live-restore activé) — c'est une interruption de tous les workloads du serveur. Le faire en dernier recours, en fenêtre annoncée :
   ```sh
   ssh <user>@<serveur> "systemctl restart docker"
   ```
5. Disque toujours plein après déblocage : lancer un prune **manuel et ciblé**, en respectant les frontières gérées :
   ```sh
   # build cache (sans danger pour les ressources gérées) :
   ssh <user>@<serveur> "docker builder prune -f --keep-storage 5GB"
   # images dangling non protégées uniquement :
   ssh <user>@<serveur> "docker image prune -f --filter label!=akerdock.retain=true"
   ```
   ⚠️ Jamais `docker system prune -a --volumes` sur un serveur géré : cela violerait INV-015 (destruction d'objets non gérés et de volumes persistants).

### B. Vérifier qu'aucune ressource gérée/persistante n'a été touchée (INV-015)

1. **Images de rollback protégées** : croiser la base et le serveur :
   ```sql
   SELECT da.image_name, da.image_tag, da.image_digest
   FROM deployment_artifacts da JOIN servers s ON s.id = da.server_id
   WHERE s.uuid = '<server_uuid>' AND da.kind = 'local_image' AND da.protected_from_cleanup;
   ```
   ```sh
   ssh <user>@<serveur> "docker image inspect <image_name>:<image_tag> --format '{{.Id}} {{index .Config.Labels \"akerdock.retain\"}}'"
   ```
   Une image protégée absente = incident : le rollback local de cette application n'est plus possible (INV-006 entamé). Remédiation : si un registry est configuré, le digest reste récupérable (`docker pull <registry>/<image>@sha256:…`) ; sinon, redéployer la ressource pour reconstituer un artifact, et consigner.
2. **Volumes gérés** : comparer le déclaré et le réel :
   ```sh
   ssh <user>@<serveur> "docker volume ls --filter label=akerdock.managed=true --format '{{.Name}}'"
   ```
   contre les `persistent_storages` (kind `volume`) des ressources du serveur (nommage `<app_uuid>_<volume_name>`, spec §6.1). Un volume géré manquant sur une app `running` = perte de données potentielle → restaurer depuis backup de volume/base.
3. **Containers** : tout container géré attendu (`GET /servers/{uuid}/resources` avec statut désiré `running`) doit exister ; un statut observé `missing` qui apparaît juste après un cleanup est suspect.
4. **Objets non gérés** : si l'équipe serveur signale la disparition de containers/volumes **sans** label `akerdock.managed` alors que le cleanup a tourné, c'est une violation directe d'INV-015 → bug à remonter avec les logs du job.

## Vérification

- Job de cleanup suivant en `succeeded` (le relancer via son cron ou l'action serveur) et notification de statut reçue (§11).
- Disque sous le seuil (`df -h`), `docker system df` cohérent.
- Les trois inventaires du §B ne montrent aucun manquant.
- Le cleanup réactivé (`cleanup_enabled = true`) si vous l'aviez suspendu.

## Prévention

- Laisser les opt-ins destructeurs **désactivés** sauf besoin réel : `cleanup_prune_volumes` et `cleanup_prune_networks` sont `false` par défaut (§3.7, data dictionary §6.1) — ne les activer que sur des serveurs sans volumes précieux non gérés.
- Régler `cleanup_disk_threshold_pct` **avant** la zone rouge (ex. 75 %) pour que le cleanup tourne hors urgence, et hors heures de déploiement via `cleanup_cron`.
- Surveiller les métriques disque (Sentinel/OTLP) et l'événement « seuil d'usage disque » (§11).
- Dimensionner la rétention d'images de rollback (3 par défaut, spec §8.2) selon le disque disponible.
