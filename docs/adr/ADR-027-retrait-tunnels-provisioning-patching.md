# ADR-027 — Retrait du périmètre : tunnels Cloudflare, provisioning cloud, server patching

- **Statut** : Accepté
- **Date** : 2026-07-14
- **Sections PRD liées** : §3.2, §3.6, §26.2

## Contexte

Le PRD héritait de trois capacités P3 issues de la parité avec le segment : les **tunnels Cloudflare** (§3.6 — exposition sans IP publique), le **provisioning cloud** (§3.2 — tokens fournisseur + création de VPS Hetzner) et le **server patching** (§3.2 — mises à jour APT/DNF/Zypper depuis le dashboard). Aucune n'est implémentée, aucune n'a d'exigence vérifiable au-delà de sa description, et chacune étend la surface du produit vers un métier adjacent : réseau d'accès pour les tunnels, gestion de flotte pour le provisioning, administration d'OS pour le patching — trois choses qu'un opérateur outillé fait déjà mieux ailleurs, et que le threat model devrait couvrir en entier si le produit les portait.

Trois ambiguïtés de nommage rendent le retrait piégeux : **Cloudflare** reste un provider **DNS-01** livré (wildcards, amendement n°21), **Hetzner** reste un provider **DNS-01 et S3** (§4.3, §7.2), et la table **`cloud_credentials`** existe en base (migration 00035) mais porte les **credentials DNS-01** — pas les tokens de provisioning que le dictionnaire décrivait.

## Décision

**Les tunnels Cloudflare, le provisioning cloud (tokens fournisseur + création de VPS) et le server patching sont retirés du périmètre produit.** Les sections §3.2 et §3.6 du PRD sont vidées au profit d'un renvoi vers cet ADR (la numérotation des sections est stable, elle ne bouge pas) ; la grille §26.2 porte la ligne avec le statut `Abandonné`.

Ne sont **pas** concernés : DNS-01 (Cloudflare, Hetzner et tout provider Lego — livré), les providers S3 compatibles (Hetzner inclus), la table `cloud_credentials` (réelle, elle porte les credentials DNS-01 — le dictionnaire est corrigé pour dire ce que la base fait vraiment), ni le firewall du fournisseur cloud recommandé par le threat model (à la charge de l'utilisateur, comme avant).

Cette décision est **réévaluable sur demande utilisateur avérée**, capacité par capacité.

## Alternatives considérées

- **Garder les trois en P3 indéfiniment** : rejeté — une capacité spécifiée mais jamais priorisée est une dette : elle maintient des tables, des permissions (`cloud:manage`), des entrées de threat model et des promesses d'API pour du code qui n'existe pas.
- **Retirer seulement du TODO, garder le PRD intact** : rejeté — le TODO est le suivi opérationnel, le PRD est la source de vérité (CLAUDE.md) ; un périmètre qui ne vit que dans le TODO diverge à la première relecture.
- **Implémenter a minima (tokens sans provisioning, patching en lecture seule)** : rejeté — une demi-capacité a le coût de surface d'une entière sans en avoir la valeur.

## Conséquences

- **Positives** : périmètre recentré sur le cœur (déploiement, bases, backups, adoption) ; suppression de la permission `cloud:manage`, de l'action sensible « suppression VPS » et de l'enum `cloud_provider` (jamais créé en base) ; le dictionnaire redevient exact sur `cloud_credentials`.
- **Négatives** : un serveur sans IP publique n'a pas de chemin d'accès géré (les tunnels l'auraient donné) ; la création de serveur reste entièrement manuelle ; les mises à jour d'OS restent à la charge de l'opérateur — assumé, c'est déjà la position du threat model pour le hardening.
- **Risques acceptés** : si la demande émerge, la réintroduction exigera un nouvel ADR et repartira de la spec — rien du retrait n'est irréversible, aucune donnée n'est détruite.
