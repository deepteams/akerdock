# Runbook — Installation de l'instance

> Références : PRD §14.1–14.2, §10.2, §7.5 ; ADR-021 (distribution compose 2 services) ; ADR-003 (clé maître) ; data dictionary §11.7 (`instance_settings`), §6.3 (`private_keys`).

## Symptômes

Sans objet — opération planifiée. Ce runbook couvre l'installation initiale d'une instance AkerDock sur une machine vierge.

## Impact

Aucun workload existant n'est affecté : l'instance ne fait que UI + déploiements SSH + monitoring (§3.3 PRD, INV-007).

## Prérequis

- **Machine** : Linux AMD64 ou ARM64, minimum **2 vCPU / 2 GB RAM / 30 GB disque** (§14.1 — AkerDock s'engage à rester réactif sur ce gabarit, §16.1(6)).
- **Docker Engine ≥ 24 + Compose v2** (snap non supporté, §3.1). Vérifier :
  ```sh
  docker version --format '{{.Server.Version}}'
  docker compose version
  ```
- **PostgreSQL 15+** : l'image PostgreSQL du compose doit être ≥ 15 (`UNIQUE NULLS NOT DISTINCT`, data dictionary §2) — utiliser le tag épinglé de la release AkerDock.
- **Réseau** : un seul port exposé pour le control plane (§27.1) — **8080 (normatif : spec [instance-config](../specs/instance-config.md) §2)** ; 80/443 seulement si le serveur `localhost` sert aussi de serveur cible avec proxy. Un enregistrement DNS pour le FQDN de l'instance (recommandé, §14.2).
- Accès root sur la machine.

## Résolution pas à pas (procédure d'installation)

### 1. Créer l'arborescence de l'instance

```sh
mkdir -p /var/lib/akerdock/keys /var/lib/akerdock/postgres /var/lib/akerdock/backups
chmod 0700 /var/lib/akerdock/keys
```

### 2. Générer la clé maître de chiffrement (ADR-003, §23.2, §27.3)

Une ligne par version de clé au format `<version>:<clé base64 32 octets>` **(normatif : spec [instance-config](../specs/instance-config.md) §3)**. Le fichier est monté en lecture seule dans le conteneur `akerdock`, qui tourne en **nonroot distroless (uid 65532)** : il doit appartenir à cet uid, sinon le démarrage échoue avec `master key file … permission denied` :

```sh
umask 077
printf '1:%s\n' "$(openssl rand -base64 32)" > /var/lib/akerdock/keys/master.key
chown 65532:65532 /var/lib/akerdock/keys/master.key
chmod 0600 /var/lib/akerdock/keys/master.key
```

(root lit le fichier quelles que soient ses permissions — le backup hors machine reste possible.)

⚠️ **Point de non-retour différé** : à partir du moment où le premier secret sera stocké, **la perte de ce fichier rend tous les secrets irrécupérables** (ADR-003). Copier immédiatement `master.key` dans un emplacement sûr **hors de la machine** (gestionnaire de mots de passe d'équipe, coffre), **séparé des backups de la base** (un attaquant qui obtient les deux lit tout, §23.1).

### 3. Écrire la configuration

`/var/lib/akerdock/.env` — noms de variables **(normatif : spec [instance-config](../specs/instance-config.md) §2)** :

```sh
cat > /var/lib/akerdock/.env <<'EOF'
AKERDOCK_TAG=v1.0.0                  # tag d'image explicite, jamais "latest"
AKERDOCK_PORT=8080
POSTGRES_PASSWORD=<généré: openssl rand -hex 24>
# Bootstrap non interactif du premier root user (§10.2) — validation stricte email/nom/mot de passe
AKERDOCK_ROOT_EMAIL=admin@example.com
AKERDOCK_ROOT_NAME=Admin
AKERDOCK_ROOT_PASSWORD=<mot de passe fort>
EOF
chmod 0600 /var/lib/akerdock/.env
```

`/var/lib/akerdock/docker-compose.yml` **(normatif : spec [instance-config](../specs/instance-config.md) §4 — 2 services, un seul port exposé, conforme ADR-021 ; le fichier de référence de la spec utilise les identifiants en minuscules `akerdock` et des volumes nommés)** :

```yaml
services:
  AkerDock:
    image: ghcr.io/deepteams/akerdock:${AKERDOCK_TAG}
    command: ["all-in-one"]                       # modes all-in-one/api/worker (§18.2)
    restart: unless-stopped
    ports:
      - "${AKERDOCK_PORT}:8080"
    environment:
      AKERDOCK_DATABASE_URL: postgres://AkerDock:${POSTGRES_PASSWORD}@postgres:5432/AkerDock?sslmode=disable
      AKERDOCK_MASTER_KEY_FILE: /run/secrets/master.key
      AKERDOCK_ROOT_EMAIL: ${AKERDOCK_ROOT_EMAIL}
      AKERDOCK_ROOT_NAME: ${AKERDOCK_ROOT_NAME}
      AKERDOCK_ROOT_PASSWORD: ${AKERDOCK_ROOT_PASSWORD}
    volumes:
      - ./keys/master.key:/run/secrets/master.key:ro
    depends_on:
      postgres:
        condition: service_healthy

  postgres:
    image: postgres:17                            # tag épinglé par la release
    restart: unless-stopped
    environment:
      POSTGRES_USER: AkerDock
      POSTGRES_DB: AkerDock
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}
    volumes:
      - ./postgres:/var/lib/postgresql/data
      - ./backups:/backups
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U AkerDock"]
      interval: 5s
      timeout: 3s
      retries: 10
```

### 4. Démarrer

```sh
cd /var/lib/akerdock
docker compose up -d
```

Au premier démarrage, le binaire applique les **migrations SQL versionnées** (ADR-025) puis crée le **premier root user** depuis les variables de bootstrap (§10.2) — la création échoue explicitement si email/nom/mot de passe ne passent pas la validation stricte. Une fois le root créé, retirer `AKERDOCK_ROOT_PASSWORD` du `.env` **(normatif : spec [instance-config](../specs/instance-config.md) §6 — les variables de bootstrap ne sont lues que si aucun utilisateur n'existe, et consommées une seule fois)**.

Dès que la première team existe, le bootstrap pré-enregistre aussi le serveur **`localhost`** (la machine hôte, jointe en SSH via `host.docker.internal` avec la clé d'instance — spec instance-config §6.2). `install.sh` autorise automatiquement la clé publique d'instance pour l'utilisateur qui installe, et le scheduler retente la validation toutes les ~5 minutes (pendant 24 h) : le serveur passe `ready` tout seul, sans action. Prérequis : un serveur SSH actif sur l'hôte. Installation manuelle (sans `install.sh`) : ajouter `/var/lib/akerdock/ssh/instance_ed25519.pub` à l'`authorized_keys` de `AKERDOCK_LOCALHOST_USER`. Supprimé, ce serveur n'est jamais recréé.

### 5. Onboarding et clés SSH

1. Se connecter à l'UI (`http://<host>:8080`), suivre l'onboarding guidé (§14.2) : première team, premier serveur, première ressource. Activer le **2FA TOTP** du root immédiatement (§10.2).
2. Générer une clé SSH pour les serveurs cibles — les clés sont scopées **par team** (§23.2, table `private_keys`) et il est recommandé d'utiliser **une clé par serveur** (séparabilité en cas de compromission, §23.1) :
   ```sh
   ssh-keygen -t ed25519 -N '' -C akerdock-server-01 -f /tmp/akerdock-server-01
   ```
   Enregistrer via l'UI (Private Keys) ou l'API :
   ```sh
   curl -sS -X POST "$AKD/private-keys" \
     -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
     -d "{\"name\":\"server-01\",\"private_key\":\"$(awk '{printf "%s\\n",$0}' /tmp/akerdock-server-01)\"}"
   ```
   Puis **supprimer les fichiers temporaires** (`shred -u /tmp/akerdock-server-01*`) : la clé chiffrée en base (`private_keys.private_key_enc`) devient la seule copie.
3. Déposer la clé publique sur le serveur cible (`~/.ssh/authorized_keys` de l'utilisateur SSH), puis ajouter et valider le serveur (UI ou `POST /servers` + `POST /servers/{uuid}/validate`).
4. Configurer sans attendre : **FQDN de l'instance** et email transactionnel (§14.2), et le **plan de backup de la base de l'instance** avec destination S3 (`database_backup_plans.is_instance_backup = true`, §7.5) — voir [postgres-failure.md](postgres-failure.md).

## Vérification post-install

```sh
cd /var/lib/akerdock
docker compose ps                                    # 2 services Up, postgres healthy
docker compose logs --tail 50 AkerDock              # migrations OK, pas d'erreur au boot
curl -fsS http://localhost:8080/api/v1/health        # healthcheck non authentifié (§12)
curl -fsS -H "Authorization: Bearer $TOKEN" "$AKD/version"   # si API activée
docker compose exec postgres psql -U AkerDock AkerDock \
  -c "SELECT fqdn, timezone, registration_enabled, api_enabled FROM instance_settings;"
```

Checklist :

- [ ] `GET /health` répond 200 ;
- [ ] login root + 2FA fonctionnels ; inscription publique **désactivée** (`registration_enabled = false`, défaut) ;
- [ ] `master.key` sauvegardée hors machine, permissions `0600 root:root` ;
- [ ] premier serveur `ready` après validation ;
- [ ] plan de backup de l'instance créé et une exécution « Backup Now » réussie (`backup_executions.status = 'succeeded'`).

## Prévention

- Épingler **toujours** un tag d'image explicite ; les upgrades passent par [upgrade-downgrade.md](upgrade-downgrade.md).
- Sauvegarder le triplet `docker-compose.yml` + `.env` + `master.key` hors machine dès l'installation : c'est exactement ce qu'exige [control-plane-restore.md](control-plane-restore.md).
- Fermer le port du control plane derrière le FQDN proxifié dès que possible (§14.2, §27.1) ; le hardening OS et le firewall cloud restent à votre charge (§10.4 — Docker bypasse UFW).
