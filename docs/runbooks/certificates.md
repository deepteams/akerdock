# Runbook — Certificats : échecs ACME, fallback self-signed, expirations, custom, wildcard

> Références : PRD §4.2–4.3 (Let's Encrypt HTTP-01, fallback self-signed, wildcard DNS-01 via Lego, certs custom dans `proxy/certs`, DNS de validation), §14.2 (`dns_validation_server`) ; spec proxy-contract §7 (certificats, §7.6 synchronisation) ; spec deployment-engine §5.1 (`/var/lib/akerdock/proxy/certs/`) ; data dictionary §6.7 (table `certificates`, reflet observé), §11.7 (`instance_settings.dns_validation_server`), §6.1 (`servers.wildcard_domain`, `proxy_http_port`).

> Note : le control plane maintient un **reflet observé** des certificats (table `certificates`) exposé par l'API : `GET /servers/{uuid}/certificates` (filtre `expiring_within_days`), `GET /certificates/{uuid}`, `POST /certificates/{uuid}/renew` (202 + job audité). Le diagnostic fin passe toujours par le serveur et les logs du proxy. Emplacements **normatifs** (proxy-contract §7.2/§7.5) : storage ACME `/var/lib/akerdock/proxy/acme.json` (0600), credentials DNS-01 `/var/lib/akerdock/proxy/acme.env` (0600).

## Symptômes

- Navigateur : avertissement TLS (certificat **self-signed** = le fallback d'échec d'émission est actif, §4.3) ou certificat **expiré**.
- Logs du proxy : erreurs ACME (`unable to obtain ACME certificate`, `acme: error: 429 … rateLimited`, `NXDOMAIN`, `connection refused` sur le challenge).
- Un domaine fraîchement ajouté ne passe jamais en HTTPS valide ; les autres domaines du serveur sont OK.

## Impact

- Self-signed actif : le site répond mais les clients voient un avertissement ; les intégrations qui vérifient le TLS échouent.
- Expiré : la plupart des clients refusent la connexion — équivalent d'une panne pour les navigateurs.
- Un échec ACME n'affecte que le(s) domaine(s) concerné(s), pas le routage HTTP ni les autres certificats du serveur.

## Diagnostic

1. **Qui est servi, jusqu'à quand ?** — d'abord le reflet control plane, puis la réalité TLS :
   ```sh
   curl -sS "$AKD/servers/$SERVER_UUID/certificates?expiring_within_days=30" \
     -H "Authorization: Bearer $TOKEN"    # inventaire : kind, domaines, status, not_after, last_error
   echo | openssl s_client -connect <fqdn>:443 -servername <fqdn> 2>/dev/null \
     | openssl x509 -noout -issuer -subject -enddate
   # issuer = Let's Encrypt (R…) attendu ; "TRAEFIK DEFAULT CERT" ou self-signed = fallback actif
   # (le reflet est synchronisé après chaque apply proxy — vérifier observed_at en cas de doute)
   ```
2. **Logs ACME du proxy** :
   ```sh
   ssh <user>@<serveur> "docker logs --tail 200 \$(docker ps -q --filter label=akerdock.type=proxy) 2>&1 | grep -i acme"
   ```
3. **DNS** — le domaine pointe-t-il vers le serveur, vu du DNS de validation de l'instance (§4.2, défaut `1.1.1.1`, custom : `instance_settings.dns_validation_server`) ?
   ```sh
   dig +short <fqdn> @1.1.1.1
   ssh <user>@<serveur> "curl -s ifconfig.me"     # IP publique du serveur — doit correspondre
   ```
4. **Port 80** — HTTP-01 exige que Let's Encrypt atteigne le port **80 public** du serveur :
   ```sh
   curl -s -o /dev/null -w '%{http_code}\n' http://<fqdn>/.well-known/acme-challenge/probe
   # 404 servi par Traefik = joignable (suffisant) ; timeout/refused = firewall ou port détourné
   ```
   ⚠️ Si `servers.proxy_http_port` ≠ 80 (reverse proxy amont, §27.1), HTTP-01 ne peut aboutir que si l'amont forwarde le port 80 vers le proxy — sinon utiliser DNS-01.
5. **Rate limits Let's Encrypt** : `429 rateLimited` dans les logs. Limites usuelles : 5 certificats en doublon exact/semaine, 50 certificats/domaine enregistré/semaine, 5 échecs de validation/compte/hostname/heure. L'erreur indique le délai de retry.

## Résolution pas à pas

### A. Échec d'émission (fallback self-signed actif)

1. Corriger la cause identifiée au diagnostic :
   - **DNS** : corriger l'enregistrement A/AAAA (ou l'entrée wildcard) ; attendre la propagation vue du DNS de validation (diagnostic 3).
   - **Port 80** : ouvrir au niveau du firewall **du provider cloud** (§10.4 — Docker bypasse UFW) ; vérifier qu'aucun autre process n'écoute (`ss -ltnp | grep ':80'`).
   - **Rate limit** : attendre le délai indiqué. ⚠️ Ne pas boucler des retries (chaque échec de validation consomme la limite « 5 échecs/heure »). Pour déboguer sans consommer de quota, tester la chaîne avec le CA de staging Let's Encrypt sur un domaine jetable.
2. Forcer une nouvelle tentative d'émission :
   ```sh
   curl -sS -X POST "$AKD/certificates/$CERT_UUID/renew" -H "Authorization: Bearer $TOKEN"
   # 202 + job audité : sauvegarde puis retrait ciblé de l'entrée d'acme.json, redémarrage du
   # proxy, resynchronisation du reflet ; 422 pour un certificat custom/self_signed
   ```
   À défaut, redéployer l'application (régénération de la config proxy) ou redémarrer le proxy — Traefik retente l'émission pour les domaines sans certificat valide au démarrage.
3. Si Traefik a mémorisé un état ACME corrompu : le job `renew` (A.2) fait précisément l'édition ciblée ; à la main (fallback), intervention sur le storage ACME (emplacement **normatif** : `/var/lib/akerdock/proxy/acme.json`) :
   ```sh
   ssh <user>@<serveur> "cp -a /var/lib/akerdock/proxy/acme.json /var/lib/akerdock/tmp/acme.json.bak-\$(date -u +%s)"
   # édition ciblée : supprimer uniquement l'entrée du domaine en échec, puis docker restart <proxy>
   ```
   ⚠️ **Supprimer `acme.json` entier force la ré-émission de TOUS les certificats du serveur** → risque direct de rate limit. Toujours une sauvegarde d'abord, et une édition ciblée.

### B. Certificat expiré

Un certificat expiré = un **renouvellement** qui échoue depuis des semaines (Traefik renouvelle ~30 jours avant l'échéance). Dérouler le diagnostic A — la cause est presque toujours DNS modifié, port 80 fermé depuis l'émission initiale, ou proxy resté down pendant la fenêtre de renouvellement. Corriger puis forcer la ré-émission (A.2).

### C. Certificats custom (§4.3)

1. Déposer clé + fullchain sur le serveur :
   ```sh
   scp fullchain.pem privkey.pem <user>@<serveur>:/var/lib/akerdock/proxy/certs/<fqdn>/
   ssh <user>@<serveur> "chmod 0600 /var/lib/akerdock/proxy/certs/<fqdn>/privkey.pem"
   ```
2. Référencer via la configuration dynamique gérée par AkerDock (UI du serveur/domaine — la config générée ajoute la section `tls.certificates`).
3. Vérifications classiques : correspondance clé/cert (`openssl x509 -noout -modulus | openssl md5` vs `openssl rsa -noout -modulus | openssl md5`), **ordre de la chaîne** (leaf d'abord), dates.
4. ⚠️ Les customs ne se renouvellent pas seuls (`POST /certificates/{uuid}/renew` répond `422` pour un `custom`) : leur expiration est suivie dans le reflet `certificates` (alerte J-30/J-7) — vérifier qu'ils apparaissent bien dans `GET /servers/{uuid}/certificates` après dépôt (Prévention).

### D. Wildcard via DNS-01 (§4.3)

1. Prérequis : provider DNS supporté par Lego (Cloudflare, Route 53, OVH, Hetzner…) et ses credentials configurés pour le proxy du serveur (matérialisés en `/var/lib/akerdock/proxy/acme.env`, 0600 — emplacement normatif ; référencés via `certificates.dns_credential_id`) ; `servers.wildcard_domain` renseigné (§4.2).
2. Échec typique : credentials DNS invalides/expirés (les roter → [key-rotation.md](key-rotation.md)) ou propagation lente du TXT. Vérifier le challenge :
   ```sh
   dig +short TXT _acme-challenge.<domaine> @1.1.1.1
   ```
3. DNS-01 ne dépend **pas** du port 80 — c'est la solution de repli quand le port 80 est structurellement indisponible (diagnostic 4).

## Vérification

```sh
echo | openssl s_client -connect <fqdn>:443 -servername <fqdn> 2>/dev/null \
  | openssl x509 -noout -issuer -enddate      # issuer Let's Encrypt, notAfter ≈ +90 jours
curl -fsS -o /dev/null https://<fqdn>/        # sans -k : la chaîne valide de bout en bout
```

- Plus d'erreurs ACME dans les logs du proxy.
- Pour un wildcard : deux sous-domaines distincts servent le même certificat `*.domaine`.
- « Force HTTPS » (§4.3) : `curl -s -o /dev/null -w '%{http_code}' http://<fqdn>/` → 301/308.

## Prévention

- **Surveiller les expirations** : `GET /servers/{uuid}/certificates?expiring_within_days=14` (reflet `certificates`, index sur `not_after` ; alerte intégrée à J-30/J-7 — proxy-contract §7.3) ; en complément, check d'uptime intégré (§27.17) ou cron externe `openssl x509 -checkend 1209600` sur chaque FQDN critique.
- Préférer un **wildcard par serveur** (§4.2) quand les sous-domaines prolifèrent : moins d'émissions, moins de rate limits.
- Ne pas fermer le port 80 « pour la sécurité » sur un serveur en HTTP-01 : le renouvellement en dépend (la redirection Force HTTPS le neutralise fonctionnellement).
- Toute modification DNS d'un domaine servi = re-vérifier l'émission au prochain renouvellement (poser un rappel).
- Sauvegarder `/var/lib/akerdock/proxy/` (acme.json + certs custom) dans les backups serveur : en cas de reconstruction, cela évite une ré-émission en masse (rate limits).
