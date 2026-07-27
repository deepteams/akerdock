# ADR-005 — Builds: server's BuildKit in P0/P1, rootless builders mandatory for untrusted code

- **Status**: Accepted
- **Date**: 2026-07-11
- **Related PRD sections**: §27.5, §20.4(8), §23.1, §18.1, INV-010

## Context

Building images via the target server's Docker/BuildKit, with direct socket access, is fast and simple — but a build executes potentially hostile code (Dockerfile, build scripts) in a privileged neighborhood. The threat model (§23.1) is explicit: builders execute untrusted code and must be isolated from the control plane's credentials, from the global Docker socket when possible and from the sensitive internal network. The critical case is the preview of an approved fork PR (§20.4(8)). A trade-off must be made between immediate simplicity and the security target.

## Decision

- **P0/P1**: builds go through the **server's Docker BuildKit** (parity with the reference).
- Dedicated **rootless BuildKit builders become mandatory for untrusted code**, at the latest with the delivery of **approved fork previews** (§20.4(8)): isolated builder, no secret injected.
- The **build adapter contract is written from P0 onward**, so that the switch to isolated builders does not touch the deployment engine.

## Alternatives considered

- **Staying on the server's Docker socket forever**: rejected — unacceptable as soon as code from external contributors (forks) is executed, in contradiction with §23.1 and INV-010.
- **Isolated builders (rootless/VM/microVM) from P0**: rejected — delays initial parity for a need that only arises with fork previews; the adapter contract written from P0 preserves the trajectory.
- **MicroVM (Firecracker/Kata) as the isolation target**: not retained at this stage — superior isolation but infrastructure requirements (KVM, dedicated images) incompatible with the generic VPS; rootless BuildKit is the chosen compromise.

## Consequences

- **Positive**: parity and time-to-market in P0/P1; an explicit and dated security trajectory (at the latest, fork previews); the deployment engine is insensitive to the builder type thanks to the adapter.
- **Negative**: in P0/P1, a malicious build from a repository the team trusts has access to the server's Docker — the risk is bounded to the server's perimeter (§23.1) but real; maintaining two build paths (direct socket and rootless) increases the test matrix.
- **Accepted risks**: a P0/P1 window without strong build isolation, accepted because only repositories configured by the team are built there (forks remain ignored by default, INV-010); rootless BuildKit has known limitations (certain network/cache cases) that will have to be documented.
