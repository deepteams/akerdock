# Runbook — Serveur cible compromis

> Références : PRD §23.1 (« Un serveur cible compromis ne doit pas donner accès aux autres serveurs : clés/credentials séparables et secrets distribués au strict besoin »), §23.2, §20.7 (adoption), INV-008 ; spec deployment-engine §5.1–5.2 (ce qui est matérialisé sur le serveur) ; data dictionary §6.1 (`servers`), §6.3 (`private_keys`), §12.

## Symptômes

- Alerte sécurité (IDS, provider cloud, abuse report), containers/processus inconnus, trafic sortant anormal, métriques Sentinel incohérentes, modification inexpliquée de `/var/lib/akerdock/` sur le serveur.
- Côté AkerDock : déploiements qui échouent bizarrement sur ce serveur, dérive de checksum proxy, ressources non gérées apparues (`docker ps` sans label `akerdock.managed`).

## Impact

Ce que l'attaquant possède (root sur le serveur cible) :

- **Tous les workloads du serveur** et leurs données (volumes, bases y résidant).
- **Les secrets distribués au serveur** — et uniquement eux (§23.1, distribution au strict besoin) : fichiers `runtime.env`/`build.env`/`secrets/` sous `/var/lib/akerdock/applications/*/env/` (spec §5.1–5.2), certificats TLS (`/var/lib/akerdock/proxy/`), credentials registry présents dans `/root/.docker/config.json` après `docker login`, token Sentinel du serveur.
- Ce que l'attaquant **ne possède pas** : la base du control plane, la clé maître, les **clés SSH privées** (elles ne quittent jamais le control plane ; seule la clé *publique* est dans `authorized_keys`), les secrets des autres serveurs/teams. L'architecture est push : le serveur n'a aucun credential pour contacter le control plane, hormis le token Sentinel (limité au push de métriques).

## Diagnostic

1. **Périmètre de la clé SSH** — la clé du serveur est-elle partagée ? (elle ne devrait pas l'être) :
   ```sql
   SELECT s2.uuid, s2.name FROM servers s1
   JOIN servers s2 ON s2.private_key_id = s1.private_key_id AND s2.deleted_at IS NULL
   WHERE s1.uuid = '<server_uuid>';
   -- + git sources utilisant cette clé (deploy keys) : vérifier les références de private_keys
   ```
2. **Inventaire de ce qui était exposé sur ce serveur** :
   ```sh
   curl -sS "$AKD/servers/$SERVER_UUID/resources" -H "Authorization: Bearer $TOKEN"
   ```
   ```sql
   -- Variables d'environnement des ressources du serveur
   -- (les valeurs étaient matérialisées en clair dans runtime.env/build.env sur le serveur, spec §5.2) :
   SELECT r.uuid AS resource_uuid, r.name, r.resource_type, ev.key, ev.is_secret, ev.is_preview
   FROM resources r
   JOIN destinations d ON d.id = r.destination_id
   JOIN servers s ON s.id = d.server_id
   LEFT JOIN environment_variables ev ON ev.resource_id = r.id
   WHERE s.uuid = '<server_uuid>' AND r.deleted_at IS NULL
   ORDER BY r.name, ev.key;
   ```
   Compléter l'inventaire : credentials **registry** utilisés par les apps du serveur (`registry_credentials` référencés par leurs build configs), **S3** utilisés par les plans de backup des bases du serveur, **domaines/certificats** servis par son proxy, **bases de données** y résidant (`database_credentials`).
3. **Ce que l'attaquant a pu faire côté AkerDock** : rien directement (pas de credential entrant), mais vérifier l'audit pour toute activité anormale autour du serveur :
   ```sql
   SELECT occurred_at, actor_kind, actor_display, action, result, ip
   FROM audit_events WHERE target_uuid = '<server_uuid>' ORDER BY occurred_at DESC LIMIT 50;
   ```

## Résolution pas à pas

### 1. Isoler (sans détruire les preuves)

1. **Geler le pilotage** : passer le serveur en maintenance pour empêcher tout nouveau job de le cibler (l'état `preparing` exige un serveur `ready`, spec §4) — pas d'endpoint dédié dans l'OpenAPI v1 **(candidat CLI/API futur)** :
   ```sql
   UPDATE servers SET status = 'maintenance', updated_at = now() WHERE uuid = '<server_uuid>';
   ```
2. **Révoquer uniquement la clé SSH de ce serveur** (les clés sont séparables, §23.1 — les autres serveurs ne sont pas touchés). La clé privée n'a pas fuité, mais on coupe le canal de pilotage vers une machine hostile et on prépare la ré-installation : ne plus l'utiliser, et si la clé était partagée (diagnostic 1), **rotation immédiate sur les autres serveurs** via [key-rotation.md](key-rotation.md) §B.
3. **Isoler réseau** au niveau du firewall du provider cloud (§10.4 : ne pas compter sur UFW, Docker le bypasse) : bloquer tout sauf votre IP d'investigation. Si le serveur portait du trafic de production, c'est une coupure de service assumée — un serveur compromis ne doit pas continuer à servir vos utilisateurs.
4. **Révoquer le token Sentinel** du serveur (régénération dans l'UI serveur ; le hash `servers.sentinel_token_hash` change) pour couper le seul canal entrant vers l'instance.

⚠️ Ne pas supprimer l'objet `Server` : la suppression est RESTRICT tant que des ressources y sont rattachées (INV-008), et vous perdriez l'inventaire. « Retirer de AkerDock » ≠ « détruire le VPS » (§3.2) — et ni l'un ni l'autre ne se fait avant la fin de l'investigation.

### 2. Rotation ciblée (tout ce qui était distribué au serveur)

Traiter comme compromis, **à la source** :

- Toutes les **variables secrètes** des ressources du serveur (inventaire du Diagnostic 2) : régénérer les valeurs chez les fournisseurs concernés (API keys tierces, etc.) et les mettre à jour dans AkerDock.
- **Mots de passe des bases** hébergées sur le serveur (`database_credentials`) et de toute base externe dont l'URL figurait dans un `runtime.env` du serveur.
- **Credentials registry** utilisés par les apps du serveur (rotation côté registry + mise à jour `registry_credentials`).
- **Credentials S3** des plans de backup exécutés depuis ce serveur (rotation côté provider + mise à jour `s3_storages` ; re-vérification `ListObjectsV2` §7.4).
- **Certificats TLS** : les clés privées étaient sur le serveur (`/var/lib/akerdock/proxy/certs`, storage ACME) — révoquer les customs, forcer la ré-émission ACME sur le serveur de remplacement ([certificates.md](certificates.md)).
- **CA SSL bases** du serveur (`servers.ca_key_enc`) : régénérer depuis l'UI (§6.3).
- **Deploy keys Git** : normalement supprimées après clone (spec §5.3.1), mais si une compromission longue est suspectée, les retirer des repos et en générer de nouvelles.

### 3. Décision : réinstaller ou adopter

- **Réinstaller (fortement recommandé)** : OS réinstallé chez le provider → nouveau serveur AkerDock (nouvelle clé SSH dédiée, `POST /servers` + validate) → redéployer les ressources depuis leur configuration (source de vérité = PostgreSQL, §18.3) → restaurer les **données** (volumes, bases) **uniquement depuis des backups antérieurs à la compromission**, après contrôle des checksums (`backup_executions.checksum_sha256`). ⚠️ Un backup postérieur à l'intrusion peut être piégé/altéré.
- **Adopter (§20.7)** : seulement si le forensic conclut à un faux positif ou à une compromission strictement confinée (ex. un seul container applicatif sans échappée) : rotation ciblée quand même, puis revalidation du serveur. En cas de doute : réinstaller.

### 4. Clôture

Une fois les ressources re-déployées ailleurs ou sur le serveur réinstallé : supprimer les ressources restantes de l'ancien objet serveur (prévisualisation + choix explicite données/objet, §20.6), puis le serveur ; enfin détruire le VPS chez le provider (action distincte et confirmée, §3.2). Consigner l'incident (timeline, périmètre, rotations effectuées).

## Vérification

- L'ancienne clé publique n'ouvre plus rien (elle n'est plus dans aucun `authorized_keys` actif) ; la clé n'est plus référencée (`private_keys` supprimable sans RESTRICT).
- Les secrets rotés fonctionnent : déploiement de test OK, backups OK (S3 re-vérifié `is_usable = true`), webhooks OK.
- Aucune ressource de l'inventaire n'a été oubliée : re-dérouler la requête d'inventaire → liste vide ou ressources migrées.
- Audit : les rotations et suppressions apparaissent dans `audit_events`.

## Prévention

- **Une clé SSH par serveur**, jamais partagée (rend ce runbook local au lieu de global).
- Secrets distribués au strict besoin (§23.1) : ne pas mettre des variables « de confort » globales dans les ressources ; utiliser les build secrets BuildKit (hors image, spec §5.2).
- Builders isolés pour le code non fiable (ADR-005) ; previews de forks sans secrets (INV-010).
- Firewall provider restrictif (SSH depuis l'IP de l'instance uniquement si possible) ; patching serveur régulier — à la charge de l'opérateur, hors périmètre produit (ADR-027).
- Backups avec checksums et rétention suffisante pour disposer d'un point **antérieur** à une intrusion découverte tardivement.
