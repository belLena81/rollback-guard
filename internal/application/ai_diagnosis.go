package application

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

const (
	maximumReasoningEvidence = 32
	maximumHypothesisBytes   = 2 * 1024
	DiagnosisPromptVersion   = "rollback-diagnosis-v1"
)

type ModelDiagnoserConfig struct {
	Model         string
	PromptVersion string
	Timeout       time.Duration
}

type modelDiagnoser struct {
	reasoner     ports.Reasoner
	rules        ports.Diagnoser
	ids          ports.IDGenerator
	config       ModelDiagnoserConfig
	strategy     domain.DiagnosisStrategy
	includeRules bool
}

type LLMDiagnoser struct {
	model modelDiagnoser
}

type HybridDiagnoser struct {
	model modelDiagnoser
}

func NewLLMDiagnoser(reasoner ports.Reasoner, rules ports.Diagnoser, ids ports.IDGenerator, config ModelDiagnoserConfig) (*LLMDiagnoser, error) {
	model, err := newModelDiagnoser(reasoner, rules, ids, config, domain.StrategyLLM, false)
	if err != nil {
		return nil, err
	}
	return &LLMDiagnoser{model: model}, nil
}

func NewHybridDiagnoser(reasoner ports.Reasoner, rules ports.Diagnoser, ids ports.IDGenerator, config ModelDiagnoserConfig) (*HybridDiagnoser, error) {
	model, err := newModelDiagnoser(reasoner, rules, ids, config, domain.StrategyHybrid, true)
	if err != nil {
		return nil, err
	}
	return &HybridDiagnoser{model: model}, nil
}

func newModelDiagnoser(reasoner ports.Reasoner, rules ports.Diagnoser, ids ports.IDGenerator, config ModelDiagnoserConfig, strategy domain.DiagnosisStrategy, includeRules bool) (modelDiagnoser, error) {
	if reasoner == nil || rules == nil || ids == nil {
		return modelDiagnoser{}, fmt.Errorf("reasoner, rule diagnoser, and id generator are required")
	}
	if strings.TrimSpace(config.Model) == "" || strings.TrimSpace(config.PromptVersion) == "" || config.Timeout <= 0 {
		return modelDiagnoser{}, fmt.Errorf("model, prompt version, and positive timeout are required")
	}
	return modelDiagnoser{reasoner: reasoner, rules: rules, ids: ids, config: config, strategy: strategy, includeRules: includeRules}, nil
}

func (d *LLMDiagnoser) Diagnose(ctx context.Context, input ports.InvestigationInput) (domain.InvestigationConclusion, error) {
	return d.model.diagnose(ctx, input)
}

func (d *HybridDiagnoser) Diagnose(ctx context.Context, input ports.InvestigationInput) (domain.InvestigationConclusion, error) {
	return d.model.diagnose(ctx, input)
}

func (d modelDiagnoser) diagnose(ctx context.Context, input ports.InvestigationInput) (domain.InvestigationConclusion, error) {
	if err := ctx.Err(); err != nil {
		return domain.InvestigationConclusion{}, err
	}
	var ruleCandidate *domain.InvestigationConclusion
	if d.includeRules {
		conclusion, err := d.rules.Diagnose(ctx, input)
		if err != nil {
			return domain.InvestigationConclusion{}, fmt.Errorf("run rule diagnosis before model review: %w", err)
		}
		ruleCandidate = &conclusion
	}

	request, err := newReasoningRequest(input, d.config.PromptVersion, ruleCandidate)
	if err != nil {
		return d.fallback(ctx, input, ruleCandidate, domain.FallbackInvalidOutput, ports.TokenUsage{}, ports.ErrReasonerInvalidResponse)
	}
	callCtx, cancel := context.WithTimeout(ctx, d.config.Timeout)
	result, reasonErr := d.reasoner.Reason(callCtx, request)
	callErr := callCtx.Err()
	cancel()
	if err := ctx.Err(); err != nil {
		return domain.InvestigationConclusion{}, err
	}
	if reasonErr != nil || callErr != nil {
		reason := fallbackReason(reasonErr, callErr)
		return d.fallback(ctx, input, ruleCandidate, reason, validUsageOrZero(result.Usage), sanitizedReasonerError(reason))
	}

	recommendation, err := validateReasoningResult(input, result)
	if err != nil {
		return d.fallback(ctx, input, ruleCandidate, domain.FallbackInvalidOutput, validUsageOrZero(result.Usage), ports.ErrReasonerInvalidResponse)
	}
	provenance := domain.DiagnosisProvenance{
		RequestedStrategy: d.strategy,
		Model:             result.Model,
		PromptVersion:     d.config.PromptVersion,
		InputTokens:       result.Usage.InputTokens,
		OutputTokens:      result.Usage.OutputTokens,
	}
	return domain.NewInvestigationConclusionWithProvenance(
		domain.ConclusionID(d.ids.NewID("conclusion")),
		input.Incident.ID(),
		d.strategy,
		strings.TrimSpace(result.Candidate.Hypothesis),
		result.Candidate.Confidence,
		result.Candidate.EvidenceIDs,
		recommendation,
		provenance,
		input.Now,
	)
}

func (d modelDiagnoser) fallback(ctx context.Context, input ports.InvestigationInput, existing *domain.InvestigationConclusion, reason domain.DiagnosisFallbackReason, usage ports.TokenUsage, initiatingErr error) (domain.InvestigationConclusion, error) {
	if err := ctx.Err(); err != nil {
		return domain.InvestigationConclusion{}, err
	}
	var conclusion domain.InvestigationConclusion
	if existing != nil {
		conclusion = *existing
	} else {
		var err error
		conclusion, err = d.rules.Diagnose(ctx, input)
		if err != nil {
			return domain.InvestigationConclusion{}, fmt.Errorf("model diagnosis and deterministic fallback failed: %w", errors.Join(initiatingErr, err))
		}
	}
	snapshot := conclusion.Snapshot()
	provenance := domain.DiagnosisProvenance{
		RequestedStrategy: d.strategy,
		Model:             d.config.Model,
		PromptVersion:     d.config.PromptVersion,
		InputTokens:       usage.InputTokens,
		OutputTokens:      usage.OutputTokens,
		FallbackReason:    reason,
	}
	return domain.NewInvestigationConclusionWithProvenance(
		snapshot.ID,
		snapshot.IncidentID,
		domain.StrategyRules,
		snapshot.Hypothesis,
		snapshot.Confidence,
		snapshot.EvidenceIDs,
		snapshot.Recommendation,
		provenance,
		snapshot.CreatedAt,
	)
}

func sanitizedReasonerError(reason domain.DiagnosisFallbackReason) error {
	switch reason {
	case domain.FallbackProviderTimeout:
		return context.DeadlineExceeded
	case domain.FallbackProviderRefusal:
		return ports.ErrReasonerRefused
	case domain.FallbackInvalidOutput:
		return ports.ErrReasonerInvalidResponse
	default:
		return ports.ErrReasonerUnavailable
	}
}

func newReasoningRequest(input ports.InvestigationInput, promptVersion string, ruleConclusion *domain.InvestigationConclusion) (ports.ReasoningRequest, error) {
	if len(input.Evidence) > maximumReasoningEvidence {
		return ports.ReasoningRequest{}, fmt.Errorf("reasoning evidence exceeds %d records", maximumReasoningEvidence)
	}
	evidence := make([]ports.ReasoningEvidence, 0, len(input.Evidence))
	for _, item := range input.Evidence {
		if item.IncidentID() != input.Incident.ID() {
			return ports.ReasoningRequest{}, fmt.Errorf("reasoning evidence belongs to another incident")
		}
		normalized := ports.ReasoningEvidence{ID: item.ID(), Kind: item.Kind(), CollectedAt: item.CollectedAt()}
		if value, ok := item.Deployment(); ok {
			normalized.Deployment = &value
		}
		if value, ok := item.Metric(); ok {
			normalized.Metric = &value
		}
		if value, ok := item.Pod(); ok {
			normalized.Pod = &value
		}
		if value, ok := item.KubernetesEvent(); ok {
			normalized.KubernetesEvent = &ports.ReasoningKubernetesEvent{Reason: value.Reason, ObservedAt: value.ObservedAt}
		}
		evidence = append(evidence, normalized)
	}
	request := ports.ReasoningRequest{
		PromptVersion: promptVersion,
		IncidentID:    input.Incident.ID(),
		Target:        input.Incident.Target(),
		Symptom:       input.Incident.Symptom(),
		Evidence:      evidence,
	}
	if ruleConclusion != nil {
		request.RuleCandidate = &ports.ReasoningCandidate{
			Hypothesis:     ruleConclusion.Hypothesis(),
			Confidence:     ruleConclusion.Confidence(),
			EvidenceIDs:    ruleConclusion.EvidenceIDs(),
			Recommendation: ruleConclusion.Recommendation().String(),
		}
	}
	return request, nil
}

func validateReasoningResult(input ports.InvestigationInput, result ports.ReasoningResult) (domain.Recommendation, error) {
	hypothesis := strings.TrimSpace(result.Candidate.Hypothesis)
	if hypothesis == "" || len(hypothesis) > maximumHypothesisBytes {
		return 0, fmt.Errorf("model hypothesis is invalid")
	}
	if math.IsNaN(result.Candidate.Confidence) || math.IsInf(result.Candidate.Confidence, 0) || result.Candidate.Confidence < 0 || result.Candidate.Confidence > 1 {
		return 0, fmt.Errorf("model confidence is invalid")
	}
	if strings.TrimSpace(result.Model) == "" || result.Usage.InputTokens < 0 || result.Usage.OutputTokens < 0 {
		return 0, fmt.Errorf("model metadata is invalid")
	}
	recommendation := domain.Recommendation(0)
	switch result.Candidate.Recommendation {
	case domain.RecommendRollback.String():
		recommendation = domain.RecommendRollback
	case domain.RecommendNoAction.String():
		recommendation = domain.RecommendNoAction
	default:
		return 0, fmt.Errorf("model recommendation is invalid")
	}
	if recommendation == domain.RecommendRollback && len(result.Candidate.EvidenceIDs) == 0 {
		return 0, fmt.Errorf("rollback recommendation requires evidence")
	}
	available := make(map[domain.EvidenceID]struct{}, len(input.Evidence))
	for _, item := range input.Evidence {
		if item.IncidentID() != input.Incident.ID() {
			return 0, fmt.Errorf("input evidence belongs to another incident")
		}
		available[item.ID()] = struct{}{}
	}
	seen := make(map[domain.EvidenceID]struct{}, len(result.Candidate.EvidenceIDs))
	for _, id := range result.Candidate.EvidenceIDs {
		if id == "" {
			return 0, fmt.Errorf("model evidence reference is empty")
		}
		if _, duplicate := seen[id]; duplicate {
			return 0, fmt.Errorf("model evidence reference is duplicated")
		}
		if _, exists := available[id]; !exists {
			return 0, fmt.Errorf("model evidence reference is unknown")
		}
		seen[id] = struct{}{}
	}
	return recommendation, nil
}

func fallbackReason(reasonErr error, callErr error) domain.DiagnosisFallbackReason {
	if errors.Is(reasonErr, ports.ErrReasonerRefused) {
		return domain.FallbackProviderRefusal
	}
	if errors.Is(reasonErr, ports.ErrReasonerInvalidResponse) {
		return domain.FallbackInvalidOutput
	}
	if errors.Is(reasonErr, context.DeadlineExceeded) || errors.Is(callErr, context.DeadlineExceeded) {
		return domain.FallbackProviderTimeout
	}
	return domain.FallbackProviderError
}

func validUsageOrZero(usage ports.TokenUsage) ports.TokenUsage {
	if usage.InputTokens < 0 || usage.OutputTokens < 0 {
		return ports.TokenUsage{}
	}
	return usage
}

var _ ports.Diagnoser = (*LLMDiagnoser)(nil)
var _ ports.Diagnoser = (*HybridDiagnoser)(nil)
