# Design system AkerDock — `akd`

> Artefact §29.13 du PRD. Couvre : principes, design tokens, inventaire des composants, patterns d'interaction, architecture Angular et checklist d'ajout. Référence les exigences PRD §21 (machines à états), §22.5 (accessibilité), §25 (dashboard/UX), §5.7 et §13 (logs, terminal), §3.8 (métriques), §19.2 (statuts stale), §22.2 (backpressure logs), §23.3 (neutralisation ANSI).
>
> Statut : révisé le 2026-07-18 — les tokens (§2) et le vocabulaire de classes (§3) reprennent le kit Claude Design validé par le product owner et remplacent les défauts proposés initiaux. Les valeurs chiffrées sont vérifiées au contraste WCAG 2.1 AA (ratios calculés selon la formule de luminance relative WCAG, affichés dans les tables). Toute révision passe par un commit sur ce document ; le catalogue Storybook fait foi pour l'implémentation.

---

## 1. Principes

1. **Minimalisme fonctionnel** (§25.3). Aucun kit UI tiers lourd. Chaque composant existe parce qu'un parcours du PRD l'exige, pas par complétude de bibliothèque. Dépendances tierces limitées aux besoins spécialisés : xterm.js (terminal), un éditeur de code embarquable, une lib de graphiques légère.
2. **Densité d'outil d'exploitation**. Le dashboard est un outil d'ops, pas un site marketing : tables et listes compactes, information dense, chrome minimal. La densité par défaut vise un opérateur qui scanne 2 000 ressources et 100 serveurs (§22.2), pas un lecteur occasionnel.
3. **Hiérarchie par la typographie et l'espacement, pas par la décoration**. Pas de dégradés, pas d'ombres portées lourdes, pas d'illustrations décoratives. La structure visuelle vient de : taille/graisse de texte, espacement vertical, bordures fines, et couleur réservée à la sémantique (états, actions, liens).
4. **Un état = une représentation unique partout** (§25.3). Un état de la machine §21 (`running`, `failed`, `queued`…) se lit exactement de la même façon sur le dashboard, une carte ressource, une ligne de table, une timeline de déploiement ou un job : même couleur, même icône, même libellé, via le seul composant `akd-status-badge`. Interdiction de re-styler un état localement.
5. **Jamais la couleur seule**. Chaque état combine couleur + icône + libellé texte (WCAG 1.4.1). Les états `unknown/stale` et `cancelled/superseded` ont en plus une forme distincte (pointillé, barré) lisible sans perception des couleurs.
6. **Accessibilité dès la conception, pas en retrofit** (§25.3, §22.5) : parcours clavier complet, focus visible, labels de formulaire, contraste AA sur le thème sombre unique (§2.7), annonces live pour progression et erreurs.
7. **Anglais, i18n-first** (§25.2). L'UI est en anglais par défaut ; aucune chaîne en dur — tous les libellés de ce document sont des valeurs par défaut de clés de traduction.

---

## 2. Design tokens

> **Note de révision (2026-07-18).** Cette section remplace l'ancienne palette gray/teal light-first (échelles zinc/teal hex, deux thèmes commutables) par les tokens du kit Claude Design « Plateforme SPA Angular pour serveurs » validé par le product owner : thème sombre unique, couleurs en `oklch`, accent teal « dock light », trois familles typographiques embarquées. Les valeurs ci-dessous sont normatives ; `web/src/styles/tokens.css` en est la copie verbatim (jamais l'inverse).

Les tokens sont la seule source de style : **aucune couleur, taille ou durée en dur dans un composant**. Trois couches :

- **Tokens canoniques** (`--bg-0…3`, `--border-1/2`, `--text-1/2/3`, `--accent*`, familles d'état `--ok/--warn/--danger/--info/--neutral` avec variantes `-dim`/`-border`, typo/espace/forme/mouvement) : la surface consommée par le nouveau code.
- **Alias sémantiques** (`--surface-page`, `--surface-card`, `--surface-terminal`, `--text-body`, `--text-muted`, `--link`…) : noms d'usage résolvant vers les canoniques.
- **Couche de compatibilité `--akd-*`** : alias **transitoires** pour les pages antérieures au redesign ; chaque `--akd-*` résout vers un token canonique. Le nouveau code ne l'utilise pas ; la couche rétrécit avec la migration et sera supprimée avec son dernier consommateur.

### 2.1 Couleurs — surfaces, bordures et texte

Rampe bleu-noir froide (`oklch`, teinte ~252), thème sombre unique :

| Token | Valeur | Usage (alias) |
|---|---|---|
| `--bg-0` | `oklch(14.5% 0.014 252)` | fond page (`--surface-page`) |
| `--bg-1` | `oklch(17.5% 0.015 252)` | cartes (`--surface-card`) |
| `--bg-2` | `oklch(20.5% 0.016 252)` | surfaces relevées, hover de ligne/bouton (`--surface-raised`) |
| `--bg-3` | `oklch(24% 0.017 252)` | overlays, toasts (`--surface-overlay`) |
| `--bg-inset` | `oklch(12% 0.013 252)` | logs, terminal, fonds d'inputs (`--surface-terminal`) |
| `--border-1` | `oklch(28% 0.018 252)` | bordures décoratives, séparateurs |
| `--border-2` | `oklch(36% 0.02 252)` | bordures renforcées (hover d'input, overlays) |
| `--text-1` | `oklch(94% 0.006 250)` | texte principal (`--text-body`) |
| `--text-2` | `oklch(74% 0.012 250)` | texte secondaire (`--text-muted`) |
| `--text-3` | `oklch(56% 0.014 250)` | micro-labels, méta (`--text-faint`) |
| `--text-disabled` | `oklch(44% 0.012 250)` | texte désactivé |

Contrastes vérifiés (calcul WCAG) :

| Paire | Ratio | Exigence | Verdict |
|---|---:|---|---|
| texte principal `--text-1` / fond `--bg-0` | **16.60:1** | 4.5:1 | ✅ |
| texte principal `--text-1` / carte `--bg-1` | **15.91:1** | 4.5:1 | ✅ |
| texte secondaire `--text-2` / fond `--bg-0` | **8.59:1** | 4.5:1 | ✅ |
| texte secondaire `--text-2` / carte `--bg-1` | **8.23:1** | 4.5:1 | ✅ |
| méta `--text-3` / fond `--bg-0` | **4.26:1** | — | ⚠️ voir règle |
| texte logs `--text-2` / `--bg-inset` | **8.81:1** | 4.5:1 | ✅ |

Règles :

- `--text-3` est sous le seuil AA texte (4.26:1) : réservé aux méta **non essentielles ou redondantes** (micro-labels en capitales accompagnant une valeur en `--text-1/2`, timestamps de logs retrouvables dans le fichier téléchargé, séparateurs de breadcrumb). Il n'est **jamais** le seul porteur d'une information.
- Les bordures `--border-1/2` sont **décoratives** (< 3:1, exemption 1.4.11 assumée) : l'identification des champs repose sur le contraste de fond `--bg-inset` / surface, et l'état focus sur `--accent` (10.40:1 ≥ 3:1).

### 2.2 Couleurs — accent teal

Accent teal « dock light » (teinte oklch 195), réservé aux actions, liens, sélection, focus et indicateurs actifs :

| Token | Valeur | Usage |
|---|---|---|
| `--accent` | `oklch(78% 0.125 195)` | liens (`--link`), focus, texte/indicateur accent |
| `--accent-strong` | `oklch(68% 0.13 195)` | fond du bouton primaire, coche/piste cochée |
| `--accent-on` | `oklch(16% 0.03 195)` | texte/glyphe sur fond accent |
| `--accent-dim` | `oklch(78% 0.125 195 / 0.12)` | fonds subtils (nav active, sélection, `::selection`) |
| `--accent-border` | `oklch(78% 0.125 195 / 0.35)` | bordures teintées accent |
| `--link-hover` | `oklch(86% 0.11 195)` | liens au survol |

Contrastes vérifiés :

| Paire | Ratio | Exigence | Verdict |
|---|---:|---|---|
| lien/accent texte `--accent` / `--bg-0` | **10.40:1** | 4.5:1 | ✅ |
| bouton primaire : `--accent-on` sur `--accent-strong` | **7.22:1** | 4.5:1 | ✅ |
| focus ring `--accent` / `--bg-0` | **10.40:1** | 3:1 (UI) | ✅ |

Le bouton primaire est **fond teal + texte sombre** (pattern « inversé » du thème sombre : `--accent-strong` / `--accent-on`), jamais blanc sur teal. Le focus des contrôles du kit utilise le double anneau `--ring-focus` (`--bg-0` puis `--accent`, §2.5), lisible sur toute surface.

### 2.3 Couleurs sémantiques d'état — mapping sur les machines à états §21

Cinq familles sémantiques + deux modificateurs de forme. **Chaque état des machines §21 est mappé sur exactement une famille** ; ce mapping est la table de vérité du composant `akd-status-badge` (§3.10) et il est exhaustif :

| Famille | Sens | États §21.1 (déploiement) | États §21.2 (ressource/serveur) | États §21.3 (job) |
|---|---|---|---|---|
| **success** (vert) | nominal, terminal heureux | `succeeded` | `running` (désiré atteint), `healthy`, `ready` | `succeeded` |
| **progress** (bleu, animé) | transitoire, en cours | `queued`, `preparing`, `cloning`, `building`, `pushing`, `starting`, `healthchecking`, `switching`, `finishing`, `retrying` | `starting`, `pending`, `validating`, `deleting` | `scheduled`, `queued`, `leased`, `running`, `retry_wait` |
| **warning** (ambre) | dégradé, attention requise | — | `unhealthy`, `maintenance`, écart désiré/observé (drift) | — |
| **danger** (rouge) | échec, terminal malheureux | `failed` | `unreachable`, `missing` | `dead_letter` |
| **neutral** (gris) | inactif volontaire | — | `stopped`, `exited`, `deleted` | — |
| **neutral + bordure pointillée** | information périmée (§19.2 : `observed_at` trop ancien → « jamais un faux running ») | — | `unknown` / stale | — |
| **neutral + libellé barré** | remplacé/abandonné, terminal | `cancelled`, `superseded` | — | `cancelled` |

Chaque famille est portée par un triplet de tokens — couleur pleine (texte, pastille), fond translucide `-dim` (alpha 0.12), bordure translucide `-border` (alpha 0.35). Même clarté/chroma oklch pour toutes les familles (78 % / 0.125, sauf danger relevé en chroma), seule la teinte varie :

| Famille | fg (couleur pleine) | bg badge | bordure badge |
|---|---|---|---|
| success | `--ok` = `oklch(78% 0.125 155)` | `--ok-dim` | `--ok-border` |
| progress | `--accent` (teinte 195) | `--info-dim` = `oklch(78% 0.125 195 / 0.12)` | `--accent-border` |
| warning | `--warn` = `oklch(78% 0.125 85)` | `--warn-dim` | `--warn-border` |
| danger | `--danger` = `oklch(72% 0.155 25)` | `--danger-dim` | `--danger-border` |
| neutral | `--neutral` = `oklch(70% 0.02 252)` | `--neutral-dim` | `--neutral-border` |

Contrastes vérifiés (texte d'état sur fond page, et texte de badge sur son fond `-dim` composité sur `--bg-0`) :

| Paire | Ratio | Exigence | Verdict |
|---|---:|---|---|
| success texte `--ok` / `--bg-0` | **10.40:1** | 4.5:1 | ✅ |
| success badge `--ok` / `--ok-dim` | **8.63:1** | 4.5:1 | ✅ |
| progress badge `--accent` / `--info-dim` | **8.66:1** | 4.5:1 | ✅ |
| warning texte `--warn` / `--bg-0` | **9.82:1** | 4.5:1 | ✅ |
| warning badge `--warn` / `--warn-dim` | **8.24:1** | 4.5:1 | ✅ |
| danger texte `--danger` / `--bg-0` | **7.46:1** | 4.5:1 | ✅ |
| danger badge `--danger` / `--danger-dim` | **6.50:1** | 4.5:1 | ✅ |
| bouton danger : `--danger` sur `--danger-dim` | **6.50:1** | 4.5:1 | ✅ |
| neutral badge `--neutral` / `--neutral-dim` | **6.40:1** | 4.5:1 | ✅ |
| pastilles d'état (couleur pleine / `--bg-0`, indicateur non textuel) | **≥ 6.40:1** | 3:1 (UI) | ✅ |

Note : la famille **progress partage la teinte de l'accent** (195) — choix assumé du kit : « en cours » se lit comme de l'activité. La distinction statut/interactif ne repose donc **jamais sur la couleur** : un statut est toujours une pill `akd-status` avec pastille de forme propre à sa famille + libellé (§3.6, principe 5) ; un élément interactif n'a jamais cette forme. Les autres familles restent sans collision avec l'accent (§25.3).

### 2.4 Typographie

Trois familles, **embarquées via `@fontsource`** depuis `node_modules` (bundlées par Angular au build — la CSP n'autorise aucune origine externe et reste **inchangée**, jamais de CDN ; une instance air-gapped n'en a de toute façon aucune) :

- **`--font-display` : Space Grotesk** (graisses 500/600/700) — titres de pages, de cartes et de modales, valeurs de stat.
- **`--font-body` : IBM Plex Sans** (400/500/600) — corps, formulaires, tables, navigation.
- **`--font-mono` : JetBrains Mono** (400/500/700) — logs, terminal, UUID, digests, SHA, URLs, valeurs d'env, cron.

Chaque famille garde un repli système (`system-ui` / `ui-monospace`).

Échelle de tailles, 8 crans (corps par défaut 14px, jamais en dessous de 10px) :

| Token | Taille | Usage |
|---|---|---|
| `--text-2xs` | 10px | compteurs d'onglets, sections de nav, méta ultra-dense |
| `--text-xs` | 11px | micro-labels en capitales (labels de champ, en-têtes de table), badges |
| `--text-sm` | 12.5px | méta, hints, logs, valeurs mono, breadcrumb |
| `--text-md` | 14px | **corps par défaut** : tables, formulaires, nav, boutons |
| `--text-lg` | 16px | titres de cartes/sections (display) |
| `--text-xl` | 20px | titres de modales |
| `--text-2xl` | 26px | titres de pages, valeurs de stat |
| `--text-3xl` | 34px | chiffres héro du dashboard |

Interlignes par tokens : `--leading-tight: 1.2` (titres, valeurs), `--leading-normal: 1.5` (corps). Graisses : `--weight-regular: 400` (corps), `--weight-medium: 500` (labels, nav active, tabs, statuts), `--weight-semibold: 600` (titres de cartes, boutons, labels de champ), `--weight-bold: 700` (**réservé au display** : h1 de page, valeurs de stat — le corps ne dépasse pas 600, principe 3). Les micro-labels sont en capitales avec `--tracking-wide: 0.06em`. Chiffres tabulaires (`font-variant-numeric: tabular-nums`) obligatoires dans les tables, durées, métriques.

### 2.5 Espacement, rayons, élévations, animation

**Espacement** — échelle à 9 crans : `--space-1: 4px` ; `-2: 8px` ; `-3: 12px` ; `-4: 16px` ; `-5: 20px` ; `-6: 24px` ; `-7: 32px` ; `-8: 40px` ; `-9: 56px`.

**Rayons** : `--radius-1: 4px` (badges, checkbox) ; `--radius-2: 6px` (inputs, boutons, logs) ; `--radius-3: 10px` (cartes, modales, toasts) ; `--radius-full: 999px` (pills de statut, switch).

**Élévations** — la hiérarchie vient d'abord de la couleur de surface (`--bg-0…3`) ; les ombres renforcent les couches flottantes :

```
--shadow-1: 0 1px 2px oklch(0% 0 0 / 0.4);
--shadow-2: 0 4px 16px oklch(0% 0 0 / 0.45);   /* toasts, popovers, dropdowns */
--shadow-3: 0 16px 48px oklch(0% 0 0 / 0.55);  /* modales */
--ring-focus: 0 0 0 2px var(--bg-0), 0 0 0 4px var(--accent);  /* double anneau focus */
```

**Animation** — rapide, jamais bloquante :

```
--dur-1: 120ms;  /* hover, focus, transitions de boutons */
--dur-2: 200ms;  /* toasts, switch, dropdowns, tooltips */
--dur-3: 350ms;  /* modales, panneaux latéraux */
--ease-out: cubic-bezier(0.2, 0.8, 0.2, 1);
```

Sous `prefers-reduced-motion: reduce` : toutes les durées passent à `1ms`, les animations d'état « progress » (pulsation de badge, spinner de timeline, shimmer de skeleton) sont remplacées par des représentations statiques équivalentes (icône fixe, texte « In progress »). Aucune information n'est portée uniquement par le mouvement.

### 2.6 Bloc CSS de référence (copié tel quel par `web/src/styles/tokens.css`)

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

> Note d'implémentation : le second bloc `:root` est la **couche de compatibilité transitoire `--akd-*`** (§2, troisième couche). Elle permet aux pages antérieures au redesign de rendre dans le nouveau langage sans modification ; elle n'introduit aucune valeur nouvelle (tout alias résout vers un token canonique) et sera supprimée avec son dernier consommateur.

### 2.7 Gestion du thème

- **Thème sombre unique** (décision actée avec le kit validé : le kit de design a été validé dark-only par le product owner et un outil d'exploitation se consulte majoritairement dans des contextes sombres — salle machine, on-call de nuit, terminaux). Il n'y a **plus de toggle light/dark ni de `prefers-color-scheme`** : le thème n'est plus une préférence utilisateur (ni localStorage, ni préférence de compte, ni `data-theme`).
- `color-scheme: dark` est posé sur `:root` pour que scrollbars, contrôles natifs et `<select>` suivent.
- `prefers-reduced-motion` reste honoré (§2.5) : la suppression du thème clair ne retire aucune adaptation d'accessibilité.
- Le contraste est testé en CI sur ce seul thème (voir §6).

---

## 3. Inventaire des composants

Tous les composants sont des composants Angular **standalone** préfixés `akd-` (§5). Conventions transverses, applicables à tout l'inventaire :

- **Focus visible** : règle globale `outline: 2px solid var(--accent); outline-offset: 2px;` ; les contrôles du kit (boutons, inputs, tabs, switch…) utilisent le double anneau `box-shadow: var(--ring-focus)` (§2.5), lisible sur toute surface. Jamais supprimé, jamais remplacé par un simple changement de couleur. Ratio ring/fond 10.40:1 ≥ 3:1 (§2.2).
- **Cible tactile/clic** : hauteur interactive ≥ 32px en densité par défaut (outil desktop, dérogation motivée à 44px mobile pour les actions d'urgence §22.4).
- **Désactivé** : `--akd-text-disabled` + `cursor: not-allowed` + `aria-disabled` (les boutons désactivés restent focusables pour rester découvrables au lecteur d'écran, avec tooltip expliquant pourquoi).
- **i18n** : toute chaîne rendue est une clé de traduction, y compris les `aria-label`.

Le vocabulaire visuel de l'inventaire est implémenté par des **classes CSS globales `.akd-*`** (convention BEM, `web/src/styles.css`, reprises du kit — tokens uniquement), que les composants Angular composent :

| Composant | Classes du kit |
|---|---|
| Boutons (§3.1) | `.akd-btn` + modificateurs `--primary` / `--secondary` / `--ghost` / `--danger`, taille `--sm` ; bouton icône `.akd-iconbtn` (+ `--bordered`) |
| Champs (§3.2) | `.akd-field` (`__label`, `__hint`, `__hint--error`), `.akd-input` (+ `--mono`, `--error`), wrapper `.akd-select` autour d'un `<select>` natif |
| Cases / toggle (§3.3) | `.akd-check`, `.akd-switch` |
| Statuts (§3.6) | pill `.akd-status` + `--ok` / `--progress` / `--warn` / `--danger` / `--neutral`, pastille `.akd-status__dot` (forme par famille) ; badges informatifs `.akd-badge` (+ `--mono`, `--accent`, `--ok`, `--warn`, `--danger`) |
| Tables (§3.5) | `.akd-table` (+ `--clickable`), valeurs mono `.akd-mono` |
| Cartes (§3.7) | `.akd-card` (`__header`, `__title`, `__body`) |
| Tabs (§3.8) | `.akd-tabs`, `.akd-tab` (+ `--active`), compteur `.akd-tab__count` |
| Modales (§3.9–3.10) | `.akd-modal-backdrop`, `.akd-modal` (+ `--danger`), `__header` / `__body` / `__footer` |
| Toasts (§3.11) | `.akd-toast` (+ `--ok`, `--warn`, `--danger`), `__icon`, `__title`, `__msg` |
| Timeline (§3.12) | `.akd-timeline`, `.akd-tstep` (+ `--done`, `--active`, `--failed`, `--pending`), `__rail` / `__node` / `__line` / `__title` / `__dur` / `__detail` |
| Log viewer (§3.13) | `.akd-log`, `.akd-log__line` (+ `--info`, `--ok`, `--warn`, `--error`, `--cmd`), `__ts`, `__msg` |
| Stats / métriques (§3.16) | `.akd-stat` (`__label`, `__value`, `__unit`, `__delta`) |
| État vide (§3.18) | `.akd-empty` (`__icon`, `__title`, `__msg`) |
| Navigation (§3.20) | `.akd-breadcrumb` (`__sep`, `__current`), `.akd-sidenav` (`__section`, `__item`, `__item--active`) |
| Gabarit de page | `.akd-page`, `.akd-bar` (h1/h2 en `--font-display`), `.akd-error`, `.akd-secret`, `.akd-dl`, `.akd-muted`, `.sr-only` |

Une **couche de compatibilité** en fin de `styles.css` rend l'ancien dialecte de classes (`.akd-btn` nu = primaire, `.akd-btn-ghost` / `.akd-btn-danger` autonomes, `select.akd-select`, tabs marquées par `aria-selected`, cartes auto-paddées) dans le nouveau langage ; comme la couche `--akd-*` (§2.6), elle disparaît avec la migration des dernières pages.

### 3.1 `akd-button`

- **Anatomie** : conteneur, libellé, icône optionnelle (gauche ou seule), spinner intégré en état loading.
- **Variantes** (modificateurs `.akd-btn--*`) : `primary` (fond `--accent-strong`, texte `--accent-on`, 7.22:1) ; `secondary` (fond `--bg-2`, bordure `--border-1`, texte `--text-1`) ; `danger` (teinté : fond `--danger-dim`, bordure `--danger-border`, texte `--danger`, 6.50:1) — réservé aux actions destructives ; `ghost` (fond transparent, texte `--text-2`, hover `--bg-2`) pour les actions tertiaires en table. Tailles : défaut 36px, `sm` (28px, tables et toolbars) ; bouton icône `.akd-iconbtn` 32px avec `aria-label` obligatoire.
- **États** : default, hover, active, focus-visible, disabled, **loading** (spinner + libellé conservé + `aria-busy="true"`, clics ignorés sans disparition du bouton).
- **A11y** : `<button>` natif (jamais de `div`), `type` explicite ; icône seule ⇒ `aria-label` obligatoire (contrôlé par lint) ; Enter/Space natifs.

### 3.2 `akd-input`, `akd-select`, `akd-textarea` + validation inline

- **Anatomie** (via le wrapper `akd-field`) : label **toujours visible** (jamais placeholder-comme-label, §22.5), contrôle, texte d'aide, message d'erreur sous le champ, préfixe/suffixe optionnels (unité, icône, bouton reveal).
- **Variantes** : tailles `sm`/`md` ; `akd-input` supporte `type` texte/nombre/mot de passe/URL ; variante `mono` (UUID, digests, domaines — `--akd-font-mono`) ; `akd-select` = `<select>` natif stylé (pas de listbox custom en P0 : le natif est accessible gratuitement) ; `akd-textarea` redimensionnable verticalement avec compteur optionnel.
- **États** : default, hover, focus (ring), disabled, readonly, **invalid** (bordure + texte d'erreur `--akd-status-danger-fg`, icône d'erreur — jamais la bordure rouge seule) ; validation inline au `blur` puis à la frappe une fois le champ en erreur (§25.1 « validation inline »).
- **A11y** : `<label for>` explicite ; erreur liée par `aria-describedby` + `aria-invalid="true"` ; l'aide et l'erreur sont dans le même `aria-describedby` ; annonce de l'erreur par live region du formulaire (§4.1).

### 3.3 `akd-checkbox`, `akd-radio`, `akd-toggle`

- **Anatomie** : contrôle natif (`input type=checkbox/radio`) visuellement remplacé, label cliquable à droite, description optionnelle.
- **Variantes** : checkbox tri-état (`indeterminate`, pour la sélection de table §3.9) ; radio uniquement en `akd-radio-group` (fieldset+legend) ; toggle = checkbox stylée en interrupteur, réservée aux réglages **à effet immédiat** — dans un formulaire soumis, utiliser une checkbox (convention produit).
- **États** : unchecked/checked/indeterminate, hover, focus-visible (ring sur le contrôle), disabled. Coche/piste cochée en `--akd-accent` avec glyphe `--akd-on-accent`.
- **A11y** : contrôles natifs ⇒ rôles et clavier gratuits (Space ; flèches dans un radio group) ; le toggle porte `role="switch"` + `aria-checked` ; l'état n'est jamais indiqué que par la couleur (position du curseur + libellé On/Off).

### 3.4 Variantes de champ du PRD §25.1 — `akd-field` states (exigence PRD)

Le PRD impose de distinguer systématiquement : **valeur enregistrée, valeur héritée, valeur générée, secret verrouillé, changement non déployé**. Représentations normalisées, cumulables (ex. héritée + non déployée), portées par le wrapper `akd-field` :

| Variante | Représentation visuelle | Comportement |
|---|---|---|
| **Saved** (enregistrée) | apparence par défaut, aucune décoration | — |
| **Inherited** (héritée, ex. variable serveur §3.1 PRD ou shared var) | chip `Inherited` gris à droite du label + valeur affichée en `--akd-text-secondary` italique + tooltip/ligne de provenance (« From server: staging-1 ») | bouton `Override` transforme le champ en valeur propre ; `Reset to inherited` pour revenir |
| **Generated** (générée : UUID, domaine wildcard, credential affichable) | chip `Generated` + valeur en `--akd-font-mono` + action de copie intégrée (`akd-copy-field`, §3.19) | régénération possible via action explicite confirmée |
| **Locked secret** (secret verrouillé §23.2) | icône cadenas dans le champ, valeur masquée `••••••••` (longueur fixe, ne révèle pas la taille réelle), chip `Secret` | write-only par défaut : on peut remplacer, jamais relire ; bouton `Reveal` visible uniquement si le produit l'autorise et `read:sensitive` présent ; reveal audité |
| **Undeployed change** (non encore déployé) | point ambre `●` accolé au label + chip `Not deployed` (`--akd-status-warning-*`) sur le champ modifié | agrégé dans la barre de dirty state du formulaire (§4.1) : « 3 changes not deployed — Deploy / Discard » |

- **A11y** : chaque chip a un texte (jamais icône seule) ; l'état est répété dans l'`aria-describedby` du champ (« Inherited from server staging-1 », « Changed, not deployed ») pour être annoncé au lecteur d'écran ; les couleurs de chips passent AA badge (§2.3).

### 3.5 `akd-table` (table dense)

- **Anatomie** : `<table>` sémantique — caption (visuellement masquée si redondante), thead collant, lignes, cellule de sélection, pied avec pagination.
- **Variantes** : densité `compact` (32px/ligne, défaut listes d'ops) / `comfortable` (40px) ; colonnes alignables ; cellules spécialisées : `akd-status-badge`, valeurs mono tronquées avec copie, cellule d'actions (boutons ghost `sm` + menu overflow).
- **Tri** : en-têtes triables = `<button>` dans `<th aria-sort="ascending|descending|none">` ; flèche visible ; tri serveur.
- **Pagination par curseur** (§22.2 : pagination obligatoire, pas d'offset) : boutons Prev/Next + taille de page ; pas de numéro de page ; le total est optionnel/approximatif.
- **Sélection** : checkbox par ligne + checkbox d'en-tête tri-état ; barre d'actions groupées apparaissant au-dessus de la table (annoncée par live region : « 4 rows selected »).
- **États** : ligne hover, sélectionnée (`--akd-accent-subtle` + bordure gauche accent, jamais fond seul), loading (skeleton rows §3.21), vide (EmptyState §3.20), erreur de chargement (Alert + retry).
- **A11y** : navigation clavier ligne à ligne facultative mais tous les contrôles internes tabulables dans l'ordre visuel ; l'en-tête collant ne masque jamais l'élément focalisé (scroll-margin).

### 3.6 `akd-status-badge` — la pièce centrale

Composant unique de rendu d'état (§25.3 : « états visuels normalisés » ; principe 4). Consomme exclusivement la table de mapping §2.3, générée depuis les enums d'états de l'OpenAPI (§24.1) — l'exhaustivité du mapping est vérifiée par un test : **tout état §21 sans entrée de mapping fait échouer la CI**.

- **Anatomie** : pill arrondie (`--radius-full`, classe `.akd-status`), **pastille + libellé texte — jamais la couleur seule**, fond translucide `-dim`, bordure `-border`, texte en couleur pleine de la famille (§2.3).
- **Formes de pastille par famille** (`.akd-status__dot` — la forme distingue les familles sans perception des couleurs) : success **rond plein**, progress **losange** (carré pivoté) en pulsation (statique sous reduced-motion), warning **triangle**, danger **carré**, neutral **anneau creux** ; unknown/stale : neutral + **bordure pointillée** (`border: 1px dashed`), cancelled/superseded : `⊘` + **libellé barré** (`text-decoration: line-through`).
- **Variantes** : `badge` (défaut) ; `dot` (pastille + texte sans fond, pour les tables ultra-denses — la pastille respecte 3:1 UI, §2.3) ; `dot-only` **interdit** hors cas où le libellé est adjacent dans la même cellule.
- **Comportements spécifiques** :
  - **stale** (§19.2) : dès que `observed_at` dépasse le seuil, le badge bascule sur `Unknown` avec tooltip « Last observed 12 min ago » — jamais un faux `Running`.
  - **superseded** (§21.1) : le tooltip/lien pointe vers le déploiement remplaçant.
  - divergence désiré/observé (§21.2) : le badge affiche l'**observé** ; l'écart avec le désiré est rendu par un second badge warning `Drift` à côté, pas par un mélange de couleurs.
- **A11y** : `role="status"` **non** utilisé (pas d'annonce spontanée en table) ; texte du libellé lisible tel quel ; l'animation progress est purement décorative (l'info est dans le texte).

### 3.7 `akd-card` / `akd-panel`

- **Anatomie** : surface `--surface-card`, bordure `--border-1`, rayon `--radius-3` ; zones header (`.akd-card__header` : titre `.akd-card__title` en `--text-lg` `--font-display` + actions) / body (`.akd-card__body`) / footer.
- **Variantes** : `card` (bloc de contenu) ; `panel` (section de page, sans ombre) ; `card` cliquable (toute la carte = un seul lien, les actions secondaires restent des contrôles distincts) ; carte de ressource du dashboard (titre, `akd-status-badge`, méta, sparkline).
- **États** : default, hover (cartes cliquables), focus-within visible.
- **A11y** : le titre de carte est un heading de niveau correct dans l'outline de page ; carte cliquable = `<a>` étendu par pseudo-élément, pas de `div onclick`.

### 3.8 `akd-tabs`

- **Anatomie** : barre d'onglets soulignés (pas de fond — principe 3), indicateur actif `--akd-accent` 2px, panneaux.
- **Variantes** : navigation de détail de ressource (Configuration / Environment / Storage / Health / Deployments / Logs / Terminal — §25.1) — dans ce cas les onglets sont des **liens routés** (`role` de navigation, URL profonde par onglet) ; tabs locales (ARIA tabs) pour les sous-sections non routées.
- **États** : active (accent + `--akd-weight-medium`), hover, focus-visible, badge de compteur optionnel (ex. « Deployments 3 »).
- **A11y** : variante locale : `role="tablist/tab/tabpanel"`, `aria-selected`, flèches gauche/droite + Home/End, activation au focus ; variante routée : `<nav>` + `aria-current="page"`, pas de rôle tab.

### 3.9 `akd-modal`

- **Anatomie** : `<dialog>` natif (ou équivalent avec focus trap), overlay, header (titre + bouton fermer), body scrollable, footer d'actions (action principale à droite).
- **Variantes** : `sm` 400px (confirmations), `md` 560px (formulaires courts), `lg` 800px (diff de config, preview de cascade §19.2). Les formulaires longs restent des pages, pas des modales.
- **États** : ouverture/fermeture animées (`--akd-duration-slow`, fondu simple sous reduced-motion).
- **A11y** : `aria-modal="true"`, `aria-labelledby` = titre ; focus trap ; à l'ouverture le focus va au premier élément pertinent (jamais l'action destructive) ; Esc ferme (sauf job en cours, alors Esc demande confirmation) ; à la fermeture le focus revient à l'élément déclencheur.

### 3.10 `akd-confirm-modal` — confirmation renforcée (§22.5)

Pattern unique pour **toute** action destructive (§25.3 : « toute action destructive suit le même pattern ») : suppression de données, restore, rotation de CA, terminal root, opérations cloud destructives, arrêt du proxy (coupe le trafic entrant, §4.1 PRD), suppression cascade prévisualisée (§19.2).

- **Anatomie** : titre explicite (« Delete application "api-prod" »), **liste des conséquences concrètes** (« 3 volumes and their data will be permanently deleted », « The Hetzner VPS will NOT be deleted » §3.2 PRD), zone d'avertissement `--akd-status-danger-bg`, **champ de saisie du nom exact de la ressource**, bouton `danger` désactivé tant que la saisie ne correspond pas (comparaison exacte, sensible à la casse, sans trim silencieux), bouton Cancel focalisé par défaut.
- **Variantes** : `type-to-confirm` (défaut destructif) ; `checklist` (restore : cocher « I understand current data will be overwritten ») ; les deux cumulables.
- **États** : saisie non concordante (bouton désactivé + aide « Type the resource name to confirm »), concordante (bouton actif), soumission (loading, champ verrouillé, annulation impossible une fois le job lancé — le suivi passe au job §4.2).
- **A11y** : le champ de confirmation a un label explicite ; l'activation du bouton est annoncée (`aria-live="polite"` sur l'aide) ; pas de collage bloqué (le collage est autorisé : la friction voulue est la lecture, pas la dactylographie) — décision explicite, cohérente avec WCAG.

### 3.11 `akd-toast` / `akd-alert`

- **`akd-toast`** (transitoire, coin bas-droit, pile max 3) : anatomie icône famille sémantique + message + action optionnelle (« View job ») + fermer. Auto-dismiss 6s **sauf** erreurs (persistantes jusqu'à fermeture). Conteneur `aria-live="polite"` (`assertive` pour les erreurs) ; jamais le seul canal d'une erreur bloquante (l'état reste visible dans la page).
- **`akd-alert`** (persistant, dans le flux) : variantes `info/success/warning/danger` sur les 5 familles §2.3 ; anatomie icône + titre + corps + actions ; utilisée pour : serveur unreachable en tête de page serveur, avertissement disque, « update available », avertissements de données (§25.1 « Base »). Fermable seulement si l'information est retrouvable ailleurs.
- **A11y** : `role="alert"` uniquement pour les alertes danger insérées dynamiquement ; contraste AA vérifié sur fonds tintés (§2.3).

### 3.12 `akd-deployment-timeline` (§21.1)

Rendu de la machine à états de déploiement, étape par étape.

- **Anatomie** : liste verticale ordonnée d'étapes — `queued → preparing → cloning → building → pushing (si registry) → starting → healthchecking → switching → finishing` — chacune avec : icône d'état (mêmes familles que `akd-status-badge`), nom, **durée** (`tabular-nums` ; tick en direct pour l'étape en cours), horodatage au survol, lien vers la section de log correspondante (§3.13). Connecteur vertical coloré jusqu'à l'étape courante.
- **Variantes** : pleine (page déploiement, avec durées et liens logs) ; compacte (liste de déploiements : étapes en points condensés) ; terminale : `succeeded` (toutes vertes), `failed` (étape fautive en rouge, suivantes grisées `Skipped`), `cancelled`/`superseded` (étape courante barrée, lien vers le remplaçant pour superseded), `retrying` (nouvelle tentative liée, jamais réécriture de l'historique — attempt N affiché, §21.1).
- **États** : étape done (success), active (progress animé), pending (neutre), failed (danger), skipped (neutre atténué).
- **A11y** : `<ol>` sémantique ; l'étape courante porte `aria-current="step"` ; la progression est annoncée par la live region de la page job (§4.2), pas par la timeline elle-même ; durées annoncées en unités lisibles.

### 3.13 `akd-log-viewer` (§5.7, §22.2, §23.3)

- **Anatomie** : toolbar (recherche, filtres de niveau si structuré, follow/pause, wrap, timestamps on/off, téléchargement, plein écran) ; zone de log **virtualisée** (rendu fenêtré, obligatoire — cible : dizaines de milliers de lignes sans dégradation) ; fond `--surface-terminal` (`--bg-inset`), texte `--akd-log-fg` (`--text-2`, 8.81:1), timestamps `--akd-log-meta` (`--text-3`, 4.37:1 — méta redondante, le fichier téléchargé fait foi) en `--font-mono`.
- **Fonctions requises par le PRD** :
  - **Recherche** dans le buffer chargé, surlignage AA des occurrences, navigation n/N.
  - **Sections repliables** : les logs de build sont groupés par étape de la timeline (§3.12) ; chevron par section, état plié persistant ; erreur ⇒ section dépliée automatiquement.
  - **Timestamps** alignés sur le **fuseau du serveur cible** (§5.7) avec indication explicite du fuseau (§22.3 : « affichage dans le fuseau utilisateur/serveur avec indication explicite ») ; toggle UTC/serveur.
  - **Téléchargement** du log complet (fichier brut).
  - **ANSI neutralisé** (§23.3) : séquences d'échappement retirées ou mappées vers un jeu restreint de couleurs **retraduites en tokens AA** ; tout HTML échappé ; jamais d'injection de balisage depuis le contenu.
  - **Backpressure** (§22.2) : buffer borné, reprise par curseur ; si des lignes sont abandonnées, insertion d'un **marqueur inline non supprimable** « ⚠ N lines dropped (buffer overflow) — Download full log » stylé warning. Le silence est interdit.
  - **Follow mode** : collé en bas ; tout scroll manuel met en pause avec bouton flottant « Resume following (+128 new lines) ».
- **États** : streaming (indicateur live), en pause, déconnecté (bandeau warning + reconnexion auto), terminé, vide.
- **A11y** : zone de log `role="log"` (`aria-live="off"` par défaut — le flux serait inexploitable vocalement ; le résumé passe par la live region du job) ; toolbar entièrement clavier ; PageUp/PageDown/Home/End dans la zone ; le focus n'est jamais volé par l'auto-scroll.

### 3.14 `akd-code-editor`

Wrapper d'un éditeur embarquable existant (défaut proposé : **CodeMirror 6** — modulaire, léger, accessibilité clavier correcte, pas de dépendance lourde type Monaco) pour : fichiers env, compose (§25.1 « Éditeur validé »), config proxy, Fluent Bit custom (§13).

- **Anatomie** : toolbar (langage, validation, diff avant/après §25.1), gouttière de numéros de ligne, zone d'édition, pied (position curseur, erreurs de validation).
- **Variantes** : `env` (coloration clé=valeur, masquage optionnel des valeurs secrètes), `yaml/compose` (validation schéma inline, erreurs soulignées + listées sous l'éditeur), `diff` (lecture seule, deux volets ou inline, pour le config diff d'un déploiement §25.1) ; limite d'édition inline 5 MiB (§23.3) — au-delà, lecture seule + téléchargement.
- **États** : éditable, lecture seule, invalide (liste d'erreurs cliquables), dirty (branché sur le dirty state du formulaire §4.1).
- **A11y** : mode « Escape sort de l'éditeur » documenté et annoncé (piège à tabulation contrôlé, WCAG 2.1.2) ; thème de coloration syntaxique dérivé des tokens, vérifié AA ; toutes les erreurs disponibles en liste texte hors de l'éditeur.

### 3.15 `akd-terminal` (§5.7, §13)

Conteneur **xterm.js** (dépendance spécialisée assumée, §25.3) : shell dans tout container ou serveur géré via WebSocket → SSH.

- **Anatomie** : barre de session (cible : serveur/container + badge de contexte, indicateur de connexion, bouton reconnect, kill), zone xterm (fond `--akd-log-bg`), scrollback.
- **Variantes** : container, serveur, **root** (accès précédé de `akd-confirm-modal` §3.10, session auditée §23.4 — bandeau persistant « Root session — audited »).
- **États** : connecting, connected, reconnecting (reconnexion §5.7 : bandeau + tentatives), disconnected (overlay avec cause + bouton), terminated.
- **A11y** : `screenReaderMode` d'xterm.js activable dans la barre de session ; limitation des séquences terminal côté affichage (§23.3) ; focus : clic/Enter entre dans le terminal, **Ctrl+Shift+Escape en sort** (raccourci documenté dans la barre — Escape seul appartient au shell) ; tailles de police réglables.

### 3.16 `akd-metric-chart` (§3.8, §13)

Graphiques CPU/RAM/disque serveur et par container (données Sentinel, push ~10 s, disque ~60 s).

- **Anatomie** : titre + valeur courante (grande, `tabular-nums`) + graphique (aire ou ligne) + axes discrets + tooltip au survol/focus + sélecteur de fenêtre (1h/6h/24h/7j selon rétention configurée).
- **Variantes** : `sparkline` (mini-courbe sans axes dans cartes et lignes de table — toujours accompagnée de la valeur numérique courante) ; `full` (page serveur/ressource) ; seuils d'alerte disque rendus en ligne pointillée warning.
- **Couleurs** : séries en accent teal et gris — les couleurs sémantiques d'état restent réservées aux dépassements de seuil ; hors seuil, un graphique n'est jamais rouge/vert.
- **États** : loading (skeleton), données absentes (« No metrics — Sentinel not enabled on this server » + lien d'activation, et mention explicite de la limitation compose §3.8), **stale** (dernier point trop ancien : zone hachurée + badge Unknown, cohérent §19.2), erreur.
- **A11y** : chaque graphique a un résumé texte (`aria-label` : « CPU usage, current 42%, average 38% over last hour ») ; les points sont explorables au clavier (flèches) avec tooltip ; jamais d'information portée par la seule couleur des séries (légende + formes de points si multi-séries).

### 3.17 `akd-copy-field` (§22.5)

Pour toute valeur générée : UUID, domaine, URLs interne/externe, credentials affichables (§25.1 « Base »).

- **Anatomie** : valeur en `--akd-font-mono` (tronquée au milieu pour les longues valeurs, title complet), **contexte clair** (label : « Internal URL », « Webhook secret ») exigé par §22.5, bouton copier.
- **Variantes** : inline (dans une phrase/cellule) ; bloc (avec label) ; `secret` (masqué par défaut, reveal soumis aux mêmes règles que §3.4 ; la **copie fonctionne sans reveal** ; copie/reveal de secret auditées §23.4).
- **États** : default, copié (icône check + « Copied » 2 s, annoncé en live region polite), reveal on/off.
- **A11y** : bouton avec `aria-label` contextualisé (« Copy internal URL ») ; la confirmation de copie est annoncée, pas seulement visuelle ; la valeur est sélectionnable au clavier.

### 3.18 `akd-empty-state`

- **Anatomie** : icône discrète (pas d'illustration décorative — principe 3), titre court, une phrase d'explication, action principale et lien doc optionnels.
- **Variantes** : première utilisation (« No servers yet — Add your first server », relie l'onboarding §25.1) ; résultat de filtre vide (« No deployments match your filters » + Clear filters) ; état d'erreur de chargement (famille danger + Retry) ; capacité non activée (ex. métriques sans Sentinel §3.16).
- **A11y** : titre = heading ; l'action principale est un vrai bouton/lien focusable ; pas de `role="alert"` (état stable, pas événement).

### 3.19 `akd-skeleton`

- **Anatomie** : blocs `--akd-surface-hover` aux dimensions du contenu attendu (lignes de table, carte, graphique), shimmer subtil (`--akd-duration-slow`) ; **statique** sous reduced-motion.
- **Variantes** : `text`, `row` (table), `card`, `chart`.
- **Règles** : uniquement pour le chargement initial (< quelques secondes attendues) ; les actions longues utilisent le pattern job (§4.2), pas un skeleton ; jamais de skeleton sur du contenu déjà affiché (pas de flash lors des refetch — l'ancien contenu reste avec indicateur discret).
- **A11y** : conteneur `aria-busy="true"` sur la région en chargement ; les skeletons eux-mêmes sont `aria-hidden="true"`.

### 3.20 `akd-breadcrumb` + `akd-side-nav` (hiérarchie Team → Project → Environment → Resource)

- **`akd-breadcrumb`** : anatomie `<nav aria-label="Breadcrumb">` + `<ol>` — Team / Project / Environment / Resource, chaque segment cliquable, dernier segment `aria-current="page"` non cliquable ; segments intermédiaires = **switchers** (dropdown au clic pour changer de project/environment sans remonter) ; troncature au milieu sur écrans étroits (premier + dernier toujours visibles).
- **`akd-side-nav`** : navigation latérale par domaine fonctionnel (Dashboard, Servers, Projects, Security, Settings — alignée sur le lazy loading §25.2) ; anatomie : sélecteur de team en tête (frontière de sécurité §23.1 — le changement de team est global et explicite), items avec icône + libellé, compteurs d'alerte (serveurs unreachable, déploiements failed — dashboard §25.1) via badge, section pliable, footer (user).
- **États** : item actif (`--akd-accent-subtle` + barre gauche accent + `--akd-weight-medium` — jamais la couleur seule), hover, focus-visible ; nav repliable en icônes (état persisté) avec tooltips.
- **A11y** : `<nav>` labellisée ; `aria-current="page"` ; skip-link « Skip to content » en premier élément focusable de l'app ; ordre de tabulation = ordre visuel ; en mode replié, les tooltips sont accessibles au focus clavier.

---

## 4. Patterns

### 4.1 Formulaires

- **Labels toujours visibles** au-dessus du champ ; placeholders réservés aux exemples de format (« e.g. app.example.com »), jamais porteurs d'information unique.
- **Erreurs** : sous le champ concerné (§3.2) **et** résumé en tête de formulaire à la soumission — liste de liens focalisant chaque champ en erreur ; le résumé reçoit le focus et est annoncé (`role="alert"`). Validation inline au blur ; les erreurs serveur (verrou optimiste §22.3 : « cette configuration a été modifiée par quelqu'un d'autre ») s'affichent dans le même résumé avec action de rechargement/diff.
- **Dirty state « non déployé »** (§25.1) : distinction stricte entre **enregistré** et **déployé**. Modifs non enregistrées ⇒ garde de navigation (confirmation). Modifs enregistrées mais non déployées ⇒ marquage par champ (§3.4) + **barre persistante** en bas de la page ressource : « 3 changes not deployed — Deploy now / View diff / Discard » (le diff réutilise `akd-code-editor` en mode diff). Cette barre est le seul endroit qui déclenche le redeploy depuis un formulaire.
- **Valeurs par défaut sûres** (§25.1) et résumé avant création pour les parcours de création de ressource.

### 4.2 Actions longues = job visible (§22.5)

Toute action > ~2 s (deploy, backup, restore, validation serveur, cleanup) devient un **job** :

- Le déclencheur passe en loading (§3.1) puis l'UI bascule vers la représentation du job : toast « Deployment queued » avec lien, ou navigation directe vers la page du job.
- La page/panneau de job affiche : `akd-status-badge` (états §21.3), **étapes** (`akd-deployment-timeline` ou liste d'étapes équivalente), durée écoulée, logs (`akd-log-viewer`), **bouton Cancel** (si l'état le permet — désactivé avec explication sinon), retry/rollback selon l'état, et remédiation en cas d'échec (message d'erreur classifié + action suggérée).
- Un indicateur global de jobs actifs (header) permet de retrouver tout job en cours ; la fermeture de la page ne cancel jamais un job (§18 : le control plane exécute, l'UI observe).
- **Live region** dédiée (voir 4.4) : transitions d'étapes et état terminal annoncés.

### 4.3 Navigation clavier globale et palette de commandes

- **Tout** est opérable au clavier : ordre de tabulation logique, skip-link, focus visible partout, aucune interaction hover-only (les actions révélées au survol des lignes de table sont aussi révélées au focus-within).
- Raccourcis globaux (défaut proposé, désactivables, jamais des lettres seules dans les champs) : `Cmd/Ctrl+K` palette de commandes, `g d` dashboard, `g s` serveurs, `g p` projets, `?` aide raccourcis. Les raccourcis ne capturent rien quand le focus est dans un input/éditeur/terminal.
- **Palette de commandes** (`akd-command-palette`, **P2**, défaut proposé) : recherche fuzzy sur ressources (par nom/uuid), navigation, et actions contextuelles non destructives ; les actions destructives y sont listées mais renvoient toujours vers `akd-confirm-modal`. ARIA combobox + listbox, résultats groupés par type.

### 4.4 Live regions (§22.5 : annonces live pour progression/erreurs)

Deux régions globales uniques, montées à la racine de l'app (jamais de live regions ad hoc par composant, pour éviter la cacophonie) :

- `polite` : progression de jobs (transitions d'étapes, throttlées à une annonce/étape), confirmations (copie, sauvegarde), compteurs de sélection.
- `assertive` : erreurs bloquantes, échec de job, perte de connexion realtime.

Un service Angular (`DbxAnnouncer`) est l'unique API d'écriture ; les composants n'insèrent jamais leur propre `aria-live` (exceptions listées : résumé d'erreurs de formulaire `role="alert"`, alertes danger dynamiques §3.11).

### 4.5 Densités

Deux densités globales, persistées (localStorage + préférence de compte) : **comfortable** (défaut) et **compact** (tables 32px→28px, espacements -1 cran, `--akd-text-sm` partout). Implémentées par un jeu de tokens `--akd-density-*` re-mappés via `data-density` sur `<html>` — les composants ne connaissent pas la densité, seulement leurs tokens. Les cibles interactives ne descendent jamais sous 24×24px (WCAG 2.5.8).

### 4.6 Temps et fuseaux (§22.3)

Timestamps internes UTC ; affichage dans le fuseau de l'utilisateur avec indication explicite au survol (ISO 8601 UTC complet) ; les logs suivent le fuseau du serveur cible (§3.13). Durées relatives (« 3 min ago ») uniquement si le timestamp absolu est disponible au survol/focus. Un composant `akd-timestamp` unique implémente cette règle.

---

## 5. Architecture Angular

### 5.1 Organisation

```
web/
  projects/
    akd-ui/                     # bibliothèque de composants (standalone)
      src/lib/
        button/
          button.component.ts   # standalone, ChangeDetectionStrategy.OnPush, signals
          button.component.html
          button.component.css  # tokens uniquement
          button.stories.ts     # catalogue — obligatoire
          button.component.spec.ts
        status-badge/ …         # un dossier par composant, même structure
      src/styles/               # package styles partagé
        tokens.css              # §2.6 — copie verbatim de la spec (thème sombre unique)
        reset.css
    dashboard/                  # l'application (lazy loading par domaine §25.2)
```

- **Composants standalone**, préfixe de sélecteur **`akd-`** (réservé à la lib — lint : l'app ne déclare pas de `akd-*`), TypeScript strict, signals pour l'état local, `OnPush` partout, zoneless si la LTS le permet.
- La lib `akd-ui` **ne dépend pas** du client API généré : elle est purement présentationnelle (inputs/outputs/signals). Les composants connectés (ex. la page qui alimente `akd-log-viewer` depuis le flux realtime) vivent dans l'app.
- Les **tokens** vivent dans le package styles partagé, importés une fois globalement ; les styles de composants ne référencent que des variables de tokens du §2.6 (canoniques ; alias `--akd-*` tolérés pendant la migration) — lint stylelint : couleur/dimension littérale interdite, voir §6.
- Les enums d'états consommés par `akd-status-badge` sont **générés depuis l'OpenAPI** (§24.1, §25.2) — le mapping état→famille vit dans la lib et sa complétude est testée contre l'enum généré.

### 5.2 Catalogue (§25.3 — exigence bloquante)

- **Storybook** (builder Angular officiel ; alternative acceptée : Analog/Sandbox équivalent, mais Storybook est le défaut proposé pour ses addons a11y/interactions).
- Règle PRD : **« un composant n'entre dans l'UI que s'il est au catalogue »** — appliquée mécaniquement : la CI échoue si un composant exporté de `akd-ui` n'a pas de `.stories.ts`, et l'app ne peut importer que ce que la lib exporte.
- Chaque composant expose une story par variante × état (incluant états d'erreur, vide, loading, stale, reduced-motion), rendue sur le thème sombre unique et les deux densités.
- Addons requis : `@storybook/addon-a11y` (axe-core, échec CI sur violation), interactions (tests de clavier scriptés), viewport (responsive §22.4).
- Le catalogue buildé est publié en interne à chaque merge : c'est la **référence unique** designers/développeurs.

### 5.3 i18n

- **Aucune chaîne en dur** — toutes les chaînes de la lib passent par des inputs ou des clés de traduction ; l'app utilise un runtime i18n à clés (défaut proposé : Transloco, pour le chargement de locale à chaud sans rebuild par locale — divergence assumée avec `@angular/localize`, voir choix discutables) ; anglais = locale par défaut et unique locale livrée initialement (§25.2), le français arrive comme fichier de clés sans refactoring.
- Convention de clés : `domaine.composant.usage` (ex. `deployments.timeline.step.building`, `common.actions.cancel`). Les libellés d'états §21 sont des clés générées en face de l'enum OpenAPI.
- Lint CI : littéral de chaîne affichable dans un template ⇒ erreur (règle eslint-plugin dédiée) ; les `aria-label` passent par les mêmes clés.
- Dates/nombres via `Intl` avec la locale active ; jamais de concaténation de fragments traduits (paramètres ICU).

---

## 6. Checklist d'ajout d'un composant

Un composant est mergé dans `akd-ui` seulement si **tout** est vrai :

1. **Tokens only** : aucun hex/oklch/px/ms littéral dans son CSS — uniquement des variables de tokens du §2.6 (canoniques ; alias `--akd-*` tolérés pendant la migration) (stylelint bloquant en CI).
2. **AA** : contrastes texte ≥ 4.5:1 et UI ≥ 3:1 vérifiés sur le thème sombre unique — automatiquement (axe sur les stories) et, pour toute nouvelle paire de couleurs, ratio calculé et consigné dans §2 de ce document.
3. **Clavier complet** : tous les usages opérables sans souris ; focus visible sur chaque élément interactif ; ordre de tabulation logique ; raccourcis documentés dans la story ; test d'interaction Storybook couvrant le parcours clavier nominal.
4. **États normalisés** : si le composant affiche un état métier, il compose `akd-status-badge` (jamais de couleurs d'état locales) ; états loading/empty/error/disabled définis, plus stale si le composant affiche des données observées (§19.2).
5. **Stories exhaustives** : une story par variante × état, deux densités, reduced-motion ; addon a11y sans violation ; le composant n'est importable par l'app qu'une fois ses stories présentes (§5.2).
6. **Spec de test** : `.spec.ts` couvrant le contrat (inputs/outputs, états, rendu ARIA — rôles et attributs asserted) ; pour les composants à interaction riche (modal, table, log viewer), tests d'interaction (harness Angular CDK ou testing-library).
7. **i18n** : aucune chaîne en dur ; toutes les clés créées avec leur valeur anglaise ; `aria-label` paramétrables.
8. **Documentation** : description d'usage dans la story (quand l'utiliser / quand ne pas l'utiliser), et mise à jour de l'inventaire §3 si le composant est nouveau.

---

## Annexe A — Récapitulatif des ratios de contraste vérifiés

Méthode : conversion oklch → sRGB puis luminance relative WCAG 2.1, ratio = (L1+0.05)/(L2+0.05) ; les fonds translucides `-dim` (alpha 0.12) sont composités sur `--bg-0`. Script de vérification à intégrer en CI (les mêmes paires, en snapshot test sur `tokens.css`).

Paires critiques (texte : exigence 4.5:1 ; UI : 3:1) — thème sombre unique :

| # | Paire | Ratio |
|---|---|---:|
| 1 | Texte principal `--text-1` / fond page `--bg-0` | 16.60 ✅ |
| 2 | Texte principal `--text-1` / carte `--bg-1` | 15.91 ✅ |
| 3 | Texte secondaire `--text-2` / fond page | 8.59 ✅ |
| 4 | Lien/accent `--accent` / fond page | 10.40 ✅ |
| 5 | Bouton primaire `--accent-on` / `--accent-strong` | 7.22 ✅ |
| 6 | Bouton/texte danger `--danger` / `--danger-dim` | 6.50 ✅ |
| 7 | Texte warning `--warn` / fond page | 9.82 ✅ |
| 8 | Focus ring `--accent` / fond page (UI 3:1) | 10.40 ✅ |
| 9 | Badge success `--ok` / `--ok-dim` | 8.63 ✅ |
| 10 | Badge progress `--accent` / `--info-dim` | 8.66 ✅ |
| 11 | Badge neutral `--neutral` / `--neutral-dim` | 6.40 ✅ |
| 12 | Log viewer `--text-2` / `--bg-inset` | 8.81 ✅ |

Garde-fous documentés : `--text-3` sur `--bg-0` = 4.26:1 → réservé aux méta **non essentielles ou redondantes**, jamais un corps de texte ni le seul porteur d'une information (§2.1) ; `--text-disabled` = décoratif uniquement ; bordures `--border-1/2` **décoratives** (< 3:1, exemption 1.4.11) — l'identification des contrôles passe par le contraste de fond `--bg-inset` et le focus accent ; pastilles d'état non textuelles ≥ 6.40:1 ≥ 3:1 (§2.3).
