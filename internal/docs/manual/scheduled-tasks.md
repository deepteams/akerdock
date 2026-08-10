---
id: scheduled-tasks
title: Scheduled tasks
icon: clock
group: Ship code
summary: Crons that run a command in your container or dispatch a GitHub workflow.
order: 8
permission: applications:update
---

## Declaring one

A task is a name, an action and a schedule — a cron expression or an alias such as `hourly` or `daily`. The action is one of two kinds: **run a command** inside the resource’s container (a specific component for a stack), so it sees the same environment and the same code as the app; or **dispatch a GitHub Actions workflow** on the application’s repository.

Typical command entries:

```
0 3 * * *     php artisan backup:clean
*/15 * * * *  node scripts/sync.js
daily         rails db:sessions:trim
```

- **Run now** triggers an execution outside the schedule.
- The execution history keeps status and output; failures can raise a notification.
- A command task does not run while the container is stopped — a scale-to-zero app is not a scheduler. A workflow task needs no container at all.

Run one on demand from your shell — the fastest way to find out why it fails:

```
akerdock app tasks list api
akerdock app tasks run backup-clean api
```

## Dispatching a GitHub workflow

GitHub documents its own `on: schedule` as best-effort: runs are delayed at busy hours, dropped under load, and silently disabled after 60 days without repository activity — the workflow’s own runs do not count. A workflow task moves the clock here instead: at each occurrence, AkerDock asks GitHub to dispatch the workflow, signed by the application’s GitHub App.

- Give the workflow file name (`build.yml`) — the workflow must declare `on: workflow_dispatch`.
- The Git ref is optional: empty falls back to the application’s branch, then the repository’s default branch.
- The GitHub App needs the **actions: write** permission. Apps created before this feature must have the added permission approved on GitHub — until then the run history shows GitHub’s `403`.
- A green run means GitHub **accepted the dispatch**; the build’s own outcome stays on GitHub’s Actions page.
