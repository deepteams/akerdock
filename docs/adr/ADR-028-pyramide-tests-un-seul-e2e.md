# ADR-028 — Pyramide de tests : un seul parcours E2E

- **Statut** : Accepté
- **Date** : 2026-07-18
- **Sections PRD liées** : §26.2, §26.3, §27.26, §29.9
- **Révise** : ADR-026 pour le volume et la cadence des E2E ; le choix Docker-in-Docker et ses risques résiduels restent inchangés

## Contexte

Le catalogue E2E a grandi jusqu'à sept suites indépendantes. Chaque suite
reconstruisait PostgreSQL, un serveur Docker-in-Docker, AkerDock et plusieurs
services auxiliaires. Même parallélisées, elles consommaient beaucoup de temps
runner, rendaient les échecs longs à reproduire et ralentissaient la boucle de
développement d'une petite équipe.

La majorité des garanties exercées par ce catalogue sont des règles
déterministes : validation, autorisation, interpolation, génération de
configuration, construction de commandes, transitions d'état, rétention,
backoff ou déduplication. Les prouver à travers le produit assemblé est plus
lent et moins précis qu'un test du module propriétaire.

## Décision

AkerDock maintient **exactement un parcours E2E produit automatisé**. Il prouve
uniquement la couture que les niveaux inférieurs ne peuvent pas établir :

1. démarrage réel avec migrations et bootstrap ;
2. ajout et validation SSH d'un serveur Docker-in-Docker ;
3. vérification de l'intention initiale `stopped`, puis démarrage explicite et
   bootstrap réel de Traefik ;
4. déploiement d'une application avec variable d'environnement ;
5. routage HTTPS et lecture des logs JSON/SSE ;
6. régénération d'une route après modification ;
7. redéploiement rolling sans requête perdue ;
8. refus des appels non authentifiés ;
9. suppression du container et de sa route.

Ce parcours :

- ne tourne **pas sur les pull requests** ;
- tourne après fusion sur `main`, à la demande et avant publication d'une
  release ;
- reste une seule commande, `make e2e`, sans shard ni catalogue nightly ;
- ne peut gagner un second scénario. Une nouvelle garantie va d'abord dans un
  test unitaire ou module. Si seul le produit assemblé peut la prouver, elle
  remplace ou enrichit une assertion du parcours existant sans créer une
  deuxième stack.

Les pull requests sont bloquées par les tests Go et Angular rapides, les tests
d'intégration PostgreSQL ciblés, la génération de contrat, le lint et les
fixtures de conformité. Le smoke de distribution reste un test de packaging
distinct : il ne pilote pas un parcours produit et s'exécute après fusion et
avant release.

## Règle de propriété des tests

Le test vit au plus bas niveau capable de prouver la garantie :

- **unitaire** : parseurs, validateurs, RBAC, calculs, états, rendu de
  configuration, échappement et commandes ;
- **module/intégration ciblée** : SQL concurrent, transport ou protocole contre
  sa dépendance réelle, sans démarrer AkerDock au complet ;
- **E2E** : seulement l'interaction réelle SSH + Docker + proxy + trafic lors
  d'une bascule.

Tout correctif de bug commence par une reproduction au niveau unitaire ou
module, sauf preuve écrite que seul le parcours assemblé peut reproduire le
défaut.

## Alternatives considérées

- **Conserver le smoke sur chaque PR et le catalogue nightly** : rejeté, car le
  coût quotidien et la maintenance restent ceux qui motivent cette décision.
- **Conserver plusieurs E2E mais les paralléliser davantage** : rejeté ; cela
  réduit le temps mural mais augmente le coût runner et ne rend pas les échecs
  plus locaux.
- **Supprimer tous les E2E** : rejeté ; aucun test isolé ne prouve une vraie
  bascule sans perte à travers SSH, Docker et Traefik.

## Conséquences

- **Positives** : feedback de pull request plus rapide, diagnostic plus local,
  moins de flakiness et de maintenance d'infrastructure ; stratégie tenable
  pour une petite équipe et prévisible pour une équipe moyenne.
- **Négatives** : les variantes de moteurs, providers et build packs ne sont
  plus rejouées bout en bout automatiquement. Leur contrat doit être couvert
  au niveau module et une validation manuelle ciblée reste nécessaire avant une
  évolution risquée.
- **Discipline requise** : retirer un ancien E2E sans test inférieur équivalent
  crée une dette explicite, pas une preuve implicite. Les matrices produit
  pointent vers les tests de module concernés.
- **Risque ADR-026 inchangé** : systemd, reboot réel, firewall, disque plein et
  ARM64 restent hors automatisation.
