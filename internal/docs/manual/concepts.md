---
id: concepts
title: How the platform is organised
icon: boxes
group: Start here
summary: Team → project → environment → resource, and where a server fits in.
order: 1
---

## The hierarchy

Everything you create hangs somewhere on this tree:

```
Team                     isolation boundary — servers, resources, tokens
 └── Project             a product, a client, a repository
      └── Environment    production, staging… + its shared variables
           └── Resource  application | database | compose stack
```

- **Team** — what your permissions are attached to. You can belong to several and hold a different role in each; the switcher sits at the bottom of the sidebar.
- **Project / environment** — grouping and scoping. Variables declared on an environment are visible to every resource in it.
- **Server** — the Linux machine the containers actually run on, with its own reverse proxy. Registered by an administrator; you only choose it.
- **Destination** — the Docker network on that server your container joins. Resources sharing a network can talk to each other by UUID.

> **Note** — Each resource has a **UUID** and that UUID is the container name, the network alias and the internal hostname. It is what you connect to from another container, and what you pass to the API and the CLI.

## The three kinds of resource

| Resource | What it is | Where |
| --- | --- | --- |
| Application | Your code: a Git repo built here, an inline Dockerfile, or an image pulled from a registry | Applications |
| Database | A managed PostgreSQL with generated credentials, backup plans and restore | Databases |
| Compose stack | A docker-compose file deployed as one unit, with a domain per routed service | Services |
