# ADR-016 — Déploiement coordonné d'un environnement : graphe, hooks de migration, rollback opt-in

- **Statut** : Accepté
- **Date** : 2026-07-11
- **Sections PRD liées** : §27.16, §20.8, §21.1, §26.2

## Contexte

Déployer ressource par ressource, sans notion d'ordre, de dépendances ni de hooks, laisse la coordination à l'opérateur : une application et sa migration de schéma, ou un frontend dépendant d'une API, doivent être lancés à la main dans le bon ordre. Pour des environnements multi-ressources réels, cette absence produit des bascules partielles et des fenêtres d'incohérence. Il faut décider si l'environnement devient une unité déployable.

## Décision

Un environnement peut être déployé **comme une unité** (workflow détaillé au §20.8) :

- **Graphe de dépendances** explicite entre ressources, ordre topologique, parallélisme au sein d'un même niveau.
- **Hooks de migration** : job one-shot exécuté après build et avant bascule (ex. migration de schéma) ; l'échec du hook empêche toute bascule dans l'environnement.
- **Mode atomique par niveau** (optionnel) : la bascule de trafic attend que toutes les ressources du niveau soient saines.
- **Rollback automatique sur santé dégradée** (politique **opt-in par application**) : après bascule, fenêtre d'observation (bake time) sur les health checks ; en cas de dégradation, rollback vers l'artifact précédent vérifié, notifié et audité.
- **Échec partiel explicite** : état de l'environnement détaillé (ressources déployées / non déployées / en échec), reprise possible au point d'échec — jamais de demi-bascule silencieuse.

## Alternatives considérées

- **Parité stricte (déploiements indépendants uniquement)** : rejetée — laisse la coordination aux scripts des utilisateurs, précisément ce qu'un PaaS doit absorber ; les déploiements unitaires restent bien sûr disponibles.
- **Pipelines CI externes comme réponse** : rejeté — un pipeline externe n'a ni la vision des health checks ni la main sur la bascule proxy ; il ne peut pas garantir « migration avant bascule ».
- **Rollback automatique activé par défaut** : rejeté — un rollback automatique non désiré peut aggraver un incident (données déjà migrées) ; il reste opt-in par application.

## Conséquences

- **Positives** : déploiements multi-ressources reproductibles et ordonnés ; les migrations de schéma trouvent enfin une place canonique (avant bascule) ; le rollback sur bake time apporte une sécurité type « progressive delivery » sans orchestrateur.
- **Négatives** : le moteur de déploiement doit gérer un graphe (cycles à détecter, niveaux, parallélisme) et des états d'environnement composites en plus des états unitaires (§21.1) ; l'UI doit représenter une timeline multi-ressources ; preuve E2E exigeante (§26.2 : graphe + hook migration + rollback sur health).
- **Risques acceptés** : le mode atomique par niveau retient des ressources saines en attente des autres — latence de bascule assumée quand l'option est choisie ; un rollback automatique ne revient pas sur les effets de bord d'une migration déjà exécutée (la compatibilité descendante des migrations reste de la responsabilité de l'utilisateur).
