# ADR-080 — Models: inference is a first-class resource, and the section that shows it follows the GPU

- **Status**: Proposed
- **Date**: 2026-08-17
- **Related**: [ADR-079](ADR-079-gpu-is-an-observed-fact.md) (the observed GPU fact and the
  typed device request this consumes), [ADR-051](ADR-051-docker-runtime-adapter.md)–ADR-055
  (the runtime family every lifecycle below rides), [ADR-036](ADR-036-scale-to-zero-waker.md)
  (the waker, opted into), [ADR-042](ADR-042-application-access-protection.md) (the walls,
  available on top), [ADR-077](ADR-077-edge-relays-by-sni-authority-stays-home.md) (how a
  Spark-hosted endpoint answers on a public domain), [ADR-003](ADR-003-secrets-envelope-encryption.md)
  (the envelope the API key lives in), the databases family (§6 — the resource shape this
  deliberately mirrors), and the paused marketplace/addons work (which this does **not**
  resurrect — see the last section)
- **Related PRD sections**: §3 (servers), §6 (the resource-kind precedent), §26

## Context

The fleet now contains what it was always going to contain: a GPU machine (a DGX Spark,
GB10, 128 GB unified, arm64) whose purpose is serving models — vLLM and SGLang, both with
[official NVIDIA playbooks for this exact hardware](https://github.com/NVIDIA/dgx-spark-playbooks/tree/main/nvidia/vllm)
and a maintained support matrix. The platform can now reach it (ADR-076), publish it
(ADR-077), update it (ADR-078) and, with ADR-079, hand its GPU to a container. What it
cannot do is the workload itself.

The state of the art is uniformly bad in a specific way. No self-hosted PaaS in our class
has a first-class inference resource; the documented practice is hand-assembling a
`vllm serve` line — tensor parallelism, quantization, `max-model-len`,
`gpu-memory-utilization` — where [one wrong flag means OOM or wasted
hardware](https://llm-academy.dev/inference/deploy-vllm/), then pasting it into a generic
container form. And on this hardware the image itself is a parameter: stock images do not
always carry sm_121a, [the community maintains GB10-specific
builds](https://technotim.com/posts/vllm-gb10-docker/). "The exact serve command" is
precisely the kind of knowledge a typed resource encodes once, the way the databases family
encoded `initdb` and `pg_hba` once.

The platform has two shapes this could take, and one of them is paused for cause. The
marketplace/addons manifests (templating + structured config) could *describe* a vLLM
container — but not validate `mem_fraction_static` against `gpu_memory_utilization`, not
place by observed GPU, not know that readiness is `/v1/models` answering and not TCP-open.
Engine-specific semantics is what made databases first-class code rather than a template;
the same reasoning holds unchanged here.

## Decision

### 1. A new resource kind: `model`

`resource_type` gains `model`; a `models` table mirrors the databases shape (`id` referencing
`resources`, engine enum, image override, server consistency trigger):

- `engine inference_engine NOT NULL` — `'vllm' | 'sglang'`, an enum like `db_engine`, born
  with exactly the two engines the hardware's playbooks cover;
- `model_id` (the Hugging Face reference), `served_model_name` (what `/v1/models` reports);
- typed tuning: `quantization`, `max_model_len`, `tensor_parallel_size`, and ONE memory
  knob, `memory_fraction`, rendered as the engine's own flag (`--gpu-memory-utilization` on
  vLLM, `--mem-fraction-static` on SGLang) — one column, because it is one concept;
- `image`/`image_tag` override with a per-engine, per-architecture default (the GB10 case:
  an arm64 default that actually carries sm_121a), exactly as databases override theirs;
- the Hugging Face token rides the existing secret-variable machinery (INV-003: never argv,
  injected as `HF_TOKEN`), instance- or team-scoped since it is rarely per-model.

The typed set stops there on purpose, because the measured upstream surface forbids more:
`vllm serve` exposes on the order of **two hundred flags across fifteen config groups**
(model, load, cache, parallel, scheduler, LoRA, multimodal, speculative, compilation,
observability, …) and `sglang launch_server` is the same order of magnitude — both moving
monthly. Typing that surface is a treadmill; *not* exposing it sends the power user back to
a hand-rolled container. So the configuration is **two-tier**:

- **Tier 1 — the typed core above**: the knobs that decide whether the model runs at all on
  this hardware, validated, placed, and rendered per engine.
- **Tier 2 — `engine_flags`, a structured flag list**: ordered `{flag, value}` pairs (value
  optional for booleans), validated in *shape* (must look like `--a-flag`), rendered
  deterministically after the typed core, stored in the deployment snapshot so two
  deployments **diff by flag** — none of which a free-text string gives. This is the whole
  upstream surface, day one, including next month's flags: `--enable-prefix-caching`,
  `--kv-cache-dtype fp8`, `--schedule-conservativeness`, speculative configs, LoRA modules —
  whatever the operator's oignons require.
- **Reserved flags are refused by name**: what the platform itself manages — `--host`,
  `--port`, `--api-key`, `--model`/`--model-path`, `--served-model-name`, `--download-dir`,
  and the tier-1 knobs' own flags — cannot appear in tier 2; a flag war between the form
  and the platform is a config that lies to one of them.

The resource anchors in a project/environment **like every other resource** — RBAC, audit,
variables, domains, deployment history all come from that chain and are not reinvented.

### 2. The endpoint is protected by a managed native key

AkerDock generates an API key at creation, stores it enveloped, and passes it to the engine
itself (`--api-key`, which both engines implement): the OpenAI SDK ecosystem authenticates
with bearer keys, not with SSO redirects, so the native mechanism is the one clients can
actually speak. The key is readable under a dedicated `models:credentials` permission — the
databases-credentials precedent, not the ADR-075 write-only one: this key exists to be put
in a client's configuration, and reading it back is its purpose. The ADR-042 walls remain
available ON TOP for browser-facing uses; `noindex` is unconditional (an API answers
nothing an index should hold).

### 3. Model discovery is a live search, proxied by the control plane

The creation flow offers a free Hugging Face field backed by **live search against the Hub
API** — `GET /models/search?q=` on our API, the control plane querying
`huggingface.co/api/models` (filtered to text-generation) server-side: the browser never
talks to a third party, the existing SSRF discipline applies to a fixed host, the instance's
HF token lets gated models appear, and an offline instance degrades to the free field
instead of a broken widget. Search results fill `model_id`; they never constrain it.

### 3bis. The serve command is a first-class representation: shown, copied, pasted

The rendered command line — the exact `vllm serve …` / `sglang launch_server …` the
container will run — is not an internal detail, it is the lingua franca of this ecosystem:
every playbook, blog post and forum answer trades in it. So the platform speaks it, both
ways, through two pure endpoints:

- **Export** — the form (and every existing model's page) shows the final command behind a
  button, copy-ready. It is produced by THE renderer, the same code the deployment runs —
  never a UI approximation that drifts. The API key is masked in the display
  (`--api-key ****`), revealed inline only under `models:credentials`; the HF token never
  appears at all (it is env, INV-003).
- **Import** — a paste field parses a command back into the two tiers: shell-words
  tokenization, the engine and `model_id` recognized from the invocation, tier-1 flags
  mapped onto their typed fields (`--gpu-memory-utilization`/`--mem-fraction-static` →
  `memory_fraction`, `--tp-size`/`--tensor-parallel-size` → `tensor_parallel_size`, …),
  everything else preserved **in order** into `engine_flags`. Reserved flags in the paste
  (`--host`, `--port`, `--api-key`, `--download-dir`) are dropped with a visible notice —
  the platform manages those, and silently honouring them would desynchronize the managed
  config; nothing else is ever silently discarded.

The invariant that makes this trustworthy is **round-trip identity**: export → import →
the same configuration, field for field, flag for flag — pinned by a unit test, because a
representation that loses information on the way back is a trap, not a feature. This is
also the switching story the workflow of §5 wants: keep two commands in a note, paste the
one you are testing today.

### 4. Lifecycle, placement, health — the databases pattern, GPU-aware

Provision/start/stop/restart/delete mirror the database job family on the agent channel.
The create carries ADR-079's device request, `IpcMode: host`, shm sizing, and the ulimits
the engines' own examples mandate (SGLang documents `memlock=-1`, `stack=67108864`).

**The container input contract is per-engine, and the entrypoint is never overridden.**
The two official images disagree by construction: `vllm/vllm-openai`'s ENTRYPOINT *is* the
server, so the container command is the **flags alone** (`--model <id> …` — never a
`vllm serve` prefix, which would be handed to the server as a bogus argument);
`lmsysorg/sglang` ships no serving entrypoint, so the container command is the **full
invocation** (`python3 -m sglang.launch_server --model-path <id> …`). The renderer
therefore produces an engine-agnostic argv of flags, and the per-engine contract decides
what wraps it. The image's own entrypoint is deliberately respected rather than replaced:
the GB10 community images this hardware depends on wrap environment setup in theirs, and
bypassing it would break exactly the images the default points at. A custom image must
honour its engine's official contract — that is what "an image for engine X" means. The
§3bis export always displays the *human* invocation (`vllm serve --model … `), and the
import strips any recognized invocation prefix, vLLM's positional model form included. Placement
requires an observed GPU (ADR-079's guard). Readiness is the engine's own signal — `/health`
then `/v1/models` answering with the served model — with a startup budget in minutes, not
seconds: weight loading is the cost of the workload and the health check must not declare a
loading model dead. `/metrics` (both engines speak Prometheus) is recorded as the scrape
endpoint. One **server-scoped HF cache volume** (`akerdock-hf-cache`), mounted in every
model container on that server: weights are tens of gigabytes, and per-instance caches
would multiply exactly the thing the machine has least of.

### 5. Stop, swap, resume — the workflow is first-class, not an accident

The daily loop on a one-GPU machine is: stop model A to free the unified memory, run model
B for a while, come back to A. Three properties make it safe:

- **An explicit stop is a state, never a defect.** Desired-state semantics as everywhere
  else (ADR-062's stance): a model the operator stopped is not "down", it is *stopped*,
  and nothing — reconciler, health check, redeploy of a neighbour — restarts it behind
  their back.
- **A restart reloads, it never re-downloads.** The weights live in the server-scoped HF
  cache volume (§4), which a `stop` does not touch: resuming model A costs the minutes of
  loading weights into memory, not the hours of pulling tens of gigabytes again. The
  config, the API key and the endpoint are unchanged across the gap.
- **Starting into an occupied GPU warns, with the swap one click away.** Two models sized
  at ninety percent of memory cannot run together, but the platform cannot know every
  legitimate combination (two small models can). So starting a model while another is
  RUNNING on the same GPU server is a **soft guard**: a confirmation naming the running
  model and its memory fraction, with "stop <A> and start <B>" as the offered action — the
  operator's actual intent, one click instead of two pages.

Changing any engine parameter recreates the container on the next start (serve flags are
read once, at process start — the §1.4 static-config reasoning, applied to a process).

### 5bis. Scale-to-zero: opt-in, off by default

A sleeping model frees what the Spark is shortest of — unified memory — but a wake replays
minutes of weight loading behind ADR-036's waiting page. That trade belongs to the
operator, per model, and defaults to off — in particular it must never surprise the manual
stop/swap loop above. When opted in, the existing waker machinery applies unchanged.
(vLLM's own `--enable-sleep-mode` — freeing weights while the process stays up — is noted
as a possible finer-grained rung and deliberately not decided here.)

### 6. The Models section follows the GPU

The dashboard gains a **Models** section, visible when the team has at least one server
with an observed GPU (or any existing model resource — never orphan what exists). It is a
*view*, not an anchor: a transverse list — engine, model, server, GPU, status, endpoint —
and a creation flow that starts from the model (engine → HF search → GPU server → typed
parameters → project/environment last, defaulted). The resource stays project-anchored
(§1); the section reorders the questions in the order a model operator actually asks them.
Permissions: `models:read/create/update/delete/lifecycle` + `models:credentials`, catalogued
in the rbac-matrix; absent from the reviewer path (ADR-059 untouched).

### What this ADR does not decide

No multi-node serving (dual-Spark tensor parallelism over RDMA is its own project). No
further engines (Ollama, TGI — the enum extends when the playbook matrix does). No
replicas or autoscaling. No curated preset catalog (the HF search replaced it; presets can
return as annotations on search results if demand shows). No GPU toggle for ordinary
applications (ADR-079's parked question). And the marketplace stays paused: nothing here
generalizes to a manifest, for the reasons stated in the context.

## Verification

- Store/spec: the `model` kind round-trips; placement on a GPU-less server refused by name;
  the memory knob renders as the right flag per engine (unit-tested command rendering, both
  engines, quantization and tier-2 flags included, ordered and quoted); reserved flags in
  `engine_flags` refused by name; the soft start-guard fires exactly when another model is
  running on the same GPU server.
- Jobs: lifecycle family on the fake runtime asserts device request, IpcMode, shm,
  ulimits, cache volume mount, HF token as env never argv, and the per-engine container
  command contract — flags-only for vLLM (no `vllm serve` prefix), full invocation for
  SGLang, image entrypoint untouched in both; readiness gate tolerates a loading window
  and fails a dead engine.
- API: key generated, enveloped, revealed only under `models:credentials`; HF search proxied,
  filtered, degrading offline; audit rows on reveal; command export produced by the
  deployment's own renderer with the key masked; command import maps tier-1 flags, keeps
  tier-2 order, drops reserved flags with a notice — and **round-trips**: export → import
  is the identity on the configuration, pinned by a unit test.
- UI: section gated on the observed-GPU fact; karma coverage per the existing thresholds.
- The E2E journey is unchanged (no GPU in CI — ADR-026's one-journey rule stands); a manual
  Spark checklist joins §27's manual validations.
