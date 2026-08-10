---
id: applications
title: Applications
icon: rocket
group: Ship code
summary: Sources, build packs, the settings that matter, and the lifecycle buttons.
order: 0
permission: applications:read
gates:
  the-lifecycle-actions-and-which-one-you-want: applications:lifecycle
  settings-worth-knowing-about: applications:update
  delete-versus-unadopt: applications:delete
links:
  - label: Applications
    route: /applications
---

## The tabs of an application

| Tab | What lives there |
| --- | --- |
| Overview | Status, URLs, current image, the lifecycle actions menu |
| Settings | General, Source, Build, Routing, Deployment hooks, Health check, Resource limits, Deploys, PR previews |
| Environment variables | Two sets — production and previews — in a table or as a raw `.env` |
| Storages | Volumes, bind mounts and file mounts declared for the container |
| Scheduled tasks | Crons in the container or GitHub workflow dispatches, with their run history |
| Deployments | History, live build logs, cancel, rollback |
| Logs | Runtime logs of the container, streamed |
| Previews | One instance per open pull request |
| Terminal | A shell inside the running container |
| Webhook | The endpoint your CI or your Git host calls to deploy |
| Danger | Delete, or unadopt (stop managing without destroying) |

## Where the code comes from

| Source | Use it when |
| --- | --- |
| Git repository | The platform clones and builds — public URL, GitHub App, or deploy key for a private repo |
| Dockerfile | You want to paste a Dockerfile and have it built as-is |
| Docker image | Your CI already builds and pushes; AkerDock only pulls and runs it |

For a Git source, the build pack decides how the image is produced: `nixpacks` detects the language and writes the Dockerfile for you, `dockerfile` uses yours (with `base directory` and `dockerfile location` for a monorepo), `static` serves a published directory, and `compose` treats a compose file as the definition of a multi-service application.

## The lifecycle actions, and which one you want

| Action | What it does | Reach for it when |
| --- | --- | --- |
| Deploy | Clones, builds, starts the new container, switches traffic | You shipped code |
| Rebuild (no cache) | Same, with every build layer recomputed | The build is stale or lying to you |
| Recreate (apply config) | Re-creates the container from the image already deployed, with the configuration as it stands — no clone, no build | You edited a variable, a limit or a mount and want it live |
| Restart | Restarts the existing container | The process is wedged |
| Start / Stop | Runs or stops the container without touching its definition | Pausing an environment |
| Rollback | Goes back to a previously deployed image | The last deploy is bad |

> **Warning** — **Restart does not pick up a new environment variable.** A container freezes its environment at creation; restarting hands the process back the values it already had. **Recreate (apply config)** is the one that applies an edited variable without a rebuild.

The same actions from your shell:

```
akerdock app info api                  # status, health, components, last deploy
akerdock app restart api               # …and start | stop
akerdock app deploy run api -f         # deploy, following the build
akerdock app deploy run api --skip-build     # Recreate (apply config)
akerdock app deploy run api --force-rebuild  # Rebuild (no cache)
akerdock app deploy rollback api
akerdock app open api                  # the public URL (--dashboard for this page)
```

## Settings worth knowing about

- **Health check** — path, port, interval, retries. Traefik only routes to a healthy container, and a rolling update waits for it; without one, a broken deploy still takes traffic.
- **Resource limits** — memory and CPU, actually enforced as cgroups on the container.
- **Deployment hooks** — a command before the deploy (in the old container) and after (in the new one). A failing post-command fails the deployment.
- **Scale to zero** — stop the container after a period without traffic and wake it on the next request. Trades a cold start for an idle server.
- **Search engines** — one switch to answer `X-Robots-Tag: noindex, nofollow` on every domain of the app. Preview URLs are never indexable regardless.

## Delete versus unadopt

**Delete** destroys the resource and its containers. **Unadopt** only makes AkerDock forget it: the containers keep running on the server, untouched, and stop being managed. Unadopt is the exit door — nothing here holds your workload hostage.
