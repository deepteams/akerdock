# ADR-002 — Durable PostgreSQL queue, no external bus

- **Status**: Accepted
- **Date**: 2026-07-11
- **Related PRD sections**: §27.2, §16.1(6), §18.1, §18.2, §18.3, §21.3

## Context

The product goal is a control plane that is lightweight to operate: a single Go binary + PostgreSQL, with no Redis or application runtime (§16.1(6)). The common solution — a Redis next to the database for queues and cache — adds a second stateful system to install, back up and monitor. A separate bus (Redis/NATS) would improve raw throughput, but would add a component to install, back up, monitor and update for every self-hosted installation. The backing store for the durable job queue (deployments, backups, server validations, etc.) must be chosen.

## Decision

The durable queue is implemented in **PostgreSQL**. PostgreSQL is the single source of truth: configuration, states, history, audits, **leases and outbox** (§18.1). Jobs follow the generic state machine of §21.3 (lease with expiration and heartbeat, retry, dead-letter), and the **transactional outbox** pattern publishes events after commit (§18.2).

The **queue interface remains abstract in the code**, but **no external bus (Redis/NATS) is planned**: a single implementation is shipped and maintained.

## Alternatives considered

- **Redis (parity with the reference)**: rejected — an additional component to operate and back up, contrary to the "Go binary + PostgreSQL only" footprint commitment.
- **NATS/JetStream**: rejected — better throughput and streaming semantics, but unjustified operational complexity for the target volumes (§22.2), which remain achievable with PostgreSQL.
- **In-memory queue in the Go process**: rejected — it would violate INV-013 (an accepted job must survive a process restart) and would rule out multi-instance operation (§18.2).

## Consequences

- **Positive**: a single stateful system to install, back up and restore; ACID transactions across business mutation, job enqueueing and outbox (no inconsistency window); simplified self-hosting in line with the product commitment.
- **Negative**: lower throughput than a dedicated bus; queue/lease queries (SELECT … FOR UPDATE SKIP LOCKED and similar) become critical paths to write and index carefully (hence pgx + sqlc, cf. ADR-025); the queue load and the business load share the same database.
- **Accepted risks**: if the capacity targets (§22.2: 1,000 webhook deliveries/minute in burst, 50 concurrent builds) became insufficient, migrating to an external bus would be a substantial undertaking — mitigated by the abstract queue interface, but no speculative work is undertaken.
