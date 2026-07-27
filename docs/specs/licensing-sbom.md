# License inventory and SBOM — AkerDock

> Artifact §29.11 of the PRD (`docs/PRD.md`). Covers the project license (ADR-020, §27.20), the dependency license policy for the chosen stack (§27.25, ADR-025), the orchestrated helper and runtime images (§6.1, §7, §16.2), the template catalog (§9, §27.10, ADR-010), the SBOM/signing/scanning requirements of §23.5 and the compose distribution (§27.21, ADR-021). The PRD and the ADRs are the source of truth; this document derives the operational rules from them.

Conventions: license identifiers use the SPDX nomenclature (`MIT`, `Apache-2.0`, `BSD-3-Clause`…). Decisions not yet ratified by an ADR are marked **"(proposed default)"**. Licenses not verified at the source (the upstream project's LICENSE file at the pinned version) are marked **"(to be verified)"** — no doubtful license claim is made without this marker.

---

## 1. Project license

**Apache-2.0** (ADR-020): maximum adoption, explicit patent clause, and alignment with the dominant license of the domain's compose templates — which makes importing them clean (§27.10). The "cloud fork" risk is accepted; it is not addressed by the license.

### 1.1 Required files at the repository root

| File | Content | Obligation |
|---|---|---|
| `LICENSE` | Full Apache License 2.0 text, unmodified | Mandatory before any code publication (ADR-020) |
| `NOTICE` | Project name, copyright line, required attributions (cf. §1.3) | Mandatory as soon as an Apache-2.0 dependency with its own `NOTICE` is embedded; created from the start |
| `THIRD-PARTY-NOTICES` (or generated `licenses/`) | Concatenation of the licenses of the dependencies embedded in the binary and the UI, generated automatically (`go-licenses save` + npm equivalent) | Generated at every release, attached to the artifacts; **(proposed default)** also exposed via `AkerDock licenses` in the CLI and a "Licenses" page in the UI |

### 1.2 File headers

**Policy: no per-file license header; the root `LICENSE` is authoritative for the whole repository. (proposed default)**

- Rationale: Apache-2.0 does not require a per-file header (the appendix of the license text is a recommendation, not a condition); headers create diff noise and are systematically forgotten.
- Exception: a file **copied or derived from a third-party project** mandatorily keeps its original copyright header and a provenance note (URL + commit), and its source project is added to the `NOTICE`.
- If the policy changes (e.g. a partner requirement), adding headers is automatable (`addlicense`) and will be the subject of a revision of this document.

### 1.3 Copyright management

- Single copyright line: `Copyright <year of first publication>-<current year> The AkerDock Authors` — **(proposed default)** the "The X Authors" model (Go/Kubernetes practice) avoids maintaining a name list and remains correct with the DCO (§7): each contributor keeps their copyright, the Apache-2.0 license carries the grant of rights.
- The `NOTICE` contains: this copyright line, the mention "This product includes software developed by third parties" and the attributions inherited from the `NOTICE` files of Apache-2.0 dependencies (collected automatically, cf. §2.3).
- No copyright assignment requested from contributors (no CLA, cf. §7).

---

## 2. Dependency license policy

Context: AkerDock distributes a **Go binary statically linking all of its dependencies** (ADR-021: static binary in a distroless image) and **bundled UI assets** (§25.2: compiled Angular embedded in the binary). Every Go dependency in the compilation graph and every npm package present in the final bundle is therefore **redistributed** under Apache-2.0: their license must allow it.

### 2.1 Allowed / to review / forbidden matrix

| Status | Licenses (SPDX) | Justification |
|---|---|---|
| **Allowed** | `MIT`, `BSD-2-Clause`, `BSD-3-Clause`, `Apache-2.0`, `ISC`, `MPL-2.0`, `Unlicense`, `0BSD`, `Zlib`, `PostgreSQL`, `BlueOak-1.0.0` | Permissive, compatible with redistribution under Apache-2.0. MPL-2.0: **file-by-file** copyleft — allowed as long as MPL files are not modified; any modification of an MPL file must be republished under MPL (constraint tracked in the exceptions file, §2.4) |
| **To review (case by case)** | `LGPL-2.1`, `LGPL-3.0`, `CDDL-1.0`, `EPL-2.0`, custom / non-SPDX licenses | LGPL: **Go static linking** makes the "relinkability" requirement (LGPL §4) hard to satisfy — an LGPL Go module linked into the binary is de facto problematic; acceptable only as an executed external tool (not linked) or with legal analysis. Custom licenses: full reading mandatory |
| **Forbidden (as a linked dependency)** | `GPL-2.0`, `GPL-3.0`, `AGPL-3.0`, `SSPL-1.0`, `BUSL-1.1`, `Elastic-2.0`, `CC-BY-NC-*`, any "non-commercial" or "source available" license | Strong copyleft: linking GPL/AGPL into the binary would force relicensing AkerDock; SSPL/BUSL/Elv2 are not open source and are incompatible with Apache-2.0 redistribution. **Forbidden = forbidden in the binary's link graph and in the UI bundle** — not in orchestrated third-party images (§4) nor in templates (§5), which are not linked dependencies |
| **Forbidden (everywhere)** | Dependencies without an identifiable license, dubious "WTFPL"-likes, proprietary licenses without redistribution rights | Unassessable legal risk |

Special cases:

- **Build and code-generation tools** (sqlc, oapi-codegen in generator mode, Angular CLI, syft…): their license does not contaminate the binary — only the **generated code** (which is ours) and any imported **runtime packages** count. A GPL tool used at build time would be acceptable (to be documented), but none is planned.
- **Test tools** (Pebble, Gitea/MinIO images in E2E…): never redistributed, license free of constraints for us; inventoried anyway (§3) to avoid an accidental switch to a linked dependency.
- **Orchestrated software** (Docker, Traefik, Nixpacks…): cf. §3 and §4 — reimplementing or embedding them is an explicit non-goal (§16.2), so never linked nor redistributed.

### 2.2 Automated verification in CI

- **Go**: `go-licenses check ./... --allowed_licenses=<list §2.1>` (or equivalent: `golicense`, `licensei`) on every PR; blocking failure if a license outside the list appears in the compilation graph. `go-licenses report`/`save` generates the `THIRD-PARTY-NOTICES` at release. **(proposed default: go-licenses, an Apache-2.0 Google tool)**
- **npm / Angular UI**: equivalent check on the bundle's production dependencies (`license-checker` or lockfile-lint + review of the lockfile's `licenses`), same allow list; devDependencies (build only) are excluded from the blocking check but inventoried.
- The check runs: on every PR touching `go.mod`/`go.sum`/the npm lockfile, on every release, and as a weekly scheduled job (detection of **upstream license changes** between versions — the Redis, MinIO, Elastic cases: a permissive dependency can become non-free at the next version).
- Every major version bump of a direct dependency goes through a PR where the license diff is checked.

### 2.3 Exception process

1. Open a "license-exception" issue: dependency, version, license, exact usage (linked / tool / test), evaluated alternatives.
2. Decision by the maintainers; if structural (e.g. first LGPL), an ADR is required.
3. The accepted exception is recorded in a versioned file (`.licenses/exceptions.yaml` — **proposed default**) read by the CI job: each entry carries dependency, covered version(s), justification, date, and a re-review deadline.
4. An exception without a deadline is forbidden; the weekly job alerts on expired exceptions.

---

## 3. Planned direct dependencies and their licenses

Inventory of the known stack (§27.25 / ADR-025, §25.2, §16.2). **Nature** column: `linked` = compiled into the binary or bundled into the UI (redistributed → matrix §2.1 applies); `orchestrated image` = third-party software that AkerDock drives via `docker pull`/`run` on the user's machine (never redistributed by us, cf. §4); `build tool` = used in CI/generation, absent from the artifacts; `test tool` = used in E2E/integration only.

| Component | License | Nature | Remarks |
|---|---|---|---|
| Go (stdlib + toolchain) | BSD-3-Clause | linked (stdlib) / build tool (toolchain) | |
| `golang.org/x/*` | BSD-3-Clause | linked | Almost certain to appear in the graph (crypto/ssh in particular) |
| pgx (`jackc/pgx`) | MIT | linked | PostgreSQL driver (ADR-025) |
| sqlc | MIT (to be verified) | **build tool** | Generates Go code that belongs to us; not linked |
| chi (`go-chi/chi`) | MIT | linked | HTTP router |
| oapi-codegen | Apache-2.0 (to be verified) | build tool + **linked runtime package** | The generator is a tool; the small runtime packages (`oapi-codegen/runtime`) are linked — same license to confirm on both modules |
| SSH library (x/crypto/ssh expected) | BSD-3-Clause | linked | ADR-001 transport |
| OpenTelemetry Go client (`go.opentelemetry.io/otel`) | Apache-2.0 | linked | ADR-008 (OTLP everywhere) |
| WebSocket library (terminal, ADR-024; choice not settled: `coder/websocket` or equivalent) | MIT / ISC (to be verified depending on the choice) | linked | To be frozen at implementation time |
| ACME/DNS-01 library if linked (lego) | MIT (to be verified) | linked (if chosen) | The parity HTTP-01 ACME is carried by Traefik (orchestrated); lego would only be linked for DNS-01 on the control plane side — implementation decision |
| Angular (framework + CLI) | MIT | linked (bundled runtime) / build tool (CLI) | §25.2: compiled assets embedded in the binary |
| xterm.js | MIT | linked (UI bundled) | Web terminal §5.7 |
| Docker Engine / Compose / BuildKit (Moby) | Apache-2.0 | image/software **orchestrated** on the target server | Non-goal to reimplement or embed it (§16.2); installed on the user's machine via Docker's script/packages, never redistributed by us |
| Traefik | MIT | orchestrated image | Default proxy §4.1 |
| Caddy | Apache-2.0 | orchestrated image | P2, ADR-009 |
| Nixpacks | MIT | tool **orchestrated** on the build server | AkerDock invokes it, does not reimplement it (§16.2); it produces a plan/Dockerfile — the resulting application image belongs to the user |
| Railpack | MIT (to be verified) | tool orchestrated on the build server | Successor of Nixpacks (§5.2), license to confirm at the pinned version |
| restic | BSD-2-Clause | orchestrated image/tool (volume backups, §20.5/ADR-014) | Check whether used via a helper image (cf. §4) or an installed binary |
| MinIO `mc` (S3 client, parity §7.2) | AGPL-3.0 | orchestrated image/tool | **Major point of attention**: cf. §4.2 — never linked, and preferably never rebundled into an image we publish |
| PostgreSQL (internal instance database, ADR-021) | PostgreSQL License | orchestrated image | Official image pulled by the distribution compose; we do not redistribute it |
| Pebble (`letsencrypt/pebble`) | MPL-2.0 (to be verified) | test tool | Test ACME server (TLS E2E) |
| Gitea | MIT | test tool | Git provider in E2E (ADR-026) |
| MinIO (server) | AGPL-3.0 | test tool | S3 target in E2E only; never distributed |
| go-licenses, syft, grype, trivy, cosign | Apache-2.0 (to be verified individually) | build/CI tools | The licenses/SBOM/signing chain itself |

Rules:

- This table is a **design snapshot**; the operational truth is the `go-licenses` report + the SBOM generated at every release (§6). Any direct dependency added to `go.mod` or the production `package.json` must comply with matrix §2.1 — the CI enforces it.
- "(to be verified)" licenses are confirmed **at the pinned version** at the time of the first `go get`/`npm install`, not from the upstream README.
- A component never silently changes "Nature" column: turning an orchestrated tool (e.g. restic) into a linked library is an explicit decision subject to matrix §2.1.

---

## 4. Helper and runtime images deployed on the user's machine

### 4.1 The key point: orchestrating is not redistributing

AkerDock does **not redistribute** third-party images: it orders a `docker pull` **from the upstream registries, directly on the user's server**. Consequences:

- **No redistribution obligation** (attribution, source availability, NOTICE) falls on the project for these images: the user obtains the software from its publisher, AkerDock is only an installer/orchestrator — the same position as a package manager.
- Network copyleft licenses (the MinIO server's AGPL, for example) apply to **the user operating the service**, not to AkerDock; for unmodified use of official images, the AGPL imposes nothing beyond the availability of the upstream sources.
- However, **as soon as we publish an image under our namespace** (the project's registry), we become a redistributor: SBOM, compliance with the licenses of everything in the image, and for AGPL/GPL, source obligation. Hence rule §4.3.
- Residual obligation on our side: **informing the user** (license displayed before deployment, §5) and **pinning** unmodified official images.

### 4.2 Inventory of orchestrated images

| Image | Role | Software license | Published by us? | Implication |
|---|---|---|---|---|
| `traefik` | Per-server proxy (PRD §4.1) | MIT | No — upstream | No obligation; pin tag + digest |
| `caddy` | Alternative proxy P2 | Apache-2.0 | No — upstream | Same |
| `postgres` | Internal instance database (ADR-021) + managed engine §6.1 | PostgreSQL License | No — upstream | Same |
| `mysql` | Managed engine §6.1 | GPL-2.0 | No — upstream | OK in orchestration; never rebundle nor link a GPL client |
| `mariadb` | Managed engine §6.1 | GPL-2.0 | No — upstream | Same |
| `mongo` | Managed engine §6.1 | **SSPL-1.0** (not open source) | No — upstream | OK in orchestration (user usage); license displayed to the user; never redistribute |
| `redis` | Managed engine §6.1 | ≤ 7.2: BSD-3-Clause; ≥ 7.4: tri-license RSALv2 / SSPLv1 / AGPL-3.0 (AGPL added with Redis 8) (to be verified at the pinned tag) | No — upstream | Choose and document the default tag knowingly; BSD alternative: `valkey` (BSD-3-Clause) — product decision to record |
| `eqalpha/keydb` | Managed engine §6.1 | BSD-3-Clause (to be verified) | No — upstream | Project not very active — monitor |
| `dragonflydb/dragonfly` | Managed engine §6.1 | **BUSL-1.1** (not open source) | No — upstream | OK in orchestration; license displayed; "non-compete" usage — it is the user this binds |
| `clickhouse/clickhouse-server` | Managed engine §6.1 | Apache-2.0 | No — upstream | No obligation |
| `minio/mc` | S3 upload of backups (parity §7.2) | **AGPL-3.0** | To be decided | If pulled upstream: OK. **Never copy `mc` into an image published by us** without assuming the AGPL obligations. **(proposed default)**: replace `mc` with a permissive S3 client — AWS Go SDK (Apache-2.0) linked on the worker side, or `rclone` (MIT) — and reserve `mc` for strict parity if necessary |
| `restic/restic` | Volume backups (ADR-014) | BSD-2-Clause | To be decided | Permissive: rebundling possible without strong constraint if a helper image proves necessary |
| AkerDock helper image(s) (cleanup §3.7, backup/restore executors §7, dynamic TCP proxy §6.2 — exact inventory to be frozen with `deployment-engine.md`) | Platform tooling on the target server | Apache-2.0 (our code) + base content | **Yes** — project namespace | **Redistribution assumed**: documented distroless/alpine base, SBOM per image, CVE scan, cosign signature, `THIRD-PARTY-NOTICES` included — same requirements as the AkerDock image (§6) |
| Sentinel/agent image (if containerized, §3.8) | Metrics agent | Apache-2.0 (our code) | **Yes** | Same |
| `nginx` | Static build pack (§5.2) | BSD-2-Clause | No — upstream | No obligation |

### 4.3 Rules

1. **By default, no third-party image is republished under the project's namespace.** A mirror (for resilience/Docker Hub rate limits) is an explicit decision that makes us a redistributor: it requires reviewing the obligations of the license in question (trivial for MIT/Apache, binding for AGPL/SSPL — and for SSPL/BUSL, probably forbidden or needlessly risky).
2. Every image **published by the project** (AkerDock, helpers, agent) follows the full §6 pipeline: SBOM, scan, signature, notices.
3. The default images of the managed engines (§6.1) are pinned **tag + digest** in the code/catalog; the user can change them (free image/tag field, PRD §6.2) — their responsibility is then displayed.
4. Non-free-licensed software orchestrated by default (MongoDB SSPL, Dragonfly BUSL, Redis ≥ 8) is flagged in the UI at engine-selection time, with the same license-display mechanics as the templates (§5.2).

---

## 5. Template catalog (§27.10, ADR-010)

The one-click templates reference upstream images with widely varying licenses, including non-free ones: MinIO (AGPL-3.0), Elasticsearch (tri-license AGPL-3.0 / SSPL-1.0 / Elastic-2.0 since 2024 — to be verified at the referenced version), n8n (Sustainable Use License, fair-code, not open source), Grafana (AGPL-3.0), MongoDB (SSPL), etc. The reference admission criterion (≥ 1000 stars) says nothing about the license: ours must be explicit.

### 5.1 License of the templates themselves

- The **template** (compose file + metadata + any init scripts) published in the project's template repository is under **Apache-2.0**, like the rest of the project — it is our work, or a substantial rewrite of an imported template. Any template derived from a third-party catalog keeps its **attribution** and is only imported if its license allows it (permissive): the inventory below attests to this.
- The template's license says **nothing** about the license of the software it deploys: the two pieces of information are kept separate everywhere (UI, metadata, docs).
- Templates from **user repositories** (ADR-010) remain under the license chosen by their author; AkerDock neither imposes nor verifies it — it validates the syntax, not the law.

### 5.2 Displaying the deployed software's license (proposed default)

- Mandatory **`license`** field in the template metadata of the official repository: SPDX identifier or expression (`AGPL-3.0-only`, `SSPL-1.0`, `Elastic-2.0 OR SSPL-1.0 OR AGPL-3.0`, `LicenseRef-n8n-Sustainable-Use`…), plus an optional `license_url` field pointing to the upstream license.
- The UI displays license + link **before deployment** (one-click confirmation screen), with a distinct badge for non-OSI licenses ("source available", "fair-code") — information, not blocking: the choice belongs to the user, who runs the software on their own machine.
- The template repository's validation pipeline (ADR-010) rejects an official template without a `license` field; for user repositories, the field is recommended, absent = "unknown license" displayed.
- A multi-image stack carries the license of each significant component (at minimum the main image; ideally a `license` per service).

### 5.3 Logos and trademarks

- The catalog's logos are used **nominatively** (to designate the software a template deploys), which is the classic usage of one-click catalogs — but each trademark remains the property of its holder and some brand guidelines (Elastic, MongoDB, Redis…) strictly govern this usage.
- Internal guidelines: logo **unmodified** (no recoloring, distortion, aggressive cropping), accompanied by the upstream project's name and a **link to the official site**; no logo in a context suggesting affiliation, partnership or certification; a provenance file per logo (source, date, any upstream brand guidelines) in the template repository.
- **Takedown-on-request procedure**: public contact documented in the template repository (`TRADEMARKS.md` file — proposed default); removal or replacement of the logo within 14 days of a verified request from a trademark holder, without contesting by default; the template survives the logo's removal (generic icon).
- The name "AkerDock" itself: note that "Docker" is a trademark of Docker, Inc. — a point to validate (risk of nominal confusion) before public communication; out of scope for this document but flagged.

---

## 6. SBOM, signing and CVE policy

Concretizes §23.5 ("SAST, dependency/container scanning, SBOM and signed images for AkerDock releases") and the catalog's chain of trust (ADR-010).

### 6.1 SBOM generation (per release, in CI)

| Artifact | Tool | Formats | Publication |
|---|---|---|---|
| Go binary (per published OS/arch) | syft (source + binary; supplemented by `go version -m`) | **CycloneDX JSON + SPDX JSON** (both) | Attached to the release (assets `akerdock-<ver>-sbom.cdx.json` / `.spdx.json`) |
| UI bundle (production npm dependencies) | syft on the lockfile/bundle | CycloneDX + SPDX | Merged into or attached to the binary's SBOM (the UI is embedded in it) |
| `AkerDock` image (and every published image: helpers, agent — §4.2) | syft on the image | CycloneDX + SPDX | Release asset **and** attestation attached to the image (`cosign attest --type spdxjson`) |
| Template catalog | Catalog manifest (list of templates + versions + referenced images + licenses §5.2) | Signed JSON | Published with each catalog version |

**(proposed default: syft.** Equivalent alternative: trivy in SBOM mode; what matters is the dual CycloneDX + SPDX format and reproducibility in CI.)

### 6.2 Signing of releases and catalog (proposed default: cosign/Sigstore)

- **Images** published by the project: signed with **cosign in keyless mode** (CI pipeline OIDC, Fulcio certificates, Rekor log) — no long-lived key to protect; the signing identity is the official repository's release workflow.
- **Binaries and release archives**: keyless `cosign sign-blob` + signed SHA-256 checksums; verification instructions documented in the release notes.
- **Template catalog** (ADR-010): the compiled catalog JSON is signed (`cosign sign-blob` — proposed default, consistent with the rest of the chain); the AkerDock instance **verifies the signature before accepting a refresh** of the official catalog; user repositories are not signed by the project (accepted risk, ADR-010).
- Provenance: SLSA/provenance attestation of the build attached to the images **(proposed default, non-blocking for the first release)**.
- Keyless point of attention: ties trust to the CI identity (GitHub Actions OIDC) — document the expected identity so that verification is effective; a project key outside CI remains the alternative if keyless is deemed too coupled to the forge.

### 6.3 Vulnerability scanning and remediation SLA

- **CI**: **grype** scan (or trivy — pick one as blocking, the other optional as a second opinion, proposed default: grype, consistent with syft) on the binary + every published image, on every PR touching dependencies and at every release; SAST (`gosec`/CodeQL) and `govulncheck` (which filters by reachability of Go symbols) as a complement.
- **Post-release**: **scheduled daily** re-scan of the artifacts of the latest stable release (CVEs appear after publication, not only during it).
- **Proposed remediation SLAs** (triggered from the confirmation that a CVE affects a published artifact, with `govulncheck`/reachability analysis being authoritative for the Go binary):

| Severity (CVSS) | Fix or mitigation published | Vehicle |
|---|---|---|
| Critical (9.0+) or actively exploited | **≤ 7 days** | Dedicated patch release + advisory |
| High (7.0–8.9) | **≤ 30 days** | Patch release |
| Medium (4.0–6.9) | **≤ 90 days** | Next minor/patch release |
| Low (< 4.0) | As we go | Next release |

- **Unreachable** CVEs (vulnerable dependency but vulnerable code never called) can be downgraded with a versioned justification (`.grype.yaml`/VEX file — each suppression carries CVE, justification, re-review deadline, like the license exceptions §2.3).
- Base images (distroless) are rebuilt/re-released if a high+ CVE affects the base even without a Go code change.

---

## 7. Contributions: DCO (proposed default)

**DCO (Developer Certificate of Origin, `Signed-off-by` on each commit) rather than a CLA.**

- **For**: near-zero friction (a `-s` flag), standard of Linux/CNCF projects, legally sufficient to attest the right to contribute under Apache-2.0, whose clause 5 already covers the license grant of contributions.
- **Against**: unlike a CLA, no future relicensing power over contributed code — consistent with ADR-020, which records that the Apache-2.0 choice is "not retroactively reversible"; accepted risk.
- Enforcement: DCO check in CI (blocking bot/action), documented in `CONTRIBUTING.md`.

---

## 8. Release checklist

Everything must be true before publishing a binary/image release or a catalog version:

**Licenses**
- [ ] `go-licenses check` and the npm check pass with no new unvalidated exception (§2.2); no expired exception (§2.3).
- [ ] License diff since the previous release reviewed (no dependency changed its upstream license).
- [ ] `LICENSE` intact; `NOTICE` up to date (new Apache-2.0 attributions, derived files §1.2); `THIRD-PARTY-NOTICES` regenerated and embedded in binary + images.

**SBOM and vulnerabilities**
- [ ] CycloneDX + SPDX SBOMs generated for binary/binaries, UI bundle and every published image; attached to the release and attested on the images (§6.1).
- [ ] grype/trivy scan + `govulncheck` + SAST passed; no reachable critical/high CVE without documented mitigation; VEX suppressions up to date (§6.3).

**Signing and integrity**
- [ ] All published images signed (cosign); binaries/checksums signed; signing identity conforming to the documented one (§6.2).
- [ ] Signature verification replayed from a clean environment (the documented command actually works).

**Template catalog** (if catalog release)
- [ ] Validation pipeline passed (compose lint, metadata, magic variables — ADR-010); `license` field present on 100% of official templates (§5.2).
- [ ] Referenced images pinned; non-OSI licenses correctly badged; logo provenance up to date, no pending takedown request (§5.3).
- [ ] Catalog JSON signed and verification tested on the instance side (§6.2).

**Miscellaneous**
- [ ] Release commits DCO-compliant (§7).
- [ ] Default third-party images (proxy, postgres, engines §6.1) still pinned tag + digest, and re-verified if the default tag changed (§4.3).
- [ ] This document updated if an inventory entry (§3, §4.2) changed license, major version or "Nature".
