-- Lock first so no row can be inserted between the check and the change.
LOCK TABLE auth_audit_events IN ACCESS EXCLUSIVE MODE;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM auth_audit_events WHERE action IN ('user_disabled', 'user_enabled')) THEN
        RAISE EXCEPTION 'cannot remove user status audit actions while such audit events exist';
    END IF;
END
$$;

ALTER TABLE auth_audit_events DROP CONSTRAINT auth_audit_events_shape;
ALTER TABLE auth_audit_events ADD CONSTRAINT auth_audit_events_shape CHECK (
    (action = 'role_changed' AND from_role IS NOT NULL AND to_role IS NOT NULL
        AND from_role <> to_role AND session_count IS NULL)
    OR (action = 'sessions_revoked' AND from_role IS NULL AND to_role IS NULL
        AND session_count IS NOT NULL)
    OR (action = 'logout' AND from_role IS NULL AND to_role IS NULL
        AND session_count IS NULL)
);

ALTER TABLE auth_audit_events DROP CONSTRAINT auth_audit_events_action_check;
ALTER TABLE auth_audit_events ADD CONSTRAINT auth_audit_events_action_check
    CHECK (action IN ('role_changed', 'sessions_revoked', 'logout'));
