# ADR-011 — PR previews enriched from initial delivery, opt-in trigger controls

- **Status**: Accepted
- **Date**: 2026-07-11
- **Related PRD sections**: §27.11, §5.6, §20.4, §26.2, INV-010

## Context

A minimal preview deployment — one container and one public URL per PR, with no cap, no TTL, no access protection, no compose support, with one comment per deployment — is below the domain standard (§5.6, §15). Dedicated preview platforms have set the bar significantly higher: full compose environments, ephemeral data, TTL, protected access, Git checks. A decision is needed on the level being targeted, and when it is delivered.

## Decision

The preview feature is **delivered enriched from the start**: the entire scope of §20.4 is **prioritized** and shipped with the feature, not as a later extension. Concretely:

- **ephemeral compose**: full stack per PR, isolated network, dedicated volumes, per-instance magic variables, full destruction on cleanup;
- **ephemeral data**: databases provisioned by seed or snapshot clone, never implicitly shared with production or another preview;
- **TTL, caps and scale-to-zero**: cap on simultaneous previews per application and per server, inactivity TTL, separate resource limits, optional preview server pool, scale-to-zero desired at the proxy level;
- **access protection by default**: basic auth or signed link + `X-Robots-Tag: noindex`, public exposure by explicit choice only;
- **watch paths for previews** (monorepo);
- **rich Git checks**: commit statuses/checks, GitHub Deployments API, single comment updated in place, GitLab/Gitea feedback parity;
- **forks on approval**: preview possible after a maintainer's approval, isolated builder, no secrets injected (INV-010).

The **trigger controls** (opt-in via PR label, comment commands `/deploy` `/destroy`, exclusion of draft PRs, cancellation of stale builds) are **per-application options, disabled by default** — the parity behavior remains the default.

## Alternatives considered

- **Minimal parity first, enrichment later**: rejected — the PRD establishes that the §20.4 scope is part of the feature itself; shipping the minimum would create public previews that are unprotected and uncapped, known flaws of the reference.
- **Trigger controls enabled by default**: rejected — would surprise users coming from the reference; the default remains the parity behavior, each control is opt-in individually.
- **Delegating previews to an external tool**: rejected — previews are a core product differentiator and require integration with the proxy, secrets and the internal lifecycle.

## Consequences

- **Positive**: major product differentiator over the reference; security by default (protected access, production secrets never copied, forks ignored without approval); controlled costs (TTL, caps, scale-to-zero) where the reference has no cap at all.
- **Negative**: a first-delivery scope significantly larger than parity — ephemeral compose, TTL lifecycle, multi-provider checks integrations and isolated builders (dependency on ADR-005) must all exist to declare the feature complete (evidence required §26.2).
- **Accepted risks**: the richness of the Git integrations (checks, deployments, single comment) multiplies the provider-specific surfaces to maintain (GitHub/GitLab/Gitea); scale-to-zero at the proxy is a SHOULD, liable to arrive after the rest of the scope.
