# Runbook — Récupération d'un déploiement orphelin

> Références : spec deployment-engine §2.3 (leases), §2.5 (reprise par inspection), §4 (machine à états, colonne « crash pendant l'état »), §7.2 (bascule), §9 (compensations C1/C2/C3) ; PRD §21.1, INV-004/005/006/013 ; data dictionary §10.1–10.2 (`deployments`, `deployment_steps`), §11.8 (`jobs`).

## Symptômes

- Un déploiement reste dans un état non terminal (`preparing` … `finishing`) sans progression ; la timeline UI est figée.
- Un container `<app_uuid>-next` traîne sur le serveur alors qu'aucun déploiement n'est actif.
- Les déploiements suivants de l'application restent en `queued` (verrou §3.1 non libéré).
- Worker mort/redémarré : `leased_by` pointe un worker qui n'existe plus, `lease_expires_at` dépassé.

## Impact

- **L'application en production n'est pas censée être affectée** : l'ancien container reste routé tant que la bascule n'a pas eu lieu (INV-005), et l'échec ne supprime jamais le dernier container sain ni les volumes (INV-006).
- Les nouveaux déploiements de la même application/destination sont bloqués derrière le verrou.
- Cas sensible : orphelin en état `switching` — la bascule a pu avoir lieu, partiellement ou pas du tout.

## Diagnostic

### 1. Lire l'état write-ahead (la base dit « ce qui a pu commencer », spec §4)

```sql
-- Déploiements non terminaux et leur job :
SELECT d.uuid, d.status, d.attempt, d.updated_at, d.commit_sha, d.image_digest,
       j.uuid AS job_uuid, j.status AS job_status, j.leased_by, j.heartbeat_at, j.lease_expires_at
FROM deployments d
LEFT JOIN jobs j ON j.payload->>'deployment_uuid' = d.uuid::text
WHERE d.status NOT IN ('succeeded','failed','cancelled','superseded')
ORDER BY d.updated_at;

-- Dernier checkpoint committé (étapes) :
SELECT seq, name, status, exit_code, started_at, finished_at
FROM deployment_steps WHERE deployment_id = (SELECT id FROM deployments WHERE uuid = '<uuid>')
ORDER BY seq;
```

**Cas normal** : `lease_expires_at < now()` → le scan des leases (toutes les 30 s) remet le job en `queued` avec `recovered = true`, et le worker repreneur applique lui-même l'inspection + les règles de reprise (spec §2.5, §4). **Laisser faire.** N'intervenir manuellement que si : le job est en `dead_letter`, la flotte de workers est down, ou la reprise automatique boucle.

### 2. Inspection distante (jamais de décision sans elle — INV-004, §22.1)

Sur le serveur cible (`<app>` = UUID de l'application, `<sha12>` = 12 premiers caractères du SHA) :

```sh
# L'image du déploiement a-t-elle été produite ?
docker image inspect AkerDock/<app>:<sha12> \
  --format '{{index .Config.Labels "akerdock.deployment_uuid"}}' 2>/dev/null

# Candidat et container courant :
docker container inspect <app>-next --format '{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{end}} {{index .Config.Labels "akerdock.deployment_uuid"}}' 2>/dev/null
docker container inspect <app>      --format '{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{end}} {{index .Config.Labels "akerdock.deployment_uuid"}}' 2>/dev/null

# Vers qui pointe le proxy ? (fichier = source de vérité du routage, spec §7.1)
grep -A2 'url:' /data/akerdock/proxy/dynamic/<app>.yaml
sha256sum /data/akerdock/proxy/dynamic/<app>.yaml     # à comparer au checksum enregistré en base

# Reste de clone ?
ls -d /data/akerdock/applications/<app>/source/<deployment_uuid> 2>/dev/null
```

## Résolution pas à pas

Appliquer la règle de reprise de l'état où le déploiement s'est figé (tableau spec §4). Résumé opérateur :

| État figé | Décision |
|---|---|
| `preparing`, `cloning` | Reprendre (idempotent) ou compenser **C1** : `rm -rf source/<deployment_uuid>` ; rien d'autre n'a été touché |
| `building` | Image présente **avec** le bon label `akerdock.deployment_uuid` → l'étape avait fini, reprendre ; absente/partielle → rebuild ou C1 |
| `pushing` | Rejouable (push idempotent) ; re-résoudre le digest |
| `starting`, `healthchecking` | Candidat `unhealthy`/`exited`/absent → compensation **C2** ; candidat `healthy` et frais → reprise possible |
| `switching` | **Cas critique**, voir ci-dessous |
| `finishing` | Tout est idempotent → rejouer ; au pire `succeeded` dégradé |

### Cas `switching` (spec §4, règle a/b/c — jamais de seconde bascule sans inspection, INV-005)

- **(a)** Le fichier proxy pointe encore l'**ancien** container : la bascule n'a pas eu lieu. Candidat encore `healthy` → laisser la reprise automatique rejouer la bascule ; candidat mort → compensation **C2** (ci-dessous), déploiement `failed`.
- **(b)** Le fichier pointe le **candidat** (par IP), l'ancien container existe encore : la bascule a eu lieu → terminer la séquence : `docker stop -t <grace> <app> && docker rm <app>`, puis `docker rename <app>-next <app>`, puis stabilisation (fichier régénéré vers `url: http://<app>:<port>` — reprise en `finishing`).
- **(c)** L'ancien est absent, le rename non fait : reprendre au rename (`docker rename <app>-next <app>`) puis `finishing`.

⚠️ **Point de non-retour** : dès que le fichier proxy vérifié pointe le candidat (cas b/c), **ne jamais « dé-basculer » implicitement** (compensation C3 : le nouveau reste en production ; un retour arrière est un rollback explicite).

### Nettoyage du candidat sans toucher l'ancien (compensation C2, INV-005/006)

Uniquement si la décision est « compenser » (cas a avec candidat mort, ou candidat `-next` abandonné d'un déploiement déjà `failed`) :

```sh
# 1. Capturer les logs du candidat AVANT suppression (diagnostic) :
docker logs --tail 200 <app>-next > /tmp/<deployment_uuid>-next.log 2>&1
# 2. Si le fichier proxy a été modifié sans vérification concluante : le re-pointer sur l'ancien
#    (contenu de la dernière révision 'applied' — voir proxy-outage.md — écrit en .tmp puis mv -f)
# 3. Supprimer le candidat SEULEMENT :
docker stop -t 10 <app>-next && docker rm <app>-next
# 4. Purger le clone du déploiement :
rm -rf /data/akerdock/applications/<app>/source/<deployment_uuid>
```

⚠️ Interdits absolus pendant la compensation : toucher au container `<app>` (l'ancien), à ses **volumes**, ou aux **images** portant `akerdock.retain=true` (INV-006, spec §9.1).

### Clôture en base (dernier recours, si la reprise automatique est impossible)

**(candidat CLI futur — `AkerDock deployment resolve <uuid> --failed`)**. Contourne l'audit : consigner manuellement.

```sql
BEGIN;
UPDATE deployments SET status = 'failed', finished_at = now(), updated_at = now()
WHERE uuid = '<deployment_uuid>'
  AND status NOT IN ('succeeded','failed','cancelled','superseded');
-- Terminer le job associé s'il n'est pas déjà terminal :
UPDATE jobs SET status = 'cancelled', finished_at = now(), updated_at = now()
WHERE payload->>'deployment_uuid' = '<deployment_uuid>'
  AND status IN ('queued','leased','running','retry_wait');
-- Libérer le verrou applicatif (table sémantique resource_locks, spec §3.1) :
DELETE FROM resource_locks
WHERE application_uuid = '<app_uuid>' AND destination_uuid = '<destination_uuid>';
COMMIT;
```

Ne clôturer en `failed` qu'**après** la compensation distante (sinon un `-next` fantôme reste sur le serveur). Pour relancer proprement : retry manuel depuis l'UI (`failed → retrying → preparing`, même snapshot et même SHA, spec §4) ou nouveau `POST /applications/{uuid}/deploy`.

## Vérification

- L'ancienne version sert toujours : `curl -fsS https://<fqdn>/<health_path>` à travers le proxy.
- Plus de container `<app>-next` sur le serveur ; plus de clone `source/<deployment_uuid>`.
- Le checksum du fichier proxy correspond à la dernière révision `applied` en base.
- Verrou libéré : un nouveau déploiement de l'application sort de `queued` et se déroule normalement.
- Les images de rollback attendues sont intactes (`docker image ls --filter label=akerdock.retain=true`).

## Prévention

- La récupération est **conçue pour être automatique** (leases 90 s, heartbeat 20 s, scan 30 s, reprise par inspection) : la plupart des « orphelins » se résolvent seuls en < 2 min. Instrumenter les métriques `leases expirés` et `retries` (spec §12.3) et alerter sur leur croissance plutôt que d'intervenir au cas par cas.
- En multi-instance, dimensionner les workers pour éviter qu'un seul worker porte tous les jobs longs.
- Ne jamais tuer un worker pendant `switching` si évitable (arrêt gracieux des instances lors des upgrades — [upgrade-downgrade.md](upgrade-downgrade.md)).
