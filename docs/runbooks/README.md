# Runbooks opérateur — AkerDock

> Artefact §29.10 du PRD (`docs/PRD.md`). Ces runbooks s'appuient exclusivement sur la distribution réelle (compose 2 services — ADR-021), les tables du data dictionary (`docs/specs/data-dictionary.md`), les endpoints de l'OpenAPI (`docs/specs/openapi-v1.yaml`) et la spécification du moteur de déploiement (`docs/specs/deployment-engine.md`). Quand une commande CLI dédiée serait idéale mais n'existe pas encore, la requête SQL/API équivalente est donnée et marquée **(candidat CLI futur)**. Les valeurs non fixées par les specs sont marquées **(défaut proposé)**.

## Index

| Runbook | Quand l'utiliser | Criticité |
|---|---|---|
| [install.md](install.md) | Installation d'une nouvelle instance (compose 2 services), clé maître, clés SSH, premier root user | Moyenne (opération planifiée) |
| [upgrade-downgrade.md](upgrade-downgrade.md) | Mise à jour de release par tag d'image, rollback de release, upgrade majeur du PostgreSQL interne | Haute (fenêtre de maintenance) |
| [key-rotation.md](key-rotation.md) | Rotation de la clé maître, d'une clé SSH serveur, des secrets webhook/OAuth ; révocation d'urgence de tokens API | Haute à critique (selon contexte) |
| [postgres-failure.md](postgres-failure.md) | Base PostgreSQL interne en panne ou corrompue ; restore depuis backup ; reprise des jobs | **Critique** |
| [control-plane-restore.md](control-plane-restore.md) | Perte totale de la machine hébergeant l'instance ; restore complet sur machine neuve | **Critique** |
| [compromised-server.md](compromised-server.md) | Suspicion ou confirmation de compromission d'un serveur cible | **Critique** (incident de sécurité) |
| [stuck-cleanup.md](stuck-cleanup.md) | Cleanup Docker bloqué, ou soupçon qu'il a touché une ressource gérée/persistante | Moyenne à haute |
| [orphaned-deployment.md](orphaned-deployment.md) | Déploiement figé : worker mort, lease expirée, container `-next` qui traîne, verrou non libéré | Haute |
| [queue-dead-letter.md](queue-dead-letter.md) | Jobs en dead-letter : triage, retry/forget, causes récurrentes | Moyenne |
| [proxy-outage.md](proxy-outage.md) | Proxy d'un serveur en panne ou configuration dynamique corrompue | **Critique** (trafic entrant coupé) |
| [certificates.md](certificates.md) | Échecs ACME, fallback self-signed actif, certificats expirés, custom certs, wildcard DNS-01 | Haute |

## Conventions communes à tous les runbooks

### Anatomie de l'instance (ADR-021, §27.21)

L'instance = **2 services Docker Compose** : l'image `AkerDock` (binaire Go statique, image distroless, modes `all-in-one`/`api`/`worker`) + PostgreSQL. Arborescence sur la machine hôte **(défaut proposé)** :

```text
/var/lib/akerdock/                  # racine de l'instance sur l'hôte du control plane
├── docker-compose.yml            # définition des 2 services
├── .env                          # configuration non secrète (tag d'image, port…)
├── keys/master.key               # clé maître de chiffrement enveloppe (0600, root) — ADR-003
├── postgres/                     # répertoire de données PostgreSQL (bind mount)
└── backups/                      # backups locaux de la base interne (§7.2)
```

> Ne pas confondre avec `/var/lib/akerdock/` **sur les serveurs cibles** (arborescence normative §5.1 de la spec deployment-engine : `applications/`, `proxy/`, `backups/`, `tmp/`). Si le serveur `localhost` est utilisé, les deux cohabitent — l'instance vit alors dans un sous-répertoire dédié, ex. `/var/lib/akerdock/instance/` **(défaut proposé)**.

### Accès aux outils

- **Toutes les commandes `docker compose`** s'exécutent depuis `/var/lib/akerdock/` sur l'hôte du control plane.
- **L'image AkerDock est distroless** (ADR-021) : pas de shell dans le container. Tout diagnostic passe par les logs (`docker compose logs AkerDock`), l'API et `psql` exécuté dans le container PostgreSQL.
- **psql** :
  ```sh
  cd /var/lib/akerdock
  docker compose exec postgres psql -U AkerDock AkerDock
  ```
- **API** : base `https://<fqdn-instance>/api/v1`, auth `Authorization: Bearer $TOKEN`. Rappel : l'API est **désactivée par défaut** (§10.3) — l'activer dans les settings avant un incident, ou passer par l'UI/SQL. Dans les exemples : `export AKD=https://akerdock.example.com/api/v1` et `export TOKEN=akd_…`.
- Les mutations SQL directes sont un **dernier recours** : elles contournent l'audit (§23.4) et le verrou optimiste. Chaque runbook les signale comme telles.

### Symboles

- ⚠️ **Point de non-retour** : au-delà de cette étape, on ne peut plus revenir en arrière sans restore/perte.
- **(défaut proposé)** : valeur ou nom non fixé par le PRD/les specs ; à confirmer à l'implémentation.
- **(candidat CLI futur)** : opération qui mérite une commande `AkerDock` dédiée ; en attendant, SQL/API.
