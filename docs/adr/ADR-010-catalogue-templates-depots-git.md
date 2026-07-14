# ADR-010 — Catalogue one-click : dépôt de templates dédié signé + dépôts de templates utilisateur

- **Statut** : Accepté
- **Date** : 2026-07-11
- **Sections PRD liées** : §27.10, §9, §27.22, §29.11

## Contexte

Un catalogue de services one-click est un ensemble de fichiers compose annotés de métadonnées (§9). Le livrer compilé dans le binaire le couple aux releases, et centralise son admission : une team ne peut pas maintenir ses propres templates internes. Il faut décider de la provenance, du cycle de vie et de la confiance accordée aux templates, en respectant les licences des templates importés.

## Décision

Le catalogue repose sur deux sources :

1. **Un dépôt de templates dédié maintenu par le projet** : versionné, validé, **signé**, et **rafraîchissable indépendamment du binaire**. Les templates compatibles de l'écosystème y sont importés sous respect des licences (et réécrits en variables `AKERDOCK_*`, cf. ADR-022 / §27.22).
2. **Des dépôts de templates utilisateur** : chaque team peut enregistrer **un ou plusieurs repositories Git** (publics ou privés, via les clés/credentials existants) contenant ses propres templates, avec **validation à l'import** et **resynchronisation à la demande**.

## Alternatives considérées

- **Catalogue embarqué dans le binaire (parité)** : rejeté — couple la fraîcheur du catalogue aux releases de la plateforme et interdit les catalogues privés d'entreprise.
- **Import direct et non validé de n'importe quelle URL compose** : rejeté — un template est exécuté avec les privilèges du serveur cible ; sans validation ni signature, c'est un vecteur d'attaque évident (reste possible via le build pack compose ordinaire, hors catalogue).
- **Marketplace centralisée avec soumissions tierces** : rejetée — coût de modération et d'infrastructure disproportionné ; les dépôts Git utilisateur couvrent le besoin de personnalisation.

## Conséquences

- **Positives** : catalogue officiel mis à jour sans release du binaire ; chaîne d'intégrité (signature) sur ce que la plateforme propose d'exécuter ; les teams peuvent maintenir des templates internes privés avec leurs credentials Git existants ; inventaire licences facilité (§29.11).
- **Négatives** : infrastructure de signature et de validation à construire et opérer (clé de signature du projet, vérification côté instance) ; un pipeline de validation de templates (lint compose, magic variables, métadonnées) devient un composant à part entière.
- **Risques acceptés** : les templates des dépôts utilisateur sont validés syntaxiquement mais restent sous la responsabilité de la team qui les enregistre (pas de signature projet) ; la réécriture des variables des templates importés d'un écosystème tiers est un coût d'entretien récurrent, assumé (cf. ADR-022).
