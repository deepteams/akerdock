-- Journal d'audit append-only FORCÉ en base (§23.4, ISO A.8.15 / SOC2 CC7).
-- Jusqu'ici l'append-only était une convention (aucune requête UPDATE/DELETE
-- générée). On l'impose par un trigger : une UPDATE est toujours refusée, une
-- DELETE aussi — SAUF via la fonction de purge de rétention, qui pose un GUC
-- local (`akerdock.audit_purge`) que le trigger reconnaît. Un rôle applicatif
-- non-superuser ne peut donc ni modifier ni effacer d'entrée d'audit hors de la
-- purge sanctionnée (qui, elle, ne touche que les lignes au-delà de la rétention).

-- +goose Up
-- +goose StatementBegin
CREATE FUNCTION audit_events_immutable() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF current_setting('akerdock.audit_purge', true) = 'on' THEN
            RETURN OLD; -- purge de rétention sanctionnée (purge_audit_events)
        END IF;
        RAISE EXCEPTION 'audit_events is append-only: DELETE is not permitted';
    END IF;
    RAISE EXCEPTION 'audit_events is append-only: UPDATE is not permitted';
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER audit_events_no_mutation
    BEFORE UPDATE OR DELETE ON audit_events
    FOR EACH ROW EXECUTE FUNCTION audit_events_immutable();

-- +goose StatementBegin
CREATE FUNCTION purge_audit_events(retention_days integer) RETURNS bigint AS $$
DECLARE
    deleted bigint;
BEGIN
    IF retention_days IS NULL OR retention_days <= 0 THEN
        RETURN 0; -- rétention désactivée : on garde tout (non-répudiation)
    END IF;
    -- Autorise la DELETE ci-dessous pour la durée de CETTE transaction seulement.
    PERFORM set_config('akerdock.audit_purge', 'on', true);
    WITH del AS (
        DELETE FROM audit_events
        WHERE occurred_at < now() - make_interval(days => retention_days)
        RETURNING 1
    )
    SELECT count(*) INTO deleted FROM del;
    RETURN deleted;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER IF EXISTS audit_events_no_mutation ON audit_events;
DROP FUNCTION IF EXISTS audit_events_immutable();
DROP FUNCTION IF EXISTS purge_audit_events(integer);
