-- Lock first so no row can be inserted between the check and the drop.
LOCK TABLE auth_audit_events IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM auth_audit_events) THEN
        RAISE EXCEPTION 'cannot remove auth audit migration while audit events exist';
    END IF;
END
$$;

DROP TABLE auth_audit_events;
DROP FUNCTION auth_audit_events_append_only();
