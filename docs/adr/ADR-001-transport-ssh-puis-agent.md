# ADR-001 — Transport de contrôle : SSH d'abord, agent sortant en cible, ports configurables

- **Statut** : Accepté
- **Date** : 2026-07-11
- **Sections PRD liées** : §27.1, §3.1, §4.1, §10.4, §16.1(2)(6), §18.1, §25.2

## Contexte

Piloter les serveurs cibles en SSH (push depuis l'instance) est le modèle le plus simple à exploiter : rien à installer sur la cible, rien à versionner. Mais il impose un port entrant ouvert sur chaque serveur et ne permet pas de recevoir les événements Docker en continu (l'état est *interrogé*, jamais *poussé*). Un agent sortant (pull) réduirait la surface entrante et ouvrirait la voie aux événements temps réel, au prix du versionnement, de l'enrollment et de l'upgrade d'un agent. Il faut trancher le modèle de transport et, en corollaire, la surface réseau exposée par la plateforme.

## Décision

Orientation validée en deux temps :

1. **SSH pour la parité initiale** : le transport distant est une interface abstraite dont l'implémentation initiale est SSH (§18.1). Tout serveur Linux joignable en SSH peut être piloté, comme dans la référence.
2. **Agent sortant comme cible** : un agent sortant optionnel est la direction à terme pour réduire la surface entrante des serveurs cibles et permettre la remontée d'événements Docker. Il pourra être ajouté sans modifier les services métier, grâce à l'abstraction du transport.

Exigences associées sur les ports :

- Le proxy de chaque serveur écoute sur **80/443 par défaut**, mais ses ports d'écoute **DOIVENT être configurables par serveur** (par exemple 8080/8443 lorsqu'un reverse proxy amont détient déjà 80/443).
- Le control plane est exposé sur **un seul port**, derrière son propre domaine/DNS : UI, API et flux temps réel le partagent (§25.2, cf. ADR-024) — un port, un certificat, une règle de firewall.

## Alternatives considérées

- **Agent pull dès le départ** : rejeté pour la première version car il introduit d'emblée versionnement, enrollment et upgrade d'agent, et retarde la parité fonctionnelle avec la référence.
- **SSH définitif sans trajectoire agent** : rejeté car il fige des ports entrants ouverts sur chaque serveur cible et interdit les événements Docker temps réel, alors que la réduction de surface est un objectif produit.
- **Un port par usage (dashboard, WebSocket, terminal)** : rejeté — surface réseau inutilement large et exploitation plus complexe (trois règles de firewall, trois certificats), contraire à l'objectif « un seul port exposé » (§16.1(6)).

## Conséquences

- **Positives** : parité immédiate avec la référence (tout serveur SSH est éligible) ; surface d'exposition du control plane réduite à un port ; cohabitation possible avec un reverse proxy amont grâce aux ports proxy configurables ; la bascule future vers un agent ne touche pas les services métier.
- **Négatives** : le modèle SSH impose de conserver des ports SSH entrants ouverts sur les serveurs cibles tant que l'agent n'existe pas ; pas d'événements Docker en push au début (polling nécessaire pour l'état observé, §18.3).
- **Risques acceptés** : maintenir deux transports à terme (SSH + agent) augmentera la matrice de test ; l'abstraction du transport doit être conçue dès P0 pour éviter que des détails SSH ne fuient dans les services métier.
