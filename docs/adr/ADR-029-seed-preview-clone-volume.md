# ADR-029 — Seed des previews de PR par clone de volume

- **Statut** : Accepté
- **Date** : 2026-07-24
- **Sections PRD liées** : §20.4 (previews), §5.2 (compose), INV-010
- **Révise** : rien — étend le contrat des previews compose (§20.4.4)

## Contexte

Une preview de PR démarre avec des volumes vides : c'est la conséquence directe
d'INV-010 (une preview ne monte jamais les données de production) et c'est le
bon défaut. Mais une stack dont le cœur est une base de données devient alors
difficile à reviewer : l'instance de PR fonctionne, techniquement, sur une base
sans données — l'évaluation fonctionnelle réelle exige un jeu de données.

Les jobs one-shot (`restart: no`, compose-spec §7.3) couvrent déjà le seed par
fixtures versionnées. Ils ne couvrent pas le besoin « données proches de la
production, prêtes à l'emploi, sans maintenir de fixtures ».

## Décision

Un volume nommé d'une stack compose PEUT déclarer, au niveau de sa déclaration
top-level :

```yaml
volumes:
  pgdata:
    x-akerdock:
      preview_seed: clone
```

Sémantique, pour un déploiement de **preview** uniquement :

1. Le volume de la preview (`<uuid-preview>_<nom>`), s'il est **encore vide**,
   est initialisé par **copie** du volume de production correspondant
   (`<uuid-app>_<nom>`), juste avant le premier démarrage du service qui le
   monte — après le build/pull, donc avec l'image du service disponible.
2. La copie s'exécute dans un conteneur éphémère de l'image du service
   (`--user 0`, `cp -a`), la source montée en **lecture seule** : la
   production n'est jamais mutée, les propriétaires/permissions sont
   préservés. L'image du service DOIT fournir `/bin/sh` (vrai pour toutes les
   images de bases de données courantes) ; un échec de copie fait échouer le
   déploiement de la preview — un seed silencieusement vide contredirait
   l'intention déclarée.
3. Un volume non vide n'est **jamais** retouché : les redéploiements d'une
   preview existante conservent ses données.
4. Si le volume de production n'existe pas encore (stack jamais déployée), le
   seed est sauté — il n'y a rien à copier, et le volume de production ne doit
   pas être créé par effet de bord.
5. `preview_seed` est refusé à la validation sur un volume `external:` (les
   objets adoptés sont la production même) et en mode raw compose (les noms n'y
   sont pas préfixés : production et preview désigneraient le même volume).

## Rapport à INV-010

INV-010 interdit le **montage** des données de production dans une preview et
tout accès « sans politique explicite ». `preview_seed: clone` est précisément
cette politique explicite : un opt-in par volume, versionné dans le compose du
dépôt, qui produit une **copie jetable** détruite avec la preview
(`previewdestroy`). Les protections de previews restent entières : basic auth
par défaut, PR de forks soumises à approbation. L'opérateur qui active le clone
accepte que les données du volume soient visibles derrière ces protections —
c'est le compromis assumé de la décision.

## Limite documentée : cohérence de la copie

La copie fichier-à-fichier d'une base de données **en cours d'écriture** est
équivalente à un instantané après crash : PostgreSQL et consorts la rejouent
via leur journal dans l'immense majorité des cas, sans garantie absolue. C'est
un compromis accepté pour un usage de review ; pour une garantie de cohérence,
les alternatives restent le job one-shot de fixtures ou un dump logique
(alternative rejetée ci-dessous, réévaluable).

## Alternatives considérées

- **Dump logique (pg_dump → restore)** : cohérent par construction, mais
  spécifique à chaque moteur (credentials, outillage, temps de restauration) —
  rejeté comme mécanisme de base ; le clone est générique. Réévaluable comme
  mode supplémentaire (`preview_seed: dump`) si le besoin se confirme.
- **Snapshot du système de fichiers (LVM/ZFS)** : cohérence excellente, mais
  impose une hypothèse d'infrastructure qu'AkerDock ne fait nulle part
  ailleurs — rejeté.
- **Fixtures uniquement** : déjà couvert par les one-shots ; ne répond pas au
  besoin « données de production sans maintenance de fixtures ».

## Conséquences

- Extension du parseur compose (validation stricte, findings dédiés) et du
  moteur de déploiement compose (script de seed par service, avant le premier
  démarrage, previews uniquement).
- Le périmètre est **compose uniquement** : les storages des applications
  single-container pourront recevoir un drapeau équivalent plus tard, dans un
  amendement de cette décision.
- Grille de suivi §26.2 : la capacité « Previews PR enrichies » intègre le
  seed par clone de volume.
