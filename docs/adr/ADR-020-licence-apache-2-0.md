# ADR-020 — Licence du projet : Apache 2.0

- **Statut** : Accepté
- **Date** : 2026-07-11
- **Sections PRD liées** : §27.20, §1, §29.11

## Contexte

AkerDock doit choisir sa licence open source. Le segment du PaaS self-hosted s'est construit sur des licences permissives, et l'absence de feature paywallée est une promesse produit explicite (§1). Le paysage récent du secteur montre deux voies : licences permissives (adoption maximale) ou licences restrictives type BSL/AGPL (protection contre un « fork cloud » par un hyperscaler). Il faut trancher avant toute publication de code.

## Décision

**Apache 2.0** — la même licence que la référence :

- adoption et contributions maximales (aucune friction juridique pour les entreprises) ;
- **clause brevets incluse** (protection explicite des contributeurs et utilisateurs, avantage sur MIT) ;
- le **fossé concurrentiel est le produit, pas la licence** ; le risque « fork cloud par un tiers » est **accepté**.

## Alternatives considérées

- **AGPL v3** : rejetée — friction d'adoption forte en entreprise (politiques interdisant l'AGPL), à rebours de l'objectif d'adoption maximale.
- **BSL / licences « source available » (SSPL, FSL…)** : rejetées — protègent d'un fork cloud mais excluent le projet de la définition open source, compliquent packaging et contributions, et enverraient un signal opposé à celui de la référence.
- **MIT** : écartée au profit d'Apache 2.0 — quasi équivalente en permissivité mais sans clause brevets explicite.

## Conséquences

- **Positives** : compatibilité maximale avec l'écosystème (dépendances, distributions, entreprises) ; licence permissive alignée sur celle des templates compose du domaine, ce qui simplifie leur import dans le respect des licences (§27.10, inventaire §29.11) ; protection brevets pour contributeurs et utilisateurs.
- **Négatives** : aucune protection juridique contre un acteur qui hébergerait AkerDock en service managé concurrent sans contribuer.
- **Risques acceptés** : le « fork cloud par un tiers » est explicitement accepté — la défense est le rythme produit, la communauté et la marque, pas la licence ; ce pari est réversible pour le code futur mais pas rétroactivement pour le code déjà publié sous Apache 2.0.
