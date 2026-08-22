# ADR-082 — The occupied-GPU guard counts memory, not models

- **Status**: Accepted
- **Date**: 2026-08-19
- **Revises**: [ADR-080](ADR-080-models-first-class-inference.md) §5, third bullet
  ("Starting into an occupied GPU warns, with the swap one click away"). The rest of §5 —
  an explicit stop is a state, a restart reloads rather than re-downloads — stands
  unchanged, as does every other section of ADR-080.
- **Related**: [ADR-079](ADR-079-gpu-is-an-observed-fact.md) (the observed GPU the guard
  hangs off), [ADR-062](ADR-062-proxy-convergence-and-lockout-recovery.md) (the
  desired-state stance the stop borrows)
- **Related PRD sections**: §3, §6, §26

## Context

ADR-080 §5 called the occupied-GPU check a **soft guard**, and reasoned for it exactly as
one would: "two models sized at ninety percent of memory cannot run together, but the
platform cannot know every legitimate combination (two small models can)". The confirmation
was specified to name the running model **and its memory fraction**.

What shipped counts models, not memory. `ListRunningModelsOnServer` returns
`memory_fraction` for every running neighbour and the handler reads only the name: any
model RUNNING on the server produces a 409, and the single offered action is
`swap=true` — stop the neighbour, start this one. The fraction is never shown and never
summed.

The consequence is that the case the ADR wrote down as the reason for softness is the case
that cannot happen. An operator running one model at `memory_fraction: 0.45` who wants a
second at `0.40` — eighty-five percent of the card, comfortably within it — is told the two
"rarely fit one GPU" and offered only the choice of killing the first. There is no path,
anywhere in the product, to cohabitation. A guard that admits no exception is not soft; it
is a wall with an apologetic message.

This is a defect against ADR-080's own reasoning, but repairing it changes the DECISION's
observable contract — a start that used to be refused now succeeds — so it goes through a
new ADR rather than an edit, per the repository's immutability rule.

## Decision

### 1. The guard sums declared fractions and compares against the card

Starting a model while others run on the same GPU server computes
`Σ memory_fraction(running) + memory_fraction(candidate)`.

- **Within budget** (≤ `0.95`): the start proceeds. No confirmation, no 409 — cohabitation
  is a normal operating mode, not an exception to be argued for each time.
- **Over budget**: 409 `gpu_busy`, as before, but stating the arithmetic — each running
  model with its fraction, the candidate's, and the total — and offering **two** actions
  rather than one.

The `0.95` bound is headroom, not superstition: the fractions are of *total* card memory
and each engine process carries a CUDA context and allocator slack that its declared
fraction does not fully account for. A sum landing between the bound and 1.0 is the case
most likely to fail late, deep in weight loading, which is the worst moment to discover it.

### 2. An unset fraction is unknown, not free

A model that declares no `memory_fraction` is counted at **0.9**, the value both engines
default to when the flag is absent (vLLM's `--gpu-memory-utilization`, SGLang's
`--mem-fraction-static`). Counting it as zero would make the guard's arithmetic silently
optimistic exactly where the operator gave it least to work with — and one unconfigured
model is, by that default, already the whole card.

### 3. Two ways past the guard, and they mean different things

- `swap=true` — the ADR-080 action, unchanged: stop the named neighbours first, start this
  one, ordered inside a single job.
- `force=true` — start alongside anyway. This exists because the declared fractions are a
  *declaration*: a quantized model may use far less than its flag reserves, and an operator
  reading `nvidia-smi` knows something the database does not. Forcing is not the platform
  being persuaded it was wrong; it is the operator taking the outcome, which at worst is an
  out-of-memory failure at load time — visible, contained, and now caught early by the
  crash-loop detection rather than waited out.

The two are mutually exclusive: a request carrying both is refused, because "stop them"
and "run beside them" cannot both be the intent.

### 4. What the guard still is not

It is not an admission controller. It reads declared configuration, never live device
memory: an honest number the operator wrote, not a measurement. A model that a *different*
tenant of the machine — a training run, a notebook — is starving of memory is outside what
this can see, and the guard neither claims otherwise nor tries. Live accounting would
require polling the device through the agent on every start, and would still be racy by the
time the container allocates.

## Consequences

- Two small models on one card become a supported, one-click operation. The daily loop of
  ADR-080 §5 gains a third move — *run both* — beside stop and swap.
- The refusal becomes actionable: it says which models, at what fractions, summing to what,
  instead of asserting that two models "rarely" fit.
- An operator can still shoot their own foot with `force`, deliberately and audibly.
- The guard's arithmetic is only as good as the declared fractions, which §4 states rather
  than hides.

## Verification

- A start whose sum fits proceeds without a 409 (0.45 + 0.40 within the bound).
- A start whose sum exceeds the bound is refused, and the error details name every running
  model with its fraction plus the computed total.
- A model with no declared fraction is counted at 0.9: one such neighbour alone puts any
  candidate over the bound.
- `swap=true` still stops the neighbours first, inside one job, in order.
- `force=true` starts alongside without stopping anything.
- `swap` and `force` together are refused before anything is enqueued.
