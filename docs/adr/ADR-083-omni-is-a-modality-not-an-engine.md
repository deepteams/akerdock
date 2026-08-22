# ADR-083 — Omni serving is a modality of the engine, not another engine

- **Status**: Accepted
- **Date**: 2026-08-22
- **Extends**: [ADR-080](ADR-080-models-first-class-inference.md) §1 (the typed core gains
  one column) and §4 (the container input contract, until now indexed by the engine alone,
  becomes indexed by the *runtime* — the pair engine × modality). The engine enum keeps its
  two values, the two-tier configuration, the reserved-flag discipline, the export/import
  round-trip and the lifecycle are untouched.
- **Related**: [ADR-079](ADR-079-gpu-is-an-observed-fact.md) (the observed GPU every model
  is placed against), [ADR-081](ADR-081-hf-cache-visible-prunable-token-per-server.md) (the
  cache and token these runtimes read), [ADR-082](ADR-082-the-gpu-guard-counts-memory.md)
  (the placement guard, which counts the same declared fraction whatever the modality)
- **Related PRD sections**: §3, §6, §26

## Context

The model resource serves text. Both engines have since grown an **omni** sibling — one
serving surface for speech, audio, image and video models — and the models operators
actually ask for now land there: `MiniMaxAI/MiniMax-Music3` is served by SGLang-Omni,
`Qwen/Qwen3-TTS` and `Qwen/Qwen-Image` by vLLM-Omni. Neither can be launched today:

- `internal/inference` hard-codes one invocation per engine — `python3 -m
  sglang.launch_server` for SGLang, nothing at all for vLLM (whose image ENTRYPOINT *is*
  `vllm serve`). SGLang-Omni is a **separate package** exposing `sgl-omni serve` and no
  `sglang.launch_server` module at all, so the rendered command names a module its image
  does not contain.
- The paste-import refuses the command upstream documents: `stripInvocation` knows four
  prefixes, none of them `sgl-omni serve`, so the parser reads `serve` as a stray
  positional and errors out.

The obvious repair — a third and fourth value of the engine enum — is wrong about what
varies. **The flag vocabulary does not change**: vLLM-Omni is a plugin of vLLM and keeps
`--gpu-memory-utilization`, `--max-model-len`, `--tensor-parallel-size`; SGLang-Omni keeps
`--mem-fraction-static`, `--context-length`, `--tp-size`. An engine, in this codebase, *is*
that vocabulary — it decides how every typed knob is spelled, how a pasted command is
recognised, and which flags are reserved. Duplicating `vllm` into `vllm` + `vllm_omni`
would duplicate all of it to change one thing: how the process is started.

What varies is the **invocation**, and behind it the serving surface. That is a second,
orthogonal axis.

## Decision

### 1. `modality` is a column, not an engine value

`models.modality` — enum `inference_modality` (`text` | `omni`), NOT NULL, default `text`.
Immutable after creation, alongside the engine, the server and the port: it decides the
image and the process, and a model that changes it is a different deployment.

The engine keeps its two values and its meaning: **the flag vocabulary**. Every typed knob,
every tier-1 spelling, every reserved flag is resolved from the engine alone, exactly as
before, in both modalities.

### 2. The invocation is resolved from the pair, in one table

| engine | modality | container command | human command |
|---|---|---|---|
| `vllm` | `text` | flags alone | `vllm serve …` |
| `vllm` | `omni` | flags alone **+ `--omni`** | `vllm serve … --omni` |
| `sglang` | `text` | `python3 -m sglang.launch_server …` | same |
| `sglang` | `omni` | `sgl-omni serve …` | same |

The asymmetry is upstream's, not ours, and it is the reason the pair is resolved by a table
rather than by composing two rules. **vLLM-Omni is a plugin of the same server**: it is
installed beside vLLM, matched to its major/minor, and activated by a marker flag on the
same `vllm serve` the official image already entrypoints — so the container command stays
flags-alone and gains `--omni`. **SGLang-Omni is a separate program**: its own package, its
own CLI, no `sglang.launch_server` to launch — so the whole invocation changes.

ADR-080 §4's rule that **the image's own ENTRYPOINT is never overridden** stands, and is
what makes the vLLM half work: `vllm/vllm-openai` entrypoints `["vllm", "serve"]`, so
flags-alone reaches the CLI that knows `--omni`.

### 3. `--omni` is a marker, therefore a reserved flag

`--omni` joins the reserved list: it may not be typed into tier 2, because a marker that
can be set in two places is a configuration that lies to one of them. On import it is
**consumed into `modality: omni`** rather than dropped with a notice — it carries meaning
the form has a field for, which is the tier-1 treatment, not the reserved one. The
export→import identity is therefore pinned over the four runtimes, not two.

### 4. No default image for an omni runtime — the override is required

Neither project publishes an image the platform can pin. vLLM-Omni ships as a package
released against the matching upstream vLLM minor and documents no image; SGLang-Omni
documents a community `:dev` tag, which is precisely what instance-config §4.1 forbids
defaulting to. So `modality: omni` **requires** an explicit image, refused at creation by
field name rather than defaulted to something that rots. Text keeps the per-engine,
per-architecture defaults ADR-080 chose.

This is the honest form of a fact the operator cannot avoid anyway: an omni runtime runs
from an image they built or vetted.

### 5. Readiness stops assuming a chat endpoint

The health path becomes a property of the runtime rather than a literal in the job. It
stays `/health` for all four today — both serving surfaces expose it — but a music model
serves `/v1/audio/speech` and an image model `/v1/images/generations`, so ADR-080 §4's
"then `/v1/models` answering with the served model" is **retired for every modality**: it
was described but never implemented (readiness has always been the container's health
check), and it is wrong for omni rather than merely unimplemented. Naming the path in the
runtime table is what lets a runtime that diverges be corrected in one line.

### 6. Not decided here

- **Multi-stage topologies.** vLLM-Omni's `--stage-id` / `--headless` /
  `--omni-master-address` and SGLang-Omni's router describe several coordinated processes;
  a model resource is one container, and stays one. Single-container multi-stage
  (`--worker-backend multi_process`) works today because it is one process tree.
- Modality inferred from the Hub metadata at search time (it is a field the operator sets).
- Omni-shaped affordances in the dashboard (audio preview, image output): the endpoint is
  exposed, the console stays text.
- Official images, when either project publishes one: a default can be added then without
  reopening this decision.

## Consequences

- `sgl-omni serve --model-path MiniMaxAI/MiniMax-Music3 --port 8000` — the command that
  motivated this — pastes into the import, lands as engine `sglang`, modality `omni`, model
  `MiniMaxAI/MiniMax-Music3`, and comes back out identical.
- The renderer grows one table and loses no branch it had; the flag vocabulary is resolved
  from the engine in one place, the invocation from the pair in another.
- Omni models cost one extra required field (the image) and one extra decision (the
  modality) at creation. Text models see nothing new: the column defaults, the UI defaults,
  existing rows are `text` by migration.
- The GPU guard, the HF cache, the token, the domains, the variables, the lifecycle jobs
  and the scale-to-zero opt-in apply unchanged — modality is invisible to all of them.

## Verification

- The four runtimes render their documented invocation: flags-alone, flags-alone + `--omni`,
  `python3 -m sglang.launch_server`, `sgl-omni serve`.
- The flag vocabulary follows the engine in both modalities: an omni SGLang model renders
  `--mem-fraction-static` / `--context-length` / `--tp-size`, an omni vLLM model renders
  `--gpu-memory-utilization` / `--max-model-len` / `--tensor-parallel-size`.
- Export → import is the identity over all four pairs, `--omni` included, with the managed
  flags dropped with their notices.
- `--omni` in tier 2 is refused by name; `--omni` in a pasted command sets the modality and
  appears in no flag list.
- Creating an omni model with no image is a validation error naming the `image` field;
  creating a text model without one still resolves the per-engine default.
- The modality is refused on update, like the engine, the server and the port.
