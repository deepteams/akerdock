# ADR-009 — Proxy : représentation intermédiaire commune, Traefik seul en P0, Caddy en P2

- **Statut** : Accepté
- **Date** : 2026-07-11
- **Sections PRD liées** : §27.9, §4.1, §18.1, §26.1, §29.6

## Contexte

Supporter plusieurs proxies (Traefik, Caddy) en générant directement leurs labels de routage sur les containers fait de chaque proxy un chemin de code distinct : difficile à tester de façon équivalente, et les comportements divergent silencieusement. Il faut décider comment supporter plusieurs proxies sans dupliquer la logique de routage.

## Décision

Décision validée :

- Une **représentation intermédiaire commune** du routage (domaines, paths, ports, middlewares, certificats) est la source unique ; la génération Traefik ou Caddy en dérive de façon déterministe (contrat proxy §18.1 : génération, validation, application atomique, rollback).
- Les **labels de routage sur les containers** restent supportés (compatibilité avec les usages courants de l'écosystème).
- Des **fixtures de conformité partagées** Traefik/Caddy garantissent un comportement identique des deux backends (§29.6).

**Séquencement** : **Traefik seul en P0** ; **Caddy arrive en P2** via la représentation intermédiaire, **dont les fixtures existent dès P0**.

## Alternatives considérées

- **Génération directe de labels par proxy (parité stricte)** : rejetée — logique dupliquée, divergences de comportement non testables, ajout d'un troisième proxy prohibitif.
- **Un seul proxy imposé (Traefik définitif)** : rejeté — Caddy est attendu pour la parité (P2) et l'abstraction protège aussi des évolutions de Traefik lui-même.
- **Écrire l'abstraction plus tard, au moment d'ajouter Caddy** : rejeté — rétrofit coûteux ; les fixtures de conformité créées dès P0 rendent l'ajout de Caddy incrémental.

## Conséquences

- **Positives** : un seul modèle de routage à valider ; les deux proxies sont testés sur les mêmes fixtures, donc réellement interchangeables par serveur ; reload atomique et rollback définis une fois pour toutes.
- **Négatives** : la représentation intermédiaire doit couvrir l'union des capacités utiles (basic auth, rate limiting, IP whitelisting, headers, priorités de path, certificats custom — §4.1), un travail de spécification supplémentaire en P0 (§29.6) alors qu'un seul proxy est livré.
- **Risques acceptés** : certaines capacités spécifiques à un proxy ne rentreront pas proprement dans l'abstraction et devront être soit exclues, soit exposées comme extensions explicites ; Caddy n'étant livré qu'en P2, les fixtures écrites en P0 ne seront réellement éprouvées contre lui que tardivement.
