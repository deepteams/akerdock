# Spécification — Pyramide de tests et parcours E2E unique

> Artefact §29.9 du PRD. ADR-028 fixe le volume et la cadence ; ADR-026 fixe
> l'environnement Docker-in-Docker et le risque résiduel.

## 1. Objectif

La suite doit donner un retour rapide aux développeurs tout en conservant une
preuve que le produit assemblé fonctionne. Le principe directeur est :
**tester au niveau le plus bas capable de prouver la garantie**.

Cette stratégie vise explicitement les petites et moyennes équipes :

- coût CI borné et prévisible ;
- échec attribuable à un package ou un module ;
- exécution locale rapide sans infrastructure pour le cas courant ;
- une seule preuve d'assemblage, lisible et maintenable.

## 2. Pyramide

| Niveau | Ce qu'il prouve | Exemples | Cadence |
|---|---|---|---|
| Unitaire | Règle déterministe sans dépendance externe | validation, RBAC, parsing, interpolation, backoff, rendu de commande/configuration, états | local + chaque PR |
| Module | Contrat d'un module avec sa dépendance réelle, sans produit complet | queue/leases sur PostgreSQL, crypto, proxy golden files, protocoles Git | chaque PR |
| Produit E2E | Couture réelle impossible à simuler fidèlement | SSH → Docker → Traefik → trafic pendant une bascule | après fusion, manuel, release |
| Packaging | L'artefact installable, pas un parcours utilisateur | image distroless + compose de référence | après fusion, release |

Un test ne monte pas dans la pyramide pour « être plus réaliste ». Il monte
seulement si le niveau inférieur ne peut pas établir la propriété.

## 3. Parcours E2E unique

Commande : `make e2e`.

Topologie :

- un PostgreSQL réel ;
- le binaire AkerDock en mode complet ;
- un serveur cible Docker-in-Docker avec `sshd` ;
- un proxy Traefik créé par AkerDock ;
- une application nginx de référence.

Le parcours complet vérifie :

1. migrations, utilisateur root et clé d'instance au démarrage ;
2. enregistrement SSH et validation du serveur ;
3. proxy initialement arrêté, puis démarrage explicite, bootstrap et
   disponibilité de Traefik ;
4. création d'un projet et d'une application ;
5. injection d'une variable, déploiement et service HTTPS réel ;
6. logs de déploiement en JSON et SSE ;
7. modification du domaine et régénération immédiate du routage ;
8. redéploiement avec health check sous trafic continu, sans requête perdue ;
9. refus des appels anonymes et des tokens invalides ;
10. suppression sûre du container et de sa route.

Ce parcours est indivisible : il utilise une stack et produit un verdict. Il
n'existe ni shard, ni matrice nightly, ni second scénario E2E.

## 4. Couverture portée par les niveaux rapides

Les anciens scénarios E2E ne constituent plus une matrice de régression. Les
garanties récurrentes appartiennent aux suites suivantes :

| Garantie | Propriétaire rapide |
|---|---|
| Isolation et permissions de toutes les opérations | `internal/handlers/rbac_test.go` |
| Validation Git, S3, uptime, cron et heures calmes | `internal/handlers/validation_test.go` |
| Échappement des valeurs hostiles et absence de secrets dans argv | `internal/jobs/shellquote_test.go`, `internal/jobs/composedeploy_test.go` |
| Construction du déploiement et reprise déterministe | `internal/jobs/deploymentrun_test.go`, `internal/jobs/applicationdelete_test.go` |
| Compose, magic variables et routage preview | `internal/compose/*_test.go`, `internal/jobs/previewrouting_test.go` |
| Webhooks, forks et chemins surveillés | `internal/gitwebhook/*_test.go`, `internal/jobs/webhookprocess_test.go` |
| Queue, leases, concurrence et idempotence | `internal/queue/queue_test.go` contre PostgreSQL |
| Chiffrement, redaction et sessions | `internal/envelope`, `internal/audit`, `internal/session` |
| Proxy déterministe et wildcards | `internal/proxy/*_test.go`, `tests/proxy-conformance/` |
| Notifications et uptime | `internal/notify/*_test.go`, `internal/uptime/uptime_test.go` |
| Client, formulaires, WebAuthn, terminal et états UI | `web/src/**/*.spec.ts` |

Une ligne manquante est une dette de test module à traiter ; elle ne justifie
pas de restaurer un catalogue E2E.

## 5. Règles pour une contribution

Pour toute nouvelle logique ou correction :

1. écrire un test unitaire table-driven près du code propriétaire ;
2. ajouter un test module uniquement si la propriété dépend réellement de
   PostgreSQL, d'un protocole ou d'un format externe ;
3. ne modifier le parcours E2E que si SSH, Docker et le proxy réels sont tous
   nécessaires à la preuve ;
4. dans ce dernier cas, enrichir ou remplacer une assertion sans créer une
   nouvelle stack ni un nouveau scénario ;
5. garder les fixtures déterministes et les timeouts uniquement aux frontières
   externes.

Les tests de correctifs doivent échouer avant la correction. Les retries
aveugles sont interdits : un test flaky est isolé avec une issue et une échéance
de suppression.

## 6. CI

| Déclencheur | Tests |
|---|---|
| Développement local | package touché, puis `make test` |
| Pull request | Go unit/module, Angular unit, contrat généré, lint, OpenAPI, Storybook |
| Fusion sur `main` | mêmes tests + parcours E2E unique + packaging |
| `workflow_dispatch` | mêmes tests + parcours E2E unique + packaging |
| Tag `v*` | tests rapides + parcours E2E unique + packaging avant publication |

Il n'y a plus de cron nightly E2E.

## 7. Risques et validations manuelles

ADR-026 reste applicable : systemd, reboot réel, firewall/UFW, disque
physiquement plein et ARM64 ne sont pas reproduits par DinD. Ces classes sont
validées ponctuellement sur une machine réelle avant une évolution sensible du
transport, de l'onboarding ou de la distribution.

Les variations complètes de providers Git, moteurs de données et build packs
sont couvertes par leurs tests de protocole/module et par une validation
manuelle ciblée lors de leur modification. Ce compromis est assumé pour garder
une boucle de développement courte.
