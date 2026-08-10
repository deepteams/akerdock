---
id: deployments
title: Deployments
icon: hammer
group: Ship code
summary: Auto-deploy, build logs, cancelling, rolling back, deploying from CI.
order: 4
permission: deployments:read
gates:
  cancelling-and-the-queue: deployments:cancel
  trigger-follow-and-cancel-without-leaving-the-terminal: applications:deploy
  rolling-back: applications:deploy
  deploying-from-your-own-ci: applications:deploy
---

## What triggers a deployment

- The **Deploy** button, `akerdock app deploy run`, or the API.
- A **push** on the configured branch, when auto-deploy is on — through the GitHub App or the application’s own webhook endpoint.
- A **pull request** event, when previews are on.
- A **scheduled** or external call to the deploy webhook from your CI.

A commit whose message contains `[skip ci]` or `[skip cd]` is ignored. **Watch paths** narrow auto-deploy to pushes touching certain files — the setting a monorepo cannot live without.

## Following a build

The Deployments tab lists every run with its status — queued, running, finished, failed — and streams the build log live. Reconnecting resumes the stream where it stopped rather than replaying it from the top.

## Cancelling and the queue

- A deployment records the **configuration changes** it carries, not just the commit — useful when the code is identical and the behaviour is not.
- A queued or running deployment can be **cancelled**.
- Each server has a concurrency limit and a queue; a burst of pushes queues rather than crushing the box.

## Trigger, follow and cancel without leaving the terminal

```
akerdock app deploy run api -f     # trigger, and follow the build log
akerdock app deploy run api --skip-build   # apply the config, no rebuild
akerdock app deploy list api       # the history, newest first
akerdock app deploy cancel <deployment-uuid>
```

> **Note** — A compose stack deploys the same way — `akerdock svc deploy run|list|cancel`. `-f` rides the same stream the Deployments tab reads, so the terminal and the browser show the same lines.

## Rolling back

Rollback redeploys a previously deployed image by its digest, from the deployment history. It is the fast path back; the durable fix is still a commit.

```
akerdock app deploy rollback api            # the previous deployment
akerdock app deploy rollback api --to <uuid>  # a chosen one
```

> **Note** — Rollback is an **application** verb only: no such endpoint exists for a compose stack, which is why `akerdock svc deploy` stops at `run`, `list` and `cancel`.

## Deploying from your own CI

Build and push wherever you like, then ask AkerDock to pull and restart:

Deploy webhook — a token with the deploy scope is enough.

```
curl -X POST "https://<instance>/api/v1/deploy?uuid=<app-uuid>" \
  -H "Authorization: Bearer $AKERDOCK_TOKEN"

# several resources at once, and a forced rebuild
curl -X POST "https://<instance>/api/v1/deploy?uuid=<a>,<b>&force=true" \
  -H "Authorization: Bearer $AKERDOCK_TOKEN"
```

The **Webhook** tab of the application creates the endpoint your Git host calls, with its secret and signature verification.
