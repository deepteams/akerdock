# ADR-023 — Migration entrante : par l'adoption générique, sans assistant d'import propriétaire

- **Statut** : Accepté
- **Date** : 2026-07-11
- **Sections PRD liées** : §27.23, §20.7, §27.13, §16.2

## Contexte

La première population susceptible d'arriver sur AkerDock exploite déjà des workloads sous une autre plateforme de déploiement. Un assistant d'import dédié lirait la **base interne** de cette plateforme — un schéma non documenté et mouvant, propre à son implémentation — pour recréer les objets côté AkerDock. Le PRD exclut déjà ce couplage de ses objectifs (§16.2). Par ailleurs, AkerDock dispose de l'**adoption générique de ressources** (§20.7, ADR-013), qui reprend des objets Docker standards. Il faut choisir le chemin de migration officiel.

## Décision

**Aucun assistant d'import propriétaire** : l'**adoption générique de ressources (§20.7) EST le chemin de migration**. AkerDock adopte les **containers, stacks compose, volumes et réseaux Docker standards** déjà présents sur le serveur — ce que produit *n'importe quelle* plateforme de containers — **sans rien connaître du schéma interne de celle qui les a créés**.

Cette décision est **réévaluable sur demande utilisateur avérée**.

## Alternatives considérées

- **Assistant d'import lisant la base interne d'une plateforme tierce** : rejeté — couplage à un schéma non documenté et mouvant, maintenance perpétuelle au rythme de ses releases, contraire au non-objectif §16.2 ; et l'essentiel (les workloads qui tournent) est déjà couvert par l'adoption.
- **Export/import via l'API d'une plateforme tierce** : rejeté comme livrable officiel — une API tierce ne couvre jamais tout (secrets, historique), le résultat exigerait quand même un redéploiement, et l'outil casserait à chaque évolution amont. Un outil communautaire reste possible au-dessus du config as code (ADR-012).
- **Aucun chemin de migration documenté** : rejeté — pouvoir reprendre l'existant sans interruption est un argument produit explicite (§27.13) ; il doit être documenté, simplement pas sous forme d'assistant lié à un tiers.

## Conséquences

- **Positives** : **zéro code spécifique à une plateforme tierce** à maintenir ; le chemin de migration bénéficie automatiquement de toute amélioration de l'adoption générique ; la migration se fait **sans interruption des workloads** (adoption sans redéploiement, prévisualisée et réversible).
- **Négatives** : ce qui vivait dans la base de la plateforme d'origine et **pas** dans les objets Docker — plans de backup, tâches planifiées, variables non injectées, membres et teams, historique — n'est **pas** migré automatiquement et doit être ressaisi.
- **Risques acceptés** : l'expérience est moins « clé en main » qu'un assistant dédié, ce qui peut freiner certains migrants — assumé, avec réévaluation explicitement prévue si une demande utilisateur avérée le justifie.
