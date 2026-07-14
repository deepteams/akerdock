# TODO — feuille de route AkerDock

État au 11 juillet 2026. Ce fichier est le suivi opérationnel du reste à faire ;
la référence fonctionnelle reste le [PRD](docs/PRD.md) (matrice de parité §26.2)
et les [ADRs](docs/adr/README.md).

**Avancement** : **les 102 opérations** de [`openapi-v1.yaml`](docs/specs/openapi-v1.yaml)
sont implémentées, 30 migrations, 40+ vérifications E2E automatisées
([`scripts/e2e.sh`](scripts/e2e.sh) — ADR-026 ; shard `smoke` à chaque commit,
catalogue complet en nightly, conformément à la pyramide du plan de tests §2).
La surface du contrat v1 est donc complète : ce qui reste ci-dessous est de la
**profondeur** (moteurs, canaux, build packs) et non des endpoints manquants.

---

## Fait — socle P0 complet

Le vertical slice P0 du PRD §29 est bouclé et prouvé E2E :
auth/team → serveur SSH → déploiement (image, Dockerfile, Git) → domaine HTTPS
→ logs → suppression sûre.

| Capacité | Preuve |
|---|---|
| Isolation team, tokens à permissions, RBAC d'API | E2E : 404 inter-team (INV-002), anti-élévation de privilèges |
| Séquence de démarrage (migrations, root, clé d'instance) | E2E : marqueurs normatifs de `instance-config` §6.1 |
| Chiffrement enveloppe AES-256-GCM, clé maître multi-versions | Tests unitaires + E2E (variables, clés SSH) |
| Queue durable PostgreSQL (ADR-002) | E2E : leases, reprise, retries, dead-letter, retry lié, forget |
| Onboarding serveur + bootstrap Traefik | E2E DinD |
| Moteur de déploiement (3 sources) | E2E : image, Dockerfile inline, Git (SHA figé) |
| **Zero-downtime réel** (ADR-015) | E2E : ~100 requêtes pendant un redéploiement, **0 perdue** |
| Proxy : application atomique, vérification, rollback, anti-dérive | E2E : révisions checksummées, réparation d'une édition manuelle |
| Rollback par artifact (ADR-006) | E2E : v2 → rollback → v1, sans rebuild |
| Volumes persistants (INV-008) | E2E : données conservées au redéploiement et à la suppression |
| Webhook CI + coalescing des pushes | E2E : par uuid/tag, anciens déploiements `superseded` |
| Audit append-only + outbox transactionnelle | E2E : actions auditées, événements publiés |
| Temps réel SSE (ADR-024) | E2E : transitions live, reprise `Last-Event-ID` |
| Idempotence HTTP + rate limiting (§24.1) | E2E : rejeu, `409` sur corps différent, `429` + `Retry-After` |
| 4 modes du binaire (ADR-021) | `all-in-one`, `api`, `worker`, `scheduler` (élection par advisory lock) |
| Bases managées PostgreSQL + backup/restore vérifié | E2E : credentials rédigés, données survivant à un restart, dump corrompu refusé |
| Rotation de la clé maître (ADR-003) | E2E : v1 → v2, toutes les lignes convergées, secrets toujours lisibles |
| Certificats : reflet observé du store ACME | E2E : synchronisation depuis le serveur, aucune clé privée en base |
| Backups planifiés par cron (§7.1) | E2E : occurrence déclenchée par le scheduler seul, fenêtre avancée |
| Dépôts Git privés par deploy key (§5.1) | E2E : clone SSH authentifié, clé effacée du serveur après le clone |
| Backups vers S3 (§7.2) | E2E MinIO : upload présigné, `s3_only`, restore depuis le bucket |
| Build pack static (§5.2) | E2E : site servi par nginx, assets et deep links SPA |
| Notifications (§11, ADR-019) | E2E : événement de déploiement routé vers un vrai webhook, avec sa sévérité |
| Alerte d'expiration des certificats (§4.3) | E2E : alerte émise une seule fois au franchissement du seuil |
| Distribution (ADR-021) | Smoke test : image distroless sans shell, `docker compose up -d`, healthcheck intégré |
| Build pack nixpacks (§5.5) | E2E : app Node sans Dockerfile, buildée et routée |
| Résumé différé (ADR-019 §4) | E2E : aucun message unitaire, un seul message groupé à l'échéance |
| Webhooks Git signés (INV-009) | E2E : 5 livraisons (signée, rejeu, falsifiée, `[skip ci]`, autre branche) → **1 seul** déploiement |
| Épinglage du host key SSH (§20.1) | E2E : clés d'hôte régénérées → connexion refusée et nommée, jamais ré-épinglée |
| Variables multilignes | E2E : clé PEM avec métacaractères reçue littéralement dans le conteneur |
| Télémétrie OTLP (ADR-008) | E2E : collecteur factice — traces et métriques réellement exportées |
| Audit : diff redacté (§23.4) | E2E : le `from`/`to` est enregistré, aucune valeur secrète dans la table |
| Hooks pre/post-déploiement (§10) | E2E : `pre` sauté sans container, `post` en échec ⇒ pas de bascule, candidat nettoyé |
| Reprise après crash (§2.5, INV-004) | E2E : crash simulé en pleine bascule → inspection, reprise au rename, **pas de double bascule** |
| Remnants d'une suppression ratée (§20.6.4) | E2E : inventaire réel, `forget` refusé sans acquittement, rien supprimé à distance |
| Rolling upgrade N-1/N (§18.2) | Migrations expand-only (test + mutation) ; le binaire N-1 sert contre le schéma N |

---

## P1 — PaaS utilisable

### 1. Bases de données managées (§6) — PostgreSQL fait, reste les moteurs et le SSL
- [x] Migration `databases` + `database_credentials` (mots de passe chiffrés enveloppe)
- [x] `POST /databases/postgresql`, `GET/PATCH/DELETE /databases/{uuid}` + `start`/`stop`/`restart` (8 opérations)
- [x] Job de provisioning idempotent (volume, réseau, credentials générés, attente de `pg_isready`)
- [x] Réservation de port public : unicité `(server, port)` garantie en base (§22.3)
- [x] Rédaction INV-003 : mot de passe et URLs seulement avec `read:sensitive`
- [x] **URL externe** (`external_url`) : construite seulement si la base est *réellement* joignable de l'extérieur (`is_public` **et** port lié) — une chaîne de connexion publiée qui ne connecte pas est pire que pas de chaîne du tout, elle a l'air utilisable. Prouvé E2E en s'y connectant vraiment
- [x] **SSL des bases** (§6.3, amendement n°23) : **une CA par serveur** — le serveur est le rayon d'explosion ; une CA d'instance qui fuite laisserait usurper *toutes* les bases de *tous* les serveurs. La clé privée de la CA ne quitte jamais le control plane (chiffrée au repos) ; ce qui part sur le serveur est une feuille signée, ce qui part au client est le **certificat de CA — public par nature**, car c'est ce qui lui permet de *vérifier* : un TLS que le client ne vérifie pas ne protège de rien. Prouvé E2E avec `sslmode=verify-ca`. L'uid postgres est **lu dans l'image**, pas deviné (999 sur Debian, 70 sur Alpine) : le deviner donne une base qui démarre, n'arrive pas à lire sa propre clé, et redémarre en boucle
- [x] **Proxy TCP dynamique** (§6.2, §2.6, amendement n°22) : en `tcp_proxy`, la base ne publie **aucun** port sur son container — c'est le proxy qui écoute. Changer le port public recrée donc le **proxy** (quelques secondes, aucune donnée touchée) au lieu de redémarrer la base (et de couper toutes les connexions ouvertes). L'entrypoint est statique parce que Traefik ne sait pas ajouter un listener à chaud : c'est précisément pourquoi le port vit là. `HostSNI(`*`)` = passthrough brut, le TLS de la base traverse tel quel. Fixture de conformité octet à octet
- [ ] Les 7 autres moteurs (MySQL, MariaDB, MongoDB, Redis, KeyDB, Dragonfly, ClickHouse) — **hors v1 par décision produit** : le contrat acte « PostgreSQL seul ». Les ajouter demande un amendement + une abstraction moteur (provisioning, backup, drill) : lot dédié

### 2. Backups et restore (§7, §20.5, ADR-014) — fait, sauf les drills
- [x] Migration `s3_storages`, `database_backup_plans`, `backup_executions`
- [x] Plans de backup (cron/alias validés), exécutions, `restore` (202 + job) — 6 opérations
- [x] Job de backup : `pg_dump` dans le container, checksum SHA-256, taille, version du moteur
- [x] Restore avec **vérification d'intégrité obligatoire** — un dump corrompu n'est jamais restauré
- [x] Rétention locale : le dernier backup valide n'est jamais supprimé
- [x] **Upload S3** : client S3 maison (SigV4, `internal/s3` — pas de SDK AWS, cf. ADR-021), CRUD `/s3-storages` (5 opérations, amendement de spec n°10), vérification écriture/lecture/suppression avant de déclarer un bucket utilisable
- [x] Le dump va **du serveur au bucket directement** par URL présignée passée à `curl` via stdin (jamais dans argv, INV-003) — jamais de transit par l'instance ; upload confirmé par la taille lue dans le bucket
- [x] Échec d'upload ⇒ statut `partial` (le backup local existe), jamais un succès silencieux ; `s3_only` ne supprime le local qu'après confirmation (INV-008)
- [x] Restore depuis le bucket (re-téléchargement puis vérification du checksum, comme pour un dump local)
- [x] Credentials S3 dans l'inventaire de rotation de la clé maître (ADR-003)
- [x] **Rétention** locale et S3, indépendantes et cumulatives (compte **et** âge) — le dernier backup réussi n'est jamais purgé, quelles que soient les règles
- [x] **Exécution planifiée** : parseur cron interne (`internal/cronexpr`, testé unitairement — plages, pas, fuseaux, DST), `next_run_at` possédé par le scheduler, déclenchement sans chevauchement (`lock_key` par plan) — prouvé E2E sans aucun appel manuel
- [x] **Restore drills automatiques** (ADR-014, amendement de spec n°16) : le dernier dump est restauré dans une base **jetable** sur le même serveur — jamais dans la base vivante, un drill qui touche la production serait une panne programmée — puis la base est détruite, y compris en cas d'échec (la laisser « pour inspection » laisserait une copie des données de production sur le serveur)
- [x] **Le drill compare, il ne se contente pas de ne pas échouer** : le nombre de tables de la base source est enregistré **au moment du dump**, et le drill exige le même nombre au retour. Un dump peut se décompresser proprement, se restaurer sans erreur et ne rien contenir — un `psql` qui restaure du vide sort en 0 (`ON_ERROR_STOP=1` transforme « quelques erreurs affichées » en « restauration échouée »)
- [x] **Un drill qui échoue est bruyant** : `backup.drill_failed.v1` (critique par suffixe) — c'est exactement le mode de défaillance que la fonctionnalité existe pour attraper : des backups au vert pendant des mois qui se restaurent en rien. Un drill raté est un **résultat**, pas un job à rejouer : le dump ne deviendra pas restaurable en réessayant dans une minute

### 3. Sources Git avancées (§5.1) — deploy keys faites
- [x] **Deploy keys** pour dépôts privés : URL SSH (`git@host:org/repo.git` ou `ssh://`), grammaire fermée (INV-012), clé installée sur le serveur de build par stdin puis **retirée après le clone** (INV-003), `GIT_SSH_COMMAND` avec `IdentitiesOnly` et `accept-new`
- [x] Table `git_sources` (data-dictionary §7.1) : une source par (team, clé), FK `applications.git_source_id` posée
- [x] `in_use` réel sur les clés privées, suppression d'une clé référencée refusée en `409` (§19.2)
- [x] `github_apps`, `repositories` (data-dictionary §7.2/§7.3, git-webhook-protocols §2) : **manifest flow** complet (brouillon + state anti-CSRF haché usage unique → conversion → credentials chiffrés enveloppe, jamais journalisés), installation (setup redirect + webhook `installation`, premier signal gagnant), **découverte des dépôts** (cache `repositories` par `external_id`, resync sur `installation_repositories`), **clone par token d'installation** (JWT RS256 maison testé, token restreint au repo, `GIT_ASKPASS` uploadé par stdin — jamais argv), **auto-deploy via le webhook de l'app** (fan-out par repo, politiques partagées avec les webhooks manuels, coalescing), création d'application par App + repo découvert (deploy key refusée en doublon), page dashboard GitHub Apps + sélecteur à la création. GHES supporté (`api_url`/`html_url`). Restent (tasks) : previews de PR (ADR-011), feedback riche (checks/deployments/commentaire upserté — client prêt et testé), E2E mock GitHub
- [x] **Webhooks GitHub/GitLab/Gitea signés** (amendement de spec n°12) : signature vérifiée sur le **corps brut avant tout parsing**, comparaison en temps constant, hors `/api/v1` et hors middleware Bearer ; une signature invalide est auditée puis `401` et ne déclenche **rien**
- [x] **Déduplication `(provider, delivery_id)`** (INV-009) : aucune forge ne signe d'horodatage, donc l'anti-rejeu repose entièrement dessus — d'où la purge des livraisons bornée à 30 jours
- [x] **Auto-deploy sur push** + `[skip ci]`, filtre de branche et `watch_paths` (monorepos) ; chaque refus est journalisé avec sa raison (`ignore_reason`)
- [x] L'enqueue de déploiement webhook est **partagée entre l'API et le moteur** — un push et un `/api/v1/deploy` empruntent le même chemin, coalescing compris

### 4. Build packs restants (§5.2) — static et nixpacks faits
- [x] **Static** (Nginx) : Dockerfile synthétisé (aucun toolchain exécuté au build), config nginx avec repli SPA sur `index.html`, `publish_directory` validé (INV-012)
- [x] **Nixpacks** : binaire épinglé par release (`NixpacksVersion`), provisionné à l'onboarding (échec non bloquant) et réinstallé à la demande au build ; plan tracé dans les logs ; variables de build par l'environnement, jamais en argv
- [x] **Railpack** (bêta) — refusé explicitement en `422` (`not_implemented`), jamais silencieusement traité comme du Nixpacks
- [x] **Build args et secrets BuildKit** (§5.2, amendement de spec n°14) : `build.env` était sourcé mais `docker build` **n'hérite pas de l'environnement** — aucune variable de build n'atteignait le Dockerfile. Pire, `is_secret` **n'était ni exposé par le contrat ni persisté** : toute variable de build serait devenue un `--build-arg`, donc visible dans `docker history`. Désormais : variable ordinaire → `--build-arg` ; variable secrète → **secret BuildKit** monté le temps d'un `RUN`, absent de toutes les couches (INV-003). Prouvé E2E : le secret n'est ni dans l'historique, ni dans la config, ni dans une couche
- [x] **Mode static de Nixpacks** : l'image nixpacks n'est que le *builder* — ce qui part en production est nginx servant le répertoire produit. Déployer l'image du builder embarquerait toute la toolchain **et ne servirait rien** : la commande de build sort, et un container dont la commande sort est un container à terre. Prouvé E2E : le site est servi, et `node` est **absent** de l'image déployée
- [x] **Build servers dédiés** (§3.4, amendement de spec n°19) : une seconde machine construit, pousse dans un registry, et le serveur de déploiement **retire par digest** — le digest est ce qui rend « l'image qui tourne » et « l'image construite » provablement identiques, et c'est ce qu'un rollback rejoue. Sans registry de push, `use_build_server` est refusé en `422` : l'image resterait sur la machine de build et la cible ne pourrait jamais la tirer. Un build server ne route rien (pas de proxy) et ne peut pas être cible de déploiement. Sélection **aléatoire** parmi les build servers prêts (un ordre fixe enverrait tout sur la même machine, ce qui est l'inverse du but) ; architectures incompatibles refusées avant de produire un `exec format error` qui ressemblerait à un bug de l'application
- [x] **Registries privés** (amendement n°17) : mot de passe chiffré au repos, jamais relu par l'API, n'atteignant le serveur que par le **stdin** de `docker login --password-stdin` — jamais dans `argv`, où un `ps` le lirait ; `docker logout` garanti après le pull, y compris sur annulation

### 5. Certificats (§4.3) — fait, reste l'alerting et DNS-01
- [x] Migration `certificates` (reflet observé, §18.3 — aucune clé privée stockée)
- [x] `GET /servers/{uuid}/certificates` (+ filtre `expiring_within_days`), `GET /certificates/{uuid}`, `POST /certificates/{uuid}/renew` (202 + job)
- [x] Job `certificate.sync` : lecture de l'`acme.json` du proxy en SSH, parsing de la chaîne publique (`x509`), réconciliation
- [x] Job `certificate.renew` : sauvegarde du store ACME puis redémarrage du proxy — l'ancien certificat continue de servir
- [x] **Alerte d'expiration J-30/J-7** : le scheduler émet `certificate.expiring.v1` dans l'outbox — une seule fois par seuil franchi, jamais à chaque passe
- [x] **DNS-01 pour les wildcards** (proxy-contract §7.2, amendement n°21) : un wildcard **ne peut pas** être validé en HTTP-01 (la CA n'a aucun hôte unique à interroger) — un `wildcard_domain` sans credential DNS-01 est donc refusé en `422`, au lieu de laisser toutes les URLs de preview sur le certificat auto-signé, pour toujours, sans que rien ne le dise. Le credential est **matérialisé en `acme.env` 0600** et injecté par `--env-file` : jamais dans `traefik.yaml` (qui est checksummé, stocké en révision et relu — un secret là-dedans serait une seconde copie du secret en base). Fixture de conformité : une route sous le wildcard passe en DNS-01, une route hors wildcard reste en HTTP-01

### 6. Notifications (§11, ADR-019) — ADR tenu, restent des canaux
- [x] Migrations `notification_channels`, `notification_rules` (config chiffrée) et `notification_deliveries` (idempotence + historique)
- [x] 7 opérations `/notification-channels` (+ règles, + test d'envoi) — amendement de spec n°11
- [x] **Consommation de l'outbox** par le scheduler avec curseur ; une purge ne dépasse jamais le curseur (une alerte ne peut pas être perdue par la rétention)
- [x] **Routage** par projet/environnement/sévérité ; **débounce** anti-flapping et **heures calmes** (fenêtre à cheval sur minuit) — un événement `critical` traverse toujours les deux
- [x] Canaux **webhook, Slack, Discord** ; les envois supprimés sont journalisés (`suppressed_reason`), jamais perdus
- [x] Canaux **SMTP, Resend, Telegram, Pushover** (amendement de spec n°18) : un canal est validé **pour le transport qu'il prétend être** — un canal `telegram` portant une URL de webhook et aucun `chat_id` serait accepté, stocké, puis échouerait au seul moment qui compte. Le chiffrement SMTP est **déclaré, jamais déduit du port** : le déduire, c'est envoyer un jour le mot de passe et l'alerte en clair sans que rien ne le dise (et si le serveur n'offre pas STARTTLS alors qu'un mot de passe est configuré, l'envoi est **refusé**). La clé Resend part en en-tête, jamais dans l'URL (une URL voyage dans les logs, les proxys et les referrers). Forme des payloads testée contre un vrai serveur pour chaque fournisseur ; SMTP prouvé **E2E contre un vrai relais** (mailpit) — l'assertion est que le mail est *arrivé*, pas que le code n'a pas planté
- [x] **Résumé différé** : les livraisons `pending` *sont* la file (aucune table à synchroniser) ; un `critical` n'attend jamais un résumé ; fenêtre configurable par règle

### 7. Invitations d'équipe (§10.1) — fait, reste l'email
- [x] `GET/POST /teams/{uuid}/invitations`, `DELETE .../{invitation_uuid}`
- [x] Le lien est un secret : seul son SHA-256 est stocké, la valeur claire n'est retournée qu'une fois (§23.2)
- [x] **Envoi par email transactionnel** (§14.2, amendement n°20) : relais d'instance (SMTP ou Resend) **vérifié avant d'être accepté** — un relais injoignable est refusé là où l'opérateur regarde, pas à la première invitation où le seul symptôme serait un mail qui n'arrive jamais. Le mail est un **ajout**, jamais un remplacement : le lien reste dans la réponse (§23.2), sinon une instance sans relais ne pourrait plus inviter personne et un relais mal configuré avalerait l'invitation

### 8. Tâches planifiées par ressource (§5.7)
- [x] Migrations `scheduled_tasks` / `task_executions` + 7 opérations (amendement de spec n°15) : cron validé à l'entrée par le **même** `cronexpr` que le scheduler — une expression que le scheduler ne saura pas déclencher est refusée en `422`, jamais acceptée puis jamais lancée
- [x] **Exécution par le scheduler** (prouvée E2E **sans aucun appel manuel**) : `docker exec` dans le container de la ressource, commande **quotée** et non assainie (c'est du shell voulu), sortie **tronquée par la queue** (on garde la fin — c'est là qu'est l'erreur) et bornée à 64 Kio
- [x] **Une occurrence non exécutée laisse une trace** (`skipped` + raison) : un historique vide se lit exactement comme « rien n'a jamais été planifié » — c'est ainsi qu'un job nocturne disparaît pendant un mois
- [x] Politique `overlap` (`skip` par défaut : un cron qui déclenche plus vite qu'il ne finit n'empile pas les exécutions) — appliquée **aussi au déclenchement manuel**, sinon la politique aurait une porte dérobée
- [x] Politique `missed_run` (`run` / `skip`) : « manquée » est définie par la **période du cron elle-même** (une tâche minutée tolère une minute de retard, une tâche quotidienne un jour) — pas de constante magique
- [x] Une commande qui échoue est un **résultat**, pas un job à rejouer : le code de sortie est historisé et `scheduled_task.failed.v1` part dans l'outbox (critique par suffixe, ADR-019). Rejouer aurait relancé la commande de l'opérateur dans son dos

---

### Proxy — cycle de vie par serveur (§3)
- [x] `POST /servers/{uuid}/proxy/{start|stop|restart}` (202 + job) et `GET /servers/{uuid}/proxy/logs` ; `proxy_desired_state` / `proxy_observed_status` exposés (intention et observation côte à côte, jamais fusionnées)
- [x] **L'intention est respectée** : un proxy volontairement arrêté n'est **pas** « réparé » par la réconciliation de dérive — le drift loop répare les accidents, or une intention n'est pas un accident
- [x] `start` **converge** (bootstrap) au lieu d'un `docker start` sur un nom qui peut ne plus exister ; verrou par serveur (une action proxy à la fois)
- [x] Dashboard : section Proxy sur le détail serveur (statuts, ports d'écoute, start/stop/restart, **confirmation nommant le rayon d'explosion**, bannière quand il est arrêté, logs)
- [x] E2E (shard `deploy`) : stop → **plus aucun trafic sur le serveur** (connexion refusée), intention persistée, **non réparée** par le reconciler → start → routes de retour ; logs servis par l'API

## P2 — Parité large

- [ ] **Compose / services one-click** (§9, spec `compose-spec.md`) : sous-ensemble supporté, magic variables, `service_components`, zero-downtime par service (ADR-015)
  - [x] Schéma : tables `services` / `service_components` (FK `resources` — amendement data-dictionary §9.2), FK `domains.service_component_id` et `database_backup_plans.service_component_id`, `default_route_port` (migration 00038)
  - [x] `internal/compose` : parse (compose-go v2), politique clé par clé §1.2–1.5 (codes stables §11), transformations §2 (réseau isolé, préfixage volumes, depends_on → plan topologique, limites normalisées, extensions `x-akerdock`, mode raw §9) — testé en unitaire
  - [x] Magic variables `SERVICE_*` §4 : parsing/scan/génération CSPRNG (unitaire), persistance chiffrée `is_generated` au déploiement, `SERVICE_FQDN_<ID>` = intention → domaine généré depuis le wildcard serveur (§6)
  - [x] Contrat : `build_pack: compose` sur les applications Git (`compose_file_location`, `raw_compose`), `GET /applications/{uuid}/components` (`ServiceComponent`)
  - [x] Moteur (itération 1 — recreate) : `executeCompose` — clone, validation avec findings dans les logs, sync composants, réseau/volumes labelisés, pull/build + create par service en ordre topo (aliases, env par sourcing, limites cgroups §8.5, health flags §7.1, one-shots §7.3), statuts observés par composant ; reprise = échec explicite (§2.5)
  - [x] Proxy : routage par composant (endpoint par route, port `target_port` → `default_route_port` → erreur déterministe)
  - [x] Dashboard : build pack Docker Compose (création + settings), composants du stack sur le détail
  - [x] Zero-downtime par service web §8.2 : images build/pull **avant toute mutation** (digest-pinnées §18.3), diff par service (hash de config sur le container — un service inchangé n'est pas remplacé), candidat `-next` sans alias court (§8.3), bascule proxy du seul composant (candidat par IP puis stabilisation par nom), promotion stop→rename→re-alias, échec = candidat supprimé et ancien routé (C2), services déjà basculés conservés (C3) ; inéligibilité §8.4 tracée dans les logs (raw, opt-out, ports hôte, pas de healthcheck — celui de l'image compte)
  - [x] Endpoints `/services` (12 opérations : CRUD avec validation compose-spec §11 au save — 422 à codes stables, `build:` refusé inline —, composants, deploy, start/stop/restart par labels avec one-shots exclus, envs, deployments) ; moteur : stack inline via la même machinerie (source = ligne `services`, répertoire `/data/akerdock/services/<uuid>`), suppression et lifecycle par labels (réseaux du stack inclus) ; UI : pages Services (création avec éditeur compose validé, détail avec éditeur/If-Match, composants, lifecycle, suppression) ; E2E : scénario inline dans le shard `compose` (422, deploy, domaine magique routé, stop/start, suppression réseau compris) — 8 checks verts
  - [ ] Reprise fine après crash (inspection par service), hooks pre/post stack
  - [ ] Backups des bases internes §10 (plans sur `service_component_id` — schéma prêt)
  - [x] E2E DinD : shard `compose` (nightly) — stack trois services depuis Git, domaine généré par `SERVICE_FQDN`, routage par composant, one-shot exit 0, magic secret persisté ; redéploiement : service inchangé non touché (hash), web basculé zero-downtime
- [ ] **Catalogue one-click** (ADR-010) : dépôts Git de templates signés + dépôts utilisateur
- [ ] **Previews de PR** (§20.4, ADR-011) — moteur livré, reste l'UI/réglages/E2E
  - [x] Schéma : `previews` (identité déterministe (application, provider, pr_id) jamais recyclée), `deployments.preview_id` (migration 00040)
  - [x] Moteur : instance par PR **à côté** de la production (nommage Docker par uuid de preview, INV-011), SHA de PR figé, **jeu de variables `is_preview` dédié** (INV-010 — jamais les secrets de prod, binds de prod jamais montés, volumes éphémères), routage protégé par défaut (**basic auth** générée + bcrypt dans le fichier Traefik, `X-Robots-Tag: noindex`)
  - [x] Cycle de vie : opened/synchronize/reopened → deploy (drafts opt-out), closed → destruction complète (routage, containers, volumes, dossier ; `cleanup_failed` retryable), **TTL d'inactivité** + **plafond de concurrence** avec file promue par le scheduler ; forks ignorés sans approbation (INV-010)
  - [x] Feedback riche §20.4.6 (best-effort) : **commentaire unique upserté** par marqueur + **check run** par SHA (condition de merge possible)
  - [x] Réglages au contrat + Settings (enabled, url_template, cap, TTL, protection, forks, drafts — PATCH partiel), onglet Previews (statuts, URLs, **approbation de fork** avec promotion immédiate), endpoints GET previews + POST approve
  - [x] **Forks** (§20.4.8, INV-010) : ignorés par défaut ; approbations activées → la preview attend **sans rien builder** ; après approbation d'un mainteneur, déployée depuis **`refs/pull/<n>/head`** du dépôt de base (le seul ref atteignable pour un fork), SHA vérifié contre celui annoncé par la livraison
  - [x] **Deployments API** (« View deployment » sur la PR) : environment transient `preview/pr-<id>` + statut avec l'URL
  - [x] **E2E** : shard `github` (nightly, 11 checks) contre un **stub HTTPS de l'API GitHub** (CA privée — même chemin qu'un GHES, `AKERDOCK_GITHUB_CA_FILE`) : manifest flow (state à usage unique prouvé par un rejeu refusé), installation, discovery via un vrai échange JWT RS256 → token d'installation, application créée depuis le dépôt découvert et clonée, **push signé → auto-deploy**, **PR → preview protégée** (401 anonyme / 200 avec le credential généré) + commentaire unique + check run + deployment, **fork approuvé**, **PR fermée → destruction**
  - [ ] Parité GitLab/Gitea du feedback, compose previews, contrôles de déclenchement (labels, `/deploy`, annulation des builds obsolètes), scale-to-zero
- [ ] **Terminal** (WebSocket, PTY, audit d'ouverture/fermeture — ADR-024)
- [ ] **Adoption de ressources existantes** (§20.7, ADR-013/023) — reprise des containers et stacks Docker déjà déployés, sans redéploiement : c'est le chemin de migration entrant
- [ ] **Uptime monitoring** (ADR-017) : checks HTTP/TCP hors workload, historique, alerting
- [ ] **Config as code** (§24.5, ADR-012) : export YAML, apply idempotent avec dry-run, provider Terraform
- [ ] **CLI `akerdock up`** (ADR-018) : déploiement d'un contexte local
- [ ] **Caddy** comme second proxy (l'IR est déjà provider-agnostique — ADR-009)
- [ ] **Sentinel** (agent de métriques) et **log drains** (§3.8, §13)
- [ ] **OAuth/OIDC** dashboard, MFA TOTP (tables `identities`/`mfa_factors` déjà migrées)
- [ ] **Variables partagées** (`shared_variables`, scopes team/project/environment/server)
- [ ] **Cleanup Docker automatisé** (§3.7) avec protection des artifacts de rollback (INV-015)
- [ ] **Déploiement coordonné d'environnement** + rollback auto sur santé dégradée (ADR-016)

---

## P3 — Périphérie

- [ ] Multi-serveurs HA d'une même app (§3.3) — registry externe obligatoire
- [ ] Cloudflare tunnels ; provisioning cloud (Hetzner) ; server patching
- [x] Rotation de clé maître forcée (`GET /system/encryption`, `POST /system/encryption/rotate`) : réécriture par lots, sérialisée par `lock_key`, convergence prouvée E2E (v1 → v2, histogramme à zéro sur l'ancienne version)
- [ ] Serveur MCP (§16)

---

## Dette technique et durcissement

### Sécurité
- [x] **Épinglage du host key SSH** (§20.1) : trust-on-first-use, empreinte épinglée à l'onboarding et comparée en temps constant à chaque connexion. Le paramètre de `sshexec.Dial` est **obligatoire** (le compilateur force chaque appelant à décider) ; une clé changée lève `ErrHostKeyChanged`, n'est **jamais** ré-épinglée automatiquement, et l'échec le dit explicitement
- [x] **Email de contact ACME explicite** (`AKERDOCK_ACME_EMAIL` → `instance_settings.acme_email`, base autoritaire ensuite). Il était **deviné** (`admin@example.com` en dernier recours — une adresse que Let's Encrypt refuse) : le proxy démarrait, tout paraissait sain, et les certificats n'arrivaient jamais. Sans adresse, l'onboarding du serveur **échoue maintenant tout de suite**, là où l'opérateur regarde. Ajouté au compose de référence et à `.env.example`
- [x] **Sessions navigateur** (§698, hors périmètre du contrat v1 — `/auth/*` vit à côté d'`/api/v1`, comme les webhooks) : cookie de session **HttpOnly** (la page ne peut pas lire le secret avec lequel elle s'authentifie — une XSS ne repart plus avec un token API) + **double-submit CSRF** (un cookie part sur les requêtes qu'un *autre* site déclenche : il prouve quel navigateur appelle, jamais que l'utilisateur a voulu appeler ; les appels Bearer en sont exemptés — il n'y a aucun credential ambiant à forger). Login **à temps constant** (le hash factice est un vrai Argon2id : sans cela, un email inconnu répond plus vite et les comptes deviennent énumérables) et message identique pour un email inconnu et un mot de passe faux. Le logout **révoque côté serveur** ; le verrouillage de compte borne le *brute force* en ligne. Bug trouvé au passage : le bootstrap créait un utilisateur root **sans équipe** — un compte qui pouvait s'authentifier puis s'entendre dire qu'il n'appartient nulle part
- [x] **Passkeys WebAuthn** (durcissement de §698) : enrôlement et login *usernameless* (`/auth/passkeys/*`, `/auth/passkey/login/*`, hors contrat v1 comme le reste de `/auth`). Le relying party est **épinglé au FQDN de l'instance**, jamais dérivé du header Host — un RP dérivé laisserait quiconque fait répondre le serveur sous un autre nom frapper des credentials pour lui (repli `localhost` seul en dev, la seule origine que les navigateurs traitent comme sûre en HTTP). `ResidentKey` **et** `UserVerification` exigés : un passkey remplace le mot de passe, pas seulement la présence. Cérémonies à usage unique (`DELETE … RETURNING`, hash du token seul en base, TTL 5 min) prouvées contre un vrai PostgreSQL ; compteur de signatures persisté **avant** l'ouverture de session et un compteur qui recule (clone) refuse le login, bruyamment. La vérification crypto est déléguée à `go-webauthn` — du parsing CBOR maison serait exactement l'inverse du but
- [x] **En-têtes de sécurité sur tout le port** (`httpserver.SecurityHeaders`) : CSP (`script-src 'self'`, `frame-ancestors 'none'`, styles inline tolérés — Angular injecte les styles de composants ainsi ; les *scripts* inline restent interdits, c'est là que la valeur est concentrée), nosniff, Referrer-Policy, COOP/CORP ; HSTS **seulement** si FQDN — le promettre sur une instance HTTP enfermerait l'opérateur dehors pour la durée du max-age. Rendu de l'UI vérifié sous cette CSP dans un vrai Chrome headless
- [x] **Rate limit par IP sur `/auth`** (30/min) : le commentaire de `Login` promettait un limiteur qui n'existait pas — `/auth/login` et les endpoints passkey sont les seuls à transformer une devinette en réponse sans credential. Clé = `RemoteAddr`, jamais `X-Forwarded-For` (une clé que le client écrit se contourne en tournant une chaîne) ; une adresse imparsable partage un seau au lieu d'être exemptée
- [ ] MFA TOTP (`mfa_factors` déjà migrée)
- [ ] Validation des `custom_docker_options` (INV-012) quand le champ sera exposé

### Moteur de déploiement
- [x] **Reprise après crash par état** (§2.5/§4) : jusqu'à `healthchecking`, rejeu (chaque effet distant est idempotent ou détruit-puis-refait). Pendant `switching`, **inspection distante d'abord** — ce qui existe sur le serveur décide : candidat déjà promu ⇒ finir ; ancien disparu ⇒ reprendre au rename ; les deux vivants ⇒ reprendre à l'arrêt de l'ancien ; rien d'utilisable ⇒ échec **bruyant**, jamais une seconde bascule à l'aveugle (INV-004/005)
- [x] **Crash ≠ échec** (bug trouvé par ce test) : `max_attempts=1` rendait terminal *tout* déploiement dont le worker mourait. Le reaper **rend la tentative** (le job survit au crash, INV-013), borné par `resume_count` pour qu'un *poison pill* finisse quand même en dead-letter
- [x] **Compensation réparée** (bug trouvé par ce test) : `execute()` fermait la connexion SSH avant que la compensation ne tente son `docker rm` du candidat — elle n'a **jamais** rien nettoyé jusqu'ici
- [x] Alerte d'expiration : un certificat à J-5 franchit J-30 **et** J-7 — il est annoncé **une seule fois**, au seuil le plus proche
- [x] **Valeurs multilignes** (clés PEM, blobs JSON) : les valeurs sont sourcées depuis un fichier shell (littéraux entre quotes simples) et passées à Docker **par nom** (`-e KEY`) — elles arrivent intactes et restent hors d'`argv` (INV-003). Le quoting est testé contre un vrai shell, pas relu à l'œil
- [x] **Commandes pre/post-déploiement** (§10, amendement de spec n°13 — les colonnes existaient, le contrat ne les exposait pas) : le `pre` s'exécute dans le container **existant** avant tout build (échec ⇒ rien n'est muté) ; le `post` dans le **candidat** une fois sain, **avant** la bascule (échec ⇒ candidat supprimé, l'ancien reste routé, INV-005, sans rollback auto). Commande **quotée** et non assainie (c'est du shell voulu), timeout 10 min, jamais de valeur en argv
- [x] **Asymétrie §10 tranchée** : la garantie du `post` suppose un candidat, donc un health check (§7.3). La combinaison « post-hook sans sonde » est **refusée** (`422`, à la création **et** au PATCH — y compris retirer la sonde sous un hook existant) plutôt que dégradée en silence : une garantie de sûreté fausse est pire que pas de garantie
- [x] **`remnants` distants + `acknowledge_remnants`** (§20.6.4) : le code annonçait « remnants recorded » et **n'enregistrait rien**. Une suppression qui échoue inventorie désormais ce qui subsiste réellement (containers, volumes, fichiers — par label) ; le `forget` d'un tel job est refusé en `409 remnants_present` **avec la liste**, exige un acquittement explicite (journalisé), et **ne supprime rien** à distance. Prouvé E2E avec un vrai échec de suppression, puis retry réussi
- [x] `resource_count` réel sur les environnements (compté en une requête par listing, jamais en fan-out)

### Observabilité
- [x] **Métriques et traces OTLP** (ADR-008) : une span par job et par requête HTTP (nommée par *pattern de route*, pas par chemin brut — sinon un nom de span par UUID) ; métriques jobs/déploiements. Configuration par les variables **`OTEL_*` standard** (seule exception assumée au préfixe `AKERDOCK_*` — §2.4). Sans endpoint : providers no-op, aucun export tenté, aucun warning. Une télémétrie cassée n'échoue jamais un déploiement
- [x] **Exposition Prometheus** (`/metrics`, opt-in via `AKERDOCK_METRICS_ENABLED`) : OTLP en push et Prometheus en scrape sont **deux readers sur un seul MeterProvider** — les instruments sont déclarés une fois (ADR-008), seule la sortie diffère
- [x] **`diff_redacted`** : l'audit dit *ce qui* a changé (avant/après). Un champ sensible est signalé comme modifié, **jamais avec sa valeur** — la table d'audit est append-only, conservée et exportable : un secret écrit dedans en serait une seconde copie. Liste de champs explicite (pas de réflexion sur la structure) et redaction testée unitairement

### Tests
- [x] **Tests d'intégration de la queue** (6, contre un vrai PostgreSQL — ses garanties sont des propriétés du SQL, un mock aurait testé le mock) : un job servi à **un seul** worker sur 8 concurrents, exclusion par `lock_key`, bail expiré récupéré (via `retry_wait` + promotion, donc avec backoff), idempotence, refus d'une queue non consommée, backoff croissant et borné. Skip propre sans base ; PostgreSQL branché en CI
- [x] **Matrice RBAC systématique** : le test est **généré depuis le contrat** (`x-required-permission`), pas depuis une liste tenue à la main — un endpoint ajouté est couvert dès qu'il existe. Les 102 opérations sont vérifiées : `401` sans token, `403` pour un token en lecture seule sur toute opération d'écriture, jamais de refus pour `root`. Efficacité confirmée par mutation (câbler `createProject` en `read` fait échouer le test)
- [x] **Fixtures de conformité proxy** (§9, ADR-009) : 4 cas (`tests/proxy-conformance/`) — une IR en entrée, la sortie Traefik attendue **octet à octet**. Elles figent les invariants qui comptent : priorité du chemin le plus spécifique (§3.1), endpoint = IP du candidat pendant une bascule (§7.2), pas de middleware de redirection sans `force_https`. Le **déterminisme** est vérifié à chaque run — le proxy n'applique une config que si son checksum change (§6.2), donc un générateur non déterministe réécrirait le fichier distant en boucle et anéantirait la détection de dérive. Caddy (P2) sera tenu aux **mêmes** cas ; leur absence est signalée, jamais tue
- [x] **Rolling upgrade N-1/N** (§18.2, ADR-021), en deux garde-fous : (1) `go test ./db` refuse mécaniquement toute migration non *expand-only* — `DROP TABLE/COLUMN`, `ALTER … TYPE`, `RENAME`, colonne `NOT NULL` sans défaut (elle ferait échouer tous les INSERT du binaire encore en vie) ; efficacité confirmée par mutation. (2) E2E : le binaire du **commit précédent** est reconstruit et doit **lire** les données écrites par la version N contre le schéma migré

### Distribution
- [x] **`Dockerfile` distroless** (binaire statique, `nonroot`, ~26 Mo — ni shell ni gestionnaire de paquets) et **`docker-compose.yml` de référence** conforme à instance-config §4.1, avec `.env.example`
- [x] **Smoke test de la distribution** (`make dist-smoke`, job CI dédié) : l'image se construit, n'a pas de shell, tourne non-root ; un seul `docker compose up -d` installe ; PostgreSQL n'est pas publié ; recréer le conteneur ne rejoue pas le bootstrap
- [x] **Workflow de release** : sur tag `v*`, mêmes garde-fous que la CI, image multi-arch (amd64/arm64, vrai cross-compile — pas d'émulation), publiée sur ghcr.io avec attestation de provenance signée. **Pas de tag `latest`** : un tag qui bouge est un tag qui met à jour une instance dans le dos de l'opérateur
- [x] **Client TypeScript généré depuis l'OpenAPI** (`web/`) : `schema.ts` généré par `make generate` (jamais édité), plus un client typé qui **encode les règles que les types ne disent pas** — `Idempotency-Key` posé d'office sur les POST (un retry sans clé, c'est un second déploiement), `If-Match` obligatoire par signature sur les PATCH sensibles (INV-014), erreurs structurées exposées (`version_conflict`, `remnants_present`). La CI type-check et **refuse toute dérive contrat ↔ client** (confirmé par mutation), comme pour le serveur Go
- [x] **UI Angular — premier slice** : workspace Angular 20 (standalone, OnPush, signals, TS strict), **tokens copiés verbatim depuis la spec** (§2.6 — seul endroit où une couleur littérale est permise), `akd-status-badge` (la pièce centrale : un état = une représentation partout, **jamais la couleur seule** — point + forme + libellé, WCAG 1.4.1), écran de connexion **email/mot de passe** sur session cookie, liste des applications avec états désiré/observé et déclenchement de déploiement. **Embarquée dans le binaire Go** (`go:embed`) et servie depuis le **port unique** (ADR-021) : deep links → `index.html`, API sur `/api/v1`. Table état→famille **testée exhaustive contre le contrat OpenAPI** (mutation vérifiée)
- [x] **UI Angular — couverture complète du contrat** : le client TypeScript couvre les **124 opérations** (méthodes nommées par `operationId`, `If-Match` obligatoire *par signature* sur les 9 PATCH sensibles, `Idempotency-Key` d'office sur les POST) et le dashboard expose chaque capacité derrière un shell de navigation unique : projets + environnements, applications (variables, volumes, tâches planifiées + exécutions, déploiements + cancel + rollback, webhook Git à secret one-time, cycle de vie, suppression avec `delete_volumes`), bases PostgreSQL (cycle de vie, plans de backup, exécutions, restore avec garde-fou, drills), serveurs (validation, ressources, domaines, certificats + renew, CA), clés privées (reveal explicite), registries/DNS/S3 (+ validation réelle), canaux de notification + règles + test, équipe (membres, invitations et tokens à secret one-time), système (email transactionnel, rotation de la clé maître, API on/off — la copie dit vrai : couper l'API coupe aussi le dashboard), jobs (retry, forget avec acquittement des remnants listés), flux SSE live, page Sécurité (passkeys), sign-in mot de passe **et passkey**. Pages en signals/OnPush, vocabulaire CSS partagé (`.akd-*`, tokens seuls), états loading/vide/erreur partout, secrets one-time affichés une fois dans un cadre qui le dit
- [ ] **UI Angular — reste** : Storybook (exigence bloquante §5.2) ; i18n (aucune chaîne en dur) ; a11y automatisée (axe) ; timeline de déploiement enrichie

---

## Performance de la suite E2E

**910 s → 616 s** (40 vérifications), sans rien retirer de la couverture. La démarche : **mesurer d'abord** (chronomètre par section dans `scripts/e2e.sh`), ce qui a invalidé l'hypothèse intuitive — le temps ne partait pas dans les pulls d'images mais dans des **attentes**.

- **Tick du scheduler configurable** (`AKERDOCK_SCHEDULER_TICK`, défaut 30 s → 2 s en E2E). Il pilote backups cron, notifications, alertes de certificats et digests : chaque assertion attendait le tick suivant. Réglage légitime, pas un artifice de test.
- **Backoff de retry configurable** (`AKERDOCK_RETRY_BASE`, défaut 5 s → 1 s en E2E) : les tests qui échouent un job *volontairement* attendaient 5 s, 10 s, 20 s…
- **Cache d'images du DinD** (volume nommé) : il ne retélécharge plus nginx/postgres/traefik/minio/nixpacks à chaque run. Attention — le volume persiste **tout** le docker root : containers, réseaux et volumes sont purgés au démarrage, seules les images sont conservées. `E2E_FRESH=1` repart de zéro.

Plafonds restants **assumés** : les fenêtres d'une minute du cron de backup et du digest sont les granularités minimales du produit ; les compresser reviendrait à tester autre chose que ce qui tourne en production.

## Amendements de spec effectués

Douze incohérences trouvées entre les artefacts pendant l'implémentation, corrigées à la source :

1. Fichiers ADR-018/ADR-022 non renommés (liens cassés de l'index)
2. OpenAPI en 3.1 alors qu'oapi-codegen (ADR-025) ne supporte que 3.0 → converti
3. `teams.personal` exposé par l'OpenAPI, absent du data dictionary
4. Clé SSH d'instance vs `private_keys.team_id NOT NULL` → `is_instance` + CHECK
5. `jobs.steps`/`result`/`retry_of_id` exposés par l'OpenAPI, absents du dictionnaire
6. `deployments.error_message` idem
7. `deployment-engine` prescrivait des images `AkerDock/<uuid>` — Docker refuse les majuscules
8. Aucun endpoint pour les volumes persistants alors que `delete_volumes` existait → endpoints ajoutés
9. ADR-024 exige SSE pour les statuts mais aucun endpoint d'événements → `GET /events` ajouté
10. Le contrat plaçait le CRUD des S3 Storages « hors v1, provisionné via l'UI » alors que les plans de backup exigent un `s3_storage_uuid` et que l'UI n'existe pas → 5 opérations `/s3-storages` ajoutées
11. Les notifications étaient « hors v1 » alors qu'ADR-019 les exige et que rien ne permettait de les configurer → 7 opérations `/notification-channels` ajoutées ; table `notification_deliveries` ajoutée au dictionnaire (le débounce et l'idempotence ne sont dérivables d'aucune des deux tables prévues)
12. Les webhooks Git signés étaient spécifiés (`git-webhook-protocols.md`, INV-009) mais aucun endpoint ne permettait de créer l'endpoint de réception → `POST/DELETE /applications/{uuid}/webhook-endpoint` ajoutés (le secret n'est renvoyé qu'une fois)
