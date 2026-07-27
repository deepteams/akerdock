# ADR-022 — Predefined variables: `AKERDOCK_*` prefix only, no aliases

- **Status**: Accepted
- **Date**: 2026-07-11
- **Related PRD sections**: §27.22, §5.4, §27.10, §29.5

## Context

AkerDock injects predefined variables into every container it deploys (FQDN, URL, branch, PR identifier… — §5.4). Their namespace must be chosen. Platforms in this domain prefix these variables with their own brand, and part of the ecosystem of templates and applications reads those names: adopting an existing prefix would ease copy-pasting third-party templates, at the cost of a name that is not ours in every one of our containers.

## Decision

**`AKERDOCK_*` prefix only**: `AKERDOCK_FQDN`, `AKERDOCK_URL`, `AKERDOCK_BRANCH`, `AKERDOCK_PR_ID`, etc. — **no aliases** under any other prefix:

- one variable, one name: two names for the same value is a divergence waiting to happen (documentation, support, and the day only one of the two gets updated);
- our own identity, no naming debt under a third-party brand inside our users' containers.

The syntax of the **`SERVICE_<TYPE>_<ID>` magic variables** (`SERVICE_FQDN_*`, `SERVICE_PASSWORD_*`, `SERVICE_URL_*`… — §5.4) is **kept as is**: it is functional and brand-free, and it is what carries most of the compatibility of compose files in this domain (generated credentials, URLs).

## Alternatives considered

- **Aliases under a third-party prefix, in parallel**: rejected — every variable would exist in duplicate forever; the debt would never be repaid and the documentation would have to cover both names.
- **Adopting the prefix of an existing platform (maximum compatibility)**: rejected — would anchor the product under a third party's brand, including in every deployed container, and would tie our namespace to its evolution.
- **Also renaming the magic variables (`AKERDOCK_SERVICE_*`)**: rejected — `SERVICE_<TYPE>_<ID>` is a functional, brand-free syntax; breaking it would destroy the compatibility of compose templates without any identity gain.

## Consequences

- **Positive**: a clean and consistent namespace from day one; no documentation ambiguity; keeping `SERVICE_<TYPE>_<ID>` preserves the compatibility of the ecosystem's compose files.
- **Negative**: a template written for another platform that reads its prefixed variables does not work as is — the variables must be translated at import time into the template repository (§27.10, ADR-010); an application whose **code** reads a third-party prefix must be adapted.
- **Accepted risks**: friction for anyone coming from an existing ecosystem (accepted — generic adoption §20.7 remains the entry path); template import must detect non-trivial usages (interpolations, scripts) that a mechanical rewrite would miss.
