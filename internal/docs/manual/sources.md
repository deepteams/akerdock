---
id: sources
title: Sources and credentials
icon: git-branch
group: Instance administration
summary: GitHub Apps, deploy keys, registries, DNS-01 and S3 storages.
order: 1
permission: sources:read
links:
  - label: Sources
    route: /sources
---

## The five families

| Tab | What it unlocks |
| --- | --- |
| GitHub Apps | Private repos, auto-deploy on push, PR previews and status comments |
| Private keys | Deploy keys for a private repo, and the SSH keys servers are reached with |
| Registries | Pulling from a private registry, and pushing a built image to one |
| DNS credentials | Wildcard certificates through the DNS-01 challenge |
| S3 storages | Backup destinations — verified before use, and flagged when they stop working |

> **Note** — Members may add GitHub Apps, registries and S3 storages (`sources:manage`), but never read a private key back: key material needs `keys:reveal`.
