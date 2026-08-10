---
id: port-forward
title: Port-forward and tunnels
icon: cable
group: Run and debug
summary: Reach a container or a private endpoint from your machine, without exposing it.
order: 2
permission: port-forwards:open
gates:
  external-endpoints-the-bastion: external-endpoints:read
  ingress-a-public-url-onto-your-laptop: ingress-tunnels:open
---

## Forwarding a port

A port-forward opens a TCP tunnel from `127.0.0.1` on your machine to a port of a container — the database keeps no public port, and your local client talks to it as if it were local.

```
akerdock db port-forward 15432:5432 main
psql postgres://user@127.0.0.1:15432/app

akerdock app port-forward 8080:3000 api --pr 42   # into a PR preview
akerdock db console main                          # forward + launch psql
```

- The **ports come first and the name last** — one look tells the two positionals apart, and the group already said which kind of resource you are aiming at.
- The tunnel rides HTTP/3, falls back to HTTP/2, then to WebSocket — it only ever needs 80/443, so a corporate proxy does not break it.
- Open sessions are listed in the dashboard, and from your shell with `akerdock tunnel list`; either surface can close one.
- Sessions are audited: who opened what, towards which target, and for how long.

## External endpoints (the bastion)

An **external endpoint** is a host:port outside the platform — a managed database, a partner’s API — that an administrator declares once. You then reach it through the same tunnel, without holding its network credentials or a VPN:

```
akerdock tunnel open prod-replica         # on a port the OS picks
akerdock tunnel open prod-replica 15432   # on a port you choose

akerdock tunnel list                      # every tunnel open in the team
akerdock tunnel close <session-uuid>
```

The remote port is not yours to choose: the endpoint froze its host and port when it was declared — which is why this is `akerdock tunnel`, its own top-level command, and not a `port-forward`, whose targets are the containers the platform deploys and which therefore lives under `app` and `db`. If the endpoint requires an approval, the CLI answers `access_request_required` and hands you the link to **request access**; a granted access has an expiry and can be revoked.

## Ingress — a public URL onto your laptop

Ingress is the mirror image: a stable public URL that relays to a port on **your** machine, for a webhook you are debugging or a demo of a branch that never left your editor.

```
akerdock ingress dev-kedric 3000
# https://dev-kedric.<ingress domain> → localhost:3000
```

- The endpoint’s FQDN is stable — declared once, so a webhook registered upstream keeps working across sessions.
- It is walled behind SSO by default; opening it to the public is an explicit choice.
- Live tunnels are visible in the dashboard, with their audit trail.
