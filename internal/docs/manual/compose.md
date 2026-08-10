---
id: compose
title: Compose stacks
icon: boxes
group: Ship code
summary: A docker-compose file deployed as one unit, with a domain per service.
order: 6
permission: services:read
links:
  - label: Services
    route: /services
---

## What a stack is

A stack is a compose file the platform owns: it deploys every service, isolates them in a network named after the stack UUID, and routes the ones you gave a domain. Services without a domain stay private and reachable by their compose name.

You can paste the compose file inline from **Services → New compose stack**, or build one from a Git repository by choosing the `compose` build pack on an application.

## Managing one

- **Components** lists the containers of the stack with their status; logs and terminal are per component.
- **Compose file** is editable in place, then redeployed.
- **Environment variables** are shared by the stack; magic variables such as `SERVICE_FQDN_*`, `SERVICE_URL_*` and `SERVICE_PASSWORD_*` are generated once and kept stable across redeployments.
- Deploy, Start, Stop and Restart act on the whole stack.
- A stack’s internal database can carry its own **backup plan**, like a managed database.

The stack group in the CLI — the verbs a stack actually has:

```
akerdock svc list
akerdock svc info shop           # the stack and its components
akerdock svc restart shop        # …and start | stop
akerdock svc deploy run shop -f  # …and deploy list | cancel
akerdock svc env list shop       # …and env get | set | unset, with --apply
```

> **Note** — A stack has **no `logs`, `shell` or `port-forward` of its own**, and no `rollback`: those endpoints exist for applications and databases only. Debug a stack’s container through the application that owns it, or from the Components tab above — each card carries the exact command to copy.

> **Note** — Access protection (`basic_auth`, `sso`) applies to every routed service of the stack, not only the first one.
