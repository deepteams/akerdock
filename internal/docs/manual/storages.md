---
id: storages
title: Persistent storage
icon: hard-drive
group: Ship code
summary: Volumes, bind mounts and editable file mounts.
order: 7
permission: storages:manage
---

## The three kinds

| Kind | What you give | Good for |
| --- | --- | --- |
| Volume | A name and a mount path | Data that must survive a redeploy — uploads, a database directory |
| Bind mount | A host path and a mount path | Something already on the server |
| File mount | A path and the file content, edited here | A config file you want to tweak without rebuilding |

A volume name is prefixed with the resource UUID, so two applications declaring `data` never collide.

## When it takes effect

Mounts are part of the container definition: declaring one changes nothing until the container is re-created — **Recreate (apply config)** or a deploy.

> **Warning** — Deleting an application does not restore the data in its volumes. Sharing one volume between containers is discouraged: two writers, one lock, and eventually a corrupt file.
