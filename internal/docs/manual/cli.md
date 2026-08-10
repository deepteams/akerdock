---
id: cli
title: The CLI
icon: terminal
group: Automate
summary: Log in, target a repository, and drive the platform from your shell.
order: 0
---

## Signing in

```
akerdock login --url https://akerdock.example.com
akerdock context list          # several instances
akerdock context use staging
akerdock whoami                # where you point, and as whom
```

Login opens your browser, works with SSO, and needs no open port on your machine. The credential lands in `~/.akerdock/credentials.yaml` (mode `0600`), separate from the configuration so the latter stays shareable.

`whoami` answers from that file alone — context, instance, team and the scopes your token carries, with no network call and never the token itself. It is the question worth asking before a command that stops something.

## How a command is spelled

The tree is typed: **`akerdock <type> <verb> [NAME]`**. Every verb that acts on one kind of resource lives under that kind’s group, and the name comes last.

What each group offers — `--help` on any of them is the authoritative list:

```
akerdock app   list · info · logs · shell · port-forward · open
               restart · start · stop
               deploy run|list|cancel|rollback
               env list|get|set|unset
               preview list|redeploy|keep
               tasks list|run
akerdock db    list · info · shell · console · port-forward
               restart · start · stop
               backups list|run
akerdock svc   list · info · restart · start · stop
               deploy run|list|cancel
               env list|get|set|unset

akerdock login · logout · context · whoami · list
               tunnel · ingress · mcp · version
```

- A group offers **the verbs its type actually has**, and not one more — a database has no `deploy`, an application has no `console`, a stack has neither `logs` nor `shell`. The asymmetry is the API’s, and stating it in `--help` beats discovering it at runtime.
- `list` is the spelling everywhere — `app list`, `db backups list`, `tunnel list`; `ls` stays an accepted alias.
- `-a`, `-e`, `-p` are the short forms of `--application`, `--environment`, `--project`. A positional name always wins over `-a`, which carries the default rather than the target.
- Output is a table; `-o json` returns the API objects unaltered, for scripting. Exit codes: `0` success, `1` error, `2` usage — a typo and a failed deployment are told apart.

> **Note** — The old `type/name` form is gone. `akerdock logs app/api` is refused rather than translated, with the command that replaced it — `akerdock app logs api`.

## A `.akerdock` file in your repository

Drop a `.akerdock` file at the root of the repo and every command in that tree gets its defaults — no UUID to paste, no flags to remember. It never contains a token, so it is committable. The six fields, the precedence between them and everything else that belongs in a repository live in **What lives in your repository**.

.akerdock:

```
context: production
project: varuna
application: api
component: web       # default compose service for logs/shell/port-forward
```

> **Note** — Only the **application** has such a default — a repository declares the app it deploys, never the database it talks to — so `akerdock app logs -f` works from that directory with no name, while `db` and `svc` verbs always take one.

## The commands you will actually use

```
akerdock list                      # apps, databases, stacks
akerdock app logs -f               # the default app of this directory
akerdock app shell                 # a shell in its container
akerdock app info                  # status, health, components, last deploy

akerdock app env set FEATURE_X=on --apply   # write it, and apply it
akerdock app deploy run -f                  # ship, and watch the build
akerdock app deploy rollback                # …or take it back
akerdock app restart                        # start | stop alongside it

akerdock app preview list          # the live PR instances
akerdock app tasks run migrate     # a scheduled task, right now

akerdock db console main           # forward + local client
akerdock db port-forward 15432:5432 main
akerdock db backups run main       # back up before you break something

akerdock tunnel open prod-replica  # a declared external endpoint
akerdock ingress dev 3000          # public URL onto localhost:3000
```

> **Note** — The CLI holds your permissions, not more: what the dashboard hides, it refuses too.

> **Note** — Two things stay in the dashboard by decision, not by omission: **restoring a backup** and **approving a fork’s preview**. And a deployment always starts from a source the platform can fetch again — a Git ref or an image — never from a local folder.
