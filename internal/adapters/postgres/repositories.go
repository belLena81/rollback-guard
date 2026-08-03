package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/jackc/pgx/v5"
)

type incidentRepo struct {
	r *repositories
}

func (repo incidentRepo) Create(incident domain.Incident) error {
	s := incident.Snapshot()
	_, err := repo.r.tx.Exec(repo.r.ctx, `
		INSERT INTO incidents (
			id, cluster, namespace, deployment, service,
			symptom_kind, symptom_observed, symptom_threshold, symptom_unit, symptom_started_at,
			status, version, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
	`, s.ID, s.Target.Cluster(), s.Target.Namespace(), s.Target.Deployment(), s.Target.Service(),
		s.Symptom.Kind(), s.Symptom.Observed(), s.Symptom.Threshold(), s.Symptom.Unit(), s.Symptom.StartedAt(),
		statusText(s.Status), s.Version, s.CreatedAt, s.UpdatedAt)
	return mapError(err, "incident", string(s.ID))
}

func (repo incidentRepo) Get(id domain.IncidentID) (domain.Incident, error) {
	row := repo.r.tx.QueryRow(repo.r.ctx, `
		SELECT id, cluster, namespace, deployment, service,
			symptom_kind, symptom_observed, symptom_threshold, symptom_unit, symptom_started_at,
			status, version, created_at, updated_at
		FROM incidents WHERE id = $1
	`, id)
	return scanIncident(row)
}

func (repo incidentRepo) Update(incident domain.Incident, expected uint64) error {
	s := incident.Snapshot()
	command, err := repo.r.tx.Exec(repo.r.ctx, `
		UPDATE incidents SET
			status = $1,
			version = $2,
			updated_at = $3
		WHERE id = $4 AND version = $5
	`, statusText(s.Status), s.Version, s.UpdatedAt, s.ID, expected)
	if err != nil {
		return mapError(err, "incident", string(s.ID))
	}
	if command.RowsAffected() == 1 {
		return nil
	}
	var actual uint64
	err = repo.r.tx.QueryRow(repo.r.ctx, `SELECT version FROM incidents WHERE id = $1`, s.ID).Scan(&actual)
	if errors.Is(err, pgx.ErrNoRows) {
		return &domain.NotFoundError{Resource: "incident", ID: string(s.ID)}
	}
	if err != nil {
		return err
	}
	return &domain.VersionConflictError{Expected: expected, Actual: actual}
}

type incidentScanner interface {
	Scan(dest ...any) error
}

func scanIncident(row incidentScanner) (domain.Incident, error) {
	var id, cluster, namespace, deployment, service, symptomKind, symptomUnit, statusValue string
	var observed, threshold float64
	var symptomStartedAt, createdAt, updatedAt time.Time
	var version uint64
	if err := row.Scan(&id, &cluster, &namespace, &deployment, &service, &symptomKind, &observed, &threshold, &symptomUnit, &symptomStartedAt, &statusValue, &version, &createdAt, &updatedAt); err != nil {
		return domain.Incident{}, mapError(err, "incident", id)
	}
	target, err := domain.NewDeploymentTarget(cluster, namespace, deployment, service)
	if err != nil {
		return domain.Incident{}, err
	}
	symptom, err := domain.NewSymptom(symptomKind, observed, threshold, symptomUnit, symptomStartedAt)
	if err != nil {
		return domain.Incident{}, err
	}
	status, err := parseStatus(statusValue)
	if err != nil {
		return domain.Incident{}, err
	}
	return domain.RehydrateIncident(domain.IncidentSnapshot{
		ID:        domain.IncidentID(id),
		Target:    target,
		Symptom:   symptom,
		Status:    status,
		Version:   version,
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	})
}

type evidenceRepo struct {
	r *repositories
}

func (repo evidenceRepo) Add(evidence domain.Evidence) error {
	payload, err := marshalEvidencePayload(evidence)
	if err != nil {
		return err
	}
	_, err = repo.r.tx.Exec(repo.r.ctx, `
		INSERT INTO incident_evidence (id, incident_id, kind, collected_at, payload)
		VALUES ($1,$2,$3,$4,$5)
	`, evidence.ID(), evidence.IncidentID(), evidenceKindText(evidence.Kind()), evidence.CollectedAt(), payload)
	return mapError(err, "evidence", string(evidence.ID()))
}

func (repo evidenceRepo) List(id domain.IncidentID) ([]domain.Evidence, error) {
	rows, err := repo.r.tx.Query(repo.r.ctx, `
		SELECT id, incident_id, kind, collected_at, payload
		FROM incident_evidence
		WHERE incident_id = $1
		ORDER BY collected_at, id
	`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []domain.Evidence
	for rows.Next() {
		var evidenceID, incidentID, kindValue string
		var collectedAt time.Time
		var payload []byte
		if err := rows.Scan(&evidenceID, &incidentID, &kindValue, &collectedAt, &payload); err != nil {
			return nil, err
		}
		kind, err := parseEvidenceKind(kindValue)
		if err != nil {
			return nil, err
		}
		evidence, err := unmarshalEvidencePayload(domain.EvidenceID(evidenceID), domain.IncidentID(incidentID), kind, collectedAt, payload)
		if err != nil {
			return nil, err
		}
		result = append(result, evidence)
	}
	return result, rows.Err()
}

type conclusionRepo struct {
	r *repositories
}

func (repo conclusionRepo) Save(conclusion domain.InvestigationConclusion) error {
	s := conclusion.Snapshot()
	_, err := repo.r.tx.Exec(repo.r.ctx, `
		INSERT INTO investigation_results (id, incident_id, strategy, hypothesis, confidence, recommendation, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
	`, s.ID, s.IncidentID, strategyText(s.Strategy), s.Hypothesis, s.Confidence, recommendationText(s.Recommendation), s.CreatedAt)
	if err != nil {
		return mapError(err, "conclusion", string(s.ID))
	}
	for index, evidenceID := range s.EvidenceIDs {
		_, err := repo.r.tx.Exec(repo.r.ctx, `
			INSERT INTO investigation_result_evidence_refs (conclusion_id, evidence_id, position)
			VALUES ($1,$2,$3)
		`, s.ID, evidenceID, index+1)
		if err != nil {
			return mapError(err, "conclusion.evidence_ref", string(evidenceID))
		}
	}
	return nil
}

func (repo conclusionRepo) Get(id domain.IncidentID) (domain.InvestigationConclusion, error) {
	row := repo.r.tx.QueryRow(repo.r.ctx, `
		SELECT id, incident_id, strategy, hypothesis, confidence, recommendation, created_at
		FROM investigation_results WHERE incident_id = $1
	`, id)
	var conclusionID, incidentID, strategyValue, hypothesis, recommendationValue string
	var confidence float64
	var createdAt time.Time
	if err := row.Scan(&conclusionID, &incidentID, &strategyValue, &hypothesis, &confidence, &recommendationValue, &createdAt); err != nil {
		return domain.InvestigationConclusion{}, mapError(err, "conclusion", string(id))
	}
	rows, err := repo.r.tx.Query(repo.r.ctx, `
		SELECT evidence_id FROM investigation_result_evidence_refs
		WHERE conclusion_id = $1
		ORDER BY position
	`, conclusionID)
	if err != nil {
		return domain.InvestigationConclusion{}, err
	}
	defer rows.Close()
	var evidenceIDs []domain.EvidenceID
	for rows.Next() {
		var evidenceID string
		if err := rows.Scan(&evidenceID); err != nil {
			return domain.InvestigationConclusion{}, err
		}
		evidenceIDs = append(evidenceIDs, domain.EvidenceID(evidenceID))
	}
	if err := rows.Err(); err != nil {
		return domain.InvestigationConclusion{}, err
	}
	strategy, err := parseStrategy(strategyValue)
	if err != nil {
		return domain.InvestigationConclusion{}, err
	}
	recommendation, err := parseRecommendation(recommendationValue)
	if err != nil {
		return domain.InvestigationConclusion{}, err
	}
	return domain.RehydrateInvestigationConclusion(domain.InvestigationConclusionSnapshot{
		ID:             domain.ConclusionID(conclusionID),
		IncidentID:     domain.IncidentID(incidentID),
		Strategy:       strategy,
		Hypothesis:     hypothesis,
		Confidence:     confidence,
		EvidenceIDs:    evidenceIDs,
		Recommendation: recommendation,
		CreatedAt:      createdAt,
	})
}

type decisionRepo struct {
	r *repositories
}

func (repo decisionRepo) Save(decision domain.PolicyDecision) error {
	s := decision.Snapshot()
	rules, err := json.Marshal(s.Rules)
	if err != nil {
		return err
	}
	_, err = repo.r.tx.Exec(repo.r.ctx, `
		INSERT INTO policy_decisions (id, incident_id, allowed, rules, decided_at)
		VALUES ($1,$2,$3,$4,$5)
	`, s.ID, s.IncidentID, decision.Allowed(), rules, s.DecidedAt)
	return mapError(err, "policy_decision", string(s.ID))
}

func (repo decisionRepo) Get(id domain.PolicyDecisionID) (domain.PolicyDecision, error) {
	var decisionID, incidentID string
	var rulesData []byte
	var allowed bool
	var decidedAt time.Time
	err := repo.r.tx.QueryRow(repo.r.ctx, `
		SELECT id, incident_id, allowed, rules, decided_at
		FROM policy_decisions WHERE id = $1
	`, id).Scan(&decisionID, &incidentID, &allowed, &rulesData, &decidedAt)
	if err != nil {
		return domain.PolicyDecision{}, mapError(err, "policy decision", string(id))
	}
	var rules []domain.RuleResult
	if err := json.Unmarshal(rulesData, &rules); err != nil {
		return domain.PolicyDecision{}, err
	}
	decision, err := domain.RehydratePolicyDecision(domain.PolicyDecisionSnapshot{
		ID:         domain.PolicyDecisionID(decisionID),
		IncidentID: domain.IncidentID(incidentID),
		Rules:      rules,
		DecidedAt:  decidedAt,
	})
	if err != nil {
		return domain.PolicyDecision{}, err
	}
	if decision.Allowed() != allowed {
		return domain.PolicyDecision{}, &domain.ValidationError{Field: "policy_decision.allowed", Reason: "does not match rules"}
	}
	return decision, nil
}

type proposalRepo struct {
	r *repositories
}

func (repo proposalRepo) Save(proposal domain.RollbackProposal) error {
	s := proposal.Snapshot()
	_, err := repo.r.tx.Exec(repo.r.ctx, `
		INSERT INTO action_proposals (
			action_id, incident_id, policy_decision_id, cluster, namespace, deployment, service,
			current_revision, target_revision, expires_at, proposal_hash
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
	`, s.ActionID, s.IncidentID, s.PolicyDecisionID, s.Target.Cluster(), s.Target.Namespace(), s.Target.Deployment(), s.Target.Service(),
		s.CurrentRevision, s.TargetRevision, s.ExpiresAt, s.Hash)
	return mapError(err, "proposal", string(s.ActionID))
}

func (repo proposalRepo) GetByIncident(id domain.IncidentID) (domain.RollbackProposal, error) {
	var actionID, incidentID, decisionID, cluster, namespace, deployment, service, currentRevision, targetRevision, hash string
	var expiresAt time.Time
	err := repo.r.tx.QueryRow(repo.r.ctx, `
		SELECT action_id, incident_id, policy_decision_id, cluster, namespace, deployment, service,
			current_revision, target_revision, expires_at, proposal_hash
		FROM action_proposals WHERE incident_id = $1
	`, id).Scan(&actionID, &incidentID, &decisionID, &cluster, &namespace, &deployment, &service, &currentRevision, &targetRevision, &expiresAt, &hash)
	if err != nil {
		return domain.RollbackProposal{}, mapError(err, "proposal", string(id))
	}
	target, err := domain.NewDeploymentTarget(cluster, namespace, deployment, service)
	if err != nil {
		return domain.RollbackProposal{}, err
	}
	return domain.RehydrateRollbackProposal(domain.RollbackProposalSnapshot{
		ActionID:         domain.ActionID(actionID),
		IncidentID:       domain.IncidentID(incidentID),
		PolicyDecisionID: domain.PolicyDecisionID(decisionID),
		Target:           target,
		CurrentRevision:  domain.Revision(currentRevision),
		TargetRevision:   domain.Revision(targetRevision),
		ExpiresAt:        expiresAt,
		Hash:             domain.ProposalHash(hash),
	})
}

type approvalRepo struct {
	r *repositories
}

func (repo approvalRepo) Save(approval domain.Approval) error {
	s := approval.Snapshot()
	var incidentID string
	err := repo.r.tx.QueryRow(repo.r.ctx, `
		SELECT incident_id FROM action_proposals WHERE action_id = $1 AND proposal_hash = $2
	`, s.ActionID, s.ProposalHash).Scan(&incidentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return &domain.ApprovalMismatchError{Reason: "approval hash does not match proposal"}
	}
	if err != nil {
		return mapError(err, "proposal action", string(s.ActionID))
	}
	_, err = repo.r.tx.Exec(repo.r.ctx, `
		INSERT INTO approvals (id, incident_id, actor_id, actor_kind, decision, action_id, proposal_hash, decided_at, expires_at, rejection_reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
	`, s.ID, incidentID, s.Actor.ID(), actorKindText(s.Actor.Kind()), approvalDecisionText(s.Decision), s.ActionID, s.ProposalHash, s.DecidedAt, s.ExpiresAt, s.RejectionReason)
	return mapError(err, "approval", string(s.ID))
}

func (repo approvalRepo) GetByIncident(id domain.IncidentID) (domain.Approval, error) {
	var approvalID, actorID, actorKindValue, decisionValue, actionID, hash, reason string
	var decidedAt, expiresAt time.Time
	err := repo.r.tx.QueryRow(repo.r.ctx, `
		SELECT id, actor_id, actor_kind, decision, action_id, proposal_hash, decided_at, expires_at, rejection_reason
		FROM approvals WHERE incident_id = $1
	`, id).Scan(&approvalID, &actorID, &actorKindValue, &decisionValue, &actionID, &hash, &decidedAt, &expiresAt, &reason)
	if err != nil {
		return domain.Approval{}, mapError(err, "approval", string(id))
	}
	actorKind, err := parseActorKind(actorKindValue)
	if err != nil {
		return domain.Approval{}, err
	}
	actor, err := domain.NewActor(domain.ActorID(actorID), actorKind)
	if err != nil {
		return domain.Approval{}, err
	}
	decision, err := parseApprovalDecision(decisionValue)
	if err != nil {
		return domain.Approval{}, err
	}
	return domain.RehydrateApproval(domain.ApprovalSnapshot{
		ID:              domain.ApprovalID(approvalID),
		Actor:           actor,
		Decision:        decision,
		ActionID:        domain.ActionID(actionID),
		ProposalHash:    domain.ProposalHash(hash),
		DecidedAt:       decidedAt,
		ExpiresAt:       expiresAt,
		RejectionReason: reason,
	})
}

type executionRepo struct {
	r *repositories
}

func (repo executionRepo) Save(execution domain.RollbackExecution) error {
	s := execution.Snapshot()
	var currentRevision, targetRevision, proposalHash, requestHash, resourceID string
	var state string
	err := repo.r.tx.QueryRow(repo.r.ctx, `
		SELECT p.current_revision, p.target_revision, p.proposal_hash, k.request_hash, k.resource_id, k.state
		FROM action_proposals p
		JOIN idempotency_keys k ON k.key = $1
		WHERE p.incident_id = $2
	`, s.IdempotencyKey, s.IncidentID).Scan(&currentRevision, &targetRevision, &proposalHash, &requestHash, &resourceID, &state)
	if err != nil {
		return mapError(err, "execution", string(s.ID))
	}
	if state != "pending" || requestHash != proposalHash || resourceID != string(s.IncidentID) || currentRevision != string(s.FromRevision) || targetRevision != string(s.ToRevision) {
		return &domain.ValidationError{Field: "execution", Reason: "does not match pending approved rollback"}
	}
	_, err = repo.r.tx.Exec(repo.r.ctx, `
		INSERT INTO executions (id, incident_id, idempotency_key, from_revision, to_revision, outcome, started_at, completed_at, failure_reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
	`, s.ID, s.IncidentID, s.IdempotencyKey, s.FromRevision, s.ToRevision, executionOutcomeText(s.Outcome), s.StartedAt, s.CompletedAt, s.FailureReason)
	return mapError(err, "execution", string(s.ID))
}

func (repo executionRepo) Get(id domain.ExecutionID) (domain.RollbackExecution, error) {
	return repo.getByQuery(`
		SELECT id, incident_id, idempotency_key, from_revision, to_revision, outcome, started_at, completed_at, failure_reason
		FROM executions WHERE id = $1
	`, string(id), "execution")
}

func (repo executionRepo) GetByIncident(id domain.IncidentID) (domain.RollbackExecution, error) {
	return repo.getByQuery(`
		SELECT id, incident_id, idempotency_key, from_revision, to_revision, outcome, started_at, completed_at, failure_reason
		FROM executions WHERE incident_id = $1
	`, string(id), "execution")
}

func (repo executionRepo) getByQuery(query string, id string, resource string) (domain.RollbackExecution, error) {
	var executionID, incidentID, key, fromRevision, toRevision, outcomeValue, reason string
	var startedAt, completedAt time.Time
	err := repo.r.tx.QueryRow(repo.r.ctx, query, id).Scan(&executionID, &incidentID, &key, &fromRevision, &toRevision, &outcomeValue, &startedAt, &completedAt, &reason)
	if err != nil {
		return domain.RollbackExecution{}, mapError(err, resource, id)
	}
	outcome, err := parseExecutionOutcome(outcomeValue)
	if err != nil {
		return domain.RollbackExecution{}, err
	}
	return domain.RehydrateRollbackExecution(domain.RollbackExecutionSnapshot{
		ID:             domain.ExecutionID(executionID),
		IncidentID:     domain.IncidentID(incidentID),
		IdempotencyKey: domain.IdempotencyKey(key),
		FromRevision:   domain.Revision(fromRevision),
		ToRevision:     domain.Revision(toRevision),
		Outcome:        outcome,
		StartedAt:      startedAt,
		CompletedAt:    completedAt,
		FailureReason:  reason,
	})
}

type verificationRepo struct {
	r *repositories
}

func (repo verificationRepo) Save(verification domain.RecoveryVerification) error {
	s := verification.Snapshot()
	_, err := repo.r.tx.Exec(repo.r.ctx, `
		INSERT INTO recovery_verifications (
			incident_id, execution_id, passed, revision, desired_replicas, ready_replicas,
			available_replicas, metric_value, checked_at, failure_reason
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
	`, s.IncidentID, s.ExecutionID, s.Passed, s.Revision, s.DesiredReplicas, s.ReadyReplicas, s.AvailableReplicas, s.MetricValue, s.CheckedAt, s.FailureReason)
	return mapError(err, "verification", string(s.IncidentID))
}

func (repo verificationRepo) GetByIncident(id domain.IncidentID) (domain.RecoveryVerification, error) {
	var incidentID, executionID, revision, reason string
	var passed bool
	var desired, ready, available int
	var metricValue float64
	var checkedAt time.Time
	err := repo.r.tx.QueryRow(repo.r.ctx, `
		SELECT incident_id, execution_id, passed, revision, desired_replicas, ready_replicas,
			available_replicas, metric_value, checked_at, failure_reason
		FROM recovery_verifications WHERE incident_id = $1
	`, id).Scan(&incidentID, &executionID, &passed, &revision, &desired, &ready, &available, &metricValue, &checkedAt, &reason)
	if err != nil {
		return domain.RecoveryVerification{}, mapError(err, "verification", string(id))
	}
	return domain.RehydrateRecoveryVerification(domain.RecoveryVerificationSnapshot{
		IncidentID:        domain.IncidentID(incidentID),
		ExecutionID:       domain.ExecutionID(executionID),
		Passed:            passed,
		Revision:          domain.Revision(revision),
		DesiredReplicas:   desired,
		ReadyReplicas:     ready,
		AvailableReplicas: available,
		MetricValue:       metricValue,
		CheckedAt:         checkedAt,
		FailureReason:     reason,
	})
}

type idempotencyRepo struct {
	r *repositories
}

func (repo idempotencyRepo) Get(key domain.IdempotencyKey) (domain.IdempotencyRecord, error) {
	var operationValue, requestHash, resourceID, stateValue string
	var startedAt time.Time
	var resultID sql.NullString
	var completedAt sql.NullTime
	err := repo.r.tx.QueryRow(repo.r.ctx, `
		SELECT operation, request_hash, resource_id, state, started_at, result_id, completed_at
		FROM idempotency_keys WHERE key = $1
	`, key).Scan(&operationValue, &requestHash, &resourceID, &stateValue, &startedAt, &resultID, &completedAt)
	if err != nil {
		return domain.IdempotencyRecord{}, mapError(err, "idempotency key", string(key))
	}
	operation, err := parseIdempotencyOperation(operationValue)
	if err != nil {
		return domain.IdempotencyRecord{}, err
	}
	state, err := parseIdempotencyState(stateValue)
	if err != nil {
		return domain.IdempotencyRecord{}, err
	}
	snapshot := domain.IdempotencyRecordSnapshot{
		Key:         key,
		Operation:   operation,
		RequestHash: requestHash,
		ResourceID:  resourceID,
		State:       state,
		StartedAt:   startedAt,
	}
	if resultID.Valid {
		snapshot.ResultID = domain.ExecutionID(resultID.String)
	}
	if completedAt.Valid {
		snapshot.CompletedAt = completedAt.Time
	}
	return domain.RehydrateIdempotencyRecord(snapshot)
}

func (repo idempotencyRepo) Create(record domain.IdempotencyRecord) error {
	s := record.Snapshot()
	_, err := repo.r.tx.Exec(repo.r.ctx, `
		INSERT INTO idempotency_keys (key, operation, request_hash, resource_id, state, started_at)
		VALUES ($1,$2,$3,$4,$5,$6)
	`, s.Key, idempotencyOperationText(s.Operation), s.RequestHash, s.ResourceID, idempotencyStateText(s.State), s.StartedAt)
	if err != nil {
		return &domain.IdempotencyConflictError{Key: s.Key}
	}
	return nil
}

func (repo idempotencyRepo) Update(record domain.IdempotencyRecord, expected domain.IdempotencyState) error {
	s := record.Snapshot()
	command, err := repo.r.tx.Exec(repo.r.ctx, `
		UPDATE idempotency_keys
		SET state = $1, result_id = $2, completed_at = $3
		WHERE key = $4 AND operation = $5 AND request_hash = $6 AND resource_id = $7 AND state = $8
	`, idempotencyStateText(s.State), s.ResultID, s.CompletedAt, s.Key, idempotencyOperationText(s.Operation), s.RequestHash, s.ResourceID, idempotencyStateText(expected))
	if err != nil {
		return mapError(err, "idempotency key", string(s.Key))
	}
	if command.RowsAffected() != 1 {
		return &domain.IdempotencyConflictError{Key: s.Key}
	}
	return nil
}

type eventRepo struct {
	r *repositories
}

func (repo eventRepo) NextSequence(_ context.Context, id domain.IncidentID) (uint64, error) {
	var exists string
	if err := repo.r.tx.QueryRow(repo.r.ctx, `SELECT id FROM incidents WHERE id = $1 FOR UPDATE`, id).Scan(&exists); err != nil {
		return 0, mapError(err, "incident", string(id))
	}
	var sequence uint64
	err := repo.r.tx.QueryRow(repo.r.ctx, `
		SELECT COALESCE(MAX(sequence), 0) + 1 FROM operation_events WHERE incident_id = $1
	`, id).Scan(&sequence)
	return sequence, err
}

func (repo eventRepo) Append(event domain.OperationEvent) error {
	payload, err := marshalEventDetail(event.Detail())
	if err != nil {
		return err
	}
	_, err = repo.r.tx.Exec(repo.r.ctx, `
		INSERT INTO operation_events (id, incident_id, sequence, kind, occurred_at, actor_id, actor_kind, detail)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
	`, event.ID(), event.IncidentID(), event.Sequence(), eventKindText(event.Kind()), event.OccurredAt(), event.Actor().ID(), actorKindText(event.Actor().Kind()), payload)
	if err == nil {
		return nil
	}
	var max uint64
	maxErr := repo.r.tx.QueryRow(repo.r.ctx, `SELECT COALESCE(MAX(sequence), 0) FROM operation_events WHERE incident_id = $1`, event.IncidentID()).Scan(&max)
	if maxErr == nil && event.Sequence() != max+1 {
		return &domain.VersionConflictError{Expected: max + 1, Actual: event.Sequence()}
	}
	return mapError(err, "event", string(event.ID()))
}

func (repo eventRepo) List(id domain.IncidentID) ([]domain.OperationEvent, error) {
	rows, err := repo.r.tx.Query(repo.r.ctx, `
		SELECT id, incident_id, sequence, kind, occurred_at, actor_id, actor_kind, detail
		FROM operation_events
		WHERE incident_id = $1
		ORDER BY sequence
	`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []domain.OperationEvent
	for rows.Next() {
		var eventID, incidentID, kindValue, actorID, actorKindValue string
		var sequence uint64
		var occurredAt time.Time
		var payload []byte
		if err := rows.Scan(&eventID, &incidentID, &sequence, &kindValue, &occurredAt, &actorID, &actorKindValue, &payload); err != nil {
			return nil, err
		}
		kind, err := parseEventKind(kindValue)
		if err != nil {
			return nil, err
		}
		actorKind, err := parseActorKind(actorKindValue)
		if err != nil {
			return nil, err
		}
		actor, err := domain.NewActor(domain.ActorID(actorID), actorKind)
		if err != nil {
			return nil, err
		}
		detail, err := unmarshalEventDetail(kind, payload)
		if err != nil {
			return nil, err
		}
		event, err := domain.RehydrateOperationEvent(domain.OperationEventSnapshot{
			ID:         domain.EventID(eventID),
			IncidentID: domain.IncidentID(incidentID),
			Sequence:   sequence,
			Kind:       kind,
			OccurredAt: occurredAt,
			Actor:      actor,
			Detail:     detail,
		})
		if err != nil {
			return nil, err
		}
		result = append(result, event)
	}
	return result, rows.Err()
}
