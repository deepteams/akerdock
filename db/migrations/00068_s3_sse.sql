-- Chiffrement au repos des backups en S3 (ISO A.8.13/A.8.24). Le dump est
-- uploadé en direct du serveur cible vers S3 (URL présignée) : le chiffrer côté
-- control plane imposerait de proxifier les données, et le chiffrer côté serveur
-- cible imposerait d'y poser la clé de l'app (rompt le modèle de confiance).
-- On expose donc le server-side encryption S3 (opt-in par storage) : quand
-- `sse_algorithm` est renseigné (ex. 'AES256'), l'en-tête est signé dans l'URL
-- présignée et envoyé à l'upload. Nullable = pas de SSE (défaut sûr : les stores
-- sans support SSE, ex. MinIO sans KMS, restent fonctionnels). Expand-only.

-- +goose Up
ALTER TABLE s3_storages ADD COLUMN sse_algorithm text;

-- +goose Down
ALTER TABLE s3_storages DROP COLUMN sse_algorithm;
