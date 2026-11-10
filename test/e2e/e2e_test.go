//go:build e2e

// Package e2e drives the Phase 6 exit gate against a running stack: the kind
// demo cluster (make kind-create images kind-load deploy), PostgreSQL,
// Prometheus, and the five services (make services-up). Run it with
// `make e2e-test`. It uses the same operator steps as `make e2e` (make
// demo-reset, make demo-v2-broken) and asserts the outcome from the REST API,
// the cluster, and, when tracing is configured, the trace backend.
//
// Missing configuration fails the test instead of skipping it: a skipped
// exit gate proves nothing.
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	threshold         = 0.1
	stableImage       = "demo-api:v1-healthy"
	pollInterval      = 2 * time.Second
	metricWaitTimeout = 3 * time.Minute
	statusWaitTimeout = 3 * time.Minute
	approveTimeout    = 15 * time.Minute
)

type env struct {
	orchestrator, prometheus, demo, kubeContext, namespace, deployment, window string
	otlp, traceQuery, repoRoot, makeCmd, kubectl, cluster, databaseURL         string
}

func loadEnv(t *testing.T) env {
	t.Helper()
	get := func(key, fallback string) string {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
		return fallback
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	e := env{
		orchestrator: get("ORCHESTRATOR_URL", "http://127.0.0.1:8080"),
		prometheus:   get("PROMETHEUS_URL", ""),
		demo:         get("DEMO_URL", ""),
		kubeContext:  get("KUBE_CONTEXT", "kind-rollback-guard"),
		cluster:      get("TARGET_CLUSTER", "local"),
		databaseURL:  get("DATABASE_URL", ""),
		namespace:    get("DEMO_NAMESPACE", "rollback-demo"),
		deployment:   get("DEMO_DEPLOYMENT", "demo-api"),
		window:       get("METRICS_WINDOW", "30s"),
		otlp:         get("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
		traceQuery:   get("TRACE_QUERY_URL", ""),
		repoRoot:     root,
		makeCmd:      get("MAKE", "make"),
		kubectl:      get("KUBECTL", "kubectl"),
	}
	if e.prometheus == "" || e.demo == "" || e.databaseURL == "" {
		t.Fatal("PROMETHEUS_URL, DEMO_URL, and DATABASE_URL are required (run through make e2e-test)")
	}
	return e
}

// TestE2ERollback is the Phase 6 exit gate: healthy -> load -> broken ->
// threshold in Prometheus -> incident -> awaiting_approval -> approve the
// exact proposal -> resolved, verified from API, cluster, audit, and trace.
func TestE2ERollback(t *testing.T) {
	e := loadEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	e.requireServicesReady(ctx, t)

	e.run(ctx, t, e.makeCmd, "-C", e.repoRoot, "--no-print-directory", "-s", "demo-reset")
	stopLoad := e.startLoad(ctx, t)
	defer stopLoad()
	e.waitForMetric(ctx, t, "healthy baseline", func(rate, requests float64) bool { return requests > 0.5 && rate < threshold })

	e.run(ctx, t, e.makeCmd, "-C", e.repoRoot, "--no-print-directory", "-s", "demo-v2-broken", "DEMO_BROKEN=error-storm")
	observed := e.waitForMetric(ctx, t, "error rate at or above the threshold", func(rate, requests float64) bool { return requests > 0.5 && rate >= threshold })

	createTrace := newTraceID(t)
	id := e.createIncident(ctx, t, observed, createTrace)
	summary := e.waitForStatus(ctx, t, id, "awaiting_approval")
	proposal := object(t, summary, "proposal")
	actionID, proposalHash := str(t, proposal, "actionId"), str(t, proposal, "proposalHash")

	approveTrace := newTraceID(t)
	status, body := e.call(ctx, t, http.MethodPost, "/v1/incidents/"+id+"/approve",
		map[string]string{"actionId": actionID, "proposalHash": proposalHash}, approveTrace, approveTimeout)
	if status != http.StatusOK {
		t.Fatalf("approve: status %d body %s", status, body)
	}

	_, body = e.call(ctx, t, http.MethodGet, "/v1/incidents/"+id, nil, "", 30*time.Second)
	summary = decode(t, body)
	incident := object(t, summary, "incident")
	if got := str(t, incident, "status"); got != "resolved" {
		t.Fatalf("incident status = %s, want resolved; summary %s", got, body)
	}
	approval, execution, verification := object(t, summary, "approval"), object(t, summary, "execution"), object(t, summary, "verification")
	if str(t, approval, "actionId") != actionID || str(t, approval, "proposalHash") != proposalHash || str(t, object(t, approval, "actor"), "kind") != "human" {
		t.Fatalf("approval %v does not bind the exact proposal %s/%s to a human", approval, actionID, proposalHash)
	}
	if str(t, execution, "outcome") != "succeeded" || str(t, execution, "toRevision") != str(t, proposal, "targetRevision") {
		t.Fatalf("execution = %v", execution)
	}
	if verification["passed"] != true || str(t, verification, "revision") != str(t, proposal, "targetRevision") ||
		num(t, verification, "readyReplicas") != num(t, verification, "desiredReplicas") || num(t, verification, "metricValue") >= threshold {
		t.Fatalf("verification = %v", verification)
	}
	if requests := e.query(ctx, t, e.requestRateQuery()); requests <= 0.5 {
		t.Fatalf("request rate %v at verification: the recovered error rate would be vacuous", requests)
	}

	events := e.requireTimeline(ctx, t, id, []string{
		"incident.created", "status:investigating", "evidence:deployment", "evidence:metric", "diagnosis:rollback", "policy:true",
		"rollback.proposal_created", "status:awaiting_approval", "rollback.approved", "status:approved", "status:executing",
		"execution:succeeded", "status:verifying", "verification:true", "status:resolved",
	})
	for _, kind := range []string{"rollback.proposal_created", "rollback.approved", "rollback.execution_completed", "rollback.verification_completed"} {
		if got := len(events.ofKind(kind)); got != 1 {
			t.Fatalf("%d %s events, want exactly 1", got, kind)
		}
	}
	if created := events.ofKind("rollback.proposal_created")[0]; created.Detail["proposalHash"] != proposalHash {
		t.Fatalf("proposal_created hash %v, approved hash %s", created.Detail["proposalHash"], proposalHash)
	}
	if approved := events.ofKind("rollback.approved")[0]; approved.Actor.Kind != "human" {
		t.Fatalf("approval event actor kind %q, want human", approved.Actor.Kind)
	}
	requireEvidenceCollected(t, object(t, summary, "conclusion"), events)
	e.requireDeploymentOnStable(ctx, t)
	e.checkTrace(ctx, t, createTrace, []string{
		"orchestrator\tPOST /v1/incidents", "orchestrator\tinvestigation.dispatch", "investigator\tinvestigation.execute",
		"kubernetes-mcp-readonly\tmcp tool get_deployment", "observability-mcp\tmcp tool get_http_error_rate",
	}, true)
	e.checkTrace(ctx, t, approveTrace, []string{"kubernetes-mcp-rollback\tmcp tool execute_approved_rollback"}, false)
}

// TestE2EDecline is the policy-decline scenario: a crashlooping rollout that
// never serves errors (the old pod keeps serving), reported by an alert
// whose error spike the live metrics do not confirm, is declined with no
// proposal and no mutation.
func TestE2EDecline(t *testing.T) {
	e := loadEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	e.requireServicesReady(ctx, t)

	e.run(ctx, t, e.makeCmd, "-C", e.repoRoot, "--no-print-directory", "-s", "demo-reset")
	stopLoad := e.startLoad(ctx, t)
	defer stopLoad()
	e.waitForMetric(ctx, t, "healthy baseline", func(rate, requests float64) bool { return requests > 0.5 && rate < threshold })
	e.run(ctx, t, e.makeCmd, "-C", e.repoRoot, "--no-print-directory", "-s", "demo-v2-broken", "DEMO_BROKEN=crashloop")
	e.waitFor(ctx, t, "the crashloop pod to restart", func() bool {
		out := e.kubectlOutput(ctx, t, "get", "pods", "-l", "app.kubernetes.io/name=demo-api", "-o",
			`jsonpath={range .items[*]}{.spec.containers[0].image} {.status.containerStatuses[0].restartCount}{"\n"}{end}`)
		for line := range strings.SplitSeq(out, "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && strings.Contains(fields[0], "crashloop") && fields[1] != "0" {
				return true
			}
		}
		return false
	})
	e.waitForMetric(ctx, t, "error rate still below the threshold", func(rate, requests float64) bool { return requests > 0.5 && rate < threshold })
	generation := e.kubectlOutput(ctx, t, "get", "deployment", e.deployment, "-o", "jsonpath={.metadata.generation}")

	id := e.createIncident(ctx, t, 0.5, newTraceID(t))
	summary := e.waitForStatus(ctx, t, id, "declined")
	for _, key := range []string{"proposal", "approval", "execution", "verification"} {
		if summary[key] != nil {
			t.Fatalf("declined incident has a %s: %v", key, summary[key])
		}
	}
	conclusion := object(t, summary, "conclusion")
	if got := str(t, conclusion, "recommendation"); got != "no_action" {
		t.Fatalf("diagnosis recommendation = %s, want no_action", got)
	}
	events := e.requireTimeline(ctx, t, id, []string{"incident.created", "status:investigating", "evidence:deployment", "evidence:metric", "diagnosis:no_action", "policy:false", "status:declined"})
	for _, event := range events.Events {
		if strings.HasPrefix(event.Kind, "rollback.") {
			t.Fatalf("declined incident has a %s event", event.Kind)
		}
	}
	requireEvidenceCollected(t, conclusion, events)
	// The decline must come from the missing metric breach, not from a stale
	// rollout timestamp (docs/local-demo-env.md, "Reset and repeatability").
	decisions := events.ofKind("policy.evaluated")
	if len(decisions) != 1 {
		t.Fatalf("%d policy.evaluated events, want 1", len(decisions))
	}
	decisionID, ok := decisions[0].Detail["decisionId"].(string)
	if !ok {
		t.Fatalf("policy.evaluated detail %v has no decisionId", decisions[0].Detail)
	}
	rules := e.policyRules(ctx, t, decisionID)
	t.Logf("policy rules: %v", rules)
	if passed, ok := rules["current_metric_threshold_breached"]; !ok || passed {
		t.Fatalf("rule current_metric_threshold_breached = %v (present %v), want failed", passed, ok)
	}
	for _, rule := range []string{"rollout_temporally_correlated", "rollout_recent"} {
		if !rules[rule] {
			t.Fatalf("rule %s failed: the decline is a stale-rollout artifact, not the scenario", rule)
		}
	}
	if got := e.kubectlOutput(ctx, t, "get", "deployment", e.deployment, "-o", "jsonpath={.metadata.generation}"); got != generation {
		t.Fatalf("deployment generation %s -> %s: the decline mutated the deployment", generation, got)
	}
	if image := e.kubectlOutput(ctx, t, "get", "deployment", e.deployment, "-o", "jsonpath={.spec.template.spec.containers[0].image}"); image == stableImage {
		t.Fatalf("deployment was rolled back to %s although the policy declined", image)
	}
}

func (e env) requireServicesReady(ctx context.Context, t *testing.T) {
	t.Helper()
	urls := []string{e.orchestrator}
	for _, port := range []int{8091, 8092, 8093, 8094} {
		urls = append(urls, "http://127.0.0.1:"+strconv.Itoa(port))
	}
	for _, base := range urls {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/health/ready", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("%s is not reachable; run make services-up: %v", base, err)
		}
		status := response.StatusCode
		if err := response.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
		if status != http.StatusOK {
			t.Fatalf("%s/health/ready = %d; run make services-up", base, status)
		}
	}
}

func (e env) run(ctx context.Context, t *testing.T, name string, args ...string) {
	t.Helper()
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = e.repoRoot
	output, err := command.CombinedOutput()
	t.Logf("%s %s\n%s", name, strings.Join(args, " "), output)
	if err != nil {
		t.Fatalf("%s %s: %v", name, strings.Join(args, " "), err)
	}
}

func (e env) kubectlOutput(ctx context.Context, t *testing.T, args ...string) string {
	t.Helper()
	full := append([]string{"--context", e.kubeContext, "-n", e.namespace}, args...)
	output, err := exec.CommandContext(ctx, e.kubectl, full...).Output()
	if err != nil {
		t.Fatalf("kubectl %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(output))
}

// startLoad sends GET /work from four workers until stopped, like
// scripts/demo/generate-load.sh. Failed requests are expected while a broken
// version runs; they are counted and logged, never fatal.
func (e env) startLoad(ctx context.Context, t *testing.T) func() {
	loadCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	var sent, failed atomic.Int64
	client := &http.Client{Timeout: 5 * time.Second}
	for range 4 {
		wg.Go(func() {
			for loadCtx.Err() == nil {
				sent.Add(1)
				if err := e.work(loadCtx, client); err != nil {
					failed.Add(1)
				}
				time.Sleep(50 * time.Millisecond)
			}
		})
	}
	return func() {
		cancel()
		wg.Wait()
		t.Logf("load: %d requests, %d without a response", sent.Load(), failed.Load())
	}
}

func (e env) work(ctx context.Context, client *http.Client) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, e.demo+"/work", nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(io.Discard, response.Body)
	return errors.Join(copyErr, response.Body.Close())
}

func closeBody(t *testing.T, body io.Closer) {
	t.Helper()
	if err := body.Close(); err != nil {
		t.Errorf("close response body: %v", err)
	}
}

func (e env) selector() string {
	return fmt.Sprintf(`cluster=%q,namespace=%q,deployment=%q,service=%q`, e.cluster, e.namespace, e.deployment, e.deployment)
}

func (e env) requestRateQuery() string {
	return fmt.Sprintf("sum(rate(rollback_guardian_http_requests_total{%s}[%s]))", e.selector(), e.window)
}

// errorRateQuery is the observability MCP's query (docs/phase6-kind.md).
func (e env) errorRateQuery() string {
	total := e.requestRateQuery()
	errors := fmt.Sprintf(`sum(rate(rollback_guardian_http_requests_total{%s,code=~"5.."}[%s]))`, e.selector(), e.window)
	return fmt.Sprintf("(%s or (%s * 0)) / (%s > 0)", errors, total, total)
}

// query returns the first sample of an instant query, or -1 if empty.
func (e env) query(ctx context.Context, t *testing.T, promql string) float64 {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, e.prometheus+"/api/v1/query?query="+url.QueryEscape(promql), nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("prometheus: %v", err)
	}
	defer closeBody(t, response.Body)
	var result struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Value [2]any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil || result.Status != "success" {
		t.Fatalf("prometheus query failed: %v (status %q)", err, result.Status)
	}
	if len(result.Data.Result) == 0 {
		return -1
	}
	raw, ok := result.Data.Result[0].Value[1].(string)
	if !ok {
		t.Fatalf("prometheus sample %v is not a string", result.Data.Result[0].Value[1])
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		t.Fatalf("prometheus sample %q: %v", raw, err)
	}
	return value
}

func (e env) waitForMetric(ctx context.Context, t *testing.T, description string, done func(rate, requests float64) bool) float64 {
	t.Helper()
	var rate, requests float64
	e.waitFor(ctx, t, description, func() bool {
		rate, requests = e.query(ctx, t, e.errorRateQuery()), e.query(ctx, t, e.requestRateQuery())
		return rate >= 0 && done(rate, requests)
	})
	t.Logf("%s: error rate %.3f, request rate %.1f/s over %s", description, rate, requests, e.window)
	return rate
}

func (e env) waitFor(ctx context.Context, t *testing.T, description string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(metricWaitTimeout)
	for !done() {
		if time.Now().After(deadline) || ctx.Err() != nil {
			t.Fatalf("timed out waiting for %s", description)
		}
		time.Sleep(pollInterval)
	}
}

func (e env) createIncident(ctx context.Context, t *testing.T, observed float64, traceID string) string {
	t.Helper()
	request := map[string]any{
		"target":  map[string]string{"cluster": e.cluster, "namespace": e.namespace, "deployment": e.deployment, "service": e.deployment},
		"symptom": map[string]any{"kind": "http_error_rate", "observed": observed, "threshold": threshold, "unit": "ratio", "startedAt": time.Now().UTC().Format(time.RFC3339)},
	}
	status, body := e.call(ctx, t, http.MethodPost, "/v1/incidents", request, traceID, 30*time.Second)
	if status != http.StatusAccepted {
		t.Fatalf("create incident: status %d body %s", status, body)
	}
	id := str(t, object(t, decode(t, body), "incident"), "id")
	t.Logf("incident %s created (observed %.3f, trace %s)", id, observed, traceID)
	return id
}

func (e env) waitForStatus(ctx context.Context, t *testing.T, id, want string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(statusWaitTimeout)
	for {
		_, body := e.call(ctx, t, http.MethodGet, "/v1/incidents/"+id, nil, "", 30*time.Second)
		summary := decode(t, body)
		status := str(t, object(t, summary, "incident"), "status")
		if status == want {
			return summary
		}
		if slices.Contains([]string{"resolved", "canceled", "declined", "rejected", "expired", "failed"}, status) {
			t.Fatalf("incident %s ended %s while waiting for %s: %s", id, status, want, body)
		}
		if time.Now().After(deadline) {
			t.Fatalf("incident %s still %s after %s, want %s", id, status, statusWaitTimeout, want)
		}
		time.Sleep(time.Second)
	}
}

type auditEvent struct {
	Sequence   int                   `json:"sequence"`
	Kind       string                `json:"kind"`
	OccurredAt time.Time             `json:"occurredAt"`
	Actor      struct{ Kind string } `json:"actor"`
	Detail     map[string]any        `json:"detail"`
}

type timeline struct {
	Events []auditEvent `json:"events"`
}

func (l timeline) ofKind(kind string) []auditEvent {
	var result []auditEvent
	for _, event := range l.Events {
		if event.Kind == kind {
			result = append(result, event)
		}
	}
	return result
}

// requireEvidenceCollected: the conclusion references at least two pieces of
// evidence, each of which the timeline shows as collected (persisted).
func requireEvidenceCollected(t *testing.T, conclusion map[string]any, events timeline) {
	t.Helper()
	collected := map[string]bool{}
	for _, event := range events.ofKind("investigation.evidence_collected") {
		if id, ok := event.Detail["evidenceId"].(string); ok {
			collected[id] = true
		}
	}
	references, ok := conclusion["evidenceIds"].([]any)
	if !ok || len(references) < 2 {
		t.Fatalf("conclusion evidenceIds = %v, want at least two", conclusion["evidenceIds"])
	}
	for _, reference := range references {
		if id, ok := reference.(string); !ok || !collected[id] {
			t.Fatalf("conclusion references evidence %v that the timeline never collected", reference)
		}
	}
}

// requireTimeline checks the audit events: sequences 1..n, timestamps that
// never go backwards, no failure/expiry/cancel event, and want as an ordered
// subsequence of event tokens (as scripts/demo/verify.sh builds them).
func (e env) requireTimeline(ctx context.Context, t *testing.T, id string, want []string) timeline {
	t.Helper()
	_, body := e.call(ctx, t, http.MethodGet, "/v1/incidents/"+id+"/events", nil, "", 30*time.Second)
	var response timeline
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	next := 0
	for index, event := range response.Events {
		if event.Sequence != index+1 {
			t.Fatalf("event %d has sequence %d", index+1, event.Sequence)
		}
		if index > 0 && event.OccurredAt.Before(response.Events[index-1].OccurredAt) {
			t.Fatalf("event %d occurred before event %d", index+1, index)
		}
		if event.Kind == "incident.failed" || event.Kind == "incident.expired" || event.Kind == "incident.canceled" {
			t.Fatalf("timeline has a %s event: %s", event.Kind, body)
		}
		token := event.Kind
		switch event.Kind {
		case "incident.status_changed":
			token = fmt.Sprintf("status:%v", event.Detail["to"])
		case "investigation.evidence_collected":
			token = fmt.Sprintf("evidence:%v", event.Detail["kind"])
		case "investigation.diagnosis_completed":
			token = fmt.Sprintf("diagnosis:%v", event.Detail["recommendation"])
		case "policy.evaluated":
			token = fmt.Sprintf("policy:%v", event.Detail["allowed"])
		case "rollback.execution_completed":
			token = fmt.Sprintf("execution:%v", event.Detail["outcome"])
		case "rollback.verification_completed":
			token = fmt.Sprintf("verification:%v", event.Detail["passed"])
		}
		if next < len(want) && token == want[next] {
			next++
		}
	}
	if next != len(want) {
		t.Fatalf("audit timeline is missing %q (in order); events: %s", want[next], body)
	}
	return response
}

// policyRules reads one policy decision's per-rule results (read-only); the
// API does not expose them yet.
func (e env) policyRules(ctx context.Context, t *testing.T, decisionID string) map[string]bool {
	t.Helper()
	connection, err := pgx.Connect(ctx, e.databaseURL)
	if err != nil {
		// Never print the DSN or the driver error (it can echo the DSN).
		t.Fatal("connect to DATABASE_URL failed")
	}
	defer func() {
		if err := connection.Close(context.WithoutCancel(ctx)); err != nil {
			t.Errorf("close database connection: %v", err)
		}
	}()
	var raw []byte
	if err := connection.QueryRow(ctx, "SELECT rules FROM policy_decisions WHERE id = $1", decisionID).Scan(&raw); err != nil {
		t.Fatalf("read policy decision %s: %v", decisionID, err)
	}
	var rules []struct {
		Name   string `json:"Name"`
		Passed bool   `json:"Passed"`
	}
	if err := json.Unmarshal(raw, &rules); err != nil {
		t.Fatalf("decode policy rules: %v", err)
	}
	result := make(map[string]bool, len(rules))
	for _, rule := range rules {
		result[rule.Name] = rule.Passed
	}
	return result
}

func (e env) requireDeploymentOnStable(ctx context.Context, t *testing.T) {
	t.Helper()
	e.run(ctx, t, e.kubectl, "--context", e.kubeContext, "-n", e.namespace, "rollout", "status", "deployment/"+e.deployment, "--timeout=30s")
	revision := e.kubectlOutput(ctx, t, "get", "deployment", e.deployment, "-o", `jsonpath={.metadata.annotations.deployment\.kubernetes\.io/revision}`)
	stable := e.kubectlOutput(ctx, t, "get", "replicasets", "-o",
		`go-template={{range .items}}{{if eq (index .metadata.annotations "rollback-guardian.io/stable") "true"}}{{index .metadata.annotations "deployment.kubernetes.io/revision"}} {{(index .spec.template.spec.containers 0).image}}{{"\n"}}{{end}}{{end}}`)
	lines := strings.Split(stable, "\n")
	if len(lines) != 1 || lines[0] != revision+" "+stableImage {
		t.Fatalf("stable ReplicaSets %q, want exactly one at deployment revision %s running %s", stable, revision, stableImage)
	}
}

// checkTrace reads a trace back from the Jaeger query API and requires the
// listed "service<TAB>operation" spans. With connected, it also requires one
// tree: exactly one span whose parent is not in the trace (the REST server
// span; the client's own span is never exported).
func (e env) checkTrace(ctx context.Context, t *testing.T, traceID string, want []string, connected bool) {
	t.Helper()
	if e.otlp == "" {
		t.Logf("trace check SKIPPED (not passed) for %s: OTEL_EXPORTER_OTLP_ENDPOINT is not set, so the services export no spans", traceID)
		return
	}
	if e.traceQuery == "" {
		t.Fatalf("trace check for %s: OTEL_EXPORTER_OTLP_ENDPOINT is set but TRACE_QUERY_URL is not, so the trace cannot be read back", traceID)
	}
	var spans []string
	roots := 0
	deadline := time.Now().Add(30 * time.Second)
	for {
		spans, roots = e.fetchTrace(ctx, t, traceID)
		missing := slices.DeleteFunc(slices.Clone(want), func(entry string) bool { return slices.Contains(spans, entry) })
		if len(missing) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("trace %s is missing spans %q; has %q", traceID, missing, spans)
		}
		time.Sleep(pollInterval)
	}
	if connected && roots != 1 {
		t.Fatalf("trace %s has %d spans without a parent in the trace, want 1 (one connected REST -> A2A -> MCP tree)", traceID, roots)
	}
	t.Logf("trace %s: %d spans, all required spans present", traceID, len(spans))
}

func (e env) fetchTrace(ctx context.Context, t *testing.T, traceID string) ([]string, int) {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, e.traceQuery+"/api/traces/"+traceID, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("trace backend: %v", err)
	}
	defer closeBody(t, response.Body)
	var result struct {
		Data []struct {
			Spans []struct {
				SpanID     string `json:"spanID"`
				Operation  string `json:"operationName"`
				ProcessID  string `json:"processID"`
				References []struct {
					RefType string `json:"refType"`
					SpanID  string `json:"spanID"`
				} `json:"references"`
			} `json:"spans"`
			Processes map[string]struct {
				ServiceName string `json:"serviceName"`
			} `json:"processes"`
		} `json:"data"`
	}
	if response.StatusCode != http.StatusOK {
		return nil, 0
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil || len(result.Data) == 0 {
		return nil, 0
	}
	trace := result.Data[0]
	ids := map[string]bool{}
	for _, span := range trace.Spans {
		ids[span.SpanID] = true
	}
	spans := make([]string, 0, len(trace.Spans))
	roots := 0
	for _, span := range trace.Spans {
		spans = append(spans, trace.Processes[span.ProcessID].ServiceName+"\t"+span.Operation)
		hasParent := false
		for _, reference := range span.References {
			if reference.RefType == "CHILD_OF" && ids[reference.SpanID] {
				hasParent = true
			}
		}
		if !hasParent {
			roots++
		}
	}
	return spans, roots
}

// call sends one API request, with a sampled traceparent when traceID is set.
func (e env) call(ctx context.Context, t *testing.T, method, path string, body any, traceID string, timeout time.Duration) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(callCtx, method, e.orchestrator+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if traceID != "" {
		request.Header.Set("traceparent", "00-"+traceID+"-"+randomHex(t, 8)+"-01")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer closeBody(t, response.Body)
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, data
}

func newTraceID(t *testing.T) string { return randomHex(t, 16) }

func randomHex(t *testing.T, size int) string {
	t.Helper()
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(buffer)
}

func decode(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return value
}

func object(t *testing.T, value map[string]any, key string) map[string]any {
	t.Helper()
	result, ok := value[key].(map[string]any)
	if !ok {
		t.Fatalf("%q missing in %v", key, value)
	}
	return result
}

func str(t *testing.T, value map[string]any, key string) string {
	t.Helper()
	result, ok := value[key].(string)
	if !ok {
		t.Fatalf("%q missing in %v", key, value)
	}
	return result
}

func num(t *testing.T, value map[string]any, key string) float64 {
	t.Helper()
	result, ok := value[key].(float64)
	if !ok {
		t.Fatalf("%q missing in %v", key, value)
	}
	return result
}
