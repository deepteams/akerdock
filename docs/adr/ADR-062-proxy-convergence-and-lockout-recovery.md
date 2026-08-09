# ADR-062 — The managed proxy converges to its intent, and never traps the operator

- **Status**: Accepted
- **Date**: 2026-08-08
- **Extends**: [ADR-009](ADR-009-proxy-intermediate-representation.md) — the IR and the
  dynamic-file reconciliation stand unchanged; this extends what is reconciled to the
  container that reads those files, and adds the recovery path around it
- **Updates**: [proxy-contract.md](../specs/proxy-contract.md) §1.3–§1.4
- **Related**: [ADR-052](ADR-052-agent-command-channel.md),
  [ADR-054](ADR-054-agent-host-ops.md)
- **Related PRD sections**: §3, §14.2, §20.1

## Context

On a self-hosted instance, the dashboard is served **by the very proxy the dashboard
administers**: the instance FQDN is routed by the managed Traefik under the reserved
`00-control-plane` scope (PRD §14.2). The control surface therefore depends on a
resource it controls, which is a bootstrap trap — and the trap has now closed on a real
operator.

The incident: the proxy was stopped from the dashboard. The instance's only sign-in
methods are passkeys and Google OIDC, and both are bound to the FQDN's origin — a
passkey's credential is scoped to its RP ID, and the OAuth redirect URI is registered for
that host. Reaching the control plane's raw port through an SSH forward therefore serves
the pages but **authenticates nobody**. Recovery required an SSH shell and prior knowledge
that the container is named `akerdock-proxy` and merely stopped, not removed.

Three gaps made that possible, and each one is independently reachable:

1. **Nothing observes the container.** The scheduler already reconciles proxy drift, but
   only the *files*: it reads each dynamic YAML, compares it to the checksum of the
   applied revision, and rewrites what moved (`internal/scheduler/scheduler.go`). The
   process that reads those files is watched by no one. A proxy removed by a human
   `docker system prune`, killed by the OOM reaper, or left stopped after a partial
   reboot stays down until someone notices the outage.
   AkerDock's own cleanup is not the risk here: `server.cleanup` prunes build cache,
   dangling images under a positive `akerdock.managed=true` filter, orphaned `-next`
   candidates, destroyed-preview images and (optionally) managed anonymous volumes. It
   never prunes containers, and no job calls `ContainersPrune`. The danger is a human on
   the host.
2. **Stopping the dashboard's own proxy is an ordinary button.** The control plane knows
   perfectly well which proxy serves the instance FQDN — the reserved scope appears among
   that server's applied revisions — and says nothing before the click.
3. **A removed container has no documented way back.** Its `docker run` carries five bind
   mounts, published TCP and UDP ports, labels, an env-file and a healthcheck. Nobody
   retypes it from memory, and the one place it exists is a Go string inside the binary
   the operator can no longer reach.

## Decision

Three layers: converge what can be converged, guard the act that cannot be undone from
inside, and leave a way in when neither applied.

### 1. The container is part of what drift reconciliation converges

Per-server reconciliation observes the managed proxy alongside its files. When
`proxy_desired_state = running`:

- container absent → re-bootstrap it (the existing idempotent path, which re-renders the
  static configuration, detects drift against what is deployed and recreates);
- container present but not running → start it.

Bounded, not blind: consecutive failures back off exponentially to a cap and, past a
threshold, set `proxy_observed_status = unhealthy` and stop retrying until the intent or
the configuration changes. A proxy that cannot bind its ports must be **reported**, not
restarted in a loop.

`proxy_desired_state = stopped` is never repaired. That clause of the API contract stands
unchanged, and it is precisely what separates converging toward the operator's intent
from overriding it.

### 2. Stopping the proxy that serves the dashboard is a guarded act

On the proxy whose applied revisions include the reserved `00-control-plane` scope — and
only that one — `stop`, and any action that would remove it, require an explicit
acknowledgement carried by the request, not merely a hopeful dialog. The refusal and the
confirmation both state the consequence and the recovery command. Every other server's
proxy keeps its one-click stop: cutting the routing of an application server is a normal
operation with a normal way back.

### 3. A break-glass that runs on the host, without the dashboard

`akerdock proxy repair` — a subcommand of the server binary, run on the machine that
hosts the instance with that instance's own configuration (database and master key). It
re-renders the static configuration for the target server and converges the container
exactly as the bootstrap does.

Its authority is possession of the host and of the instance's configuration — never an
API session, because the missing API session is the whole reason it exists. It is not a
new credential path: it grants nothing beyond restoring a resource the operator already
owns, and it reads secrets it is already trusted with.

## Alternatives considered

- **A recovery login token printed on the host.** Solves authentication, not a dead
  proxy, and adds a second way into an instance whose sign-in policy is deliberately
  narrow. Repairing the proxy restores the *normal* path, which is strictly better than
  standing up a bypass beside it.
- **Serving the dashboard beside the proxy** (binding the instance FQDN straight to the
  control plane). Moves the problem rather than solving it: certificate ownership, port
  443 contention, and still nothing when the container is gone.
- **`--restart always` instead of `unless-stopped`.** Would fight an operator's explicit
  stop, and survives neither `docker rm` nor a host that pruned the container.
- **Excluding the proxy from cleanup.** A no-op: cleanup already prunes no containers.

## Consequences

- A proxy stopped or removed outside AkerDock returns within one reconciliation tick, and
  the operator learns it from the server's activity rather than from a customer.
- The incident that motivated this ADR stays *possible* — an operator may still stop the
  dashboard's proxy — but stops being a trap: the consequence and the way back are stated
  before the act, and the way back is one documented command instead of a reconstructed
  `docker run`.
- An explicit `stopped` still means stopped, on every server.
- The scheduler gains one container observation per proxied server per tick, over the
  existing agent channel (ADR-052) — no SSH connection to open, same as the file check.
- proxy-contract §1.4 gains the convergence clause and the guarded-stop rule.

## Verification

- Unit: the convergence decision table (desired × observed → action) including every case
  where the answer is "do nothing"; backoff and the flip to `unhealthy` after the failure
  threshold; `stopped` never repaired; the guard predicate that recognizes the dashboard's
  own proxy from its applied revisions; `proxy repair` rendering byte-identical static
  configuration to the bootstrap path.
- The single E2E journey (ADR-026/028) is untouched.
