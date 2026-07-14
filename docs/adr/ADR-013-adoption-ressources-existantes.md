# ADR-013 — Adoption de ressources existantes sans redéploiement, prévisualisée et réversible

- **Statut** : Accepté
- **Date** : 2026-07-11
- **Sections PRD liées** : §27.13, §20.7, §27.23, INV-015

## Contexte

Aucune plateforme du segment ne sait prendre le contrôle d'un container ou d'un stack compose **déjà déployé** : l'utilisateur doit tout recréer dans la plateforme puis redéployer, avec interruption et risque sur les données. Or AkerDock distingue déjà structurellement ressources gérées et non gérées (INV-015). Il faut décider si la plateforme sait « adopter » l'existant, ce qui conditionne aussi la stratégie de migration (cf. ADR-023).

## Décision

**Adoption sans redéploiement, prévisualisée et réversible** — c'est aussi le chemin d'entrée depuis n'importe quelle plateforme (ADR-023). Le workflow est celui du §20.7 :

1. **Scan** d'un serveur : inventaire des containers et stacks compose non gérés (s'appuie sur INV-015).
2. **Mapping proposé** vers le modèle AkerDock : application ou service, réseaux, volumes, variables, ports et domaines détectés par inspection et labels.
3. **Prévisualisation** : ce qui sera géré, ce qui sera modifié (labels/metadata ajoutés), ce qui n'est pas adoptable et pourquoi.
4. **Adoption sans redéploiement** : prise de contrôle sans redémarrer le workload lorsque c'est possible ; le premier redéploiement normalise complètement la ressource.
5. **Réversibilité** : « désadopter » rend la ressource à son état non géré sans la détruire.

Critères d'acceptation (§20.7) : adopter un stack compose multi-services avec volumes puis le redéployer sans perte de données ; une ressource non représentable est signalée avec le motif, jamais adoptée partiellement en silence.

## Alternatives considérées

- **Pas d'adoption (parité)** : rejetée — migrer vers AkerDock exigerait de tout redéployer, friction maximale précisément au moment où l'on veut convaincre.
- **Import par recréation assistée (wizard qui régénère la ressource et redéploie)** : rejeté comme mécanisme principal — interruption de service et risque sur les volumes ; la recréation reste disponible via le premier redéploiement normalisateur.
- **Adoption silencieuse automatique de tout ce qui tourne** : rejetée — violerait la frontière géré/non géré (INV-015) et créerait des prises de contrôle non consenties ; l'adoption est toujours explicite et prévisualisée.

## Conséquences

- **Positives** : argument de migration unique sur le segment ; aucune interruption au moment de l'adoption ; réversibilité qui réduit le risque d'essai ; c'est le chemin de migration entrant du produit (ADR-023).
- **Négatives** : le moteur de mapping (inspection Docker → modèle interne) est complexe : cas partiels, labels hétérogènes, compose non standard ; la coexistence « adopté mais pas encore normalisé » crée un état intermédiaire que l'UI et le moteur de déploiement doivent gérer explicitement.
- **Risques acceptés** : certaines ressources resteront non adoptables (signalées avec motif) ; entre adoption et première normalisation, la ressource peut diverger du modèle interne — le redéploiement normalisateur est le point de convergence assumé.
