# Runbook — Proxy en panne ou configuration corrompue

> Références : PRD §4.1 (cycle de vie du proxy, « l'arrêt du proxy coupe tout le trafic entrant du serveur »), §18.3 (routage : génération déterministe + validation + checksum), §20.1(5) ; ADR-009 (représentation intermédiaire) ; spec deployment-engine §7.1–7.2 ; data dictionary §11.1 (`proxy_config_revisions`), §6.1 (colonnes `proxy_*` de `servers`).

## Symptômes

- **Tous** les sites d'un même serveur sont down (timeout, connexion refusée, 502) — les apps des autres serveurs répondent.
- Dashboard : `proxy_observed_status` du serveur `unhealthy`/`exited` ; notification « proxy » (§11).
- Config corrompue : le proxy tourne mais route mal (404 Traefik « page not found », mauvais backend), logs Traefik pleins d'erreurs du provider `file`.

## Impact

- Tout le **trafic entrant** du serveur est coupé ou dégradé (§4.1). Les containers applicatifs, bases et tâches **continuent de tourner** : seul l'accès HTTP(S) externe est cassé.
- Le control plane et les autres serveurs ne sont pas affectés (proxy par serveur, §3.3).

## Diagnostic

1. **Le container proxy** (sur le serveur) :
   ```sh
   ssh <user>@<serveur> "docker ps -a --filter label=akerdock.type=proxy --format '{{.Names}}\t{{.Status}}\t{{.Image}}'"
   ssh <user>@<serveur> "docker logs --tail 100 \$(docker ps -aq --filter label=akerdock.type=proxy)"
   ssh <user>@<serveur> "curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:80 ; curl -sk -o /dev/null -w '%{http_code}\n' https://127.0.0.1:443"
   ```
   (Ports 80/443 = défauts ; vérifier `servers.proxy_http_port`/`proxy_https_port`, configurables par serveur — §27.1.)
2. **La configuration dynamique** — dérive par rapport à la dernière révision appliquée (§18.3) :
   ```sql
   SELECT revision, status, checksum_sha256, applied_at, error
   FROM proxy_config_revisions
   WHERE server_id = (SELECT id FROM servers WHERE uuid = '<server_uuid>')
   ORDER BY revision DESC LIMIT 5;
   ```
   ```sh
   ssh <user>@<serveur> "ls -la /data/akerdock/proxy/dynamic/ && sha256sum /data/akerdock/proxy/dynamic/*.yaml"
   ```
   Un fichier au checksum inconnu de la base = édition manuelle ou corruption ; un fichier YAML invalide est nommé explicitement dans les logs Traefik (`error while parsing dynamic configuration`).
3. **État désiré vs observé** : `proxy_desired_state` doit être `running` — s'il est `stopped`, quelqu'un a arrêté le proxy volontairement (audit : `action LIKE 'server.proxy%'` dans `audit_events`).
4. Distinguer d'une panne **certificats** (sites up en HTTP, erreurs TLS uniquement) → [certificates.md](certificates.md).

## Résolution pas à pas

### A. Container proxy arrêté ou crashloop

1. Redémarrer via l'UI serveur (cycle de vie du proxy : start/restart, §4.1) — c'est le chemin audité. À défaut, sur le serveur :
   ```sh
   ssh <user>@<serveur> "docker restart \$(docker ps -aq --filter label=akerdock.type=proxy)"
   ```
2. S'il crashe au boot à cause d'un fichier statique/dynamique invalide → cas B.
3. S'il crashe pour une autre raison (port déjà pris, image corrompue) : libérer le port (`ss -ltnp | grep ':80'`), ou re-provisionner le proxy (cas C).

### B. Configuration dynamique corrompue — restore de la dernière révision valide

Le fichier fait foi pour le routage (spec §7.1) ; la base conserve chaque révision générée avec son contenu (`proxy_config_revisions.content`) :

1. Extraire la dernière révision `applied` **(candidat CLI futur — `AkerDock proxy restore <server>`)** :
   ```sh
   docker compose exec -T postgres psql -U AkerDock AkerDock -At -c "
     SELECT content FROM proxy_config_revisions
     WHERE server_id = (SELECT id FROM servers WHERE uuid = '<server_uuid>')
       AND status = 'applied'
     ORDER BY revision DESC LIMIT 1;" > /tmp/proxy-restore.yaml
   ```
2. Sauvegarder l'état corrompu puis appliquer **atomiquement** (même mécanique que le moteur : tmp + `mv -f`, spec §7.2.3) :
   ```sh
   scp /tmp/proxy-restore.yaml <user>@<serveur>:/data/akerdock/proxy/dynamic/.restore.tmp
   ssh <user>@<serveur> "cp -a /data/akerdock/proxy/dynamic /data/akerdock/tmp/dynamic-corrupted-\$(date -u +%s) \
     && mv -f /data/akerdock/proxy/dynamic/.restore.tmp /data/akerdock/proxy/dynamic/<fichier_cible>.yaml"
   ```
   Le provider `file` de Traefik (`watch: true`) recharge sans redémarrage.
   > Si la corruption touche plusieurs applications, alternative plus sûre : **redéployer chaque application concernée** — chaque déploiement régénère son fichier `/data/akerdock/proxy/dynamic/<app_uuid>.yaml` de façon déterministe depuis la représentation intermédiaire (spec §7.2.7).
3. ⚠️ Ne jamais « corriger à la main » un fichier dynamique et en rester là : la base ne connaîtrait pas ce contenu (checksum divergent) et la prochaine génération l'écrasera. Toute correction manuelle doit converger vers un redéploiement/régénération par AkerDock.

### C. Redéploiement complet du proxy

Si le container proxy lui-même est irrécupérable (image corrompue, config statique cassée) :

1. ```sh
   ssh <user>@<serveur> "docker stop \$(docker ps -aq --filter label=akerdock.type=proxy) ; docker rm \$(docker ps -aq --filter label=akerdock.type=proxy)"
   ```
   ⚠️ **Coupure totale du trafic entrant du serveur** entre le `rm` et la fin du re-provisionnement — fenêtre à annoncer. Les certificats et fichiers dynamiques sur `/data/akerdock/proxy/` (bind mount) **survivent** à la suppression du container.
2. Relancer la validation du serveur, qui redéploie et vérifie le proxy (workflow d'onboarding §20.1, étape 5) :
   ```sh
   curl -sS -X POST "$AKD/servers/$SERVER_UUID/validate" -H "Authorization: Bearer $TOKEN"
   ```

## Vérification

- Container proxy `running` ; `proxy_observed_status` revenu à `healthy` côté dashboard.
- **Checksum aligné** : `sha256sum` distant = `checksum_sha256` de la dernière révision `applied` en base (§18.3).
- Chaque domaine critique répond à travers le proxy :
  ```sh
  curl -fsS -o /dev/null -w '%{http_code} %{ssl_verify_result}\n' --resolve <fqdn>:443:<ip_serveur> https://<fqdn>/
  ```
- Un déploiement de test sur ce serveur passe l'étape `switching` (vérification proxy incluse, spec §7.2.4).
- Logs Traefik silencieux sur le provider `file`.

## Prévention

- Ne jamais éditer `/data/akerdock/proxy/dynamic/` à la main ; l'édition de config proxy passe par l'UI (§4.1), versionnée en `proxy_config_revisions`.
- Surveiller la notification « proxy obsolète » (§11) et mettre à jour l'image proxy en fenêtre choisie.
- Activer l'uptime monitoring intégré (§27.17) sur au moins un domaine par serveur : détection en secondes plutôt qu'au premier ticket utilisateur.
- La rétention des révisions (« purge en conservant les N dernières par serveur », data dictionary §11.1) est votre profondeur de rollback — ne pas la réduire à 1.
