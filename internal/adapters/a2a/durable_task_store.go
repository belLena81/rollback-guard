package a2aadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"

	"github.com/belLena81/rollback-guard/internal/ports"
)

// TaskRecords persists serialized task snapshots with optimistic versions.
// It carries no SDK types: this adapter owns the JSON encoding.
type TaskRecords interface {
	// Insert stores a new task. It reports false when the task exists.
	Insert(ctx context.Context, taskID, contextID, state string, version int64, snapshot []byte, now time.Time) (created bool, err error)
	// Update replaces the snapshot when the stored version equals expected.
	// When nothing changed, exists tells a missing task from a conflict.
	Update(ctx context.Context, taskID, contextID, state string, expected, next int64, snapshot []byte, now time.Time) (updated, exists bool, err error)
	Get(ctx context.Context, taskID string) (snapshot []byte, version int64, found bool, err error)
	// List returns snapshots newest first, plus the unpaged match count.
	List(ctx context.Context, contextID, state string, updatedAfter *time.Time, limit int) (snapshots [][]byte, total int, err error)
}

// DurableTaskStore is the A2A task store backed by TaskRecords.
type DurableTaskStore struct {
	records TaskRecords
	clock   ports.Clock
}

func NewDurableTaskStore(records TaskRecords, clock ports.Clock) (*DurableTaskStore, error) {
	if records == nil || clock == nil {
		return nil, fmt.Errorf("task records and clock are required")
	}
	return &DurableTaskStore{records: records, clock: clock}, nil
}

func (s *DurableTaskStore) Create(ctx context.Context, task *a2a.Task) (taskstore.TaskVersion, error) {
	if task == nil || task.ID == "" || task.ContextID == "" {
		return taskstore.TaskVersionMissing, fmt.Errorf("invalid task: %w", a2a.ErrInvalidRequest)
	}
	snapshot, err := json.Marshal(task)
	if err != nil {
		return 0, err
	}
	const version taskstore.TaskVersion = 1
	created, err := s.records.Insert(ctx, string(task.ID), task.ContextID, task.Status.State.String(), int64(version), snapshot, s.clock.Now().UTC())
	if err != nil {
		return 0, fmt.Errorf("create A2A task: %w", err)
	}
	if !created {
		return 0, taskstore.ErrTaskAlreadyExists
	}
	return version, nil
}

func (s *DurableTaskStore) Update(ctx context.Context, request *taskstore.UpdateRequest) (taskstore.TaskVersion, error) {
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
	updated, exists, err := s.records.Update(ctx, string(request.Task.ID), request.Task.ContextID, request.Task.Status.State.String(), int64(expected), int64(next), snapshot, s.clock.Now().UTC())
	if err != nil {
		return 0, fmt.Errorf("update A2A task: %w", err)
	}
	if updated {
		return next, nil
	}
	if !exists {
		return 0, a2a.ErrTaskNotFound
	}
	return 0, taskstore.ErrConcurrentModification
}

func (s *DurableTaskStore) Get(ctx context.Context, id a2a.TaskID) (*taskstore.StoredTask, error) {
	snapshot, version, found, err := s.records.Get(ctx, string(id))
	if err != nil {
		return nil, fmt.Errorf("get A2A task: %w", err)
	}
	if !found {
		return nil, a2a.ErrTaskNotFound
	}
	var task a2a.Task
	if err := json.Unmarshal(snapshot, &task); err != nil {
		return nil, fmt.Errorf("decode A2A task: %w", err)
	}
	return &taskstore.StoredTask{Task: &task, Version: taskstore.TaskVersion(version)}, nil
}

func (s *DurableTaskStore) List(ctx context.Context, request *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
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
	snapshots, total, err := s.records.List(ctx, request.ContextID, state, request.StatusTimestampAfter, pageSize)
	if err != nil {
		return nil, err
	}
	tasks := make([]*a2a.Task, 0, len(snapshots))
	for _, snapshot := range snapshots {
		var task a2a.Task
		if err := json.Unmarshal(snapshot, &task); err != nil {
			return nil, err
		}
		if !request.IncludeArtifacts {
			task.Artifacts = nil
		}
		tasks = append(tasks, &task)
	}
	return &a2a.ListTasksResponse{Tasks: tasks, TotalSize: total, PageSize: pageSize}, nil
}

var _ taskstore.Store = (*DurableTaskStore)(nil)
