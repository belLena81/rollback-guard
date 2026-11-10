package a2aadapter

import (
	"context"
	"fmt"
	"net/http"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	"github.com/belLena81/rollback-guard/internal/domain"
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

func (g *Gateway) Submit(ctx context.Context, submission ports.InvestigationSubmission) (domain.InvestigationTaskRef, error) {
	part := a2a.NewDataPart(encodeRequest(submission.Incident))
	part.MediaType = JSONMediaType
	message := a2a.NewMessage(a2a.MessageRoleUser, part)
	message.ID = submission.MessageID
	message.Extensions = []string{ExtensionURI}
	request := &a2a.SendMessageRequest{Message: message, Config: &a2a.SendMessageConfig{AcceptedOutputModes: []string{JSONMediaType}, ReturnImmediately: true}}
	request.SetMeta(ExtensionURI, correlationDTO{SchemaVersion: SchemaVersion, OperationID: submission.OperationID, IdempotencyKey: submission.IdempotencyKey, TraceID: submission.TraceID, RequestHash: submission.RequestHash})
	response, err := g.client.SendMessage(extensionContext(ctx), request)
	if err != nil {
		return domain.InvestigationTaskRef{}, fmt.Errorf("submit investigation task: %w", err)
	}
	task, ok := response.(*a2a.Task)
	if !ok {
		return domain.InvestigationTaskRef{}, fmt.Errorf("investigator returned a message instead of a task")
	}
	if task.ID == "" || task.ContextID == "" {
		return domain.InvestigationTaskRef{}, fmt.Errorf("investigator returned an incomplete task reference")
	}
	return domain.InvestigationTaskRef{TaskID: string(task.ID), ContextID: task.ContextID}, nil
}

func (g *Gateway) Watch(ctx context.Context, ref domain.InvestigationTaskRef, progress func(domain.InvestigationStage) error) (ports.InvestigationResult, error) {
	task, err := g.client.GetTask(extensionContext(ctx), &a2a.GetTaskRequest{ID: a2a.TaskID(ref.TaskID)})
	if err != nil {
		return ports.InvestigationResult{}, fmt.Errorf("get investigation task: %w", err)
	}
	if task.Status.State.Terminal() {
		return resultFromTask(task)
	}
	watch := &taskWatch{gateway: g, ctx: ctx, ref: ref, progress: progress}
	for event, eventErr := range g.client.SubscribeToTask(extensionContext(ctx), &a2a.SubscribeToTaskRequest{ID: a2a.TaskID(ref.TaskID)}) {
		if eventErr != nil {
			return g.resultAfterStreamError(ctx, ref, eventErr)
		}
		if final, err := watch.observe(event); final != nil || err != nil {
			if err != nil {
				return ports.InvestigationResult{}, err
			}
			return *final, nil
		}
	}
	latest, err := g.client.GetTask(extensionContext(ctx), &a2a.GetTaskRequest{ID: a2a.TaskID(ref.TaskID)})
	if err != nil {
		return ports.InvestigationResult{}, fmt.Errorf("investigation event stream ended: %w", err)
	}
	return resultFromTask(latest)
}

// resultAfterStreamError prefers the task's terminal state over a broken
// event stream: the task may have finished while the stream failed.
func (g *Gateway) resultAfterStreamError(ctx context.Context, ref domain.InvestigationTaskRef, streamErr error) (ports.InvestigationResult, error) {
	latest, latestErr := g.client.GetTask(extensionContext(ctx), &a2a.GetTaskRequest{ID: a2a.TaskID(ref.TaskID)})
	if latestErr == nil && latest.Status.State.Terminal() {
		return resultFromTask(latest)
	}
	return ports.InvestigationResult{}, fmt.Errorf("watch investigation task: %w", streamErr)
}

// taskWatch folds the events of one task subscription into a result.
type taskWatch struct {
	gateway  *Gateway
	ctx      context.Context
	ref      domain.InvestigationTaskRef
	progress func(domain.InvestigationStage) error
	result   *ports.InvestigationResult
}

// observe handles one event. It returns the final result, or an error, once
// the watch is over; it returns neither while the task is still running.
func (w *taskWatch) observe(event a2a.Event) (*ports.InvestigationResult, error) {
	switch value := event.(type) {
	case *a2a.Task:
		if parsed, parseErr := resultFromArtifacts(value.Artifacts); parseErr == nil {
			w.result = &parsed
		}
		return nil, failureOfState(value.Status.State)
	case *a2a.TaskStatusUpdateEvent:
		return w.observeStatus(value)
	case *a2a.TaskArtifactUpdateEvent:
		if value.Artifact == nil || value.Artifact.Name != ResultArtifactName {
			return nil, fmt.Errorf("unexpected investigation artifact")
		}
		parsed, err := resultFromParts(value.Artifact.Parts)
		if err != nil {
			return nil, err
		}
		w.result = &parsed
		return nil, nil
	case *a2a.Message:
		return nil, fmt.Errorf("investigator returned a message instead of task events")
	default:
		return nil, fmt.Errorf("investigator returned an unknown event")
	}
}

func (w *taskWatch) observeStatus(update *a2a.TaskStatusUpdateEvent) (*ports.InvestigationResult, error) {
	state := update.Status.State
	if state == a2a.TaskStateWorking && update.Status.Message != nil {
		if err := w.reportProgress(update.Status.Message.Parts); err != nil {
			return nil, err
		}
	}
	if err := failureOfState(state); err != nil {
		return nil, err
	}
	if state != a2a.TaskStateCompleted {
		return nil, nil
	}
	if w.result != nil {
		return w.result, nil
	}
	latest, err := w.gateway.client.GetTask(extensionContext(w.ctx), &a2a.GetTaskRequest{ID: a2a.TaskID(w.ref.TaskID)})
	if err != nil {
		return nil, err
	}
	result, err := resultFromTask(latest)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func (w *taskWatch) reportProgress(parts a2a.ContentParts) error {
	stage, err := stageFromParts(parts)
	if err != nil {
		return err
	}
	if w.progress == nil {
		return nil
	}
	return w.progress(stage)
}

// failureOfState is the error for a task that failed or was canceled, and nil
// for every other state.
func failureOfState(state a2a.TaskState) error {
	switch state {
	case a2a.TaskStateFailed, a2a.TaskStateRejected:
		return fmt.Errorf("investigation task failed")
	case a2a.TaskStateCanceled:
		return context.Canceled
	case a2a.TaskStateUnspecified, a2a.TaskStateSubmitted, a2a.TaskStateWorking, a2a.TaskStateCompleted,
		a2a.TaskStateAuthRequired, a2a.TaskStateInputRequired:
		return nil
	default:
		return nil
	}
}

func (g *Gateway) Cancel(ctx context.Context, ref domain.InvestigationTaskRef) error {
	_, err := g.client.CancelTask(extensionContext(ctx), &a2a.CancelTaskRequest{ID: a2a.TaskID(ref.TaskID)})
	if err != nil {
		return fmt.Errorf("cancel investigation task: %w", err)
	}
	return nil
}

func (g *Gateway) Close() error { return g.client.Destroy() }

func stageFromParts(parts a2a.ContentParts) (domain.InvestigationStage, error) {
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
	return domain.ParseInvestigationStage(wire.Stage)
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

var _ ports.InvestigationGateway = (*Gateway)(nil)
