package postgres

import (
	"errors"
	"fmt"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
	"github.com/jackc/pgx/v5"
)

type investigationTaskRepo struct {
	r *repositories
}

func (repo investigationTaskRepo) Create(task ports.InvestigationDispatch) error {
	_, err := repo.r.tx.Exec(repo.r.ctx, `
		INSERT INTO investigation_dispatches (
			incident_id, operation_id, idempotency_key, message_id, trace_id, request_hash,
			task_id, context_id, state, stage, attempts, lease_owner, lease_expires_at,
			failure_code, version, created_at, updated_at, completed_at
		) VALUES ($1,$2,$3,$4,$5,$6,NULL,NULL,$7,$8,$9,NULL,NULL,'',$10,$11,$12,NULL)
	`, task.IncidentID, task.OperationID, task.IdempotencyKey, task.MessageID, task.TraceID, task.RequestHash,
		investigationTaskStateText(task.State), investigationStageText(task.Stage), task.Attempts,
		task.Version, task.CreatedAt, task.UpdatedAt)
	return mapError(err, "investigation", string(task.IncidentID))
}

func (repo investigationTaskRepo) GetByIncident(id domain.IncidentID) (ports.InvestigationDispatch, error) {
	return scanInvestigationDispatch(repo.r.tx.QueryRow(repo.r.ctx, investigationDispatchSelect+` WHERE incident_id = $1`, id))
}

func (repo investigationTaskRepo) ClaimNext(now time.Time, owner string, leaseUntil time.Time) (ports.InvestigationDispatch, error) {
	row := repo.r.tx.QueryRow(repo.r.ctx, `
		WITH candidate AS (
			SELECT incident_id
			FROM investigation_dispatches
			WHERE state = 'queued'
			   OR (state = 'cancel_requested' AND (lease_expires_at IS NULL OR lease_expires_at <= $1))
			   OR (state IN ('submitting', 'submitted', 'working') AND lease_expires_at <= $1)
			ORDER BY created_at, incident_id
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		UPDATE investigation_dispatches AS task
		SET state = CASE WHEN task.state = 'cancel_requested' OR task.task_id IS NOT NULL THEN task.state ELSE 'submitting' END,
			attempts = task.attempts + 1,
			lease_owner = $2,
			lease_expires_at = $3,
			version = task.version + 1,
			updated_at = $1
		FROM candidate
		WHERE task.incident_id = candidate.incident_id
		RETURNING task.incident_id, task.operation_id, task.idempotency_key, task.message_id,
			task.trace_id, task.request_hash, task.task_id, task.context_id, task.state::text,
			task.stage::text, task.attempts, task.lease_owner, task.lease_expires_at,
			task.failure_code, task.version, task.created_at, task.updated_at, task.completed_at
	`, now, owner, leaseUntil)
	return scanInvestigationDispatch(row)
}

func (repo investigationTaskRepo) Update(task ports.InvestigationDispatch, expected uint64) error {
	command, err := repo.r.tx.Exec(repo.r.ctx, `
		UPDATE investigation_dispatches SET
			task_id = NULLIF($1, ''), context_id = NULLIF($2, ''), state = $3, stage = $4,
			attempts = $5, lease_owner = NULLIF($6, ''), lease_expires_at = $7,
			failure_code = $8, version = $9, updated_at = $10, completed_at = $11
		WHERE incident_id = $12 AND version = $13
	`, task.Ref.TaskID, task.Ref.ContextID, investigationTaskStateText(task.State), investigationStageText(task.Stage),
		task.Attempts, task.LeaseOwner, nullableTime(task.LeaseExpiresAt), task.FailureCode, task.Version,
		task.UpdatedAt, nullableTime(task.CompletedAt), task.IncidentID, expected)
	if err != nil {
		return mapError(err, "investigation", string(task.IncidentID))
	}
	if command.RowsAffected() == 1 {
		return nil
	}
	var actual uint64
	err = repo.r.tx.QueryRow(repo.r.ctx, `SELECT version FROM investigation_dispatches WHERE incident_id = $1`, task.IncidentID).Scan(&actual)
	if errors.Is(err, pgx.ErrNoRows) {
		return &domain.NotFoundError{Resource: "investigation", ID: string(task.IncidentID)}
	}
	if err != nil {
		return err
	}
	return &domain.VersionConflictError{Expected: expected, Actual: actual}
}

const investigationDispatchSelect = `
	SELECT incident_id, operation_id, idempotency_key, message_id, trace_id, request_hash,
		task_id, context_id, state::text, stage::text, attempts, lease_owner, lease_expires_at,
		failure_code, version, created_at, updated_at, completed_at
	FROM investigation_dispatches`

type investigationDispatchScanner interface {
	Scan(...any) error
}

func scanInvestigationDispatch(row investigationDispatchScanner) (ports.InvestigationDispatch, error) {
	var task ports.InvestigationDispatch
	var taskID, contextID, leaseOwner *string
	var stateValue, stageValue string
	var leaseExpiresAt, completedAt *time.Time
	err := row.Scan(&task.IncidentID, &task.OperationID, &task.IdempotencyKey, &task.MessageID, &task.TraceID,
		&task.RequestHash, &taskID, &contextID, &stateValue, &stageValue, &task.Attempts, &leaseOwner,
		&leaseExpiresAt, &task.FailureCode, &task.Version, &task.CreatedAt, &task.UpdatedAt, &completedAt)
	if err != nil {
		return ports.InvestigationDispatch{}, mapError(err, "investigation", string(task.IncidentID))
	}
	state, err := parseInvestigationTaskState(stateValue)
	if err != nil {
		return ports.InvestigationDispatch{}, err
	}
	stage, err := parseInvestigationStage(stageValue)
	if err != nil {
		return ports.InvestigationDispatch{}, err
	}
	task.State = state
	task.Stage = stage
	if taskID != nil {
		task.Ref.TaskID = *taskID
	}
	if contextID != nil {
		task.Ref.ContextID = *contextID
	}
	if leaseOwner != nil {
		task.LeaseOwner = *leaseOwner
	}
	if leaseExpiresAt != nil {
		task.LeaseExpiresAt = *leaseExpiresAt
	}
	if completedAt != nil {
		task.CompletedAt = *completedAt
	}
	return task, nil
}

func investigationTaskStateText(state ports.InvestigationTaskState) string {
	return state.String()
}

func parseInvestigationTaskState(value string) (ports.InvestigationTaskState, error) {
	for state := ports.InvestigationTaskQueued; state <= ports.InvestigationTaskCanceled; state++ {
		if state.String() == value {
			return state, nil
		}
	}
	return 0, fmt.Errorf("unknown investigation task state %q", value)
}

func investigationStageText(stage ports.InvestigationStage) string {
	return stage.String()
}

func parseInvestigationStage(value string) (ports.InvestigationStage, error) {
	for stage := ports.InvestigationStageQueued; stage <= ports.InvestigationStageFinalizing; stage++ {
		if stage.String() == value {
			return stage, nil
		}
	}
	return 0, fmt.Errorf("unknown investigation stage %q", value)
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}
