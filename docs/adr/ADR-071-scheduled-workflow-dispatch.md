# ADR-071 — A scheduled task can fire a GitHub Actions workflow, because GitHub's own cron cannot be trusted

- **Status**: Accepted
- **Date**: 2026-08-10
- **Extends**: scheduled tasks (§192, spec amendment #15) with a second task kind — nothing
  the existing kind decided (cron grammar, scheduler ownership of `next_run_at`, overlap and
  missed-run policies, execution history, failure events) is touched
- **Related**: [ADR-002](ADR-002-postgresql-queue.md) (the queue that runs the occurrence);
  the GitHub App integration whose installation token signs the dispatch is specified in
  [git-webhook-protocols.md](../specs/git-webhook-protocols.md) (§2.1 manifest, §2.2 tokens)
- **Related PRD sections**: §5.7, §11, §24.3, §26

## Context

Teams that build on GitHub Actions and deploy with AkerDock keep one scheduling need on
GitHub's side: "run the build workflow every day at 06:00". GitHub's `on: schedule` is the
obvious tool and it is, by GitHub's own documentation, not a clock: scheduled runs "can be
delayed during periods of high loads" and "if the load is sufficiently high enough, some queued
jobs may be dropped"; on a public repository the schedule is silently disabled after 60 days
without repository activity — and the workflow's own successful runs do not count as activity,
so a healthy pure-cron workflow is exactly the one that dies. The run is attributed to whoever
last touched the cron line, so an offboarded account can stop a schedule too. The ecosystem's
standard mitigation is to add `workflow_dispatch` to the workflow and move the clock somewhere
reliable.

AkerDock already owns a reliable clock. The scheduled-tasks vertical (§192) has a leader-elected
scheduler that fires within 30 seconds of the due minute, an execution history in which a
missed or skipped occurrence is a **row with a reason** rather than silence, and a
`scheduled_task.failed.v1` event that is classified critical by the notification pipeline. And
AkerDock already owns the credential: the GitHub App integration mints installation tokens
scoped to the application's repository for check runs, deployments and PR comments
(`internal/githubapp`). What is missing is one column's worth of vocabulary — a scheduled task
can only say "exec this in the container", not "tell GitHub to dispatch this workflow".

The distinction matters because the container is the wrong place from which to trigger a build:
the job would need a GitHub credential inside the runtime environment (INV-003 says secrets do
not wander), and an app scaled to zero or mid-redeploy has no container to exec into — while a
workflow dispatch needs no container at all.

## Decision

### 1. `kind` on the task: `container_command` (default) or `github_workflow`

One table, one scheduler, one history, two kinds. The alternative — a separate
`workflow_schedules` table with its own endpoints — would duplicate the cron grammar, the
policies, the history pagination, the events and the UI tab to express a thing that differs in
exactly one step: what happens when the occurrence fires. The enum keeps every guarantee the
operator already learned (aliases, timezone, `overlap_policy`, `missed_run_policy`,
`timeout_seconds`, run-now, skip rows) automatically true for the new kind.

`kind` is **immutable after creation**. The two kinds have disjoint required fields; a PATCH
that flips one into the other is a delete-and-create wearing an update's clothes, and would
inherit a history whose rows mean something else.

Per-kind columns, enforced by CHECK constraints rather than handler discipline:

- `container_command`: `command` required, `container` optional — unchanged.
- `github_workflow`: `workflow_file` required (the file name under `.github/workflows/`, or a
  numeric workflow id — GitHub's endpoint accepts both); `workflow_ref` optional;
  `workflow_inputs` optional (a string→string map, GitHub's own constraint). `command` and
  `container` must be NULL: there is nothing to exec.

### 2. The dispatch is signed by the application's GitHub App, scoped to its repository

A `github_workflow` task can only be created on an application whose git source is a GitHub
App (`git_sources.kind = 'github_app'`); this is validated at creation with an error naming
the fix, and re-checked at fire time. The job resolves application → git source → GitHub App →
repository, decrypts the App's private key (`Keyring`, AAD-bound as everywhere else), mints an
installation token restricted to that one repository — the same chain and the same restriction
as `githubForApplication` and the deploy job's `installGithubToken` — and POSTs
`/repos/{full_name}/actions/workflows/{workflow_file}/dispatches`.

The App manifest gains `actions: write`, the permission `workflow_dispatch` requires.
**Existing installations must approve the added permission on GitHub** (GitHub emails the
installer and shows a banner on the installation page); until approved, the dispatch fails
with GitHub's own `403 Resource not accessible by integration`, which lands verbatim in the
execution history — the failure names its fix, it does not hide.

The ref is resolved at fire time, first match wins: the task's `workflow_ref`, else the
application's `git_branch`, else the repository's default branch. A task with none of the
three fails with a reason, it does not guess `main`. Resolution at fire time rather than at
creation means a repository whose default branch is renamed keeps working.

### 3. A dispatch accepted is the success; the workflow's outcome stays GitHub's

GitHub answers `204 No Content` and reveals no run id. The execution row therefore records
that GitHub **accepted the dispatch** (`succeeded`, no exit code, an output line naming the
workflow, the ref and the repository), not that the build passed — the description says so.
Correlating the dispatch to its `workflow_run` and importing that run's conclusion would need
the `workflow_run` webhook event, a correlation heuristic GitHub does not offer an exact form
of, and a second status vocabulary; it is out of scope here and can be a later ADR without
this one deciding anything against it.

Failure semantics follow the existing invariant: a dispatch refused by GitHub (4xx/5xx), a
missing installation, an unresolvable ref — all are **results** written to the history and
published as `scheduled_task.failed.v1`, never errors returned to the queue. Retrying a
dispatch behind the operator's back is precisely what the existing kind's comment forbids for
commands, and a dispatch is worse: it is not idempotent, each one starts a build. For the same
reason, once the dispatch has been accepted, a subsequent bookkeeping failure is logged and
swallowed rather than allowed to re-run the job: **at most one dispatch per occurrence**.
`timeout_seconds` bounds the HTTP call.

### 4. No new permission, no new endpoints, no new events

Creating and running either kind stays behind `applications:exec`; dispatching the repo's
build workflow is within the blast radius that permission already grants (a member who may
exec arbitrary commands in the container may certainly ask GitHub to build). The six
scheduled-task endpoints, the two events (`scheduled_task.succeeded.v1` / `.failed.v1`), the
queue type (`scheduled_task.run`), the lock key and the scheduler are all unchanged — the
kind is a branch inside the job handler, plus fields on the schemas.

The outbound call follows the house conventions for GitHub traffic: `internal/githubapp`'s
client (per-app `api_url`, GHES-ready), bounded by `context.WithTimeout`, **not** wrapped in
`safedial` — a GHES `api_url` is operator-configured infrastructure, the category safedial's
own doc comment excludes.

## Consequences

- Migration: `task_kind` enum, four columns on `scheduled_tasks`, `command` drops NOT NULL,
  CHECK constraints tie each column to its kind. Existing rows become `container_command`
  untouched.
- `internal/githubapp` gains `DispatchWorkflow`; `Manifest` gains `actions: write`; the job
  struct gains the `Keyring` it never needed before.
- OpenAPI: `TaskKind` schema; `command` leaves the `required` list of `ScheduledTask` and
  `ScheduledTaskCreate` (per-kind requirement is the handler's, backed by the CHECKs);
  `workflow_file` / `workflow_ref` / `workflow_inputs` appear on the three task schemas. The
  generated Go and TypeScript follow (`make generate`).
- The CLI's `app tasks list` shows the kind; `tasks run` already does the right thing (same
  run-now path). The dashboard's tasks tab gains the kind choice and the workflow fields.
- PRD §26.2 gains the scheduled-tasks row that was missing since amendment #15 — this ADR is
  its proof for both kinds.

## Alternatives rejected

- **A standalone "workflow schedules" resource**: rejected — see §1. Every property it needs
  already exists on scheduled tasks; a second resource is a second place for the same bugs.
- **Triggering via repository_dispatch**: rejected. It targets the repository, not a workflow;
  every consuming workflow must subscribe to a custom event type, which moves configuration
  into YAML the platform cannot validate. `workflow_dispatch` names the workflow explicitly
  and is the documented mitigation GitHub's own community converged on.
- **A PAT instead of the App**: rejected. The App is already installed, already scoped, its
  key already encrypted; a PAT is a personal credential with a person's lifespan (the exact
  attribution failure `on: schedule` suffers from) and AkerDock deliberately stores none for
  GitHub.
- **Polling the workflow run to completion**: rejected for this ADR — see §3. The execution
  would hold a queue slot for the build's whole duration to learn a fact GitHub will happily
  push later.
- **Exec-ing `gh workflow run` in the container**: rejected. It requires the `gh` binary and
  a credential inside the runtime image (INV-003), and fails exactly when the container is
  absent — the situation a build trigger must survive.

## Verification

Unit tests, per the project's pyramid (ADR-026/028):

- `DispatchWorkflow` hits the documented path with `{ref, inputs}` and treats `204` as
  success (httptest).
- The job handler, `github_workflow` kind: a `204` closes the execution `succeeded` with no
  exit code and publishes `scheduled_task.succeeded.v1`; a `403` closes it `failed` with
  GitHub's body as output and publishes `.failed.v1`; a missing installation and an
  unresolvable ref fail with their reasons; **none of these return an error to the queue**.
- Ref resolution order: task ref → application branch → repository default branch → failure.
- Create validation: `github_workflow` without `workflow_file` is 422; with `command` or
  `container` is 422; on an application without a GitHub App source is 422 naming the fix;
  `container_command` without `command` is 422 (was: schema-level required).
- Update validation: patching `command` on a `github_workflow` task (and the converse) is
  422; `kind` is not patchable.
- The manifest declares `actions: write`.
