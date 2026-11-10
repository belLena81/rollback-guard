package a2aadapter

import (
	"context"
	"errors"
	"iter"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/belLena81/rollback-guard/internal/application"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/httpserver"
	"github.com/belLena81/rollback-guard/internal/ports"
)

type ServerConfig struct {
	PublicURL       string
	TaskTimeout     time.Duration
	MaxRequestBytes int64
	Ready           func(context.Context) error
	// ReadyTimeout bounds each readiness check. Zero uses the shared default
	// (httpserver.DefaultReadyTimeout).
	ReadyTimeout time.Duration
	// Tracer continues the caller's trace for task execution, which may run
	// detached from the HTTP request. Nil disables tracing.
	Tracer ports.Tracer
	// Metrics, when set, is served at GET /metrics.
	Metrics http.Handler
}

func AgentCard(publicURL string) *a2a.AgentCard {
	extension := a2a.AgentExtension{URI: ExtensionURI, Description: "Rollback Guardian typed investigation contract", Required: true}
	return &a2a.AgentCard{
		Name:                "Rollback Guardian Investigator",
		Description:         "Collects Kubernetes rollout evidence and returns a deterministic diagnosis input",
		Version:             SchemaVersion,
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(strings.TrimRight(publicURL, "/")+"/a2a", a2a.TransportProtocolHTTPJSON)},
		Capabilities:        a2a.AgentCapabilities{Streaming: true, Extensions: []a2a.AgentExtension{extension}},
		DefaultInputModes:   []string{JSONMediaType},
		DefaultOutputModes:  []string{JSONMediaType},
		Skills: []a2a.AgentSkill{
			{
				ID:          "investigate_rollout",
				Name:        "Investigate rollout",
				Description: "Collect rollout and metric evidence and diagnose the incident",
				Tags:        []string{"kubernetes", "rollback", "investigation"},
				InputModes:  []string{JSONMediaType},
				OutputModes: []string{JSONMediaType},
			},
		},
	}
}

func NewServerHandler(investigator ports.Investigator, store taskstore.Store, logger *slog.Logger, config ServerConfig) (http.Handler, error) {
	if investigator == nil {
		return nil, errors.New("investigator is required")
	}
	if strings.TrimSpace(config.PublicURL) == "" || config.TaskTimeout <= 0 {
		return nil, errors.New("public URL and task timeout are required")
	}
	if store == nil {
		store = taskstore.NewInMemory(nil)
	}
	if logger == nil {
		logger = slog.Default()
	}
	if config.MaxRequestBytes <= 0 {
		config.MaxRequestBytes = maxDataPartBytes
	}
	if config.Tracer == nil {
		config.Tracer = ports.NoopTracer{}
	}
	card := AgentCard(config.PublicURL)
	executor := &investigationExecutor{investigator: investigator, timeout: config.TaskTimeout, logger: logger, tracer: config.Tracer}
	handler := a2asrv.NewHandler(
		executor,
		a2asrv.WithTaskStore(store),
		a2asrv.WithCapabilityChecks(&card.Capabilities),
		a2asrv.WithAgentInactivityTimeout(config.TaskTimeout),
		a2asrv.WithLogger(logger),
	)
	rest := http.MaxBytesHandler(a2asrv.NewRESTHandler(handler), config.MaxRequestBytes)
	mux := http.NewServeMux()
	mux.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(card))
	mux.Handle("/a2a/", http.StripPrefix("/a2a", rest))
	httpserver.NewHealth(config.Ready, config.ReadyTimeout, logger).Register(mux)
	if config.Metrics != nil {
		mux.Handle("GET /metrics", config.Metrics)
	}
	return mux, nil
}

// RouteOf maps requests to the fixed routes served by NewServerHandler, for
// HTTP telemetry labels.
func RouteOf(request *http.Request) string {
	path := request.URL.Path
	switch {
	case path == a2asrv.WellKnownAgentCardPath, path == httpserver.LivePath, path == httpserver.ReadyPath, path == "/metrics":
		return path
	case strings.HasPrefix(path, "/a2a/"):
		return "/a2a/*"
	default:
		return "unmatched"
	}
}

type investigationExecutor struct {
	investigator ports.Investigator
	timeout      time.Duration
	logger       *slog.Logger
	tracer       ports.Tracer
}

func (e *investigationExecutor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if execCtx.StoredTask == nil && !yield(a2a.NewSubmittedTask(execCtx, execCtx.Message), nil) {
			return
		}
		if execCtx.Message == nil || len(execCtx.Message.Parts) != 1 || execCtx.Message.Parts[0].Data() == nil ||
			execCtx.Message.Parts[0].MediaType != JSONMediaType {
			yield(failedEvent(execCtx, "invalid_request"), nil)
			return
		}
		request, err := decodeRequest(execCtx.Message.Parts[0].Data())
		if err != nil {
			e.logger.WarnContext(ctx, "rejected investigation request", "error", err)
			yield(failedEvent(execCtx, "invalid_request"), nil)
			return
		}
		var correlation correlationDTO
		if err := strictDecode(
			execCtx.Metadata[ExtensionURI],
			&correlation,
		); err != nil || correlation.SchemaVersion != SchemaVersion || correlation.IdempotencyKey == "" ||
			correlation.RequestHash == "" {
			e.logger.WarnContext(ctx, "rejected investigation correlation metadata", "error", err)
			yield(failedEvent(execCtx, "invalid_correlation"), nil)
			return
		}
		if correlation.RequestHash != application.InvestigationRequestHash(request.Incident) {
			e.logger.WarnContext(ctx, "rejected investigation request hash mismatch")
			yield(failedEvent(execCtx, "request_hash_mismatch"), nil)
			return
		}
		request.IdempotencyKey = correlation.IdempotencyKey
		request.RequestHash = correlation.RequestHash
		request.OperationID = correlation.OperationID
		request.TaskID = string(execCtx.TaskID)
		request.TraceID = correlation.TraceID
		ctx = ports.WithCorrelation(
			ctx,
			ports.Correlation{
				IncidentID:     request.Incident.ID(),
				OperationID:    request.OperationID,
				TaskID:         request.TaskID,
				TraceID:        request.TraceID,
				IdempotencyKey: request.IdempotencyKey,
			},
		)
		ctx, endSpan := e.tracer.StartSpan(ctx, "investigation.execute", request.TraceID)
		var executeErr error
		defer func() { endSpan(executeErr) }()
		e.logger.InfoContext(ctx, "investigation task started")
		request.Progress = func(stage domain.InvestigationStage) error {
			data, err := dataMap(progressDTO{SchemaVersion: SchemaVersion, Stage: stage.String()})
			if err != nil {
				return err
			}
			part := a2a.NewDataPart(data)
			part.MediaType = JSONMediaType
			message := a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx, part)
			if !yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateWorking, message), nil) {
				return context.Canceled
			}
			return nil
		}
		taskCtx, cancel := context.WithTimeout(ctx, e.timeout)
		defer cancel()
		result, err := e.investigator.Investigate(taskCtx, request)
		if err != nil {
			executeErr = err
			e.logger.ErrorContext(ctx, "investigation task failed", "error", err)
			yield(failedEvent(execCtx, "investigation_failed"), nil)
			return
		}
		data, err := dataMap(encodeResult(request.Incident.ID(), result))
		if err != nil {
			executeErr = err
			yield(failedEvent(execCtx, "encoding_failed"), nil)
			return
		}
		e.logger.InfoContext(
			ctx,
			"investigation task completed",
			"recommendation",
			result.Conclusion.Recommendation().String(),
			"strategy",
			result.Conclusion.Strategy().String(),
		)
		part := a2a.NewDataPart(data)
		part.MediaType = JSONMediaType
		artifact := a2a.NewArtifactEvent(execCtx, part)
		artifact.Artifact.Name = ResultArtifactName
		artifact.Artifact.Description = "Structured rollout evidence and diagnosis"
		artifact.LastChunk = true
		if !yield(artifact, nil) {
			return
		}
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCompleted, nil), nil)
	}
}

func (e *investigationExecutor) Cancel(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
	}
}

func failedEvent(execCtx *a2asrv.ExecutorContext, code string) *a2a.TaskStatusUpdateEvent {
	part := a2a.NewDataPart(map[string]any{"schemaVersion": SchemaVersion, "code": code})
	part.MediaType = JSONMediaType
	return a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateFailed, a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx, part))
}

var _ a2asrv.AgentExecutor = (*investigationExecutor)(nil)
