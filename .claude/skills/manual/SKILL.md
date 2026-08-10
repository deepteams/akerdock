---
name: manual
description: Write or review a page of AkerDock's in-app manual (the /docs pages) — the Markdown format, the front-matter, the permission gates, the house voice, and how to check a page before claiming it is done. Use whenever documentation content is added, moved or corrected.
---

# Writing the in-app manual

The manual is the dashboard's own documentation, at `/docs`. It is **Markdown with a YAML
front-matter**, one file per chapter, under `internal/docs/manual/<id>.md` — beside its parser,
because `go:embed` cannot reach above its own package (ADR-072). It is embedded in the binary,
parsed at boot, and served by `GET /api/v1/docs` **already filtered** to the reader.

Read `docs/adr/ADR-072-manual-as-markdown.md` for why. This is how.

## The file

```markdown
---
id: previews                 # = the filename, the route (/docs/previews), the anchor
title: Pull request previews
icon: git-branch             # must exist in web/src/ui/icon/icons.ts
group: Ship code             # one of the six groups below, exactly
summary: An isolated instance per pull request, and what governs it.
order: 5                     # position inside the group
permission: previews:read    # omit when the chapter needs none
root: true                   # instance-root-only chapter; omit otherwise
gates:                       # a section needing MORE than the chapter
  acting-on-a-preview: previews:manage
  admin-only: root
links:                       # omit when the chapter links nowhere
  - label: Applications
    route: /applications     # a declared dashboard route, or href: https://…
---

Prose before the first heading belongs to the chapter itself.

## Day to day

A section is a `##` heading. Its id is the slug of its title, which is also its anchor
and the key `gates` uses.
```

The six groups, in reading order: **Start here**, **Ship code**, **Run and debug**,
**Automate**, **Your account and team**, **Instance administration**. Anything else fails the
parse — a typo in `group:` is not a new section of the manual.

## The rules the parser enforces (it fails the boot, and CI, not the page)

- `id` equals the filename — which is also what makes ids unique: the directory cannot hold
  the same name twice, so nothing else enforces it.
- `title`, `group`, `summary` are present; the group is one of the six.
- Every `gates` key matches a `##` slug **in that file** — a gate on a section that does not
  exist protects nothing.
- Every permission named exists in `internal/auth`'s catalogue; `root` is spelled `root`.
- Icons exist in the dashboard; internal links resolve to a declared route; external links are
  absolute `https://`.
- Two headings may not slug to the same thing (inline markup is stripped first, so
  `## The **build** pack` and `## The build pack` collide).

## Gates, and what they can and cannot do

A section's gate is **on top of** its chapter's: a section is never more visible than the
chapter holding it. To widen access, move the content to a chapter that is itself open — the
front-matter cannot lie about this, and the API applies the same rule.

Filtering happens **server-side**. A reader who lacks the gate does not receive the content at
all; the "show everything" toggle asks for `?all=true`, which returns it marked `beyond_role`.
So: never write a section that documents *how* to do something as a way of explaining *why*
someone cannot — the reader who needs that explanation is exactly the one who will not receive
it.

## The house voice

The manual is written for the developer who uses AkerDock daily, not for the operator who
installed it. Match what is already there:

- Second person, present tense, concrete. "Drop a `.akerdock` at the root", not "It is possible
  for users to create a configuration file".
- Dense. Every sentence earns its place; no throat-clearing, no marketing, no "simply".
- **Every claim must be true of THIS build.** When a capability moves, the page moves with it.
  If the API cannot do it, the manual does not say it can — and when something is deliberately
  absent (no backup restore, no fork approval in the CLI), say so as a decision, so a reader
  stops looking for a button that is not coming.
- State the *why* where it is not obvious, once, in the reader's terms.
- Instance administration is documented, at the end, never first.

## Markdown you can use

Ordinary CommonMark + GFM tables. Raw HTML is **dropped** by the renderer, so writing `<div>`
gets you nothing — that is the safety property, not a bug to work around. `##` makes a section;
start any deeper heading at `###`. Fences carry no language hint in this corpus. A callout is a blockquote whose
first words are `**Note** — ` or `**Warning** — `: the parser reads that opening and marks the
blockquote, so a warning renders in the warning colour. Any other blockquote is an ordinary
quotation — the marker is the opening word, not the shape.

## Check it before saying it is done

```bash
go test ./internal/docs/          # parse, gates, icons, routes, permissions, reading order
make lint
```

The whole corpus parses in `TestEmbeddedManualParses`; a broken page fails there rather than at
someone's boot. `TestManualCoversTheDailySurfaceForAMember` is the one that fails when a
chapter a plain member needs becomes invisible — read it before changing a `permission:`.

To see a page rendered, run the app (`.claude/skills/verify`) and open `/docs`.

## When you add a chapter

1. Pick its group and its `order:` — renumber neighbours if you insert in the middle.
2. Make sure it is reachable: a chapter nobody links to and no group carries is a chapter
   nobody reads.
3. If it documents a CLI command, the spelling must match `docs/specs/cli.md`, which is the
   contract; the manual quotes it, never the other way round.
