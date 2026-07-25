# ADR-034 — Métriques live à la demande via la connexion runtime

## Statut

Accepté — complète (ne supersede pas) [ADR-008](ADR-008-observabilite-otlp-partout.md).

## Contexte

ADR-008 décide que l'observabilité **historique** (métriques CPU/RAM serveur et
container, traces, logs) transite en **OTLP** vers un stockage de séries
temporelles externe : rien n'est modélisé dans PostgreSQL, et le protocole de
push propriétaire de l'agent est rejeté au profit d'OTLP standard.

Cette décision est bonne pour l'historique et l'analytique, mais elle laisse un
trou pour l'usage le plus courant du dashboard : **« ce service consomme
combien, maintenant ? »**. Répondre imposerait aujourd'hui à l'opérateur de
brancher un backend OTLP + une UI tierce (Grafana), là où il veut une jauge
immédiate à côté des logs et du shell qu'il a déjà sous la main (§13, §3.16 —
le composant `akd-metric-chart` était spécifié mais jamais livré, faute de
source de données côté control plane).

## Décision

Le control plane expose des **métriques live, à la demande, sans persistance** :

- La source est un **`docker stats --no-stream`** exécuté sur le serveur cible
  **via la connexion SSH runtime existante** (le même canal que `docker logs`,
  le terminal et le port-forward), résolu par le nommage de container
  déterministe `<uuid>-<service>` (INV-011).
- La lecture est **point-in-time** : un appel = un échantillon. Le dashboard
  rafraîchit en interrogeant périodiquement et construit une mini-tendance
  **côté client** ; aucun échantillon n'est écrit en base ni bufferisé au-delà
  d'une réponse HTTP.
- Endpoints read-only sous la ressource (`GET …/metrics`), permission
  `read` ; la métrique n'est jamais un secret.

L'historique, l'alerting et l'agrégation restent **hors périmètre** de cet ADR
et continuent de suivre ADR-008 (OTLP vers un backend externe).

## Conséquences

- **Positives** : jauges CPU/RAM par service immédiates, zéro dépendance externe,
  zéro table de métriques, zéro protocole de push à opérer — cohérent avec la
  philosophie « Docker standard, réversible » (§16.1) et avec les autres accès
  runtime déjà passés par SSH.
- **Négatives / limites** : pas d'historique ni de tendance longue (c'est le
  rôle d'ADR-008) ; chaque lecture ouvre/relaie un `docker stats` (coût borné,
  `--no-stream`, un seul appel pour tous les containers d'un stack) ; si le
  serveur est injoignable la réponse est un 409, comme `docker logs`.
- La permission `metrics:read` de la grille RBAC reste réservée à l'historique
  (backend externe) ; le live à la demande relève de `read` sur la ressource.

## Alternatives rejetées

- **Ressusciter le push Sentinel + rétention courte en base** : réintroduit le
  protocole propriétaire qu'ADR-008 rejette et ajoute une table de métriques —
  pour un simple affichage live, coût structurel disproportionné.
- **Interroger le backend OTLP externe (PromQL)** : lie le dashboard à un
  Prometheus/compatible que l'opérateur n'a pas forcément, et couple l'UI à un
  format de requête tiers. Reste la bonne voie pour l'**historique** (ADR-008),
  pas pour la jauge live.
