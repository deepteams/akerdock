---
id: domains
title: Domains, HTTPS and access protection
icon: globe
group: Ship code
summary: Routing several domains and ports, and walling an app behind SSO.
order: 3
permission: applications:update
---

## Declaring routes

Under Settings → Routing, each row maps one or more domains to a container port. The underlying format is one entry per line:

```
app.example.com              # port inferred
api.example.com:8080         # explicit target port
example.com/checkout         # path-based route, most specific wins
```

- Point the DNS record at the **server’s** IP: traffic goes straight to it, never through the control plane.
- A certificate is issued and renewed automatically for each domain; wildcards go through a DNS-01 credential an administrator registers.
- A compose stack routes per service — each routed service gets its own domain.

## Who can reach the application

| Access protection | Effect |
| --- | --- |
| none | Public — the default |
| basic_auth | Shared credentials prompted by the proxy |
| sso | An AkerDock session with access to this team is required |

The wall applies to **every** domain of the application, previews included. Paths that must stay open through it — a webhook receiver, a health endpoint, an OAuth callback — are listed as **public routes**, with their methods.

> **Note** — This is the cheap way to keep a staging app off the open internet without touching its code.
