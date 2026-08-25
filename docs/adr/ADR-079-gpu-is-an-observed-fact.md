# ADR-079 — The GPU is an observed fact of a server, carried typed to the container

- **Status**: Accepted
- **Date**: 2026-08-17
- **Related**: [ADR-052](ADR-052-agent-command-channel.md)/[ADR-054](ADR-054-agent-host-ops.md)
  (the typed command channel this rides — unchanged, which is the point),
  [ADR-051](ADR-051-docker-runtime-adapter.md) (the runtime adapter whose producers gain the
  fields), [ADR-080](ADR-080-models-first-class-inference.md) (the first — and in v1 only —
  consumer), the build-architecture guard (`deploymentrun`, an image for another CPU is
  refused by name — the precedent this ADR's placement guard mirrors)
- **Related PRD sections**: §3.1, §20.1, §22.4, §26

## Context

The platform has no notion of a GPU. Not a `DeviceRequest` anywhere, no `--gpus` on any
`docker run`, no shared-memory sizing, and nothing at validation that would notice an
accelerator. Meanwhile the fleet this product actually runs on grew one: a DGX Spark — a
GB10 Grace Blackwell SoC, arm64, 128 GB of unified memory — whose entire reason to be
managed is GPU workloads (ADR-080).

The wire, it turns out, is already done. `agentwire.ContainerCreateParams` carries the
Docker SDK's `*container.HostConfig` **verbatim** (ADR-052/054), and the agent hands it to
the daemon untouched: `DeviceRequests`, `ShmSize` and `IpcMode` transit today. What is
missing is everything around the wire — nobody detects a GPU, nobody records one, no
producer fills those fields, and nothing refuses a GPU workload placed on a server that has
none.

The design question is whether GPU-ness is *declared* (a checkbox on the server) or
*observed*. The platform already answered it for every comparable property: OS, architecture
and Docker version are facts the validation records (`recordFacts`), never claims the
operator makes — a checkbox can lie, `nvidia-smi` cannot.

## Decision

### 1. Validation observes the GPU and records it as facts

A new validation step probes, over the existing SSH session: the NVIDIA container runtime
(`docker info` runtimes) and the accelerator itself (`nvidia-smi --query-gpu=name,memory.total`).
Two nullable columns on `servers` — `gpu_name`, `gpu_memory_mb` — recorded with the other
facts, shown on the server page, `NULL` meaning "none observed". A server with a GPU but no
NVIDIA runtime is recorded GPU-less **with a step warning naming the fix** (install
`nvidia-container-toolkit`): a device the daemon cannot hand to containers is not a device
the platform can schedule onto. Unified-memory machines (the Spark reports the shared pool)
record what `nvidia-smi` says — the number is for the operator and the placement guard, not
an allocator.

### 2. Producers fill what the wire already carries

A resource whose configuration demands the GPU (ADR-080's models; nothing else in v1) gets,
on its `ContainerCreate`: `DeviceRequests` asking for all GPUs (`count: -1`,
capability `gpu` — one accelerator per server is the fleet this serves; splitting is not
decided here), `IpcMode: host` and a configurable `ShmSize` — the two flags every inference
runtime documents as required before anything else. No agent change, no wire change, no new
command: the fields were always in the payload.

### 3. Placement is guarded by the observed fact

Creating or moving a GPU resource onto a server whose `gpu_name` is `NULL` is refused at the
API with a named error — the mirror of the build-architecture guard, and for the same
reason: a workload that starts and dies with "no CUDA device" on someone else's schedule
explains nothing, a refusal at placement time explains everything.

### What this ADR does not decide

No GPU toggle on ordinary applications (the mechanism is ready; deciding *who else* may ask
is ADR-080's successor's problem, driven by demand). No `gpus:` support in compose files
(the allowed-key list stays as it is). No fractional or multi-GPU scheduling, no MIG, no
per-container memory carving on unified-memory machines — one server, one accelerator, whole,
matching the hardware this was built for.

## Verification

- Unit: detection parses runtime list and `nvidia-smi` output (present, absent, toolkit
  missing → warning), facts recorded and reported; the scripted-SSH validation harness gains
  a GPU host profile.
- Unit: a GPU-demanding create carries `DeviceRequests`/`IpcMode`/`ShmSize` on the typed
  command (fake runtime asserts the HostConfig), and a GPU-less server refuses placement
  with the named error.
