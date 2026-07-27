# ADR-003 — Secrets: AEAD envelope encryption in the database, internal SecretStore interface

- **Status**: Accepted
- **Date**: 2026-07-11
- **Related PRD sections**: §27.3, §19.2, §23.2, INV-003

## Context

The platform stores many secrets: private SSH keys, environment variables, webhook secrets, registry/S3/cloud credentials, OAuth client secrets, private CAs. Encrypting them with a single, global application key is simple, but allows neither rotation nor compartmentalization. An external secret store (Vault, SOPS, KMS) would offer better separation, but would impose an additional component on every self-hosted installation. The level of protection at rest and the future extension point must be defined.

## Decision

**AEAD envelope encryption (AES-256-GCM) in PostgreSQL**:

- The master key resides in a **root-only file** or an **environment variable**, external to the database (§23.2).
- **Key versioning and rotation** are supported from the start: each secret carries the version of the key that encrypted it, and rotation is performed without a blocking rewrite of the entire database (§19.2).
- An internal **`SecretStore` interface exists from the start**, but **a single implementation is shipped** (envelope encryption in the database). Vault/KMS will only be considered **upon validated user demand**.

The usage rules of §23.2 apply: secrets masked in UI/API/logs/audit, revealed only with the `read:sensitive` permission (INV-003), passwords hashed with Argon2id, API tokens hashed irreversibly.

## Alternatives considered

- **Vault/KMS from the start**: rejected — a heavy component to operate for the target user (modest VPS), contrary to the operational simplicity goal; remains possible later via the `SecretStore` interface.
- **SOPS/encrypted files outside the database**: rejected — separates secrets from their transactional lifecycle (versions, audit, deletion) and complicates backup/restore of the control plane.
- **Disk-level encryption only (LUKS/at-rest DB)**: rejected — protects neither against an exfiltrated SQL dump nor against overly broad application access, and offers neither per-secret versioning nor rotation.

## Consequences

- **Positive**: no external dependency; a database backup/restore carries the secrets (encrypted); key rotation possible without downtime; a clean extension point if Vault/KMS is ever requested.
- **Negative**: the master key becomes a critical point — losing it makes all secrets unrecoverable; its management (root-only file, permissions, backup separate from the database) must be documented in the runbooks (§29.10).
- **Accepted risks**: an attacker who obtains both a database dump and the master key (control plane compromise) reads all secrets — this is consistent with the threat model §23.1, where the control plane is highly privileged; no HSM/KMS integration as long as no validated demand exists.
