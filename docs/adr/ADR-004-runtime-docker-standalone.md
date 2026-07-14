# ADR-004 — Runtime : Docker standalone confirmé, Kubernetes écarté, Swarm non réimplémenté

- **Statut** : Accepté
- **Date** : 2026-07-11
- **Sections PRD liées** : §27.4, §3.5, §16.1(3)(6), §16.2, §18.1, §26.1

## Contexte

Il faut choisir le runtime cible : Docker standalone (Engine/Compose), ou un orchestrateur (Swarm, Nomad, Kubernetes — y compris un Kubernetes « embarqué et transparent » type k3s) pour obtenir scheduling et haute disponibilité. La proposition de valeur du produit repose sur des VPS modestes (dès 2 GB de RAM), des objets Docker standards réversibles et un catalogue de templates compose.

## Décision

**Docker standalone est confirmé comme runtime** : Docker Engine, Compose et BuildKit.

**Kubernetes est écarté**, y compris sous forme « embarquée et transparente » : il contredit la proposition de valeur (VPS modestes dès 2 GB, objets Docker standards réversibles §16.1(3), catalogue de templates compose) et l'abstraction fuirait au premier incident — pods, PVC, ingress apparaîtraient face à des utilisateurs qui ont précisément choisi la plateforme pour ne pas apprendre Kubernetes.

**Swarm n'est pas réimplémenté** : au mieux une compatibilité dépréciée, derrière feature flag, en P3.

Un orchestrateur ne sera **réévalué que sur besoin utilisateur validé**, via le **contrat d'adaptateur runtime** (§18.1) — tous les appels au runtime passent par un adaptateur unique, instrumenté et sécurisé — et **sans jamais être imposé aux installations existantes**. Le présent ADR consigne ce rejet et ses motifs.

## Alternatives considérées

- **Kubernetes embarqué (k3s ou équivalent), masqué par l'UI** : rejeté — empreinte mémoire incompatible avec un VPS 2 GB, perte de la réversibilité « tout est du Docker standard », et abstraction qui fuit au premier incident (pods, PVC, ingress).
- **Docker Swarm réimplémenté proprement** : rejeté — déprécié chez la référence elle-même, stockage multi-nœuds non résolu, écosystème en déclin.
- **Nomad** : rejeté — orchestrateur supplémentaire à installer et apprendre, sans demande utilisateur validée ; réévaluable via l'adaptateur runtime.

## Conséquences

- **Positives** : empreinte minimale sur les serveurs cibles ; toutes les ressources restent des objets Docker/Compose standards, administrables hors de AkerDock (§16.1(3)) ; parité directe avec le catalogue de templates compose ; simplicité de diagnostic pour l'utilisateur.
- **Négatives** : pas de scheduling automatique multi-nœuds, pas de HA native d'une application (le multi-serveur reste du build → push registry → pull, avec load balancer externe, comme dans la référence §3.3) ; pas d'auto-scaling.
- **Risques acceptés** : les utilisateurs dont les besoins évoluent vers l'orchestration devront sortir de AkerDock ou attendre une réévaluation sur besoin validé ; le contrat d'adaptateur runtime doit rester réellement étanche pour que cette réévaluation reste possible sans réécriture des services métier.
