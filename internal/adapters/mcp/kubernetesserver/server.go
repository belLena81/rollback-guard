package kubernetesserver

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/belLena81/rollback-guard/internal/adapters/kubernetes"
	protocol "github.com/belLena81/rollback-guard/internal/adapters/mcp"
	"github.com/belLena81/rollback-guard/internal/domain"
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
	ExecuteApprovedRollback(context.Context, domain.ApprovedRollback, domain.IdempotencyKey) (kubernetes.MutationResult, error)
}

func New(backend Backend, mode Mode) (*mcp.Server, error) {
	if backend == nil || (mode != ModeReadonly && mode != ModeRollback) {
		return nil, &domain.ValidationError{Field: "kubernetes_mcp", Reason: "invalid backend or mode"}
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "rollback-guardian-kubernetes-" + string(mode), Version: "v0.3.0"}, nil)
	if mode == ModeReadonly {
		registerReadonly(server, backend)
	} else {
		registerRollback(server, backend)
	}
	return server, nil
}

func NewHTTPHandler(backend Backend, mode Mode, ready func(context.Context) error, sessionTimeout time.Duration, logger *slog.Logger) (http.Handler, error) {
	server, err := New(backend, mode)
	if err != nil {
		return nil, err
	}
	return protocol.HTTPHandler(server, ready, protocol.DefaultMaxRequestBytes, sessionTimeout, logger), nil
}

func registerReadonly(server *mcp.Server, backend Backend) {
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: boolPointer(false), IdempotentHint: true, OpenWorldHint: boolPointer(false)}
	mcp.AddTool(server, &mcp.Tool{Name: protocol.ToolGetDeployment, Description: "Read one allowed Deployment summary", Annotations: readOnly}, getDeployment(backend))
	mcp.AddTool(server, &mcp.Tool{Name: protocol.ToolGetReplicaSets, Description: "Read owned ReplicaSet summaries", Annotations: readOnly}, getReplicaSets(backend))
	mcp.AddTool(server, &mcp.Tool{Name: protocol.ToolListPods, Description: "List bounded pod health summaries for one allowed Deployment", Annotations: readOnly}, listPods(backend))
	mcp.AddTool(server, &mcp.Tool{Name: protocol.ToolGetPodEvents, Description: "Read bounded events for a pod owned by one allowed Deployment", Annotations: readOnly}, getPodEvents(backend))
	mcp.AddTool(server, &mcp.Tool{Name: protocol.ToolGetRolloutStatus, Description: "Read rollout status for one allowed Deployment", Annotations: readOnly}, getRolloutStatus(backend))
	mcp.AddTool(server, &mcp.Tool{Name: protocol.ToolWatchRollout, Description: "Wait a bounded time for one expected Deployment template revision", Annotations: readOnly}, watchRollout(backend))
}

func registerRollback(server *mcp.Server, backend Backend) {
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: boolPointer(false), IdempotentHint: true, OpenWorldHint: boolPointer(false)}
	mutating := &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPointer(true), IdempotentHint: true, OpenWorldHint: boolPointer(false)}
	mcp.AddTool(server, &mcp.Tool{Name: protocol.ToolPrepareRollback, Description: "Validate an immutable rollback proposal without mutation", Annotations: readOnly}, prepareRollback(backend))
	mcp.AddTool(server, &mcp.Tool{Name: protocol.ToolExecuteApprovedRollback, Description: "Execute one exactly approved rollback against an allowed Deployment", Annotations: mutating}, executeRollback(backend))
}

func getDeployment(backend Backend) mcp.ToolHandlerFor[protocol.TargetInput, protocol.Response[protocol.DeploymentOutput]] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, input protocol.TargetInput) (*mcp.CallToolResult, protocol.Response[protocol.DeploymentOutput], error) {
		target, err := input.Target.Domain()
		if err != nil {
			result, response := protocol.Failure[protocol.DeploymentOutput](err)
			return result, response, nil
		}
		evidence, err := backend.InspectDeployment(ctx, target)
		if err != nil {
			result, response := protocol.Failure[protocol.DeploymentOutput](err)
			return result, response, nil
		}
		output := protocol.DeploymentOutput{Target: protocol.TargetFromDomain(evidence.Target), CurrentRevision: string(evidence.CurrentRevision), PreviousStableRevision: string(evidence.PreviousStableRevision), DesiredReplicas: evidence.DesiredReplicas, ReadyReplicas: evidence.ReadyReplicas, AvailableReplicas: evidence.AvailableReplicas, Healthy: evidence.Healthy, RolloutAt: evidence.RolloutAt}
		return nil, protocol.Success(output), nil
	}
}

func getReplicaSets(backend Backend) mcp.ToolHandlerFor[protocol.TargetInput, protocol.Response[protocol.ReplicaSetsOutput]] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, input protocol.TargetInput) (*mcp.CallToolResult, protocol.Response[protocol.ReplicaSetsOutput], error) {
		target, err := input.Target.Domain()
		if err != nil {
			result, response := protocol.Failure[protocol.ReplicaSetsOutput](err)
			return result, response, nil
		}
		items, err := backend.ReplicaSets(ctx, target)
		if err != nil {
			result, response := protocol.Failure[protocol.ReplicaSetsOutput](err)
			return result, response, nil
		}
		output := protocol.ReplicaSetsOutput{Items: make([]protocol.ReplicaSetOutput, len(items))}
		for index, item := range items {
			output.Items[index] = protocol.ReplicaSetOutput{Name: item.Name, UID: item.UID, Revision: string(item.Revision), Stable: item.Stable, DesiredReplicas: item.DesiredReplicas, ReadyReplicas: item.ReadyReplicas, AvailableReplicas: item.AvailableReplicas, CreatedAt: item.CreatedAt}
		}
		return nil, protocol.Success(output), nil
	}
}

func listPods(backend Backend) mcp.ToolHandlerFor[protocol.ListInput, protocol.Response[protocol.PodsOutput]] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, input protocol.ListInput) (*mcp.CallToolResult, protocol.Response[protocol.PodsOutput], error) {
		target, err := input.Target.Domain()
		if err != nil {
			result, response := protocol.Failure[protocol.PodsOutput](err)
			return result, response, nil
		}
		items, truncated, err := backend.ListPods(ctx, target, input.Limit)
		if err != nil {
			result, response := protocol.Failure[protocol.PodsOutput](err)
			return result, response, nil
		}
		output := protocol.PodsOutput{Items: make([]protocol.PodOutput, len(items)), Truncated: truncated}
		for index, item := range items {
			output.Items[index] = protocol.PodOutput{Name: item.Name, UID: item.UID, Phase: item.Phase, Ready: item.Ready, Restarts: item.Restarts, CreatedAt: item.CreatedAt}
		}
		return nil, protocol.Success(output), nil
	}
}

func getPodEvents(backend Backend) mcp.ToolHandlerFor[protocol.PodEventsInput, protocol.Response[protocol.EventsOutput]] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, input protocol.PodEventsInput) (*mcp.CallToolResult, protocol.Response[protocol.EventsOutput], error) {
		target, err := input.Target.Domain()
		if err != nil {
			result, response := protocol.Failure[protocol.EventsOutput](err)
			return result, response, nil
		}
		items, truncated, err := backend.PodEvents(ctx, target, input.PodName, input.Limit)
		if err != nil {
			result, response := protocol.Failure[protocol.EventsOutput](err)
			return result, response, nil
		}
		output := protocol.EventsOutput{Items: make([]protocol.EventOutput, len(items)), Truncated: truncated}
		for index, item := range items {
			output.Items[index] = protocol.EventOutput{Reason: item.Reason, Type: item.Type, Message: item.Message, Count: item.Count, ObservedAt: item.ObservedAt}
		}
		return nil, protocol.Success(output), nil
	}
}

func getRolloutStatus(backend Backend) mcp.ToolHandlerFor[protocol.TargetInput, protocol.Response[protocol.RolloutOutput]] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, input protocol.TargetInput) (*mcp.CallToolResult, protocol.Response[protocol.RolloutOutput], error) {
		target, err := input.Target.Domain()
		if err != nil {
			result, response := protocol.Failure[protocol.RolloutOutput](err)
			return result, response, nil
		}
		status, err := backend.GetRolloutStatus(ctx, target)
		if err != nil {
			result, response := protocol.Failure[protocol.RolloutOutput](err)
			return result, response, nil
		}
		return nil, protocol.Success(rolloutOutput(status)), nil
	}
}

func watchRollout(backend Backend) mcp.ToolHandlerFor[protocol.WatchRolloutInput, protocol.Response[protocol.RolloutOutput]] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, input protocol.WatchRolloutInput) (*mcp.CallToolResult, protocol.Response[protocol.RolloutOutput], error) {
		if input.ExpectedRevision == "" || input.TimeoutSeconds < 1 || input.TimeoutSeconds > 120 {
			result, response := protocol.Failure[protocol.RolloutOutput](&domain.ValidationError{Field: "watch_rollout", Reason: "revision and timeout are invalid"})
			return result, response, nil
		}
		target, err := input.Target.Domain()
		if err != nil {
			result, response := protocol.Failure[protocol.RolloutOutput](err)
			return result, response, nil
		}
		watchCtx, cancel := context.WithTimeout(ctx, time.Duration(input.TimeoutSeconds)*time.Second)
		defer cancel()
		status, err := backend.WatchRollout(watchCtx, target, domain.Revision(input.ExpectedRevision))
		if err != nil {
			result, response := protocol.Failure[protocol.RolloutOutput](err)
			return result, response, nil
		}
		return nil, protocol.Success(rolloutOutput(status)), nil
	}
}

func prepareRollback(backend Backend) mcp.ToolHandlerFor[protocol.PrepareRollbackInput, protocol.Response[protocol.PrepareRollbackOutput]] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, input protocol.PrepareRollbackInput) (*mcp.CallToolResult, protocol.Response[protocol.PrepareRollbackOutput], error) {
		proposal, err := input.Proposal.Domain()
		if err == nil {
			err = backend.PrepareRollback(ctx, proposal)
		}
		if err != nil {
			result, response := protocol.Failure[protocol.PrepareRollbackOutput](err)
			return result, response, nil
		}
		return nil, protocol.Success(protocol.PrepareRollbackOutput{ProposalHash: string(proposal.Hash()), TargetRevision: string(proposal.TargetRevision())}), nil
	}
}

func executeRollback(backend Backend) mcp.ToolHandlerFor[protocol.ExecuteRollbackInput, protocol.Response[protocol.ExecuteRollbackOutput]] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, input protocol.ExecuteRollbackInput) (*mcp.CallToolResult, protocol.Response[protocol.ExecuteRollbackOutput], error) {
		proposal, err := input.Proposal.Domain()
		if err != nil {
			result, response := protocol.Failure[protocol.ExecuteRollbackOutput](err)
			return result, response, nil
		}
		decision, err := input.PolicyDecision.Domain()
		if err != nil {
			result, response := protocol.Failure[protocol.ExecuteRollbackOutput](err)
			return result, response, nil
		}
		approval, err := input.Approval.Domain()
		if err != nil {
			result, response := protocol.Failure[protocol.ExecuteRollbackOutput](err)
			return result, response, nil
		}
		fresh, err := backend.InspectDeployment(ctx, proposal.Target())
		if err != nil {
			result, response := protocol.Failure[protocol.ExecuteRollbackOutput](err)
			return result, response, nil
		}
		approved, err := domain.AuthorizeRollback(proposal, decision, approval, fresh, time.Now())
		if err != nil {
			result, response := protocol.Failure[protocol.ExecuteRollbackOutput](err)
			return result, response, nil
		}
		mutation, err := backend.ExecuteApprovedRollback(ctx, approved, domain.IdempotencyKey(input.IdempotencyKey))
		if err != nil {
			result, response := protocol.Failure[protocol.ExecuteRollbackOutput](err)
			return result, response, nil
		}
		output := protocol.ExecuteRollbackOutput{ExecutionID: string(mutation.ExecutionID), FromRevision: string(mutation.FromRevision), ToRevision: string(mutation.ToRevision), StartedAt: mutation.StartedAt, CompletedAt: mutation.CompletedAt, AlreadyApplied: mutation.AlreadyApplied}
		return nil, protocol.Success(output), nil
	}
}

func rolloutOutput(status kubernetes.RolloutStatus) protocol.RolloutOutput {
	return protocol.RolloutOutput{Revision: string(status.Revision), DesiredReplicas: status.DesiredReplicas, UpdatedReplicas: status.UpdatedReplicas, ReadyReplicas: status.ReadyReplicas, AvailableReplicas: status.AvailableReplicas, ObservedGeneration: status.ObservedGeneration, Generation: status.Generation, Complete: status.Complete}
}

func boolPointer(value bool) *bool {
	return &value
}
