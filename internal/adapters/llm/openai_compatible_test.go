package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

func TestOpenAICompatibleReasonerUsesStrictToolFreeSchema(t *testing.T) {
	transport := &recordingTransport{response: `{
		"id":"completion-1","object":"chat.completion","created":1,"model":"served-model",
		"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"{\"hypothesis\":\"rollout regression\",\"confidence\":0.92,\"evidence_ids\":[\"deployment\",\"metric\"],\"recommendation\":\"rollback\"}","refusal":null}}],
		"usage":{"prompt_tokens":41,"completion_tokens":13,"total_tokens":54}
	}`}
	reasoner := newTestReasoner(t, transport)
	request := reasoningRequest(t)

	result, err := reasoner.Reason(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Model != "served-model" || result.Usage.InputTokens != 41 || result.Usage.OutputTokens != 13 {
		t.Fatalf("result = %#v", result)
	}
	if result.Candidate.Recommendation != "rollback" || len(result.Candidate.EvidenceIDs) != 2 {
		t.Fatalf("candidate = %#v", result.Candidate)
	}
	if transport.calls != 1 || transport.path != "/v1/chat/completions" || transport.authorization != "Bearer test-key" {
		t.Fatalf("calls/path/auth = %d/%q/%q", transport.calls, transport.path, transport.authorization)
	}
	var body map[string]any
	if err := json.Unmarshal(transport.body, &body); err != nil {
		t.Fatal(err)
	}
	if _, exists := body["tools"]; exists {
		t.Fatal("reasoning request exposed tools")
	}
	if _, exists := body["tool_choice"]; exists {
		t.Fatal("reasoning request exposed tool choice")
	}
	format, ok := body["response_format"].(map[string]any)
	if !ok {
		t.Fatalf("response format = %#v", body["response_format"])
	}
	schema, ok := format["json_schema"].(map[string]any)
	if !ok {
		t.Fatalf("JSON schema = %#v", format["json_schema"])
	}
	if schema["strict"] != true || schema["name"] != "rollback_diagnosis" {
		t.Fatalf("response format = %#v", format)
	}
	outputSchema, ok := schema["schema"].(map[string]any)
	if !ok {
		t.Fatalf("output schema = %#v", schema["schema"])
	}
	properties, ok := outputSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema properties = %#v", outputSchema["properties"])
	}
	evidenceIDs, ok := properties["evidence_ids"].(map[string]any)
	if !ok || evidenceIDs["minItems"] != float64(1) {
		t.Fatalf("evidence_ids schema = %#v", properties["evidence_ids"])
	}
	if strings.Contains(string(transport.body), "sensitive event payload") {
		t.Fatal("omitted evidence text reached provider request")
	}
}

func TestOpenAICompatibleReasonerRejectsOversizedProviderResponse(t *testing.T) {
	valid := `{"id":"completion","object":"chat.completion","created":1,"model":"model","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"{\"hypothesis\":\"regression\",\"confidence\":0.9,\"evidence_ids\":[\"deployment\"],\"recommendation\":\"rollback\"}","refusal":null}}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`
	transport := &recordingTransport{response: valid + strings.Repeat(" ", (1<<20)+1)}
	reasoner := newTestReasoner(t, transport)

	_, err := reasoner.Reason(context.Background(), reasoningRequest(t))
	if !errors.Is(err, ports.ErrReasonerInvalidResponse) {
		t.Fatalf("error = %v, want invalid response", err)
	}
	if transport.calls != 1 {
		t.Fatalf("calls = %d, want 1", transport.calls)
	}
	if strings.Contains(err.Error(), valid) {
		t.Fatalf("provider response leaked in error: %v", err)
	}
}

func TestOpenAICompatibleReasonerClassifiesUnsafeResponses(t *testing.T) {
	tests := []struct {
		name     string
		response string
		status   int
		want     error
	}{
		{name: "refusal", response: `{"id":"completion","object":"chat.completion","created":1,"model":"model","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"","refusal":"cannot comply"}}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`, want: ports.ErrReasonerRefused},
		{name: "empty choices", response: `{"id":"completion","object":"chat.completion","created":1,"model":"model","choices":[],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`, want: ports.ErrReasonerInvalidResponse},
		{name: "malformed content", response: `{"id":"completion","object":"chat.completion","created":1,"model":"model","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"not-json","refusal":null}}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`, want: ports.ErrReasonerInvalidResponse},
		{name: "missing confidence", response: `{"id":"completion","object":"chat.completion","created":1,"model":"model","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"{\"hypothesis\":\"regression\",\"evidence_ids\":[\"deployment\"],\"recommendation\":\"rollback\"}","refusal":null}}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`, want: ports.ErrReasonerInvalidResponse},
		{name: "null confidence", response: `{"id":"completion","object":"chat.completion","created":1,"model":"model","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"{\"hypothesis\":\"regression\",\"confidence\":null,\"evidence_ids\":[\"deployment\"],\"recommendation\":\"rollback\"}","refusal":null}}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`, want: ports.ErrReasonerInvalidResponse},
		{name: "provider error", response: `{"error":{"message":"raw-secret-provider-body"}}`, status: http.StatusInternalServerError, want: ports.ErrReasonerUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transport := &recordingTransport{status: test.status, response: test.response}
			reasoner := newTestReasoner(t, transport)
			result, err := reasoner.Reason(context.Background(), reasoningRequest(t))
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if strings.Contains(err.Error(), "raw-secret-provider-body") {
				t.Fatalf("provider body leaked in error: %v", err)
			}
			if transport.calls != 1 {
				t.Fatalf("calls = %d, retries must be disabled", transport.calls)
			}
			if test.name == "refusal" && (result.Model != "model" || result.Usage.InputTokens != 2 || result.Usage.OutputTokens != 1) {
				t.Fatalf("safe refusal metadata = %#v", result)
			}
		})
	}
}

func TestOpenAICompatibleReasonerStripsAmbientProviderHeaders(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "ambient-key")
	t.Setenv("OPENAI_ORG_ID", "ambient-org")
	t.Setenv("OPENAI_PROJECT_ID", "ambient-project")
	transport := &recordingTransport{response: `{
		"id":"completion-1","object":"chat.completion","created":1,"model":"served-model",
		"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"{\"hypothesis\":\"rollout regression\",\"confidence\":0.92,\"evidence_ids\":[\"deployment\"],\"recommendation\":\"rollback\"}","refusal":null}}],
		"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}
	}`}
	reasoner, err := NewOpenAICompatibleReasoner(OpenAICompatibleConfig{
		BaseURL: "http://127.0.0.1:11434/v1", Model: "local-model", MaxOutputTokens: 512,
		HTTPClient: &http.Client{Transport: transport},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reasoner.Reason(context.Background(), reasoningRequest(t)); err != nil {
		t.Fatal(err)
	}
	if transport.authorization != "" || transport.organization != "" || transport.project != "" {
		t.Fatalf("provider headers leaked: auth=%q organization=%q project=%q", transport.authorization, transport.organization, transport.project)
	}
}

func newTestReasoner(t *testing.T, transport http.RoundTripper) *OpenAICompatibleReasoner {
	t.Helper()
	reasoner, err := NewOpenAICompatibleReasoner(OpenAICompatibleConfig{
		BaseURL: "https://provider.example/v1", APIKey: "test-key", Model: "configured-model",
		MaxOutputTokens: 512, HTTPClient: &http.Client{Transport: transport},
	})
	if err != nil {
		t.Fatal(err)
	}
	return reasoner
}

func reasoningRequest(t *testing.T) ports.ReasoningRequest {
	t.Helper()
	now := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	target, err := domain.NewDeploymentTarget("local", "rollback-demo", "demo-api", "demo-api")
	if err != nil {
		t.Fatal(err)
	}
	symptom, err := domain.NewSymptom("http_error_rate", .35, .1, "ratio", now)
	if err != nil {
		t.Fatal(err)
	}
	deployment := domain.DeploymentEvidence{Target: target, CurrentRevision: "revision-2", PreviousStableRevision: "revision-1", DesiredReplicas: 3, ReadyReplicas: 1, AvailableReplicas: 1, RolloutAt: now.Add(-time.Minute)}
	metric := domain.MetricEvidence{Name: "http_error_rate", Value: .35, Threshold: .1, Unit: "ratio", ObservedAt: now}
	return ports.ReasoningRequest{
		PromptVersion: "rollback-diagnosis-v1", IncidentID: "incident", Target: target, Symptom: symptom,
		Evidence: []ports.ReasoningEvidence{
			{ID: "deployment", Kind: domain.EvidenceDeployment, CollectedAt: now, Deployment: &deployment},
			{ID: "metric", Kind: domain.EvidenceMetric, CollectedAt: now, Metric: &metric},
			{ID: "event", Kind: domain.EvidenceKubernetesEvent, CollectedAt: now, KubernetesEvent: &ports.ReasoningKubernetesEvent{Reason: "BackOff", ObservedAt: now}},
		},
	}
}

type recordingTransport struct {
	status        int
	response      string
	calls         int
	path          string
	authorization string
	organization  string
	project       string
	body          []byte
}

func (t *recordingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.calls++
	t.path = request.URL.Path
	t.authorization = request.Header.Get("Authorization")
	t.organization = request.Header.Get("OpenAI-Organization")
	t.project = request.Header.Get("OpenAI-Project")
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	t.body = body
	status := t.status
	if status == 0 {
		status = http.StatusOK
	}
	header := make(http.Header)
	header.Set("Content-Type", "application/json")
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(t.response)),
		Request:    request,
	}, nil
}
