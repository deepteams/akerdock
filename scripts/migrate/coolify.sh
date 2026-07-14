#!/usr/bin/env bash
# =============================================================================
# coolify.sh — migrer un serveur Coolify vers AkerDock, par adoption générique
#
# ADR-023 : AkerDock n'a AUCUN assistant d'import propriétaire — l'adoption de
# ressources Docker standards (§20.7, ADR-013) EST le chemin de migration. Ce
# script est un outil de commodité PAR-DESSUS l'API publique : il ne lit ni la
# base interne de Coolify, ni son API — uniquement ce que le scan d'adoption
# voit sur le serveur (containers, stacks compose, volumes, labels standards).
#
# Ce qu'il fait :
#   1. lance un scan d'adoption du serveur (POST /servers/{uuid}/adoption-scans)
#   2. repère les workloads créés par Coolify via leurs labels Docker publics
#      (coolify.managed / coolify.*), en écartant l'infrastructure de Coolify
#      elle-même (coolify, coolify-db, coolify-realtime, coolify-proxy, …)
#   3. affiche le rapport : adoptables, non adoptables (avec motif), ignorés
#   4. avec --apply : crée (ou réutilise) le projet cible et adopte tout —
#      sans redémarrer un seul workload
#
# Ce qu'il ne migre PAS (ADR-023, conséquence assumée) : ce qui vit dans la
# base de Coolify et pas dans les objets Docker — plans de backup, membres,
# webhooks, historique. À ressaisir côté AkerDock.
#
# Après adoption, chaque ressource reste servie par le proxy de Coolify jusqu'à
# son premier redéploiement AkerDock (normalisation). Migration type :
#   1. adopter (ce script)  2. arrêter le proxy Coolify  3. démarrer le proxy
#   AkerDock  4. redéployer ressource par ressource (les volumes sont repris
#   sous leur nom d'origine — aucune donnée n'est perdue).
#
# Usage :
#   AKERDOCK_URL=https://akerdock.example.com \
#   AKERDOCK_TOKEN=<token write> \
#   ./coolify.sh --server <server_uuid> [--project coolify-import] [--all] [--apply]
#
#   --server UUID   serveur AkerDock (déjà enregistré et validé) qui héberge
#                   les workloads Coolify
#   --project NAME  nom du projet AkerDock cible (défaut : coolify-import)
#   --all           adopter TOUS les candidats adoptables, pas seulement ceux
#                   portant des labels Coolify
#   --apply         exécuter. Sans ce drapeau : dry-run, rien n'est créé.
#
# Dépendances : curl, jq.
# =============================================================================
set -euo pipefail

SERVER_UUID="" PROJECT_NAME="coolify-import" APPLY=0 ALL=0
while [ $# -gt 0 ]; do
  case "$1" in
    --server)  SERVER_UUID=$2; shift 2 ;;
    --project) PROJECT_NAME=$2; shift 2 ;;
    --apply)   APPLY=1; shift ;;
    --all)     ALL=1; shift ;;
    -h|--help) sed -n '2,40p' "$0"; exit 0 ;;
    *) echo "argument inconnu : $1 (voir --help)" >&2; exit 2 ;;
  esac
done

: "${AKERDOCK_URL:?AKERDOCK_URL est requis (ex: https://akerdock.example.com)}"
: "${AKERDOCK_TOKEN:?AKERDOCK_TOKEN est requis (token API avec la permission write)}"
[ -n "$SERVER_UUID" ] || { echo "--server <uuid> est requis" >&2; exit 2; }
command -v jq >/dev/null || { echo "jq est requis" >&2; exit 2; }

B="${AKERDOCK_URL%/}/api/v1"

api() { # api METHOD PATH [JSON_BODY] — échoue bruyamment sur un statut non-2xx
  local method=$1 path=$2 body=${3:-} out code
  if [ -n "$body" ]; then
    out=$(curl -sS -w '\n%{http_code}' -X "$method" \
      -H "Authorization: Bearer $AKERDOCK_TOKEN" -H 'Content-Type: application/json' \
      -H "Idempotency-Key: coolify-migrate-$(date +%s)-$RANDOM" \
      -d "$body" "$B$path")
  else
    out=$(curl -sS -w '\n%{http_code}' -X "$method" \
      -H "Authorization: Bearer $AKERDOCK_TOKEN" "$B$path")
  fi
  code=${out##*$'\n'}
  out=${out%$'\n'*}
  case "$code" in
    2*) printf '%s' "$out" ;;
    *)  echo "✘ $method $path → HTTP $code" >&2; echo "$out" >&2; exit 1 ;;
  esac
}

wait_job() { # wait_job JOB_UUID [TIMEOUT_S]
  local ju=$1 timeout=${2:-300} st=""
  for _ in $(seq 1 "$timeout"); do
    st=$(api GET "/jobs/$ju" | jq -r '.status')
    case "$st" in succeeded) return 0 ;; dead_letter|cancelled) return 1 ;; esac
    sleep 1
  done
  return 1
}

echo "== scan d'adoption du serveur $SERVER_UUID"
SCAN=$(api POST "/servers/$SERVER_UUID/adoption-scans")
SCAN_UUID=$(echo "$SCAN" | jq -r '.adoption_scan_uuid')
wait_job "$(echo "$SCAN" | jq -r '.job_uuid')" || { echo "✘ le scan a échoué" >&2; exit 1; }
RESULT=$(api GET "/adoption-scans/$SCAN_UUID")

# --- tri des candidats --------------------------------------------------------
# Un workload Coolify se reconnaît à ses labels Docker PUBLICS (coolify.*) —
# exactement ce que le scan expose, sans lire le schéma interne de Coolify
# (ADR-023). L'infrastructure de Coolify elle-même (le panneau, sa base, son
# proxy) est écartée : l'adopter reviendrait à faire gérer Coolify par
# AkerDock, ce qui n'est pas migrer.
INFRA_RE='^(coolify|coolify-db|coolify-redis|coolify-realtime|coolify-proxy|coolify-sentinel)(-[0-9]+)?$'

CANDIDATES=$(echo "$RESULT" | jq -c --arg all "$ALL" --arg infra "$INFRA_RE" '
  [.candidates[]
    | .is_infra = ([.containers[].container_name] | any(test($infra)))
    | .is_coolify = ([.containers[] | (.labels // {}) | keys[]] | any(startswith("coolify.")))
    | select(.is_infra | not)
    | select(($all == "1") or .is_coolify)
  ]')

TOTAL=$(echo "$CANDIDATES" | jq 'length')
ADOPTABLE=$(echo "$CANDIDATES" | jq '[.[] | select(.adoptable)] | length')
echo
echo "== rapport du scan ($TOTAL candidats retenus, $ADOPTABLE adoptables)"
echo "$CANDIDATES" | jq -r '.[] |
  (if .adoptable then "  ✔ " else "  ✘ " end)
  + .proposed_name + "  [" + .kind + "]"
  + (if .compose_project then "  (compose: " + .compose_project + ")" else "" end)
  + (if .adoptable | not then "\n      motifs: " + ((.reasons // []) | join(" ; ")) else "" end)'
SKIPPED=$(echo "$RESULT" | jq --argjson kept "$CANDIDATES" \
  '($kept | map(.id)) as $ids | [.candidates[] | select(.id as $i | $ids | index($i) | not) | .proposed_name]')
[ "$(echo "$SKIPPED" | jq 'length')" = "0" ] || {
  echo
  echo "-- ignorés (infra Coolify ou hors périmètre — utilisez --all pour tout retenir) :"
  echo "$SKIPPED" | jq -r '.[] | "     " + .'
}

if [ "$APPLY" != "1" ]; then
  echo
  echo "Dry-run : rien n'a été créé. Relancez avec --apply pour adopter les $ADOPTABLE candidats adoptables."
  exit 0
fi
[ "$ADOPTABLE" != "0" ] || { echo "rien à adopter"; exit 0; }

# --- projet + environnement cible ----------------------------------------------
echo
echo "== projet cible « $PROJECT_NAME »"
PROJECT_UUID=$(api GET "/projects?limit=100" | jq -r --arg n "$PROJECT_NAME" '[.data[] | select(.name == $n) | .uuid][0] // empty')
if [ -z "$PROJECT_UUID" ]; then
  PROJECT_UUID=$(api POST /projects "{\"name\":\"$PROJECT_NAME\"}" | jq -r '.uuid')
  echo "   projet créé"
else
  echo "   projet existant réutilisé"
fi
ENV_UUID=$(api GET "/projects/$PROJECT_UUID/environments" | jq -r '.data[0].uuid // empty')
[ -n "$ENV_UUID" ] || { echo "✘ le projet n'a pas d'environnement" >&2; exit 1; }

# --- adoption en masse ----------------------------------------------------------
ITEMS=$(echo "$CANDIDATES" | jq -c '[.[] | select(.adoptable) | {candidate_id: .id}]')
echo
echo "== adoption de $ADOPTABLE ressources (sans redémarrage)"
JU=$(api POST "/adoption-scans/$SCAN_UUID/adopt" \
  "{\"environment_uuid\":\"$ENV_UUID\",\"items\":$ITEMS}" | jq -r '.job_uuid')
wait_job "$JU" || { echo "✘ l'adoption a échoué — voir le job $JU" >&2; exit 1; }
JOB=$(api GET "/jobs/$JU")
echo "$JOB" | jq -r '.result.adopted // [] | "   " + (length | tostring) + " ressources adoptées :" , ("     " + .[])'
echo "$JOB" | jq -r '(.result.warnings // [])[] | "   ⚠ " + .'

cat <<'EOF'

== migration adoptée — prochaines étapes
   1. Vérifiez les ressources dans le dashboard AkerDock (elles tournent
      toujours sous le proxy Coolify, rien n'a été redémarré).
   2. Ressaisissez ce que Docker ne porte pas : plans de backup, webhooks,
      notifications, membres (ADR-023).
   3. Quand vous êtes prêt à basculer : arrêtez le proxy Coolify, démarrez le
      proxy AkerDock du serveur, puis redéployez chaque ressource — ce premier
      redéploiement la normalise, en conservant volumes et domaines.
   4. Une ressource peut être rendue à tout moment : POST .../disown ne
      détruit jamais rien.
EOF
