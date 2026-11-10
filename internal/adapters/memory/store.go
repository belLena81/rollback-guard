package memory

import (
	"context"
	"sync"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/ports"
)

type data struct {
	incidents      map[domain.IncidentID]domain.Incident
	evidence       map[domain.IncidentID][]domain.Evidence
	evidenceIDs    map[domain.EvidenceID]domain.IncidentID
	conclusions    map[domain.IncidentID]domain.InvestigationConclusion
	conclusionIDs  map[domain.ConclusionID]domain.IncidentID
	decisions      map[domain.PolicyDecisionID]domain.PolicyDecision
	proposals      map[domain.IncidentID]domain.RollbackProposal
	actions        map[domain.ActionID]domain.IncidentID
	approvals      map[domain.IncidentID]domain.Approval
	approvalIDs    map[domain.ApprovalID]domain.IncidentID
	executions     map[domain.IncidentID]domain.RollbackExecution
	executionIDs   map[domain.ExecutionID]domain.IncidentID
	verifications  map[domain.IncidentID]domain.RecoveryVerification
	idempotency    map[domain.IdempotencyKey]domain.IdempotencyRecord
	investigations map[domain.IncidentID]domain.InvestigationDispatch
	events         map[domain.IncidentID][]domain.OperationEvent
}

func newData() *data {
	return &data{
		incidents:      map[domain.IncidentID]domain.Incident{},
		evidence:       map[domain.IncidentID][]domain.Evidence{},
		evidenceIDs:    map[domain.EvidenceID]domain.IncidentID{},
		conclusions:    map[domain.IncidentID]domain.InvestigationConclusion{},
		conclusionIDs:  map[domain.ConclusionID]domain.IncidentID{},
		decisions:      map[domain.PolicyDecisionID]domain.PolicyDecision{},
		proposals:      map[domain.IncidentID]domain.RollbackProposal{},
		actions:        map[domain.ActionID]domain.IncidentID{},
		approvals:      map[domain.IncidentID]domain.Approval{},
		approvalIDs:    map[domain.ApprovalID]domain.IncidentID{},
		executions:     map[domain.IncidentID]domain.RollbackExecution{},
		executionIDs:   map[domain.ExecutionID]domain.IncidentID{},
		verifications:  map[domain.IncidentID]domain.RecoveryVerification{},
		idempotency:    map[domain.IdempotencyKey]domain.IdempotencyRecord{},
		investigations: map[domain.IncidentID]domain.InvestigationDispatch{},
		events:         map[domain.IncidentID][]domain.OperationEvent{},
	}
}
func (d *data) clone() *data {
	c := newData()
	for k, v := range d.incidents {
		c.incidents[k] = v
	}
	for k, v := range d.evidence {
		c.evidence[k] = append([]domain.Evidence(nil), v...)
	}
	for k, v := range d.evidenceIDs {
		c.evidenceIDs[k] = v
	}
	for k, v := range d.conclusions {
		c.conclusions[k] = v
	}
	for k, v := range d.conclusionIDs {
		c.conclusionIDs[k] = v
	}
	for k, v := range d.decisions {
		c.decisions[k] = v
	}
	for k, v := range d.proposals {
		c.proposals[k] = v
	}
	for k, v := range d.actions {
		c.actions[k] = v
	}
	for k, v := range d.approvals {
		c.approvals[k] = v
	}
	for k, v := range d.approvalIDs {
		c.approvalIDs[k] = v
	}
	for k, v := range d.executions {
		c.executions[k] = v
	}
	for k, v := range d.executionIDs {
		c.executionIDs[k] = v
	}
	for k, v := range d.verifications {
		c.verifications[k] = v
	}
	for k, v := range d.idempotency {
		c.idempotency[k] = v
	}
	for k, v := range d.investigations {
		c.investigations[k] = v
	}
	for k, v := range d.events {
		c.events[k] = append([]domain.OperationEvent(nil), v...)
	}
	return c
}

type UnitOfWork struct {
	mu   sync.RWMutex
	data *data
}

func NewUnitOfWork() *UnitOfWork {
	return &UnitOfWork{data: newData()}
}
func (u *UnitOfWork) WithinTransaction(ctx context.Context, fn func(ports.Repositories) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	tx := &repositories{data: u.data.clone()}
	if err := fn(tx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	u.data = tx.data
	return nil
}

type repositories struct{ data *data }

func (r *repositories) Incidents() ports.IncidentRepository {
	return incidentRepo{r.data}
}
func (r *repositories) Evidence() ports.EvidenceRepository {
	return evidenceRepo{r.data}
}
func (r *repositories) Conclusions() ports.ConclusionRepository {
	return conclusionRepo{r.data}
}
func (r *repositories) PolicyDecisions() ports.PolicyDecisionRepository {
	return decisionRepo{r.data}
}
func (r *repositories) Proposals() ports.ProposalRepository {
	return proposalRepo{r.data}
}
func (r *repositories) Approvals() ports.ApprovalRepository {
	return approvalRepo{r.data}
}
func (r *repositories) Executions() ports.ExecutionRepository {
	return executionRepo{r.data}
}
func (r *repositories) Verifications() ports.VerificationRepository {
	return verificationRepo{r.data}
}
func (r *repositories) Idempotency() ports.IdempotencyRepository {
	return idempotencyRepo{r.data}
}
func (r *repositories) InvestigationTasks() ports.InvestigationTaskRepository {
	return investigationTaskRepo{r.data}
}
func (r *repositories) Events() ports.EventRepository {
	return eventRepo{r.data}
}

type incidentRepo struct{ d *data }

func (r incidentRepo) Create(v domain.Incident) error {
	if _, ok := r.d.incidents[v.ID()]; ok {
		return &domain.AlreadyExistsError{Resource: "incident.id"}
	}
	r.d.incidents[v.ID()] = v
	return nil
}
func (r incidentRepo) Get(id domain.IncidentID) (domain.Incident, error) {
	v, ok := r.d.incidents[id]
	if !ok {
		return domain.Incident{}, &domain.NotFoundError{Resource: "incident", ID: string(id)}
	}
	return v, nil
}
func (r incidentRepo) Update(v domain.Incident, expected uint64) error {
	old, ok := r.d.incidents[v.ID()]
	if !ok {
		return &domain.NotFoundError{Resource: "incident", ID: string(v.ID())}
	}
	if old.Version() != expected {
		return &domain.VersionConflictError{Expected: expected, Actual: old.Version()}
	}
	if v.Version() != expected+1 {
		return &domain.VersionConflictError{Expected: expected + 1, Actual: v.Version()}
	}
	r.d.incidents[v.ID()] = v
	return nil
}

type evidenceRepo struct{ d *data }

func (r evidenceRepo) Add(v domain.Evidence) error {
	if _, ok := r.d.incidents[v.IncidentID()]; !ok {
		return &domain.NotFoundError{Resource: "incident", ID: string(v.IncidentID())}
	}
	if _, exists := r.d.evidenceIDs[v.ID()]; exists {
		return &domain.AlreadyExistsError{Resource: "evidence.id"}
	}
	r.d.evidence[v.IncidentID()] = append(r.d.evidence[v.IncidentID()], v)
	r.d.evidenceIDs[v.ID()] = v.IncidentID()
	return nil
}
func (r evidenceRepo) List(id domain.IncidentID) ([]domain.Evidence, error) {
	v := r.d.evidence[id]
	return append([]domain.Evidence(nil), v...), nil
}

type conclusionRepo struct{ d *data }

func (r conclusionRepo) Save(v domain.InvestigationConclusion) error {
	if _, ok := r.d.incidents[v.IncidentID()]; !ok {
		return &domain.NotFoundError{Resource: "incident", ID: string(v.IncidentID())}
	}
	if _, exists := r.d.conclusions[v.IncidentID()]; exists {
		return &domain.AlreadyExistsError{Resource: "conclusion.incident_id"}
	}
	if _, exists := r.d.conclusionIDs[v.ID()]; exists {
		return &domain.AlreadyExistsError{Resource: "conclusion.id"}
	}
	for _, evidenceID := range v.EvidenceIDs() {
		if incidentID, ok := r.d.evidenceIDs[evidenceID]; !ok || incidentID != v.IncidentID() {
			return &domain.EvidenceReferenceError{EvidenceID: evidenceID}
		}
	}
	r.d.conclusions[v.IncidentID()] = v
	r.d.conclusionIDs[v.ID()] = v.IncidentID()
	return nil
}
func (r conclusionRepo) Get(id domain.IncidentID) (domain.InvestigationConclusion, error) {
	v, ok := r.d.conclusions[id]
	if !ok {
		return v, &domain.NotFoundError{Resource: "conclusion", ID: string(id)}
	}
	return v, nil
}

type decisionRepo struct{ d *data }

func (r decisionRepo) Save(v domain.PolicyDecision) error {
	if _, ok := r.d.incidents[v.IncidentID()]; !ok {
		return &domain.NotFoundError{Resource: "incident", ID: string(v.IncidentID())}
	}
	if _, exists := r.d.decisions[v.ID()]; exists {
		return &domain.AlreadyExistsError{Resource: "policy_decision.id"}
	}
	r.d.decisions[v.ID()] = v
	return nil
}
func (r decisionRepo) Get(id domain.PolicyDecisionID) (domain.PolicyDecision, error) {
	v, ok := r.d.decisions[id]
	if !ok {
		return v, &domain.NotFoundError{Resource: "policy decision", ID: string(id)}
	}
	return v, nil
}

type proposalRepo struct{ d *data }

func (r proposalRepo) Save(v domain.RollbackProposal) error {
	if _, ok := r.d.incidents[v.IncidentID()]; !ok {
		return &domain.NotFoundError{Resource: "incident", ID: string(v.IncidentID())}
	}
	decision, ok := r.d.decisions[v.PolicyDecisionID()]
	if !ok || decision.IncidentID() != v.IncidentID() {
		return &domain.NotFoundError{Resource: "policy decision", ID: string(v.PolicyDecisionID())}
	}
	if _, exists := r.d.proposals[v.IncidentID()]; exists {
		return &domain.AlreadyExistsError{Resource: "proposal.incident_id"}
	}
	if _, exists := r.d.actions[v.ActionID()]; exists {
		return &domain.AlreadyExistsError{Resource: "proposal.action_id"}
	}
	r.d.proposals[v.IncidentID()] = v
	r.d.actions[v.ActionID()] = v.IncidentID()
	return nil
}
func (r proposalRepo) GetByIncident(id domain.IncidentID) (domain.RollbackProposal, error) {
	v, ok := r.d.proposals[id]
	if !ok {
		return v, &domain.NotFoundError{Resource: "proposal", ID: string(id)}
	}
	return v, nil
}

type approvalRepo struct{ d *data }

func (r approvalRepo) Save(v domain.Approval) error {
	incident, ok := r.d.actions[v.ActionID()]
	if !ok {
		return &domain.NotFoundError{Resource: "proposal action", ID: string(v.ActionID())}
	}
	proposal := r.d.proposals[incident]
	if proposal.Hash() != v.ProposalHash() {
		return &domain.ApprovalMismatchError{Reason: "approval hash does not match proposal"}
	}
	if _, exists := r.d.approvals[incident]; exists {
		return &domain.AlreadyExistsError{Resource: "approval.incident_id"}
	}
	if _, exists := r.d.approvalIDs[v.ID()]; exists {
		return &domain.AlreadyExistsError{Resource: "approval.id"}
	}
	r.d.approvals[incident] = v
	r.d.approvalIDs[v.ID()] = incident
	return nil
}
func (r approvalRepo) GetByIncident(id domain.IncidentID) (domain.Approval, error) {
	v, ok := r.d.approvals[id]
	if !ok {
		return v, &domain.NotFoundError{Resource: "approval", ID: string(id)}
	}
	return v, nil
}

type executionRepo struct{ d *data }

func (r executionRepo) Save(v domain.RollbackExecution) error {
	if _, ok := r.d.incidents[v.IncidentID()]; !ok {
		return &domain.NotFoundError{Resource: "incident", ID: string(v.IncidentID())}
	}
	if _, exists := r.d.executions[v.IncidentID()]; exists {
		return &domain.AlreadyExistsError{Resource: "execution.incident_id"}
	}
	if _, exists := r.d.executionIDs[v.ID()]; exists {
		return &domain.AlreadyExistsError{Resource: "execution.id"}
	}
	proposal, proposalExists := r.d.proposals[v.IncidentID()]
	record, recordExists := r.d.idempotency[v.IdempotencyKey()]
	if !proposalExists || !recordExists || record.State() != domain.IdempotencyPending || record.ResourceID() != string(v.IncidentID()) || record.RequestHash() != string(proposal.Hash()) || v.FromRevision() != proposal.CurrentRevision() || v.ToRevision() != proposal.TargetRevision() {
		return &domain.ValidationError{Field: "execution", Reason: "does not match pending approved rollback"}
	}
	r.d.executions[v.IncidentID()] = v
	r.d.executionIDs[v.ID()] = v.IncidentID()
	return nil
}
func (r executionRepo) Get(id domain.ExecutionID) (domain.RollbackExecution, error) {
	incidentID, ok := r.d.executionIDs[id]
	if !ok {
		return domain.RollbackExecution{}, &domain.NotFoundError{Resource: "execution", ID: string(id)}
	}
	return r.d.executions[incidentID], nil
}
func (r executionRepo) GetByIncident(id domain.IncidentID) (domain.RollbackExecution, error) {
	v, ok := r.d.executions[id]
	if !ok {
		return v, &domain.NotFoundError{Resource: "execution", ID: string(id)}
	}
	return v, nil
}

type verificationRepo struct{ d *data }

func (r verificationRepo) Save(v domain.RecoveryVerification) error {
	if _, ok := r.d.incidents[v.IncidentID()]; !ok {
		return &domain.NotFoundError{Resource: "incident", ID: string(v.IncidentID())}
	}
	if _, ok := r.d.executionIDs[v.ExecutionID()]; !ok {
		return &domain.NotFoundError{Resource: "execution", ID: string(v.ExecutionID())}
	}
	executionIncident := r.d.executionIDs[v.ExecutionID()]
	if executionIncident != v.IncidentID() {
		return &domain.ValidationError{Field: "verification.execution_id", Reason: "belongs to another incident"}
	}
	if _, exists := r.d.verifications[v.IncidentID()]; exists {
		return &domain.AlreadyExistsError{Resource: "verification.incident_id"}
	}
	r.d.verifications[v.IncidentID()] = v
	return nil
}
func (r verificationRepo) GetByIncident(id domain.IncidentID) (domain.RecoveryVerification, error) {
	v, ok := r.d.verifications[id]
	if !ok {
		return v, &domain.NotFoundError{Resource: "verification", ID: string(id)}
	}
	return v, nil
}

type idempotencyRepo struct{ d *data }

func (r idempotencyRepo) Get(k domain.IdempotencyKey) (domain.IdempotencyRecord, error) {
	v, ok := r.d.idempotency[k]
	if !ok {
		return v, &domain.NotFoundError{Resource: "idempotency key", ID: string(k)}
	}
	return v, nil
}
func (r idempotencyRepo) Create(v domain.IdempotencyRecord) error {
	if v.State() != domain.IdempotencyPending {
		return &domain.ValidationError{Field: "idempotency.state", Reason: "must be pending on create"}
	}
	if _, ok := r.d.idempotency[v.Key()]; ok {
		return &domain.IdempotencyConflictError{Key: v.Key()}
	}
	r.d.idempotency[v.Key()] = v
	return nil
}
func (r idempotencyRepo) Update(v domain.IdempotencyRecord, expected domain.IdempotencyState) error {
	old, ok := r.d.idempotency[v.Key()]
	if !ok {
		return &domain.NotFoundError{Resource: "idempotency key", ID: string(v.Key())}
	}
	if old.State() != expected || old.Operation() != v.Operation() || old.RequestHash() != v.RequestHash() || old.ResourceID() != v.ResourceID() || old.StartedAt() != v.StartedAt() {
		return &domain.IdempotencyConflictError{Key: v.Key()}
	}
	if expected != domain.IdempotencyPending || v.State() != domain.IdempotencyCompleted {
		return &domain.ValidationError{Field: "idempotency.state", Reason: "invalid transition"}
	}
	incidentID, exists := r.d.executionIDs[v.ResultID()]
	if !exists || string(incidentID) != v.ResourceID() {
		return &domain.ValidationError{Field: "idempotency.result_id", Reason: "must reference the bound execution"}
	}
	execution := r.d.executions[incidentID]
	if execution.IdempotencyKey() != v.Key() {
		return &domain.ValidationError{Field: "idempotency.result_id", Reason: "execution uses a different idempotency key"}
	}
	r.d.idempotency[v.Key()] = v
	return nil
}

type eventRepo struct{ d *data }

type investigationTaskRepo struct{ d *data }

func (r investigationTaskRepo) Create(v domain.InvestigationDispatch) error {
	if _, ok := r.d.incidents[v.IncidentID]; !ok {
		return &domain.NotFoundError{Resource: "incident", ID: string(v.IncidentID)}
	}
	if _, exists := r.d.investigations[v.IncidentID]; exists {
		return &domain.AlreadyExistsError{Resource: "investigation.incident_id"}
	}
	r.d.investigations[v.IncidentID] = v
	return nil
}

func (r investigationTaskRepo) GetByIncident(id domain.IncidentID) (domain.InvestigationDispatch, error) {
	v, ok := r.d.investigations[id]
	if !ok {
		return v, &domain.NotFoundError{Resource: "investigation", ID: string(id)}
	}
	return v, nil
}

func (r investigationTaskRepo) ClaimNext(now time.Time, owner string, leaseUntil time.Time) (domain.InvestigationDispatch, error) {
	for id, task := range r.d.investigations {
		staleActive := (task.State == domain.InvestigationTaskSubmitting || task.State == domain.InvestigationTaskSubmitted || task.State == domain.InvestigationTaskWorking) && !task.LeaseExpiresAt.After(now)
		claimableCancellation := task.State == domain.InvestigationTaskCancelRequested && !task.LeaseExpiresAt.After(now)
		if task.State != domain.InvestigationTaskQueued && !claimableCancellation && !staleActive {
			continue
		}
		if task.State != domain.InvestigationTaskCancelRequested && task.Ref.TaskID == "" {
			task.State = domain.InvestigationTaskSubmitting
		}
		task.LeaseOwner = owner
		task.LeaseExpiresAt = leaseUntil
		task.Attempts++
		task.Version++
		task.UpdatedAt = now
		r.d.investigations[id] = task
		return task, nil
	}
	return domain.InvestigationDispatch{}, &domain.NotFoundError{Resource: "investigation", ID: "ready"}
}

func (r investigationTaskRepo) Update(v domain.InvestigationDispatch, expected uint64) error {
	old, ok := r.d.investigations[v.IncidentID]
	if !ok {
		return &domain.NotFoundError{Resource: "investigation", ID: string(v.IncidentID)}
	}
	if old.Version != expected || v.Version != expected+1 {
		return &domain.VersionConflictError{Expected: expected, Actual: old.Version}
	}
	r.d.investigations[v.IncidentID] = v
	return nil
}

func (r eventRepo) NextSequence(ctx context.Context, id domain.IncidentID) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return uint64(len(r.d.events[id]) + 1), nil
}
func (r eventRepo) Append(v domain.OperationEvent) error {
	if _, ok := r.d.incidents[v.IncidentID()]; !ok {
		return &domain.NotFoundError{Resource: "incident", ID: string(v.IncidentID())}
	}
	expected := uint64(len(r.d.events[v.IncidentID()]) + 1)
	if v.Sequence() != expected {
		return &domain.VersionConflictError{Expected: expected, Actual: v.Sequence()}
	}
	r.d.events[v.IncidentID()] = append(r.d.events[v.IncidentID()], v)
	return nil
}
func (r eventRepo) List(id domain.IncidentID) ([]domain.OperationEvent, error) {
	v := r.d.events[id]
	return append([]domain.OperationEvent(nil), v...), nil
}

var _ ports.UnitOfWork = (*UnitOfWork)(nil)
