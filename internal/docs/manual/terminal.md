---
id: terminal
title: Terminal
icon: terminal
group: Run and debug
summary: A shell inside a running container, from the browser or the CLI.
order: 1
permission: terminal:open
---

## Opening a shell

The Terminal tab of an application, a preview, a database or a stack component opens a real interactive shell in the container — a full terminal, not a command runner. Sessions reconnect and keep their scrollback.

```
akerdock app shell api             # the application's container
akerdock app shell api -c worker   # a component of a compose stack
akerdock db shell main             # a managed database
```

> **Note** — Every session is authenticated, bound to the team you act in, and audited. A shell **on the server itself** is a different permission (`terminal:root`) and belongs to administrators.
