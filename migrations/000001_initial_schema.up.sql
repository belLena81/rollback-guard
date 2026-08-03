CREATE TYPE incident_status AS ENUM (
    'created',
    'investigating',
    'awaiting_approval',
    'approved',
    'executing',
    'verifying',
    'resolved',
    'canceled',
    'declined',
    'rejected',
    'expired',
    'failed'
);

CREATE TYPE evidence_kind AS ENUM (
    'deployment',
    'metric',
    'pod',
    'kubernetes_event'
);

CREATE TYPE diagnosis_strategy AS ENUM (
    'rules'
);

CREATE TYPE recommendation AS ENUM (
    'rollback',
    'no_action'
);

CREATE TYPE actor_kind AS ENUM (
    'human',
    'agent',
    'system',
    'service'
);

CREATE TYPE approval_decision AS ENUM (
    'approved',
    'rejected'
);

CREATE TYPE execution_outcome AS ENUM (
    'succeeded',
    'failed'
);

CREATE TYPE event_kind AS ENUM (
    'incident.created',
    'incident.status_changed',
    'investigation.evidence_collected',
    'investigation.diagnosis_completed',
    'policy.evaluated',
    'rollback.proposal_created',
    'rollback.approved',
    'rollback.rejected',
    'rollback.execution_completed',
    'rollback.verification_completed',
    'incident.failed',
    'incident.expired',
    'incident.canceled'
);

CREATE TYPE idempotency_operation AS ENUM (
    'execute_rollback'
);

CREATE TYPE idempotency_state AS ENUM (
    'pending',
    'completed'
);

CREATE TABLE incidents (
    id text PRIMARY KEY,
    cluster text NOT NULL,
    namespace text NOT NULL,
    deployment text NOT NULL,
    service text NOT NULL,
    symptom_kind text NOT NULL,
    symptom_observed double precision NOT NULL,
    symptom_threshold double precision NOT NULL CHECK (symptom_threshold > 0),
    symptom_unit text NOT NULL,
    symptom_started_at timestamptz NOT NULL,
    status incident_status NOT NULL,
    version bigint NOT NULL CHECK (version > 0),
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL CHECK (updated_at >= created_at)
);

CREATE UNIQUE INDEX incidents_one_active_per_deployment
    ON incidents (cluster, namespace, deployment, service)
    WHERE status NOT IN ('resolved', 'canceled', 'declined', 'rejected', 'expired', 'failed');

CREATE TABLE incident_evidence (
    id text PRIMARY KEY,
    incident_id text NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
    kind evidence_kind NOT NULL,
    collected_at timestamptz NOT NULL,
    payload jsonb NOT NULL
);

CREATE INDEX incident_evidence_incident_id_collected_at_idx
    ON incident_evidence (incident_id, collected_at, id);

CREATE TABLE investigation_results (
    id text PRIMARY KEY,
    incident_id text NOT NULL UNIQUE REFERENCES incidents(id) ON DELETE CASCADE,
    strategy diagnosis_strategy NOT NULL,
    hypothesis text NOT NULL,
    confidence double precision NOT NULL CHECK (confidence >= 0 AND confidence <= 1),
    recommendation recommendation NOT NULL,
    created_at timestamptz NOT NULL
);

CREATE TABLE investigation_result_evidence_refs (
    conclusion_id text NOT NULL REFERENCES investigation_results(id) ON DELETE CASCADE,
    evidence_id text NOT NULL REFERENCES incident_evidence(id) ON DELETE RESTRICT,
    position integer NOT NULL CHECK (position > 0),
    PRIMARY KEY (conclusion_id, evidence_id),
    UNIQUE (conclusion_id, position)
);

CREATE TABLE policy_decisions (
    id text PRIMARY KEY,
    incident_id text NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
    allowed boolean NOT NULL,
    rules jsonb NOT NULL,
    decided_at timestamptz NOT NULL
);

CREATE TABLE action_proposals (
    action_id text PRIMARY KEY,
    incident_id text NOT NULL UNIQUE REFERENCES incidents(id) ON DELETE CASCADE,
    policy_decision_id text NOT NULL REFERENCES policy_decisions(id) ON DELETE RESTRICT,
    cluster text NOT NULL,
    namespace text NOT NULL,
    deployment text NOT NULL,
    service text NOT NULL,
    current_revision text NOT NULL,
    target_revision text NOT NULL,
    expires_at timestamptz NOT NULL,
    proposal_hash text NOT NULL UNIQUE,
    CHECK (current_revision <> target_revision),
    UNIQUE (action_id, proposal_hash)
);

CREATE TABLE approvals (
    id text PRIMARY KEY,
    incident_id text NOT NULL UNIQUE REFERENCES incidents(id) ON DELETE CASCADE,
    actor_id text NOT NULL,
    actor_kind actor_kind NOT NULL,
    decision approval_decision NOT NULL,
    action_id text NOT NULL,
    proposal_hash text NOT NULL,
    decided_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL CHECK (expires_at > decided_at),
    rejection_reason text NOT NULL DEFAULT '',
    CHECK ((decision = 'approved' AND rejection_reason = '') OR (decision = 'rejected' AND rejection_reason <> '')),
    FOREIGN KEY (action_id, proposal_hash) REFERENCES action_proposals(action_id, proposal_hash) ON DELETE RESTRICT
);

CREATE TABLE idempotency_keys (
    key text PRIMARY KEY,
    operation idempotency_operation NOT NULL,
    request_hash text NOT NULL,
    resource_id text NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
    state idempotency_state NOT NULL,
    started_at timestamptz NOT NULL,
    result_id text,
    completed_at timestamptz,
    CHECK ((state = 'pending' AND result_id IS NULL AND completed_at IS NULL) OR (state = 'completed' AND result_id IS NOT NULL AND completed_at IS NOT NULL AND completed_at >= started_at)),
    FOREIGN KEY (request_hash) REFERENCES action_proposals(proposal_hash) ON DELETE RESTRICT
);

CREATE TABLE executions (
    id text PRIMARY KEY,
    incident_id text NOT NULL UNIQUE REFERENCES incidents(id) ON DELETE CASCADE,
    idempotency_key text NOT NULL UNIQUE REFERENCES idempotency_keys(key) ON DELETE RESTRICT,
    from_revision text NOT NULL,
    to_revision text NOT NULL,
    outcome execution_outcome NOT NULL,
    started_at timestamptz NOT NULL,
    completed_at timestamptz NOT NULL CHECK (completed_at >= started_at),
    failure_reason text NOT NULL DEFAULT '',
    CHECK (from_revision <> to_revision),
    CHECK ((outcome = 'succeeded' AND failure_reason = '') OR (outcome = 'failed' AND failure_reason <> ''))
);

CREATE TABLE recovery_verifications (
    incident_id text PRIMARY KEY REFERENCES incidents(id) ON DELETE CASCADE,
    execution_id text NOT NULL REFERENCES executions(id) ON DELETE RESTRICT,
    passed boolean NOT NULL,
    revision text NOT NULL,
    desired_replicas integer NOT NULL CHECK (desired_replicas >= 0),
    ready_replicas integer NOT NULL CHECK (ready_replicas >= 0),
    available_replicas integer NOT NULL CHECK (available_replicas >= 0),
    metric_value double precision NOT NULL,
    checked_at timestamptz NOT NULL,
    failure_reason text NOT NULL DEFAULT '',
    CHECK ((passed AND failure_reason = '') OR (NOT passed AND failure_reason <> ''))
);

CREATE TABLE operation_events (
    id text PRIMARY KEY,
    incident_id text NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
    sequence bigint NOT NULL CHECK (sequence > 0),
    kind event_kind NOT NULL,
    occurred_at timestamptz NOT NULL,
    actor_id text NOT NULL,
    actor_kind actor_kind NOT NULL,
    detail jsonb NOT NULL,
    UNIQUE (incident_id, sequence)
);
