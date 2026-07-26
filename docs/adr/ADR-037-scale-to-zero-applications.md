# ADR-037 — Scale-to-zero des applications de production

## Statut

Accepté — **étend** [ADR-036](ADR-036-scale-to-zero-waker.md) (waker en coupure)
aux applications de production, au-delà des previews. Ne change pas le mécanisme
du waker ; ajoute un opt-in et des garde-fous propres à la production.

## Contexte

Le scale-to-zero (ADR-036) a été livré **previews d'abord** — proxy-contract
§8.3 dit même « jamais implicite en production ». Le waker, lui, est **générique** :
il route par Host et réveille un ensemble de conteneurs, il ne sait rien de la
notion de preview. Beaucoup d'apps auto-hébergées (outils internes, side-projects,
back-offices peu sollicités) gagneraient au même « éteint quand inactif, réveillé
à la première requête » — c'est une demande directe.

La production n'a pourtant pas le même profil de risque qu'une preview :

- le **cold-start** est payé par un **vrai utilisateur** (jusqu'à 60 s → 504),
  pas par un développeur qui relit sa PR ;
- une app peut porter des **workers/crons** qu'un `docker stop` tuerait ;
- le **monitoring uptime** pingerait l'app en continu et la garderait éveillée ;
- « endormie » ne doit pas être confondu avec « plantée » par l'UI et les alertes.

## Décision

Le scale-to-zero est étendu aux applications, **en opt-in explicite et séparé**
des previews, avec des garde-fous.

### 1. Deux opt-ins distincts

`scale_to_zero` (+ `scale_to_zero_after_minutes`) sur `applications` gouverne
**l'application elle-même** ; l'ancien flag preview est **renommé**
`preview_scale_to_zero` (+ `preview_scale_to_zero_after_minutes`). On ne couple
pas les deux : endormir ses previews et endormir sa prod sont deux décisions de
risque différentes. « Jamais implicite en production » (§8.3) est respecté — c'est
un interrupteur que l'opérateur arme sciemment, jamais un défaut.

### 2. Périmètre et garde-fous

- **Workloads pilotés par requête uniquement.** L'UI avertit : une app qui fait
  tourner des workers, des consumers de queue ou des crans en tâche de fond n'est
  **pas** un bon candidat — le `docker stop` les arrête aussi.
- **Cold-start assumé.** L'UI affiche que la première requête après inactivité
  peut attendre le démarrage (jusqu'à 60 s). À réserver aux apps tolérant cette
  latence.
- **Bases managées exclues par construction.** Le flag n'existe que sur
  `applications`, pas sur les `databases` : on n'endort jamais une base standalone
  (connexions coupées, fenêtres de backup). Une app *compose* embarquant sa
  propre base reste le choix de l'opérateur (les volumes persistent).

### 3. État explicite, distinct d'une panne

`applications.scale_slept_at` (timestamptz, NULL = éveillée) matérialise le
sommeil **volontaire**. L'UI et le monitoring lisent cet état : une app endormie
s'affiche « en veille (scale-to-zero) », **jamais** « down »/« unhealthy ». Le
control plane n'endort qu'une app dont `desired_status = running` — une app
arrêtée manuellement le reste, et un déploiement la réveille (le waker `docker
start` le nouveau conteneur au premier hit).

### 4. Uptime : répondu sans réveiller une app endormie

Un check d'uptime AkerDock porte un en-tête d'identification (`X-AkerDock-Uptime`).
Le waker ne le compte **jamais** comme activité, et surtout :

- **app endormie** → le waker **répond directement `200`** (en-tête
  `X-AkerDock-Scale: asleep`) **sans démarrer quoi que ce soit**. C'est honnête :
  une app scale-to-zero endormie *est* disponible — elle se réveille au premier
  vrai trafic. Le monitoring la voit *up*, et un check ne cold-starte pas tout le
  stack ;
- **app déjà éveillée** → le check est **relayé** vers l'app réelle (santé
  réelle), toujours sans compter comme activité.

Ainsi le monitoring ne défait pas le scale-to-zero et ne provoque aucun réveil
périodique. Contrepartie assumée : sur une app endormie, l'uptime mesure la
*disponibilité du service* (capacité à répondre), pas la santé interne d'un
conteneur arrêté — ce qui est le sens voulu du scale-to-zero. (Les alternatives —
réveiller à chaque check, exclure l'app du monitoring, ou la laisser éveillée en
permanence — ont été écartées.)

### 5. Mécanisme réutilisé tel quel

Aucun changement au waker (ADR-036) : même conteneur (1 par serveur, partagé
previews + apps), routage par Host, `routes.json` fusionné par ressource. Le
`wake set` d'une app est l'ensemble de ses conteneurs (label
`akerdock.resource_uuid`, INV-011). Le scan du scheduler pour les apps reflète
celui des previews (lecture du fichier d'activité par SSH, `docker stop` des
inactives, réveil des endormies dont l'activité redevient fraîche).

## Conséquences

- **Positives** : un seul mécanisme, un seul waker par serveur, couvre previews
  **et** apps ; opt-in par app ; économie de ressources sur les apps peu
  sollicitées sans registry ni composant supplémentaire.
- **Négatives / limites** : cold-start sur trafic réel (à réserver aux apps qui
  le tolèrent) ; incompatible avec les workloads à tâche de fond (documenté, non
  bloqué techniquement — c'est un choix opérateur) ; un check uptime provoque un
  réveil périodique. Le comportement live reste **validé en E2E** (ADR-028) ; les
  tests unitaires couvrent la décision (endormir/réveiller, respect de
  `desired_status`, en-tête uptime).

## Alternatives rejetées

- **Réutiliser le flag preview unique pour l'app** : coupler previews et prod
  sous un seul interrupteur, alors que ce sont deux décisions de risque
  distinctes. Écarté au profit de deux opt-ins séparés.
- **Exclure les apps STZ du monitoring uptime** : prive l'opérateur de la mesure
  de disponibilité réelle. Écarté au profit de « le check réveille mais ne compte
  pas comme activité ».
- **Endormir aussi les bases managées** : coupe les connexions et fragilise les
  backups pour un gain douteux. Exclu par construction (flag sur `applications`
  seulement).
