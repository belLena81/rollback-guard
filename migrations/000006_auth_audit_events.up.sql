-- Append-only audit of auth changes (docs/phase7-auth.md). Rows hold only
-- identifiers, roles and counts: no tokens, hashes, emails or free text.
-- target_user_id is not a foreign key so the audit outlives the user row.

CREATE TABLE auth_audit_events (
    id text PRIMARY KEY CHECK (btrim(id) <> ''),
    occurred_at timestamptz NOT NULL,
    actor_id text NOT NULL CHECK (btrim(actor_id) <> ''),
    action text NOT NULL CHECK (action IN ('role_changed', 'sessions_revoked', 'logout')),
    target_user_id text CHECK (btrim(target_user_id) <> ''),
    from_role text CHECK (from_role IN ('viewer', 'operator', 'administrator')),
    to_role text CHECK (to_role IN ('viewer', 'operator', 'administrator')),
    session_count integer CHECK (session_count >= 0),
    CONSTRAINT auth_audit_events_shape CHECK (
        (action = 'role_changed' AND from_role IS NOT NULL AND to_role IS NOT NULL
            AND from_role <> to_role AND session_count IS NULL)
        OR (action = 'sessions_revoked' AND from_role IS NULL AND to_role IS NULL
            AND session_count IS NOT NULL)
        OR (action = 'logout' AND from_role IS NULL AND to_role IS NULL
            AND session_count IS NULL)
    )
);

-- ListRecent.
CREATE INDEX auth_audit_events_occurred_at_idx ON auth_audit_events (occurred_at DESC, id DESC);

-- Append-only: reject every UPDATE and DELETE.
CREATE FUNCTION auth_audit_events_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'auth_audit_events is append-only';
END
$$;

CREATE TRIGGER auth_audit_events_append_only
    BEFORE UPDATE OR DELETE ON auth_audit_events
    FOR EACH ROW EXECUTE FUNCTION auth_audit_events_append_only();
