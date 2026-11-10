package kubernetesserver

import (
	"context"
	"fmt"
	"time"

	"github.com/belLena81/rollback-guard/internal/adapters/kubernetes"
	protocol "github.com/belLena81/rollback-guard/internal/adapters/mcp"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func getDeployment(backend Backend) mcp.ToolHandlerFor[protocol.TargetInput, protocol.Response[protocol.DeploymentOutput]] {
	return protocol.Handle(func(ctx context.Context, input protocol.TargetInput) (protocol.DeploymentOutput, error) {
		target, err := input.Target.Domain()
		if err != nil {
			return protocol.DeploymentOutput{}, fmt.Errorf("convert target: %w", err)
		}
		evidence, err := backend.InspectDeployment(ctx, target)
		if err != nil {
			return protocol.DeploymentOutput{}, fmt.Errorf("inspect deployment backend: %w", err)
		}
		return protocol.DeploymentOutput{
			Target:                 protocol.TargetFromDomain(evidence.Target),
			CurrentRevision:        string(evidence.CurrentRevision),
			PreviousStableRevision: string(evidence.PreviousStableRevision),
			DesiredReplicas:        evidence.DesiredReplicas,
			ReadyReplicas:          evidence.ReadyReplicas,
			AvailableReplicas:      evidence.AvailableReplicas,
			Healthy:                evidence.Healthy,
			RolloutAt:              evidence.RolloutAt,
		}, nil
	})
}

func getReplicaSets(backend Backend) mcp.ToolHandlerFor[protocol.TargetInput, protocol.Response[protocol.ReplicaSetsOutput]] {
	return protocol.Handle(func(ctx context.Context, input protocol.TargetInput) (protocol.ReplicaSetsOutput, error) {
		target, err := input.Target.Domain()
		if err != nil {
			return protocol.ReplicaSetsOutput{}, fmt.Errorf("convert target: %w", err)
		}
		items, err := backend.ReplicaSets(ctx, target)
		if err != nil {
			return protocol.ReplicaSetsOutput{}, fmt.Errorf("replica sets backend: %w", err)
		}
		output := protocol.ReplicaSetsOutput{Items: make([]protocol.ReplicaSetOutput, len(items))}
		for index, item := range items {
			output.Items[index] = protocol.ReplicaSetOutput{
				Name:              item.Name,
				UID:               item.UID,
				Revision:          string(item.Revision),
				Stable:            item.Stable,
				DesiredReplicas:   item.DesiredReplicas,
				ReadyReplicas:     item.ReadyReplicas,
				AvailableReplicas: item.AvailableReplicas,
				CreatedAt:         item.CreatedAt,
			}
		}
		return output, nil
	})
}

func listPods(backend Backend) mcp.ToolHandlerFor[protocol.ListInput, protocol.Response[protocol.PodsOutput]] {
	return protocol.Handle(func(ctx context.Context, input protocol.ListInput) (protocol.PodsOutput, error) {
		target, err := input.Target.Domain()
		if err != nil {
			return protocol.PodsOutput{}, fmt.Errorf("convert target: %w", err)
		}
		items, truncated, err := backend.ListPods(ctx, target, input.Limit)
		if err != nil {
			return protocol.PodsOutput{}, fmt.Errorf("list pods backend: %w", err)
		}
		output := protocol.PodsOutput{Items: make([]protocol.PodOutput, len(items)), Truncated: truncated}
		for index, item := range items {
			output.Items[index] = protocol.PodOutput{
				Name:      item.Name,
				UID:       item.UID,
				Phase:     item.Phase,
				Ready:     item.Ready,
				Restarts:  item.Restarts,
				CreatedAt: item.CreatedAt,
			}
		}
		return output, nil
	})
}

func getPodEvents(backend Backend) mcp.ToolHandlerFor[protocol.PodEventsInput, protocol.Response[protocol.EventsOutput]] {
	return protocol.Handle(func(ctx context.Context, input protocol.PodEventsInput) (protocol.EventsOutput, error) {
		target, err := input.Target.Domain()
		if err != nil {
			return protocol.EventsOutput{}, fmt.Errorf("convert target: %w", err)
		}
		items, truncated, err := backend.PodEvents(ctx, target, input.PodName, input.Limit)
		if err != nil {
			return protocol.EventsOutput{}, fmt.Errorf("pod events backend: %w", err)
		}
		output := protocol.EventsOutput{Items: make([]protocol.EventOutput, len(items)), Truncated: truncated}
		for index, item := range items {
			output.Items[index] = protocol.EventOutput{
				Reason:     item.Reason,
				Type:       item.Type,
				Message:    item.Message,
				Count:      item.Count,
				ObservedAt: item.ObservedAt,
			}
		}
		return output, nil
	})
}

func getRolloutStatus(backend Backend) mcp.ToolHandlerFor[protocol.TargetInput, protocol.Response[protocol.RolloutOutput]] {
	return protocol.Handle(func(ctx context.Context, input protocol.TargetInput) (protocol.RolloutOutput, error) {
		target, err := input.Target.Domain()
		if err != nil {
			return protocol.RolloutOutput{}, fmt.Errorf("convert target: %w", err)
		}
		status, err := backend.GetRolloutStatus(ctx, target)
		if err != nil {
			return protocol.RolloutOutput{}, fmt.Errorf("get rollout status backend: %w", err)
		}
		return rolloutOutput(status), nil
	})
}

func watchRollout(backend Backend) mcp.ToolHandlerFor[protocol.WatchRolloutInput, protocol.Response[protocol.RolloutOutput]] {
	return protocol.Handle(func(ctx context.Context, input protocol.WatchRolloutInput) (protocol.RolloutOutput, error) {
		if input.ExpectedRevision == "" || !protocol.ValidWatchSeconds(input.TimeoutSeconds) {
			return protocol.RolloutOutput{}, &domain.ValidationError{Field: "watch_rollout", Reason: "revision and timeout are invalid"}
		}
		target, err := input.Target.Domain()
		if err != nil {
			return protocol.RolloutOutput{}, fmt.Errorf("convert target: %w", err)
		}
		watchCtx, cancel := context.WithTimeout(ctx, time.Duration(input.TimeoutSeconds)*time.Second)
		defer cancel()
		status, err := backend.WatchRollout(watchCtx, target, domain.Revision(input.ExpectedRevision))
		if err != nil {
			return protocol.RolloutOutput{}, fmt.Errorf("watch rollout backend: %w", err)
		}
		return rolloutOutput(status), nil
	})
}

func prepareRollback(backend Backend) mcp.ToolHandlerFor[protocol.PrepareRollbackInput, protocol.Response[protocol.PrepareRollbackOutput]] {
	return protocol.Handle(func(ctx context.Context, input protocol.PrepareRollbackInput) (protocol.PrepareRollbackOutput, error) {
		proposal, err := input.Proposal.Domain()
		if err == nil {
			err = backend.PrepareRollback(ctx, proposal)
		}
		if err != nil {
			return protocol.PrepareRollbackOutput{}, fmt.Errorf("prepare rollback backend: %w", err)
		}
		return protocol.PrepareRollbackOutput{ProposalHash: string(proposal.Hash()), TargetRevision: string(proposal.TargetRevision())}, nil
	})
}

func executeRollback(backend Backend) mcp.ToolHandlerFor[protocol.ExecuteRollbackInput, protocol.Response[protocol.ExecuteRollbackOutput]] {
	return protocol.Handle(func(ctx context.Context, input protocol.ExecuteRollbackInput) (protocol.ExecuteRollbackOutput, error) {
		if input.ProposalHash == "" || input.IdempotencyKey == "" {
			return protocol.ExecuteRollbackOutput{}, &domain.ValidationError{
				Field:  "rollback_mutation",
				Reason: "proposal hash and idempotency key are required",
			}
		}
		mutation, err := backend.ExecuteApprovedRollback(ctx, domain.ProposalHash(input.ProposalHash), domain.IdempotencyKey(input.IdempotencyKey))
		if err != nil {
			return protocol.ExecuteRollbackOutput{}, fmt.Errorf("execute approved rollback backend: %w", err)
		}
		return protocol.ExecuteRollbackOutput{
			ExecutionID:    string(mutation.ExecutionID),
			FromRevision:   string(mutation.FromRevision),
			ToRevision:     string(mutation.ToRevision),
			StartedAt:      mutation.StartedAt,
			CompletedAt:    mutation.CompletedAt,
			AlreadyApplied: mutation.AlreadyApplied,
		}, nil
	})
}

func rolloutOutput(status kubernetes.RolloutStatus) protocol.RolloutOutput {
	return protocol.RolloutOutput{
		Revision:           string(status.Revision),
		DesiredReplicas:    status.DesiredReplicas,
		UpdatedReplicas:    status.UpdatedReplicas,
		ReadyReplicas:      status.ReadyReplicas,
		AvailableReplicas:  status.AvailableReplicas,
		ObservedGeneration: status.ObservedGeneration,
		Generation:         status.Generation,
		Complete:           status.Complete,
	}
}
