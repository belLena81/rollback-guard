package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"slices"
	"strings"
	"time"
)

type RuleResult struct {
	Name   string
	Passed bool
	Reason string
}
type PolicyDecision struct {
	id         PolicyDecisionID
	incidentID IncidentID
	allowed    bool
	rules      []RuleResult
	decidedAt  time.Time
}

func NewPolicyDecision(id PolicyDecisionID, incidentID IncidentID, rules []RuleResult, now time.Time) (PolicyDecision, error) {
	if id == "" || incidentID == "" || len(rules) == 0 || now.IsZero() {
		return PolicyDecision{}, invalid("policy_decision", "invalid decision")
	}
	allowed := true
	copied := slices.Clone(rules)
	seen := make(map[string]struct{}, len(copied))
	for _, rule := range copied {
		if strings.TrimSpace(rule.Name) == "" {
			return PolicyDecision{}, invalid("policy_decision.rule", "name required")
		}
		if _, exists := seen[rule.Name]; exists {
			return PolicyDecision{}, invalid("policy_decision.rule", "names must be unique")
		}
		seen[rule.Name] = struct{}{}
		allowed = allowed && rule.Passed
	}
	return PolicyDecision{id: id, incidentID: incidentID, allowed: allowed, rules: copied, decidedAt: now.UTC()}, nil
}
func (d PolicyDecision) ID() PolicyDecisionID {
	return d.id
}
func (d PolicyDecision) IncidentID() IncidentID {
	return d.incidentID
}
func (d PolicyDecision) Allowed() bool {
	return d.allowed
}
func (d PolicyDecision) Rules() []RuleResult {
	return slices.Clone(d.rules)
}
func (d PolicyDecision) DecidedAt() time.Time {
	return d.decidedAt
}

type RollbackProposal struct {
	actionID                        ActionID
	incidentID                      IncidentID
	policyDecisionID                PolicyDecisionID
	target                          DeploymentTarget
	currentRevision, targetRevision Revision
	expiresAt                       time.Time
	hash                            ProposalHash
}

func NewRollbackProposal(actionID ActionID, incidentID IncidentID, policyDecisionID PolicyDecisionID, target DeploymentTarget, currentRevision, targetRevision Revision, expiresAt time.Time) (RollbackProposal, error) {
	if actionID == "" || incidentID == "" || policyDecisionID == "" || currentRevision == "" || targetRevision == "" || expiresAt.IsZero() {
		return RollbackProposal{}, invalid("proposal", "required value is empty")
	}
	if !target.Valid() {
		return RollbackProposal{}, invalid("proposal.target", "must be valid")
	}
	if currentRevision == targetRevision {
		return RollbackProposal{}, invalid("proposal.target_revision", "must differ from current revision")
	}
	p := RollbackProposal{actionID: actionID, incidentID: incidentID, policyDecisionID: policyDecisionID, target: target, currentRevision: currentRevision, targetRevision: targetRevision, expiresAt: canonicalProposalExpiry(expiresAt)}
	hash, err := p.computeHash()
	if err != nil {
		return RollbackProposal{}, invalid("proposal.hash", err.Error())
	}
	p.hash = hash
	return p, nil
}

func canonicalProposalExpiry(expiresAt time.Time) time.Time {
	return expiresAt.UTC().Truncate(time.Microsecond)
}

func (p RollbackProposal) computeHash() (ProposalHash, error) {
	canonical := struct {
		Version          string           `json:"version"`
		ActionID         ActionID         `json:"action_id"`
		IncidentID       IncidentID       `json:"incident_id"`
		PolicyDecisionID PolicyDecisionID `json:"policy_decision_id"`
		Cluster          string           `json:"cluster"`
		Namespace        string           `json:"namespace"`
		Deployment       string           `json:"deployment"`
		Service          string           `json:"service"`
		CurrentRevision  Revision         `json:"current_revision"`
		TargetRevision   Revision         `json:"target_revision"`
		ExpiresAt        string           `json:"expires_at"`
	}{
		Version:          "v2",
		ActionID:         p.actionID,
		IncidentID:       p.incidentID,
		PolicyDecisionID: p.policyDecisionID,
		Cluster:          p.target.Cluster(),
		Namespace:        p.target.Namespace(),
		Deployment:       p.target.Deployment(),
		Service:          p.target.Service(),
		CurrentRevision:  p.currentRevision,
		TargetRevision:   p.targetRevision,
		ExpiresAt:        p.expiresAt.UTC().Format(time.RFC3339Nano),
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return ProposalHash(hex.EncodeToString(sum[:])), nil
}
func (p RollbackProposal) ActionID() ActionID {
	return p.actionID
}
func (p RollbackProposal) IncidentID() IncidentID {
	return p.incidentID
}
func (p RollbackProposal) PolicyDecisionID() PolicyDecisionID {
	return p.policyDecisionID
}
func (p RollbackProposal) Target() DeploymentTarget {
	return p.target
}
func (p RollbackProposal) CurrentRevision() Revision {
	return p.currentRevision
}
func (p RollbackProposal) TargetRevision() Revision {
	return p.targetRevision
}
func (p RollbackProposal) ExpiresAt() time.Time {
	return p.expiresAt
}
func (p RollbackProposal) Hash() ProposalHash {
	return p.hash
}
func (p RollbackProposal) Expired(now time.Time) bool {
	return !now.Before(p.expiresAt)
}

type ApprovalDecision uint8

const (
	ApprovalApproved ApprovalDecision = iota + 1
	ApprovalRejected
)

func (d ApprovalDecision) String() string {
	if d == ApprovalApproved {
		return "approved"
	}
	if d == ApprovalRejected {
		return "rejected"
	}
	return unknownName
}

func ParseApprovalDecision(value string) (ApprovalDecision, error) {
	return parseEnum("approval decision", value, ApprovalApproved, ApprovalRejected)
}

type Approval struct {
	id                   ApprovalID
	actor                Actor
	decision             ApprovalDecision
	actionID             ActionID
	proposalHash         ProposalHash
	decidedAt, expiresAt time.Time
	rejectionReason      string
}

func NewApproval(id ApprovalID, actor Actor, decision ApprovalDecision, actionID ActionID, hash ProposalHash, decidedAt, expiresAt time.Time, reason string) (Approval, error) {
	if !actor.IsHuman() {
		return Approval{}, &NonHumanApprovalError{Kind: actor.Kind()}
	}
	if id == "" || actionID == "" || hash == "" || decidedAt.IsZero() || expiresAt.IsZero() {
		return Approval{}, invalid("approval", "required value is empty")
	}
	if !decidedAt.Before(expiresAt) {
		return Approval{}, &ExpiryError{Resource: "approval"}
	}
	if decision != ApprovalApproved && decision != ApprovalRejected {
		return Approval{}, invalid("approval.decision", "unknown decision")
	}
	if decision == ApprovalRejected && strings.TrimSpace(reason) == "" {
		return Approval{}, invalid("approval.rejection_reason", "required for rejection")
	}
	if decision == ApprovalApproved && reason != "" {
		return Approval{}, invalid("approval.rejection_reason", "not allowed for approval")
	}
	return Approval{id: id, actor: actor, decision: decision, actionID: actionID, proposalHash: hash, decidedAt: decidedAt.UTC(), expiresAt: expiresAt.UTC(), rejectionReason: reason}, nil
}
func (a Approval) ID() ApprovalID {
	return a.id
}
func (a Approval) Actor() Actor {
	return a.actor
}
func (a Approval) Decision() ApprovalDecision {
	return a.decision
}
func (a Approval) ActionID() ActionID {
	return a.actionID
}
func (a Approval) ProposalHash() ProposalHash {
	return a.proposalHash
}
func (a Approval) DecidedAt() time.Time {
	return a.decidedAt
}
func (a Approval) ExpiresAt() time.Time {
	return a.expiresAt
}
func (a Approval) RejectionReason() string {
	return a.rejectionReason
}
func (a Approval) Expired(now time.Time) bool {
	return !now.Before(a.expiresAt)
}

type ApprovedRollback struct {
	proposal RollbackProposal
	decision PolicyDecision
	approval Approval
}

func (a ApprovedRollback) Proposal() RollbackProposal {
	return a.proposal
}
func (a ApprovedRollback) Approval() Approval {
	return a.approval
}
func (a ApprovedRollback) PolicyDecision() PolicyDecision {
	return a.decision
}

func AuthorizeRollback(proposal RollbackProposal, decision PolicyDecision, approval Approval, fresh DeploymentEvidence, now time.Time) (ApprovedRollback, error) {
	if !decision.Allowed() || decision.ID() != proposal.PolicyDecisionID() || decision.IncidentID() != proposal.IncidentID() {
		return ApprovedRollback{}, &PolicyDenialError{Rule: "policy_decision_binding"}
	}
	if proposal.Expired(now) {
		return ApprovedRollback{}, &ExpiryError{Resource: "proposal"}
	}
	if approval.Expired(now) {
		return ApprovedRollback{}, &ExpiryError{Resource: "approval"}
	}
	if !approval.Actor().IsHuman() {
		return ApprovedRollback{}, &NonHumanApprovalError{Kind: approval.Actor().Kind()}
	}
	if approval.Decision() != ApprovalApproved || approval.ActionID() != proposal.ActionID() || approval.ProposalHash() != proposal.Hash() {
		return ApprovedRollback{}, &ApprovalMismatchError{Reason: "decision, action, or hash does not match proposal"}
	}
	if !fresh.Target.Equal(proposal.Target()) || fresh.CurrentRevision != proposal.CurrentRevision() {
		return ApprovedRollback{}, &ApprovalMismatchError{Reason: "deployment identity or current revision changed"}
	}
	hash, err := proposal.computeHash()
	if err != nil || proposal.Hash() != hash {
		return ApprovedRollback{}, &ApprovalMismatchError{Reason: "proposal hash is invalid"}
	}
	return ApprovedRollback{proposal: proposal, decision: decision, approval: approval}, nil
}

func ResumeApprovedRollback(proposal RollbackProposal, decision PolicyDecision, approval Approval, record IdempotencyRecord, key IdempotencyKey) (ApprovedRollback, error) {
	if record.Key() != key ||
		record.Operation() != IdempotencyOperationExecuteRollback ||
		record.State() != IdempotencyPending ||
		record.RequestHash() != string(proposal.Hash()) ||
		record.ResourceID() != string(proposal.IncidentID()) {
		return ApprovedRollback{}, &IdempotencyConflictError{Key: key}
	}
	if !decision.Allowed() || decision.ID() != proposal.PolicyDecisionID() || decision.IncidentID() != proposal.IncidentID() {
		return ApprovedRollback{}, &PolicyDenialError{Rule: "policy_decision_binding"}
	}
	if !approval.Actor().IsHuman() {
		return ApprovedRollback{}, &NonHumanApprovalError{Kind: approval.Actor().Kind()}
	}
	if approval.Decision() != ApprovalApproved || approval.ActionID() != proposal.ActionID() || approval.ProposalHash() != proposal.Hash() {
		return ApprovedRollback{}, &ApprovalMismatchError{Reason: "decision, action, or hash does not match proposal"}
	}
	hash, err := proposal.computeHash()
	if err != nil || proposal.Hash() != hash {
		return ApprovedRollback{}, &ApprovalMismatchError{Reason: "proposal hash is invalid"}
	}
	return ApprovedRollback{proposal: proposal, decision: decision, approval: approval}, nil
}

type ApprovalAuthorizer struct{}

func (ApprovalAuthorizer) Authorize(proposal RollbackProposal, decision PolicyDecision, approval Approval, fresh DeploymentEvidence, now time.Time) (ApprovedRollback, error) {
	return AuthorizeRollback(proposal, decision, approval, fresh, now)
}

type ExecutionOutcome uint8

const (
	ExecutionSucceeded ExecutionOutcome = iota + 1
	ExecutionFailed
)

func (o ExecutionOutcome) String() string {
	if o == ExecutionSucceeded {
		return "succeeded"
	}
	if o == ExecutionFailed {
		return "failed"
	}
	return unknownName
}

func ParseExecutionOutcome(value string) (ExecutionOutcome, error) {
	return parseEnum("execution outcome", value, ExecutionSucceeded, ExecutionFailed)
}

type RollbackExecution struct {
	id                     ExecutionID
	incidentID             IncidentID
	key                    IdempotencyKey
	from, to               Revision
	outcome                ExecutionOutcome
	startedAt, completedAt time.Time
	failureReason          string
}

func NewRollbackExecution(id ExecutionID, incidentID IncidentID, key IdempotencyKey, from, to Revision, outcome ExecutionOutcome, startedAt, completedAt time.Time, reason string) (RollbackExecution, error) {
	if id == "" || incidentID == "" || key == "" || from == "" || to == "" || from == to || startedAt.IsZero() || completedAt.Before(startedAt) {
		return RollbackExecution{}, invalid("execution", "invalid execution")
	}
	if outcome != ExecutionSucceeded && outcome != ExecutionFailed {
		return RollbackExecution{}, invalid("execution.outcome", "unknown outcome")
	}
	if outcome == ExecutionFailed && strings.TrimSpace(reason) == "" {
		return RollbackExecution{}, invalid("execution.failure_reason", "required")
	}
	if outcome == ExecutionSucceeded && reason != "" {
		return RollbackExecution{}, invalid("execution.failure_reason", "not allowed for successful execution")
	}
	return RollbackExecution{id: id, incidentID: incidentID, key: key, from: from, to: to, outcome: outcome, startedAt: startedAt.UTC(), completedAt: completedAt.UTC(), failureReason: reason}, nil
}
func (e RollbackExecution) ID() ExecutionID {
	return e.id
}
func (e RollbackExecution) IncidentID() IncidentID {
	return e.incidentID
}
func (e RollbackExecution) IdempotencyKey() IdempotencyKey {
	return e.key
}
func (e RollbackExecution) FromRevision() Revision {
	return e.from
}
func (e RollbackExecution) ToRevision() Revision {
	return e.to
}
func (e RollbackExecution) Outcome() ExecutionOutcome {
	return e.outcome
}
func (e RollbackExecution) StartedAt() time.Time {
	return e.startedAt
}
func (e RollbackExecution) CompletedAt() time.Time {
	return e.completedAt
}
func (e RollbackExecution) FailureReason() string {
	return e.failureReason
}

type RecoveryVerification struct {
	incidentID                IncidentID
	executionID               ExecutionID
	passed                    bool
	revision                  Revision
	desired, ready, available int
	metricValue               float64
	checkedAt                 time.Time
	failureReason             string
}

func NewRecoveryVerification(incidentID IncidentID, executionID ExecutionID, passed bool, revision Revision, desired, ready, available int, metricValue float64, checkedAt time.Time, reason string) (RecoveryVerification, error) {
	if incidentID == "" || executionID == "" || revision == "" || desired < 0 || ready < 0 || available < 0 || math.IsNaN(metricValue) || math.IsInf(metricValue, 0) || checkedAt.IsZero() {
		return RecoveryVerification{}, invalid("verification", "required value is empty")
	}
	if !passed && strings.TrimSpace(reason) == "" {
		return RecoveryVerification{}, invalid("verification.failure_reason", "required")
	}
	if passed && reason != "" {
		return RecoveryVerification{}, invalid("verification.failure_reason", "not allowed for successful verification")
	}
	return RecoveryVerification{incidentID: incidentID, executionID: executionID, passed: passed, revision: revision, desired: desired, ready: ready, available: available, metricValue: metricValue, checkedAt: checkedAt.UTC(), failureReason: reason}, nil
}
func (v RecoveryVerification) IncidentID() IncidentID {
	return v.incidentID
}
func (v RecoveryVerification) ExecutionID() ExecutionID {
	return v.executionID
}
func (v RecoveryVerification) Passed() bool {
	return v.passed
}
func (v RecoveryVerification) Revision() Revision {
	return v.revision
}
func (v RecoveryVerification) DesiredReplicas() int {
	return v.desired
}
func (v RecoveryVerification) ReadyReplicas() int {
	return v.ready
}
func (v RecoveryVerification) AvailableReplicas() int {
	return v.available
}
func (v RecoveryVerification) MetricValue() float64 {
	return v.metricValue
}
func (v RecoveryVerification) CheckedAt() time.Time {
	return v.checkedAt
}
func (v RecoveryVerification) FailureReason() string {
	return v.failureReason
}
