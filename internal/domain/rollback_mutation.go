package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// executionIDPrefix and executionIDHexLength shape every deterministic
// execution ID: a readable prefix and the first 128 bits of a SHA-256.
const (
	executionIDPrefix    = "execution-"
	executionIDHexLength = 32
)

// DeterministicExecutionID is the one ID of the execution an idempotency key
// and proposal hash stand for. The rollback ledger and the executor both use
// it, so a replay of the same approval always names the same execution. The
// newline keeps a (key, hash) pair from being re-split into another pair.
func DeterministicExecutionID(key IdempotencyKey, hash ProposalHash) ExecutionID {
	sum := sha256.Sum256([]byte(string(key) + "\n" + string(hash)))
	return ExecutionID(executionIDPrefix + hex.EncodeToString(sum[:])[:executionIDHexLength])
}

type RollbackMutationState uint8

const (
	RollbackMutationPending RollbackMutationState = iota + 1
	RollbackMutationCompleted
)

func (s RollbackMutationState) String() string {
	switch s {
	case RollbackMutationPending:
		return "pending"
	case RollbackMutationCompleted:
		return "completed"
	default:
		return unknownName
	}
}

func ParseRollbackMutationState(value string) (RollbackMutationState, error) {
	return parseEnum("rollback mutation state", value, RollbackMutationPending, RollbackMutationCompleted)
}

func (s RollbackMutationState) Valid() bool {
	return s == RollbackMutationPending || s == RollbackMutationCompleted
}

type RollbackMutation struct {
	key            IdempotencyKey
	proposalHash   ProposalHash
	executionID    ExecutionID
	fromRevision   Revision
	toRevision     Revision
	state          RollbackMutationState
	startedAt      time.Time
	completedAt    time.Time
	alreadyApplied bool
}

type RollbackMutationSnapshot struct {
	Key            IdempotencyKey
	ProposalHash   ProposalHash
	ExecutionID    ExecutionID
	FromRevision   Revision
	ToRevision     Revision
	State          RollbackMutationState
	StartedAt      time.Time
	CompletedAt    time.Time
	AlreadyApplied bool
}

func NewPendingRollbackMutation(key IdempotencyKey, proposalHash ProposalHash, executionID ExecutionID, fromRevision, toRevision Revision, startedAt time.Time) (RollbackMutation, error) {
	return RehydrateRollbackMutation(RollbackMutationSnapshot{
		Key:          key,
		ProposalHash: proposalHash,
		ExecutionID:  executionID,
		FromRevision: fromRevision,
		ToRevision:   toRevision,
		State:        RollbackMutationPending,
		StartedAt:    startedAt,
	})
}

func RehydrateRollbackMutation(snapshot RollbackMutationSnapshot) (RollbackMutation, error) {
	if snapshot.Key == "" || snapshot.ProposalHash == "" || snapshot.ExecutionID == "" || snapshot.FromRevision == "" || snapshot.ToRevision == "" || snapshot.StartedAt.IsZero() {
		return RollbackMutation{}, invalid("rollback_mutation", "required value is empty")
	}
	if snapshot.FromRevision == snapshot.ToRevision {
		return RollbackMutation{}, invalid("rollback_mutation.revisions", "must differ")
	}
	if !snapshot.State.Valid() {
		return RollbackMutation{}, invalid("rollback_mutation.state", "unknown state")
	}
	if snapshot.State == RollbackMutationPending && (!snapshot.CompletedAt.IsZero() || snapshot.AlreadyApplied) {
		return RollbackMutation{}, invalid("rollback_mutation.pending", "cannot contain a result")
	}
	if snapshot.State == RollbackMutationCompleted && (snapshot.CompletedAt.IsZero() || snapshot.CompletedAt.Before(snapshot.StartedAt)) {
		return RollbackMutation{}, invalid("rollback_mutation.completed", "invalid completion time")
	}
	return RollbackMutation{
		key:            snapshot.Key,
		proposalHash:   snapshot.ProposalHash,
		executionID:    snapshot.ExecutionID,
		fromRevision:   snapshot.FromRevision,
		toRevision:     snapshot.ToRevision,
		state:          snapshot.State,
		startedAt:      snapshot.StartedAt.UTC(),
		completedAt:    snapshot.CompletedAt.UTC(),
		alreadyApplied: snapshot.AlreadyApplied,
	}, nil
}

func (m RollbackMutation) Complete(alreadyApplied bool, completedAt time.Time) (RollbackMutation, error) {
	if m.state != RollbackMutationPending || completedAt.IsZero() || completedAt.Before(m.startedAt) {
		return RollbackMutation{}, invalid("rollback_mutation.completion", "invalid completion")
	}
	m.state = RollbackMutationCompleted
	m.completedAt = completedAt.UTC()
	m.alreadyApplied = alreadyApplied
	return m, nil
}

func (m RollbackMutation) Snapshot() RollbackMutationSnapshot {
	return RollbackMutationSnapshot{
		Key:            m.key,
		ProposalHash:   m.proposalHash,
		ExecutionID:    m.executionID,
		FromRevision:   m.fromRevision,
		ToRevision:     m.toRevision,
		State:          m.state,
		StartedAt:      m.startedAt,
		CompletedAt:    m.completedAt,
		AlreadyApplied: m.alreadyApplied,
	}
}

func (m RollbackMutation) Key() IdempotencyKey {
	return m.key
}

func (m RollbackMutation) ProposalHash() ProposalHash {
	return m.proposalHash
}

func (m RollbackMutation) ExecutionID() ExecutionID {
	return m.executionID
}

func (m RollbackMutation) FromRevision() Revision {
	return m.fromRevision
}

func (m RollbackMutation) ToRevision() Revision {
	return m.toRevision
}

func (m RollbackMutation) State() RollbackMutationState {
	return m.state
}

func (m RollbackMutation) StartedAt() time.Time {
	return m.startedAt
}

func (m RollbackMutation) CompletedAt() time.Time {
	return m.completedAt
}

func (m RollbackMutation) AlreadyApplied() bool {
	return m.alreadyApplied
}
