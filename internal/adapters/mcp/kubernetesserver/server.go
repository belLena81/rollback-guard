package kubernetesserver

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/belLena81/rollback-guard/internal/adapters/kubernetes"
	protocol "github.com/belLena81/rollback-guard/internal/adapters/mcp"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/buildinfo"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Mode string

const (
	ModeReadonly Mode = "readonly"
	ModeRollback Mode = "rollback"
)

type Backend interface {
	InspectDeployment(context.Context, domain.DeploymentTarget) (domain.DeploymentEvidence, error)
	ReplicaSets(context.Context, domain.DeploymentTarget) ([]kubernetes.ReplicaSetSummary, error)
	ListPods(context.Context, domain.DeploymentTarget, int) ([]kubernetes.PodSummary, bool, error)
	PodEvents(context.Context, domain.DeploymentTarget, string, int) ([]kubernetes.EventSummary, bool, error)
	GetRolloutStatus(context.Context, domain.DeploymentTarget) (kubernetes.RolloutStatus, error)
	WatchRollout(context.Context, domain.DeploymentTarget, domain.Revision) (kubernetes.RolloutStatus, error)
	PrepareRollback(context.Context, domain.RollbackProposal) error
	ExecuteApprovedRollback(context.Context, domain.ProposalHash, domain.IdempotencyKey) (kubernetes.MutationResult, error)
}

func New(backend Backend, mode Mode) (*mcp.Server, error) {
	if backend == nil || (mode != ModeReadonly && mode != ModeRollback) {
		return nil, &domain.ValidationError{Field: "kubernetes_mcp", Reason: "invalid backend or mode"}
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "rollback-guardian-kubernetes-" + string(mode), Version: buildinfo.Version}, nil)
	if mode == ModeReadonly {
		registerReadonly(server, backend)
	} else {
		registerRollback(server, backend)
	}
	return server, nil
}

func NewHTTPHandler(backend Backend, mode Mode, ready func(context.Context) error, sessionTimeout time.Duration, logger *slog.Logger, opts ...protocol.HandlerOption) (http.Handler, error) {
	server, err := New(backend, mode)
	if err != nil {
		return nil, err
	}
	return protocol.HTTPHandler(server, ready, protocol.DefaultMaxRequestBytes, sessionTimeout, logger, opts...), nil
}

func registerReadonly(server *mcp.Server, backend Backend) {
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: new(false), IdempotentHint: true, OpenWorldHint: new(false)}
	mcp.AddTool(server, &mcp.Tool{Name: protocol.ToolGetDeployment, Description: "Read one allowed Deployment summary", Annotations: readOnly}, getDeployment(backend))
	mcp.AddTool(server, &mcp.Tool{Name: protocol.ToolGetReplicaSets, Description: "Read owned ReplicaSet summaries", Annotations: readOnly}, getReplicaSets(backend))
	mcp.AddTool(server, &mcp.Tool{Name: protocol.ToolListPods, Description: "List bounded pod health summaries for one allowed Deployment", Annotations: readOnly}, listPods(backend))
	mcp.AddTool(server, &mcp.Tool{Name: protocol.ToolGetPodEvents, Description: "Read bounded events for a pod owned by one allowed Deployment", Annotations: readOnly}, getPodEvents(backend))
	mcp.AddTool(server, &mcp.Tool{Name: protocol.ToolGetRolloutStatus, Description: "Read rollout status for one allowed Deployment", Annotations: readOnly}, getRolloutStatus(backend))
	mcp.AddTool(server, &mcp.Tool{Name: protocol.ToolWatchRollout, Description: "Wait a bounded time for one expected Deployment template revision", Annotations: readOnly}, watchRollout(backend))
}

func registerRollback(server *mcp.Server, backend Backend) {
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: new(false), IdempotentHint: true, OpenWorldHint: new(false)}
	mutating := &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: new(true), IdempotentHint: true, OpenWorldHint: new(false)}
	mcp.AddTool(server, &mcp.Tool{Name: protocol.ToolPrepareRollback, Description: "Validate an immutable rollback proposal without mutation", Annotations: readOnly}, prepareRollback(backend))
	mcp.AddTool(server, &mcp.Tool{Name: protocol.ToolExecuteApprovedRollback, Description: "Execute one exactly approved rollback against an allowed Deployment", Annotations: mutating}, executeRollback(backend))
}
