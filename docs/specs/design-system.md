# AkerDock design system — `akd`

> PRD §29.13 artifact. Covers: principles, design tokens, component inventory, interaction patterns, Angular architecture and the addition checklist. References PRD requirements §21 (state machines), §22.5 (accessibility), §25 (dashboard/UX), §5.7 and §13 (logs, terminal), §3.8 (metrics), §19.2 (stale statuses), §22.2 (log backpressure), §23.3 (ANSI neutralization).
>
> Status: revised on 2026-07-18 — the tokens (§2) and the class vocabulary (§3) adopt the Claude Design kit validated by the product owner and replace the initially proposed defaults. The numeric values are verified for WCAG 2.1 AA contrast (ratios computed with the WCAG relative luminance formula, shown in the tables). Any revision goes through a commit on this document; the Storybook catalogue is authoritative for the implementation.

---

## 1. Principles

1. **Functional minimalism** (§25.3). No heavy third-party UI kit. Each component exists because a PRD journey requires it, not for library completeness. Third-party dependencies are limited to specialized needs: xterm.js (terminal), an embeddable code editor, a lightweight charting lib.
2. **Operations-tool density**. The dashboard is an ops tool, not a marketing site: compact tables and lists, dense information, minimal chrome. The default density targets an operator scanning 2,000 resources and 100 servers (§22.2), not a casual reader.
3. **Hierarchy through typography and spacing, not decoration**. No gradients, no heavy drop shadows, no decorative illustrations. Visual structure comes from: text size/weight, vertical spacing, thin borders, and color reserved for semantics (states, actions, links).
4. **One state = a single representation everywhere** (§25.3). A state from the §21 machine (`running`, `failed`, `queued`…) reads exactly the same way on the dashboard, a resource card, a table row, a deployment timeline or a job: same color, same icon, same label, through the single `akd-status-badge` component. Re-styling a state locally is forbidden.
5. **Never color alone**. Each state combines color + icon + text label (WCAG 1.4.1). The `unknown/stale` and `cancelled/superseded` states additionally have a distinct shape (dashed, struck through) legible without color perception.
6. **Accessibility by design, not as a retrofit** (§25.3, §22.5): full keyboard journeys, visible focus, form labels, AA contrast on the single dark theme (§2.7), live announcements for progress and errors.
7. **English, i18n-first** (§25.2). The UI is in English by default; no hard-coded string — every label in this document is the default value of a translation key.

---

## 2. Design tokens

> **Revision note (2026-07-18).** This section replaces the former light-first gray/teal palette (zinc/teal hex scales, two switchable themes) with the tokens of the Claude Design kit "Angular SPA platform for servers" validated by the product owner: single dark theme, colors in `oklch`, "dock light" teal accent, three bundled type families. The values below are normative; `web/src/styles/tokens.css` is their verbatim copy (never the other way around).

The tokens are the sole source of style: **no hard-coded color, size or duration in a component**. Three layers:

- **Canonical tokens** (`--bg-0…3`, `--border-1/2`, `--text-1/2/3`, `--accent*`, state families `--ok/--warn/--danger/--info/--neutral` with `-dim`/`-border` variants, type/space/shape/motion): the surface consumed by new code.
- **Semantic aliases** (`--surface-page`, `--surface-card`, `--surface-terminal`, `--text-body`, `--text-muted`, `--link`…): usage names resolving to the canonical ones.
- **`--akd-*` compatibility layer**: **transitional** aliases for pages predating the redesign; each `--akd-*` resolves to a canonical token. New code does not use it; the layer shrinks with the migration and will be removed with its last consumer.

### 2.1 Colors — surfaces, borders and text

Cool blue-black ramp (`oklch`, hue ~252), single dark theme:

| Token | Value | Usage (alias) |
|---|---|---|
| `--bg-0` | `oklch(14.5% 0.014 252)` | page background (`--surface-page`) |
| `--bg-1` | `oklch(17.5% 0.015 252)` | cards (`--surface-card`) |
| `--bg-2` | `oklch(20.5% 0.016 252)` | raised surfaces, row/button hover (`--surface-raised`) |
| `--bg-3` | `oklch(24% 0.017 252)` | overlays, toasts (`--surface-overlay`) |
| `--bg-inset` | `oklch(12% 0.013 252)` | logs, terminal, input backgrounds (`--surface-terminal`) |
| `--border-1` | `oklch(28% 0.018 252)` | decorative borders, separators |
| `--border-2` | `oklch(36% 0.02 252)` | reinforced borders (input hover, overlays) |
| `--text-1` | `oklch(94% 0.006 250)` | primary text (`--text-body`) |
| `--text-2` | `oklch(74% 0.012 250)` | secondary text (`--text-muted`) |
| `--text-3` | `oklch(56% 0.014 250)` | micro-labels, meta (`--text-faint`) |
| `--text-disabled` | `oklch(44% 0.012 250)` | disabled text |

Verified contrasts (WCAG computation):

| Pair | Ratio | Requirement | Verdict |
|---|---:|---|---|
| primary text `--text-1` / background `--bg-0` | **16.60:1** | 4.5:1 | ✅ |
| primary text `--text-1` / card `--bg-1` | **15.91:1** | 4.5:1 | ✅ |
| secondary text `--text-2` / background `--bg-0` | **8.59:1** | 4.5:1 | ✅ |
| secondary text `--text-2` / card `--bg-1` | **8.23:1** | 4.5:1 | ✅ |
| meta `--text-3` / background `--bg-0` | **4.26:1** | — | ⚠️ see rule |
| log text `--text-2` / `--bg-inset` | **8.81:1** | 4.5:1 | ✅ |

Rules:

- `--text-3` is below the AA text threshold (4.26:1): reserved for **non-essential or redundant** meta (uppercase micro-labels accompanying a value in `--text-1/2`, log timestamps retrievable in the downloaded file, breadcrumb separators). It is **never** the sole carrier of a piece of information.
- The `--border-1/2` borders are **decorative** (< 3:1, 1.4.11 exemption assumed): field identification relies on the `--bg-inset` / surface background contrast, and the focus state on `--accent` (10.40:1 ≥ 3:1).

### 2.2 Colors — teal accent

"Dock light" teal accent (oklch hue 195), reserved for actions, links, selection, focus and active indicators:

| Token | Value | Usage |
|---|---|---|
| `--accent` | `oklch(78% 0.125 195)` | links (`--link`), focus, accent text/indicator |
| `--accent-strong` | `oklch(68% 0.13 195)` | primary button background, checked check/track |
| `--accent-on` | `oklch(16% 0.03 195)` | text/glyph on accent background |
| `--accent-dim` | `oklch(78% 0.125 195 / 0.12)` | subtle backgrounds (active nav, selection, `::selection`) |
| `--accent-border` | `oklch(78% 0.125 195 / 0.35)` | accent-tinted borders |
| `--link-hover` | `oklch(86% 0.11 195)` | links on hover |

Verified contrasts:

| Pair | Ratio | Requirement | Verdict |
|---|---:|---|---|
| link/accent text `--accent` / `--bg-0` | **10.40:1** | 4.5:1 | ✅ |
| primary button: `--accent-on` on `--accent-strong` | **7.22:1** | 4.5:1 | ✅ |
| focus ring `--accent` / `--bg-0` | **10.40:1** | 3:1 (UI) | ✅ |

The primary button is **teal background + dark text** (the dark theme's "inverted" pattern: `--accent-strong` / `--accent-on`), never white on teal. Focus for the kit's controls uses the `--ring-focus` double ring (`--bg-0` then `--accent`, §2.5), legible on any surface.

### 2.3 Semantic state colors — mapping onto the §21 state machines

Five semantic families + two shape modifiers. **Each state of the §21 machines is mapped onto exactly one family**; this mapping is the truth table of the `akd-status-badge` component (§3.10) and it is exhaustive:

| Family | Meaning | §21.1 states (deployment) | §21.2 states (resource/server) | §21.3 states (job) |
|---|---|---|---|---|
| **success** (green) | nominal, happy terminal | `succeeded` | `running` (desired reached), `healthy`, `ready` | `succeeded` |
| **progress** (blue, animated) | transient, in progress | `queued`, `preparing`, `cloning`, `building`, `pushing`, `starting`, `healthchecking`, `switching`, `finishing`, `retrying` | `starting`, `pending`, `validating`, `deleting` | `scheduled`, `queued`, `leased`, `running`, `retry_wait` |
| **warning** (amber) | degraded, attention required | — | `unhealthy`, `maintenance`, desired/observed gap (drift) | — |
| **danger** (red) | failure, unhappy terminal | `failed` | `unreachable`, `missing` | `dead_letter` |
| **neutral** (gray) | intentionally inactive | — | `stopped`, `exited`, `deleted` | — |
| **neutral + dashed border** | outdated information (§19.2: `observed_at` too old → "never a false running") | — | `unknown` / stale | — |
| **neutral + struck-through label** | replaced/abandoned, terminal | `cancelled`, `superseded` | — | `cancelled` |

Each family is carried by a triplet of tokens — solid color (text, dot), translucent `-dim` background (alpha 0.12), translucent `-border` border (alpha 0.35). Same oklch lightness/chroma for all families (78% / 0.125, except danger raised in chroma), only the hue varies:

| Family | fg (solid color) | badge bg | badge border |
|---|---|---|---|
| success | `--ok` = `oklch(78% 0.125 155)` | `--ok-dim` | `--ok-border` |
| progress | `--accent` (hue 195) | `--info-dim` = `oklch(78% 0.125 195 / 0.12)` | `--accent-border` |
| warning | `--warn` = `oklch(78% 0.125 85)` | `--warn-dim` | `--warn-border` |
| danger | `--danger` = `oklch(72% 0.155 25)` | `--danger-dim` | `--danger-border` |
| neutral | `--neutral` = `oklch(70% 0.02 252)` | `--neutral-dim` | `--neutral-border` |

Verified contrasts (state text on page background, and badge text on its `-dim` background composited on `--bg-0`):

| Pair | Ratio | Requirement | Verdict |
|---|---:|---|---|
| success text `--ok` / `--bg-0` | **10.40:1** | 4.5:1 | ✅ |
| success badge `--ok` / `--ok-dim` | **8.63:1** | 4.5:1 | ✅ |
| progress badge `--accent` / `--info-dim` | **8.66:1** | 4.5:1 | ✅ |
| warning text `--warn` / `--bg-0` | **9.82:1** | 4.5:1 | ✅ |
| warning badge `--warn` / `--warn-dim` | **8.24:1** | 4.5:1 | ✅ |
| danger text `--danger` / `--bg-0` | **7.46:1** | 4.5:1 | ✅ |
| danger badge `--danger` / `--danger-dim` | **6.50:1** | 4.5:1 | ✅ |
| danger button: `--danger` on `--danger-dim` | **6.50:1** | 4.5:1 | ✅ |
| neutral badge `--neutral` / `--neutral-dim` | **6.40:1** | 4.5:1 | ✅ |
| status dots (solid color / `--bg-0`, non-text indicator) | **≥ 6.40:1** | 3:1 (UI) | ✅ |

Note: the **progress family shares the accent hue** (195) — a deliberate choice of the kit: "in progress" reads as activity. The status/interactive distinction therefore **never rests on color**: a status is always an `akd-status` pill with a family-specific dot shape + label (§3.6, principle 5); an interactive element never has this shape. The other families remain collision-free with the accent (§25.3).

### 2.4 Typography

Three families, **bundled via `@fontsource`** from `node_modules` (bundled by Angular at build time — the CSP allows no external origin and remains **unchanged**, never a CDN; an air-gapped instance has none anyway):

- **`--font-display`: Space Grotesk** (weights 500/600/700) — page, card and modal titles, stat values.
- **`--font-body`: IBM Plex Sans** (400/500/600) — body, forms, tables, navigation.
- **`--font-mono`: JetBrains Mono** (400/500/700) — logs, terminal, UUIDs, digests, SHAs, URLs, env values, cron.

Each family keeps a system fallback (`system-ui` / `ui-monospace`).

Size scale, 8 steps (default body 14px, never below 10px):

| Token | Size | Usage |
|---|---|---|
| `--text-2xs` | 10px | tab counters, nav sections, ultra-dense meta |
| `--text-xs` | 11px | uppercase micro-labels (field labels, table headers), badges |
| `--text-sm` | 12.5px | meta, hints, logs, mono values, breadcrumb |
| `--text-md` | 14px | **default body**: tables, forms, nav, buttons |
| `--text-lg` | 16px | card/section titles (display) |
| `--text-xl` | 20px | modal titles |
| `--text-2xl` | 26px | page titles, stat values |
| `--text-3xl` | 34px | dashboard hero figures |

Line heights via tokens: `--leading-tight: 1.2` (titles, values), `--leading-normal: 1.5` (body). Weights: `--weight-regular: 400` (body), `--weight-medium: 500` (labels, active nav, tabs, statuses), `--weight-semibold: 600` (card titles, buttons, field labels), `--weight-bold: 700` (**reserved for display**: page h1, stat values — body never exceeds 600, principle 3). Micro-labels are uppercase with `--tracking-wide: 0.06em`. Tabular figures (`font-variant-numeric: tabular-nums`) are mandatory in tables, durations, metrics.

### 2.5 Spacing, radii, elevations, motion

**Spacing** — 9-step scale: `--space-1: 4px`; `-2: 8px`; `-3: 12px`; `-4: 16px`; `-5: 20px`; `-6: 24px`; `-7: 32px`; `-8: 40px`; `-9: 56px`.

**Radii**: `--radius-1: 4px` (badges, checkbox); `--radius-2: 6px` (inputs, buttons, logs); `--radius-3: 10px` (cards, modals, toasts); `--radius-full: 999px` (status pills, switch).

**Elevations** — hierarchy comes first from the surface color (`--bg-0…3`); shadows reinforce the floating layers:

```
--shadow-1: 0 1px 2px oklch(0% 0 0 / 0.4);
--shadow-2: 0 4px 16px oklch(0% 0 0 / 0.45);   /* toasts, popovers, dropdowns */
--shadow-3: 0 16px 48px oklch(0% 0 0 / 0.55);  /* modals */
--ring-focus: 0 0 0 2px var(--bg-0), 0 0 0 4px var(--accent);  /* double focus ring */
```

**Motion** — fast, never blocking:

```
--dur-1: 120ms;  /* hover, focus, button transitions */
--dur-2: 200ms;  /* toasts, switch, dropdowns, tooltips */
--dur-3: 350ms;  /* modals, side panels */
--ease-out: cubic-bezier(0.2, 0.8, 0.2, 1);
```

Under `prefers-reduced-motion: reduce`: all durations drop to `1ms`, the "progress" state animations (badge pulse, timeline spinner, skeleton shimmer) are replaced by equivalent static representations (fixed icon, "In progress" text). No information is carried by motion alone.

### 2.6 Reference CSS block (copied as-is by `web/src/styles/tokens.css`)

```css
/* Design tokens — copied verbatim from docs/specs/design-system.md §2, which
   is normative (source: the AkerDock UI kit, dark-first). This file is the
   ONLY place a literal colour, size or duration may appear: components consume
   the CSS variables below and nothing else.
   Do not hand-edit — change the spec, then re-copy. */

/* ---- colors (dark-first, single theme) ---- */
:root {
  color-scheme: dark;
  /* Surfaces (cool blue-black ramp) */
  --bg-0: oklch(14.5% 0.014 252);
  --bg-1: oklch(17.5% 0.015 252);
  --bg-2: oklch(20.5% 0.016 252);
  --bg-3: oklch(24% 0.017 252);
  --bg-inset: oklch(12% 0.013 252);
  /* Borders */
  --border-1: oklch(28% 0.018 252);
  --border-2: oklch(36% 0.02 252);
  /* Text */
  --text-1: oklch(94% 0.006 250);
  --text-2: oklch(74% 0.012 250);
  --text-3: oklch(56% 0.014 250);
  --text-disabled: oklch(44% 0.012 250);
  /* Accent — teal (dock light) */
  --accent: oklch(78% 0.125 195);
  --accent-strong: oklch(68% 0.13 195);
  --accent-on: oklch(16% 0.03 195);
  --accent-dim: oklch(78% 0.125 195 / 0.12);
  --accent-border: oklch(78% 0.125 195 / 0.35);
  /* Semantic — same lightness/chroma, hue varies */
  --ok: oklch(78% 0.125 155);
  --ok-dim: oklch(78% 0.125 155 / 0.12);
  --ok-border: oklch(78% 0.125 155 / 0.35);
  --warn: oklch(78% 0.125 85);
  --warn-dim: oklch(78% 0.125 85 / 0.12);
  --warn-border: oklch(78% 0.125 85 / 0.35);
  --danger: oklch(72% 0.155 25);
  --danger-dim: oklch(72% 0.155 25 / 0.12);
  --danger-border: oklch(72% 0.155 25 / 0.35);
  --info: oklch(78% 0.125 195);
  --info-dim: oklch(78% 0.125 195 / 0.12);
  --neutral: oklch(70% 0.02 252);
  --neutral-dim: oklch(70% 0.02 252 / 0.12);
  --neutral-border: oklch(70% 0.02 252 / 0.35);
  /* Semantic aliases */
  --surface-page: var(--bg-0);
  --surface-card: var(--bg-1);
  --surface-raised: var(--bg-2);
  --surface-overlay: var(--bg-3);
  --surface-terminal: var(--bg-inset);
  --text-body: var(--text-1);
  --text-muted: var(--text-2);
  --text-faint: var(--text-3);
  --link: var(--accent);
  --link-hover: oklch(86% 0.11 195);

  /* ---- typography (fonts bundled via @fontsource, no external origin) ---- */
  --font-display: 'Space Grotesk', system-ui, sans-serif;
  --font-body: 'IBM Plex Sans', system-ui, sans-serif;
  --font-mono: 'JetBrains Mono', ui-monospace, monospace;
  --text-2xs: 10px;
  --text-xs: 11px;
  --text-sm: 12.5px;
  --text-md: 14px;
  --text-lg: 16px;
  --text-xl: 20px;
  --text-2xl: 26px;
  --text-3xl: 34px;
  --weight-regular: 400;
  --weight-medium: 500;
  --weight-semibold: 600;
  --weight-bold: 700;
  --leading-tight: 1.2;
  --leading-normal: 1.5;
  --tracking-wide: 0.06em; /* uppercase micro-labels */

  /* ---- spacing, radii, shadows, motion ---- */
  --space-1: 4px; --space-2: 8px; --space-3: 12px; --space-4: 16px;
  --space-5: 20px; --space-6: 24px; --space-7: 32px; --space-8: 40px; --space-9: 56px;
  --radius-1: 4px; --radius-2: 6px; --radius-3: 10px; --radius-full: 999px;
  --shadow-1: 0 1px 2px oklch(0% 0 0 / 0.4);
  --shadow-2: 0 4px 16px oklch(0% 0 0 / 0.45);
  --shadow-3: 0 16px 48px oklch(0% 0 0 / 0.55);
  --ring-focus: 0 0 0 2px var(--bg-0), 0 0 0 4px var(--accent);
  --ease-out: cubic-bezier(0.2, 0.8, 0.2, 1);
  --dur-1: 120ms; --dur-2: 200ms; --dur-3: 350ms;
}

/* ---------------------------------------------------------------------------
   Compatibility aliases — the pre-redesign pages consume var(--akd-*). Each
   alias resolves to a token above so those pages render in the new language
   without edits. New code uses the canonical tokens; this block shrinks as
   pages migrate and is deleted when the last consumer is gone.
--------------------------------------------------------------------------- */
:root {
  --akd-bg:            var(--bg-0);
  --akd-surface:       var(--bg-1);
  --akd-surface-hover: var(--bg-2);
  --akd-border:        var(--border-1);
  --akd-border-input:  var(--border-2);
  --akd-text:          var(--text-1);
  --akd-text-secondary:var(--text-2);
  --akd-text-muted:    var(--text-3);
  --akd-text-disabled: var(--text-disabled);
  --akd-accent:        var(--accent);
  --akd-accent-hover:  var(--link-hover);
  --akd-accent-subtle: var(--accent-dim);
  --akd-on-accent:     var(--accent-on);
  --akd-focus-ring:    var(--accent);
  --akd-status-success-fg: var(--ok);      --akd-status-success-bg: var(--ok-dim);      --akd-status-success-dot: var(--ok);
  --akd-status-progress-fg: var(--accent); --akd-status-progress-bg: var(--info-dim);   --akd-status-progress-dot: var(--accent);
  --akd-status-warning-fg: var(--warn);    --akd-status-warning-bg: var(--warn-dim);    --akd-status-warning-dot: var(--warn);
  --akd-status-danger-fg: var(--danger);   --akd-status-danger-bg: var(--danger-dim);   --akd-status-danger-dot: var(--danger);
  --akd-status-neutral-fg: var(--neutral); --akd-status-neutral-bg: var(--neutral-dim); --akd-status-neutral-dot: var(--neutral);
  --akd-danger: var(--danger);
  --akd-danger-hover: var(--danger);
  --akd-on-danger: var(--bg-0);
  --akd-log-bg: var(--bg-inset);
  --akd-log-fg: var(--text-2);
  --akd-log-meta: var(--text-3);
  --akd-font-ui: var(--font-body);
  --akd-font-mono: var(--font-mono);
  --akd-text-2xs: var(--text-2xs); --akd-text-xs: var(--text-xs); --akd-text-sm: var(--text-sm);
  --akd-text-md: var(--text-md); --akd-text-lg: var(--text-lg); --akd-text-xl: var(--text-xl);
  --akd-text-2xl: var(--text-2xl);
  --akd-weight-regular: var(--weight-regular); --akd-weight-medium: var(--weight-medium);
  --akd-weight-semibold: var(--weight-semibold);
  --akd-space-05: 2px;
  --akd-space-1: var(--space-1); --akd-space-2: var(--space-2); --akd-space-3: var(--space-3);
  --akd-space-4: var(--space-4); --akd-space-5: var(--space-5); --akd-space-6: var(--space-6);
  --akd-space-8: var(--space-7); --akd-space-10: var(--space-8); --akd-space-12: 48px;
  --akd-space-16: 64px;
  --akd-radius-xs: 2px; --akd-radius-sm: var(--radius-1); --akd-radius-md: var(--radius-2);
  --akd-radius-lg: var(--radius-3); --akd-radius-full: var(--radius-full);
  --akd-shadow-1: var(--shadow-1); --akd-shadow-2: var(--shadow-2); --akd-shadow-3: var(--shadow-3);
  --akd-duration-fast: var(--dur-1); --akd-duration-base: var(--dur-2); --akd-duration-slow: var(--dur-3);
  --akd-ease: var(--ease-out);
}

@media (prefers-reduced-motion: reduce) {
  :root { --dur-1: 1ms; --dur-2: 1ms; --dur-3: 1ms; }
}
```

> Implementation note: the second `:root` block is the **transitional `--akd-*` compatibility layer** (§2, third layer). It lets pages predating the redesign render in the new language without modification; it introduces no new value (every alias resolves to a canonical token) and will be removed with its last consumer.

### 2.7 Theme handling

- **Single dark theme** (decision recorded with the validated kit: the design kit was validated dark-only by the product owner, and an operations tool is mostly consulted in dark contexts — server room, night on-call, terminals). There is **no more light/dark toggle nor `prefers-color-scheme`**: the theme is no longer a user preference (no localStorage, no account preference, no `data-theme`).
- `color-scheme: dark` is set on `:root` so that scrollbars, native controls and `<select>` follow.
- `prefers-reduced-motion` remains honored (§2.5): removing the light theme takes away no accessibility adaptation.
- Contrast is tested in CI on this single theme (see §6).

---

## 3. Component inventory

All components are **standalone** Angular components prefixed `akd-` (§5). Cross-cutting conventions, applicable to the whole inventory:

- **Visible focus**: global rule `outline: 2px solid var(--accent); outline-offset: 2px;`; the kit's controls (buttons, inputs, tabs, switch…) use the double ring `box-shadow: var(--ring-focus)` (§2.5), legible on any surface. Never removed, never replaced by a mere color change. Ring/background ratio 10.40:1 ≥ 3:1 (§2.2).
- **Touch/click target**: interactive height ≥ 32px at default density (desktop tool, justified derogation to 44px mobile for the §22.4 emergency actions).
- **Disabled**: `--akd-text-disabled` + `cursor: not-allowed` + `aria-disabled` (disabled buttons remain focusable to stay discoverable by screen readers, with a tooltip explaining why).
- **i18n**: every rendered string is a translation key, including `aria-label`s.

The inventory's visual vocabulary is implemented by **global `.akd-*` CSS classes** (BEM convention, `web/src/styles.css`, taken from the kit — tokens only), which the Angular components compose:

| Component | Kit classes |
|---|---|
| Buttons (§3.1) | `.akd-btn` + modifiers `--primary` / `--secondary` / `--ghost` / `--danger`, size `--sm`; icon button `.akd-iconbtn` (+ `--bordered`) |
| Fields (§3.2) | `.akd-field` (`__label`, `__hint`, `__hint--error`), `.akd-input` (+ `--mono`, `--error`), `.akd-select` wrapper around a native `<select>` |
| Checkboxes / toggle (§3.3) | `.akd-check`, `.akd-switch` |
| Statuses (§3.6) | `.akd-status` pill + `--ok` / `--progress` / `--warn` / `--danger` / `--neutral`, `.akd-status__dot` dot (shape per family); informative badges `.akd-badge` (+ `--mono`, `--accent`, `--ok`, `--warn`, `--danger`) |
| Tables (§3.5) | `.akd-table` (+ `--clickable`), mono values `.akd-mono` |
| Cards (§3.7) | `.akd-card` (`__header`, `__title`, `__body`) |
| Tabs (§3.8) | `.akd-tabs`, `.akd-tab` (+ `--active`), counter `.akd-tab__count` |
| Modals (§3.9–3.10) | `.akd-modal-backdrop`, `.akd-modal` (+ `--danger`), `__header` / `__body` / `__footer` |
| Toasts (§3.11) | `.akd-toast` (+ `--ok`, `--warn`, `--danger`), `__icon`, `__title`, `__msg` |
| Timeline (§3.12) | `.akd-timeline`, `.akd-tstep` (+ `--done`, `--active`, `--failed`, `--pending`), `__rail` / `__node` / `__line` / `__title` / `__dur` / `__detail` |
| Log viewer (§3.13) | `.akd-log`, `.akd-log__line` (+ `--info`, `--ok`, `--warn`, `--error`, `--cmd`), `__ts`, `__msg` |
| Stats / metrics (§3.16) | `.akd-stat` (`__label`, `__value`, `__unit`, `__delta`) |
| Empty state (§3.18) | `.akd-empty` (`__icon`, `__title`, `__msg`) |
| Navigation (§3.20) | `.akd-breadcrumb` (`__sep`, `__current`), `.akd-sidenav` (`__section`, `__item`, `__item--active`) |
| Page template | `.akd-page`, `.akd-bar` (h1/h2 in `--font-display`), `.akd-error`, `.akd-secret`, `.akd-dl`, `.akd-muted`, `.sr-only` |

A **compatibility layer** at the end of `styles.css` renders the old class dialect (bare `.akd-btn` = primary, standalone `.akd-btn-ghost` / `.akd-btn-danger`, `select.akd-select`, tabs marked by `aria-selected`, auto-padded cards) in the new language; like the `--akd-*` layer (§2.6), it disappears as the last pages migrate.

### 3.1 `akd-button`

- **Anatomy**: container, label, optional icon (left or alone), built-in spinner in the loading state.
- **Variants** (`.akd-btn--*` modifiers): `primary` (background `--accent-strong`, text `--accent-on`, 7.22:1); `secondary` (background `--bg-2`, border `--border-1`, text `--text-1`); `danger` (tinted: background `--danger-dim`, border `--danger-border`, text `--danger`, 6.50:1) — reserved for destructive actions; `ghost` (transparent background, text `--text-2`, hover `--bg-2`) for tertiary actions in tables. Sizes: default 36px, `sm` (28px, tables and toolbars); icon button `.akd-iconbtn` 32px with mandatory `aria-label`.
- **States**: default, hover, active, focus-visible, disabled, **loading** (spinner + label kept + `aria-busy="true"`, clicks ignored without the button disappearing).
- **A11y**: native `<button>` (never a `div`), explicit `type`; icon alone ⇒ mandatory `aria-label` (enforced by lint); native Enter/Space.

### 3.2 `akd-input`, `akd-select`, `akd-textarea` + inline validation

- **Anatomy** (via the `akd-field` wrapper): **always visible** label (never placeholder-as-label, §22.5), control, help text, error message below the field, optional prefix/suffix (unit, icon, reveal button).
- **Variants**: sizes `sm`/`md`; `akd-input` supports `type` text/number/password/URL; `mono` variant (UUIDs, digests, domains — `--akd-font-mono`); `akd-select` = styled native `<select>` (no custom listbox in P0: the native one is accessible for free); `akd-textarea` vertically resizable with optional counter.
- **States**: default, hover, focus (ring), disabled, readonly, **invalid** (border + error text `--akd-status-danger-fg`, error icon — never the red border alone); inline validation on `blur` then on keystroke once the field is in error (§25.1 "inline validation").
- **A11y**: explicit `<label for>`; error linked via `aria-describedby` + `aria-invalid="true"`; help and error are in the same `aria-describedby`; error announced through the form's live region (§4.1).

### 3.3 `akd-checkbox`, `akd-radio`, `akd-toggle`

- **Anatomy**: native control (`input type=checkbox/radio`) visually replaced, clickable label on the right, optional description.
- **Variants**: tri-state checkbox (`indeterminate`, for table selection §3.9); radio only within an `akd-radio-group` (fieldset+legend); toggle = checkbox styled as a switch, reserved for **immediate-effect** settings — in a submitted form, use a checkbox (product convention).
- **States**: unchecked/checked/indeterminate, hover, focus-visible (ring on the control), disabled. Checked check/track in `--akd-accent` with `--akd-on-accent` glyph.
- **A11y**: native controls ⇒ roles and keyboard for free (Space; arrows within a radio group); the toggle carries `role="switch"` + `aria-checked`; the state is never indicated by color alone (thumb position + On/Off label).

### 3.4 PRD §25.1 field variants — `akd-field` states (PRD requirement)

The PRD requires systematically distinguishing: **saved value, inherited value, generated value, locked secret, undeployed change**. Normalized representations, combinable (e.g. inherited + undeployed), carried by the `akd-field` wrapper:

| Variant | Visual representation | Behavior |
|---|---|---|
| **Saved** | default appearance, no decoration | — |
| **Inherited** (e.g. server variable PRD §3.1 or shared var) | gray `Inherited` chip to the right of the label + value displayed in `--akd-text-secondary` italic + provenance tooltip/line ("From server: staging-1") | `Override` button turns the field into its own value; `Reset to inherited` to go back |
| **Generated** (UUID, wildcard domain, displayable credential) | `Generated` chip + value in `--akd-font-mono` + built-in copy action (`akd-copy-field`, §3.19) | regeneration possible via an explicit confirmed action |
| **Locked secret** (locked secret §23.2) | padlock icon in the field, value masked `••••••••` (fixed length, does not reveal the real size), `Secret` chip | write-only by default: can be replaced, never read back; `Reveal` button visible only if the product allows it and `read:sensitive` is present; reveal is audited |
| **Undeployed change** (not yet deployed) | amber dot `●` next to the label + `Not deployed` chip (`--akd-status-warning-*`) on the modified field | aggregated in the form's dirty state bar (§4.1): "3 changes not deployed — Deploy / Discard" |

- **A11y**: each chip has text (never icon alone); the state is repeated in the field's `aria-describedby` ("Inherited from server staging-1", "Changed, not deployed") so it is announced to screen readers; chip colors pass AA badge (§2.3).

### 3.5 `akd-table` (dense table)

- **Anatomy**: semantic `<table>` — caption (visually hidden if redundant), sticky thead, rows, selection cell, footer with pagination.
- **Variants**: density `compact` (32px/row, default for ops lists) / `comfortable` (40px); alignable columns; specialized cells: `akd-status-badge`, truncated mono values with copy, actions cell (ghost `sm` buttons + overflow menu).
- **Sorting**: sortable headers = `<button>` inside `<th aria-sort="ascending|descending|none">`; visible arrow; server-side sorting.
- **Cursor pagination** (§22.2: pagination mandatory, no offset): Prev/Next buttons + page size; no page number; the total is optional/approximate.
- **Selection**: checkbox per row + tri-state header checkbox; bulk action bar appearing above the table (announced by a live region: "4 rows selected").
- **States**: row hover, selected (`--akd-accent-subtle` + accent left border, never background alone), loading (skeleton rows §3.21), empty (EmptyState §3.20), load error (Alert + retry).
- **A11y**: row-by-row keyboard navigation optional but all internal controls tabbable in visual order; the sticky header never hides the focused element (scroll-margin).

### 3.6 `akd-status-badge` — the centerpiece

Single state-rendering component (§25.3: "normalized visual states"; principle 4). Consumes exclusively the §2.3 mapping table, generated from the OpenAPI state enums (§24.1) — the mapping's exhaustiveness is verified by a test: **any §21 state without a mapping entry fails CI**.

- **Anatomy**: rounded pill (`--radius-full`, class `.akd-status`), **dot + text label — never color alone**, translucent `-dim` background, `-border` border, text in the family's solid color (§2.3).
- **Dot shapes per family** (`.akd-status__dot` — the shape distinguishes families without color perception): success **solid circle**, progress **diamond** (rotated square) pulsing (static under reduced-motion), warning **triangle**, danger **square**, neutral **hollow ring**; unknown/stale: neutral + **dashed border** (`border: 1px dashed`), cancelled/superseded: `⊘` + **struck-through label** (`text-decoration: line-through`).
- **Variants**: `badge` (default); `dot` (dot + text without background, for ultra-dense tables — the dot meets 3:1 UI, §2.3); `dot-only` **forbidden** except where the label is adjacent within the same cell.
- **Specific behaviors**:
  - **stale** (§19.2): as soon as `observed_at` exceeds the threshold, the badge switches to `Unknown` with the tooltip "Last observed 12 min ago" — never a false `Running`.
  - **superseded** (§21.1): the tooltip/link points to the superseding deployment.
  - desired/observed divergence (§21.2): the badge shows the **observed** state; the gap with the desired state is rendered by a second warning `Drift` badge next to it, not by mixing colors.
- **A11y**: `role="status"` is **not** used (no spontaneous announcement in tables); the label text is readable as-is; the progress animation is purely decorative (the information is in the text).

### 3.7 `akd-card` / `akd-panel`

- **Anatomy**: `--surface-card` surface, `--border-1` border, `--radius-3` radius; header zone (`.akd-card__header`: title `.akd-card__title` in `--text-lg` `--font-display` + actions) / body (`.akd-card__body`) / footer.
- **Variants**: `card` (content block); `panel` (page section, no shadow); clickable `card` (the whole card = a single link, secondary actions remain distinct controls); dashboard resource card (title, `akd-status-badge`, meta, sparkline).
- **States**: default, hover (clickable cards), visible focus-within.
- **A11y**: the card title is a heading of the correct level in the page outline; clickable card = `<a>` extended by a pseudo-element, no `div onclick`.

### 3.8 `akd-tabs`

- **Anatomy**: bar of underlined tabs (no background — principle 3), active indicator `--akd-accent` 2px, panels.
- **Variants**: resource detail navigation (Configuration / Environment / Storage / Health / Deployments / Logs / Terminal — §25.1) — in this case the tabs are **routed links** (navigation `role`, deep URL per tab); local tabs (ARIA tabs) for non-routed subsections.
- **States**: active (accent + `--akd-weight-medium`), hover, focus-visible, optional counter badge (e.g. "Deployments 3").
- **A11y**: local variant: `role="tablist/tab/tabpanel"`, `aria-selected`, left/right arrows + Home/End, activation on focus; routed variant: `<nav>` + `aria-current="page"`, no tab role.

### 3.9 `akd-modal`

- **Anatomy**: native `<dialog>` (or equivalent with focus trap), overlay, header (title + close button), scrollable body, actions footer (primary action on the right).
- **Variants**: `sm` 400px (confirmations), `md` 560px (short forms), `lg` 800px (config diff, cascade preview §19.2). Long forms remain pages, not modals.
- **States**: animated open/close (`--akd-duration-slow`, simple fade under reduced-motion).
- **A11y**: `aria-modal="true"`, `aria-labelledby` = title; focus trap; on open, focus goes to the first relevant element (never the destructive action); Esc closes (except with a job in progress, in which case Esc asks for confirmation); on close, focus returns to the triggering element.

### 3.10 `akd-confirm-modal` — reinforced confirmation (§22.5)

Single pattern for **every** destructive action (§25.3: "every destructive action follows the same pattern"): data deletion, restore, CA rotation, root terminal, destructive cloud operations, stopping the proxy (cuts incoming traffic, PRD §4.1), previewed cascade deletion (§19.2).

- **Anatomy**: explicit title ("Delete application 'api-prod'"), **list of concrete consequences** ("3 volumes and their data will be permanently deleted", "The Hetzner VPS will NOT be deleted" PRD §3.2), warning zone `--akd-status-danger-bg`, **input field for the exact resource name**, `danger` button disabled until the input matches (exact comparison, case-sensitive, no silent trim), Cancel button focused by default.
- **Variants**: `type-to-confirm` (destructive default); `checklist` (restore: check "I understand current data will be overwritten"); both combinable.
- **States**: non-matching input (disabled button + help "Type the resource name to confirm"), matching (active button), submission (loading, field locked, cancellation impossible once the job is launched — tracking moves to the job §4.2).
- **A11y**: the confirmation field has an explicit label; the button's activation is announced (`aria-live="polite"` on the help text); pasting is not blocked (pasting is allowed: the intended friction is reading, not typing) — an explicit decision, consistent with WCAG.

### 3.11 `akd-toast` / `akd-alert`

- **`akd-toast`** (transient, bottom-right corner, stack of max 3): anatomy semantic-family icon + message + optional action ("View job") + close. Auto-dismiss 6s **except** errors (persistent until closed). Container `aria-live="polite"` (`assertive` for errors); never the sole channel for a blocking error (the state remains visible in the page).
- **`akd-alert`** (persistent, in the flow): variants `info/success/warning/danger` over the 5 families §2.3; anatomy icon + title + body + actions; used for: unreachable server at the top of a server page, disk warning, "update available", data warnings (§25.1 "Database"). Dismissible only if the information can be found elsewhere.
- **A11y**: `role="alert"` only for dynamically inserted danger alerts; AA contrast verified on tinted backgrounds (§2.3).

### 3.12 `akd-deployment-timeline` (§21.1)

Rendering of the deployment state machine, step by step.

- **Anatomy**: ordered vertical list of steps — `queued → preparing → cloning → building → pushing (if registry) → starting → healthchecking → switching → finishing` — each with: state icon (same families as `akd-status-badge`), name, **duration** (`tabular-nums`; live tick for the current step), timestamp on hover, link to the corresponding log section (§3.13). Colored vertical connector up to the current step.
- **Variants**: full (deployment page, with durations and log links); compact (deployment list: steps as condensed dots); terminal: `succeeded` (all green), `failed` (faulty step in red, subsequent ones grayed `Skipped`), `cancelled`/`superseded` (current step struck through, link to the replacement for superseded), `retrying` (new attempt linked, never rewriting history — attempt N displayed, §21.1).
- **States**: step done (success), active (animated progress), pending (neutral), failed (danger), skipped (dimmed neutral).
- **A11y**: semantic `<ol>`; the current step carries `aria-current="step"`; progress is announced by the job page's live region (§4.2), not by the timeline itself; durations announced in readable units.

### 3.13 `akd-log-viewer` (§5.7, §22.2, §23.3)

- **Anatomy**: toolbar (search, level filters if structured, follow/pause, wrap, timestamps on/off, download, full screen); **virtualized** log area (windowed rendering, mandatory — target: tens of thousands of lines without degradation); background `--surface-terminal` (`--bg-inset`), text `--akd-log-fg` (`--text-2`, 8.81:1), timestamps `--akd-log-meta` (`--text-3`, 4.37:1 — redundant meta, the downloaded file is authoritative) in `--font-mono`.
- **Functions required by the PRD**:
  - **Search** within the loaded buffer, AA highlighting of matches, n/N navigation.
  - **Collapsible sections**: build logs are grouped by timeline step (§3.12); chevron per section, collapsed state persisted; error ⇒ section automatically expanded.
  - **Timestamps** aligned on the **target server's timezone** (§5.7) with explicit timezone indication (§22.3: "display in the user/server timezone with explicit indication"); UTC/server toggle.
  - **Download** of the complete log (raw file).
  - **ANSI neutralized** (§23.3): escape sequences stripped or mapped onto a restricted color set **retranslated into AA tokens**; all HTML escaped; never any markup injection from the content.
  - **Backpressure** (§22.2): bounded buffer, cursor-based resumption; if lines are dropped, insertion of a **non-removable inline marker** "⚠ N lines dropped (buffer overflow) — Download full log" styled as warning. Silence is forbidden.
  - **Follow mode**: pinned to the bottom; any manual scroll pauses it with a floating button "Resume following (+128 new lines)".
- **States**: streaming (live indicator), paused, disconnected (warning banner + auto reconnect), finished, empty.
- **A11y**: log area `role="log"` (`aria-live="off"` by default — the stream would be unusable aurally; the summary goes through the job's live region); fully keyboard-operable toolbar; PageUp/PageDown/Home/End within the area; focus is never stolen by auto-scroll.

### 3.14 `akd-code-editor`

Wrapper around an existing embeddable editor (proposed default: **CodeMirror 6** — modular, lightweight, decent keyboard accessibility, no heavy Monaco-style dependency) for: env files, compose (§25.1 "Validated editor"), proxy config, custom Fluent Bit (§13).

- **Anatomy**: toolbar (language, validation, before/after diff §25.1), line-number gutter, editing area, footer (cursor position, validation errors).
- **Variants**: `env` (key=value highlighting, optional masking of secret values), `yaml/compose` (inline schema validation, errors underlined + listed below the editor), `diff` (read-only, two panes or inline, for a deployment's config diff §25.1); inline editing limit 5 MiB (§23.3) — beyond that, read-only + download.
- **States**: editable, read-only, invalid (clickable error list), dirty (wired to the form's dirty state §4.1).
- **A11y**: "Escape leaves the editor" mode documented and announced (controlled tab trap, WCAG 2.1.2); syntax highlighting theme derived from the tokens, verified AA; all errors available as a text list outside the editor.

### 3.15 `akd-terminal` (§5.7, §13)

**xterm.js** container (assumed specialized dependency, §25.3): shell into any container or managed server via WebSocket → SSH.

- **Anatomy**: session bar (target: server/container + context badge, connection indicator, reconnect button, kill), xterm area (background `--akd-log-bg`), scrollback.
- **Variants**: container, server, **root** (access preceded by `akd-confirm-modal` §3.10, audited session §23.4 — persistent banner "Root session — audited").
- **States**: connecting, connected, reconnecting (reconnection §5.7: banner + attempts), disconnected (overlay with cause + button), terminated.
- **A11y**: xterm.js `screenReaderMode` toggleable in the session bar; terminal sequences limited on the display side (§23.3); focus: click/Enter enters the terminal, **Ctrl+Shift+Escape leaves it** (shortcut documented in the bar — Escape alone belongs to the shell); adjustable font sizes.

### 3.16 `akd-metric-chart` (§3.8, §13)

Server and per-container CPU/RAM/disk charts (Sentinel data, push ~10 s, disk ~60 s).

- **Anatomy**: title + current value (large, `tabular-nums`) + chart (area or line) + discreet axes + tooltip on hover/focus + window selector (1h/6h/24h/7d depending on configured retention).
- **Variants**: `sparkline` (mini curve without axes in cards and table rows — always accompanied by the current numeric value); `full` (server/resource page); disk alert thresholds rendered as a dashed warning line.
- **Colors**: series in teal accent and gray — the semantic state colors remain reserved for threshold breaches; below threshold, a chart is never red/green.
- **States**: loading (skeleton), missing data ("No metrics — Sentinel not enabled on this server" + activation link, and explicit mention of the compose limitation §3.8), **stale** (last point too old: hatched zone + Unknown badge, consistent §19.2), error.
- **A11y**: each chart has a text summary (`aria-label`: "CPU usage, current 42%, average 38% over last hour"); data points are keyboard-explorable (arrows) with a tooltip; information is never carried by series color alone (legend + point shapes if multi-series).

### 3.17 `akd-copy-field` (§22.5)

For any generated value: UUIDs, domain, internal/external URLs, displayable credentials (§25.1 "Database").

- **Anatomy**: value in `--akd-font-mono` (middle-truncated for long values, full title), **clear context** (label: "Internal URL", "Webhook secret") required by §22.5, copy button.
- **Variants**: inline (within a sentence/cell); block (with label); `secret` (masked by default, reveal subject to the same rules as §3.4; **copy works without reveal**; secret copy/reveal audited §23.4).
- **States**: default, copied (check icon + "Copied" for 2 s, announced in a polite live region), reveal on/off.
- **A11y**: button with contextualized `aria-label` ("Copy internal URL"); the copy confirmation is announced, not merely visual; the value is keyboard-selectable.

### 3.18 `akd-empty-state`

- **Anatomy**: discreet icon (no decorative illustration — principle 3), short title, one explanatory sentence, optional primary action and doc link.
- **Variants**: first use ("No servers yet — Add your first server", links to onboarding §25.1); empty filter result ("No deployments match your filters" + Clear filters); load-error state (danger family + Retry); capability not enabled (e.g. metrics without Sentinel §3.16).
- **A11y**: title = heading; the primary action is a real focusable button/link; no `role="alert"` (stable state, not an event).

### 3.19 `akd-skeleton`

- **Anatomy**: `--akd-surface-hover` blocks sized to the expected content (table rows, card, chart), subtle shimmer (`--akd-duration-slow`); **static** under reduced-motion.
- **Variants**: `text`, `row` (table), `card`, `chart`.
- **Rules**: only for initial loading (< a few expected seconds); long actions use the job pattern (§4.2), not a skeleton; never a skeleton over already displayed content (no flash on refetch — the old content stays with a discreet indicator).
- **A11y**: `aria-busy="true"` container on the loading region; the skeletons themselves are `aria-hidden="true"`.

### 3.20 `akd-breadcrumb` + `akd-side-nav` (Team → Project → Environment → Resource hierarchy)

- **`akd-breadcrumb`**: anatomy `<nav aria-label="Breadcrumb">` + `<ol>` — Team / Project / Environment / Resource, each segment clickable, last segment `aria-current="page"` non-clickable; intermediate segments = **switchers** (dropdown on click to change project/environment without going back up); middle truncation on narrow screens (first + last always visible).
- **`akd-side-nav`**: side navigation by functional domain (Dashboard, Servers, Projects, Security, Settings — aligned with lazy loading §25.2); anatomy: team selector at the top (security boundary §23.1 — switching teams is global and explicit), items with icon + label, alert counters (unreachable servers, failed deployments — dashboard §25.1) via badge, collapsible section, footer (user).
- **States**: active item (`--akd-accent-subtle` + accent left bar + `--akd-weight-medium` — never color alone), hover, focus-visible; nav collapsible to icons (state persisted) with tooltips.
- **A11y**: labelled `<nav>`; `aria-current="page"`; "Skip to content" skip-link as the app's first focusable element; tab order = visual order; in collapsed mode, tooltips are accessible on keyboard focus.

---

## 4. Patterns

### 4.1 Forms

- **Labels always visible** above the field; placeholders reserved for format examples ("e.g. app.example.com"), never sole carriers of information.
- **Errors**: below the affected field (§3.2) **and** summarized at the top of the form on submission — a list of links focusing each field in error; the summary receives focus and is announced (`role="alert"`). Inline validation on blur; server errors (optimistic lock §22.3: "this configuration was modified by someone else") appear in the same summary with a reload/diff action.
- **"Not deployed" dirty state** (§25.1): strict distinction between **saved** and **deployed**. Unsaved changes ⇒ navigation guard (confirmation). Saved but undeployed changes ⇒ per-field marking (§3.4) + **persistent bar** at the bottom of the resource page: "3 changes not deployed — Deploy now / View diff / Discard" (the diff reuses `akd-code-editor` in diff mode). This bar is the only place that triggers a redeploy from a form.
- **Safe defaults** (§25.1) and a pre-creation summary for the resource creation journeys.

### 4.2 Long actions = visible job (§22.5)

Any action > ~2 s (deploy, backup, restore, server validation, cleanup) becomes a **job**:

- The trigger goes into loading (§3.1) then the UI switches to the job representation: a "Deployment queued" toast with a link, or direct navigation to the job page.
- The job page/panel shows: `akd-status-badge` (§21.3 states), **steps** (`akd-deployment-timeline` or an equivalent step list), elapsed duration, logs (`akd-log-viewer`), **Cancel button** (if the state allows it — disabled with an explanation otherwise), retry/rollback depending on the state, and remediation on failure (classified error message + suggested action).
- A global active-jobs indicator (header) lets any running job be found again; closing the page never cancels a job (§18: the control plane executes, the UI observes).
- Dedicated **live region** (see 4.4): step transitions and terminal state announced.

### 4.3 Global keyboard navigation and command palette

- **Everything** is keyboard-operable: logical tab order, skip-link, visible focus everywhere, no hover-only interaction (actions revealed on table-row hover are also revealed on focus-within).
- Global shortcuts (proposed default, disableable, never bare letters inside fields): `Cmd/Ctrl+K` command palette, `g d` dashboard, `g s` servers, `g p` projects, `?` shortcut help. Shortcuts capture nothing when focus is in an input/editor/terminal.
- **Command palette** (`akd-command-palette`, **P2**, proposed default): fuzzy search over resources (by name/uuid), navigation, and non-destructive contextual actions; destructive actions are listed there but always route to `akd-confirm-modal`. ARIA combobox + listbox, results grouped by type.

### 4.4 Live regions (§22.5: live announcements for progress/errors)

Two unique global regions, mounted at the app root (never ad hoc live regions per component, to avoid cacophony):

- `polite`: job progress (step transitions, throttled to one announcement/step), confirmations (copy, save), selection counters.
- `assertive`: blocking errors, job failure, realtime connection loss.

An Angular service (`DbxAnnouncer`) is the single write API; components never insert their own `aria-live` (listed exceptions: form error summary `role="alert"`, dynamic danger alerts §3.11).

### 4.5 Densities

Two global densities, persisted (localStorage + account preference): **comfortable** (default) and **compact** (tables 32px→28px, spacing -1 step, `--akd-text-sm` everywhere). Implemented by a set of `--akd-density-*` tokens remapped via `data-density` on `<html>` — components do not know about density, only their tokens. Interactive targets never go below 24×24px (WCAG 2.5.8).

### 4.6 Time and timezones (§22.3)

Internal timestamps in UTC; display in the user's timezone with explicit indication on hover (full ISO 8601 UTC); logs follow the target server's timezone (§3.13). Relative durations ("3 min ago") only if the absolute timestamp is available on hover/focus. A single `akd-timestamp` component implements this rule.

---

## 5. Angular architecture

### 5.1 Organization

```
web/
  projects/
    akd-ui/                     # component library (standalone)
      src/lib/
        button/
          button.component.ts   # standalone, ChangeDetectionStrategy.OnPush, signals
          button.component.html
          button.component.css  # tokens only
          button.stories.ts     # catalogue — mandatory
          button.component.spec.ts
        status-badge/ …         # one folder per component, same structure
      src/styles/               # shared styles package
        tokens.css              # §2.6 — verbatim copy of the spec (single dark theme)
        reset.css
    dashboard/                  # the application (lazy loading by domain §25.2)
```

- **Standalone components**, selector prefix **`akd-`** (reserved for the lib — lint: the app declares no `akd-*`), strict TypeScript, signals for local state, `OnPush` everywhere, zoneless if the LTS allows it.
- The `akd-ui` lib **does not depend** on the generated API client: it is purely presentational (inputs/outputs/signals). Connected components (e.g. the page feeding `akd-log-viewer` from the realtime stream) live in the app.
- The **tokens** live in the shared styles package, imported once globally; component styles reference only token variables from §2.6 (canonical; `--akd-*` aliases tolerated during the migration) — stylelint lint: literal color/dimension forbidden, see §6.
- The state enums consumed by `akd-status-badge` are **generated from the OpenAPI** (§24.1, §25.2) — the state→family mapping lives in the lib and its completeness is tested against the generated enum.

### 5.2 Catalogue (§25.3 — blocking requirement)

- **Storybook** (official Angular builder; accepted alternative: an equivalent Analog/Sandbox, but Storybook is the proposed default for its a11y/interactions addons).
- PRD rule: **"a component enters the UI only if it is in the catalogue"** — mechanically enforced: CI fails if an exported `akd-ui` component has no `.stories.ts`, and the app can only import what the lib exports.
- Each component exposes one story per variant × state (including error, empty, loading, stale, reduced-motion states), rendered on the single dark theme and both densities.
- Required addons: `@storybook/addon-a11y` (axe-core, CI failure on violation), interactions (scripted keyboard tests), viewport (responsive §22.4).
- The built catalogue is published internally on every merge: it is the **single reference** for designers/developers.

### 5.3 i18n

- **No hard-coded string** — all lib strings go through inputs or translation keys; the app uses a key-based i18n runtime (proposed default: Transloco, for hot locale loading without a rebuild per locale — assumed divergence from `@angular/localize`, see debatable choices); English = default locale and the only locale shipped initially (§25.2), French arrives as a key file without refactoring.
- Key convention: `domain.component.usage` (e.g. `deployments.timeline.step.building`, `common.actions.cancel`). The §21 state labels are keys generated alongside the OpenAPI enum.
- CI lint: a displayable string literal in a template ⇒ error (dedicated eslint-plugin rule); `aria-label`s go through the same keys.
- Dates/numbers via `Intl` with the active locale; never concatenation of translated fragments (ICU parameters).

---

## 6. Component addition checklist

A component is merged into `akd-ui` only if **everything** is true:

1. **Tokens only**: no literal hex/oklch/px/ms in its CSS — only token variables from §2.6 (canonical; `--akd-*` aliases tolerated during the migration) (blocking stylelint in CI).
2. **AA**: text contrasts ≥ 4.5:1 and UI ≥ 3:1 verified on the single dark theme — automatically (axe on the stories) and, for any new color pair, ratio computed and recorded in §2 of this document.
3. **Full keyboard**: all usages operable without a mouse; visible focus on every interactive element; logical tab order; shortcuts documented in the story; Storybook interaction test covering the nominal keyboard journey.
4. **Normalized states**: if the component displays a business state, it composes `akd-status-badge` (never local state colors); loading/empty/error/disabled states defined, plus stale if the component displays observed data (§19.2).
5. **Exhaustive stories**: one story per variant × state, both densities, reduced-motion; a11y addon with no violation; the component is importable by the app only once its stories exist (§5.2).
6. **Test spec**: `.spec.ts` covering the contract (inputs/outputs, states, ARIA rendering — roles and attributes asserted); for richly interactive components (modal, table, log viewer), interaction tests (Angular CDK harness or testing-library).
7. **i18n**: no hard-coded string; all keys created with their English value; parameterizable `aria-label`s.
8. **Documentation**: usage description in the story (when to use it / when not to use it), and update of the §3 inventory if the component is new.

---

## Appendix A — Summary of verified contrast ratios

Method: oklch → sRGB conversion then WCAG 2.1 relative luminance, ratio = (L1+0.05)/(L2+0.05); the translucent `-dim` backgrounds (alpha 0.12) are composited on `--bg-0`. Verification script to be integrated in CI (the same pairs, as a snapshot test on `tokens.css`).

Critical pairs (text: requirement 4.5:1; UI: 3:1) — single dark theme:

| # | Pair | Ratio |
|---|---|---:|
| 1 | Primary text `--text-1` / page background `--bg-0` | 16.60 ✅ |
| 2 | Primary text `--text-1` / card `--bg-1` | 15.91 ✅ |
| 3 | Secondary text `--text-2` / page background | 8.59 ✅ |
| 4 | Link/accent `--accent` / page background | 10.40 ✅ |
| 5 | Primary button `--accent-on` / `--accent-strong` | 7.22 ✅ |
| 6 | Danger button/text `--danger` / `--danger-dim` | 6.50 ✅ |
| 7 | Warning text `--warn` / page background | 9.82 ✅ |
| 8 | Focus ring `--accent` / page background (UI 3:1) | 10.40 ✅ |
| 9 | Success badge `--ok` / `--ok-dim` | 8.63 ✅ |
| 10 | Progress badge `--accent` / `--info-dim` | 8.66 ✅ |
| 11 | Neutral badge `--neutral` / `--neutral-dim` | 6.40 ✅ |
| 12 | Log viewer `--text-2` / `--bg-inset` | 8.81 ✅ |

Documented guardrails: `--text-3` on `--bg-0` = 4.26:1 → reserved for **non-essential or redundant** meta, never body text nor the sole carrier of a piece of information (§2.1); `--text-disabled` = decorative only; `--border-1/2` borders **decorative** (< 3:1, 1.4.11 exemption) — control identification goes through the `--bg-inset` background contrast and the accent focus; non-text status dots ≥ 6.40:1 ≥ 3:1 (§2.3).
