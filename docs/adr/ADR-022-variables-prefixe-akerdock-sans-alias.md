# ADR-022 — Variables prédéfinies : préfixe `AKERDOCK_*` uniquement, sans alias

- **Statut** : Accepté
- **Date** : 2026-07-11
- **Sections PRD liées** : §27.22, §5.4, §27.10, §29.5

## Contexte

AkerDock injecte des variables prédéfinies dans chaque container qu'il déploie (FQDN, URL, branche, identifiant de PR… — §5.4). Il faut choisir leur espace de noms. Les plateformes du domaine préfixent ces variables par leur propre marque, et une partie de l'écosystème de templates et d'applications lit ces noms-là : reprendre un préfixe existant faciliterait le copier-coller de templates tiers, au prix d'un nom qui n'est pas le nôtre dans chacun de nos containers.

## Décision

Préfixe **`AKERDOCK_*` uniquement** : `AKERDOCK_FQDN`, `AKERDOCK_URL`, `AKERDOCK_BRANCH`, `AKERDOCK_PR_ID`, etc. — **aucun alias** sous un autre préfixe :

- une variable, un nom : deux noms pour une même valeur est une divergence qui attend son heure (documentation, support, et le jour où seul l'un des deux est mis à jour) ;
- identité propre, pas de dette de nommage sous une marque tierce dans les containers de nos utilisateurs.

La syntaxe des **magic variables `SERVICE_<TYPE>_<ID>`** (`SERVICE_FQDN_*`, `SERVICE_PASSWORD_*`, `SERVICE_URL_*`… — §5.4) est **conservée telle quelle** : elle est fonctionnelle et sans marque, et c'est elle qui porte l'essentiel de la compatibilité des fichiers compose du domaine (credentials générés, URLs).

## Alternatives considérées

- **Alias sous un préfixe tiers, en parallèle** : rejeté — chaque variable existerait en double pour toujours ; la dette ne serait jamais remboursée et la documentation devrait couvrir les deux noms.
- **Adopter le préfixe d'une plateforme existante (compatibilité maximale)** : rejeté — ancrerait le produit sous la marque d'un tiers, y compris dans tous les containers déployés, et lierait notre espace de noms à ses évolutions.
- **Renommer aussi les magic variables (`AKERDOCK_SERVICE_*`)** : rejeté — `SERVICE_<TYPE>_<ID>` est une syntaxe fonctionnelle sans marque ; la casser détruirait la compatibilité des templates compose sans aucun gain d'identité.

## Conséquences

- **Positives** : espace de noms propre et cohérent dès le premier jour ; aucune ambiguïté documentaire ; le maintien de `SERVICE_<TYPE>_<ID>` préserve la compatibilité des fichiers compose de l'écosystème.
- **Négatives** : un template écrit pour une autre plateforme et lisant ses variables préfixées ne fonctionne pas tel quel — les variables doivent être traduites à l'import dans le dépôt de templates (§27.10, ADR-010) ; une application dont le **code** lit un préfixe tiers doit être adaptée.
- **Risques acceptés** : friction pour qui arrive d'un écosystème existant (assumée — l'adoption générique §20.7 reste le chemin d'entrée) ; l'import de templates doit détecter les usages non triviaux (interpolations, scripts) qu'une réécriture mécanique manquerait.
