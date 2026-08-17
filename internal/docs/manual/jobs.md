---
id: jobs
title: Jobs
icon: list-checks
group: Run and debug
summary: Where an asynchronous operation went, and why it stopped.
order: 7
permission: deployments:read
links:
  - label: Jobs
    route: /jobs
---

## Reading a job

Deployments, backups, cleanups and cross-server operations run as jobs. The Jobs page lists them with their state; a job’s page shows its steps, its result or its error, and any remnants left on the server when it failed mid-way.

A job that failed every retry is **dead-lettered** — kept rather than dropped, so nothing silently vanishes. Retrying or forgetting one is an administrator action (`jobs:manage`).
