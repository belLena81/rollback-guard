package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// InvestigationRequestLedger is the durable idempotency record for A2A
// investigation requests (table a2a_investigation_requests). It speaks in
// plain values; the A2A adapter owns the interface it consumes.
type InvestigationRequestLedger struct {
	pool *pgxpool.Pool
}

func NewInvestigationRequestLedger(pool *pgxpool.Pool) (*InvestigationRequestLedger, error) {
	if pool == nil {
		return nil, errors.New("PostgreSQL pool is required")
	}
	return &InvestigationRequestLedger{pool: pool}, nil
}

// Claim records a new working request. It reports false when the key exists.
func (l *InvestigationRequestLedger) Claim(ctx context.Context, key, requestHash, taskID string, now time.Time) (bool, error) {
	command, err := l.pool.Exec(
		ctx,
		`INSERT INTO a2a_investigation_requests(idempotency_key,request_hash,task_id,state,created_at,updated_at)
		VALUES($1,$2,$3,'working',$4,$4) ON CONFLICT DO NOTHING`,
		key,
		requestHash,
		taskID,
		now,
	)
	if err != nil {
		return false, fmt.Errorf("write a2a investigation record: %w", err)
	}
	return command.RowsAffected() == 1, nil
}

// Reclaim takes over a working request last updated before staleBefore. It
// reports false when the request is not reclaimable.
func (l *InvestigationRequestLedger) Reclaim(ctx context.Context, key, requestHash, taskID string, now, staleBefore time.Time) (bool, error) {
	command, err := l.pool.Exec(
		ctx,
		`UPDATE a2a_investigation_requests SET task_id=$5,updated_at=$3 WHERE idempotency_key=$1 AND request_hash=$2 AND state='working' AND updated_at<$4`,
		key,
		requestHash,
		now,
		staleBefore,
		taskID,
	)
	if err != nil {
		return false, fmt.Errorf("write a2a investigation record: %w", err)
	}
	return command.RowsAffected() == 1, nil
}

// Fail moves a request owned by taskID to a terminal failed or canceled
// state. It reports false when the caller no longer owns the request.
func (l *InvestigationRequestLedger) Fail(ctx context.Context, key, requestHash, taskID, state, failureCode string, now time.Time) (bool, error) {
	command, err := l.pool.Exec(
		ctx,
		`UPDATE a2a_investigation_requests SET state=$2,failure_code=$3,updated_at=$4
		WHERE idempotency_key=$1 AND request_hash=$5 AND task_id=$6 AND state='working'`,
		key,
		state,
		failureCode,
		now,
		requestHash,
		taskID,
	)
	if err != nil {
		return false, fmt.Errorf("write a2a investigation record: %w", err)
	}
	return command.RowsAffected() == 1, nil
}

// Complete stores the result of a request owned by taskID. It reports false
// when the caller no longer owns the request.
func (l *InvestigationRequestLedger) Complete(ctx context.Context, key, requestHash, taskID string, result []byte, now time.Time) (bool, error) {
	command, err := l.pool.Exec(
		ctx,
		`UPDATE a2a_investigation_requests SET state='completed',result=$2,updated_at=$3
		WHERE idempotency_key=$1 AND request_hash=$4 AND task_id=$5 AND state='working'`,
		key,
		result,
		now,
		requestHash,
		taskID,
	)
	if err != nil {
		return false, fmt.Errorf("write a2a investigation record: %w", err)
	}
	return command.RowsAffected() == 1, nil
}

// Lookup reads the record for key. found is false when none exists.
func (l *InvestigationRequestLedger) Lookup(ctx context.Context, key string) (requestHash, state, failureCode string, result []byte, found bool, err error) {
	err = l.pool.QueryRow(ctx, `SELECT request_hash,state::text,failure_code,result FROM a2a_investigation_requests WHERE idempotency_key=$1`, key).
		Scan(&requestHash, &state, &failureCode, &result)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", "", nil, false, nil
	}
	if err != nil {
		return "", "", "", nil, false, fmt.Errorf("read a2a investigation record: %w", err)
	}
	return requestHash, state, failureCode, result, true, nil
}

// A2ATaskRecords stores serialized A2A task snapshots (table a2a_tasks) with
// optimistic versioning. The A2A adapter owns the SDK types and the JSON.
type A2ATaskRecords struct {
	pool *pgxpool.Pool
}

func NewA2ATaskRecords(pool *pgxpool.Pool) (*A2ATaskRecords, error) {
	if pool == nil {
		return nil, errors.New("PostgreSQL pool is required")
	}
	return &A2ATaskRecords{pool: pool}, nil
}

// Insert stores a new task at the given version. It reports false when the
// task already exists.
func (r *A2ATaskRecords) Insert(ctx context.Context, taskID, contextID, state string, version int64, snapshot []byte, now time.Time) (bool, error) {
	_, err := r.pool.Exec(
		ctx,
		`INSERT INTO a2a_tasks(task_id,context_id,state,version,snapshot,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$6)`,
		taskID,
		contextID,
		state,
		version,
		snapshot,
		now,
	)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("insert a2a task: %w", err)
	}
	return true, nil
}

// Update replaces the snapshot when the stored version equals expected and
// moves it to next. When nothing changed, exists tells a missing task from a
// version conflict.
func (r *A2ATaskRecords) Update(
	ctx context.Context,
	taskID, contextID, state string,
	expected, next int64,
	snapshot []byte,
	now time.Time,
) (updated, exists bool, err error) {
	command, err := r.pool.Exec(
		ctx,
		`UPDATE a2a_tasks SET context_id=$1,state=$2,version=$3,snapshot=$4,updated_at=$5 WHERE task_id=$6 AND version=$7`,
		contextID,
		state,
		next,
		snapshot,
		now,
		taskID,
		expected,
	)
	if err != nil {
		return false, false, fmt.Errorf("write a2a investigation record: %w", err)
	}
	if command.RowsAffected() == 1 {
		return true, true, nil
	}
	err = r.pool.QueryRow(ctx, `SELECT true FROM a2a_tasks WHERE task_id=$1`, taskID).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, exists, fmt.Errorf("check a2a task: %w", err)
	}
	return false, exists, nil
}

// Get reads one snapshot and its version. found is false when none exists.
func (r *A2ATaskRecords) Get(ctx context.Context, taskID string) (snapshot []byte, version int64, found bool, err error) {
	err = r.pool.QueryRow(ctx, `SELECT snapshot,version FROM a2a_tasks WHERE task_id=$1`, taskID).Scan(&snapshot, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, fmt.Errorf("read a2a investigation record: %w", err)
	}
	return snapshot, version, true, nil
}

// List returns the newest snapshots first, filtered by context, state and
// last update, together with the unpaged match count.
func (r *A2ATaskRecords) List(ctx context.Context, contextID, state string, updatedAfter *time.Time, limit int) (snapshots [][]byte, total int, err error) {
	rows, err := r.pool.Query(
		ctx,
		`SELECT snapshot,count(*) OVER() FROM a2a_tasks
		WHERE ($1='' OR context_id=$1) AND ($2='' OR state=$2) AND ($3::timestamptz IS NULL OR updated_at>$3)
		ORDER BY updated_at DESC,task_id DESC LIMIT $4`,
		contextID,
		state,
		updatedAfter,
		limit,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("list a2a tasks: %w", err)
	}
	snapshots, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) ([]byte, error) {
		var snapshot []byte
		if err := row.Scan(&snapshot, &total); err != nil {
			return nil, fmt.Errorf("scan a2a task: %w", err)
		}
		return snapshot, nil
	})
	if err != nil {
		return nil, 0, fmt.Errorf("collect a2a tasks: %w", err)
	}
	return snapshots, total, nil
}
