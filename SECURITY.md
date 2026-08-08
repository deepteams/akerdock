# Security Policy

## Supported versions

AkerDock has not published a tagged release yet. Until the first release, only
the current `main` branch receives security fixes. It is development software
and should be evaluated accordingly before being exposed to untrusted users or
production infrastructure.

After versioned releases begin, security fixes will target the latest supported
release and `main`; this section will then list the exact supported versions.

## Report a vulnerability

Please do **not** disclose a suspected vulnerability in a public issue,
discussion, or pull request.

Use GitHub's [private vulnerability reporting
form](https://github.com/deepteams/akerdock/security/advisories/new). If that
form is unavailable, email [contact@deepteams.io](mailto:contact@deepteams.io)
with `AkerDock security` in the subject line.

Provide as much of the following as is safe and practical:

- the affected component and version or commit;
- deployment assumptions and required privileges;
- reproducible steps or a minimal proof of concept;
- the observed and potential impact;
- any suggested mitigation; and
- logs or screenshots with credentials, tokens, keys, hostnames, and personal
  data removed.

You should receive an acknowledgement within three business days and an initial
assessment within seven business days. Complex reports may take longer to
validate. We will coordinate remediation and disclosure with the reporter and,
with permission, credit the discovery in the advisory or release notes.

## Safe research

Good-faith research should avoid privacy violations, service disruption, data
destruction, and access beyond what is necessary to demonstrate the issue. Do
not test against infrastructure you do not own or have explicit permission to
use.

The architecture and known trust boundaries are documented in the [threat
model](docs/specs/threat-model.md). The public policy in this file takes
precedence for vulnerability reporting.

