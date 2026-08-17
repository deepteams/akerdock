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

## Proxy and certificates

- Start, stop and restart the proxy, read its logs, edit its configuration. **Stopping it cuts every inbound request on that server.**
- The Certificates tab lists what the proxy actually serves, with expiry, and can force a renewal.
- Routed domains shows every FQDN the server answers for — the fastest way to find a stale route.

## Cleanup and adoption

- **Automated cleanup** reclaims disk on a threshold or a schedule, never during a deployment, and only on resources the platform manages.
- **Adoption scans** find containers running on the server that AkerDock does not manage, and adopt them without redeploying — how an existing machine is brought in without downtime.
