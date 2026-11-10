package a2aadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/belLena81/rollback-guard/internal/ports"
)

const (
	terminalPersistenceTimeout = 5 * time.Second
	// Slack before a silent owner is declared gone.
	staleReclaimMargin = 5 * time.Second
	// A duplicate request polls the owner's record about this many times
	// per task timeout, within the bounds below.
	resultPollsPerTask    = 100
	minResultPollInterval = 10 * time.Millisecond
	maxResultPollInterval = 100 * time.Millisecond
)

// RequestLedger is the durable record that deduplicates investigations by
// idempotency key. Every write is fenced by the owning task ID and reports
// false when the caller no longer owns the record.
type RequestLedger interface {
	// Claim records a new working request. It reports false when the key exists.
	Claim(ctx context.Context, key, requestHash, taskID string, now time.Time) (claimed bool, err error)
	// Reclaim takes over a working request last updated before staleBefore.
	Reclaim(ctx context.Context, key, requestHash, taskID string, now, staleBefore time.Time) (reclaimed bool, err error)
	// Fail moves an owned request to a terminal failed or canceled state.
	Fail(ctx context.Context, key, requestHash, taskID, state, failureCode string, now time.Time) (owned bool, err error)
	// Complete stores the result of an owned request.
	Complete(ctx context.Context, key, requestHash, taskID string, result []byte, now time.Time) (owned bool, err error)
	// Lookup reads the record for key; found is false when none exists.
	Lookup(ctx context.Context, key string) (requestHash, state, failureCode string, result []byte, found bool, err error)
}

type IdempotentInvestigator struct {
	ledger       RequestLedger
	next         ports.Investigator
	pollInterval time.Duration
	staleAfter   time.Duration
	clock        ports.Clock
}

// NewIdempotentInvestigator deduplicates investigations by idempotency key.
// taskTimeout is the deadline the caller puts on every Investigate call
// (A2A_TASK_TIMEOUT); the reclaim and polling intervals follow from it.
func NewIdempotentInvestigator(ledger RequestLedger, next ports.Investigator, taskTimeout time.Duration, clock ports.Clock) (*IdempotentInvestigator, error) {
	if ledger == nil || next == nil || clock == nil {
		return nil, errors.New("request ledger, investigator, and clock are required")
	}
	if taskTimeout <= 0 {
		return nil, errors.New("investigation task timeout must be positive")
	}
	pollInterval, staleAfter := idempotencyTimings(taskTimeout)
	return &IdempotentInvestigator{ledger: ledger, next: next, pollInterval: pollInterval, staleAfter: staleAfter, clock: clock}, nil
}

// idempotencyTimings derives both intervals from the task timeout.
//
// staleAfter: an owner works for at most taskTimeout and then records the
// terminal state within terminalPersistenceTimeout. A "working" record older
// than that, plus staleReclaimMargin for clock skew between replicas and a
// late-committing terminal write, belongs to an owner that is gone and may
// be reclaimed. Reclaiming is fenced by task_id, so a slow owner that loses
// the record fails instead of overwriting the new owner's result.
//
// pollInterval: a fraction of the task timeout, clamped so short timeouts
// still get several polls and long ones do not poll needlessly slowly.
func idempotencyTimings(taskTimeout time.Duration) (pollInterval, staleAfter time.Duration) {
	pollInterval = min(max(taskTimeout/resultPollsPerTask, minResultPollInterval), maxResultPollInterval)
	return pollInterval, taskTimeout + terminalPersistenceTimeout + staleReclaimMargin
}

func (i *IdempotentInvestigator) Investigate(ctx context.Context, request ports.InvestigationRequest) (ports.InvestigationResult, error) {
	if request.IdempotencyKey == "" || request.RequestHash == "" {
		return ports.InvestigationResult{}, errors.New("idempotency key and request hash are required")
	}
	claimed, err := i.ledger.Claim(ctx, request.IdempotencyKey, request.RequestHash, request.TaskID, i.clock.Now().UTC())
	if err != nil {
		return ports.InvestigationResult{}, fmt.Errorf("claim investigation request: %w", err)
	}
	if !claimed {
		reclaimed, reclaimErr := i.ledger.Reclaim(
			ctx,
			request.IdempotencyKey,
			request.RequestHash,
			request.TaskID,
			i.clock.Now().UTC(),
			i.clock.Now().UTC().Add(-i.staleAfter),
		)
		if reclaimErr != nil {
			return ports.InvestigationResult{}, fmt.Errorf("reclaim request ledger: %w", reclaimErr)
		}
		if !reclaimed {
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
		owned, recordErr := i.ledger.Fail(recordCtx, request.IdempotencyKey, request.RequestHash, request.TaskID, state, failure, i.clock.Now().UTC())
		if recordErr == nil && !owned {
			recordErr = fmt.Errorf("investigation request ownership was lost before recording %s", state)
		}
		return ports.InvestigationResult{}, errors.Join(err, recordErr)
	}
	snapshot, err := json.Marshal(encodeResult(request.Incident.ID(), result))
	if err != nil {
		return ports.InvestigationResult{}, fmt.Errorf("marshal investigation result: %w", err)
	}
	recordCtx, cancel := terminalPersistenceContext(ctx)
	defer cancel()
	owned, err := i.ledger.Complete(recordCtx, request.IdempotencyKey, request.RequestHash, request.TaskID, snapshot, i.clock.Now().UTC())
	if err != nil {
		return ports.InvestigationResult{}, fmt.Errorf("complete investigation request: %w", err)
	}
	if !owned {
		return ports.InvestigationResult{}, errors.New("investigation request ownership was lost before completion")
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
		hash, state, failure, result, found, err := i.ledger.Lookup(ctx, request.IdempotencyKey)
		if err != nil {
			return ports.InvestigationResult{}, fmt.Errorf("lookup request ledger: %w", err)
		}
		if !found {
			return ports.InvestigationResult{}, errors.New("investigation idempotency record disappeared")
		}
		if hash != request.RequestHash {
			return ports.InvestigationResult{}, errors.New("idempotency key reused with a different request")
		}
		switch state {
		case "completed":
			var wire resultDTO
			if err := json.Unmarshal(result, &wire); err != nil {
				return ports.InvestigationResult{}, fmt.Errorf("unmarshal investigation result: %w", err)
			}
			return decodeResult(wire)
		case "failed", "canceled":
			return ports.InvestigationResult{}, fmt.Errorf("investigation request is %s: %s", state, failure)
		}
		select {
		case <-ctx.Done():
			return ports.InvestigationResult{}, fmt.Errorf("context ended: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

var _ ports.Investigator = (*IdempotentInvestigator)(nil)
