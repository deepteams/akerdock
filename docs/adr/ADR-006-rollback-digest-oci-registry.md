# ADR-006 — Rollback : digests OCI en registry si configuré, rétention locale protégée sinon

- **Statut** : Accepté
- **Date** : 2026-07-11
- **Sections PRD liées** : §27.6, §5.5, §15, §18.3, INV-015

## Contexte

Un rollback limité aux images encore présentes localement sur le serveur n'en est pas un : si le cleanup disque a purgé l'image, il est impossible — et un tag mutable ne garantit pas de revenir au binaire exact (§15). La source de vérité du PRD (§18.3) exige déjà d'identifier l'image déployée par **digest OCI**, pas seulement par tag. Il faut décider comment garantir un rollback reproductible sans imposer un registry à toutes les installations.

## Décision

- **Si un registry est configuré** : chaque déploiement est **pushé et référencé par digest OCI**. Le rollback est reproductible vers **toute version conservée** dans le registry, indépendamment de l'état du disque du serveur.
- **Sans registry** : **rétention locale des N dernières images**, ces images étant **explicitement protégées du cleanup automatique** (INV-015 — le cleanup ne détruit jamais un objet persistant requis).

## Alternatives considérées

- **Rollback local uniquement (parité stricte)** : rejeté — le rollback devient aléatoire (dépend du passage du cleanup) et non reproductible, limitation connue de la référence (§15).
- **Registry obligatoire pour tous** : rejeté — alourdit l'installation minimale (un VPS, pas d'infrastructure annexe) et contredit la simplicité d'exploitation ; le registry reste un choix.
- **Re-build du commit précédent en cas de rollback** : rejeté comme mécanisme principal — un rebuild n'est pas reproductible bit à bit (dépendances, base images mutables) et est lent au moment précis où l'on veut revenir en arrière vite.

## Conséquences

- **Positives** : rollback déterministe par digest quand un registry existe ; sans registry, fenêtre de rollback garantie (N images protégées) au lieu du comportement aléatoire de la référence ; cohérent avec §18.3 (résolution du digest avant bascule).
- **Négatives** : le push systématique vers le registry allonge chaque déploiement et consomme du stockage registry (rétention à gérer côté registry) ; sans registry, les N images conservées consomment du disque serveur et la profondeur de rollback est bornée.
- **Risques acceptés** : deux chemins de rollback à tester (registry et local) ; la valeur de N et l'interaction précise avec le cleanup automatisé (§3.7) devront être spécifiées dans le moteur de déploiement (§29.4).
