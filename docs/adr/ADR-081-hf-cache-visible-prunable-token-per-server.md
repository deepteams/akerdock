# ADR-081 — The HF cache is visible and prunable, and the token follows the server

- **Status**: Accepted
- **Date**: 2026-08-17
- **Related**: [ADR-080](ADR-080-models-first-class-inference.md) §4 (the server-scoped
  cache this makes visible), [ADR-079](ADR-079-gpu-is-an-observed-fact.md) (the observed
  GPU that gates the tab), [ADR-075](ADR-075-keys-born-inside-never-leave.md) (the
  write-only stance the token borrows), [ADR-052](ADR-052-agent-command-channel.md) (the
  typed one-shot the inspection rides)
- **Related PRD sections**: §3, §6, §26

## Context

ADR-080 gave every GPU server one shared weights cache (`akerdock-hf-cache`) that nothing
ever deletes — deliberately: a model is not the owner of the corpus it read. That leaves
two operator needs unanswered. The cache **grows silently** — tens of gigabytes per model
tried — with no way to see what is in it or reclaim space short of an SSH session and a
`docker volume` incantation. And the Hugging Face token was **instance-global**
(`AKERDOCK_HF_TOKEN`, an env var): two GPU servers with different HF accounts, or an
operator who wants to set the token from the dashboard rather than the compose file, had
no story.

## Decision

### 1. A per-server token, write-only

`servers` gains `hf_token_enc` (enveloped, ADR-003). It is **set, replaced or cleared —
never read back** (the ADR-075 stance: a stored secret's purpose here is to be *used*, not
retrieved; the dashboard shows only "a token is stored"). For the engines of that server it
**wins over** `AKERDOCK_HF_TOKEN`, which stays as the instance-wide fallback and remains
what the Hub *search proxy* uses — the search serves a form that has not chosen a server
yet. The token reaches containers as env, never argv (INV-003), so it appears in no
exported command.

### 2. The cache is listed and pruned through typed one-shots

Inspection and deletion run as **one-shot busybox containers** on the server (the ADR-052
pattern — `probePostgresUID`'s precedent), mounting the cache volume: `du` over the hub
layout (`models--org--name`) for the listing, `rm -rf` **as pure argv** for the deletion —
no shell interpolation anywhere near an operator-supplied string, and the model reference
is validated against the Hub's own naming rules before it is even mapped to a path.
Deleting one model's weights or emptying the whole cache is an explicit operator act,
confirmed in the UI; a model currently running keeps serving from its mapped pages and
simply re-downloads at its next start.

### 3. The tab follows the GPU

The server page gains a **Hugging Face** tab, shown when the server carries an observed
GPU (ADR-079's fact — the tab would be noise anywhere else): the token field, the cache
contents with per-model sizes, per-model delete and empty-the-cache. Listing requires
`servers:read`; deleting, `servers:maintain` (the cleanup permission — this is cache
maintenance); the token, `servers:manage`.

### What this ADR does not decide

No quotas, no automatic cache GC, no LRU policy — reclaiming space stays an operator act.
No per-model token. No datasets/spaces listing (the hub layout carries them; models are
what this platform serves).

## Verification

- Unit: hub-name mapping and validation (accepts the Hub's charset, refuses traversal and
  shell metacharacters by construction), one-shots asserted on the fake runtime (image,
  volume bind, pure-argv delete), listing parses `du` output, per-server token wins over
  the instance fallback in the container env.
- Handlers: token set/clear enveloped and never echoed; delete refuses an invalid
  reference by name; permissions as stated.
