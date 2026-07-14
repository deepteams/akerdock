# Inventaire licences et SBOM — AkerDock

> Artefact §29.11 du PRD (`docs/PRD.md`). Couvre la licence du projet (ADR-020, §27.20), la politique de licences des dépendances de la stack retenue (§27.25, ADR-025), les images helper et runtime orchestrées (§6.1, §7, §16.2), le catalogue de templates (§9, §27.10, ADR-010), les exigences SBOM/signature/scan du §23.5 et la distribution compose (§27.21, ADR-021). Le PRD et les ADR sont la source de vérité ; ce document en dérive les règles opérationnelles.

Conventions : les identifiants de licences utilisent la nomenclature SPDX (`MIT`, `Apache-2.0`, `BSD-3-Clause`…). Les décisions non encore actées par ADR sont marquées **« (défaut proposé) »**. Les licences non vérifiées à la source (fichier LICENSE du projet upstream à la version épinglée) sont marquées **« (à vérifier) »** — aucune affirmation de licence douteuse n'est faite sans ce marqueur.

---

## 1. Licence du projet

**Apache-2.0** (ADR-020) : adoption maximale, clause brevets explicite, et alignement avec la licence dominante des templates compose du domaine — ce qui rend leur import propre (§27.10). Le risque « fork cloud » est accepté ; il n'est pas traité par la licence.

### 1.1 Fichiers requis à la racine du dépôt

| Fichier | Contenu | Obligation |
|---|---|---|
| `LICENSE` | Texte intégral Apache License 2.0, non modifié | Obligatoire avant toute publication de code (ADR-020) |
| `NOTICE` | Nom du projet, ligne de copyright, attributions requises (cf. §1.3) | Obligatoire dès qu'une dépendance Apache-2.0 avec son propre `NOTICE` est embarquée ; créé dès le départ |
| `THIRD-PARTY-NOTICES` (ou `licenses/` généré) | Concaténation des licences des dépendances embarquées dans le binaire et l'UI, générée automatiquement (`go-licenses save` + équivalent npm) | Généré à chaque release, joint aux artefacts ; **(défaut proposé)** exposé aussi via `AkerDock licenses` en CLI et une page « Licences » dans l'UI |

### 1.2 En-têtes de fichiers

**Politique : pas d'en-tête de licence par fichier ; le `LICENSE` racine fait foi pour tout le dépôt. (défaut proposé)**

- Rationale : Apache-2.0 n'exige pas d'en-tête par fichier (l'appendice du texte de licence est une recommandation, pas une condition) ; les en-têtes créent du bruit de diff et sont systématiquement oubliés.
- Exception : un fichier **copié ou dérivé d'un projet tiers** conserve obligatoirement l'en-tête de copyright d'origine et une mention de provenance (URL + commit), et son projet source est ajouté au `NOTICE`.
- Si la politique change (ex. exigence d'un partenaire), l'ajout d'en-têtes est automatisable (`addlicense`) et fera l'objet d'une révision de ce document.

### 1.3 Gestion du copyright

- Ligne de copyright unique : `Copyright <année de première publication>-<année courante> The AkerDock Authors` — **(défaut proposé)** le modèle « The X Authors » (pratique Go/Kubernetes) évite de maintenir une liste nominative et reste correct avec le DCO (§7) : chaque contributeur conserve son copyright, la licence Apache-2.0 porte la concession de droits.
- Le `NOTICE` contient : cette ligne de copyright, la mention « This product includes software developed by third parties » et les attributions héritées des `NOTICE` de dépendances Apache-2.0 (collectées automatiquement, cf. §2.3).
- Pas de cession de copyright demandée aux contributeurs (pas de CLA, cf. §7).

---

## 2. Politique de licences des dépendances

Contexte : AkerDock distribue un **binaire Go liant statiquement toutes ses dépendances** (ADR-021 : binaire statique dans une image distroless) et des **assets UI bundlés** (§25.2 : Angular compilé embarqué dans le binaire). Toute dépendance Go du graphe de compilation et tout paquet npm présent dans le bundle final sont donc **redistribués** sous Apache-2.0 : leur licence doit le permettre.

### 2.1 Matrice autorisé / à examiner / interdit

| Statut | Licences (SPDX) | Justification |
|---|---|---|
| **Autorisées** | `MIT`, `BSD-2-Clause`, `BSD-3-Clause`, `Apache-2.0`, `ISC`, `MPL-2.0`, `Unlicense`, `0BSD`, `Zlib`, `PostgreSQL`, `BlueOak-1.0.0` | Permissives, compatibles avec une redistribution sous Apache-2.0. MPL-2.0 : copyleft **fichier par fichier** — autorisée tant que les fichiers MPL ne sont pas modifiés ; toute modification d'un fichier MPL doit être republiée sous MPL (contrainte tracée dans le fichier d'exceptions, §2.4) |
| **À examiner (cas par cas)** | `LGPL-2.1`, `LGPL-3.0`, `CDDL-1.0`, `EPL-2.0`, licences custom / non-SPDX | LGPL : la **liaison statique Go** rend l'exigence de « relinkabilité » (LGPL §4) difficile à satisfaire — un module Go LGPL lié dans le binaire est de facto problématique ; acceptable uniquement en outil externe exécuté (pas linké) ou avec analyse juridique. Licences custom : lecture intégrale obligatoire |
| **Interdites (en dépendance liée)** | `GPL-2.0`, `GPL-3.0`, `AGPL-3.0`, `SSPL-1.0`, `BUSL-1.1`, `Elastic-2.0`, `CC-BY-NC-*`, toute licence « non-commercial » ou « source available » | Copyleft fort : lier du GPL/AGPL dans le binaire imposerait de relicencier AkerDock ; SSPL/BUSL/Elv2 ne sont pas open source et sont incompatibles avec une redistribution Apache-2.0. **Interdit = interdit dans le graphe de liaison du binaire et dans le bundle UI** — pas dans les images tierces orchestrées (§4) ni les templates (§5), qui ne sont pas des dépendances liées |
| **Interdites (partout)** | Dépendances sans licence identifiable, « WTFPL »-like douteuses, licences propriétaires sans droit de redistribution | Risque juridique non évaluable |

Cas particuliers :

- **Outils de build et de génération de code** (sqlc, oapi-codegen en mode générateur, Angular CLI, syft…) : leur licence ne contamine pas le binaire — seul le **code généré** (qui nous appartient) et les éventuels **paquets runtime** importés comptent. Un outil GPL utilisé en build serait acceptable (à documenter), mais aucun n'est prévu.
- **Outils de test** (Pebble, images Gitea/MinIO en E2E…) : jamais redistribués, licence libre de contrainte pour nous ; inventoriés quand même (§3) pour éviter une bascule accidentelle vers une dépendance liée.
- **Logiciels orchestrés** (Docker, Traefik, Nixpacks…) : cf. §3 et §4 — non-objectif explicite de les réimplémenter ou de les embarquer (§16.2), donc jamais liés ni redistribués.

### 2.2 Vérification automatisée en CI

- **Go** : `go-licenses check ./... --allowed_licenses=<liste §2.1>` (ou équivalent : `golicense`, `licensei`) sur chaque PR ; échec bloquant si une licence hors liste apparaît dans le graphe de compilation. `go-licenses report`/`save` génère le `THIRD-PARTY-NOTICES` à la release. **(défaut proposé : go-licenses, outil Google Apache-2.0)**
- **npm / UI Angular** : vérification équivalente sur les dépendances de production du bundle (`license-checker` ou lockfile-lint + revue des `licenses` du lockfile), même liste d'autorisation ; les devDependencies (build only) sont exclues du contrôle bloquant mais inventoriées.
- Le contrôle tourne : sur chaque PR touchant `go.mod`/`go.sum`/lockfile npm, sur chaque release, et en job planifié hebdomadaire (détection des **changements de licence upstream** entre versions — cas Redis, MinIO, Elastic : une dépendance permissive peut devenir non libre à la version suivante).
- Toute montée de version majeure d'une dépendance directe passe par une PR où le diff de licence est vérifié.

### 2.3 Processus d'exception

1. Ouvrir une issue « license-exception » : dépendance, version, licence, usage exact (liée / outil / test), alternatives évaluées.
2. Décision par les mainteneurs ; si structurante (ex. première LGPL), un ADR est requis.
3. L'exception acceptée est consignée dans un fichier versionné (`.licenses/exceptions.yaml` — **défaut proposé**) lu par le job CI : chaque entrée porte dépendance, version(s) couverte(s), justification, date, et échéance de réexamen.
4. Une exception sans échéance est interdite ; le job hebdomadaire alerte sur les exceptions expirées.

---

## 3. Dépendances directes prévues et leurs licences

Inventaire de la stack connue (§27.25 / ADR-025, §25.2, §16.2). Colonne **Nature** : `linké` = compilé dans le binaire ou bundlé dans l'UI (redistribué → matrice §2.1 applicable) ; `image orchestrée` = logiciel tiers que AkerDock pilote via `docker pull`/`run` chez l'utilisateur (jamais redistribué par nous, cf. §4) ; `outil de build` = utilisé en CI/génération, absent des artefacts ; `outil de test` = utilisé en E2E/intégration uniquement.

| Composant | Licence | Nature | Remarques |
|---|---|---|---|
| Go (stdlib + toolchain) | BSD-3-Clause | linké (stdlib) / outil de build (toolchain) | |
| `golang.org/x/*` | BSD-3-Clause | linké | Quasi certain d'apparaître dans le graphe (crypto/ssh notamment) |
| pgx (`jackc/pgx`) | MIT | linké | Driver PostgreSQL (ADR-025) |
| sqlc | MIT (à vérifier) | **outil de build** | Génère du code Go qui nous appartient ; non linké |
| chi (`go-chi/chi`) | MIT | linké | Router HTTP |
| oapi-codegen | Apache-2.0 (à vérifier) | outil de build + **paquet runtime linké** | Le générateur est un outil ; les petits paquets runtime (`oapi-codegen/runtime`) sont linkés — même licence à confirmer sur les deux modules |
| Bibliothèque SSH (x/crypto/ssh attendu) | BSD-3-Clause | linké | Transport ADR-001 |
| Client OpenTelemetry Go (`go.opentelemetry.io/otel`) | Apache-2.0 | linké | ADR-008 (OTLP partout) |
| Bibliothèque WebSocket (terminal, ADR-024 ; choix non arrêté : `coder/websocket` ou équivalent) | MIT / ISC (à vérifier selon le choix) | linké | À figer à l'implémentation |
| Bibliothèque ACME/DNS-01 si linkée (lego) | MIT (à vérifier) | linké (si retenu) | L'ACME HTTP-01 de parité est porté par Traefik (orchestré) ; lego ne serait linké que pour du DNS-01 côté control plane — décision d'implémentation |
| Angular (framework + CLI) | MIT | linké (runtime bundlé) / outil de build (CLI) | §25.2 : assets compilés embarqués dans le binaire |
| xterm.js | MIT | linké (bundlé UI) | Terminal web §5.7 |
| Docker Engine / Compose / BuildKit (Moby) | Apache-2.0 | image/logiciel **orchestré** côté serveur cible | Non-objectif de le réimplémenter ou l'embarquer (§16.2) ; installé chez l'utilisateur via script/paquets Docker, jamais redistribué par nous |
| Traefik | MIT | image orchestrée | Proxy par défaut §4.1 |
| Caddy | Apache-2.0 | image orchestrée | P2, ADR-009 |
| Nixpacks | MIT | outil **orchestré** sur le serveur de build | AkerDock l'invoque, ne le réimplémente pas (§16.2) ; il produit un plan/Dockerfile — l'image applicative résultante appartient à l'utilisateur |
| Railpack | MIT (à vérifier) | outil orchestré sur le serveur de build | Successeur de Nixpacks (§5.2), licence à confirmer à la version épinglée |
| restic | BSD-2-Clause | image/outil orchestré (backups volumes, §20.5/ADR-014) | Vérifier si utilisé via image helper (cf. §4) ou binaire installé |
| MinIO `mc` (client S3, parité §7.2) | AGPL-3.0 | image/outil orchestré | **Point d'attention majeur** : cf. §4.2 — jamais linké, et de préférence jamais rebundlé dans une image que nous publions |
| PostgreSQL (base interne de l'instance, ADR-021) | PostgreSQL License | image orchestrée | Image officielle pullée par le compose de distribution ; nous ne la redistribuons pas |
| Pebble (`letsencrypt/pebble`) | MPL-2.0 (à vérifier) | outil de test | Serveur ACME de test (E2E TLS) |
| Gitea | MIT | outil de test | Provider Git en E2E (ADR-026) |
| MinIO (serveur) | AGPL-3.0 | outil de test | Cible S3 en E2E uniquement ; jamais distribué |
| go-licenses, syft, grype, trivy, cosign | Apache-2.0 (à vérifier individuellement) | outils de build/CI | Chaîne licences/SBOM/signature elle-même |

Règles :

- Ce tableau est un **instantané de conception** ; la vérité opérationnelle est le rapport `go-licenses` + SBOM généré à chaque release (§6). Toute dépendance directe ajoutée à `go.mod` ou au `package.json` de prod doit être conforme à la matrice §2.1 — le CI l'impose.
- Les licences « (à vérifier) » sont confirmées **à la version épinglée** au moment du premier `go get`/`npm install`, pas depuis le README upstream.
- Un composant ne change jamais silencieusement de colonne « Nature » : passer un outil orchestré (ex. restic) en bibliothèque linkée est une décision explicite soumise à la matrice §2.1.

---

## 4. Images helper et runtime déployées chez l'utilisateur

### 4.1 Le point clé : orchestrer n'est pas redistribuer

AkerDock ne **redistribue pas** les images tierces : il ordonne un `docker pull` **depuis les registries upstream, directement sur le serveur de l'utilisateur**. Conséquences :

- **Aucune obligation de redistribution** (attribution, fourniture des sources, NOTICE) ne pèse sur le projet pour ces images : c'est l'utilisateur qui obtient le logiciel de son éditeur, AkerDock n'est qu'un installateur/orchestrateur — même position qu'un gestionnaire de paquets.
- Les licences copyleft réseau (AGPL du serveur MinIO, par exemple) s'appliquent à **l'utilisateur qui exploite le service**, pas à AkerDock ; pour un usage non modifié d'images officielles, l'AGPL n'impose rien de plus que la disponibilité des sources upstream.
- En revanche, **dès que nous publions une image sous notre namespace** (registry du projet), nous devenons redistributeur : SBOM, respect des licences de tout le contenu de l'image, et pour de l'AGPL/GPL, obligation de source. D'où la règle §4.3.
- Obligation résiduelle de notre côté : **information de l'utilisateur** (licence affichée avant déploiement, §5) et **épinglage** d'images officielles non modifiées.

### 4.2 Inventaire des images orchestrées

| Image | Rôle | Licence du logiciel | Publiée par nous ? | Implication |
|---|---|---|---|---|
| `traefik` | Proxy par serveur (§4.1 PRD) | MIT | Non — upstream | Aucune obligation ; épingler tag + digest |
| `caddy` | Proxy alternatif P2 | Apache-2.0 | Non — upstream | Idem |
| `postgres` | Base interne de l'instance (ADR-021) + moteur managé §6.1 | PostgreSQL License | Non — upstream | Idem |
| `mysql` | Moteur managé §6.1 | GPL-2.0 | Non — upstream | OK en orchestration ; ne jamais rebundler ni linker de client GPL |
| `mariadb` | Moteur managé §6.1 | GPL-2.0 | Non — upstream | Idem |
| `mongo` | Moteur managé §6.1 | **SSPL-1.0** (non open source) | Non — upstream | OK en orchestration (usage utilisateur) ; licence affichée à l'utilisateur ; ne jamais redistribuer |
| `redis` | Moteur managé §6.1 | ≤ 7.2 : BSD-3-Clause ; ≥ 7.4 : tri-licence RSALv2 / SSPLv1 / AGPL-3.0 (AGPL ajoutée avec Redis 8) (à vérifier au tag épinglé) | Non — upstream | Choisir et documenter le tag par défaut en connaissance de cause ; alternative BSD : `valkey` (BSD-3-Clause) — décision produit à tracer |
| `eqalpha/keydb` | Moteur managé §6.1 | BSD-3-Clause (à vérifier) | Non — upstream | Projet peu actif — surveiller |
| `dragonflydb/dragonfly` | Moteur managé §6.1 | **BUSL-1.1** (non open source) | Non — upstream | OK en orchestration ; licence affichée ; usage « non concurrent » — c'est l'utilisateur que ça engage |
| `clickhouse/clickhouse-server` | Moteur managé §6.1 | Apache-2.0 | Non — upstream | Aucune obligation |
| `minio/mc` | Upload S3 des backups (parité §7.2) | **AGPL-3.0** | À décider | Si pullée upstream : OK. **Ne jamais copier `mc` dans une image publiée par nous** sans assumer les obligations AGPL. **(défaut proposé)** : remplacer `mc` par un client S3 permissif — SDK Go AWS (Apache-2.0) linké côté worker, ou `rclone` (MIT) — et réserver `mc` à la stricte parité si nécessaire |
| `restic/restic` | Backups volumes (ADR-014) | BSD-2-Clause | À décider | Permissif : rebundle possible sans contrainte forte si une image helper s'avère nécessaire |
| Image(s) helper AkerDock (cleanup §3.7, exécuteurs de backup/restore §7, proxy TCP dynamique §6.2 — inventaire exact à figer avec `deployment-engine.md`) | Outillage plateforme sur le serveur cible | Apache-2.0 (notre code) + contenu de base | **Oui** — namespace du projet | **Redistribution assumée** : base distroless/alpine documentée, SBOM par image, scan CVE, signature cosign, `THIRD-PARTY-NOTICES` inclus — mêmes exigences que l'image AkerDock (§6) |
| Image Sentinel/agent (si conteneurisé, §3.8) | Agent métriques | Apache-2.0 (notre code) | **Oui** | Idem |
| `nginx` | Build pack static (§5.2) | BSD-2-Clause | Non — upstream | Aucune obligation |

### 4.3 Règles

1. **Par défaut, aucune image tierce n'est republiée sous le namespace du projet.** Un miroir (pour résilience/rate-limit Docker Hub) est une décision explicite qui fait de nous un redistributeur : elle exige la revue des obligations de la licence concernée (trivial pour MIT/Apache, engageant pour AGPL/SSPL — et pour SSPL/BUSL, probablement interdit ou inutilement risqué).
2. Toute image **publiée par le projet** (AkerDock, helpers, agent) suit le pipeline complet du §6 : SBOM, scan, signature, notices.
3. Les images par défaut des moteurs managés (§6.1) sont épinglées **tag + digest** dans le code/catalogue ; l'utilisateur peut les changer (champ image/tag libre, §6.2 PRD) — sa responsabilité est alors affichée.
4. Les logiciels sous licence non libre orchestrés par défaut (MongoDB SSPL, Dragonfly BUSL, Redis ≥ 8) sont signalés dans l'UI au moment du choix du moteur, avec la même mécanique d'affichage de licence que les templates (§5.2).

---

## 5. Catalogue de templates (§27.10, ADR-010)

Les templates one-click référencent des images upstream aux licences très variées, y compris non libres : MinIO (AGPL-3.0), Elasticsearch (tri-licence AGPL-3.0 / SSPL-1.0 / Elastic-2.0 depuis 2024 — à vérifier à la version référencée), n8n (Sustainable Use License, fair-code non open source), Grafana (AGPL-3.0), MongoDB (SSPL), etc. Le critère d'admission de la référence (≥ 1000 stars) ne dit rien de la licence : la nôtre doit être explicite.

### 5.1 Licence des templates eux-mêmes

- Le **template** (fichier compose + métadonnées + éventuels scripts d'init) publié dans le dépôt de templates du projet est sous **Apache-2.0**, comme le reste du projet — c'est notre œuvre, ou une réécriture substantielle d'un template importé. Tout template dérivé d'un catalogue tiers conserve son **attribution** et n'est importé que si sa licence le permet (permissive) : l'inventaire ci-dessous en fait foi.
- La licence du template ne dit **rien** de la licence du logiciel qu'il déploie : les deux informations sont séparées partout (UI, métadonnées, docs).
- Les templates des **dépôts utilisateur** (ADR-010) restent sous la licence choisie par leur auteur ; AkerDock ne l'impose ni ne la vérifie — il valide la syntaxe, pas le droit.

### 5.2 Affichage de la licence du logiciel déployé (défaut proposé)

- Champ **`license`** obligatoire dans les métadonnées de template du dépôt officiel : identifiant SPDX ou expression (`AGPL-3.0-only`, `SSPL-1.0`, `Elastic-2.0 OR SSPL-1.0 OR AGPL-3.0`, `LicenseRef-n8n-Sustainable-Use`…), plus un champ optionnel `license_url` vers la licence upstream.
- L'UI affiche licence + lien **avant le déploiement** (écran de confirmation du one-click), avec un badge distinct pour les licences non-OSI (« source available », « fair-code ») — information, pas blocage : le choix appartient à l'utilisateur, qui exécute le logiciel chez lui.
- Le pipeline de validation du dépôt de templates (ADR-010) refuse un template officiel sans champ `license` ; pour les dépôts utilisateur, le champ est recommandé, absent = « licence inconnue » affiché.
- Un stack multi-images porte la licence de chaque composant significatif (au minimum l'image principale ; idéalement `license` par service).

### 5.3 Logos et marques

- Les logos du catalogue sont utilisés en **usage nominatif** (désigner le logiciel qu'un template déploie), ce qui est l'usage classique des catalogues one-click — mais chaque marque reste la propriété de son titulaire et certaines chartes (Elastic, MongoDB, Redis…) encadrent strictement cet usage.
- Guidelines internes : logo **non modifié** (pas de recoloration, déformation, détourage agressif), accompagné du nom du projet upstream et d'un **lien vers le site officiel** ; aucun logo dans un contexte laissant croire à une affiliation, un partenariat ou une certification ; fichier de provenance par logo (source, date, éventuelles brand guidelines upstream) dans le dépôt de templates.
- **Procédure de retrait sur demande** : contact public documenté dans le dépôt de templates (fichier `TRADEMARKS.md` — défaut proposé) ; retrait ou remplacement du logo sous 14 jours après demande vérifiée d'un titulaire de marque, sans contestation par défaut ; le template survit au retrait du logo (icône générique).
- Le nom « AkerDock » lui-même : à noter que « Docker » est une marque de Docker, Inc. — point à valider (risque de confusion nominale) avant communication publique ; hors périmètre de ce document mais signalé.

---

## 6. SBOM, signature et politique CVE

Concrétise §23.5 (« SAST, dependency/container scanning, SBOM et images signées pour les releases AkerDock ») et la chaîne de confiance du catalogue (ADR-010).

### 6.1 Génération des SBOM (par release, en CI)

| Artefact | Outil | Formats | Publication |
|---|---|---|---|
| Binaire Go (par OS/arch publié) | syft (source + binaire ; complété par `go version -m`) | **CycloneDX JSON + SPDX JSON** (les deux) | Attachés à la release (assets `akerdock-<ver>-sbom.cdx.json` / `.spdx.json`) |
| Bundle UI (dépendances npm de prod) | syft sur le lockfile/bundle | CycloneDX + SPDX | Fusionné ou joint au SBOM du binaire (l'UI est embarquée dedans) |
| Image `AkerDock` (et chaque image publiée : helpers, agent — §4.2) | syft sur l'image | CycloneDX + SPDX | Asset de release **et** attestation attachée à l'image (`cosign attest --type spdxjson`) |
| Catalogue de templates | Manifeste du catalogue (liste templates + versions + images référencées + licences §5.2) | JSON signé | Publié avec chaque version du catalogue |

**(défaut proposé : syft.** Alternative équivalente : trivy en mode SBOM ; l'important est le double format CycloneDX + SPDX et la reproductibilité en CI.)

### 6.2 Signature des releases et du catalogue (défaut proposé : cosign/Sigstore)

- **Images** publiées par le projet : signées avec **cosign en mode keyless** (OIDC du pipeline CI, certificats Fulcio, journal Rekor) — pas de clé longue durée à protéger ; l'identité signante est le workflow de release du dépôt officiel.
- **Binaires et archives de release** : `cosign sign-blob` keyless + checksums SHA-256 signés ; instructions de vérification documentées dans les release notes.
- **Catalogue de templates** (ADR-010) : le JSON compilé du catalogue est signé (`cosign sign-blob` — défaut proposé, cohérent avec le reste de la chaîne) ; l'instance AkerDock **vérifie la signature avant d'accepter un rafraîchissement** du catalogue officiel ; les dépôts utilisateur ne sont pas signés par le projet (risque accepté, ADR-010).
- Provenance : attestation SLSA/provenance du build attachée aux images **(défaut proposé, non bloquant pour la première release)**.
- Point d'attention keyless : lie la confiance à l'identité CI (GitHub Actions OIDC) — documenter l'identité attendue pour que la vérification soit effective ; une clé projet hors-CI reste l'alternative si le keyless est jugé trop couplé à la forge.

### 6.3 Scan de vulnérabilités et SLA de correction

- **CI** : scan **grype** (ou trivy — en choisir un comme bloquant, l'autre optionnel en second avis, défaut proposé : grype, cohérent avec syft) sur le binaire + chaque image publiée, à chaque PR touchant les dépendances et à chaque release ; SAST (`gosec`/CodeQL) et `govulncheck` (qui filtre par atteignabilité des symboles Go) en complément.
- **Post-release** : re-scan **planifié quotidien** des artefacts de la dernière release stable (les CVE apparaissent après la publication, pas seulement pendant).
- **SLA de correction proposés** (déclenchés à partir de la confirmation qu'une CVE affecte un artefact publié, `govulncheck`/analyse d'atteignabilité faisant foi pour le binaire Go) :

| Sévérité (CVSS) | Correctif ou mitigation publiée | Véhicule |
|---|---|---|
| Critique (9.0+) ou exploitée activement | **≤ 7 jours** | Release patch dédiée + advisory |
| Haute (7.0–8.9) | **≤ 30 jours** | Release patch |
| Moyenne (4.0–6.9) | **≤ 90 jours** | Prochaine release mineure/patch |
| Basse (< 4.0) | Au fil de l'eau | Prochaine release |

- Les CVE **non atteignables** (dépendance vulnérable mais code vulnérable non appelé) peuvent être déclassées avec justification versionnée (fichier `.grype.yaml`/VEX — chaque suppression porte CVE, justification, échéance de réexamen, comme les exceptions de licence §2.3).
- Les images de base (distroless) sont reconstruites/re-releasées si une CVE haute+ touche la base même sans changement du code Go.

---

## 7. Contributions : DCO (défaut proposé)

**DCO (Developer Certificate of Origin, `Signed-off-by` sur chaque commit) plutôt que CLA.**

- **Pour** : friction quasi nulle (un flag `-s`), standard des projets Linux/CNCF, suffisant juridiquement pour attester du droit de contribuer sous Apache-2.0 dont la clause 5 couvre déjà la concession de licence des contributions.
- **Contre** : contrairement à un CLA, pas de pouvoir de relicenciement futur du code contribué — cohérent avec ADR-020 qui acte que le choix Apache-2.0 n'est « pas rétroactivement réversible » ; risque accepté.
- Application : check DCO en CI (bot/action bloquante), documenté dans `CONTRIBUTING.md`.

---

## 8. Checklist de release

Tout doit être vrai avant de publier une release du binaire/image ou une version du catalogue :

**Licences**
- [ ] `go-licenses check` et le contrôle npm passent sans nouvelle exception non validée (§2.2) ; aucune exception expirée (§2.3).
- [ ] Diff de licences depuis la release précédente revu (aucune dépendance n'a changé de licence upstream).
- [ ] `LICENSE` intact ; `NOTICE` à jour (nouvelles attributions Apache-2.0, fichiers dérivés §1.2) ; `THIRD-PARTY-NOTICES` régénéré et embarqué dans binaire + images.

**SBOM et vulnérabilités**
- [ ] SBOM CycloneDX + SPDX générés pour binaire(s), bundle UI et chaque image publiée ; attachés à la release et attestés sur les images (§6.1).
- [ ] Scan grype/trivy + `govulncheck` + SAST passés ; aucune CVE critique/haute atteignable sans mitigation documentée ; suppressions VEX à jour (§6.3).

**Signature et intégrité**
- [ ] Toutes les images publiées signées (cosign) ; binaires/checksums signés ; identité de signature conforme à celle documentée (§6.2).
- [ ] Vérification de signature rejouée depuis un environnement vierge (la commande documentée fonctionne réellement).

**Catalogue de templates** (si release du catalogue)
- [ ] Pipeline de validation passé (lint compose, métadonnées, magic variables — ADR-010) ; champ `license` présent sur 100 % des templates officiels (§5.2).
- [ ] Images référencées épinglées ; licences non-OSI correctement badgées ; provenance des logos à jour, aucune demande de retrait en attente (§5.3).
- [ ] JSON du catalogue signé et vérification testée côté instance (§6.2).

**Divers**
- [ ] Commits de la release conformes DCO (§7).
- [ ] Images tierces par défaut (proxy, postgres, moteurs §6.1) toujours épinglées tag + digest, et re-vérifiées si le tag par défaut a changé (§4.3).
- [ ] Ce document mis à jour si une entrée d'inventaire (§3, §4.2) a changé de licence, de version majeure ou de « Nature ».
