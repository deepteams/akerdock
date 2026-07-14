# Design system AkerDock — `akd`

> Artefact §29.13 du PRD. Couvre : principes, design tokens, inventaire des composants, patterns d'interaction, architecture Angular et checklist d'ajout. Référence les exigences PRD §21 (machines à états), §22.5 (accessibilité), §25 (dashboard/UX), §5.7 et §13 (logs, terminal), §3.8 (métriques), §19.2 (statuts stale), §22.2 (backpressure logs), §23.3 (neutralisation ANSI).
>
> Statut : spécification initiale. Toutes les valeurs chiffrées (hex, échelles, durées) sont des **défauts proposés**, vérifiées au contraste WCAG 2.1 AA (ratios calculés selon la formule de luminance relative WCAG, affichés dans les tables). Toute révision passe par un commit sur ce document ; le catalogue Storybook fait foi pour l'implémentation.

---

## 1. Principes

1. **Minimalisme fonctionnel** (§25.3). Aucun kit UI tiers lourd. Chaque composant existe parce qu'un parcours du PRD l'exige, pas par complétude de bibliothèque. Dépendances tierces limitées aux besoins spécialisés : xterm.js (terminal), un éditeur de code embarquable, une lib de graphiques légère.
2. **Densité d'outil d'exploitation**. Le dashboard est un outil d'ops, pas un site marketing : tables et listes compactes, information dense, chrome minimal. La densité par défaut vise un opérateur qui scanne 2 000 ressources et 100 serveurs (§22.2), pas un lecteur occasionnel.
3. **Hiérarchie par la typographie et l'espacement, pas par la décoration**. Pas de dégradés, pas d'ombres portées lourdes, pas d'illustrations décoratives. La structure visuelle vient de : taille/graisse de texte, espacement vertical, bordures fines, et couleur réservée à la sémantique (états, actions, liens).
4. **Un état = une représentation unique partout** (§25.3). Un état de la machine §21 (`running`, `failed`, `queued`…) se lit exactement de la même façon sur le dashboard, une carte ressource, une ligne de table, une timeline de déploiement ou un job : même couleur, même icône, même libellé, via le seul composant `akd-status-badge`. Interdiction de re-styler un état localement.
5. **Jamais la couleur seule**. Chaque état combine couleur + icône + libellé texte (WCAG 1.4.1). Les états `unknown/stale` et `cancelled/superseded` ont en plus une forme distincte (pointillé, barré) lisible sans perception des couleurs.
6. **Accessibilité dès la conception, pas en retrofit** (§25.3, §22.5) : parcours clavier complet, focus visible, labels de formulaire, contraste AA dans les deux thèmes, annonces live pour progression et erreurs.
7. **Anglais, i18n-first** (§25.2). L'UI est en anglais par défaut ; aucune chaîne en dur — tous les libellés de ce document sont des valeurs par défaut de clés de traduction.

---

## 2. Design tokens

Les tokens sont la seule source de style : **aucune couleur, taille ou durée en dur dans un composant**. Deux couches :

- **Tokens primitifs** (`--akd-gray-500`, `--akd-teal-600`…) : la palette brute, identique dans les deux thèmes. Jamais consommés directement par les composants.
- **Tokens sémantiques** (`--akd-bg`, `--akd-text`, `--akd-status-running-fg`…) : réassignés par thème. Seule surface consommée par les composants.

### 2.1 Couleurs — palette neutre (défaut proposé)

Échelle de gris légèrement froide (base zinc), 12 crans. Sert surfaces, bordures et texte dans les deux thèmes.

| Token | Hex | Usage clair | Usage sombre |
|---|---|---|---|
| `--akd-gray-0` | `#ffffff` | fond page | texte sur accent/danger foncé |
| `--akd-gray-50` | `#fafafa` | surface (cartes) | — |
| `--akd-gray-100` | `#f4f4f5` | surface survolée, code inline | texte principal |
| `--akd-gray-200` | `#e4e4e7` | séparateurs, bordures décoratives | texte logs |
| `--akd-gray-300` | `#d4d4d8` | bordures de cartes | — |
| `--akd-gray-400` | `#a1a1aa` | texte désactivé | texte secondaire |
| `--akd-gray-500` | `#71717a` | texte tertiaire, bordures d'inputs | texte tertiaire, bordures d'inputs |
| `--akd-gray-600` | `#52525b` | texte secondaire | texte désactivé |
| `--akd-gray-700` | `#3f3f46` | — | bordures fortes |
| `--akd-gray-800` | `#27272a` | — | séparateurs, surface survolée |
| `--akd-gray-900` | `#18181b` | texte principal | surface (cartes) |
| `--akd-gray-950` | `#101012` | fond logs/terminal (les deux thèmes) | fond page |

Contrastes vérifiés (calcul WCAG) :

| Paire | Thème | Ratio | Exigence | Verdict |
|---|---|---:|---|---|
| texte principal `gray-900` / fond `gray-0` | clair | **17.72:1** | 4.5:1 | ✅ |
| texte secondaire `gray-600` / fond `gray-0` | clair | **7.73:1** | 4.5:1 | ✅ |
| texte tertiaire `gray-500` / fond `gray-0` | clair | **4.83:1** | 4.5:1 | ✅ |
| texte principal `gray-100` / fond `gray-950` | sombre | **17.29:1** | 4.5:1 | ✅ |
| texte principal `gray-100` / surface `gray-900` | sombre | **16.12:1** | 4.5:1 | ✅ |
| texte secondaire `gray-400` / fond `gray-950` | sombre | **7.42:1** | 4.5:1 | ✅ |
| bordure d'input `gray-500` / fond `gray-0` | clair | **4.83:1** | 3:1 (UI) | ✅ |
| bordure d'input `gray-500` / fond `gray-950` | sombre | **3.93:1** | 3:1 (UI) | ✅ |

Règle : les **bordures porteuses de sens** (inputs, contrôles) utilisent `gray-500` dans les deux thèmes (≥ 3:1). Les bordures **décoratives** (séparateurs de cartes, dividers) utilisent `gray-200`/`gray-800` — exemptées de 1.4.11 car non nécessaires à l'identification du composant.

### 2.2 Couleurs — accent teal (défaut proposé)

Couleur de marque teal/cyan (§25.3 — sans collision avec les couleurs sémantiques d'état : succès, alerte, danger). Base `#14b8a6` (≈ `oklch(0.75 0.13 180)`), 10 crans :

| Token | Hex | Token | Hex |
|---|---|---|---|
| `--akd-teal-50` | `#f0fdfa` | `--akd-teal-500` | `#14b8a6` |
| `--akd-teal-100` | `#ccfbf1` | `--akd-teal-600` | `#0d9488` |
| `--akd-teal-200` | `#99f6e4` | `--akd-teal-700` | `#0f766e` |
| `--akd-teal-300` | `#5eead4` | `--akd-teal-800` | `#115e59` |
| `--akd-teal-400` | `#2dd4bf` | `--akd-teal-900` | `#134e4a` |

Contrastes vérifiés — le teal moyen est un piège classique, d'où des crans différents par thème :

| Paire | Thème | Ratio | Exigence | Verdict |
|---|---|---:|---|---|
| lien/accent texte `teal-700` / `gray-0` | clair | **5.47:1** | 4.5:1 | ✅ |
| bouton primaire : texte `gray-0` sur fond `teal-700` | clair | **5.47:1** | 4.5:1 | ✅ |
| ⚠️ contre-exemple : `gray-0` sur `teal-600` | clair | **3.74:1** | 4.5:1 | ❌ interdit pour du texte |
| lien/accent texte `teal-400` / `gray-950` | sombre | **10.21:1** | 4.5:1 | ✅ |
| bouton primaire : texte `gray-950` sur fond `teal-400` | sombre | **10.21:1** | 4.5:1 | ✅ |
| focus ring `teal-600` / `gray-0` | clair | **3.74:1** | 3:1 (UI) | ✅ |
| focus ring `teal-400` / `gray-950` | sombre | **10.21:1** | 3:1 (UI) | ✅ |

En sombre, le bouton primaire est donc **fond teal clair + texte sombre** (pattern « inversé »), pas blanc sur teal.

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

Valeurs proposées (crans Tailwind pour l'interopérabilité outillage), avec fond tinté pour les badges :

| Rôle | Clair : fg / bg badge | Sombre : fg / bg badge |
|---|---|---|
| success | `#15803d` / `#f0fdf4` | `#4ade80` / `#052e16` |
| progress | `#2563eb` / `#eff6ff` | `#60a5fa` / `#172554` |
| warning | `#b45309` / `#fffbeb` | `#fbbf24` / `#451a03` |
| danger | `#dc2626` / `#fef2f2` | `#f87171` / `#450a0a` |
| neutral | `#52525b` / `#f4f4f5` | `#a1a1aa` / `#27272a` |

Contrastes vérifiés (texte de badge sur son fond tinté, et texte d'état sur fond page) :

| Paire | Thème | Ratio | Exigence | Verdict |
|---|---|---:|---|---|
| success texte `#15803d` / `gray-0` | clair | **5.02:1** | 4.5:1 | ✅ |
| success badge `#15803d` / `#f0fdf4` | clair | **4.79:1** | 4.5:1 | ✅ |
| success texte `#4ade80` / `gray-950` | sombre | **10.91:1** | 4.5:1 | ✅ |
| success badge `#4ade80` / `#052e16` | sombre | **8.55:1** | 4.5:1 | ✅ |
| progress texte `#2563eb` / `gray-0` | clair | **5.17:1** | 4.5:1 | ✅ |
| progress badge `#93c5fd` / `#172554` (var. sombre) | sombre | **8.15:1** | 4.5:1 | ✅ |
| warning texte `#b45309` / `gray-0` | clair | **5.02:1** | 4.5:1 | ✅ |
| warning texte `#fbbf24` / `gray-950` | sombre | **11.39:1** | 4.5:1 | ✅ |
| danger texte `#dc2626` / `gray-0` | clair | **4.83:1** | 4.5:1 | ✅ |
| danger texte `#f87171` / `gray-950` | sombre | **6.87:1** | 4.5:1 | ✅ |
| bouton danger : `gray-0` sur `#dc2626` | clair | **4.83:1** | 4.5:1 | ✅ |
| bouton danger : `gray-950` sur `#f87171` | sombre | **6.87:1** | 4.5:1 | ✅ |
| pastille success `#16a34a` / `gray-0` (indicateur non textuel) | clair | **3.30:1** | 3:1 (UI) | ✅ |
| pastille warning `#d97706` / `gray-0` (indicateur non textuel) | clair | **3.19:1** | 3:1 (UI) | ✅ |

Note : l'accent teal n'est **jamais** utilisé comme couleur d'état — il est réservé aux actions, liens, sélection et focus, ce qui garantit qu'un élément teal se lit toujours « interactif », jamais « statut » (§25.3).

### 2.4 Typographie (défaut proposé)

- **Police UI : system stack** (défaut proposé). Zéro octet embarqué dans le binaire Go, rendu natif par OS, robustesse offline. Inter (variable, self-hosted — jamais de CDN) est l'alternative documentée si la cohérence de rendu inter-OS devient une exigence de marque ; le changement est un swap de token.
- **Police mono** pour logs, terminal, UUID, digests, SHA, URLs, valeurs d'env, cron : stack mono système.

```
--akd-font-ui:   -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Ubuntu, "Helvetica Neue", Arial, sans-serif;
--akd-font-mono: ui-monospace, "SF Mono", "Cascadia Mono", "JetBrains Mono", Menlo, Consolas, monospace;
```

Échelle de tailles, 7 crans (densité outil d'ops : corps à 13px, jamais en dessous de 11px) :

| Token | Taille / interligne | Usage |
|---|---|---|
| `--akd-text-2xs` | 11px / 16px | timestamps de logs, axes de graphiques, méta ultra-dense |
| `--akd-text-xs` | 12px / 16px | badges, labels de table, breadcrumb, aides de champ |
| `--akd-text-sm` | 13px / 20px | **corps par défaut** : tables, formulaires, nav |
| `--akd-text-md` | 14px / 20px | corps confortable, descriptions |
| `--akd-text-lg` | 16px / 24px | titres de cartes/sections |
| `--akd-text-xl` | 20px / 28px | titres de pages |
| `--akd-text-2xl` | 24px / 32px | dashboard, chiffres clés |

Graisses : `--akd-weight-regular: 400` (corps), `--akd-weight-medium: 500` (labels, nav active), `--akd-weight-semibold: 600` (titres, boutons, badges). Pas de 700+ : la hiérarchie vient de la taille et de l'espacement (principe 3). Chiffres tabulaires (`font-variant-numeric: tabular-nums`) obligatoires dans les tables, durées, métriques.

### 2.5 Espacement, rayons, élévations, animation

**Espacement** — échelle base 4px : `--akd-space-0: 0` ; `-1: 4px` ; `-2: 8px` ; `-3: 12px` ; `-4: 16px` ; `-5: 20px` ; `-6: 24px` ; `-8: 32px` ; `-10: 40px` ; `-12: 48px` ; `-16: 64px`. Demi-cran `--akd-space-05: 2px` réservé aux intérieurs de badges.

**Rayons** : `--akd-radius-xs: 2px` (badges, checkbox) ; `--akd-radius-sm: 4px` (inputs, boutons) ; `--akd-radius-md: 6px` (cartes, panels) ; `--akd-radius-lg: 8px` (modales, popovers) ; `--akd-radius-full: 9999px` (pastilles d'état, toggle).

**Élévations** — discrètes ; en thème sombre la hiérarchie vient surtout de la couleur de surface, les ombres restent subtiles :

```
--akd-shadow-1: 0 1px 2px rgb(0 0 0 / 0.05);                              /* cartes */
--akd-shadow-2: 0 2px 8px rgb(0 0 0 / 0.08), 0 1px 2px rgb(0 0 0 / 0.04); /* popovers, dropdowns */
--akd-shadow-3: 0 8px 24px rgb(0 0 0 / 0.14), 0 2px 6px rgb(0 0 0 / 0.06);/* modales */
```

**Animation** — rapide, jamais bloquante :

```
--akd-duration-fast: 100ms;  /* hover, focus, toggle */
--akd-duration-base: 150ms;  /* dropdowns, tooltips, toasts */
--akd-duration-slow: 250ms;  /* modales, panneaux latéraux */
--akd-ease: cubic-bezier(0.2, 0, 0, 1);
```

Sous `prefers-reduced-motion: reduce` : toutes les durées passent à `1ms`, les animations d'état « progress » (pulsation de badge, spinner de timeline, shimmer de skeleton) sont remplacées par des représentations statiques équivalentes (icône fixe, texte « In progress »). Aucune information n'est portée uniquement par le mouvement.

### 2.6 Bloc CSS de référence (utilisable tel quel)

```css
/* packages/styles/tokens.css — source de vérité des tokens sémantiques.
   Thème par défaut : suivi du système (prefers-color-scheme),
   surchargé par data-theme="light|dark" posé sur <html> par le toggle. */

:root {
  /* ---- primitives (identiques aux tables 2.1–2.3) ---- */
  --akd-gray-0:#ffffff; --akd-gray-50:#fafafa; --akd-gray-100:#f4f4f5;
  --akd-gray-200:#e4e4e7; --akd-gray-300:#d4d4d8; --akd-gray-400:#a1a1aa;
  --akd-gray-500:#71717a; --akd-gray-600:#52525b; --akd-gray-700:#3f3f46;
  --akd-gray-800:#27272a; --akd-gray-900:#18181b; --akd-gray-950:#101012;
  --akd-teal-50:#f0fdfa; --akd-teal-100:#ccfbf1; --akd-teal-200:#99f6e4;
  --akd-teal-300:#5eead4; --akd-teal-400:#2dd4bf; --akd-teal-500:#14b8a6;
  --akd-teal-600:#0d9488; --akd-teal-700:#0f766e; --akd-teal-800:#115e59;
  --akd-teal-900:#134e4a;

  /* ---- typo / espace / forme / mouvement ---- */
  --akd-font-ui: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Ubuntu, "Helvetica Neue", Arial, sans-serif;
  --akd-font-mono: ui-monospace, "SF Mono", "Cascadia Mono", "JetBrains Mono", Menlo, Consolas, monospace;
  --akd-text-2xs:11px; --akd-text-xs:12px; --akd-text-sm:13px; --akd-text-md:14px;
  --akd-text-lg:16px; --akd-text-xl:20px; --akd-text-2xl:24px;
  --akd-weight-regular:400; --akd-weight-medium:500; --akd-weight-semibold:600;
  --akd-space-05:2px; --akd-space-1:4px; --akd-space-2:8px; --akd-space-3:12px;
  --akd-space-4:16px; --akd-space-5:20px; --akd-space-6:24px; --akd-space-8:32px;
  --akd-space-10:40px; --akd-space-12:48px; --akd-space-16:64px;
  --akd-radius-xs:2px; --akd-radius-sm:4px; --akd-radius-md:6px;
  --akd-radius-lg:8px; --akd-radius-full:9999px;
  --akd-shadow-1:0 1px 2px rgb(0 0 0 / .05);
  --akd-shadow-2:0 2px 8px rgb(0 0 0 / .08), 0 1px 2px rgb(0 0 0 / .04);
  --akd-shadow-3:0 8px 24px rgb(0 0 0 / .14), 0 2px 6px rgb(0 0 0 / .06);
  --akd-duration-fast:100ms; --akd-duration-base:150ms; --akd-duration-slow:250ms;
  --akd-ease:cubic-bezier(.2,0,0,1);
}

/* ---- thème clair (défaut) ---- */
:root, :root[data-theme="light"] {
  color-scheme: light;
  --akd-bg:            var(--akd-gray-0);
  --akd-surface:       var(--akd-gray-50);
  --akd-surface-hover: var(--akd-gray-100);
  --akd-border:        var(--akd-gray-200);   /* décoratif */
  --akd-border-input:  var(--akd-gray-500);   /* UI, 4.83:1 */
  --akd-text:          var(--akd-gray-900);   /* 17.72:1 */
  --akd-text-secondary:var(--akd-gray-600);   /* 7.73:1 */
  --akd-text-muted:    var(--akd-gray-500);   /* 4.83:1 */
  --akd-text-disabled: var(--akd-gray-400);
  --akd-accent:        var(--akd-teal-700);   /* texte/liens, 5.47:1 */
  --akd-accent-hover:  var(--akd-teal-800);
  --akd-accent-subtle: var(--akd-teal-50);
  --akd-on-accent:     var(--akd-gray-0);     /* sur teal-700, 5.47:1 */
  --akd-focus-ring:    var(--akd-teal-600);   /* 3.74:1 ≥ 3:1 UI */
  --akd-status-success-fg:#15803d; --akd-status-success-bg:#f0fdf4; --akd-status-success-dot:#16a34a;
  --akd-status-progress-fg:#2563eb; --akd-status-progress-bg:#eff6ff; --akd-status-progress-dot:#2563eb;
  --akd-status-warning-fg:#b45309; --akd-status-warning-bg:#fffbeb; --akd-status-warning-dot:#d97706;
  --akd-status-danger-fg:#dc2626;  --akd-status-danger-bg:#fef2f2;  --akd-status-danger-dot:#dc2626;
  --akd-status-neutral-fg:var(--akd-gray-600); --akd-status-neutral-bg:var(--akd-gray-100);
  --akd-status-neutral-dot:var(--akd-gray-500);
  --akd-danger:#dc2626; --akd-danger-hover:#b91c1c; --akd-on-danger:var(--akd-gray-0);
  /* logs & terminal : toujours sombres, même en thème clair */
  --akd-log-bg:var(--akd-gray-950); --akd-log-fg:var(--akd-gray-200); /* 14.98:1 */
  --akd-log-meta:var(--akd-gray-400);                                 /* 7.42:1 */
}

/* ---- thème sombre : par défaut via l'OS… ---- */
@media (prefers-color-scheme: dark) {
  :root:not([data-theme="light"]) { --akd-_dark:1; }
}
/* …et explicitement via le toggle. Les deux blocs partagent les mêmes valeurs
   (générées par le build depuis une seule définition ; dupliquées ici pour lisibilité). */
:root[data-theme="dark"] {
  color-scheme: dark;
  --akd-bg:            var(--akd-gray-950);
  --akd-surface:       var(--akd-gray-900);
  --akd-surface-hover: var(--akd-gray-800);
  --akd-border:        var(--akd-gray-800);
  --akd-border-input:  var(--akd-gray-500);   /* 3.93:1 ≥ 3:1 UI */
  --akd-text:          var(--akd-gray-100);   /* 17.29:1 */
  --akd-text-secondary:var(--akd-gray-400);   /* 7.42:1 */
  --akd-text-muted:    var(--akd-gray-500);
  --akd-text-disabled: var(--akd-gray-600);
  --akd-accent:        var(--akd-teal-400);   /* 10.21:1 */
  --akd-accent-hover:  var(--akd-teal-300);
  --akd-accent-subtle: color-mix(in srgb, var(--akd-teal-900) 40%, transparent);
  --akd-on-accent:     var(--akd-gray-950);   /* sur teal-400, 10.21:1 */
  --akd-focus-ring:    var(--akd-teal-400);
  --akd-status-success-fg:#4ade80; --akd-status-success-bg:#052e16; --akd-status-success-dot:#4ade80;
  --akd-status-progress-fg:#60a5fa; --akd-status-progress-bg:#172554; --akd-status-progress-dot:#60a5fa;
  --akd-status-warning-fg:#fbbf24; --akd-status-warning-bg:#451a03;  --akd-status-warning-dot:#fbbf24;
  --akd-status-danger-fg:#f87171;  --akd-status-danger-bg:#450a0a;   --akd-status-danger-dot:#f87171;
  --akd-status-neutral-fg:var(--akd-gray-400); --akd-status-neutral-bg:var(--akd-gray-800);
  --akd-status-neutral-dot:var(--akd-gray-400);
  --akd-danger:#f87171; --akd-danger-hover:#fca5a5; --akd-on-danger:var(--akd-gray-950);
  --akd-log-bg:var(--akd-gray-950); --akd-log-fg:var(--akd-gray-200);
  --akd-log-meta:var(--akd-gray-400);
}

@media (prefers-reduced-motion: reduce) {
  :root { --akd-duration-fast:1ms; --akd-duration-base:1ms; --akd-duration-slow:1ms; }
}
```

> Note d'implémentation : le mode « suivi du système » ne pose **aucun** `data-theme` ; le bloc `@media (prefers-color-scheme: dark)` applique alors les valeurs sombres (générées par le build à partir de la même définition que `[data-theme="dark"]`, pour éviter la duplication manuelle montrée ci-dessus). Le sentinel `--akd-_dark` ci-dessus est un raccourci de lisibilité du document, pas le code final.

### 2.7 Gestion du thème

- **Défaut : suivi du système** via `prefers-color-scheme` (décision actée). Trois valeurs utilisateur : `system | light | dark`.
- Le toggle (dans le menu utilisateur) pose `data-theme` sur `<html>` et persiste : **localStorage** (`akd.theme`) pour l'application instantanée avant bootstrap Angular (script inline anti-FOUC dans `index.html`), **préférence de compte** via l'API pour la synchronisation multi-appareils. localStorage prime au chargement, la préférence compte réconcilie après login.
- `color-scheme` est posé par thème pour que scrollbars, contrôles natifs et `<select>` suivent.
- Les deux thèmes sont testés au contraste en CI (voir §6) ; un composant ne peut pas passer AA dans un seul thème.

---

## 3. Inventaire des composants

Tous les composants sont des composants Angular **standalone** préfixés `akd-` (§5). Conventions transverses, applicables à tout l'inventaire :

- **Focus visible** : `outline: 2px solid var(--akd-focus-ring); outline-offset: 2px;` — jamais supprimé, jamais remplacé par un simple changement de couleur. Ratio ring/fond ≥ 3:1 dans les deux thèmes (§2.2).
- **Cible tactile/clic** : hauteur interactive ≥ 32px en densité par défaut (outil desktop, dérogation motivée à 44px mobile pour les actions d'urgence §22.4).
- **Désactivé** : `--akd-text-disabled` + `cursor: not-allowed` + `aria-disabled` (les boutons désactivés restent focusables pour rester découvrables au lecteur d'écran, avec tooltip expliquant pourquoi).
- **i18n** : toute chaîne rendue est une clé de traduction, y compris les `aria-label`.

### 3.1 `akd-button`

- **Anatomie** : conteneur, libellé, icône optionnelle (gauche ou seule), spinner intégré en état loading.
- **Variantes** : `primary` (fond `--akd-accent`… clair : teal-700/texte blanc 5.47:1 ; sombre : teal-400/texte gray-950 10.21:1) ; `secondary` (fond transparent, bordure `--akd-border-input`, texte `--akd-text`) ; `danger` (clair : blanc sur `#dc2626` 4.83:1 ; sombre : gray-950 sur `#f87171` 6.87:1) — réservé aux actions destructives ; `ghost` (texte seul, hover `--akd-surface-hover`) pour les actions tertiaires en table. Tailles `sm` (24px, tables), `md` (32px, défaut), `lg` (40px, onboarding).
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

- **Anatomie** : conteneur arrondi (`--akd-radius-full`), **pastille/icône + libellé texte — jamais la couleur seule**, fond tinté (`--akd-status-*-bg`), texte (`--akd-status-*-fg`).
- **Icônes par famille** : success `●` (check en variante icône), progress **anneau animé** (rotation ; statique sous reduced-motion), warning `▲`, danger `✕`, neutral `■`, unknown/stale `?` + **bordure pointillée** (`border: 1px dashed`), cancelled/superseded `⊘` + **libellé barré** (`text-decoration: line-through`).
- **Variantes** : `badge` (défaut) ; `dot` (pastille + texte sans fond, pour les tables ultra-denses — la pastille respecte 3:1 UI, §2.3) ; `dot-only` **interdit** hors cas où le libellé est adjacent dans la même cellule.
- **Comportements spécifiques** :
  - **stale** (§19.2) : dès que `observed_at` dépasse le seuil, le badge bascule sur `Unknown` avec tooltip « Last observed 12 min ago » — jamais un faux `Running`.
  - **superseded** (§21.1) : le tooltip/lien pointe vers le déploiement remplaçant.
  - divergence désiré/observé (§21.2) : le badge affiche l'**observé** ; l'écart avec le désiré est rendu par un second badge warning `Drift` à côté, pas par un mélange de couleurs.
- **A11y** : `role="status"` **non** utilisé (pas d'annonce spontanée en table) ; texte du libellé lisible tel quel ; l'animation progress est purement décorative (l'info est dans le texte).

### 3.7 `akd-card` / `akd-panel`

- **Anatomie** : surface `--akd-surface`, bordure `--akd-border`, rayon `--akd-radius-md`, ombre `--akd-shadow-1` (optionnelle) ; zones header (titre `--akd-text-lg` + actions) / body / footer.
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

- **Anatomie** : toolbar (recherche, filtres de niveau si structuré, follow/pause, wrap, timestamps on/off, téléchargement, plein écran) ; zone de log **virtualisée** (rendu fenêtré, obligatoire — cible : dizaines de milliers de lignes sans dégradation) ; fond `--akd-log-bg` (sombre dans les deux thèmes), texte `--akd-log-fg` (14.98:1), timestamps `--akd-log-meta` (7.42:1) en `--akd-font-mono` `--akd-text-2xs`.
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
- **A11y** : mode « Escape sort de l'éditeur » documenté et annoncé (piège à tabulation contrôlé, WCAG 2.1.2) ; thème de coloration syntaxique dérivé des tokens, vérifié AA dans les deux thèmes ; toutes les erreurs disponibles en liste texte hors de l'éditeur.

### 3.15 `akd-terminal` (§5.7, §13)

Conteneur **xterm.js** (dépendance spécialisée assumée, §25.3) : shell dans tout container ou serveur géré via WebSocket → SSH.

- **Anatomie** : barre de session (cible : serveur/container + badge de contexte, indicateur de connexion, bouton reconnect, kill), zone xterm (fond `--akd-log-bg` dans les deux thèmes), scrollback.
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
- **`akd-side-nav`** : navigation latérale par domaine fonctionnel (Dashboard, Servers, Projects, Security, Settings — alignée sur le lazy loading §25.2) ; anatomie : sélecteur de team en tête (frontière de sécurité §23.1 — le changement de team est global et explicite), items avec icône + libellé, compteurs d'alerte (serveurs unreachable, déploiements failed — dashboard §25.1) via badge, section pliable, footer (thème, user).
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

Deux densités globales, persistées avec le thème : **comfortable** (défaut) et **compact** (tables 32px→28px, espacements -1 cran, `--akd-text-sm` partout). Implémentées par un jeu de tokens `--akd-density-*` re-mappés via `data-density` sur `<html>` — les composants ne connaissent pas la densité, seulement leurs tokens. Les cibles interactives ne descendent jamais sous 24×24px (WCAG 2.5.8).

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
        tokens.css              # §2.6 — source de vérité
        reset.css
        themes/                 # généré : light.css, dark.css depuis une définition unique
    dashboard/                  # l'application (lazy loading par domaine §25.2)
```

- **Composants standalone**, préfixe de sélecteur **`akd-`** (réservé à la lib — lint : l'app ne déclare pas de `akd-*`), TypeScript strict, signals pour l'état local, `OnPush` partout, zoneless si la LTS le permet.
- La lib `akd-ui` **ne dépend pas** du client API généré : elle est purement présentationnelle (inputs/outputs/signals). Les composants connectés (ex. la page qui alimente `akd-log-viewer` depuis le flux realtime) vivent dans l'app.
- Les **tokens** vivent dans le package styles partagé, importés une fois globalement ; les styles de composants ne référencent que des `var(--akd-*)` (lint stylelint : couleur/dimension littérale interdite, voir §6).
- Les enums d'états consommés par `akd-status-badge` sont **générés depuis l'OpenAPI** (§24.1, §25.2) — le mapping état→famille vit dans la lib et sa complétude est testée contre l'enum généré.

### 5.2 Catalogue (§25.3 — exigence bloquante)

- **Storybook** (builder Angular officiel ; alternative acceptée : Analog/Sandbox équivalent, mais Storybook est le défaut proposé pour ses addons a11y/interactions).
- Règle PRD : **« un composant n'entre dans l'UI que s'il est au catalogue »** — appliquée mécaniquement : la CI échoue si un composant exporté de `akd-ui` n'a pas de `.stories.ts`, et l'app ne peut importer que ce que la lib exporte.
- Chaque composant expose une story par variante × état (incluant états d'erreur, vide, loading, stale, reduced-motion), rendue **dans les deux thèmes** (toolbar de thème globale) et les deux densités.
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

1. **Tokens only** : aucun hex/px/ms littéral dans son CSS — uniquement `var(--akd-*)` (stylelint bloquant en CI). Aucun style ne dépend du thème autrement que via les tokens sémantiques.
2. **AA dans les deux thèmes** : contrastes texte ≥ 4.5:1 et UI ≥ 3:1 vérifiés en clair **et** sombre — automatiquement (axe sur les stories des deux thèmes) et, pour toute nouvelle paire de couleurs, ratio calculé et consigné dans §2 de ce document.
3. **Clavier complet** : tous les usages opérables sans souris ; focus visible sur chaque élément interactif ; ordre de tabulation logique ; raccourcis documentés dans la story ; test d'interaction Storybook couvrant le parcours clavier nominal.
4. **États normalisés** : si le composant affiche un état métier, il compose `akd-status-badge` (jamais de couleurs d'état locales) ; états loading/empty/error/disabled définis, plus stale si le composant affiche des données observées (§19.2).
5. **Stories exhaustives** : une story par variante × état, deux thèmes, deux densités, reduced-motion ; addon a11y sans violation ; le composant n'est importable par l'app qu'une fois ses stories présentes (§5.2).
6. **Spec de test** : `.spec.ts` couvrant le contrat (inputs/outputs, états, rendu ARIA — rôles et attributs asserted) ; pour les composants à interaction riche (modal, table, log viewer), tests d'interaction (harness Angular CDK ou testing-library).
7. **i18n** : aucune chaîne en dur ; toutes les clés créées avec leur valeur anglaise ; `aria-label` paramétrables.
8. **Documentation** : description d'usage dans la story (quand l'utiliser / quand ne pas l'utiliser), et mise à jour de l'inventaire §3 si le composant est nouveau.

---

## Annexe A — Récapitulatif des ratios de contraste vérifiés

Méthode : luminance relative WCAG 2.1 (sRGB), ratio = (L1+0.05)/(L2+0.05). Script de vérification à intégrer en CI (les mêmes paires, en snapshot test sur `tokens.css`).

Paires critiques (texte : exigence 4.5:1 ; UI : 3:1) :

| # | Paire | Clair | Sombre |
|---|---|---:|---:|
| 1 | Texte principal / fond page | 17.72 ✅ | 17.29 ✅ |
| 2 | Texte secondaire / fond page | 7.73 ✅ | 7.42 ✅ |
| 3 | Bouton primaire (texte/fond accent) | 5.47 ✅ | 10.21 ✅ |
| 4 | Bouton/texte danger | 4.83 ✅ | 6.87 ✅ |
| 5 | Texte warning / fond page | 5.02 ✅ | 11.39 ✅ |
| 6 | Focus ring / fond page (UI 3:1) | 3.74 ✅ | 10.21 ✅ |
| 7 | Badge success (fg/bg tinté) | 4.79 ✅ | 8.55 ✅ |
| 8 | Bordure d'input / fond (UI 3:1) | 4.83 ✅ | 3.93 ✅ |
| 9 | Log viewer (texte/fond) | 14.98 ✅ | 14.98 ✅ |

Garde-fous documentés : `teal-600` sur blanc = 3.74:1 → **interdit pour du texte** (réservé au focus ring et aux éléments UI) ; `teal-500` sur blanc = 2.49:1 → décoratif uniquement ; pastilles d'état non textuelles ≥ 3:1 dans les deux thèmes (§2.3).
