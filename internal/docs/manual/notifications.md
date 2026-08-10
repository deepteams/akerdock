---
id: notifications
title: Notifications
icon: bell
group: Run and debug
summary: Channels, routing rules, and the events you can subscribe to.
order: 5
permission: notifications:read
gates:
  channels: notifications:manage
links:
  - label: Notifications
    route: /notifications
---

## Channels

A channel is a destination — email, Slack, Discord, Telegram, or a custom webhook — with a **Send a test message** button so you find out it is misconfigured now rather than during an incident.

## What you can be told about

- Deployments — succeeded, failed, cancelled.
- Previews — created, updated, expiring soon, deleted.
- Backups — failed or partial, and restore drills that failed.
- Servers — unreachable, recovered, disk cleanup results, certificates expiring.
- Scheduled tasks — succeeded or failed; uptime checks — failed or recovered.
- Jobs that ended up dead-lettered.

Each event is toggled per channel, and **routing rules** narrow a channel further — so the on-call channel gets failures and the team channel gets the rest, instead of everyone muting everything.
