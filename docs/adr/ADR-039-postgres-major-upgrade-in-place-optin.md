# ADR-039 — Upgrade majeur du PostgreSQL de l'instance : in-place opt-in via pgautoupgrade, backup-first

- **Statut** : Accepté
- **Date** : 2026-07-27
- **Sections PRD liées** : §14.3, §22.4, ADR-021, ADR-025
- **Supersede** : la clause « pas de `pg_upgrade` in-place entre volumes de containers » du runbook `upgrade-downgrade.md` §C (formulation, pas un ADR) — remplacée par le chemin outillé ci-dessous.

## Contexte

La distribution de référence (ADR-021) épingle la version majeure de PostgreSQL dans `docker-compose.yml`. Une majeure Postgres n'est **pas compatible in-place** entre volumes de containers : bumper l'image (ex. 16 → 17) sur un volume existant fait crash-looper le container (`database files are incompatible`). Jusqu'ici la seule procédure documentée était un dump/restore manuel — long sur une grosse base, et son texte décrivait un `mv postgres` de bind-mount qui ne correspond pas au volume nommé réellement utilisé (`akerdock_pgdata`). Comme la base contient **tout** l'état de l'instance (état + queue, ADR-025), l'opération est la plus destructrice qui soit ; l'automatiser silencieusement dans `install.sh` casserait l'invariant « rien n'est perdu » de cet install.

## Décision

- L'upgrade majeur reste **opt-in et explicite** — jamais lancé automatiquement pendant `install.sh` ni au boot.
- `install.sh` (et le message d'erreur au boot) **détecte** l'écart entre le major du volume et celui épinglé, et **s'arrête proprement** en pointant vers l'outil — au lieu de laisser le container crash-looper. Aucune donnée touchée par la détection.
- L'outil `scripts/pg-upgrade.sh` réalise l'upgrade **in-place** via l'image tierce **`pgautoupgrade/pgautoupgrade`** (qui embarque les binaires source + cible et exécute `pg_upgrade`), en mode one-shot, **précédé d'une copie complète du volume de données** (filesystem, agnostique de la version) conservée sous `backups/` comme unique rollback. La stack redémarre ensuite sur l'image **officielle** `postgres:<major>` — `pgautoupgrade` n'est utilisé que pendant la fenêtre de migration, jamais comme image runtime permanente.

## Alternatives considérées

- **Auto in-place silencieux dans `install.sh`** : rejeté — opération destructrice sur le datastore sans checkpoint humain ni garantie de backup, contraire à l'éthos « ne jamais risquer l'état persistant » (INV-015).
- **Dump/restore uniquement (statu quo)** : conservé en **repli** documenté, mais lent sur gros volumes et sans détection au boot ; `pg_upgrade` in-place est nettement plus rapide.
- **`pgautoupgrade` comme image runtime permanente** : rejeté — supply-chain tierce en permanence (ADR-021 vise le minimal/officiel) ; on ne l'expose que le temps de la migration.
- **Upgrade orchestré par le binaire akerdock lui-même** : rejeté — distroless sans outils `pg_*`, et le contrôle-plane parle à Postgres par le réseau, il ne peut pas migrer un format on-disk.

## Conséquences

- **Positives** : plus de crash-loop cryptique après un bump de major (détection + arrêt guidé) ; upgrade rapide et outillé, backup-first, avec rollback explicite ; l'image runtime reste l'officielle.
- **Négatives** : dépendance ponctuelle à une image tierce (`pgautoupgrade`), à épingler et à scanner ; l'opérateur doit disposer de ~2× l'espace du volume (copie + upgrade) le temps de la migration.
- **Risques acceptés** : l'in-place échoué laisse potentiellement un volume à moitié migré — mitigé par la copie préalable obligatoire et un message de restauration explicite ; la copie de rollback n'est supprimée qu'après plusieurs jours de fonctionnement vérifié.
