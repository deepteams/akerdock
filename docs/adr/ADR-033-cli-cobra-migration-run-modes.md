# ADR-033 — Passage à Cobra : modes serveur en sous-commandes

- **Statut** : Accepté
- **Date** : 2026-07-25
- **Sections PRD liées** : §12 (CLI officielle), §18.2 (modes de run), §14.1 (installation)
- **Lié** : ADR-021 (distribution compose, binaire unique), ADR-031/ADR-032 (commandes client du CLI)

## Contexte

Le binaire `akerdock` parse aujourd'hui ses arguments à la main : le premier argument
positionnel choisit le mode serveur (`all-in-one`, `api`, `worker`, `scheduler`) ou la
sous-commande `healthcheck`. Le CLI local (ADR-031/032) ajoute une famille de commandes
client (`login`, `logout`, `context`, `ls`, `logs`, `shell`, `port-forward`, `db`) avec
flags, sous-commandes imbriquées, aide générée et complétion — que le parsing manuel ne peut
pas porter proprement. `github.com/spf13/cobra` est déjà présent dans l'arbre de dépendances
(en `// indirect`).

## Décision

Le binaire adopte **Cobra pour tout l'arbre de commandes**, dans le binaire unique (ADR-021).
Les modes serveur deviennent des sous-commandes explicites :

- `akerdock serve all-in-one|api|worker|scheduler` — anciens arguments positionnels de mode.
- `akerdock healthcheck` — inchangé (sonde de la healthcheck compose distroless).
- `akerdock version` — inchangé.
- Les commandes client d'ADR-031/032 sont des sous-commandes de premier niveau.

`AKERDOCK_MODE` reste lu comme défaut de `serve` (parité avec l'existant).

### Repli de compatibilité (le temps d'une version majeure)

Un `akerdock all-in-one` (argument positionnel historique, sans `serve`) **DOIT** rester
reconnu, exécuter le mode correspondant, et émettre un **avertissement de dépréciation** sur
stderr pointant vers `serve`. Cela évite de casser les instances existantes au premier
`git pull && ./install.sh` avant que le compose ne soit mis à jour.

### Migration des artefacts de lancement (dans le même changement)

- `docker-compose.yml` : `command: ["serve", "all-in-one"]`.
- `Dockerfile` : la healthcheck distroless reste `["/akerdock", "healthcheck"]`.
- `install.sh`, runbooks (`docs/runbooks/*`) et ADR-021 : commandes de lancement mises à
  jour vers `serve …`.

## Alternatives considérées

- **Cohabitation (Cobra pour le client, modes serveur en positionnel)** : rejeté — deux
  styles de parsing dans un même binaire, incohérent et source de confusion à long terme.
  L'utilisateur a explicitement tranché pour le full-Cobra.
- **Binaire client séparé (`akerdockctl`)** : rejeté — un artefact et un cycle de release de
  plus, contraire au principe « un binaire » d'ADR-021.
- **Rupture sèche sans repli** : rejeté — casserait toute instance déployée dont le compose
  n'a pas encore la nouvelle commande.

## Conséquences

- **Positives** : arbre de commandes cohérent, aide/complétion générées, base saine pour les
  commandes client et les extensions v2 (`up`, `env`, `domains`…) ; Cobra passe simplement
  de dépendance indirecte à directe.
- **Négatives** : migration de rupture à coordonner sur compose/Dockerfile/install.sh/
  runbooks ; un repli déprécié à porter puis retirer à la version majeure suivante.
- **Risques acceptés** : fenêtre transitoire où l'ancienne et la nouvelle invocation
  coexistent — bornée par le retrait annoncé du repli.
