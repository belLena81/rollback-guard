package a2aadapter

import (
	"context"
	"fmt"
	"net/http"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	"github.com/belLena81/rollback-guard/internal/ports"
)

type Gateway struct{ client *a2aclient.Client }

func NewGateway(ctx context.Context, discoveryURL string, httpClient *http.Client) (*Gateway, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	card, err := agentcard.NewResolver(httpClient).Resolve(ctx, discoveryURL)
	if err != nil {
		return nil, fmt.Errorf("resolve investigator agent card: %w", err)
	}
	if err := validateAgentCard(card); err != nil {
		return nil, err
	}
	client, err := a2aclient.NewFromCard(ctx, card, a2aclient.WithDefaultsDisabled(), a2aclient.WithRESTTransport(httpClient), a2aclient.WithConfig(a2aclient.Config{PreferredTransports: []a2a.TransportProtocol{a2a.TransportProtocolHTTPJSON}, AcceptedOutputModes: []string{JSONMediaType}}))
	if err != nil {
		return nil, fmt.Errorf("create investigator A2A client: %w", err)
	}
	return &Gateway{client: client}, nil
}

func validateAgentCard(card *a2a.AgentCard) error {
	if card == nil {
		return fmt.Errorf("investigator agent card is empty")
	}
	foundExtension := false
	for _, extension := range card.Capabilities.Extensions {
		if extension.URI == ExtensionURI {
			foundExtension = true
		}
	}
	if !foundExtension {
		return fmt.Errorf("investigator agent card does not support required extension %q", ExtensionURI)
	}
	foundSkill := false
	for _, skill := range card.Skills {
		if skill.ID == "investigate_rollout" {
			foundSkill = true
		}
	}
	if !foundSkill {
		return fmt.Errorf("investigator agent card does not expose investigate_rollout")
	}
	return nil
}

func extensionContext(ctx context.Context) context.Context {
	return a2aclient.AttachServiceParams(ctx, a2aclient.ServiceParams{a2a.SvcParamExtensions: []string{ExtensionURI}})
}

func (g *Gateway) Submit(ctx context.Context, submission ports.InvestigationSubmission) (ports.InvestigationTaskRef, error) {
	part := a2a.NewDataPart(encodeRequest(submission.Incident))
	part.MediaType = JSONMediaType
	message := a2a.NewMessage(a2a.MessageRoleUser, part)
	message.ID = submission.MessageID
	message.Extensions = []string{ExtensionURI}
	request := &a2a.SendMessageRequest{Message: message, Config: &a2a.SendMessageConfig{AcceptedOutputModes: []string{JSONMediaType}, ReturnImmediately: true}}
	request.SetMeta(ExtensionURI, correlationDTO{SchemaVersion: SchemaVersion, OperationID: submission.OperationID, IdempotencyKey: submission.IdempotencyKey, TraceID: submission.TraceID, RequestHash: submission.RequestHash})
	response, err := g.client.SendMessage(extensionContext(ctx), request)
	if err != nil {
		return ports.InvestigationTaskRef{}, fmt.Errorf("submit investigation task: %w", err)
	}
	task, ok := response.(*a2a.Task)
	if !ok {
		return ports.InvestigationTaskRef{}, fmt.Errorf("investigator returned a message instead of a task")
	}
	if task.ID == "" || task.ContextID == "" {
		return ports.InvestigationTaskRef{}, fmt.Errorf("investigator returned an incomplete task reference")
	}
	return ports.InvestigationTaskRef{TaskID: string(task.ID), ContextID: task.ContextID}, nil
}

func (g *Gateway) Watch(ctx context.Context, ref ports.InvestigationTaskRef, progress func(ports.InvestigationStage) error) (ports.InvestigationResult, error) {
	task, err := g.client.GetTask(extensionContext(ctx), &a2a.GetTaskRequest{ID: a2a.TaskID(ref.TaskID)})
	if err != nil {
		return ports.InvestigationResult{}, fmt.Errorf("get investigation task: %w", err)
	}
	if task.Status.State.Terminal() {
		return resultFromTask(task)
	}
	var result *ports.InvestigationResult
	for event, eventErr := range g.client.SubscribeToTask(extensionContext(ctx), &a2a.SubscribeToTaskRequest{ID: a2a.TaskID(ref.TaskID)}) {
		if eventErr != nil {
			latest, latestErr := g.client.GetTask(extensionContext(ctx), &a2a.GetTaskRequest{ID: a2a.TaskID(ref.TaskID)})
			if latestErr == nil && latest.Status.State.Terminal() {
				return resultFromTask(latest)
			}
			return ports.InvestigationResult{}, fmt.Errorf("watch investigation task: %w", eventErr)
		}
		switch value := event.(type) {
		case *a2a.Task:
			parsed, parseErr := resultFromArtifacts(value.Artifacts)
			if parseErr == nil {
				result = &parsed
			}
			if value.Status.State == a2a.TaskStateFailed || value.Status.State == a2a.TaskStateRejected {
				return ports.InvestigationResult{}, fmt.Errorf("investigation task failed")
			}
			if value.Status.State == a2a.TaskStateCanceled {
				return ports.InvestigationResult{}, context.Canceled
			}
		case *a2a.TaskStatusUpdateEvent:
			if value.Status.State == a2a.TaskStateWorking && value.Status.Message != nil {
				stage, parseErr := stageFromParts(value.Status.Message.Parts)
				if parseErr != nil {
					return ports.InvestigationResult{}, parseErr
				}
				if progress != nil {
					if err := progress(stage); err != nil {
						return ports.InvestigationResult{}, err
					}
				}
			}
			if value.Status.State == a2a.TaskStateFailed || value.Status.State == a2a.TaskStateRejected {
				return ports.InvestigationResult{}, fmt.Errorf("investigation task failed")
			}
			if value.Status.State == a2a.TaskStateCanceled {
				return ports.InvestigationResult{}, context.Canceled
			}
			if value.Status.State == a2a.TaskStateCompleted {
				if result == nil {
					latest, err := g.client.GetTask(extensionContext(ctx), &a2a.GetTaskRequest{ID: a2a.TaskID(ref.TaskID)})
					if err != nil {
						return ports.InvestigationResult{}, err
					}
					return resultFromTask(latest)
				}
				return *result, nil
			}
		case *a2a.TaskArtifactUpdateEvent:
			if value.Artifact == nil || value.Artifact.Name != ResultArtifactName {
				return ports.InvestigationResult{}, fmt.Errorf("unexpected investigation artifact")
			}
			parsed, parseErr := resultFromParts(value.Artifact.Parts)
			if parseErr != nil {
				return ports.InvestigationResult{}, parseErr
			}
			result = &parsed
		case *a2a.Message:
			return ports.InvestigationResult{}, fmt.Errorf("investigator returned a message instead of task events")
		default:
			return ports.InvestigationResult{}, fmt.Errorf("investigator returned an unknown event")
		}
	}
	latest, err := g.client.GetTask(extensionContext(ctx), &a2a.GetTaskRequest{ID: a2a.TaskID(ref.TaskID)})
	if err != nil {
		return ports.InvestigationResult{}, fmt.Errorf("investigation event stream ended: %w", err)
	}
	return resultFromTask(latest)
}

func (g *Gateway) Get(ctx context.Context, ref ports.InvestigationTaskRef) (ports.InvestigationTaskSnapshot, error) {
	task, err := g.client.GetTask(extensionContext(ctx), &a2a.GetTaskRequest{ID: a2a.TaskID(ref.TaskID)})
	if err != nil {
		return ports.InvestigationTaskSnapshot{}, err
	}
	state, err := mapTaskState(task.Status.State)
	if err != nil {
		return ports.InvestigationTaskSnapshot{}, err
	}
	snapshot := ports.InvestigationTaskSnapshot{Ref: ref, State: state}
	if result, err := resultFromArtifacts(task.Artifacts); err == nil {
		snapshot.Result = &result
	}
	return snapshot, nil
}

func (g *Gateway) Cancel(ctx context.Context, ref ports.InvestigationTaskRef) error {
	_, err := g.client.CancelTask(extensionContext(ctx), &a2a.CancelTaskRequest{ID: a2a.TaskID(ref.TaskID)})
	if err != nil {
		return fmt.Errorf("cancel investigation task: %w", err)
	}
	return nil
}

func (g *Gateway) Close() error { return g.client.Destroy() }

func stageFromParts(parts a2a.ContentParts) (ports.InvestigationStage, error) {
	if len(parts) != 1 || parts[0].Data() == nil || parts[0].MediaType != JSONMediaType {
		return 0, fmt.Errorf("invalid investigation progress")
	}
	var wire progressDTO
	if err := strictDecode(parts[0].Data(), &wire); err != nil {
		return 0, err
	}
	if wire.SchemaVersion != SchemaVersion {
		return 0, fmt.Errorf("unsupported progress schema")
	}
	for stage := ports.InvestigationStageQueued; stage <= ports.InvestigationStageFinalizing; stage++ {
		if stage.String() == wire.Stage {
			return stage, nil
		}
	}
	return 0, fmt.Errorf("unknown investigation stage %q", wire.Stage)
}

func resultFromTask(task *a2a.Task) (ports.InvestigationResult, error) {
	if task.Status.State == a2a.TaskStateFailed || task.Status.State == a2a.TaskStateRejected {
		return ports.InvestigationResult{}, fmt.Errorf("investigation task failed")
	}
	if task.Status.State == a2a.TaskStateCanceled {
		return ports.InvestigationResult{}, context.Canceled
	}
	if task.Status.State != a2a.TaskStateCompleted {
		return ports.InvestigationResult{}, fmt.Errorf("investigation task is not complete")
	}
	return resultFromArtifacts(task.Artifacts)
}
func resultFromArtifacts(artifacts []*a2a.Artifact) (ports.InvestigationResult, error) {
	if len(artifacts) != 1 {
		return ports.InvestigationResult{}, fmt.Errorf("result artifact count must be exactly one")
	}
	artifact := artifacts[0]
	if artifact == nil || artifact.Name != ResultArtifactName {
		return ports.InvestigationResult{}, fmt.Errorf("unexpected result artifact")
	}
	return resultFromParts(artifact.Parts)
}
func resultFromParts(parts a2a.ContentParts) (ports.InvestigationResult, error) {
	if len(parts) != 1 || parts[0].Data() == nil || parts[0].MediaType != JSONMediaType {
		return ports.InvestigationResult{}, fmt.Errorf("invalid result artifact")
	}
	return decodeResult(parts[0].Data())
}

// mapTaskState fails closed on states the Investigator never reports
// (interactive or unspecified) instead of treating them as still submitted.
func mapTaskState(state a2a.TaskState) (ports.InvestigationTaskState, error) {
	switch state {
	case a2a.TaskStateSubmitted:
		return ports.InvestigationTaskSubmitted, nil
	case a2a.TaskStateWorking:
		return ports.InvestigationTaskWorking, nil
	case a2a.TaskStateCompleted:
		return ports.InvestigationTaskCompleted, nil
	case a2a.TaskStateCanceled:
		return ports.InvestigationTaskCanceled, nil
	case a2a.TaskStateFailed, a2a.TaskStateRejected:
		return ports.InvestigationTaskFailed, nil
	case a2a.TaskStateUnspecified, a2a.TaskStateAuthRequired, a2a.TaskStateInputRequired:
		return 0, fmt.Errorf("unsupported investigation task state %q", state)
	default:
		return 0, fmt.Errorf("unknown investigation task state %q", state)
	}
}

var _ ports.InvestigationGateway = (*Gateway)(nil)
