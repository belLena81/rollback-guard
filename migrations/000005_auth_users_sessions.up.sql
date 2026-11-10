-- Users and server-side sessions (docs/adr-auth-sessions.md). Only the
-- SHA-256 of a session token is stored; there is no raw token column.

CREATE TABLE users (
    id text PRIMARY KEY CHECK (btrim(id) <> ''),
    github_id bigint NOT NULL UNIQUE CHECK (github_id > 0),
    login text NOT NULL CHECK (login ~ '^[A-Za-z0-9-]{1,39}$'),
    email text NOT NULL DEFAULT '' CHECK (
        email = '' OR (char_length(email) <= 254 AND email ~ '^[^[:space:]]+@[^[:space:]]+$')
    ),
    organizations text[] NOT NULL DEFAULT '{}' CHECK (
        cardinality(organizations) = 0 OR array_ndims(organizations) = 1
    ),
    role text NOT NULL CHECK (role IN ('viewer', 'operator', 'administrator')),
    disabled boolean NOT NULL DEFAULT false,
    version bigint NOT NULL CHECK (version > 0),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL CHECK (updated_at >= created_at)
);

-- GitHub logins are case-insensitive.
CREATE UNIQUE INDEX users_login_key ON users (lower(login));

CREATE TABLE sessions (
    id text PRIMARY KEY CHECK (btrim(id) <> ''),
    user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash bytea NOT NULL CHECK (octet_length(token_hash) = 32),
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL CHECK (expires_at > created_at),
    revoked_at timestamptz CHECK (revoked_at >= created_at),
    last_seen_at timestamptz NOT NULL CHECK (last_seen_at >= created_at),
    CONSTRAINT sessions_token_hash_key UNIQUE (token_hash)
);

-- RevokeAllForUser and the users ON DELETE CASCADE.
CREATE INDEX sessions_user_id_idx ON sessions (user_id);

-- DeleteExpired.
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);
