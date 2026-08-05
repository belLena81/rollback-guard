CREATE TYPE investigation_task_state AS ENUM (
    'queued',
    'submitting',
    'submitted',
    'working',
    'cancel_requested',
    'completed',
    'failed',
    'canceled'
);

CREATE TYPE investigation_stage AS ENUM (
    'queued',
    'deployment',
    'metrics',
    'diagnosis',
    'finalizing'
);

CREATE TABLE investigation_dispatches (
    incident_id text PRIMARY KEY REFERENCES incidents(id) ON DELETE CASCADE,
    operation_id text NOT NULL UNIQUE,
    idempotency_key text NOT NULL UNIQUE,
    message_id text NOT NULL UNIQUE,
    trace_id text NOT NULL,
    request_hash text NOT NULL,
    task_id text UNIQUE,
    context_id text,
    state investigation_task_state NOT NULL,
    stage investigation_stage NOT NULL,
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    lease_owner text,
    lease_expires_at timestamptz,
    failure_code text NOT NULL DEFAULT '',
    version bigint NOT NULL CHECK (version > 0),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL CHECK (updated_at >= created_at),
    completed_at timestamptz,
    CHECK ((lease_owner IS NULL) = (lease_expires_at IS NULL)),
    CHECK ((state IN ('completed', 'failed', 'canceled')) = (completed_at IS NOT NULL)),
    CHECK ((state = 'failed') = (failure_code <> ''))
);

CREATE INDEX investigation_dispatches_ready_idx
    ON investigation_dispatches (created_at, incident_id)
    WHERE state IN ('queued', 'submitting', 'submitted', 'working', 'cancel_requested');

CREATE TABLE a2a_tasks (
    task_id text PRIMARY KEY,
    context_id text NOT NULL,
    state text NOT NULL,
    version bigint NOT NULL CHECK (version > 0),
    snapshot jsonb NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL CHECK (updated_at >= created_at)
);

CREATE TABLE a2a_investigation_requests (
    idempotency_key text PRIMARY KEY,
    request_hash text NOT NULL,
    task_id text NOT NULL UNIQUE,
    state investigation_task_state NOT NULL,
    result jsonb,
    failure_code text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL CHECK (updated_at >= created_at),
    CHECK (state IN ('submitting', 'submitted', 'working', 'completed', 'failed', 'canceled')),
    CHECK ((state = 'completed') = (result IS NOT NULL)),
    CHECK ((state = 'failed') = (failure_code <> ''))
);
