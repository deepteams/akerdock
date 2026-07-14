# ADR-008 — Observabilité : OTLP partout, exposition Prometheus, aucun protocole propriétaire

- **Statut** : Accepté
- **Date** : 2026-07-11
- **Sections PRD liées** : §27.8, §3.8, §13

## Contexte

Un agent de métriques qui pousse vers l'instance avec un protocole maison (endpoint + token) enferme les données de télémétrie : impossible de brancher l'outillage standard (collecteurs, dashboards, alerting) sans écrire un adaptateur. Il faut choisir entre un protocole propriétaire et les protocoles ouverts du domaine (§3.8).

## Décision

**OTLP partout** : l'agent serveur, le control plane et les workers émettent **métriques, traces et logs en OpenTelemetry (OTLP)**, avec **exposition Prometheus** ; **aucun protocole propriétaire** n'est introduit.

Le principe d'un agent léger par serveur est conservé (parité Sentinel : CPU/RAM serveur et par container, disque, historique dans l'UI — §3.8, §13), mais son transport et son format sont standards.

## Alternatives considérées

- **Protocole push propriétaire (parité Sentinel)** : rejeté — enferme la télémétrie, impose de maintenir un protocole, et empêche l'utilisateur de brancher son propre backend (Grafana, Datadog, etc.).
- **Prometheus pull uniquement, sans OTLP** : rejeté — couvre les métriques mais ni les traces ni les logs, et le pull exige des ports entrants sur les serveurs cibles, à rebours de la réduction de surface (ADR-001).
- **Pas d'agent du tout (exec de commandes via SSH)** : rejeté — pas d'historique fiable, coût SSH répété, pas de granularité par container satisfaisante.

## Conséquences

- **Positives** : interopérabilité totale avec l'écosystème (collecteurs OTel, Prometheus, Grafana) ; le même standard sert l'auto-observation du control plane et le monitoring des serveurs ; instrumentation par trace/correlation ID cohérente avec les exigences d'audit et de DoD (§26.3).
- **Négatives** : dépendance aux SDK/semconv OpenTelemetry, qui évoluent encore ; l'agent doit embarquer un exporter OTLP plutôt qu'un simple POST maison, ce qui augmente un peu son empreinte.
- **Risques acceptés** : la volumétrie traces/logs OTLP peut être significative sur de petites installations — les fréquences et rétentions doivent rester configurables comme dans la référence ; les graphiques intégrés de l'UI doivent consommer ces données standards sans réintroduire de canal parallèle.
