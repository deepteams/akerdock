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

> **Note** — Members may add GitHub Apps, registries and S3 storages (`sources:manage`), but nobody reads a private key back — not even root. Once a key enters AkerDock, only its public half is ever served.

## Private keys never leave

A private key enters AkerDock by paste or by **Generate**, which creates an ed25519 keypair
inside the platform. Either way the private half is stored encrypted and is write-only from
then on: there is no reveal, whatever your permissions. This is deliberate — the only
consumer of that material is AkerDock's own SSH connection, so keep your own copy at
generation time if you need one elsewhere; the platform is not an escrow. What every key
serves is its public half: one line ready for the server's `authorized_keys`, shown in the
generation banner with a copy button and unfoldable on any row with the key-shaped button.
Replacing a key's material (rotation) is still a write, and the new material is no more
readable than the old.
