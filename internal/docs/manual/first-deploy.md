---
id: first-deploy
title: Deploy your first application
icon: rocket
group: Start here
summary: From a Git repository to a running container with an HTTPS URL.
order: 0
permission: applications:create
links:
  - label: New application
    route: /applications/new
  - label: Applications
    route: /applications
---

## What must already exist

Applications are deployed onto a server your team already has, into a project and an environment. You do not need to own or configure the server: pick one from the list — the placement step only asks where the container should run.

- A **server** registered and validated by an administrator (Servers page, read-only for you).
- A **project** and an **environment** — create them yourself from Projects, `production` is created with every project.
- For a private repository: a **GitHub App** or a **deploy key** under Sources. You may add these yourself.

## The five steps

1. Open **Applications → New application**, or start from an environment so the placement is pre-filled.
2. Choose the **placement**: project, environment, server and destination (the Docker network the container joins).
3. Choose the **source**: a Git repository (public URL, GitHub App or deploy key), an inline Dockerfile, or a pre-built Docker image.
4. Pick the **build pack** for a Git source — `nixpacks` (auto-detection), `dockerfile`, `static`, or `compose` — and the branch.
5. Create, then hit **Deploy**. The Deployments tab streams the build log line by line.

> **Note** — An application with no domain is reachable from other containers on its network under its UUID as hostname — that is enough for a worker or an internal API. Add a domain when it needs to be reached from the outside.

## Right after the first deploy

- Add your **domains** under Settings → Routing; the certificate is issued automatically.
- Declare the **environment variables** the app reads — a container freezes its environment when it is created, so a variable added later needs *Recreate (apply config)*.
- Set a **health check** so rolling updates can switch traffic only once the new container answers.
- Turn on **auto-deploy** if you want every push on the branch to ship.
