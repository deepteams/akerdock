# Runbook — Rotation des clés et secrets

> Références : PRD §23.2, §19.2, §27.3 ; ADR-003 (chiffrement enveloppe, versionnement de clé) ; data dictionary §2.7 (format `*_enc`), §12 (inventaire des colonnes chiffrées), §4.8 (`api_tokens`), §6.3 (`private_keys`) ; OpenAPI (`/private-keys`, `/servers/{uuid}`, `/servers/{uuid}/validate`, `/teams/{team_uuid}/tokens`, `/system/encryption`, `/system/encryption/rotate`).

## Symptômes

Sans objet en fonctionnement normal (rotation planifiée). Déclencheurs d'urgence : fuite suspectée de la clé maître, d'une clé SSH, d'un secret webhook/OAuth ou d'un token API ; départ d'un opérateur privilégié ; exigence de conformité.

## Impact

- Rotation de la clé maître : **aucune interruption** — le re-chiffrement est progressif par version de clé, sans réécriture bloquante (§19.2, ADR-003).
- Rotation d'une clé SSH serveur : brève fenêtre où un déploiement vers ce serveur peut échouer (retry automatique d'infra) ; les workloads ne sont pas touchés.
- Révocation de tokens API : coupe immédiatement les intégrations (CI, MCP) qui les utilisent.

## Diagnostic

Vue d'ensemble via l'API (permission `root`) : `GET /system/encryption` — version de clé active, versions encore référencées en base, compteurs de lignes par version et par colonne chiffrée, job de re-chiffrement en cours le cas échéant.

Rappel du format (data dictionary §2.7) : chaque colonne `*_enc` est `key_version (4 octets big-endian) || nonce (12) || ciphertext AES-256-GCM`. La version de clé est donc lisible en SQL. Histogramme des versions en usage (fallback SQL équivalent) :

```sql
-- exemple sur private_keys ; répéter sur chaque colonne chiffrée (liste §12 du data dictionary) :
-- private_keys.private_key_enc, mfa_factors.secret_enc, cloud_credentials.config_enc,
-- registry_credentials.password_enc, s3_storages.access_key_enc + secret_key_enc,
-- github_apps.client_secret_enc + webhook_secret_enc + app_private_key_enc,
-- webhook_endpoints.secret_enc, environment_variables.value_enc, shared_variables.value_enc,
-- database_credentials.password_enc, servers.ca_key_enc + log_drain_config_enc,
-- notification_channels.config_enc, instance_settings.transactional_email_config_enc
SELECT (get_byte(private_key_enc,0)<<24) | (get_byte(private_key_enc,1)<<16)
     | (get_byte(private_key_enc,2)<<8)  |  get_byte(private_key_enc,3) AS key_version,
       count(*)
FROM private_keys GROUP BY 1 ORDER BY 1;
```

Serveurs partageant une même clé SSH (à connaître avant toute révocation) :

```sql
SELECT pk.uuid AS key_uuid, pk.name, count(s.id) AS servers, array_agg(s.name) AS server_names
FROM private_keys pk LEFT JOIN servers s ON s.private_key_id = pk.id AND s.deleted_at IS NULL
GROUP BY 1,2 ORDER BY 3 DESC;
```

## Résolution pas à pas

### A. Rotation de la clé maître (enveloppe — ADR-003)

1. **Ajouter** une nouvelle version au fichier de clés, **sans supprimer les anciennes** :
   ```sh
   cd /var/lib/akerdock
   cp keys/master.key keys/master.key.bak-$(date -u +%Y%m%d)
   printf '2:%s\n' "$(openssl rand -base64 32)" >> keys/master.key
   ```
   ⚠️ **Ne jamais retirer une version tant qu'au moins une ligne en base la référence** : les ciphertexts correspondants deviendraient définitivement illisibles.
2. **Recharger** : `docker compose up -d AkerDock` — la version active pour les nouveaux chiffrements devient la plus haute **(normatif : spec [instance-config](../specs/instance-config.md) §3)**.
3. Sauvegarder immédiatement le nouveau `master.key` hors machine (mêmes règles qu'à l'installation).
4. **Re-chiffrement progressif** : les lignes sont réécrites paresseusement à la lecture/écriture (§19.2). Pour forcer la convergence complète — nécessaire si la rotation répond à une fuite — déclencher le re-chiffrement actif vers la version active :
   ```sh
   curl -sS -X POST "$AKD/system/encryption/rotate" \
     -H "Authorization: Bearer $ROOT_TOKEN" -H "Idempotency-Key: rotate-$(date -u +%Y%m%d)"
   # 202 + job audité ; réécriture par lots, sans blocage ; 409 si un re-chiffrement est déjà en cours
   ```
   Suivre l'avancement avec `GET /system/encryption` (compteurs de lignes par version de clé et par colonne) ; l'histogramme SQL du Diagnostic reste le fallback, colonne par colonne.
5. Quand **plus aucune ligne** ne porte l'ancienne version (`GET /system/encryption` : la version 2 est la seule référencée — ou histogramme SQL = version 2 uniquement, sur les 16 colonnes de la liste §12) : retirer la ligne `1:` de `master.key`, recharger, re-sauvegarder hors machine.

⚠️ **Cas fuite avérée de la clé maître** : la rotation ne suffit pas si l'attaquant a aussi un dump de la base (il lit tout ce qui était chiffré avec l'ancienne version). Traiter alors chaque secret comme compromis : rotation **à la source** (mots de passe DB, tokens DNS/registry/S3, webhook secrets, clés SSH — voir sections B/C/D), pas seulement re-chiffrement.

### B. Rotation d'une clé SSH de serveur

Les clés sont séparables par serveur (§23.1) — la rotation se fait serveur par serveur, sans interruption :

1. Générer et enregistrer une nouvelle clé (voir [install.md](install.md) étape 5) : `POST /private-keys` → noter `private_key_uuid`.
2. **Installer d'abord la nouvelle clé publique** sur le serveur (via le terminal web AkerDock, ou un accès SSH direct) :
   ```sh
   echo 'ssh-ed25519 AAAA… akerdock-server-01-2026' >> ~/.ssh/authorized_keys
   ```
3. Basculer le serveur sur la nouvelle clé :
   ```sh
   curl -sS -X PATCH "$AKD/servers/$SERVER_UUID" \
     -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
     -d "{\"private_key_uuid\":\"$NEW_KEY_UUID\"}"
   curl -sS -X POST "$AKD/servers/$SERVER_UUID/validate" -H "Authorization: Bearer $TOKEN"
   ```
4. Après validation `ready` : **retirer l'ancienne clé publique** de `authorized_keys` sur le serveur.
5. Supprimer l'ancienne clé côté AkerDock (`DELETE /private-keys/{uuid}`) — refusé en **RESTRICT** tant qu'un serveur ou une git source la référence (§19.2) : utiliser la requête « serveurs partageant une clé » du Diagnostic pour trouver les références restantes.

⚠️ Ne jamais inverser 4 et 3 : retirer l'ancienne clé de `authorized_keys` avant la bascule vous enferme dehors (récupération = console du provider).

### C. Rotation des secrets webhook / OAuth

- **Webhook entrant (HMAC, `webhook_endpoints.secret_enc`)** : générer un nouveau secret (`openssl rand -hex 32`), le mettre à jour **d'abord côté AkerDock** (UI de l'application → webhook ; pas d'endpoint dédié dans l'OpenAPI v1 — **candidat API/CLI futur**), **puis côté provider Git** (settings du repo). Dans cet ordre, la fenêtre d'invalidité produit des livraisons `signature_valid = false` visibles dans `webhook_deliveries`, sans déclenchement indu (INV-009).
- **GitHub App (`github_apps.client_secret_enc`, `webhook_secret_enc`, `app_private_key_enc`)** : régénérer sur github.com (Settings → Developer settings → GitHub Apps), reporter dans l'UI AkerDock. Les deux secrets peuvent coexister brièvement côté GitHub (client secrets multiples) — en profiter pour une rotation sans coupure.
- **OAuth dashboard (Azure/GitHub/GitLab/Google/Bitbucket/OIDC, §10.2)** : régénérer le client secret chez l'IdP, reporter dans les settings d'instance. Les sessions ouvertes ne sont pas coupées ; seuls les nouveaux logins utilisent le nouveau secret.

Vérification immédiate : provoquer un événement (push de test) et contrôler :

```sql
SELECT delivery_id, event_type, signature_valid, status, ignore_reason, received_at
FROM webhook_deliveries ORDER BY received_at DESC LIMIT 5;
```

### D. Révocation d'urgence de tokens API

Token par token (audité, préféré) :

```sh
curl -sS "$AKD/teams/$TEAM_UUID/tokens" -H "Authorization: Bearer $TOKEN"     # inventaire
curl -sS -X DELETE "$AKD/teams/$TEAM_UUID/tokens/$TOKEN_UUID" -H "Authorization: Bearer $TOKEN"
```

Révocation **massive** (fuite générale, dernier recours SQL — contourne l'audit, consigner manuellement) **(candidat CLI futur)** :

```sql
UPDATE api_tokens SET revoked_at = now(), updated_at = now() WHERE revoked_at IS NULL;
```

Couper toute l'API le temps de l'enquête (réversible) : `POST /system/api/disable` (permission `root`) ou le toggle des settings d'instance — après cet appel, seuls `GET /health` et la réactivation via l'UI restent disponibles. Identifier l'usage récent des tokens compromis :

```sql
SELECT token_prefix, name, last_used_at, ip_allowlist, permissions
FROM api_tokens ORDER BY last_used_at DESC NULLS LAST LIMIT 20;
-- et l'audit des actions du token :
SELECT occurred_at, action, target_kind, target_uuid, result, ip
FROM audit_events WHERE actor_kind = 'token' AND actor_uuid = '<token_uuid>'
ORDER BY occurred_at DESC LIMIT 100;
```

## Vérification

- Clé maître : `GET /system/encryption` (ou histogrammes SQL) = uniquement la nouvelle version référencée, job de re-chiffrement `succeeded` ; un `docker compose restart AkerDock` puis la révélation d'un secret (avec `read:sensitive`) prouve que la nouvelle clé déchiffre.
- Clé SSH : `POST /servers/{uuid}/validate` → `ready` ; un déploiement de test passe.
- Webhook : livraison de test `signature_valid = true`, déploiement déclenché.
- Tokens : l'ancien token répond `401` ; l'entrée d'audit `token.*` existe.

## Prévention

- Une clé SSH **par serveur** (jamais partagée) ; rotation calendaire (ex. annuelle) plutôt qu'en réaction.
- Tokens API avec `expires_at`, permissions minimales (`deploy` pour la CI, §16.3) et `ip_allowlist` CIDR (§10.3).
- La clé maître et les dumps de base ne doivent **jamais** être stockés au même endroit (§23.1).
- Tester la procédure de rotation de la clé maître à froid (staging) avant d'en avoir besoin à chaud ; les scénarios « rotation de clé pendant job » font partie des tests obligatoires (§23.5).
