# ADR-024 — Transport temps réel : SSE pour logs/statuts/jobs, WebSocket réservé au terminal

- **Statut** : Accepté
- **Date** : 2026-07-11
- **Sections PRD liées** : §27.24, §24.4, §27.1, §10.4, §22.2

## Contexte

Utiliser des WebSockets pour tout le temps réel, sur des ports dédiés (§10.4), complique l'exposition derrière proxies et firewalls d'entreprise. Or les besoins temps réel d'AkerDock sont presque tous **unidirectionnels** (logs de build/runtime, statuts, progression de jobs) ; seul le terminal est réellement bidirectionnel. Il faut choisir le transport et son intégration au port unique du control plane (ADR-001).

## Décision

- **SSE (Server-Sent Events)** pour les logs, les statuts et la progression de jobs : **reconnexion native**, **reprise par curseur `Last-Event-ID`**, compatible avec les proxies d'entreprise.
- **WebSocket réservé au terminal** — le seul flux bidirectionnel (PTY interactif).
- **Tout passe par le port unique du control plane** (§27.1) : aucun port dédié au temps réel.

Les exigences du §24.4 s'appliquent : flux protégés par la même policy que l'endpoint REST équivalent, token realtime court/mono-usage ou borné à la ressource, terminal avec heartbeat, idle timeout, durée maximum et kill garanti, ouverture/fermeture auditées.

## Alternatives considérées

- **WebSocket pour tout (parité)** : rejeté — bidirectionnalité inutile pour des flux de logs, reconnexion et reprise à réimplémenter à la main, et passages proxy/firewall d'entreprise plus fragiles.
- **Polling long/short pour les statuts** : rejeté — latence et charge inutiles à l'échelle visée (500 flux realtime concurrents, §22.2) ; SSE offre le push avec la simplicité HTTP.
- **gRPC streaming** : rejeté — non consommable nativement par un navigateur sans passerelle, dépendance de toolchain supplémentaire pour un besoin couvert par HTTP standard.

## Conséquences

- **Positives** : reprise des logs sans perte via `Last-Event-ID` (aligné sur le backpressure à curseur du §22.2) ; un seul port et une seule pile d'auth pour REST et temps réel ; SSE traverse les intermédiaires HTTP standards ; le terminal conserve le transport adapté à son besoin.
- **Négatives** : deux transports à maintenir quand même (SSE + WebSocket terminal) ; SSE est unidirectionnel — toute future interaction (annulation fine, entrée utilisateur) passera par des requêtes REST séparées ; les connexions SSE longues exigent une gestion attentive des buffers et des proxies intermédiaires (keep-alive, timeouts).
- **Risques acceptés** : limite historique de connexions simultanées SSE en HTTP/1.1 par domaine côté navigateur — atténuée par HTTP/2 (multiplexage), qui devient de fait la cible de déploiement recommandée ; le port unique concentre tout le trafic control plane, dimensionnement à surveiller (§22.2).
