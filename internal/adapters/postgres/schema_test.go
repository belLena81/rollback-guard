package postgres

import (
	"os"
	"strings"
	"testing"
)

func TestInitialSchemaUsesPostgresEnumsForClosedSets(t *testing.T) {
	schema := readInitialSchema(t)
	requiredTypes := []string{
		"CREATE TYPE incident_status AS ENUM",
		"CREATE TYPE evidence_kind AS ENUM",
		"CREATE TYPE diagnosis_strategy AS ENUM",
		"CREATE TYPE recommendation AS ENUM",
		"CREATE TYPE actor_kind AS ENUM",
		"CREATE TYPE approval_decision AS ENUM",
		"CREATE TYPE execution_outcome AS ENUM",
		"CREATE TYPE event_kind AS ENUM",
		"CREATE TYPE idempotency_operation AS ENUM",
		"CREATE TYPE idempotency_state AS ENUM",
	}
	for _, required := range requiredTypes {
		if !strings.Contains(schema, required) {
			t.Fatalf("schema missing %q", required)
		}
	}
	for _, forbidden := range []string{
		"status text NOT NULL CHECK",
		"kind text NOT NULL CHECK",
		"strategy text NOT NULL CHECK",
		"decision text NOT NULL CHECK",
		"outcome text NOT NULL CHECK",
		"operation text NOT NULL CHECK",
		"state text NOT NULL CHECK",
	} {
		if strings.Contains(schema, forbidden) {
			t.Fatalf("schema still uses text enum check %q", forbidden)
		}
	}
}

func TestInitialSchemaKeepsOnlyAppQueryIndexes(t *testing.T) {
	schema := readInitialSchema(t)
	for _, required := range []string{
		"incidents_one_active_per_deployment",
		"incident_evidence_incident_id_collected_at_idx",
		"UNIQUE (incident_id, sequence)",
	} {
		if !strings.Contains(schema, required) {
			t.Fatalf("schema missing required index or constraint %q", required)
		}
	}
	for _, speculative := range []string{
		"policy_decisions_incident_id_idx",
		"action_proposals_identity_expires_at_idx",
		"approvals_action_hash_idx",
		"operation_events_incident_sequence_idx",
		"idempotency_keys_resource_id_idx",
	} {
		if strings.Contains(schema, speculative) {
			t.Fatalf("schema contains speculative index %q", speculative)
		}
	}
}

func TestInitialSchemaEnforcesIncidentScopedBindings(t *testing.T) {
	schema := readInitialSchema(t)
	requiredBindings := []string{
		"policy_decision_id text NOT NULL REFERENCES policy_decisions(id) ON DELETE RESTRICT",
		"FOREIGN KEY (action_id, proposal_hash) REFERENCES action_proposals(action_id, proposal_hash)",
		"FOREIGN KEY (request_hash) REFERENCES action_proposals(proposal_hash)",
		"idempotency_key text NOT NULL UNIQUE REFERENCES idempotency_keys(key)",
	}
	for _, required := range requiredBindings {
		if !strings.Contains(schema, required) {
			t.Fatalf("schema missing required binding %q", required)
		}
	}
	for _, overSpecific := range []string{
		"UNIQUE (id, incident_id)",
		"UNIQUE (incident_id, proposal_hash)",
		"UNIQUE (incident_id, action_id, proposal_hash)",
		"UNIQUE (resource_id, key)",
		"FOREIGN KEY (policy_decision_id, incident_id)",
		"FOREIGN KEY (incident_id, action_id, proposal_hash)",
		"FOREIGN KEY (resource_id, request_hash)",
		"FOREIGN KEY (incident_id, idempotency_key)",
		"FOREIGN KEY (resource_id, result_id)",
	} {
		if strings.Contains(schema, overSpecific) {
			t.Fatalf("schema contains over-specific binding %q", overSpecific)
		}
	}
}

func TestRollbackMutationMigrationPersistsTrustedIdempotencyBinding(t *testing.T) {
	data, err := os.ReadFile("../../../migrations/000002_rollback_mutation_ledger.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	schema := string(data)
	for _, required := range []string{
		"CREATE TYPE rollback_mutation_state AS ENUM",
		"idempotency_keys_key_request_hash_unique UNIQUE (key, request_hash)",
		"CREATE TABLE rollback_mutations",
		"PRIMARY KEY",
		"FOREIGN KEY (idempotency_key, proposal_hash)",
		"REFERENCES idempotency_keys(key, request_hash)",
		"state = 'pending' AND completed_at IS NULL AND already_applied IS NULL",
		"state = 'completed' AND completed_at IS NOT NULL",
	} {
		if !strings.Contains(schema, required) {
			t.Fatalf("rollback mutation migration missing %q", required)
		}
	}
}

func TestA2AMigrationPersistsDispatchTasksAndIdempotency(t *testing.T) {
	data, err := os.ReadFile("../../../migrations/000003_a2a_investigation.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	schema := string(data)
	for _, required := range []string{"CREATE TYPE investigation_task_state AS ENUM", "CREATE TABLE investigation_dispatches", "idempotency_key text NOT NULL UNIQUE", "CREATE TABLE a2a_tasks", "snapshot jsonb NOT NULL", "CREATE TABLE a2a_investigation_requests", "request_hash text NOT NULL", "task_id text NOT NULL UNIQUE"} {
		if !strings.Contains(schema, required) {
			t.Fatalf("A2A migration missing %q", required)
		}
	}
}

func TestAIDiagnosisMigrationPersistsValidatedProvenance(t *testing.T) {
	data, err := os.ReadFile("../../../migrations/000004_ai_diagnosis.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	schema := string(data)
	for _, required := range []string{
		"'llm'",
		"'hybrid'",
		"CREATE TYPE diagnosis_fallback_reason AS ENUM",
		"requested_strategy diagnosis_strategy NOT NULL",
		"input_tokens bigint NOT NULL",
		"output_tokens bigint NOT NULL",
		"investigation_results_provenance_check",
		"requested_strategy IN ('llm', 'hybrid')",
		"requested_strategy = strategy",
	} {
		if !strings.Contains(schema, required) {
			t.Fatalf("AI diagnosis migration missing %q", required)
		}
	}
	if strings.Count(schema, "btrim(model) <> ''") != 2 || strings.Count(schema, "btrim(prompt_version) <> ''") != 2 {
		t.Fatal("Phase 5 provenance constraints must reject whitespace-only model metadata")
	}
}

func TestAuthMigrationStoresOnlyTokenHashes(t *testing.T) {
	data, err := os.ReadFile("../../../migrations/000005_auth_users_sessions.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	schema := string(data)
	for _, required := range []string{
		"github_id bigint NOT NULL UNIQUE",
		"CREATE UNIQUE INDEX users_login_key ON users (lower(login))",
		"role text NOT NULL CHECK (role IN ('viewer', 'operator', 'administrator'))",
		"REFERENCES users(id) ON DELETE CASCADE",
		"CHECK (octet_length(token_hash) = 32)",
		"CONSTRAINT sessions_token_hash_key UNIQUE (token_hash)",
		"CHECK (expires_at > created_at)",
		"sessions_user_id_idx ON sessions (user_id)",
		"sessions_expires_at_idx ON sessions (expires_at)",
	} {
		if !strings.Contains(schema, required) {
			t.Fatalf("auth migration missing %q", required)
		}
	}
	for line := range strings.SplitSeq(schema, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") {
			continue
		}
		if strings.Contains(trimmed, "token ") || strings.HasPrefix(trimmed, "token text") || strings.HasPrefix(trimmed, "access_token") {
			t.Fatalf("auth migration declares a raw token column: %q", trimmed)
		}
	}
}

func readInitialSchema(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("../../../migrations/000001_initial_schema.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
