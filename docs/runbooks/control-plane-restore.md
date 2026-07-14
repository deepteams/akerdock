# Runbook — Perte totale de l'instance : restore sur machine neuve

> Références : PRD §7.5, §16.4 (RTO ≤ 2 h), §22.1 (« Une procédure documentée restaure PostgreSQL, clés de chiffrement, clés SSH, configurations proxy et fichiers nécessaires »), INV-007 ; ADR-021 ; ADR-003 ; spec deployment-engine §2.5, §7.

## Symptômes

- La machine hébergeant l'instance est perdue (disque mort, VPS supprimé, datacenter) ou irrécupérable.
- **Les applications, bases et proxies sur les serveurs cibles continuent de tourner normalement** (INV-007) : les utilisateurs finaux ne voient rien — seul le pilotage est perdu.

## Impact

- Plus d'UI/API/déploiements/notifications/backups planifiés tant que l'instance n'est pas restaurée.
- Fenêtre RPO : tout ce qui suit le dernier backup de la base est perdu (voir [postgres-failure.md](postgres-failure.md) §D).
- Les certificats existants continuent d'être servis par les proxies ; leurs **renouvellements ACME** ne dépendent pas de l'instance (le proxy les gère localement) mais les changements de domaines si.

## Prérequis du restore — les 3 pièces

| Pièce | Source | Sans elle |
|---|---|---|
| Dump PostgreSQL | S3 du plan `is_instance_backup`, ou copie locale exfiltrée | Reconstruction manuelle complète (ré-onboarding de tous les serveurs) |
| **Clé maître** (`master.key`, toutes versions) | Coffre/gestionnaire de secrets hors machine ([install.md](install.md) étape 2) | ⚠️ **Tous les secrets du dump sont irrécupérables** (ADR-003) : clés SSH, variables chiffrées, credentials S3/registry/cloud — voir « Cas dégradé » ci-dessous |
| `docker-compose.yml` + `.env` (ou leur contenu : **tag d'image exact**, port, mot de passe PG) | Copie hors machine / Git d'infra | Reconstituables depuis [install.md](install.md), mais le **tag d'image doit correspondre au schéma du dump** |

## Diagnostic

Avant de restaurer, sécuriser ce qui existe encore :

```sh
# Le dump le plus récent sur S3 (endpoint/bucket du plan d'instance) :
aws s3 ls s3://<bucket>/<prefix>/ --endpoint-url <endpoint> | sort | tail -5
# Les serveurs cibles tournent toujours (depuis votre poste, avec un accès SSH de secours) :
ssh <user>@<serveur> "docker ps --filter label=akerdock.managed=true --format '{{.Names}}\t{{.Status}}'"
```

## Résolution pas à pas

### 1. Machine neuve

Provisionner une machine conforme aux prérequis d'[install.md](install.md) (Docker ≥ 24, Compose v2). Si possible, réutiliser la même IP ; sinon prévoir l'étape DNS (5).

### 2. Reconstituer l'arborescence

```sh
mkdir -p /data/akerdock/keys /data/akerdock/postgres /data/akerdock/backups
# Restaurer les 3 pièces :
#   /data/akerdock/docker-compose.yml, /data/akerdock/.env  (tag d'image = celui d'avant la perte ⚠️)
#   /data/akerdock/keys/master.key   (0600 root:root, umask 077)
chmod 0700 /data/akerdock/keys && chmod 0600 /data/akerdock/keys/master.key
```

⚠️ Démarrer avec un **tag d'image plus récent** que celui du dump appliquerait des migrations lors du boot : faisable, mais cela mélange restore et upgrade. Restaurer d'abord à l'identique, upgrader ensuite ([upgrade-downgrade.md](upgrade-downgrade.md)).

### 3. Restaurer la base

```sh
cd /data/akerdock
docker compose up -d postgres
# attendre pg_isready :
docker compose exec postgres pg_isready -U AkerDock
docker compose exec -T postgres pg_restore -U AkerDock -d AkerDock --no-owner --exit-on-error \
  < /chemin/du/dump/akerdock-instance-….dump
docker compose up -d AkerDock
```

### 4. Vérifier le déchiffrement (la preuve que la clé maître est la bonne)

Dans l'UI, révéler un secret quelconque (permission `read:sensitive`) ou valider un serveur (étape 6) — la connexion SSH exige le déchiffrement de `private_keys.private_key_enc`. Une erreur de déchiffrement ici = mauvaise version de clé dans `master.key` : ne pas aller plus loin, retrouver le bon fichier.

### 5. Rebrancher le DNS de l'instance

Pointer le FQDN de l'instance (§14.2) vers la nouvelle IP. Les webhooks entrants (GitHub/GitLab…) recommencent à arriver dès la propagation — c'est voulu.

### 6. Reconnexion aux serveurs existants

Les serveurs, clés et ressources sont dans le dump ; rien à ré-enregistrer. Pour chaque serveur :

```sh
curl -sS -X POST "$AKD/servers/$SERVER_UUID/validate" -H "Authorization: Bearer $TOKEN"
```

La validation reteste SSH, Docker, réseau, proxy et Sentinel (§20.1) et repasse le serveur `ready`. Aucun `authorized_keys` à modifier : les clés privées restaurées sont celles que les serveurs connaissent déjà.

### 7. Réconciliation des états observés (INV-007)

Les workloads ont continué à tourner pendant la panne — la base est en retard sur la réalité :

1. **Statuts observés** : marqués stale (`observed_at` ancien) ; la réconciliation les rafraîchit ; les actions destructives sont suspendues tant que l'observation est trop ancienne (§21.2) — comportement attendu, ne pas forcer.
2. **Jobs en vol au moment de la perte** : leases expirées → reprise automatique par inspection distante (spec §2.5). Traiter les reliquats via [orphaned-deployment.md](orphaned-deployment.md) (containers `-next` éventuels).
3. **Dérive proxy** : comparer le checksum réel au dernier appliqué connu :
   ```sql
   SELECT s.name, r.revision, r.checksum_sha256, r.applied_at
   FROM proxy_config_revisions r JOIN servers s ON s.id = r.server_id
   WHERE r.status = 'applied'
     AND r.revision = (SELECT max(revision) FROM proxy_config_revisions WHERE server_id = s.id AND status = 'applied');
   ```
   ```sh
   ssh <user>@<serveur> "cat /data/akerdock/proxy/dynamic/*.yaml | sha256sum"
   ```
   Écart = un déploiement a eu lieu dans la fenêtre RPO ; voir [postgres-failure.md](postgres-failure.md) §D.2 (redéployer la ressource pour réaligner).
4. **Fenêtre RPO** (déploiements/webhooks/objets perdus) : dérouler intégralement [postgres-failure.md](postgres-failure.md) §D.
5. **Sentinel** : les agents poussent vers l'ancien endpoint tant que DNS/IP n'ont pas convergé ; ils raccrochent seuls après l'étape 5 (architecture push, §3.8).

### Cas dégradé : dump présent, clé maître perdue

Les données non chiffrées (teams, projets, serveurs, ressources, historique) sont intactes ; **tout ce qui est `*_enc` est perdu** (les 16 colonnes du data dictionary §12). Il faut alors, dans l'ordre : recréer une clé maître neuve, re-saisir les clés SSH (ou en générer de nouvelles et les déposer sur chaque serveur via un accès de secours), re-saisir variables secrètes, credentials S3/registry/cloud, secrets webhook, config SMTP. C'est long et c'est exactement ce que la sauvegarde séparée de `master.key` évite.

## Vérification

- [ ] `GET /health` 200, login + 2FA OK, `GET /version` = tag attendu ;
- [ ] tous les serveurs `ready` après validation ; statuts observés frais (`observed_at` récents) ;
- [ ] un déploiement de test de bout en bout réussit ;
- [ ] un webhook de test (push) arrive avec `signature_valid = true` ;
- [ ] backup d'instance relancé (« Backup Now ») → `succeeded` avec upload S3 vérifié ;
- [ ] chronométrer : l'objectif produit est **RTO ≤ 2 h** (§16.4).

## Prévention

- Les 3 pièces (dump S3 + `master.key` + compose/.env) stockées **hors machine et dans deux emplacements distincts** (clé séparée des dumps, §23.1).
- Restore drill complet (cette procédure, sur une VM jetable) au moins une fois, chronométré — c'est le seul moyen de garantir le RTO.
- FQDN d'instance avec TTL DNS court (≤ 300 s) pour accélérer l'étape 5.
- Ne jamais héberger de workload critique sur le serveur `localhost` de l'instance (§3.1 : déconseillé en production) — la perte de la machine emporterait control plane **et** workloads.
