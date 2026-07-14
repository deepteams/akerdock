# ADR-018 — Déploiement depuis le poste : `akerdock up` avec contexte local

- **Statut** : Accepté
- **Date** : 2026-07-11
- **Sections PRD liées** : §27.18, §12, §5.1, §26.2

## Contexte

Exiger un dépôt Git accessible (public, GitHub App ou deploy key) ou une image déjà publiée pour déployer quoi que ce soit (§5.1) interdit de prototyper depuis un répertoire local avant d'avoir créé et poussé un dépôt. Les CLI du domaine (heroku, fly, etc.) ont montré la valeur d'un « push du dossier courant ». Il faut décider si la CLI AkerDock offre ce chemin et avec quelles garanties de traçabilité.

## Décision

La CLI **PEUT pousser un contexte local** (`akerdock up`) : détection du build pack, création de l'application si besoin, build et déploiement — destiné au **prototypage avant branchement d'un provider Git**.

Garde-fous de traçabilité :

- Un déploiement de source locale est **marqué comme tel dans l'historique** : pas de SHA Git, un **digest du contexte** le remplace.
- Un tel déploiement **n'active jamais d'auto-deploy** : aucun webhook ni déclenchement automatique ne peut en découler.

## Alternatives considérées

- **Parité stricte (Git ou image uniquement)** : rejetée — friction inutile au premier contact avec le produit, alors que la CLI existe déjà pour le reste (§12).
- **Faire du push local un mode de production à part entière** (watch, sync continue) : rejeté — encouragerait des déploiements non traçables vers la production ; le positionnement est explicitement le prototypage, le chemin nominal reste Git.
- **Exiger un commit Git local et pousser le SHA** : rejeté — impose un dépôt et un état commité pour un simple essai ; le digest du contexte donne une traçabilité suffisante sans cette contrainte.

## Conséquences

- **Positives** : time-to-first-deploy minimal (un dossier, une commande) ; parcours d'évaluation du produit sans configuration Git ; traçabilité préservée (digest de contexte, marquage explicite dans l'historique).
- **Négatives** : l'upload du contexte de build (taille, exclusions type .dockerignore, streaming vers le serveur de build) est un canal d'ingestion supplémentaire à sécuriser et à limiter ; l'historique doit gérer des déploiements sans référence Git (diff de configuration sans diff de code).
- **Risques acceptés** : un déploiement local n'est pas reproductible depuis une source de vérité externe — assumé et signalé par le marquage ; risque d'usage en production malgré le positionnement prototypage, atténué par l'absence d'auto-deploy et la visibilité du marquage dans l'historique.
