# ADR-005 — Builds : BuildKit du serveur en P0/P1, builders rootless obligatoires pour le code non fiable

- **Statut** : Accepté
- **Date** : 2026-07-11
- **Sections PRD liées** : §27.5, §20.4(8), §23.1, §18.1, INV-010

## Contexte

Construire les images via le Docker/BuildKit du serveur cible, en accès direct au socket, est rapide et simple — mais un build exécute du code potentiellement hostile (Dockerfile, scripts de build) dans un voisinage privilégié. Le modèle de menace (§23.1) est explicite : les builders exécutent du code non fiable et doivent être isolés des credentials du control plane, du socket Docker global lorsque possible et du réseau interne sensible. Le cas critique est la preview d'une PR de fork approuvée (§20.4(8)). Il faut arbitrer entre simplicité immédiate et cible de sécurité.

## Décision

- **P0/P1** : les builds passent par le **BuildKit du Docker du serveur** (parité avec la référence).
- Des builders **BuildKit rootless dédiés deviennent obligatoires pour le code non fiable**, au plus tard avec la livraison des **previews de forks approuvés** (§20.4(8)) : builder isolé, aucun secret injecté.
- Le **contrat d'adaptateur build est écrit dès P0**, afin que la bascule vers des builders isolés ne touche pas le moteur de déploiement.

## Alternatives considérées

- **Rester définitivement sur le socket Docker du serveur** : rejeté — inacceptable dès qu'on exécute du code de contributeurs externes (fork), en contradiction avec §23.1 et INV-010.
- **Builders isolés (rootless/VM/microVM) dès P0** : rejeté — retarde la parité initiale pour un besoin qui n'apparaît qu'avec les previews de forks ; le contrat d'adaptateur écrit dès P0 préserve la trajectoire.
- **MicroVM (Firecracker/Kata) comme cible d'isolation** : non retenu à ce stade — isolation supérieure mais exigences d'infrastructure (KVM, images dédiées) incompatibles avec le VPS générique ; BuildKit rootless est le compromis retenu.

## Conséquences

- **Positives** : parité et time-to-market en P0/P1 ; trajectoire de sécurité explicite et datée (au plus tard les previews de forks) ; le moteur de déploiement est insensible au type de builder grâce à l'adaptateur.
- **Négatives** : en P0/P1, un build malveillant d'un dépôt auquel la team fait confiance a accès au Docker du serveur — le risque est borné au périmètre du serveur (§23.1) mais réel ; maintenir deux chemins de build (socket direct et rootless) augmente la matrice de test.
- **Risques acceptés** : fenêtre P0/P1 sans isolation forte des builds, assumée parce que seuls les dépôts configurés par la team y sont buildés (les forks restent ignorés par défaut, INV-010) ; BuildKit rootless a des limitations connues (certains cas de réseau/cache) qui devront être documentées.
