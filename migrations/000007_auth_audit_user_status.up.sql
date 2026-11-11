-- Adds the user_disabled and user_enabled auth audit actions
-- (docs/phase7-auth.md). user_disabled carries the number of the target's
-- sessions revoked in the same transaction; user_enabled carries nothing.

ALTER TABLE auth_audit_events DROP CONSTRAINT auth_audit_events_action_check;
ALTER TABLE auth_audit_events ADD CONSTRAINT auth_audit_events_action_check
    CHECK (action IN ('role_changed', 'sessions_revoked', 'logout', 'user_disabled', 'user_enabled'));

ALTER TABLE auth_audit_events DROP CONSTRAINT auth_audit_events_shape;
ALTER TABLE auth_audit_events ADD CONSTRAINT auth_audit_events_shape CHECK (
    (action = 'role_changed' AND from_role IS NOT NULL AND to_role IS NOT NULL
        AND from_role <> to_role AND session_count IS NULL)
    OR (action IN ('sessions_revoked', 'user_disabled') AND from_role IS NULL AND to_role IS NULL
        AND session_count IS NOT NULL)
    OR (action IN ('logout', 'user_enabled') AND from_role IS NULL AND to_role IS NULL
        AND session_count IS NULL)
);
