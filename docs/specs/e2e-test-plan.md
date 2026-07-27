# Specification — Test pyramid and single E2E journey

> PRD §29.9 artifact. ADR-028 sets the volume and the cadence; ADR-026 sets
> the Docker-in-Docker environment and the residual risk.

## 1. Objective

The suite must give developers fast feedback while retaining proof that the
assembled product works. The guiding principle is:
**test at the lowest level capable of proving the guarantee**.

This strategy explicitly targets small and medium-sized teams:

- bounded and predictable CI cost;
- failures attributable to a package or a module;
- fast local execution without infrastructure for the common case;
- a single assembly proof, readable and maintainable.

## 2. Pyramid

| Level | What it proves | Examples | Cadence |
|---|---|---|---|
| Unit | Deterministic rule without external dependency | validation, RBAC, parsing, interpolation, backoff, command/configuration rendering, states | local + every PR |
| Module | A module's contract with its real dependency, without the full product | queue/leases on PostgreSQL, crypto, proxy golden files, Git protocols | every PR |
| Product E2E | Real seam impossible to simulate faithfully | SSH → Docker → Traefik → traffic during a switchover | after merge, manual, release |
| Packaging | The installable artifact, not a user journey | distroless image + reference compose | after merge, release |

A test does not move up the pyramid to "be more realistic". It moves up
only if the lower level cannot establish the property.

## 3. Single E2E journey

Command: `make e2e`.

Topology:

- a real PostgreSQL;
- the AkerDock binary in all-in-one mode;
- a Docker-in-Docker target server with `sshd`;
- a Traefik proxy created by AkerDock;
- a reference nginx application.

The full journey verifies:

1. migrations, root user and instance key at startup;
2. SSH registration and server validation;
3. proxy initially stopped, then explicit start, bootstrap and
   Traefik availability;
4. creation of a project and an application;
5. injection of a variable, deployment and a real HTTPS service;
6. deployment logs in JSON and over SSE;
7. domain change and immediate regeneration of the routing;
8. redeployment with health check under continuous traffic, without a lost request;
9. rejection of anonymous calls and invalid tokens;
10. safe deletion of the container and its route.

This journey is indivisible: it uses one stack and produces one verdict. There
is no shard, no nightly matrix, no second E2E scenario.

## 4. Coverage carried by the fast levels

The former E2E scenarios no longer constitute a regression matrix. The
recurring guarantees belong to the following suites:

| Guarantee | Fast owner |
|---|---|
| Isolation and permissions of all operations | `internal/handlers/rbac_test.go` |
| Git, S3, uptime, cron and quiet-hours validation | `internal/handlers/validation_test.go` |
| Escaping of hostile values and absence of secrets in argv | `internal/jobs/shellquote_test.go`, `internal/jobs/composedeploy_test.go` |
| Deployment construction and deterministic resumption | `internal/jobs/deploymentrun_test.go`, `internal/jobs/applicationdelete_test.go` |
| Compose, magic variables and preview routing | `internal/compose/*_test.go`, `internal/jobs/previewrouting_test.go` |
| Webhooks, forks and watched paths | `internal/gitwebhook/*_test.go`, `internal/jobs/webhookprocess_test.go` |
| Queue, leases, concurrency and idempotence | `internal/queue/queue_test.go` against PostgreSQL |
| Encryption, redaction and sessions | `internal/envelope`, `internal/audit`, `internal/session` |
| Deterministic proxy and wildcards | `internal/proxy/*_test.go`, `tests/proxy-conformance/` |
| Notifications and uptime | `internal/notify/*_test.go`, `internal/uptime/uptime_test.go` |
| Client, forms, WebAuthn, terminal and UI states | `web/src/**/*.spec.ts` |

A missing line is module-test debt to be addressed; it does not justify
restoring an E2E catalog.

## 5. Rules for a contribution

For any new logic or fix:

1. write a table-driven unit test close to the owning code;
2. add a module test only if the property genuinely depends on
   PostgreSQL, a protocol or an external format;
3. modify the E2E journey only if real SSH, Docker and the proxy are all
   necessary for the proof;
4. in that last case, enrich or replace an assertion without creating a
   new stack or a new scenario;
5. keep fixtures deterministic and timeouts only at external
   boundaries.

Fix tests must fail before the fix. Blind retries are
forbidden: a flaky test is quarantined with an issue and a removal
deadline.

## 6. CI

| Trigger | Tests |
|---|---|
| Local development | touched package, then `make test` |
| Pull request | Go unit/module, Angular unit, generated contract, lint, OpenAPI, Storybook |
| Merge to `main` | same tests + single E2E journey + packaging |
| `workflow_dispatch` | same tests + single E2E journey + packaging |
| Tag `v*` | fast tests + single E2E journey + packaging before publication |

There is no longer an E2E nightly cron.

## 7. Risks and manual validations

ADR-026 still applies: systemd, real reboot, firewall/UFW, physically
full disk and ARM64 are not reproduced by DinD. These classes are
validated on an ad hoc basis on a real machine before any sensitive change to the
transport, onboarding or distribution.

The full variations of Git providers, data engines and build packs
are covered by their protocol/module tests and by a targeted manual
validation when they are modified. This trade-off is accepted to keep
a short development loop.
