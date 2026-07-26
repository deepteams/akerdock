# ADR-035 — Routes de preview par table de templates

## Statut

Accepté — révise la formulation « un seul `preview_url_template` » de §5.6 (PRD)
sans toucher le reste du périmètre preview (ADR-011).

## Contexte

Une preview ne pouvait exposer qu'**une** URL, dérivée d'un unique
`preview_url_template` (`{{pr_id}}`, `{{domain}}`, `{{random}}`). Le multi-service
compose était géré par un préfixe automatique `<service>-<base>` non configurable.

C'est trop pauvre pour un usage sérieux du flow PR→preview : l'opérateur veut le
même contrôle qu'au **routing de l'application** (plusieurs hôtes, un port cible
par route), pas un motif unique subi.

## Décision

La config d'URL des previews devient une **table ordonnée de routes**, calquée
sur le routing d'application (ADR-034 voisin, §4.2) :

- Chaque ligne = `{ host, port? }` où `host` est un motif avec placeholders
  **`{{pr_id}}`**, **`{{service}}`**, **`{{domain}}`** (1er domaine de l'app),
  **`{{random}}`** (slug stable par preview).
- Une ligne **sans `{{service}}`** = une route explicite ; le service cible est
  résolu par le `port` (comme un domaine d'application, `resolveWebComponent`).
- Une ligne **avec `{{service}}`** = un gabarit appliqué à **chaque service
  servi** non déjà couvert par une ligne explicite ; le port est celui résolu du
  service (le `port` de la ligne l'emporte).
- Stockage : colonne `applications.preview_url_templates` (JSONB, tableau).
  **Rétro-compatibilité** : vide/absent ⇒ comportement historique
  (`preview_url_template` unique + auto-préfixe). Aucun backfill requis.
- `{{random}}` s'appuie sur `previews.random_slug`, généré une fois au scaffolding
  et réutilisé pour toutes les routes/déploiements — les hôtes restent stables
  (pas de churn de certificat).
- L'hôte **primaire** de la preview (`previews.fqdn`, affiché, SSO, feedback) =
  la 1re ligne résolue (`{{service}}` → composant web principal).

Le wildcard un-niveau (§4.2) reste la contrainte : un motif produisant plusieurs
niveaux sous le wildcard n'obtient pas de certificat (inchangé, énoncé).

## Conséquences

- **Positives** : parité UX avec le routing app, multi-hôtes/ports par preview,
  motifs maîtrisés au lieu du préfixe imposé ; rétro-compatible.
- **Négatives** : moteur de routing preview plus riche (résolveur partagé
  `single-container` + compose) ; `random_slug` ajouté ; surface de test accrue.
  La logique déterministe (résolution des motifs, mapping port→service) est
  prouvée en tests unitaires ; le comportement proxy/cert/SSO de bout en bout
  relève de la validation manuelle/E2E (ADR-028).

## Alternatives rejetées

- **Garder le template unique + plus de placeholders** : ne donne pas le contrôle
  par route/port demandé.
- **Base + overrides par service** : moins de rewrite mais deux mécanismes
  concurrents ; la table unique est plus cohérente avec le routing app.
