# Runbook — Jobs en dead-letter

> Références : PRD §21.3 (machine à états job), §22.1 (classification d'erreurs), §24.3 (files/priorités) ; ADR-002 (queue PostgreSQL) ; spec deployment-engine §2.4 (retry, backoff, dead-letter : « Rejeu depuis la dead-letter = action manuelle auditée qui crée une **nouvelle tentative liée** ») ; data dictionary §11.8 (`jobs`) ; OpenAPI (`GET /jobs`, `GET /jobs/{uuid}`, `POST /jobs/{uuid}/retry`, `POST /jobs/{uuid}/forget`).

## Symptômes

- Entrées « actions prioritaires » sur le dashboard ; notifications `deployment.failed` (ou backup/cleanup failed) répétées.
- Jobs en statut `dead_letter` (conservés jusqu'à intervention retry/forget, data dictionary §11.8).
- Symptôme indirect : une opération (déploiement, backup, suppression) qui « ne se fait plus jamais » — son job est mort en silence si les notifications sont mal configurées.

## Impact

- Un job en dead-letter ne sera **jamais** rejoué automatiquement : l'opération qu'il portait est en attente d'une décision humaine.
- Cas particulier `resource.delete` : un job de suppression en dead-letter laisse un **tombstone réconciliable** avec la liste des restes distants (§20.6.4) — le « forget » sans nettoyage laisse des objets orphelins sur le serveur.

## Diagnostic

### 1. Inventaire

```sh
curl -sS "$AKD/jobs?status=dead_letter" -H "Authorization: Bearer $TOKEN"
# liste paginée (curseur) ; filtres supplémentaires : &queue=deploy, &type=resource.delete
```

SQL équivalent (fallback, vue inter-team) :

```sql
SELECT uuid, queue, job_type, attempt, max_attempts, priority,
       left(last_error, 120) AS last_error, correlation_id, dead_lettered_at
FROM jobs WHERE status = 'dead_letter'
ORDER BY dead_lettered_at DESC;
```

### 2. Causes récurrentes (agrégation)

```sql
SELECT job_type, left(last_error, 80) AS err, count(*) AS n,
       min(dead_lettered_at) AS first_seen, max(dead_lettered_at) AS last_seen
FROM jobs WHERE status = 'dead_letter'
GROUP BY 1, 2 ORDER BY n DESC;
```

Lecture selon la classification (§22.1, spec §2.4) :

- **Erreurs transitoires** (SSH injoignable, timeout réseau, registry 5xx, disque saturé) arrivées en dead-letter = la panne a duré plus longtemps que la fenêtre de backoff (3 tentatives, backoff expo 30 s → 15 min pour `deployment.run`) → chercher l'incident d'infrastructure sous-jacent (serveur `unreachable` ? registry down ?) **avant** tout rejeu, sinon le rejeu re-mourra.
- **Erreurs déterministes** (build en échec, healthcheck jamais sain, config invalide) : pour `deployment.run` elles ne passent normalement **pas** par la dead-letter (échec direct sans retry auto, spec §2.4) — en voir en dead-letter suggère une mauvaise classification (bug à remonter). Pour les autres types (backup, cleanup, delete : `max_attempts` défaut 5), c'est le signe d'une cause à corriger avant rejeu.

### 3. Contexte d'un job précis

```sql
-- Chaîne complète via la corrélation (job → événements → audit) :
SELECT event_type, occurred_at, payload FROM outbox_events
WHERE correlation_id = '<correlation_id>' ORDER BY id;
SELECT occurred_at, action, result, actor_display FROM audit_events
WHERE correlation_id = '<correlation_id>' ORDER BY occurred_at;
-- Si c'est un déploiement : ses étapes et logs
SELECT seq, name, status, exit_code FROM deployment_steps
WHERE deployment_id = (SELECT id FROM deployments WHERE uuid = (
  SELECT payload->>'deployment_uuid' FROM jobs WHERE uuid = '<job_uuid>')::uuid)
ORDER BY seq;
```

Suivi API générique d'un job : `GET /jobs/{job_uuid}`.

## Résolution pas à pas

### A. Retry (rejeu)

Règle absolue (spec §2.4) : le rejeu est une **action manuelle auditée qui crée une nouvelle tentative liée**. ⚠️ **Ne jamais** remettre la ligne `dead_letter` en `queued` par UPDATE : cela réécrit l'historique (`attempt`, liaison des tentatives) et contourne l'audit.

1. Corriger la cause d'abord (serveur re-joignable, disque libéré, credentials réparés, config corrigée).
2. Rejouer par le canal métier, qui crée la tentative liée :
   - Déploiement : bouton retry de l'UI (transition `failed → retrying → preparing`, même snapshot/SHA) ou nouveau `POST /applications/{uuid}/deploy` (nouveau snapshot) selon que la config devait changer ou non.
   - Backup : `POST /databases/{db}/backups/{plan}/execute`.
   - Validation serveur : `POST /servers/{uuid}/validate`.
   - Suppression de ressource : relancer la suppression depuis l'UI (retry du tombstone, §20.6.4).
   - Autres types sans endpoint métier dédié : retry générique
     ```sh
     curl -sS -X POST "$AKD/jobs/$JOB_UUID/retry" -H "Authorization: Bearer $TOKEN"
     # 202 : nouveau job avec retry_of_uuid → job d'origine (tentative liée, spec §2.4) ;
     # 409 invalid_state si le job n'est pas (plus) en dead_letter
     ```
     ou l'UI « actions prioritaires ».
3. Après rejeu réussi, clôturer l'entrée dead-letter (voir B) si le produit ne le fait pas automatiquement.

### B. Forget (abandon)

À réserver aux jobs dont l'opération n'a plus de sens (ressource supprimée entre-temps, doublon, décision d'abandon).

⚠️ Avant d'oublier un job **`resource.delete`** : vérifier la liste des restes distants du tombstone (§20.6.4, colonne `resources.remnants`) et nettoyer manuellement sur le serveur, sinon containers/volumes orphelins :

```sql
SELECT uuid, name, remnants FROM resources
WHERE deleted_at IS NOT NULL AND remnants IS NOT NULL;
```

Via l'API (audité, le job passe en `cancelled`) ou l'UI (« forget ») :

```sh
curl -sS -X POST "$AKD/jobs/$JOB_UUID/forget" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"acknowledge_remnants": true}'
# acknowledge_remnants=true OBLIGATOIRE si le job laisse des restes distants,
# sinon 409 remnants_present (avec la liste des restes dans details)
```

Dernier recours SQL (contourne l'audit et la vérification des remnants — consigner manuellement) :

```sql
UPDATE jobs SET status = 'cancelled', finished_at = now(), updated_at = now()
WHERE uuid = '<job_uuid>' AND status = 'dead_letter';
```

### C. Traiter une vague (même cause, N jobs)

1. Corriger la cause commune (ex. serveur redevenu joignable).
2. Rejouer **un** job représentatif ; vérifier son succès.
3. Rejouer le reste par lot via le canal métier (boucle sur `POST /applications/{uuid}/deploy`, `POST /jobs/{uuid}/retry`, etc.). Attention aux plafonds : `concurrent_builds` (2/serveur) et `deployment_queue_limit` (25/serveur, `429 deployment_queue_full`) — étaler les rejeux.

## Vérification

- Plus de `dead_letter` inexpliqué : la requête d'inventaire ne montre que des jobs en cours de traitement décisionnel.
- Les rejeux apparaissent comme **nouvelles tentatives liées** (déploiements : `retry_of_id` renseigné, `attempt` incrémenté ; jobs : `retry_of_uuid` dans `GET /jobs/{uuid}`) et dans `audit_events`.
- La cause racine est corrigée : plus de nouveaux dead-letters du même `(job_type, last_error)` depuis la correction (requête d'agrégation, colonne `last_seen`).

## Prévention

- Alerter sur la **métrique dead-letters** (spec §12.3, OTLP) et sur les événements `deployment.failed` (§11) — un dead-letter silencieux est un incident différé.
- Régler les timeouts par application quand ils sont la cause récurrente (clone 600 s, build 3600 s, pull/push 900 s — spec §11) plutôt que de rejouer en boucle.
- Les pannes fournisseur ne doivent pas saturer les workers : le circuit breaker (§22.1) est là pour ça — une vague de dead-letters sur un même fournisseur doit le déclencher, sinon bug.
- Purge : les jobs terminés sont purgés par rétention, les `dead_letter` **conservés jusqu'à intervention** (data dictionary §11.8) — traiter la file régulièrement pour qu'elle reste un signal.
