package domain

import (
	"errors"
	"strings"
	"testing"
)

func TestParseRoleAndActionRoundTripAndRejectInvalid(t *testing.T) {
	runEnumCase(t, enumCase[Role]{"role", []Role{RoleViewer, RoleOperator, RoleAdministrator}, ParseRole})
	runEnumCase(t, enumCase[Action]{"action", []Action{
		ActionRead, ActionCreateIncident, ActionRejectIncident, ActionCancelIncident, ActionApproveRollback, ActionAdminister,
	}, ParseAction})
}

func TestParseRoleRejectsNonCanonicalSpellings(t *testing.T) {
	for _, value := range []string{"Viewer", "ADMINISTRATOR", " operator", "operator ", "admin", "Admin", "root"} {
		if got, err := ParseRole(value); err == nil {
			t.Errorf("ParseRole(%q) = %v, nil; want error", value, got)
		}
	}
	for _, value := range []string{"Read", "approve", "approve-rollback", " read"} {
		if got, err := ParseAction(value); err == nil {
			t.Errorf("ParseAction(%q) = %v, nil; want error", value, got)
		}
	}
}

func TestRoleStringAndValidity(t *testing.T) {
	tests := []struct {
		role  Role
		name  string
		valid bool
	}{
		{RoleViewer, "viewer", true},
		{RoleOperator, "operator", true},
		{RoleAdministrator, "administrator", true},
		{Role(0), "unknown", false},
		{Role(4), "unknown", false},
		{Role(255), "unknown", false},
	}
	for _, test := range tests {
		if got := test.role.String(); got != test.name {
			t.Errorf("Role(%d).String() = %q, want %q", test.role, got, test.name)
		}
		if got := test.role.Valid(); got != test.valid {
			t.Errorf("Role(%d).Valid() = %v, want %v", test.role, got, test.valid)
		}
	}
}

func TestActionStringAndValidity(t *testing.T) {
	tests := []struct {
		action Action
		name   string
		valid  bool
	}{
		{ActionRead, "read", true},
		{ActionCreateIncident, "create_incident", true},
		{ActionRejectIncident, "reject_incident", true},
		{ActionCancelIncident, "cancel_incident", true},
		{ActionApproveRollback, "approve_rollback", true},
		{ActionAdminister, "administer", true},
		{Action(0), "unknown", false},
		{Action(7), "unknown", false},
		{Action(255), "unknown", false},
	}
	for _, test := range tests {
		if got := test.action.String(); got != test.name {
			t.Errorf("Action(%d).String() = %q, want %q", test.action, got, test.name)
		}
		if got := test.action.Valid(); got != test.valid {
			t.Errorf("Action(%d).Valid() = %v, want %v", test.action, got, test.valid)
		}
	}
}

func TestRoleAtLeast(t *testing.T) {
	tests := []struct {
		role, other Role
		want        bool
	}{
		{RoleViewer, RoleViewer, true},
		{RoleViewer, RoleOperator, false},
		{RoleViewer, RoleAdministrator, false},
		{RoleOperator, RoleViewer, true},
		{RoleOperator, RoleOperator, true},
		{RoleOperator, RoleAdministrator, false},
		{RoleAdministrator, RoleViewer, true},
		{RoleAdministrator, RoleOperator, true},
		{RoleAdministrator, RoleAdministrator, true},
		// Invalid values never satisfy and are never satisfied.
		{Role(0), RoleViewer, false},
		{Role(9), RoleViewer, false},
		{RoleAdministrator, Role(0), false},
		{RoleAdministrator, Role(9), false},
		{Role(0), Role(0), false},
	}
	for _, test := range tests {
		if got := test.role.AtLeast(test.other); got != test.want {
			t.Errorf("%s.AtLeast(%s) = %v, want %v", test.role, test.other, got, test.want)
		}
	}
}

// TestRolePolicyMatrix pins the full role x action matrix from
// docs/adr-auth-sessions.md. Every cell is listed explicitly.
func TestRolePolicyMatrix(t *testing.T) {
	const (
		allow = true
		deny  = false
	)
	tests := []struct {
		role   Role
		action Action
		want   bool
	}{
		{RoleViewer, ActionRead, allow},
		{RoleViewer, ActionCreateIncident, deny},
		{RoleViewer, ActionRejectIncident, deny},
		{RoleViewer, ActionCancelIncident, deny},
		{RoleViewer, ActionApproveRollback, deny},
		{RoleViewer, ActionAdminister, deny},

		{RoleOperator, ActionRead, allow},
		{RoleOperator, ActionCreateIncident, allow},
		{RoleOperator, ActionRejectIncident, allow},
		{RoleOperator, ActionCancelIncident, allow},
		{RoleOperator, ActionApproveRollback, deny},
		{RoleOperator, ActionAdminister, deny},

		{RoleAdministrator, ActionRead, allow},
		{RoleAdministrator, ActionCreateIncident, allow},
		{RoleAdministrator, ActionRejectIncident, allow},
		{RoleAdministrator, ActionCancelIncident, allow},
		{RoleAdministrator, ActionApproveRollback, allow},
		{RoleAdministrator, ActionAdminister, allow},
	}
	policy := RolePolicy{}
	for _, test := range tests {
		t.Run(test.role.String()+"/"+test.action.String(), func(t *testing.T) {
			err := policy.Authorize(test.role, test.action)
			if test.want {
				if err != nil {
					t.Fatalf("Authorize(%s, %s) = %v, want allowed", test.role, test.action, err)
				}
				return
			}
			assertForbidden(t, err, test.role, test.action)
		})
	}
}

func TestRolePolicyDeniesInvalidValues(t *testing.T) {
	allActions := []Action{ActionRead, ActionCreateIncident, ActionRejectIncident, ActionCancelIncident, ActionApproveRollback, ActionAdminister}
	allRoles := []Role{RoleViewer, RoleOperator, RoleAdministrator}
	tests := []struct {
		name   string
		role   Role
		action Action
	}{
		{"zero role, zero action", Role(0), Action(0)},
		{"administrator, zero action", RoleAdministrator, Action(0)},
		{"administrator, out of range action", RoleAdministrator, Action(200)},
	}
	for _, action := range allActions {
		tests = append(tests,
			struct {
				name   string
				role   Role
				action Action
			}{"zero role/" + action.String(), Role(0), action},
			struct {
				name   string
				role   Role
				action Action
			}{"out of range role/" + action.String(), Role(200), action},
		)
	}
	for _, role := range allRoles {
		tests = append(tests, struct {
			name   string
			role   Role
			action Action
		}{role.String() + "/unknown action", role, Action(99)})
	}
	policy := RolePolicy{}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertForbidden(t, policy.Authorize(test.role, test.action), test.role, test.action)
		})
	}
}

func TestForbiddenErrorMessage(t *testing.T) {
	err := RolePolicy{}.Authorize(RoleOperator, ActionApproveRollback)
	if err == nil {
		t.Fatal("expected denial")
	}
	if got, want := err.Error(), `role "operator" may not perform "approve_rollback"`; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if errors.Is(err, ErrValidation) || errors.Is(err, ErrPolicyDenied) {
		t.Errorf("role denial %v must not match validation or rollback-policy sentinels", err)
	}
}

func assertForbidden(t *testing.T, err error, role Role, action Action) {
	t.Helper()
	if err == nil {
		t.Fatalf("Authorize(%d, %d) allowed; want denied", role, action)
	}
	if !errors.Is(err, ErrForbidden) {
		t.Errorf("error %v does not match ErrForbidden", err)
	}
	var forbidden *ForbiddenError
	if !errors.As(err, &forbidden) {
		t.Fatalf("error %T is not *ForbiddenError", err)
	}
	if forbidden.Role != role || forbidden.Action != action {
		t.Errorf("ForbiddenError = {%v, %v}, want {%v, %v}", forbidden.Role, forbidden.Action, role, action)
	}
	if strings.TrimSpace(forbidden.Error()) == "" {
		t.Error("empty error message")
	}
}
