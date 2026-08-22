---
id: models
title: Models
icon: cpu
group: Run and debug
summary: Serving vLLM and SGLang, text or omni, on a GPU server — parameters, the command, the swap.
order: 4
permission: models:read
gates:
  creating-one: models:create
  lifecycle-and-the-swap: models:lifecycle
  the-api-key: models:credentials
links:
  - label: Models
    route: /models
---

A model is an inference server — vLLM or SGLang — running on a server whose validation
observed a GPU. It answers an OpenAI-compatible API on the server's LAN address, protected
by a managed API key the engine itself enforces — and, when you give it a public domain
(optional, in its settings), on HTTPS through the server's proxy, across the edge relay
when the server is LAN-only. The domain applies immediately, no restart; search engines
are always told not to index it. The Models section lists every model of the
team across projects; the resource itself lives in a project and environment like everything
else.

## Creating one

Pick the engine, then the model: the Hugging Face field searches the Hub as you type
(gated models appear when the instance carries a token) — the search suggests, it never
constrains. Pick a GPU server: only servers with an observed GPU are offered; a server whose
card lacks the NVIDIA container toolkit counts as GPU-less until it is installed and the
server re-validated.

The typed fields are the knobs that decide whether the model runs at all: quantization,
max model length, the memory fraction (rendered as each engine's own flag), tensor
parallelism, an image override. Everything else — the full upstream flag surface — goes in
the **engine flags** list, in order. Platform-managed flags (`--port`, `--api-key`,
`--host`, `--download-dir`, `--hf-token`) are refused there by name: the platform sets them.

Two shortcuts speak the ecosystem's language. **Paste a command** from any playbook or blog
and the form fills itself — managed flags are taken over, each with a notice, and the
invocation names the runtime (`vllm serve`, `vllm-omni serve`, `python3 -m
sglang.launch_server`, `sgl-omni serve`). **Show the command** renders the exact line the
container will run, by the same code that runs it.
On the model's page the command is shown masked, copy-ready; export → paste back is
guaranteed to reproduce the same configuration.

## Text or omni

**Modality** is the second choice, beside the engine. `text` is the engine's own server, the
default, and what you want for an LLM. `omni` is its sibling for speech, audio, image and
video models — vLLM-Omni beside vLLM, SGLang-Omni beside SGLang — the runtimes that serve
`MiniMaxAI/MiniMax-Music3`, `Qwen3-TTS` or `Qwen-Image` on `/v1/audio/speech` and
`/v1/images/generations` instead of `/v1/chat/completions`.

The engine still decides how every parameter is spelled: an omni SGLang model gets
`--mem-fraction-static` and `--context-length`, an omni vLLM model `--gpu-memory-utilization`
and `--max-model-len`. What the modality changes is the program started — `sgl-omni serve`
for SGLang-Omni, which is a separate package, and the `--omni` marker for vLLM-Omni, which is
a plugin of the same server. `--omni` therefore belongs to the modality, not to the flag
list, and is refused there like any managed flag; a pasted command carrying it fills the
field instead.

An omni model **requires an image**: neither project publishes one this platform could pin,
so the field is not optional there and creation refuses without it. Build it from the
runtime's own installation instructions and pin it — a `:dev` tag moves under you.

The modality, like the engine, the server and the port, is fixed at creation.

> **Note** — One model is one container. The multi-process topologies these runtimes
> document (`--stage-id`, `--headless`, a separate router) coordinate several servers and are
> not something a model resource can express; a single-container multi-stage setup works
> because it is one process.

## Lifecycle and the swap

Starting loads the weights — minutes on first run, faster afterwards: the downloaded
weights live in one shared cache per server, which neither a stop nor a delete touches.
A stopped model is a state, not a failure: nothing restarts it behind your back, and its
endpoint, key and configuration wait unchanged for the resume.

Starting a model while others run on the same GPU server adds up what they each declare in
**Memory fraction** and compares it with the card. If it fits — two models at 45 % and
40 %, say — the start simply proceeds: running several models on one GPU is a normal thing
to do, not an exception to argue for.

If it does not fit, the refusal shows the arithmetic — each running model with what it
claims, yours, and the total — and offers two ways forward. **Stop them and start this
one** is the swap: one job stops the neighbours, then starts yours, in that order; this is
the daily loop on a one-GPU machine, stop A, try B, come back to A, made one click each
way. **Start it alongside** overrides the sum, because the fractions are a declaration:
a quantized model often uses far less than its flag reserves, and if `nvidia-smi` tells you
there is room, you are the one who knows. The worst case is an out-of-memory failure while
the weights load, which the start now catches quickly rather than waiting out.

A model that declares no memory fraction counts as 90 % — what both engines take when the
flag is absent. If one unconfigured model seems to fill the card on its own, that is why:
give it an explicit fraction and the arithmetic gets its room back.

Parameter changes apply at the next start: serve flags are read once, when the engine
process starts.

One lifecycle operation runs at a time: a second start while one is queued is refused, and
the page shows the active job — follow it, or cancel it. A job that has not started stops
at once; one already running is asked, stops at its next checkpoint, and leaves no engine
loading behind it. A start that never becomes ready gives up on its own: a container that
keeps being restarted is crash-looping, not loading, and the job says so with the engine's
last lines rather than waiting out the fifteen-minute readiness budget. It is then stopped
rather than left to consume the GPU, and it is **not** retried — fix the configuration and
start again. The **Logs** card is the engine's own console (the weight download narrates
itself there);
switch on *Follow* while a start runs. **Environment variables** use the same machinery as
every resource — shared references resolve, server variables inherit, and your variable
wins over anything managed, `HF_TOKEN` included; they reach the engine at the next start.

## The API key

Generated at creation, stored encrypted, passed to the engine's own `--api-key` — the
mechanism OpenAI SDK clients actually speak. Reveal it from the model page (it exists to be
put in a client's configuration); every reveal is audited. The command view keeps the key
masked unless you reveal it explicitly.
