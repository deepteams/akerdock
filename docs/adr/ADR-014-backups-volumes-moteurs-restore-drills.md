# ADR-014 — Backups au-delà des bases : volumes chiffrés/dédupliqués, Redis et ClickHouse, restore drills

- **Statut** : Accepté
- **Date** : 2026-07-11
- **Sections PRD liées** : §27.14, §20.5, §7, §15, §26.2

## Contexte

Ne sauvegarder que les moteurs SQL/Mongo laisse la moitié de l'état sur la table : les données applicatives hors bases (uploads, fichiers des services one-click) ne sont pas couvertes. Et un backup qui n'a **jamais été restauré** n'offre aucune garantie (§7, §15). Il faut décider du périmètre de backup et du niveau de preuve exigé.

## Décision

Trois extensions actées (exigences détaillées au §20.5), en plus de la parité §7 :

1. **Backup des volumes applicatifs** : plans de backup sur les volumes et bind mounts des applications et services — pas seulement les bases — **chiffrés et dédupliqués** (outil type restic), avec option de quiesce/stop par ressource pour la cohérence, et la même planification, rétention locale/S3 et notifications que les backups de bases.
2. **Moteurs additionnels** : **Redis** (snapshot RDB) et **ClickHouse** couverts nativement, levant la limitation de parité (§15).
3. **Restore drills automatiques** : test de restauration périodique dans un environnement jetable — restauration réelle + vérification d'intégrité (checksum, comptage) — avec alerte si un plan de backup s'avère non restaurable. Un backup jamais restauré n'est pas considéré comme fiable.

Cette capacité est classée **P1** dans la matrice de parité (§26.2).

## Alternatives considérées

- **Parité stricte (4 moteurs, pas de volumes)** : rejetée — laisse les données applicatives sans protection et reproduit le défaut le plus grave de la référence : des restores jamais éprouvés.
- **Déléguer les volumes à un outil externe (restic/borg géré par l'utilisateur)** : rejeté comme réponse produit — sans intégration à la planification, à la rétention et aux notifications de la plateforme, la couverture reste aléatoire ; l'outil type restic est en revanche retenu comme brique interne.
- **Snapshots au niveau infrastructure (LVM/cloud snapshots)** : rejetés — dépendants du fournisseur, non portables entre serveurs, hors du contrat « n'importe quel serveur Linux ».

## Conséquences

- **Positives** : couverture complète des données (bases + volumes) ; chiffrement et déduplication réduisent coût de stockage et exposition ; les drills transforment les backups en garantie mesurée plutôt qu'en espoir ; différenciateur fort classé P1.
- **Négatives** : dépendance à un outil de backup de fichiers (type restic) à intégrer, superviser et mettre à jour ; le quiesce/stop optionnel introduit un compromis cohérence/disponibilité que l'utilisateur doit comprendre ; les drills consomment CPU, disque et temps sur une infrastructure jetable à provisionner.
- **Risques acceptés** : un backup de volume sans quiesce peut être incohérent pour les applications qui écrivent en continu (choix par ressource, documenté) ; les drills valident la restaurabilité technique, pas la validité métier des données restaurées.
