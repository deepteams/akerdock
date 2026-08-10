# ADR-072 — The manual is Markdown, parsed at boot and served already filtered

- **Status**: Accepted
- **Date**: 2026-08-10
- **Revises**: PRD **§25.4** on two of its clauses — the content format ("structured data
  rendered as text… no HTML, no markdown-to-innerHTML") and *where* the permission filter
  runs. What §25.4 decided about the manual's purpose, its grouping by task, its
  permission-awareness and its unit-tested invariants stands unchanged
- **Related**: [ADR-021](ADR-021-compose-distribution-two-services.md) (one static binary — the manual ships
  inside it, as the dashboard already does), [ADR-025](ADR-025-go-stack-pgx-sqlc-chi-oapi-codegen.md)
  (spec-first: the endpoint below is generated from the OpenAPI contract, not hand-written),
  [ADR-038](ADR-038-roles-model.md) (the permission catalogue the gates name)
- **Related PRD sections**: §12, §18.2, §23.2, §25.4, §26

## Context

The manual is 1 725 lines of TypeScript in `docs.content.ts`: an array of topics, each a tree
of typed block literals. It renders well and it is unreviewable. Changing one sentence shows up
in a diff as a quoted string inside three levels of object literals; adding a paragraph means
choosing a `kind`, matching the surrounding punctuation of a data structure, and re-reading a
comma-separated list to be sure nothing else moved. Nobody proof-reads prose in that shape,
which is exactly what a manual needs most.

The format also puts documentation on the wrong side of the build. It is TypeScript, so it is
compiled into the Angular bundle, so **every reader downloads the whole manual** — including
the chapters their role cannot use. §25.4's filter is real in the UI and cosmetic on the wire:
the instance-administration chapters sit in the bundle of a `reviewer` who will never see them
rendered. That is not a leak of secrets, but it is a promise the architecture does not keep.

## Decision

### 1. The source is Markdown with a YAML front-matter, one file per topic

```
internal/docs/manual/previews.md
---
id: previews
title: PR previews
icon: git-branch
group: Run and debug
summary: An isolated instance per pull request, and what governs it.
order: 5                      # position inside the group
permission: previews:read
gates:
  forks: previews:manage      # this section needs more than the topic
  reviewer-path:              # no gate of its own — same as omitting the key
links:
  - label: Applications
    route: /applications
---

Every pull request gets its own instance…

## Forks

A fork's preview does not run until…
```

The body is **ordinary Markdown**, reviewable as prose, with the metadata where metadata
belongs. `group` is one of six closed values and `order` places the chapter inside its group:
files are read alphabetically, which is not an order anyone reads a manual in, and a group that
is not in the list is a typo rather than a new part of the manual — both are parse errors. A topic's sections are its `##` headings; a section's id is the slug of its title,
which is also its anchor. `gates` attaches a permission — or `root: true` — to a section **on
top of** the topic's own; it never widens access, because a section of a hidden topic is
hidden regardless, and pretending otherwise would be a lie in the front-matter.

### 2. Parsed at boot, embedded in the binary, never generated into the repository

`go:embed` takes `internal/docs/manual/*.md` into the binary (ADR-021: one artifact), and the
manual is parsed once at startup into the structure the API serves. The corpus sits beside its
parser rather than under `docs/` because `go:embed` cannot reach above its own package
directory — a technical constraint, stated here so nobody moves the files back and discovers it
the hard way. They remain plain Markdown files, reviewed in pull requests like any other prose. No generated JSON is committed: a
generated file is one more thing to forget to regenerate, and this content has no consumer
outside the process that embeds it.

**A malformed manual fails the boot**, naming the file, the line and what was wrong. That is
the honest consequence of parsing at runtime rather than at build time, and it is bounded: the
parse reads embedded bytes that shipped with the binary, so it cannot fail on one instance and
succeed on another. A CI test parses the same embed, so the failure is normally found long
before a deployment.

### 3. Markdown renders to HTML, and the safety comes from the parser, not from a sanitiser

§25.4's closed vocabulary was a real protection expressed as a restriction: no HTML meant no
injection. Writing a manual through it means asking "which `kind` is this?" for every
paragraph, which is the friction this ADR removes. So the vocabulary opens, and the protection
moves to where it costs nothing:

- **goldmark parses, and raw HTML is not enabled.** Without `html.WithUnsafe()`, a tag written
  inside a `.md` is **dropped** — replaced by an HTML comment, with the prose around it kept —
  rather than passed through. The HTML the API returns is therefore HTML that **goldmark itself
  produced** from Markdown — headings, paragraphs, lists, tables, code, emphasis, links — and
  not a byte of author-supplied markup. (Stronger than escaping, which was what this decision
  first assumed; the test asserts the behaviour rather than the flag, which is how the
  difference surfaced.)
- **The Angular side binds `[innerHTML]`**, which runs Angular's DomSanitizer. Nothing calls
  `bypassSecurityTrustHtml`; that is asserted by a test, because the whole safety argument
  collapses the day someone adds it to fix a rendering detail.
- **The corpus is the repository's own**, reviewed in pull requests like code. This is a
  defence in depth, not the defence: the two mechanisms above hold even for a file nobody read.

Link targets are checked at parse time: an internal link must resolve to a declared dashboard
route (the invariant §25.4 already required), and an external one must be absolute `https://`.

### 4. The API serves the manual, filtered server-side

`GET /docs` — spec-first in `openapi-v1.yaml`, handler generated like every other
(ADR-025) — returns the topics **the caller may actually read**, sections and blocks included.
`?all=true` returns the whole manual with what is beyond the reader's role **marked** rather
than removed, which is §25.4's opt-in, kept.

The filter is the same predicate as everywhere else: the caller's permission set against the
gate in the front-matter. Moving it server-side is the point — it is what makes §25.4's
sentence true on the wire and not only in the DOM. Each topic carries, per section, the
rendered `html` and the plain `text` extracted from it, so the client's search keeps working
on words rather than on markup.

The endpoint requires authentication and nothing more: a manual is not a secret, and gating it
behind a permission would hide from a reader the explanation of why they cannot do something.

## Consequences

- `docs.content.ts` (1 725 lines) is **deleted**; `docs.model.ts` keeps the types the API
  returns and loses the block union. The article component renders sections rather than
  walking a block tree, and the topic list comes from a fetch instead of an import.
- The bundle shrinks by the manual, and a reader downloads only their own chapters.
- The invariants §25.4 unit-tested in TypeScript move to Go, where the corpus now lives:
  unique ids, icons that exist, links that resolve, permissions that are in the catalogue, and
  a manual that still covers the daily surface for a plain `member`.
- Writing documentation stops requiring a TypeScript build: a `.md` file, `make test`, done.
- **Four affordances of the block vocabulary do not survive**, and are recorded rather than
  quietly dropped: per-snippet copy buttons (a section is one HTML blob now), snippet captions
  (no Markdown equivalent — they became a sentence above the fence), the visual difference
  between an ordered list and a numbered *procedure*, and block-level gates (the front-matter
  gates sections, not paragraphs). The note/warning distinction **was** restored: the parser
  marks a blockquote opening with `**Note**` or `**Warning**`, because a warning that reads
  like an aside is a defect, not a style regression.
- **goldmark becomes a direct dependency** (it is already in the module graph, indirectly).
- Translating the manual, should it ever happen, becomes a directory per language rather than
  a second array of literals. Not decided here, but no longer blocked by the format.

## Alternatives rejected

- **Compiling the Markdown to JSON at `make generate`**: rejected, though it is the discipline
  this repository uses for oapi-codegen and sqlc. Those generate *code* whose shape the
  compiler must see; this generates data one process reads at startup. The committed artifact
  would double every documentation diff and add a synchronisation the CI has to police, to
  save a parse that costs milliseconds once per boot.
- **Compiling in the frontend build**: rejected — it leaves the manual outside the API
  contract, so the server-side filter of §4 becomes impossible and the npm chain becomes a
  dependency of documentation.
- **Keeping the closed block vocabulary, with Markdown as its syntax**: rejected as the worst
  of both — the author still has to know which constructs are allowed, but now discovers it
  from a build error rather than from a type.
- **Rendering Markdown client-side** (shipping `.md` to the browser): rejected — it needs a
  Markdown library and a sanitiser in the bundle, and it puts the parse on every reader's
  machine to save one at boot.

## Verification

Unit tests, per the pyramid (ADR-026/028):

- Parsing: a topic with a front-matter and `##` sections yields the expected ids, titles and
  section slugs; a duplicate id, an unknown icon, an unknown permission, an unresolvable route
  and a malformed front-matter each fail with the file named.
- The embedded corpus parses — the test that turns a boot failure into a CI failure.
- Raw HTML in a `.md` is **escaped, not emitted** (the §3 guarantee, asserted on the parser
  rather than assumed from a flag).
- Filtering: a `reviewer` receives the reviewer-visible topics only; `?all=true` returns the
  rest marked; a section gate hides its section without hiding its topic.
- The endpoint answers the schema the OpenAPI declares, and `text` is the searchable prose of
  the `html` beside it.
- Angular: the article renders the returned HTML through `[innerHTML]`, and **no call to
  `bypassSecurityTrustHtml` exists in the codebase**.
