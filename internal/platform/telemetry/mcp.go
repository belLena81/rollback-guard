package telemetry

import (
	"context"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// ObserveToolCall instruments one outgoing MCP tool call. server and tool
// come from fixed catalogs; outcome is a bounded value chosen by the MCP
// adapter.
func (p *Provider) ObserveToolCall(ctx context.Context, server string, tool string) (context.Context, func(outcome string)) {
	start := time.Now()
	ctx, span := p.startInternal(ctx, "mcp call "+tool, trace.SpanKindInternal, attribute.String("mcp.server", server), attribute.String("mcp.tool", tool))
	return ctx, func(outcome string) {
		elapsed := time.Since(start)
		p.metrics.mcpCalls.WithLabelValues(server, tool, outcome).Inc()
		p.metrics.mcpCallDuration.WithLabelValues(server, tool, outcome).Observe(elapsed.Seconds())
		p.logToolOutcome(ctx, "mcp tool call", server, tool, outcome, elapsed)
		endOutcomeSpan(span, outcome)
	}
}

// ObserveToolRequest instruments one MCP tool call handled by this server.
// When ctx is not already traced, the W3C trace context is taken from the
// HTTP headers of the request that carried the call.
func (p *Provider) ObserveToolRequest(ctx context.Context, header http.Header, server string, tool string) (context.Context, func(outcome string)) {
	start := time.Now()
	if !trace.SpanContextFromContext(ctx).IsValid() && header != nil {
		ctx = p.propagator.Extract(ctx, propagation.HeaderCarrier(header))
	}
	ctx, span := p.startInternal(ctx, "mcp tool "+tool, trace.SpanKindServer, attribute.String("mcp.server", server), attribute.String("mcp.tool", tool))
	return ctx, func(outcome string) {
		elapsed := time.Since(start)
		p.metrics.mcpTools.WithLabelValues(server, tool, outcome).Inc()
		p.metrics.mcpToolDuration.WithLabelValues(server, tool, outcome).Observe(elapsed.Seconds())
		p.logToolOutcome(ctx, "mcp tool handled", server, tool, outcome, elapsed)
		endOutcomeSpan(span, outcome)
	}
}

func (p *Provider) logToolOutcome(ctx context.Context, message string, server string, tool string, outcome string, elapsed time.Duration) {
	if outcome == "ok" {
		p.logger.InfoContext(ctx, message, "server", server, "tool", tool, "outcome", outcome, "duration_ms", elapsed.Milliseconds())
		return
	}
	p.logger.WarnContext(ctx, message, "server", server, "tool", tool, "outcome", outcome, "duration_ms", elapsed.Milliseconds())
}

func endOutcomeSpan(span trace.Span, outcome string) {
	span.SetAttributes(attribute.String("outcome", outcome))
	if outcome != "ok" {
		span.SetStatus(codes.Error, outcome)
	}
	span.End()
}
