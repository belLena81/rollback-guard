package a2aadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/belLena81/rollback-guard/internal/ports"
)

type PostgresTaskStore struct {
	pool  *pgxpool.Pool
	clock ports.Clock
}

func NewPostgresTaskStore(pool *pgxpool.Pool, clock ports.Clock) (*PostgresTaskStore, error) {
	if pool == nil || clock == nil {
		return nil, fmt.Errorf("PostgreSQL pool and clock are required")
	}
	return &PostgresTaskStore{pool: pool, clock: clock}, nil
}

func (s *PostgresTaskStore) Create(ctx context.Context, task *a2a.Task) (taskstore.TaskVersion, error) {
	if task == nil || task.ID == "" || task.ContextID == "" {
		return taskstore.TaskVersionMissing, fmt.Errorf("invalid task: %w", a2a.ErrInvalidRequest)
	}
	snapshot, err := json.Marshal(task)
	if err != nil {
		return 0, err
	}
	now := s.clock.Now().UTC()
	const version taskstore.TaskVersion = 1
	_, err = s.pool.Exec(ctx, `INSERT INTO a2a_tasks(task_id,context_id,state,version,snapshot,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$6)`, task.ID, task.ContextID, task.Status.State.String(), version, snapshot, now)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return 0, taskstore.ErrTaskAlreadyExists
	}
	if err != nil {
		return 0, fmt.Errorf("create A2A task: %w", err)
	}
	return version, nil
}

func (s *PostgresTaskStore) Update(ctx context.Context, request *taskstore.UpdateRequest) (taskstore.TaskVersion, error) {
	if request == nil || request.Task == nil {
		return 0, fmt.Errorf("invalid task update: %w", a2a.ErrInvalidRequest)
	}
	snapshot, err := json.Marshal(request.Task)
	if err != nil {
		return 0, err
	}
	expected := request.PrevVersion
	if expected == taskstore.TaskVersionMissing {
		stored, getErr := s.Get(ctx, request.Task.ID)
		if getErr != nil {
			return 0, getErr
		}
		expected = stored.Version
	}
	next := expected + 1
	command, err := s.pool.Exec(ctx, `UPDATE a2a_tasks SET context_id=$1,state=$2,version=$3,snapshot=$4,updated_at=$5 WHERE task_id=$6 AND version=$7`, request.Task.ContextID, request.Task.Status.State.String(), next, snapshot, s.clock.Now().UTC(), request.Task.ID, expected)
	if err != nil {
		return 0, fmt.Errorf("update A2A task: %w", err)
	}
	if command.RowsAffected() == 1 {
		return next, nil
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT true FROM a2a_tasks WHERE task_id=$1`, request.Task.ID).Scan(&exists); errors.Is(err, pgx.ErrNoRows) {
		return 0, a2a.ErrTaskNotFound
	} else if err != nil {
		return 0, err
	}
	return 0, taskstore.ErrConcurrentModification
}

func (s *PostgresTaskStore) Get(ctx context.Context, id a2a.TaskID) (*taskstore.StoredTask, error) {
	var snapshot []byte
	var version taskstore.TaskVersion
	err := s.pool.QueryRow(ctx, `SELECT snapshot,version FROM a2a_tasks WHERE task_id=$1`, id).Scan(&snapshot, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, a2a.ErrTaskNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get A2A task: %w", err)
	}
	var task a2a.Task
	if err := json.Unmarshal(snapshot, &task); err != nil {
		return nil, fmt.Errorf("decode A2A task: %w", err)
	}
	return &taskstore.StoredTask{Task: &task, Version: version}, nil
}

func (s *PostgresTaskStore) List(ctx context.Context, request *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	if request == nil {
		return nil, fmt.Errorf("list request is required: %w", a2a.ErrInvalidRequest)
	}
	if request.PageToken != "" {
		return nil, fmt.Errorf("page tokens are not supported: %w", a2a.ErrInvalidRequest)
	}
	pageSize := request.PageSize
	if pageSize == 0 {
		pageSize = 50
	}
	if pageSize < 1 || pageSize > 100 {
		return nil, fmt.Errorf("page size must be between 1 and 100: %w", a2a.ErrInvalidRequest)
	}
	state := ""
	if request.Status != a2a.TaskStateUnspecified {
		state = request.Status.String()
	}
	rows, err := s.pool.Query(ctx, `SELECT snapshot,count(*) OVER() FROM a2a_tasks WHERE ($1='' OR context_id=$1) AND ($2='' OR state=$2) AND ($3::timestamptz IS NULL OR updated_at>$3) ORDER BY updated_at DESC,task_id DESC LIMIT $4`, request.ContextID, state, request.StatusTimestampAfter, pageSize)
	if err != nil {
		return nil, err
	}
	total := 0
	tasks, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*a2a.Task, error) {
		var snapshot []byte
		if err := row.Scan(&snapshot, &total); err != nil {
			return nil, err
		}
		var task a2a.Task
		if err := json.Unmarshal(snapshot, &task); err != nil {
			return nil, err
		}
		if !request.IncludeArtifacts {
			task.Artifacts = nil
		}
		return &task, nil
	})
	if err != nil {
		return nil, err
	}
	return &a2a.ListTasksResponse{Tasks: tasks, TotalSize: total, PageSize: pageSize}, nil
}

var _ taskstore.Store = (*PostgresTaskStore)(nil)
