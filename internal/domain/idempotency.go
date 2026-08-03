package domain

import "time"

type IdempotencyOperation uint8

const IdempotencyOperationExecuteRollback IdempotencyOperation = 1

func (o IdempotencyOperation) Valid() bool { return o == IdempotencyOperationExecuteRollback }

type IdempotencyState uint8

const (
	IdempotencyPending IdempotencyState = iota + 1
	IdempotencyCompleted
)

type IdempotencyRecord struct {
	key         IdempotencyKey
	operation   IdempotencyOperation
	requestHash string
	resourceID  string
	state       IdempotencyState
	startedAt   time.Time
	resultID    ExecutionID
	completedAt time.Time
}

func NewPendingIdempotencyRecord(key IdempotencyKey, operation IdempotencyOperation, requestHash, resourceID string, startedAt time.Time) (IdempotencyRecord, error) {
	if key == "" || !operation.Valid() || requestHash == "" || resourceID == "" || startedAt.IsZero() {
		return IdempotencyRecord{}, invalid("idempotency_record", "invalid pending record")
	}
	return IdempotencyRecord{key: key, operation: operation, requestHash: requestHash, resourceID: resourceID, state: IdempotencyPending, startedAt: startedAt.UTC()}, nil
}

func (r IdempotencyRecord) Complete(resultID ExecutionID, completedAt time.Time) (IdempotencyRecord, error) {
	if r.state != IdempotencyPending {
		return IdempotencyRecord{}, invalid("idempotency_record.state", "only pending records can be completed")
	}
	if resultID == "" || completedAt.IsZero() || completedAt.Before(r.startedAt) {
		return IdempotencyRecord{}, invalid("idempotency_record.completion", "invalid completion")
	}
	r.state = IdempotencyCompleted
	r.resultID = resultID
	r.completedAt = completedAt.UTC()
	return r, nil
}

func (r IdempotencyRecord) Key() IdempotencyKey             { return r.key }
func (r IdempotencyRecord) Operation() IdempotencyOperation { return r.operation }
func (r IdempotencyRecord) RequestHash() string             { return r.requestHash }
func (r IdempotencyRecord) ResourceID() string              { return r.resourceID }
func (r IdempotencyRecord) State() IdempotencyState         { return r.state }
func (r IdempotencyRecord) StartedAt() time.Time            { return r.startedAt }
func (r IdempotencyRecord) ResultID() ExecutionID           { return r.resultID }
func (r IdempotencyRecord) CompletedAt() time.Time          { return r.completedAt }
