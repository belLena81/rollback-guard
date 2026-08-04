package observabilityserver

import (
	"context"
	"testing"
	"time"

	protocol "github.com/belLena81/rollback-guard/internal/adapters/mcp"
	metricsprom "github.com/belLena81/rollback-guard/internal/adapters/prometheus"
	"github.com/belLena81/rollback-guard/internal/domain"
)

func TestEvaluateRecoveryRejectsEmptyRevisions(t *testing.T) {
	tests := []struct {
		name             string
		expectedRevision string
		observedRevision string
	}{
		{name: "missing expected", observedRevision: "revision-stable"},
		{name: "missing observed", expectedRevision: "revision-stable"},
		{name: "both missing"},
		{name: "blank expected", expectedRevision: "  ", observedRevision: "revision-stable"},
		{name: "blank observed", expectedRevision: "revision-stable", observedRevision: "  "},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := &recordingRecoveryBackend{}
			handler := evaluateRecovery(backend)

			_, response, err := handler(context.Background(), nil, protocol.RecoveryInput{
				Target:            protocol.Target{Cluster: "local", Namespace: "demo", Deployment: "demo-api", Service: "demo-api"},
				MetricKind:        "http_error_rate",
				Unit:              "ratio",
				Threshold:         0.10,
				ExpectedRevision:  test.expectedRevision,
				ObservedRevision:  test.observedRevision,
				DesiredReplicas:   1,
				ReadyReplicas:     1,
				AvailableReplicas: 1,
				Healthy:           true,
				WindowSeconds:     300,
			})
			if err != nil {
				t.Fatal(err)
			}
			if response.OK || response.Error == nil || response.Error.Code != protocol.CodeInvalidArgument {
				t.Fatalf("response = %#v, want invalid argument failure", response)
			}
			if backend.queries != 0 {
				t.Fatalf("metric queries = %d, want 0", backend.queries)
			}
		})
	}
}

type recordingRecoveryBackend struct {
	queries int
}

func (b *recordingRecoveryBackend) QueryMetric(context.Context, domain.DeploymentTarget, metricsprom.QueryRequest) (metricsprom.MetricSample, error) {
	b.queries++
	return metricsprom.MetricSample{Name: "http_error_rate", Value: 0.01, Unit: "ratio", ObservedAt: time.Now().UTC()}, nil
}
