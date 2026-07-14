# ADR-017 — Uptime monitoring intégré : checks HTTP/TCP simples, sans APM

- **Statut** : Accepté
- **Date** : 2026-07-11
- **Sections PRD liées** : §27.17, §11, §13, §26.2

## Contexte

Surveiller l'état des containers et la joignabilité des serveurs ne dit pas si l'application **répond depuis Internet** (§13). Sans check externe, l'utilisateur doit installer, configurer et maintenir un second outil pour le savoir. Il faut décider si AkerDock intègre cette capacité et jusqu'où.

## Décision

Des **checks HTTP/TCP simples intégrés** : cible, intervalle, seuils d'échec, **exécutés hors du workload surveillé** — avec **alerting via les canaux de notification existants** (§11) et **historique de disponibilité par ressource**.

**Pas d'APM** : le périmètre s'arrête au **up/down et à la latence**. Tout ce qui relève du profiling, des transactions ou des erreurs applicatives reste hors périmètre.

## Alternatives considérées

- **Parité stricte (déléguer à Uptime Kuma)** : rejetée — casse l'expérience intégrée (deuxième outil, deuxième configuration d'alerting) pour une capacité simple à fournir ; Uptime Kuma reste disponible dans le catalogue pour les besoins avancés.
- **APM/monitoring applicatif complet** : rejeté — périmètre démesuré, marché déjà servi par des acteurs spécialisés, et contraire à l'empreinte légère du produit.
- **Checks exécutés depuis le serveur qui héberge le workload** : rejetés — un serveur en difficulté rapporterait faussement ses propres workloads sains ou ne rapporterait rien ; les checks s'exécutent hors du workload surveillé.

## Conséquences

- **Positives** : réponse intégrée à la question « mon app répond-elle ? » sans outil tiers ; réutilisation directe des canaux et règles de notification (§11, ADR-019) ; historique de disponibilité par ressource dans la même UI.
- **Négatives** : un scheduler de checks fiable (intervalles, jitter, seuils, anti-flapping) et le stockage de l'historique de disponibilité sont des composants à part entière ; « hors du workload » implique de définir précisément le ou les points d'exécution des checks et leurs angles morts réseau.
- **Risques acceptés** : sans sondes multi-régions, un check reflète le point de vue du point d'exécution, pas celui de tous les utilisateurs finaux ; la frontière « pas d'APM » devra être défendue face aux demandes d'extension (codes d'erreur détaillés, pages de statut publiques…), qui exigeraient de nouvelles décisions.
