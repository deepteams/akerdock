---
id: instance
title: Instance settings
icon: settings
group: Instance administration
summary: FQDN, email, sign-in providers, API, telemetry and encryption.
order: 2
root: true
links:
  - label: Global settings
    route: /system
---

## What is configured there

- **Instance** — the FQDN the dashboard is served on and the ACME contact address.
- **Teams** — every team on the instance.
- **Email** — transactional email (SMTP or Resend) for invitations and password resets; teams may reuse it for their notifications.
- **API access** — the API is off until enabled here.
- **Sign-in** — OAuth and OIDC providers offered on the login page.
- **Telemetry** — remote OTLP export, signal by signal.
- **Encryption** — state of encryption at rest and master key rotation.
- **Audit** — the instance-wide log, across every team.

> **Note** — The control plane is never indexable: it serves `robots.txt` with `Disallow: /` and answers `X-Robots-Tag: noindex, nofollow`. That is not a setting.
