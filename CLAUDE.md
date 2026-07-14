# AkerDock — conventions du projet

PaaS self-hosted en Go : déploiement d'applications, bases et stacks compose sur des serveurs SSH, avec proxy, HTTPS automatique, backups et previews de PR.

**Les conventions d'ingénierie sont dans [CONTRIBUTING.md](CONTRIBUTING.md)** (layout du dépôt, workflow spec-first, migrations, commits). Résumé des choix : code/commentaires/commits **en anglais** (Conventional Commits), doc de conception en français ; migrations **goose** ; monorepo (UI Angular dans `web/`) ; outils épinglés via la directive `tool` de `go.mod` (`go tool sqlc|oapi-codegen|goose`).

## Sources de vérité

- `docs/PRD.md` — spécification produit. Les sections 1–14 décrivent le périmètre fonctionnel ; les sections 16+ sont les exigences vérifiables (mots normatifs DOIT / NE DOIT PAS / DEVRAIT / PEUT).
- `docs/adr/` — 26 ADRs acceptés. **Un ADR accepté est immuable** : toute révision de la DÉCISION passe par un nouvel ADR qui supersede l'ancien. (La formulation, elle, peut être corrigée en place — reformuler n'est pas décider.) Toute décision structurante exige un ADR + une entrée dans la grille de suivi (PRD §26).
- `docs/specs/openapi-v1.yaml` — contrat d'API. **Spec-first** : handlers Go et client TypeScript sont générés depuis ce fichier (oapi-codegen), jamais écrits à la main puis documentés après coup. La spec reste en **OpenAPI 3.0.3** (oapi-codegen ne supporte pas 3.1). Après toute modification : `make generate` et commiter le code généré (la CI vérifie la synchronisation).

## Stack imposée (ADR-025, ADR-021)

- PostgreSQL est la seule dépendance externe : état **et** queue de jobs (pas de Redis/NATS).
- pgx + sqlc (SQL explicite typé à la compilation, migrations versionnées), chi + oapi-codegen.
- Livrable : binaire Go statique, image distroless, modes all-in-one/api/worker.

## Conventions

- Documentation en **français** ; code, identifiants et messages de commit selon l'usage Go habituel.
- Variables prédéfinies préfixées `AKERDOCK_*` — jamais d'alias sous une autre marque (ADR-022). L'ancien nom du projet était « dockerbox » : ne pas réintroduire ce nom.
- Temps réel : SSE avec reprise `Last-Event-ID` ; WebSocket réservé au terminal (ADR-024).
- Tests : la majorité de la couverture est **unitaire** (toute logique nouvelle DOIT être testée en unitaire) ; l'E2E est réduit au minimum que seul le produit assemblé peut prouver — Docker-in-Docker uniquement (ADR-026), shard `smoke` à chaque commit, catalogue complet en nightly (plan de tests §2).
