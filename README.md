# AkerDock

PaaS self-hosted en Go : déployez applications, bases de données et services en containers Docker sur vos propres serveurs, avec reverse proxy, SSL automatique, backups et monitoring — sans vendor lock-in.

AkerDock déploie applications, bases de données et stacks Docker Compose sur vos propres serveurs (voir le [PRD](docs/PRD.md)) : binaire Go statique unique, PostgreSQL comme seule dépendance (état **et** queue de jobs), API spec-first.

> **Statut : phase de conception terminée, développement non commencé.** Ce dépôt contient aujourd'hui le socle documentaire (PRD, ADRs, specs, runbooks) qui sert de référence au développement.

## Documentation

| Répertoire | Contenu |
|---|---|
| [`docs/PRD.md`](docs/PRD.md) | Spécification produit : périmètre fonctionnel et exigences vérifiables |
| [`docs/adr/`](docs/adr/README.md) | 26 Architecture Decision Records, tous acceptés |
| [`docs/specs/`](docs/specs/) | Specs techniques : [OpenAPI v1](docs/specs/openapi-v1.yaml), ERD, threat model, matrice RBAC, contrat proxy, plan E2E… |
| [`docs/runbooks/`](docs/runbooks/README.md) | Runbooks opérationnels (installation, pannes, rotation de clés…) |

## Décisions structurantes

- **Transport** : SSH d'abord, agent sortant en cible ([ADR-001](docs/adr/ADR-001-transport-ssh-puis-agent.md))
- **Queue durable dans PostgreSQL**, aucun bus externe ([ADR-002](docs/adr/ADR-002-queue-postgresql.md))
- **Runtime Docker standalone** — Kubernetes et Swarm écartés ([ADR-004](docs/adr/ADR-004-runtime-docker-standalone.md))
- **Socle Go** : pgx + sqlc, chi + oapi-codegen, spec-first ([ADR-025](docs/adr/ADR-025-socle-go-pgx-sqlc-chi-oapi-codegen.md))
- **Distribution** : docker-compose minimal à 2 services (AkerDock + PostgreSQL) ([ADR-021](docs/adr/ADR-021-distribution-compose-deux-services.md))
- **Temps réel** : SSE, WebSocket réservé au terminal ([ADR-024](docs/adr/ADR-024-temps-reel-sse-websocket-terminal.md))

## Installation depuis les sources

Prérequis : Docker Engine ≥ 24 avec Compose v2, et `openssl`.

```sh
git clone https://github.com/deepteams/akerdock.git
cd akerdock
./install.sh
```

Le script construit l'image depuis le Dockerfile local (aucune image AkerDock publiée n'est requise), génère la clé maître (`keys/master.key` — **à sauvegarder hors machine immédiatement**) et la configuration (`.env`), puis démarre la stack de référence (ADR-021) et affiche les identifiants du premier root user. Pour mettre à jour une instance existante : `git pull && ./install.sh` — le script reconstruit l'image et redéploie, les migrations s'appliquent au démarrage et l'état persiste dans les volumes. Le port et le premier utilisateur se personnalisent au premier lancement via `AKERDOCK_PORT`, `AKERDOCK_ROOT_EMAIL`, etc. (voir l'en-tête du script) ; l'installation manuelle détaillée reste documentée dans [docs/runbooks/install.md](docs/runbooks/install.md).

## Développement

Prérequis : Go ≥ 1.26 et [golangci-lint](https://golangci-lint.run) v2 (les autres outils — sqlc, oapi-codegen, goose — sont épinglés dans `go.mod` et invoqués via `go tool`).

```sh
make generate   # régénère le code depuis la spec OpenAPI et les requêtes sqlc
make build      # compile bin/akerdock
make test lint  # tests et lint
```

Les conventions (langue, commits, workflow spec-first, migrations) sont décrites dans [CONTRIBUTING.md](CONTRIBUTING.md).

## Licence

[Apache 2.0](LICENSE) ([ADR-020](docs/adr/ADR-020-licence-apache-2-0.md)).
