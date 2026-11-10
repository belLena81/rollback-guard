package resilience

import (
	"context"
	"errors"
	"fmt"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

// Protector combines the retrier and the breaker of one dependency. Reads
// use Call; calls that must not be repeated use CallOnce.
type Protector struct {
	retrier *Retrier
	breaker *Breaker
}

// NewProtector requires both parts.
func NewProtector(retrier *Retrier, breaker *Breaker) (*Protector, error) {
	if retrier == nil || breaker == nil {
		return nil, errors.New("protector requires a retrier and a breaker")
	}
	return &Protector{retrier: retrier, breaker: breaker}, nil
}

// Call runs a read: each attempt passes through the breaker, and transient
// failures are retried within the policy. An open breaker ends the call at
// once with a *CircuitOpenError.
func Call[T any](ctx context.Context, p *Protector, fn func(context.Context) (T, error)) (T, error) {
	return Do(ctx, p.retrier, IsTransient, func(ctx context.Context) (T, error) {
		return Guard(ctx, p.breaker, fn)
	})
}

// CallOnce runs fn through the breaker without retrying it.
func CallOnce[T any](ctx context.Context, p *Protector, fn func(context.Context) (T, error)) (T, error) {
	return Guard(ctx, p.breaker, fn)
}

type inspector struct {
	next      ports.DeploymentInspector
	protector *Protector
}

// NewInspector protects deployment reads (MCP readonly server).
func NewInspector(next ports.DeploymentInspector, p *Protector) (ports.DeploymentInspector, error) {
	if next == nil || p == nil {
		return nil, errors.New("deployment inspector and protector are required")
	}
	return inspector{next: next, protector: p}, nil
}

func (i inspector) InspectDeployment(ctx context.Context, target domain.DeploymentTarget) (domain.DeploymentEvidence, error) {
	return Call(ctx, i.protector, func(ctx context.Context) (domain.DeploymentEvidence, error) {
		return i.next.InspectDeployment(ctx, target)
	})
}

type metrics struct {
	next      ports.MetricsPort
	protector *Protector
}

// NewMetrics protects metric reads (MCP observability server).
func NewMetrics(next ports.MetricsPort, p *Protector) (ports.MetricsPort, error) {
	if next == nil || p == nil {
		return nil, errors.New("metrics port and protector are required")
	}
	return metrics{next: next, protector: p}, nil
}

func (m metrics) Observe(ctx context.Context, target domain.DeploymentTarget, symptom domain.Symptom) (domain.MetricEvidence, error) {
	return Call(ctx, m.protector, func(ctx context.Context) (domain.MetricEvidence, error) {
		return m.next.Observe(ctx, target, symptom)
	})
}

type reasoner struct {
	next      ports.Reasoner
	protector *Protector
}

// NewReasoner protects LLM calls. An open breaker is reported as
// ports.ErrReasonerUnavailable (wrapping the CircuitOpenError), so the
// diagnosers fall back to the rule diagnoser exactly as for any other
// unavailable provider.
func NewReasoner(next ports.Reasoner, p *Protector) (ports.Reasoner, error) {
	if next == nil || p == nil {
		return nil, errors.New("reasoner and protector are required")
	}
	return reasoner{next: next, protector: p}, nil
}

func (r reasoner) Reason(ctx context.Context, request ports.ReasoningRequest) (ports.ReasoningResult, error) {
	result, err := Call(ctx, r.protector, func(ctx context.Context) (ports.ReasoningResult, error) {
		return r.next.Reason(ctx, request)
	})
	if errors.Is(err, ErrCircuitOpen) {
		return ports.ReasoningResult{}, fmt.Errorf("%w: %w", ports.ErrReasonerUnavailable, err)
	}
	return result, err
}

type gateway struct {
	next      ports.InvestigationGateway
	protector *Protector
}

// NewGateway protects the A2A investigator client. Submit is safe to repeat
// because the investigator deduplicates on the submission's idempotency key
// and message ID; Get and Cancel are idempotent. Watch is guarded but never
// retried here: the coordinator owns re-watching a task under its lease and
// attempt budget, and a replayed stream would repeat progress callbacks.
func NewGateway(next ports.InvestigationGateway, p *Protector) (ports.InvestigationGateway, error) {
	if next == nil || p == nil {
		return nil, errors.New("investigation gateway and protector are required")
	}
	return gateway{next: next, protector: p}, nil
}

func (g gateway) Submit(ctx context.Context, submission ports.InvestigationSubmission) (domain.InvestigationTaskRef, error) {
	return Call(ctx, g.protector, func(ctx context.Context) (domain.InvestigationTaskRef, error) {
		return g.next.Submit(ctx, submission)
	})
}

func (g gateway) Watch(ctx context.Context, ref domain.InvestigationTaskRef, progress func(domain.InvestigationStage) error) (ports.InvestigationResult, error) {
	return CallOnce(ctx, g.protector, func(ctx context.Context) (ports.InvestigationResult, error) {
		return g.next.Watch(ctx, ref, progress)
	})
}

func (g gateway) Cancel(ctx context.Context, ref domain.InvestigationTaskRef) error {
	_, err := Call(ctx, g.protector, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, g.next.Cancel(ctx, ref)
	})
	return err
}

type executor struct {
	next    ports.RollbackExecutor
	retrier *Retrier
}

// NewExecutor decorates the rollback executor with the only retry a mutation
// may have.
//
// Retry rule. ExecuteApprovedRollback (which includes prepare_rollback) is
// repeated only when BOTH hold:
//
//  1. RequestNotSent(err): the error proves the request never reached the
//     rollback server, because the TCP dial failed. A timeout, a reset, a
//     tool error, or a failure after the connection was made does not
//     qualify, since the server may have acted on the request.
//  2. The error is not ports.ErrRollbackOutcomeUnknown, which the adapter
//     returns on the ambiguous paths it detects. Condition 1 is the real
//     safeguard: a failure after the request was written (a reset, EOF, or
//     timeout) is never a dial failure, so it is not repeated here whether or
//     not the adapter labels it unknown. How the application records such a
//     failure is the adapter's behaviour and is unchanged by this decorator.
//
// The repeat uses the same approved rollback and the same idempotency key,
// which the port contract and the rollback server's mutation ledger already
// treat as one logical request, so even a wrong classification could not
// apply a second mutation. Every other error, including every ambiguous one,
// is returned unchanged for the application to surface as before. There is
// deliberately no circuit breaker on this path: a breaker rejection would
// turn an approved rollback into a recorded failed execution without ever
// trying it.
func NewExecutor(next ports.RollbackExecutor, retrier *Retrier) (ports.RollbackExecutor, error) {
	if next == nil || retrier == nil {
		return nil, errors.New("rollback executor and retrier are required")
	}
	return executor{next: next, retrier: retrier}, nil
}

func (e executor) ExecuteApprovedRollback(ctx context.Context, approved domain.ApprovedRollback, key domain.IdempotencyKey) (domain.RollbackExecution, error) {
	return Do(ctx, e.retrier, RequestNotSent, func(ctx context.Context) (domain.RollbackExecution, error) {
		return e.next.ExecuteApprovedRollback(ctx, approved, key)
	})
}
