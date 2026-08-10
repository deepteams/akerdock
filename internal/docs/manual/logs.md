---
id: logs
title: Logs
icon: scroll-text
group: Run and debug
summary: Runtime logs and build logs, in the dashboard or in your terminal.
order: 0
permission: logs:read
gates:
  build-logs: deployments:read
  the-events-page: audit:read
---

## Runtime logs

The Logs tab streams the container’s output, with a component selector for a compose stack and a configurable number of lines. Timestamps follow the target server’s timezone, and HTML in a log line is rendered inert rather than interpreted.

The same thing from your shell:

```
akerdock app logs api -f            # follow
akerdock app logs api -n 500        # last 500 lines
akerdock app logs api --pr 42       # the preview of PR 42
akerdock app logs api --deployment  # the latest build log instead
```

> **Note** — Logs are an **application** verb: there is no `akerdock db logs` or `akerdock svc logs`, because no endpoint serves them — a stack’s container is read through the application that owns it, or from the Components tab here.

## Build logs

A deployment’s log is separate from the container’s: open the run from the Deployments tab. It stays readable after the fact, which is what you want when a nightly deploy failed at 3am.

## The Events page

Events is the live feed of the team: status changes, jobs, deployments as they happen. Useful on a second screen while something is shipping.
