package a2aadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/belLena81/rollback-guard/internal/ports"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const terminalPersistenceTimeout = 5 * time.Second

type IdempotentInvestigator struct {
	pool         *pgxpool.Pool
	next         ports.Investigator
	pollInterval time.Duration
	staleAfter   time.Duration
}

func NewIdempotentInvestigator(pool *pgxpool.Pool, next ports.Investigator) (*IdempotentInvestigator, error) {
	if pool == nil || next == nil {
		return nil, fmt.Errorf("PostgreSQL pool and investigator are required")
	}
	return &IdempotentInvestigator{pool: pool, next: next, pollInterval: 100 * time.Millisecond, staleAfter: 2 * time.Minute}, nil
}

func (i *IdempotentInvestigator) Investigate(ctx context.Context, request ports.InvestigationRequest) (ports.InvestigationResult, error) {
	if request.IdempotencyKey == "" || request.RequestHash == "" {
		return ports.InvestigationResult{}, fmt.Errorf("idempotency key and request hash are required")
	}
	command, err := i.pool.Exec(ctx, `INSERT INTO a2a_investigation_requests(idempotency_key,request_hash,task_id,state,created_at,updated_at) VALUES($1,$2,$3,'working',$4,$4) ON CONFLICT DO NOTHING`, request.IdempotencyKey, request.RequestHash, request.TaskID, time.Now().UTC())
	if err != nil {
		return ports.InvestigationResult{}, fmt.Errorf("claim investigation request: %w", err)
	}
	if command.RowsAffected() == 0 {
		reclaimed, reclaimErr := i.pool.Exec(ctx, `UPDATE a2a_investigation_requests SET task_id=$5,updated_at=$3 WHERE idempotency_key=$1 AND request_hash=$2 AND state='working' AND updated_at<$4`, request.IdempotencyKey, request.RequestHash, time.Now().UTC(), time.Now().UTC().Add(-i.staleAfter), request.TaskID)
		if reclaimErr != nil {
			return ports.InvestigationResult{}, reclaimErr
		}
		if reclaimed.RowsAffected() == 0 {
			return i.waitForResult(ctx, request)
		}
	}
	result, err := i.next.Investigate(ctx, request)
	if err != nil {
		state, failure := "failed", "investigation_failed"
		if errors.Is(err, context.Canceled) {
			state, failure = "canceled", ""
		}
		recordCtx, cancel := terminalPersistenceContext(ctx)
		defer cancel()
		completed, recordErr := i.pool.Exec(recordCtx, `UPDATE a2a_investigation_requests SET state=$2,failure_code=$3,updated_at=$4 WHERE idempotency_key=$1 AND request_hash=$5 AND task_id=$6 AND state='working'`, request.IdempotencyKey, state, failure, time.Now().UTC(), request.RequestHash, request.TaskID)
		if recordErr == nil && completed.RowsAffected() != 1 {
			recordErr = fmt.Errorf("investigation request ownership was lost before recording %s", state)
		}
		return ports.InvestigationResult{}, errors.Join(err, recordErr)
	}
	snapshot, err := json.Marshal(encodeResult(request.Incident.ID(), result))
	if err != nil {
		return ports.InvestigationResult{}, err
	}
	recordCtx, cancel := terminalPersistenceContext(ctx)
	defer cancel()
	completed, err := i.pool.Exec(recordCtx, `UPDATE a2a_investigation_requests SET state='completed',result=$2,updated_at=$3 WHERE idempotency_key=$1 AND request_hash=$4 AND task_id=$5 AND state='working'`, request.IdempotencyKey, snapshot, time.Now().UTC(), request.RequestHash, request.TaskID)
	if err != nil {
		return ports.InvestigationResult{}, fmt.Errorf("complete investigation request: %w", err)
	}
	if completed.RowsAffected() != 1 {
		return ports.InvestigationResult{}, fmt.Errorf("investigation request ownership was lost before completion")
	}
	return result, nil
}

func terminalPersistenceContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), terminalPersistenceTimeout)
}

func (i *IdempotentInvestigator) waitForResult(ctx context.Context, request ports.InvestigationRequest) (ports.InvestigationResult, error) {
	ticker := time.NewTicker(i.pollInterval)
	defer ticker.Stop()
	for {
		var hash, state, failure string
		var result []byte
		err := i.pool.QueryRow(ctx, `SELECT request_hash,state::text,failure_code,result FROM a2a_investigation_requests WHERE idempotency_key=$1`, request.IdempotencyKey).Scan(&hash, &state, &failure, &result)
		if errors.Is(err, pgx.ErrNoRows) {
			return ports.InvestigationResult{}, fmt.Errorf("investigation idempotency record disappeared")
		}
		if err != nil {
			return ports.InvestigationResult{}, err
		}
		if hash != request.RequestHash {
			return ports.InvestigationResult{}, fmt.Errorf("idempotency key reused with a different request")
		}
		switch state {
		case "completed":
			var wire resultDTO
			if err := json.Unmarshal(result, &wire); err != nil {
				return ports.InvestigationResult{}, err
			}
			return decodeResult(wire)
		case "failed", "canceled":
			return ports.InvestigationResult{}, fmt.Errorf("investigation request is %s: %s", state, failure)
		}
		select {
		case <-ctx.Done():
			return ports.InvestigationResult{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

var _ ports.Investigator = (*IdempotentInvestigator)(nil)
