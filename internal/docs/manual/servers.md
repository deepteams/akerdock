---
id: servers
title: Servers
icon: server
group: Instance administration
summary: Registering a machine, its proxy, its certificates and its cleanup.
order: 0
permission: servers:manage
gates:
  proxy-and-certificates: servers:proxy
  cleanup-and-adoption: servers:maintain
links:
  - label: Servers
    route: /servers
---

## Registering and validating

1. Add the private key under **Sources → Private keys** (SSH key only — no password, no passphrase).
2. Register the server with its host, port and user; root, or a non-root user with `sudo NOPASSWD: ALL`.
3. Run **Validate**: connectivity, prerequisites, Docker Engine, and the health checks.
4. Set the wildcard domain if new resources should get an automatic subdomain.

## A non-root user

Root is the nominal contract; a non-root user works two ways, and the form shows the choice
once the user is not `root`.

- **Escalate remote commands with sudo** wraps every command the platform runs on the server
  in non-interactive `sudo -n`. The connection is authenticated with an SSH key, so there is
  no password to type when sudo prompts: the user needs a passwordless sudoers entry —
  `echo 'deploy ALL=(ALL) NOPASSWD: ALL' | sudo tee /etc/sudoers.d/90-akerdock` — and
  validation proves it with its own `check_sudo` step before anything depends on it.
- **Without sudo**, prepare the machine once by hand: give the user
  `/var/lib/akerdock` (`sudo mkdir -p /var/lib/akerdock && sudo chown -R deploy: /var/lib/akerdock`)
  and put them in the `docker` group (`sudo usermod -aG docker deploy`, new session required).

Toggling the sudo option later moves the server back to *pending* — every command changes its
execution identity, so nothing proven by the last validation still holds. The server terminal
is never escalated: it is your session, type `sudo` yourself.

## A server behind an edge

For a machine the internet cannot reach — a GPU box on the LAN, a homelab behind one NAT —
set **Edge server** in the settings to the server that receives the public 80/443. The edge
relays that server's public domains by TLS passthrough: it reads the requested name in the
TLS handshake and pipes the bytes through, holding no certificate and no key. Certificates,
access walls, noindex and the scale-to-zero waiting page keep running on the server that
hosts the application — nothing about the app moves.

- One DNS record is enough: point the wildcard at the public entry. The same wildcard domain
  may be declared on several servers as their naming template; each host gets its own
  certificate, issued by the server that hosts it (the ACME challenge traverses the edge).
- The edge must run a Traefik proxy and cannot itself relay through an edge — no chains.
- Changing the designation converges everything by itself: the relay file moves to the new
  edge and the origin's proxy is recreated to trust it. No revalidation, SSH is untouched.
- A service with no public domain gets no relay: from the internet it simply does not exist.
  To reach it from inside the LAN under the same name, teach your local resolver to answer
  the origin's address for it — that is your resolver's job, not the platform's.

## Proxy and certificates

- Start, stop and restart the proxy, read its logs, edit its configuration. **Stopping it cuts every inbound request on that server.**
- The Certificates tab lists what the proxy actually serves, with expiry, and can force a renewal.
- Routed domains shows every FQDN the server answers for — the fastest way to find a stale route.

## Hugging Face on a GPU server

A server whose validation observed a GPU gains a **Hugging Face** tab:

- **Token** — used by this server's model containers for gated downloads; it wins over the
  instance-wide `AKERDOCK_HF_TOKEN`. Write-only: set it, replace it, clear it — it is never
  shown again.
- **Weights cache** — the one cache every model on this server shares. Stopping or deleting
  a model never touches it; reclaiming space is your explicit act here, per model or all at
  once. A model whose weights you delete keeps serving from memory and re-downloads at its
  next start.

## Cleanup and adoption

- **Automated cleanup** reclaims disk on a threshold or a schedule, never during a deployment, and only on resources the platform manages.
- **Adoption scans** find containers running on the server that AkerDock does not manage, and adopt them without redeploying — how an existing machine is brought in without downtime.
