---
name: verify
description: Lancer AkerDock en local contre un Postgres jetable et piloter l'API /auth ou l'UI embarquée pour vérifier un changement en conditions réelles.
---

# Vérifier AkerDock en local

## Lancer l'app (all-in-one)

```bash
# Postgres jetable (l'image officielle contient citext)
docker run -d --rm --name akd-verify -e POSTGRES_USER=akerdock \
  -e POSTGRES_PASSWORD=verify -e POSTGRES_DB=akerdock -p 15477:5432 postgres:17-alpine

export AKERDOCK_DATABASE_URL="postgres://akerdock:verify@localhost:15477/akerdock?sslmode=disable"
export AKERDOCK_MASTER_KEY="1:$(openssl rand -base64 32)"
export AKERDOCK_ROOT_EMAIL=root@example.com AKERDOCK_ROOT_NAME=Root \
  AKERDOCK_ROOT_PASSWORD="a-very-long-verify-password"
export AKERDOCK_PORT=18475
export AKERDOCK_DATA_DIR=$(mktemp -d)   # sinon fatal: /var/lib/akerdock non créable

go build -o /tmp/akerdock ./cmd/akerdock && /tmp/akerdock
```

Les migrations goose s'appliquent au démarrage ; le bootstrap crée le root
user depuis les variables `AKERDOCK_ROOT_*`. Le job `server.validate` du
serveur localhost échoue en boucle (pas de SSH) — bruit attendu, pas une
panne.

## Pièges

- **L'UI embarquée vient de `internal/web/dist`** (go:embed), PAS de
  `web/dist`. Après un changement UI : `npm --prefix web run build` puis
  `cp -r web/dist/akerdock-web/browser/. internal/web/dist/` (cible
  `make web`), puis recompiler le binaire. Sans cela, on pilote l'ancienne UI.
  `internal/web/dist` est suivi par git et se committe avec les changements UI.
- **Rate limit `/auth`** : 30 req/min par IP — espacer les sondes ou dormir 30 s.
- **Lockout** : 5 échecs (login ou code MFA) verrouillent le compte 15 min.
  Déverrouiller : `docker exec akd-verify psql -U akerdock -d akerdock -c
  "UPDATE users SET failed_login_count=0, locked_until=NULL;"`.

## Piloter

- **API** : `POST /auth/login` (JSON email/password) → cookies + `csrf_token` ;
  mutations `/auth/*` exigent le header `X-CSRF-Token`. Le contrat v1 est sous
  `/api/v1` (Bearer).
- **UI** : Playwright avec le Chrome système, sans télécharger de navigateur :
  `chromium.launch({ channel: 'chrome', headless: true })` (installer le paquet
  `playwright` seul dans un dossier temporaire). Routes utiles : `/sign-in`,
  `/security`, `/applications`.
- **TOTP** : générer les codes avec une implémentation indépendante (python3 :
  `hmac` + `base32decode`, SHA-1, 6 chiffres, pas de 30 s) — prouve
  l'interopérabilité avec les vraies apps. L'anti-rejeu brûle le pas courant :
  pour un second code immédiat, prendre le pas suivant (+1).
- **OAuth/OIDC** : IdP factice en Go stdlib sur `http://localhost:9091`
  (discovery + JWKS + authorize auto-approuvé + token signant un vrai JWT
  RS256, PKCE **vérifié** côté IdP, `POST /control` pour changer sub/email
  entre scénarios) — `ValidateIssuer` tolère http sur localhost uniquement.
  Configurer via `PUT /api/v1/system/oauth-providers/oidc` (session root +
  `X-CSRF-Token`). `registration_enabled` est `false` par défaut : l'activer
  en SQL et attendre ~4 s (TTL du cache de settings).
