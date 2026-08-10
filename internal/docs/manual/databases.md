---
id: databases
title: Databases
icon: database
group: Run and debug
summary: "Managed PostgreSQL: connecting, lifecycle and what stays admin-only."
order: 3
permission: databases:read
gates:
  creating-one: databases:create
  lifecycle: databases:lifecycle
links:
  - label: Databases
    route: /databases
---

## Creating one

From Databases, create a **PostgreSQL** instance in a project and environment — the managed engine of this version. Credentials are generated for you; image, tag, resource limits, health check and custom configuration are editable.

## Connecting to it

- **From an application on the same network** — use the database UUID as hostname; nothing needs to be exposed.
- **From your machine** — `akerdock db console <name>` opens a tunnel and launches `psql`; `akerdock db port-forward 15432:5432 <name>` leaves the tunnel open for your own client. For a database service inside a compose stack, name the application instead: `akerdock db console --app <app> -c <service>`.
- **From the dashboard** — the Database shell card opens `psql` in the container; `akerdock db shell <name>` is the same shell from your terminal.

> **Note** — The full connection string with its password needs `databases:credentials`, which members do not hold. The Connection card still shows you everything else, and the tunnel works without ever showing you the password.

## Lifecycle

Start, stop and restart from the detail page, or from your shell — `akerdock db restart <name>`, and `start` / `stop` alongside it. Stopping a database stops everything that depends on it — check the environment before you do it on a shared instance. There is no confirmation prompt in the terminal: the platform can start it again.
