---
id: models
title: Models
icon: cpu
group: Run and debug
summary: Serving vLLM and SGLang on a GPU server — parameters, the command, the swap.
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
and the form fills itself — managed flags are taken over, each with a notice. **Show the
command** renders the exact line the container will run, by the same code that runs it.
On the model's page the command is shown masked, copy-ready; export → paste back is
guaranteed to reproduce the same configuration.

## Lifecycle and the swap

Starting loads the weights — minutes on first run, faster afterwards: the downloaded
weights live in one shared cache per server, which neither a stop nor a delete touches.
A stopped model is a state, not a failure: nothing restarts it behind your back, and its
endpoint, key and configuration wait unchanged for the resume.

Starting a model while another runs on the same GPU server is refused with the running
model's name — two models rarely fit one GPU. Confirming the dialog **is** the swap: one
job stops the neighbour, then starts yours, in that order. This is the daily loop on a
one-GPU machine — stop A, try B, come back to A — made one click each way.

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
