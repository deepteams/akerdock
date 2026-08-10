---
id: env-vars
title: Environment variables and secrets
icon: key
group: Ship code
summary: Two sets, build-time versus runtime, shared variables and masking.
order: 2
permission: secrets:read
gates:
  making-an-edit-take-effect: secrets:write
---

## Production and previews are two separate sets

The Environment variables tab has a switch: **Production** and **Previews**. Nothing is copied implicitly from one to the other — a PR instance never inherits your production credentials, and that is deliberate. Keys a preview needs must exist in the previews set.

Each set can be edited as a table, or in bulk as a raw `.env` in the developer view — paste a whole file and it is parsed key by key.

## Build time versus runtime

A variable is available to the running process by default. Flag it **available at build time** and it is also passed to the build — which is what a framework baking its public configuration into the bundle needs.

> **Warning** — A value used at build time ends up in the image. Never flag a real secret as build-time unless you accept it living in a layer.

## Masked values

A variable marked secret is masked once written: the dashboard shows it as redacted and you can overwrite it, never read it back. Revealing a stored secret needs `secrets:reveal`, which members do not have. If you lose a value, rotate it — the platform will not recover it for you.

## Shared variables and interpolation

A value can reference a variable declared higher up, so a shared endpoint or an API base URL is written once:

```
API_URL={{environment.API_BASE}}/v1
SENTRY_DSN={{team.SENTRY_DSN}}
CORS_ORIGIN={{deployment.url}}
```

- `{{team.KEY}}`, `{{project.KEY}}`, `{{environment.KEY}}` — declared under **Team settings → Shared variables**, at the scope you choose.
- `{{deployment.fqdn}}`, `{{deployment.url}}`, `{{deployment.pr_id}}` — the deployment’s own identity, resolved to the app’s domain in production and to the generated URL in a preview.
- Predefined: `AKERDOCK_FQDN`, `AKERDOCK_URL`, `AKERDOCK_BRANCH`, `AKERDOCK_PR_ID`, `AKERDOCK_RESOURCE_UUID`, `AKERDOCK_CONTAINER_NAME`, `SOURCE_COMMIT`, `PORT`, `HOST`.

> **Note** — A preview built from a **fork** never resolves shared references, approved or not: resolving them would hand a stranger’s branch your team’s values.

## Making an edit take effect

Saving a variable does not touch the running container. Use **Recreate (apply config)** to apply it without rebuilding, or deploy if you were shipping code anyway.

From your shell — `--apply` is the recreate, in the same command:

```
akerdock app env list api
akerdock app env get DATABASE_URL api
akerdock app env set API_URL=https://api.example.com LOG_LEVEL=debug api --apply
akerdock app env unset LEGACY_FLAG api
akerdock app env list --pr 42 api    # the previews set of PR 42
```

> **Note** — A compose stack has the same four verbs under `akerdock svc env`. Masking is the server’s answer, not the client’s: a value you cannot reveal here — `secrets:reveal`, and a token carrying `read:sensitive` — comes back redacted in the terminal too. Without `--apply`, `set` writes and stops there: a variable set and never applied is the mistake the flag exists to prevent.
