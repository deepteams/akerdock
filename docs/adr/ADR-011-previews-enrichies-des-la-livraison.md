# ADR-011 — Previews de PR enrichies dès la livraison, contrôles de déclenchement opt-in

- **Statut** : Accepté
- **Date** : 2026-07-11
- **Sections PRD liées** : §27.11, §5.6, §20.4, §26.2, INV-010

## Contexte

Un preview deployment minimal — un container et une URL publique par PR, sans plafond, sans TTL, sans protection d'accès, sans support compose, avec un commentaire par déploiement — est en dessous du standard du domaine (§5.6, §15). Les plateformes de preview dédiées ont établi une barre nettement plus haute : environnements compose complets, données éphémères, TTL, accès protégé, checks Git. Il faut décider du niveau visé, et à quel moment il est livré.

## Décision

La feature preview est **livrée d'emblée enrichie** : tout le périmètre du §20.4 est **prioritaire** et livré avec la feature, pas en extension ultérieure. Concrètement :

- **compose éphémère** : stack complet par PR, réseau isolé, volumes propres, magic variables par instance, destruction intégrale au cleanup ;
- **données éphémères** : bases provisionnées par seed ou clone de snapshot, jamais partagées implicitement avec la production ou une autre preview ;
- **TTL, plafonds et scale-to-zero** : plafond de previews simultanées par application et par serveur, TTL d'inactivité, resource limits distincts, pool de serveurs de preview optionnel, scale-to-zero souhaité au niveau proxy ;
- **protection d'accès par défaut** : basic auth ou lien signé + `X-Robots-Tag: noindex`, exposition publique sur choix explicite ;
- **watch paths en preview** (monorepo) ;
- **checks Git riches** : commit statuses/checks, API Deployments GitHub, commentaire unique mis à jour en place, parité de feedback GitLab/Gitea ;
- **forks sur approbation** : preview possible après approbation d'un mainteneur, builder isolé, aucun secret injecté (INV-010).

Les **contrôles de déclenchement** (opt-in par label de PR, commandes en commentaire `/deploy` `/destroy`, exclusion des draft PRs, annulation des builds obsolètes) sont des **options activables par application, désactivées par défaut** — le comportement de parité reste le défaut.

## Alternatives considérées

- **Parité minimale d'abord, enrichissement plus tard** : rejetée — le PRD acte que le périmètre §20.4 fait partie de la feature elle-même ; livrer le minimum créerait des previews publiques non protégées et sans plafond, défauts connus de la référence.
- **Contrôles de déclenchement activés par défaut** : rejetés — surprendrait les utilisateurs venant de la référence ; le défaut reste le comportement de parité, chaque contrôle est opt-in individuellement.
- **Déléguer les previews à un outil externe** : rejeté — les previews sont un différenciateur produit central et exigent l'intégration au proxy, aux secrets et au cycle de vie interne.

## Conséquences

- **Positives** : différenciateur produit majeur face à la référence ; sécurité par défaut (accès protégé, secrets de production jamais copiés, forks ignorés sans approbation) ; coûts maîtrisés (TTL, plafonds, scale-to-zero) là où la référence n'a aucun cap.
- **Négatives** : périmètre de première livraison nettement plus gros que la parité — compose éphémère, cycle de vie TTL, intégrations checks multi-providers et builders isolés (dépendance sur ADR-005) doivent tous exister pour déclarer la feature complète (preuves exigées §26.2).
- **Risques acceptés** : la richesse des intégrations Git (checks, deployments, commentaire unique) multiplie les surfaces provider-spécifiques à maintenir (GitHub/GitLab/Gitea) ; le scale-to-zero au proxy est un DEVRAIT, susceptible d'arriver après le reste du périmètre.
