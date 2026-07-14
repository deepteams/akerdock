# ADR-026 — Stratégie de tests E2E : Docker-in-Docker uniquement, risque résiduel documenté

- **Statut** : Accepté
- **Date** : 2026-07-11
- **Sections PRD liées** : §27.26, §29.9, §22.4, §26.2, §26.3

## Contexte

AkerDock pilote de vrais serveurs Linux : la matrice complète (OS variés, AMD64/ARM64, systemd, reboots, firewalls — §22.4) ne peut être couverte en E2E automatisé qu'au prix de VM provisionnées à chaque exécution — lentes, coûteuses et peu parallélisables. À l'inverse, des serveurs cibles simulés en containers (Docker-in-Docker) permettent des E2E rapides et gratuits sur chaque commit, mais ne reproduisent pas fidèlement un vrai serveur. Il faut arbitrer la stratégie d'automatisation et assumer explicitement ce qu'elle ne couvre pas.

## Décision

Les E2E automatisés s'exécutent en **Docker-in-Docker uniquement** : les serveurs cibles sont **simulés en containers** — rapides, gratuits, **parallélisables sur chaque commit**. Le plan de tests E2E (§29.9) couvre ainsi moteurs de bases, build packs, proxies, providers Git et storages S3.

**Risque résiduel accepté et documenté** : **systemd, les reboots réels, les firewalls, les disques pleins et ARM64 ne sont pas couverts par l'automatisation** — ces classes de bugs seront découvertes **en usage réel ou lors de validations manuelles ponctuelles**. La matrice OS/architecture reste supportée (§22.4) mais validée manuellement, hors automatisation (§29.9) ; la matrice de parité le trace explicitement (§26.2, ex. « VM/ARM64 non automatisé, risque accepté §27.26 »).

## Alternatives considérées

- **VM éphémères en CI (cloud ou libvirt/Vagrant)** : rejetées comme socle systématique — minutes de CI coûteuses, exécutions lentes, parallélisme limité ; la vitesse de la boucle de feedback sur chaque commit prime.
- **Stratégie hybride (DinD sur chaque commit + VM nightly)** : non retenue à ce stade — c'est l'évolution naturelle si les classes de bugs non couvertes se matérialisent, mais aucun pipeline VM n'est engagé aujourd'hui.
- **Ferme de matériel ARM64 dédiée** : rejetée — coût d'infrastructure et de maintenance disproportionné ; ARM64 reste supporté mais validé manuellement.

## Conséquences

- **Positives** : E2E sur chaque commit, sans coût d'infrastructure ni goulet de parallélisme ; les régressions du cœur (déploiements, proxy, backups, webhooks) sont détectées immédiatement ; l'exigence « au moins un E2E représentatif » de la Definition of Done (§26.3) reste tenable.
- **Négatives** : des pans entiers du réel ne sont **jamais** exercés automatiquement — interactions systemd, comportement après reboot d'un serveur, règles firewall, disque plein en cours de déploiement, spécificités ARM64 ; les bugs de ces classes atteindront des utilisateurs avant d'être connus.
- **Risques acceptés** : c'est le cœur de la décision — le risque résiduel (systemd, reboots, firewalls, disques pleins, ARM64 non couverts) est **explicitement accepté** et documenté partout où il compte (matrice §26.2, plan de tests §29.9) ; en contrepartie, des validations manuelles ponctuelles restent dues, et cette décision devra être révisée (nouvel ADR) si l'usage réel révèle une fréquence de bugs inacceptable dans ces classes.
