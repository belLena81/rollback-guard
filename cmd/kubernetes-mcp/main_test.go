package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/config"
	"github.com/belLena81/rollback-guard/internal/platform/httpserver"
	"github.com/belLena81/rollback-guard/internal/platform/telemetry"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func testConfig(t *testing.T, mode string) config.KubernetesMCPConfig {
	t.Helper()
	target, err := domain.NewDeploymentTarget("local", "rollback-demo", "demo-api", "demo-api")
	if err != nil {
		t.Fatal(err)
	}
	return config.KubernetesMCPConfig{Mode: mode, ClusterID: "local", AllowedTargets: []domain.DeploymentTarget{target}, RequestTimeout: time.Second, SessionTimeout: time.Minute, Resilience: config.DefaultResilience()}
}

func demoDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "rollback-demo", Name: "demo-api"}}
}

func TestReadinessReadsTheFirstAllowedDeployment(t *testing.T) {
	tests := []struct {
		name    string
		objects []runtime.Object
		wantErr bool
	}{
		{name: "deployment readable", objects: []runtime.Object{demoDeployment()}},
		{name: "deployment missing", wantErr: true},
		{name: "only another deployment exists", objects: []runtime.Object{&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "rollback-demo", Name: "other"}}}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// A nil pool is readonly mode: PostgreSQL is not checked.
			ready := readiness(testConfig(t, config.KubernetesMCPModeReadonly), fake.NewClientset(test.objects...), nil)
			err := ready(context.Background())
			if (err != nil) != test.wantErr {
				t.Fatalf("readiness error = %v, wantErr %t", err, test.wantErr)
			}
		})
	}
}

func TestBuildHTTPServesHealthMetricsAndMCPForBothModes(t *testing.T) {
	for _, mode := range []string{config.KubernetesMCPModeReadonly, config.KubernetesMCPModeRollback} {
		t.Run(mode, func(t *testing.T) {
			cfg := testConfig(t, mode)
			clientset := fake.NewClientset()
			tel, err := telemetry.New(telemetry.Settings{ServiceName: "kubernetes-mcp-" + mode, SampleRatio: 1})
			if err != nil {
				t.Fatal(err)
			}
			backend, err := buildServices(cfg, clientset, nil)
			if err != nil {
				t.Fatal(err)
			}
			handler, err := buildHTTP(cfg, backend, readiness(cfg, clientset, nil), tel, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			get := func(path string) *httptest.ResponseRecorder {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
				return response
			}

			if live := get(httpserver.LivePath); live.Code != http.StatusOK || live.Body.String() != httpserver.HealthOKBody {
				t.Fatalf("live = %d %q", live.Code, live.Body.String())
			}
			// The fake cluster has no demo-api Deployment.
			if ready := get(httpserver.ReadyPath); ready.Code != http.StatusServiceUnavailable || ready.Body.String() != httpserver.HealthNotReadyBody {
				t.Fatalf("ready = %d %q, want the shared not-ready response", ready.Code, ready.Body.String())
			}
			metrics := get("/metrics")
			if metrics.Code != http.StatusOK || !strings.Contains(metrics.Body.String(), `rollback_guardian_http_server_requests_total{code="503",method="GET",route="/health/ready"} 1`) {
				t.Fatalf("metrics = %d, want the instrumented readiness request:\n%s", metrics.Code, metrics.Body.String())
			}
		})
	}
}
