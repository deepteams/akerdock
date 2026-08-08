## Summary

<!-- Explain the user-visible outcome and why this change is needed. -->

## Related issue or specification

<!-- Use "Closes #123" when applicable. Link relevant PRD, ADR, OpenAPI, or technical-spec sections. -->

## Changes

<!-- List the important implementation and contract changes. -->

## Validation

<!-- List the exact automated and manual checks run, including their results. -->

```text
make test
make lint
```

## Checklist

<!-- Check applicable items. For an unchecked item that applies, explain why it is deferred. -->

- [ ] New logic has unit tests.
- [ ] API contract changes were made spec-first in `docs/specs/openapi-v1.yaml`, followed by `make generate`, and generated code is included.
- [ ] Database changes include a goose migration and regenerated sqlc output where needed.
- [ ] Structural decisions include a new ADR and the corresponding PRD §26 tracking update.
- [ ] Documentation, metrics, audit events, authorization checks, and recovery behavior were updated where applicable.
- [ ] Logs, errors, tests, and fixtures do not expose secrets or key material.
