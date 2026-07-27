# ADR-015 — Compose reliability "by design": zero-downtime and resource limits actually enforced

- **Status**: Accepted
- **Date**: 2026-07-11
- **Related PRD sections**: §27.15, §15, §5.3, §5.5, §26.2, INV-005

## Context

Two structural pitfalls lie in wait for Docker Compose stacks (§15): zero-downtime is often abandoned there — every redeployment cuts the service — and the resource limits declared in the file are not actually enforced. These defects affect precisely the build pack most used for real services. We must decide whether they are accepted or addressed from the design stage.

## Decision

Both limitations are addressed **from the design stage**:

1. AkerDock **MUST provide zero-downtime for the web services of compose stacks**: **per-service** switchover behind the proxy (new container for the service started, health check satisfied, traffic switched, old one stopped), instead of a global `down`/`up` of the stack. Consistent with INV-005: a healthy application remains routed as long as its replacement has not satisfied the switchover conditions.
2. AkerDock **MUST actually enforce the declared resource limits** on compose resources (memory/CPU, §5.3), verifiable at the cgroups level (proof required in the §26.2 matrix: "E2E rolling update of a compose stack + cgroups verification").

## Alternatives considered

- **Strict parity (reproducing the limitations)**: rejected — these are acknowledged defects of the reference, not behaviors to preserve; the targeted parity covers capabilities, not bugs.
- **Compose zero-downtime via blue/green of the entire stack**: rejected — doubling the whole stack (databases included) is costly and dangerous for data; per-service switchover behind the proxy limits the doubling to web services.
- **Fix later, after parity**: rejected — the PRD establishes a "by design" treatment: retrofitting zero-downtime into an already-written compose engine would cost more than designing it in from the start.

## Consequences

- **Positive**: compose stacks become first-class citizens — deployments without downtime, limits actually enforceable — on the same level as a single-container application.
- **Negative**: the compose deployment engine is significantly more complex than a `docker compose up`: per-service orchestration, temporary coexistence of two versions of a service on the same network, per-service proxy configuration generation; enforcing the limits requires a systematic transformation of the user's compose file.
- **Accepted risks**: per-service zero-downtime only applies to web services behind the proxy — non-exposed services (workers, databases) follow a classic replacement; some stacks (shared state, application locks) do not tolerate two simultaneous instances of the same service, and this case must remain disableable per service.
