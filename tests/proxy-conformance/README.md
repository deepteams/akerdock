# Proxy conformance fixtures (proxy-contract §9, ADR-009)

The point of these fixtures is stated in ADR-009: **the same fixtures, two
providers**. Traefik is the P0 provider; Caddy is planned for P2. What makes the
two interchangeable is not that they emit the same configuration — they cannot —
but that they produce the same *behaviour* from the same intermediate
representation.

Each case is one input (`ir.json`) and one expected output per provider:

```
cases/<name>/
├── ir.json                 the intermediate representation (the only input)
├── expected/traefik/       byte-for-byte output of the Traefik generator
│   ├── traefik.yaml        static config (when the case covers it)
│   └── dynamic/<uuid>.yaml per-application dynamic file
└── expected/caddy/         P2 — its absence is reported, not ignored
```

The generators must be **deterministic**: the same IR always yields the same
bytes. That is not a stylistic preference — the proxy applies a config only when
its checksum differs (§6.2), so a generator that reorders a map on every run
would rewrite the remote file forever and defeat drift detection.

Golden files are level 1. Level 2 — replaying real HTTP assertions against the
generated config in Docker-in-Docker — is covered today by `scripts/e2e.sh`,
which routes real traffic through a real Traefik.
