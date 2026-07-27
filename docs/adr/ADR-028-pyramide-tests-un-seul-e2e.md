# ADR-028 — Test pyramid: a single E2E journey

- **Status**: Accepted
- **Date**: 2026-07-18
- **Related PRD sections**: §26.2, §26.3, §27.26, §29.9
- **Revises**: ADR-026 for the volume and cadence of E2E; the Docker-in-Docker choice and its residual risks remain unchanged

## Context

The E2E catalog had grown to seven independent suites. Each suite
rebuilt PostgreSQL, a Docker-in-Docker server, AkerDock and several
auxiliary services. Even parallelized, they consumed a lot of runner
time, made failures slow to reproduce and slowed down the development
loop of a small team.

The majority of the guarantees exercised by this catalog are
deterministic rules: validation, authorization, interpolation,
configuration generation, command construction, state transitions,
retention, backoff or deduplication. Proving them through the assembled
product is slower and less precise than a test of the owning module.

## Decision

AkerDock maintains **exactly one automated product E2E journey**. It proves
only the seam that the lower levels cannot establish:

1. real startup with migrations and bootstrap;
2. adding and SSH-validating a Docker-in-Docker server;
3. verification of the initial `stopped` intent, then explicit start and
   real bootstrap of Traefik;
4. deployment of an application with an environment variable;
5. HTTPS routing and reading of JSON/SSE logs;
6. regeneration of a route after a modification;
7. rolling redeployment with no lost request;
8. rejection of unauthenticated calls;
9. deletion of the container and its route.

This journey:

- does **not run on pull requests**;
- runs after merge to `main`, on demand and before publishing a
  release;
- remains a single command, `make e2e`, with no shard and no nightly
  catalog;
- cannot gain a second scenario. A new guarantee goes first into a
  unit or module test. If only the assembled product can prove it, it
  replaces or enriches an assertion of the existing journey without
  creating a second stack.

Pull requests are gated by the fast Go and Angular tests, the targeted
PostgreSQL integration tests, contract generation, lint and the
conformance fixtures. The distribution smoke remains a separate packaging
test: it does not drive a product journey and runs after merge and
before release.

## Test ownership rule

The test lives at the lowest level capable of proving the guarantee:

- **unit**: parsers, validators, RBAC, computations, states,
  configuration rendering, escaping and commands;
- **module/targeted integration**: concurrent SQL, transport or protocol against
  its real dependency, without starting AkerDock in full;
- **E2E**: only the real SSH + Docker + proxy + traffic interaction during
  a switchover.

Every bug fix starts with a reproduction at the unit or module
level, unless there is written proof that only the assembled journey can
reproduce the defect.

## Alternatives considered

- **Keeping the smoke on every PR and the nightly catalog**: rejected, because the
  daily cost and the maintenance remain the very reasons motivating this decision.
- **Keeping several E2E but parallelizing them further**: rejected; it
  reduces wall-clock time but increases runner cost and does not make failures
  more local.
- **Removing all E2E**: rejected; no isolated test proves a real
  lossless switchover through SSH, Docker and Traefik.

## Consequences

- **Positive**: faster pull request feedback, more local diagnosis,
  less flakiness and infrastructure maintenance; a strategy tenable
  for a small team and predictable for a medium one.
- **Negative**: variants of engines, providers and build packs are no
  longer replayed end to end automatically. Their contract must be covered
  at the module level and a targeted manual validation remains necessary before a
  risky change.
- **Required discipline**: removing an old E2E without an equivalent lower-level test
  creates explicit debt, not implicit proof. The product matrices
  point to the relevant module tests.
- **ADR-026 risk unchanged**: systemd, real reboot, firewall, full disk and
  ARM64 remain outside automation.
