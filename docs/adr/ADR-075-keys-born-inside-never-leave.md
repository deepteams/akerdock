# ADR-075 — A private key enters AkerDock and never leaves it

- **Status**: Accepted
- **Date**: 2026-08-15
- **Related**: PRD §3.1/§5.1 (keys "generated or imported" — the generated half was never
  built), §23.2 (encryption at rest), INV-003 (secrets never returned without
  `read:sensitive`); [ADR-038](ADR-038-granular-rbac.md) (the `keys:*` vocabulary this
  shrinks); instance-config §6.2 (the instance key — the same generator, already in the code)
- **Related PRD sections**: §3.1, §5.1, §23.2, §26

## Context

Every team key in AkerDock was **imported**: the operator generates a keypair on their
laptop, pastes the private half into the dashboard, and installs the public half on the
server. The platform guarded that material carefully — AEAD at rest (§23.2), never echoed on
creation — but kept a way back out: `GET /private-keys/{uuid}?reveal=true` returned the
private material to `read:sensitive`, audited, under the permission `keys:reveal`.

Two observations end that reveal.

First, the platform never needs it. A private key has exactly one consumer here: the SSH
dialer. Everything an operator legitimately retrieves about a key — to install it on a
server, to reference it as a deploy key, to compare fingerprints — is the **public half**,
which every list and get already serves.

Second, the PRD has promised "generated or imported" keys since §5.1, and only imported
shipped, while the generator already existed for the instance key
(`sshkey.GenerateEd25519`, instance-config §6.2). For a generated key, a reveal is not even
a *recovery*: nobody ever had the material, so the audited-reveal rationale of INV-003 —
giving an operator back what they once possessed — does not apply. And a first design that
kept the reveal for imported keys while refusing it for generated ones would have needed a
per-row `exportable` flag to remember which promise each row carries. The flag is the tell:
two classes of keys with two contracts, for a distinction no consumer needs. One contract is
simpler and strictly safer.

## Decision

### 1. Private key material is write-only, for every key, forever

Once a key enters the platform — pasted or generated — its private material can never be
read back through the API, whatever the caller's permissions. The reveal parameter, the
`private_key` response field, `is_redacted`, and the `keys:reveal` permission are removed;
`GET /private-keys/{uuid}` serves metadata and the public key, exactly like the list. There
is no per-key flag: the guarantee is uniform, so there is nothing to store.

This narrows INV-003's reveal pattern for this resource on purpose. `secrets:reveal` and
`databases:credentials` remain: an env var or a database password is *used by* the operator's
own systems, and reading it back is the product. A private key is used by AkerDock's SSH
dialer and by nothing else; serving it to anyone — root included — is pure exfiltration
surface. The rbac-matrix already stated the asymmetry ("setting a value is a configuration
act, reading one back is exfiltration"); this ADR makes the key half of it total.

### 2. `POST /private-keys/generate` creates an ed25519 keypair server-side

Permission `keys:manage`, same as import. Body: `name` (required) and `description`. The
platform generates ed25519 — no algorithm parameter: it is what the instance key already
uses, what every supported server accepts, and offering RSA would only invite the weaker
choice. The private half is envelope-encrypted and stored exactly like an imported key's;
the `201` carries the metadata and the **public key only**, one line ready for the server's
`authorized_keys`. A generated key's private material has then never existed anywhere but
inside the platform.

### 3. Rotation-by-replacement stays, uniformly

`PATCH` with `private_key` still replaces the material: it is a write, and the new material
is no more readable than what it replaces. With the reveal gone there is no longer any
contract difference between an imported and a generated key for a replacement to erode —
which is also why no origin flag needs to survive this decision.

### 4. The public half is the product

The dashboard's keys page grows a **Generate** action beside the import form, shows the new
key's public line immediately with the `authorized_keys` instruction and a copy button, and
every row can unfold its public key. The reveal affordance disappears from the UI entirely.
Nothing about a public key is secret; `keys:read` is its only gate.

### 5. What does not change

Deletion guards, `in_use` accounting, server and deploy-key references, encryption at rest,
the instance key's path: identical. No new permission — `keys:manage` covers creating keys
however they come to exist; `keys:reveal` leaves the catalogue and the rbac-matrix rather
than surviving as a permission that no endpoint consults.

## Consequences

- A key lost with the instance is lost — already true of the instance key; the backup story
  (§16) covers it: the encrypted row travels with the database backup, the master key with
  the documented restore procedure. An operator who wants an escrow copy keeps it wherever
  they generated it — the platform is not that escrow.
- `keys:reveal` disappears from the permission catalogue (`/permissions`), the role
  composer, and rbac-matrix; its number is retired, not reused.
- The `secret.reveal` audit action no longer has a private-key emitter; its other emitters
  are untouched.

## Verification

- Unit: generate → `201` with public key and no `private_key` field; `GET` on a key whose
  stored ciphertext is decryptable never carries the material; rotation by `PATCH` still
  answers `200`; the permission catalogue no longer lists `keys:reveal`.
- UI: generate flow shows the public line; no reveal affordance anywhere on the keys page.
