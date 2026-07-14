# ADR-019 — Notifications : routage par règles, agrégation/débounce, heures calmes

- **Statut** : Accepté
- **Date** : 2026-07-11
- **Sections PRD liées** : §27.19, §11, §26.2

## Contexte

Émettre un message par événement, avec pour seul réglage l'activation d'événements par canal (§11), rend le canal inutilisable : un serveur qui flappe (injoignable/joignable en boucle) génère des dizaines d'alertes identiques, et tous les projets d'une team partagent les mêmes canaux. Ce bruit détruit la valeur des alertes — les vraies finissent ignorées. Il faut décider du modèle de routage et de la lutte contre le bruit.

## Décision

Quatre capacités actées, au-dessus des canaux de parité (Email, Discord, Telegram, Slack, Pushover, webhooks — §11) :

1. **Règles de routage** par projet, environnement et sévérité vers les canaux — et plus seulement une matrice événement × canal au niveau team.
2. **Agrégation/débounce des événements répétitifs** : un serveur qui flappe produit une alerte agrégée, pas des dizaines de messages.
3. **Heures calmes configurables**.
4. **Résumé différé des événements non critiques** : ce qui n'exige pas d'action immédiate est regroupé et envoyé plus tard.

## Alternatives considérées

- **Parité stricte (un message par événement)** : rejetée — le flapping en boucle est précisément le défaut constaté de la référence ; le bruit rend l'alerting contre-productif.
- **Déléguer à un outil d'alerting externe (Alertmanager, PagerDuty…)** : rejeté comme réponse par défaut — impose un composant de plus à la cible self-hosted ; les webhooks custom restent disponibles pour qui veut brancher ces outils.
- **Routage par ressource individuelle** : rejeté — grain trop fin à administrer ; projet/environnement/sévérité couvre les besoins réels (prod alerte, staging résume).

## Conséquences

- **Positives** : alertes actionnables (la production réveille, le staging attend le résumé) ; fin des tempêtes de messages en cas de flapping ; les heures calmes respectent les astreintes sans désactiver l'alerting.
- **Négatives** : moteur de règles, fenêtres de débounce et files de résumés différés à concevoir, persister et tester (tests flapping/débounce exigés §26.2) ; la configuration devient plus riche donc plus complexe à présenter — des défauts sûrs et simples sont indispensables.
- **Risques acceptés** : toute agrégation retarde ou regroupe de l'information — un événement critique mal classé en « non critique » serait différé, d'où l'importance de la taxonomie de sévérité ; le comportement par défaut doit rester simple et prévisible pour ne pas surprendre l'opérateur qui découvre le produit.
