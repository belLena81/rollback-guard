package telemetry

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

// ObserveGateway decorates the A2A investigation gateway with call metrics,
// client spans, and correlated logs. Trace context crosses the wire through
// the HTTP transport (see Transport); this decorator adds the logical span.
func (p *Provider) ObserveGateway(next ports.InvestigationGateway) (ports.InvestigationGateway, error) {
	if next == nil {
		return nil, errors.New("investigation gateway is required")
	}
	return observedGateway{next: next, provider: p}, nil
}

type observedGateway struct {
	next     ports.InvestigationGateway
	provider *Provider
}

func (g observedGateway) Submit(ctx context.Context, submission ports.InvestigationSubmission) (domain.InvestigationTaskRef, error) {
	ctx, finish := g.provider.observeA2A(ctx, "submit")
	ref, err := g.next.Submit(ctx, submission)
	finish(ports.WithCorrelation(ctx, ports.Correlation{TaskID: ref.TaskID}), err)
	return ref, err
}

func (g observedGateway) Watch(ctx context.Context, ref domain.InvestigationTaskRef, progress func(domain.InvestigationStage) error) (ports.InvestigationResult, error) {
	ctx, finish := g.provider.observeA2A(ports.WithCorrelation(ctx, ports.Correlation{TaskID: ref.TaskID}), "watch")
	result, err := g.next.Watch(ctx, ref, progress)
	finish(ctx, err)
	return result, err
}

func (g observedGateway) Cancel(ctx context.Context, ref domain.InvestigationTaskRef) error {
	ctx, finish := g.provider.observeA2A(ports.WithCorrelation(ctx, ports.Correlation{TaskID: ref.TaskID}), "cancel")
	err := g.next.Cancel(ctx, ref)
	finish(ctx, err)
	return err
}

func (p *Provider) observeA2A(ctx context.Context, operation string) (context.Context, func(context.Context, error)) {
	start := time.Now()
	ctx, span := p.startInternal(ctx, "a2a "+operation, trace.SpanKindInternal, attribute.String("a2a.operation", operation))
	return ctx, func(logCtx context.Context, err error) {
		outcome := outcomeOf(err)
		elapsed := time.Since(start)
		p.metrics.a2aCalls.WithLabelValues(operation, outcome).Inc()
		p.metrics.a2aDuration.WithLabelValues(operation, outcome).Observe(elapsed.Seconds())
		if err != nil {
			p.logger.WarnContext(logCtx, "a2a call failed", "operation", operation, "outcome", outcome, "duration_ms", elapsed.Milliseconds())
		} else {
			p.logger.InfoContext(logCtx, "a2a call completed", "operation", operation, "duration_ms", elapsed.Milliseconds())
		}
		endSpan(span, err)
	}
}

// ObserveDiagnoser decorates a diagnoser with strategy and fallback counts
// and a span. requested is the configured DIAGNOSIS_STRATEGY.
func (p *Provider) ObserveDiagnoser(next ports.Diagnoser, requested domain.DiagnosisStrategy) (ports.Diagnoser, error) {
	if next == nil || !requested.Valid() {
		return nil, errors.New("diagnoser and a valid requested strategy are required")
	}
	return observedDiagnoser{next: next, requested: requested, provider: p}, nil
}

type observedDiagnoser struct {
	next      ports.Diagnoser
	requested domain.DiagnosisStrategy
	provider  *Provider
}

func (d observedDiagnoser) Diagnose(ctx context.Context, input ports.InvestigationInput) (domain.InvestigationConclusion, error) {
	ctx, span := d.provider.startInternal(ctx, "diagnosis", trace.SpanKindInternal, attribute.String("diagnosis.requested_strategy", d.requested.String()))
	conclusion, err := d.next.Diagnose(ctx, input)
	effective, fallback := "error", "none"
	if err == nil {
		effective = conclusion.Strategy().String()
		if reason := conclusion.Provenance().FallbackReason; reason.Valid() {
			fallback = reason.String()
		}
		span.SetAttributes(attribute.String("diagnosis.effective_strategy", effective), attribute.String("diagnosis.fallback_reason", fallback))
	}
	d.provider.metrics.diagnoses.WithLabelValues(d.requested.String(), effective, fallback).Inc()
	if fallback != "none" {
		d.provider.logger.WarnContext(ctx, "diagnosis fell back to rules", "requested_strategy", d.requested.String(), "fallback_reason", fallback)
	}
	endSpan(span, err)
	return conclusion, err
}
