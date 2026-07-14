# ADR-012 — Configuration as code : export YAML + apply idempotent + provider Terraform/OpenTofu officiel

- **Statut** : Accepté
- **Date** : 2026-07-11
- **Sections PRD liées** : §27.12, §24.5, §22.4, §12, §24.1

## Contexte

Une plateforme pilotée uniquement par l'UI n'offre aucune configuration déclarative native (§12). Pour des équipes qui versionnent leur infrastructure, l'absence d'export/apply reproductible est un frein d'adoption et un facteur de dérive entre environnements. Il faut décider si AkerDock reste UI/API-first ou offre un vrai contrat de configuration déclarative.

## Décision

Décision actée (exigences détaillées au §24.5) :

- **Export YAML complet** de toute la configuration non secrète d'une team (projets, environnements, ressources, domaines, variables non secrètes, plans de backup, tâches planifiées), dans un format **stable et versionnable en Git**, contrat versionné avec schéma publié, soumis à la même politique de compatibilité que l'API (§22.4).
- **Apply idempotent** : la soumission du YAML fait converger l'état — création, mise à jour, et suppression **uniquement sur demande explicite** ; mode **dry-run** produisant le diff complet ; conflits détectés par version optimiste (§24.1) ; apply audité et exécuté comme job visible.
- Les **secrets sont référencés** (nom + version), jamais inline ; leurs valeurs passent exclusivement par les endpoints dédiés.
- Un **provider Terraform/OpenTofu officiel** est construit sur l'API et couvre au minimum le périmètre P0/P1.

## Alternatives considérées

- **UI/API uniquement (parité)** : rejetée — reproduit une lacune connue de la référence ; la dérive de configuration entre environnements reste alors invisible et non réversible.
- **Terraform communautaire sans engagement officiel** : rejeté — qualité et couverture non garanties ; un provider officiel est un signal d'engagement et un produit testé avec l'API.
- **Format propriétaire riche (DSL type Pulumi/opérateur GitOps complet)** : rejeté — le YAML exporté + apply idempotent couvre le besoin sans imposer un runtime supplémentaire ; un flux GitOps peut être construit au-dessus par l'utilisateur.

## Conséquences

- **Positives** : configuration versionnable et revue en PR ; environnements reproductibles ; dry-run/diff avant application ; sortie du lock-in (avec l'export du §22.4) ; le provider Terraform officiel s'appuie sur l'API publique, garantissant que tout ce que fait l'UI est scriptable (§25.2).
- **Négatives** : le format YAML devient un contrat public de plus à faire évoluer avec compatibilité descendante ; la logique de convergence (diff, ordre d'application, suppressions explicites) est un moteur à part entière à concevoir et tester (round-trip export→apply exigé, §26.2) ; le provider Terraform est un livrable et un rythme de release supplémentaires.
- **Risques acceptés** : divergence possible entre schéma YAML, OpenAPI et modèle interne si la génération n'est pas outillée ; les suppressions restant explicites, un drift « ressources orphelines » peut subsister volontairement — c'est un choix de sécurité assumé.
