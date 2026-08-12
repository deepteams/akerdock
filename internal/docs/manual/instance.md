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

> **Note** — The control plane is never indexable: every response carries `X-Robots-Tag: noindex, nofollow`, and that is not a setting. Its `robots.txt` allows crawling on purpose — a crawler has to fetch the page to read that header, so banning the fetch would leave the dashboard listed instead of removing it.

An FQDN indexed before that header shipped drops out at the next crawl, which can take weeks on a URL a crawler has learned never changes. To force it, ask for a removal in the search engine's webmaster console (Google Search Console → *Removals*). Such a removal expires after about six months; by then the header has been read and the entry is gone for good.
