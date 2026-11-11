package domain

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// UserID is the orchestrator's internal identifier of a user record.
type UserID string

const (
	// MaxGitHubLoginLength is GitHub's limit on user and organization logins.
	MaxGitHubLoginLength = 39
	// MaxEmailLength is the RFC 5321 limit on a forward path.
	MaxEmailLength = 254
)

// User is a human who signs in through GitHub. It is immutable: every change
// returns a new value with an incremented version. The role is assigned in
// the database, never derived from GitHub (docs/adr-auth-sessions.md).
type User struct {
	id                   UserID
	githubID             int64
	login                string
	email                string
	organizations        []string
	role                 Role
	disabled             bool
	createdAt, updatedAt time.Time
	version              uint64
}

// NewUser builds an enabled user at version 1. Login, email and organization
// names are trimmed; email is optional.
func NewUser(id UserID, githubID int64, login, email string, organizations []string, role Role, now time.Time) (User, error) {
	if now.IsZero() {
		return User{}, invalid("user.created_at", "must be set")
	}
	return RehydrateUser(UserSnapshot{
		ID:            id,
		GitHubID:      githubID,
		Login:         login,
		Email:         email,
		Organizations: organizations,
		Role:          role,
		CreatedAt:     now,
		UpdatedAt:     now,
		Version:       1,
	})
}

// UserSnapshot is the persisted form of a User.
type UserSnapshot struct {
	ID                   UserID
	GitHubID             int64
	Login                string
	Email                string
	Organizations        []string
	Role                 Role
	Disabled             bool
	CreatedAt, UpdatedAt time.Time
	Version              uint64
}

// RehydrateUser rebuilds a User from storage, applying every constructor rule.
func RehydrateUser(s UserSnapshot) (User, error) {
	if err := requireText("user.id", string(s.ID)); err != nil {
		return User{}, err
	}
	if s.GitHubID <= 0 {
		return User{}, invalid("user.github_id", "must be positive")
	}
	login := strings.TrimSpace(s.Login)
	if !validGitHubLogin(login) {
		return User{}, invalid("user.login", fmt.Sprintf("must be 1-%d characters of a-z, A-Z, 0-9 or -", MaxGitHubLoginLength))
	}
	email := strings.TrimSpace(s.Email)
	if email != "" && !validEmail(email) {
		return User{}, invalid("user.email", "must be an address of the form local@domain")
	}
	organizations := make([]string, 0, len(s.Organizations))
	for _, org := range s.Organizations {
		org = strings.TrimSpace(org)
		if !validGitHubLogin(org) {
			return User{}, invalid("user.organizations", "each must be a valid GitHub organization login")
		}
		organizations = append(organizations, org)
	}
	if !s.Role.Valid() {
		return User{}, invalid("user.role", "unknown role")
	}
	if s.CreatedAt.IsZero() {
		return User{}, invalid("user.created_at", "must be set")
	}
	if s.UpdatedAt.IsZero() || s.UpdatedAt.Before(s.CreatedAt) {
		return User{}, invalid("user.updated_at", "must be set and not before created_at")
	}
	if s.Version == 0 {
		return User{}, invalid("user.version", "must be positive")
	}
	return User{
		id:            s.ID,
		githubID:      s.GitHubID,
		login:         login,
		email:         email,
		organizations: organizations,
		role:          s.Role,
		disabled:      s.Disabled,
		createdAt:     s.CreatedAt.UTC(),
		updatedAt:     s.UpdatedAt.UTC(),
		version:       s.Version,
	}, nil
}

// IsGitHubLogin reports whether value is a well-formed GitHub user or
// organization login: 1 to MaxGitHubLoginLength characters of a-z, A-Z, 0-9
// or -.
func IsGitHubLogin(value string) bool {
	return validGitHubLogin(value)
}

func validGitHubLogin(login string) bool {
	if login == "" || len(login) > MaxGitHubLoginLength {
		return false
	}
	return strings.IndexFunc(login, func(r rune) bool { return !gitHubLoginRune(r) }) < 0
}

func gitHubLoginRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-'
}

func validEmail(email string) bool {
	if len(email) > MaxEmailLength || strings.IndexFunc(email, unicode.IsSpace) >= 0 {
		return false
	}
	at := strings.LastIndexByte(email, '@')
	return at > 0 && at < len(email)-1
}

func (u User) ID() UserID              { return u.id }
func (u User) GitHubID() int64         { return u.githubID }
func (u User) Login() string           { return u.login }
func (u User) Email() string           { return u.email }
func (u User) Organizations() []string { return slices.Clone(u.organizations) }
func (u User) Role() Role              { return u.role }
func (u User) Disabled() bool          { return u.disabled }
func (u User) CreatedAt() time.Time    { return u.createdAt }
func (u User) UpdatedAt() time.Time    { return u.updatedAt }
func (u User) Version() uint64         { return u.version }

// Valid reports whether u was produced by NewUser or RehydrateUser.
func (u User) Valid() bool {
	return u.id != "" && u.githubID > 0 && u.login != "" && u.role.Valid() && u.version > 0
}

// Snapshot returns the persisted form of u.
func (u User) Snapshot() UserSnapshot {
	return UserSnapshot{
		ID:            u.id,
		GitHubID:      u.githubID,
		Login:         u.login,
		Email:         u.email,
		Organizations: slices.Clone(u.organizations),
		Role:          u.role,
		Disabled:      u.disabled,
		CreatedAt:     u.createdAt,
		UpdatedAt:     u.updatedAt,
		Version:       u.version,
	}
}

// WithRole returns a copy of u holding role.
func (u User) WithRole(role Role, now time.Time) (User, error) {
	if !role.Valid() {
		return User{}, invalid("user.role", "unknown role")
	}
	return u.changed(now, func(next *User) { next.role = role })
}

// WithProfile returns a copy of u holding the login, email and organizations
// GitHub reported at the latest sign-in, validated like NewUser. The role,
// disabled flag and GitHub ID are unchanged.
func (u User) WithProfile(login, email string, organizations []string, now time.Time) (User, error) {
	if !u.Valid() {
		return User{}, invalid("user", "must be a constructed user")
	}
	snapshot := u.Snapshot()
	snapshot.Login, snapshot.Email, snapshot.Organizations = login, email, organizations
	profile, err := RehydrateUser(snapshot)
	if err != nil {
		return User{}, err
	}
	return u.changed(now, func(next *User) {
		next.login = profile.login
		next.email = profile.email
		next.organizations = profile.organizations
	})
}

// SameProfile reports whether u already holds login, email and organizations
// after the trimming NewUser applies, so a sign-in need not write the row.
func (u User) SameProfile(login, email string, organizations []string) bool {
	if u.login != strings.TrimSpace(login) || u.email != strings.TrimSpace(email) || len(u.organizations) != len(organizations) {
		return false
	}
	for i, org := range organizations {
		if u.organizations[i] != strings.TrimSpace(org) {
			return false
		}
	}
	return true
}

// Disable returns a copy of u that cannot produce an Actor.
func (u User) Disable(now time.Time) (User, error) {
	return u.changed(now, func(next *User) { next.disabled = true })
}

// Enable returns a copy of u that can produce an Actor again.
func (u User) Enable(now time.Time) (User, error) {
	return u.changed(now, func(next *User) { next.disabled = false })
}

func (u User) changed(now time.Time, apply func(*User)) (User, error) {
	if !u.Valid() {
		return User{}, invalid("user", "must be a constructed user")
	}
	if now.IsZero() || now.Before(u.updatedAt) {
		return User{}, invalid("user.updated_at", "must be set and not before the last update")
	}
	next := u
	next.organizations = slices.Clone(u.organizations)
	apply(&next)
	next.updatedAt = now.UTC()
	next.version++
	return next, nil
}

// BootstrapAdmin is one AUTH_BOOTSTRAP_ADMINS entry: a GitHub account created
// as administrator on its first login. Only GitHubID is matched; Login is a
// human-readable label and is never trusted, because a GitHub login can be
// renamed and then taken by a different account.
type BootstrapAdmin struct {
	Login    string
	GitHubID int64
}

// ParseBootstrapAdmin parses "login:githubID". Both parts are required: the
// login must be a GitHub login and the ID a canonical positive decimal.
func ParseBootstrapAdmin(value string) (BootstrapAdmin, error) {
	login, id, ok := strings.Cut(strings.TrimSpace(value), ":")
	login, id = strings.TrimSpace(login), strings.TrimSpace(id)
	if !ok || !validGitHubLogin(login) {
		return BootstrapAdmin{}, invalid("bootstrap_admin", "must be login:githubID with a GitHub login")
	}
	githubID, err := strconv.ParseInt(id, 10, 64)
	if err != nil || githubID <= 0 || strconv.FormatInt(githubID, 10) != id {
		return BootstrapAdmin{}, invalid("bootstrap_admin", "must be login:githubID with a positive numeric GitHub ID")
	}
	return BootstrapAdmin{Login: login, GitHubID: githubID}, nil
}

// ActorFor returns the human Actor that attributes u's actions in the audit
// trail. Its ID is "github:<GitHubID>", stable across login renames. A
// disabled user cannot act.
func ActorFor(u User) (Actor, error) {
	if !u.Valid() {
		return Actor{}, invalid("user", "must be a constructed user")
	}
	if u.disabled {
		return Actor{}, &UserDisabledError{UserID: u.id}
	}
	return NewActor(ActorID(githubActorPrefix+strconv.FormatInt(u.githubID, 10)), ActorHuman)
}

// githubActorPrefix is the ID prefix of an Actor derived from a GitHub user.
const githubActorPrefix = "github:"

// GitHubIDOf parses the canonical "github:<positive decimal>" actor ID that
// ActorFor produces. Any other form, including a non-canonical number, is
// not a GitHub actor.
func GitHubIDOf(id ActorID) (int64, bool) {
	raw, ok := strings.CutPrefix(string(id), githubActorPrefix)
	if !ok {
		return 0, false
	}
	githubID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || githubID <= 0 || strconv.FormatInt(githubID, 10) != raw {
		return 0, false
	}
	return githubID, true
}
