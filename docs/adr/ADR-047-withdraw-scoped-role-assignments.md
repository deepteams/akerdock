# ADR-047 — Withdraw scoped role assignments: the team stays the boundary

- **Status**: Accepted
- **Date**: 2026-07-29
- **Supersedes**: [ADR-046](ADR-046-scoped-role-assignments.md) (role assignments scoped to a
  project or an environment), built and removed within a day
- **Also settles**: the scoped half of [ADR-007](ADR-007-fine-grained-rbac-project-environment.md),
  which ADR-046 was implementing — it is not deferred, it is dropped
- **Related PRD sections**: §15, §16.3, §23.1, §27.7, §29.7

## Context

ADR-046 shipped: `role_assignments`, the `none` base role, a resolution engine, the scoped
check inside every resolver, collection filtering, an assignment API and its UI. It worked,
it was tested, and the maintainer's verdict on reading it was that **authorization had become
the most complicated part of the platform** — harder to hold in the head than deployments,
backups, previews or the proxy, which are the things AkerDock exists to do.

That verdict is the decisive input, and it is worth taking at face value rather than
arguing with. A self-hosted PaaS is chosen for being comprehensible by one person. The
scoped model asked that person to hold, at every read: which scope a resource belongs to,
which assignment covers it, whether the most specific one replaces or adds, which of three
classes each permission falls into, and why a list is shorter than the table behind it. Six
things where the rest of the product asks for one.

The cost was also structural, not only cognitive. Every collection endpoint had to filter,
forever, including ones written years from now by someone who never read this ADR. Every
resolver carried a second question. `Identity` stopped being an answer and became material
to re-evaluate. None of that is wrong — it is what per-project authorization costs, and it
is why every platform that offers it has a dedicated screen and a support page for it.

## Decision

**Scoped role assignments are removed.** Authorization goes back to: one role per member per
team, one role per API token, evaluated with `require(permission)` and nothing else.

1. **The team is the isolation boundary** (§23.1), and it is the only one. A member holds
   their role over every project of their team. Two perimeters that must not see each other
   are two teams — infrastructure is declared twice, and that duplication is the honest
   price of a boundary that is real and understandable.
2. **`role_assignments` is dropped** (migration 00082). Members parked on the `none` base
   role are moved back to `member` **before** the table goes, because `none` only ever made
   sense paired with assignments: leaving them would be a silent lockout nobody could
   diagnose. The enum value survives — PostgreSQL does not remove one — and nothing writes
   it.
3. **`projects:create` is folded back into `projects:manage`.** It existed because creating
   a project had no parent scope to be evaluated against; with no scopes, the distinction
   costs a permission and buys nothing.
4. **Invitations and SCIM go back to provisioning `member`.** The `none` default was there
   so an arrival held nothing until assigned; without assignments it would mean an arrival
   holds nothing at all, forever, which is not a default, it is a bug.
5. **The access view goes too.** It was built to answer "who can reach this resource"; with
   one role per member per team, the answer on an application, a database, a project and an
   environment is the same list every time — the team's members and their roles, which is
   what **Team → Members** already shows. Five screens repeating one table is not a review,
   it is duplication that has to be kept in step. The question it existed for is answered by
   the members list; if a richer review comes back, it comes back as *one* screen.
6. **A token capped by its creator stays** (`Middleware.boundToCreator`). rbac-matrix §4.2
   has specified it from the start and it was never built: a token carried its own
   permissions and outlived the authority that produced it — a demoted or departed creator
   kept handing out everything the token was minted with. That is a real hole, it has
   nothing to do with per-project scoping, and it stays closed. It is the one thing this
   whole episode leaves behind.

## Consequences

- **Positive**: authorization is one question again — does this identity hold this
  permission — and the whole model fits in the RBAC matrix's §2 table; no filter to remember
  in the next list endpoint; no second evaluation per read; `Identity` is an answer again.
  The product loses a feature it could not explain, and keeps the one thing from that work
  that stands on its own: a token that cannot outlive its creator's authority.
- **Negative**: "deploy to staging but not production" and "restrict Alice to billing" are
  **not expressible**, and the answer to both is "use a separate team" — which duplicates
  servers, keys, sources and registries, and gives no cross-perimeter view. Teams that
  needed it will feel it. ADR-007's promise of per-project RBAC is now **withdrawn**, not
  pending: the documents say so plainly rather than leaving a stale intention that reads as
  a roadmap.
- **Accepted risks**: an endpoint's `project_id`/`environment_id` (ADR-045 §1) becomes
  *documentation of intent* — it says what a destination is for and enforces nothing. This
  is the state the code was already in before ADR-046; what changes is that the matrix and
  the UI now say so instead of implying a protection. A field that describes without
  protecting is acceptable **only** while it is labelled as such, and that labelling is part
  of this decision, not an afterthought.

## Alternatives considered

- **Keep it and document it better**: rejected — the objection was not that the feature was
  badly explained, it was that it made authorization the hardest part of the product. More
  documentation about a complicated thing leaves it complicated.
- **Keep the engine, hide the UI**: rejected — dead code that every future read still pays
  for and that nobody exercises is the worst of both. If it comes back, it comes back with
  its screen.
- **Keep project scope, drop environment scope**: rejected — halves the expressiveness while
  keeping the whole mechanism (resolution, classes, filtering). The cost is in the mechanism.
- **Scope only the sensitive verbs** (deploy, secrets, terminal): rejected for the same
  reason, plus it produces a model where two permissions behave differently for reasons a
  reader has to look up.

## Verification

The suite that proved the scoped behavior is deleted with it. What must be checked is that
**removal changed nothing else**: the existing authorization tests pass unchanged (they do —
they were written against the un-scoped model and never adapted), a member reaches every
project of their team, and `require` is the only gate. The survivor keeps its own tests: a
token narrowing with its creator — demoted, departed, and the no-creator-recorded case.
