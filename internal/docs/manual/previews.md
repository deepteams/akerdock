---
id: previews
title: Pull request previews
icon: git-branch
group: Ship code
summary: One live instance per PR, its own variables, its TTL and fork approval.
order: 5
permission: previews:read
gates:
  turning-them-on: applications:update
  acting-on-a-preview: previews:manage
  pull-requests-from-a-fork: previews:manage
---

## Turning them on

1. Connect the repository through a **GitHub App** (or configure the application’s webhook for GitLab/Gitea).
2. Ask an administrator for a **wildcard DNS record** — `*.preview.example.com` pointing at the server.
3. In Settings → **PR previews**, enable them and set the host template, for example `pr-{{pr_id}}.preview.example.com`.
4. Fill the **previews** set of environment variables — nothing is inherited from production.

- **Max concurrent** caps how many PR instances may live at once.
- **TTL** destroys an idle preview after N minutes; the **Keep** action re-arms it when you still need it.

## Day to day

- A PR opened redeploys on each new commit; closing or merging it destroys the instance.
- The Previews tab lists the open pull requests of the repository — a PR opened before you enabled the feature can be deployed by hand from there.
- A preview has its own **Logs**, **Terminal**, **Environment variables** and **Storages** tabs, plus *Redeploy*, *Rebuild (no cache)* and *Recreate (apply config)*.
- Its URL is never indexable by search engines.

Which PRs are live, and where — without opening the dashboard:

```
akerdock app preview list api
```

## Acting on a preview

Driving one — `--pr` carries the PR number everywhere:

```
akerdock app logs api --pr 42        # its logs
akerdock app env list --pr 42 api    # its own set of variables
akerdock app preview redeploy --pr 42 api
akerdock app preview keep --pr 42 api    # re-arm the TTL while you debug
```

> **Note** — Approving a fork’s preview is **not** a CLI verb: authorising code you have not written to run is project governance, and it stays here where the context is. `keep` is on the other side of that line — holding a preview alive while you debug on it is debugging.

## Pull requests from a fork

A fork’s PR is not deployed automatically: its branch is code you have not written, and deploying it would run it next to your team’s values. Someone with `previews:manage` **approves** the preview explicitly.

> **Warning** — Even approved, a fork preview never resolves `{{team.*}}`, `{{project.*}}` or `{{environment.*}}` references, and receives no server-level variables. Approving says "run this code", not "hand it our secrets".

## Sharing a preview with a reviewer

The **reviewer** role exists for exactly this: someone invited as a reviewer sees the path down to the previews and their URLs, and nothing else — no logs, no variables, no deploy buttons. The same list answers in a terminal, `akerdock app preview list <app>`, with the same narrow reach.
