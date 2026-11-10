package domain

// Role is the coarse, DB-assigned authority of a human user
// (docs/adr-auth-sessions.md). Members are ordered: viewer < operator <
// administrator. The zero value is not a role and is never authorized.
type Role uint8

const (
	RoleViewer Role = iota + 1
	RoleOperator
	RoleAdministrator
)

func (r Role) String() string {
	switch r {
	case RoleViewer:
		return "viewer"
	case RoleOperator:
		return "operator"
	case RoleAdministrator:
		return "administrator"
	default:
		return unknownName
	}
}

func ParseRole(value string) (Role, error) {
	return parseEnum("role", value, RoleViewer, RoleAdministrator)
}

// Valid reports whether r is a member of the enumeration.
func (r Role) Valid() bool {
	return r >= RoleViewer && r <= RoleAdministrator
}

// AtLeast reports whether r grants at least the authority of other. An
// invalid value on either side is never satisfied.
func (r Role) AtLeast(other Role) bool {
	return r.Valid() && other.Valid() && r >= other
}

// Action is an operation a human user asks the orchestrator to perform.
type Action uint8

const (
	// ActionRead covers reading incidents, events and the caller's identity.
	ActionRead Action = iota + 1
	ActionCreateIncident
	ActionRejectIncident
	ActionCancelIncident
	// ActionApproveRollback decides a pending rollback approval.
	ActionApproveRollback
	// ActionAdminister covers changing user roles and revoking sessions.
	ActionAdminister
)

func (a Action) String() string {
	switch a {
	case ActionRead:
		return "read"
	case ActionCreateIncident:
		return "create_incident"
	case ActionRejectIncident:
		return "reject_incident"
	case ActionCancelIncident:
		return "cancel_incident"
	case ActionApproveRollback:
		return "approve_rollback"
	case ActionAdminister:
		return "administer"
	default:
		return unknownName
	}
}

func ParseAction(value string) (Action, error) {
	return parseEnum("action", value, ActionRead, ActionAdminister)
}

// Valid reports whether a is a member of the enumeration.
func (a Action) Valid() bool {
	return a >= ActionRead && a <= ActionAdminister
}

// RolePolicy is the role/action matrix of docs/adr-auth-sessions.md. It is a
// stateless value; the zero value is ready to use.
type RolePolicy struct{}

// minimumRole is the least role allowed to perform each action. An action
// absent from this table is denied to everyone.
func minimumRole(action Action) (Role, bool) {
	switch action {
	case ActionRead:
		return RoleViewer, true
	case ActionCreateIncident, ActionRejectIncident, ActionCancelIncident:
		return RoleOperator, true
	case ActionApproveRollback, ActionAdminister:
		return RoleAdministrator, true
	default:
		return 0, false
	}
}

// Authorize returns nil when role may perform action and a *ForbiddenError
// otherwise. It denies by default: an invalid role or an unknown action is
// never allowed.
func (RolePolicy) Authorize(role Role, action Action) error {
	required, known := minimumRole(action)
	if !known || !role.AtLeast(required) {
		return &ForbiddenError{Role: role, Action: action}
	}
	return nil
}
