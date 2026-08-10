---
id: repository
title: What lives in your repository
icon: folder-git-2
group: Ship code
summary: The `.akerdock` file, the files the build reads, and what must never be committed.
order: 1
---

## A `.akerdock` file at the root

Commit a `.akerdock` file at the root of the repository and every CLI command run from that tree — or any directory below it — gets its defaults. No UUID to paste, no flags to remember, and the same behaviour for everyone who clones it.

.akerdock — committed with the code:

```
context: production     # which instance, from your ~/.akerdock contexts
project: varuna
environment: production
application: api        # the default target of every app verb
component: web          # default compose service for logs, shell, port-forward
```

All six fields are optional: `context`, `team`, `project`, `application`, `environment`, `component`. The file is found by walking up from the current directory the way git finds `.git` — and it must be a **file**: a `.akerdock` directory is ignored.

- It **never contains a token** — those live in `~/.akerdock/credentials.yaml`, mode `0600` — which is exactly why it is safe to commit.
- Precedence, strongest first: a CLI flag (`-a`, `-e`, `-p`), then an `AKERDOCK_*` environment variable, then this file, then your global configuration.
- A positional name always wins over the file: `akerdock app logs api` reads `api` whatever the default says.

> **Note** — Only the **application** has a default — a repository declares the app it deploys, never the database it talks to. So `akerdock app logs -f` works from that directory with no name, while `db` and `svc` verbs always take one.

What it buys you, from a fresh clone:

```
git clone git@github.com:acme/varuna.git && cd varuna

akerdock app logs -f          # the app this repo deploys
akerdock app deploy run -f    # ship it, and watch the build
akerdock app shell            # a shell in its container
```

## The files the build reads

For a Git source, the build pack decides which file in your repository defines the image. Each path is a setting on the application, so a monorepo moves them without moving your code.

| Build pack | What it reads | The setting that moves it |
| --- | --- | --- |
| nixpacks (default) | Nothing — the language and framework are detected, and the Dockerfile is generated for you | Install / build / start commands, overridden in Settings → Build |
| dockerfile | Your `Dockerfile` | `dockerfile location`, relative to `base directory` (default `/Dockerfile`) |
| compose | Your `docker-compose.yml` | `compose file location`, relative to `base directory` |
| static | The directory your build publishes | `publish directory`, served by nginx |

`base directory` (default `/`) is the root of the sub-project inside the repository, and every other path hangs off it — the one setting a monorepo cannot do without.

## Branches, pushes and previews

- One application tracks **one branch**. A second environment is a second application on the same repository, not a second branch on this one.
- With **auto-deploy** on, a push to that branch ships. **Watch paths** narrow this to pushes that touch certain files.
- A commit whose message contains `[skip ci]` or `[skip cd]` is ignored.
- With **previews** on, every pull request gets its own instance and URL, destroyed when the PR closes.

## What does not belong in the repository

- **Environment variables and secrets** — they live on the platform, per application and per environment, and reach the container at creation. Read them with `akerdock app env list`, write them with `akerdock app env set KEY=value --apply`.
- **Your CLI token** — `akerdock login` writes it to `~/.akerdock/credentials.yaml`, never to the repository.
- **Build-time secrets** — pass them as build secrets (`akerdock app env set KEY=value --secret`), which BuildKit mounts for one `RUN` instead of baking them into a layer `docker history` would show.
