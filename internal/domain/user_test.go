package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var userNow = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)

type userInput struct {
	id       UserID
	githubID int64
	login    string
	email    string
	orgs     []string
	role     Role
	now      time.Time
}

func validUserInput() userInput {
	return userInput{
		id:       "user-1",
		githubID: 42,
		login:    "octo-cat",
		email:    "octo@example.com",
		orgs:     []string{"acme", "acme-ops"},
		role:     RoleViewer,
		now:      userNow,
	}
}

func (in userInput) build() (User, error) {
	return NewUser(in.id, in.githubID, in.login, in.email, in.orgs, in.role, in.now)
}

func mustUser(t *testing.T, mutate func(*userInput)) User {
	t.Helper()
	in := validUserInput()
	if mutate != nil {
		mutate(&in)
	}
	user, err := in.build()
	if err != nil {
		t.Fatalf("NewUser: %v", err)
	}
	return user
}

func TestNewUserValidation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*userInput)
		field  string // empty means valid
	}{
		{"valid", func(*userInput) {}, ""},
		{"valid without email", func(in *userInput) { in.email = "" }, ""},
		{"valid without organizations", func(in *userInput) { in.orgs = nil }, ""},
		{"valid single char login", func(in *userInput) { in.login = "a" }, ""},
		{"valid 39 char login", func(in *userInput) { in.login = strings.Repeat("a", 39) }, ""},
		{"valid digits and upper case", func(in *userInput) { in.login = "Octo-Cat-2026" }, ""},
		{"valid administrator", func(in *userInput) { in.role = RoleAdministrator }, ""},

		{"empty id", func(in *userInput) { in.id = "" }, "user.id"},
		{"blank id", func(in *userInput) { in.id = "  " }, "user.id"},
		{"zero github id", func(in *userInput) { in.githubID = 0 }, "user.github_id"},
		{"negative github id", func(in *userInput) { in.githubID = -1 }, "user.github_id"},
		{"empty login", func(in *userInput) { in.login = "" }, "user.login"},
		{"blank login", func(in *userInput) { in.login = "   " }, "user.login"},
		{"40 char login", func(in *userInput) { in.login = strings.Repeat("a", 40) }, "user.login"},
		{"login with underscore", func(in *userInput) { in.login = "octo_cat" }, "user.login"},
		{"login with dot", func(in *userInput) { in.login = "octo.cat" }, "user.login"},
		{"login with inner space", func(in *userInput) { in.login = "octo cat" }, "user.login"},
		{"login with slash", func(in *userInput) { in.login = "octo/cat" }, "user.login"},
		{"login with non-ascii", func(in *userInput) { in.login = "octö" }, "user.login"},
		{"email without at", func(in *userInput) { in.email = "octo.example.com" }, "user.email"},
		{"email with space", func(in *userInput) { in.email = "octo @example.com" }, "user.email"},
		{"email only at", func(in *userInput) { in.email = "@" }, "user.email"},
		{"email too long", func(in *userInput) { in.email = strings.Repeat("a", 250) + "@x.io" }, "user.email"},
		{"blank organization", func(in *userInput) { in.orgs = []string{"acme", " "} }, "user.organizations"},
		{"invalid organization", func(in *userInput) { in.orgs = []string{"acme/ops"} }, "user.organizations"},
		{"zero role", func(in *userInput) { in.role = Role(0) }, "user.role"},
		{"out of range role", func(in *userInput) { in.role = Role(17) }, "user.role"},
		{"zero time", func(in *userInput) { in.now = time.Time{} }, "user.created_at"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			in := validUserInput()
			test.mutate(&in)
			user, err := in.build()
			if test.field == "" {
				if err != nil {
					t.Fatalf("NewUser error = %v, want nil", err)
				}
				if !user.Valid() {
					t.Error("constructed user is not Valid")
				}
				return
			}
			var validation *ValidationError
			if !errors.As(err, &validation) {
				t.Fatalf("NewUser error = %v, want *ValidationError", err)
			}
			if validation.Field != test.field {
				t.Errorf("field = %q, want %q", validation.Field, test.field)
			}
			if user.Valid() {
				t.Error("zero user returned with an error must not be Valid")
			}
		})
	}
}

func TestNewUserNormalizesAndExposesFields(t *testing.T) {
	local := time.FixedZone("UTC+2", 2*60*60)
	user := mustUser(t, func(in *userInput) {
		in.login = "  octo-cat  "
		in.email = "  octo@example.com "
		in.orgs = []string{" acme "}
		in.now = userNow.In(local)
	})
	if user.ID() != "user-1" || user.GitHubID() != 42 {
		t.Errorf("identity = %q/%d", user.ID(), user.GitHubID())
	}
	if user.Login() != "octo-cat" {
		t.Errorf("login = %q, want trimmed", user.Login())
	}
	if user.Email() != "octo@example.com" {
		t.Errorf("email = %q, want trimmed", user.Email())
	}
	if got := user.Organizations(); len(got) != 1 || got[0] != "acme" {
		t.Errorf("organizations = %q, want [acme]", got)
	}
	if user.Role() != RoleViewer || user.Disabled() {
		t.Errorf("role/disabled = %s/%v", user.Role(), user.Disabled())
	}
	if !user.CreatedAt().Equal(userNow) || user.CreatedAt().Location() != time.UTC || !user.UpdatedAt().Equal(userNow) {
		t.Errorf("timestamps = %v/%v, want UTC %v", user.CreatedAt(), user.UpdatedAt(), userNow)
	}
	if user.Version() != 1 {
		t.Errorf("version = %d, want 1", user.Version())
	}
}

func TestUserOrganizationsAreCopiedDefensively(t *testing.T) {
	orgs := []string{"acme", "acme-ops"}
	user := mustUser(t, func(in *userInput) { in.orgs = orgs })

	orgs[0] = "mutated-input"
	if got := user.Organizations()[0]; got != "acme" {
		t.Fatalf("caller mutation of input leaked into user: %q", got)
	}

	out := user.Organizations()
	out[1] = "mutated-output"
	if got := user.Organizations()[1]; got != "acme-ops" {
		t.Fatalf("caller mutation of output leaked into user: %q", got)
	}

	changed, err := user.WithRole(RoleOperator, userNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	changed.Organizations()[0] = "mutated-derived"
	if got := user.Organizations()[0]; got != "acme" {
		t.Fatalf("derived value shares backing array with original: %q", got)
	}
}

func TestUserWithRole(t *testing.T) {
	later := userNow.Add(time.Hour)
	tests := []struct {
		name  string
		role  Role
		now   time.Time
		field string
	}{
		{"promote to operator", RoleOperator, later, ""},
		{"promote to administrator", RoleAdministrator, later, ""},
		{"same role", RoleViewer, later, ""},
		{"zero role", Role(0), later, "user.role"},
		{"out of range role", Role(8), later, "user.role"},
		{"zero time", RoleOperator, time.Time{}, "user.updated_at"},
		{"time before last update", RoleOperator, userNow.Add(-time.Second), "user.updated_at"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			original := mustUser(t, nil)
			changed, err := original.WithRole(test.role, test.now)
			if original.Role() != RoleViewer || original.Version() != 1 || !original.UpdatedAt().Equal(userNow) {
				t.Fatal("WithRole mutated the receiver")
			}
			if test.field != "" {
				var validation *ValidationError
				if !errors.As(err, &validation) || validation.Field != test.field {
					t.Fatalf("WithRole error = %v, want validation on %s", err, test.field)
				}
				return
			}
			if err != nil {
				t.Fatalf("WithRole: %v", err)
			}
			if changed.Role() != test.role || changed.Version() != 2 || !changed.UpdatedAt().Equal(test.now) {
				t.Errorf("changed = role %s version %d updated %v", changed.Role(), changed.Version(), changed.UpdatedAt())
			}
			if !changed.CreatedAt().Equal(original.CreatedAt()) || changed.ID() != original.ID() {
				t.Error("WithRole changed identity or creation time")
			}
		})
	}
}

func TestUserDisableEnable(t *testing.T) {
	original := mustUser(t, nil)
	t1 := userNow.Add(time.Minute)
	disabled, err := original.Disable(t1)
	if err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if original.Disabled() {
		t.Fatal("Disable mutated the receiver")
	}
	if !disabled.Disabled() || disabled.Version() != 2 || !disabled.UpdatedAt().Equal(t1) {
		t.Errorf("disabled = %v version %d updated %v", disabled.Disabled(), disabled.Version(), disabled.UpdatedAt())
	}

	t2 := t1.Add(time.Minute)
	enabled, err := disabled.Enable(t2)
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if !disabled.Disabled() {
		t.Fatal("Enable mutated the receiver")
	}
	if enabled.Disabled() || enabled.Version() != 3 || !enabled.UpdatedAt().Equal(t2) {
		t.Errorf("enabled = %v version %d updated %v", enabled.Disabled(), enabled.Version(), enabled.UpdatedAt())
	}

	for name, call := range map[string]func(time.Time) (User, error){
		"disable": original.Disable,
		"enable":  disabled.Enable,
	} {
		for _, bad := range []time.Time{{}, userNow.Add(-time.Hour)} {
			if _, err := call(bad); !errors.Is(err, ErrValidation) {
				t.Errorf("%s(%v) error = %v, want validation", name, bad, err)
			}
		}
	}
}

func TestUserMethodsRejectZeroValue(t *testing.T) {
	var zero User
	if zero.Valid() {
		t.Fatal("zero User must not be Valid")
	}
	if _, err := zero.WithRole(RoleAdministrator, userNow); !errors.Is(err, ErrValidation) {
		t.Errorf("zero.WithRole error = %v, want validation", err)
	}
	if _, err := zero.Disable(userNow); !errors.Is(err, ErrValidation) {
		t.Errorf("zero.Disable error = %v, want validation", err)
	}
	if _, err := zero.Enable(userNow); !errors.Is(err, ErrValidation) {
		t.Errorf("zero.Enable error = %v, want validation", err)
	}
}

func TestActorFor(t *testing.T) {
	enabled := mustUser(t, func(in *userInput) { in.githubID = 9876543210; in.role = RoleAdministrator })
	disabled, err := enabled.Disable(userNow.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	actor, err := ActorFor(enabled)
	if err != nil {
		t.Fatalf("ActorFor(enabled): %v", err)
	}
	if actor.ID() != "github:9876543210" || actor.Kind() != ActorHuman || !actor.IsHuman() || !actor.Valid() {
		t.Errorf("actor = %q/%s", actor.ID(), actor.Kind())
	}

	tests := []struct {
		name   string
		user   User
		target error
	}{
		{"disabled user", disabled, ErrUserDisabled},
		{"zero user", User{}, ErrValidation},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ActorFor(test.user)
			if !errors.Is(err, test.target) {
				t.Fatalf("ActorFor error = %v, want %v", err, test.target)
			}
			if got.Valid() {
				t.Errorf("ActorFor returned a valid actor %q alongside an error", got.ID())
			}
		})
	}

	var disabledErr *UserDisabledError
	_, err = ActorFor(disabled)
	if !errors.As(err, &disabledErr) || disabledErr.UserID != "user-1" {
		t.Fatalf("error = %v, want *UserDisabledError for user-1", err)
	}
	if strings.Contains(err.Error(), "octo@example.com") {
		t.Errorf("error message leaks email: %q", err.Error())
	}
}

func TestRehydrateUser(t *testing.T) {
	base := UserSnapshot{
		ID:            "user-1",
		GitHubID:      42,
		Login:         "octo-cat",
		Email:         "octo@example.com",
		Organizations: []string{"acme"},
		Role:          RoleOperator,
		Disabled:      true,
		CreatedAt:     userNow,
		UpdatedAt:     userNow.Add(time.Hour),
		Version:       7,
	}
	user, err := RehydrateUser(base)
	if err != nil {
		t.Fatalf("RehydrateUser: %v", err)
	}
	if got := user.Snapshot(); got.Version != 7 || !got.Disabled || got.Role != RoleOperator || !got.UpdatedAt.Equal(base.UpdatedAt) {
		t.Errorf("snapshot round trip = %+v", got)
	}
	base.Organizations[0] = "mutated"
	if user.Organizations()[0] != "acme" {
		t.Error("RehydrateUser did not copy organizations")
	}

	tests := []struct {
		name   string
		mutate func(*UserSnapshot)
	}{
		{"zero version", func(s *UserSnapshot) { s.Version = 0 }},
		{"updated before created", func(s *UserSnapshot) { s.UpdatedAt = userNow.Add(-time.Second) }},
		{"zero updated", func(s *UserSnapshot) { s.UpdatedAt = time.Time{} }},
		{"invalid role", func(s *UserSnapshot) { s.Role = 0 }},
		{"invalid login", func(s *UserSnapshot) { s.Login = "bad login" }},
		{"invalid github id", func(s *UserSnapshot) { s.GitHubID = 0 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := user.Snapshot()
			test.mutate(&snapshot)
			if _, err := RehydrateUser(snapshot); !errors.Is(err, ErrValidation) {
				t.Errorf("RehydrateUser error = %v, want validation", err)
			}
		})
	}
}
