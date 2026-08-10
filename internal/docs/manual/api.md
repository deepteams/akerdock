---
id: api
title: API and tokens
icon: webhook
group: Automate
summary: A personal token, the REST API, and the read-only MCP server.
order: 1
---

## Creating your token

Under **Personal settings → API tokens**, mint your own token — scoped to the team you are acting in, shown once, stored hashed. A colleague’s tokens are theirs; you never need to share one.

| Scope | Grants |
| --- | --- |
| read | Reading the inventory and the statuses |
| read:sensitive | Reading values normally masked — only if your own role has it |
| write | Creating and updating resources |
| deploy | Triggering deployments and lifecycle actions |
| root | Everything your role can do |

> **Warning** — A token can never exceed the permissions of the person who created it. Yours narrows with your role — including when your role is narrowed later.

## Calling the API

```
curl -H "Authorization: Bearer $AKERDOCK_TOKEN" \
  https://<instance>/api/v1/applications

curl -X POST -H "Authorization: Bearer $AKERDOCK_TOKEN" \
  https://<instance>/api/v1/applications/<uuid>/deploy
```

The contract is an OpenAPI document, and the dashboard itself is one of its clients — anything the UI does, the API does. The API must be enabled at instance level, and can carry an IP allow-list per token.

## Your assistant, read-only

When the instance enables it, `akerdock mcp` exposes a read-only MCP server over stdio for a local assistant: overview, servers, projects, applications, databases and services. It reads with your permissions and writes nothing.
