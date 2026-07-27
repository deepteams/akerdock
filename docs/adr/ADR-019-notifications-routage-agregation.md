# ADR-019 — Notifications: rule-based routing, aggregation/debounce, quiet hours

- **Status**: Accepted
- **Date**: 2026-07-11
- **Related PRD sections**: §27.19, §11, §26.2

## Context

Emitting one message per event, with per-channel event activation as the only setting (§11), makes the channel unusable: a flapping server (unreachable/reachable in a loop) generates dozens of identical alerts, and all of a team's projects share the same channels. This noise destroys the value of alerts — the real ones end up ignored. We must decide on the routing model and the fight against noise.

## Decision

Four capabilities established, on top of the parity channels (Email, Discord, Telegram, Slack, Pushover, webhooks — §11):

1. **Routing rules** by project, environment, and severity toward channels — and no longer just an event × channel matrix at the team level.
2. **Aggregation/debounce of repetitive events**: a flapping server produces one aggregated alert, not dozens of messages.
3. **Configurable quiet hours**.
4. **Deferred digest of non-critical events**: what does not require immediate action is grouped and sent later.

## Alternatives considered

- **Strict parity (one message per event)**: rejected — flapping in a loop is precisely the observed defect of the reference; noise makes alerting counterproductive.
- **Delegating to an external alerting tool (Alertmanager, PagerDuty…)**: rejected as the default answer — imposes one more component on the self-hosted target; custom webhooks remain available for whoever wants to plug in these tools.
- **Routing per individual resource**: rejected — too fine a grain to administer; project/environment/severity covers the real needs (prod alerts, staging digests).

## Consequences

- **Positive**: actionable alerts (production wakes you up, staging waits for the digest); end of message storms in case of flapping; quiet hours respect on-call duty without disabling alerting.
- **Negative**: rule engine, debounce windows, and deferred digest queues to design, persist, and test (flapping/debounce tests required §26.2); the configuration becomes richer and therefore more complex to present — safe and simple defaults are indispensable.
- **Accepted risks**: any aggregation delays or groups information — a critical event misclassified as "non-critical" would be deferred, hence the importance of the severity taxonomy; the default behavior must remain simple and predictable so as not to surprise the operator discovering the product.
