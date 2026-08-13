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

## Its data

A preview never mounts the data of the instance it hangs off (INV-010): its volumes are created empty, and they are removed with it. A stack whose review needs a real dataset opts in per volume, in the compose file of the repository:

```
volumes:
  pgdata:
    x-akerdock:
      preview_seed: clone
```

The preview's volume is then filled by copying the parent stack's volume, just before the first start of the service that mounts it, with the source mounted read-only — the instance you copy from is never written to. What you get is a file-level copy of a live database: PostgreSQL replays its journal at start, which is what review needs and not a consistency guarantee. The seed is refused on an `external:` volume and in raw compose mode, and it is skipped when the parent volume does not exist yet.

**The copy happens once, on an empty volume, and never again.** *Redeploy*, *Rebuild (no cache)* and *Recreate (apply config)* all keep the data already there — that is what protects the accounts and fixtures you created on the preview while reviewing it. A preview whose database is empty, stale, or was created before the dataset existed therefore does **not** catch up by redeploying, whatever the build does: **destroy it, then deploy it again**. The new instance starts on an empty volume and seeds from the current state of the parent stack.

Destroying one is in its **Danger zone**, with `previews:manage` — the containers, volumes, networks and routing go, the pull request stays open. From the pull request itself, when comment commands are enabled on the application, the first line of a comment does the same: `/destroy`, then `/deploy` to bring the preview back, plus `/rebuild` and `/keep`. The author needs write access on the repository, and a fork still waits for its approval. The CLI has `preview list`, `redeploy` and `keep` — destroying is not one of its verbs.

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
