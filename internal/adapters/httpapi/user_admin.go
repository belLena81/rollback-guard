package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/belLena81/rollback-guard/internal/application"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/httpserver"
)

// maxAuditCursorLength bounds the cursor query value before decoding.
const maxAuditCursorLength = 256

// auditEventDTO carries exactly the columns the audit table holds.
type auditEventDTO struct {
	ID           string `json:"id"`
	OccurredAt   string `json:"occurredAt"`
	ActorID      string `json:"actorId"`
	Action       string `json:"action"`
	TargetUserID string `json:"targetUserId,omitempty"`
	FromRole     string `json:"fromRole,omitempty"`
	ToRole       string `json:"toRole,omitempty"`
	SessionCount *int   `json:"sessionCount,omitempty"`
}

type auditPageDTO struct {
	Events     []auditEventDTO `json:"events"`
	NextCursor string          `json:"nextCursor,omitempty"`
}

func auditEventOf(e domain.AuthAuditEvent) auditEventDTO {
	dto := auditEventDTO{
		ID:           string(e.ID()),
		OccurredAt:   e.OccurredAt().UTC().Format(time.RFC3339Nano),
		ActorID:      string(e.Actor().ID()),
		Action:       e.Action().String(),
		TargetUserID: string(e.TargetUserID()),
	}
	if e.FromRole() != 0 {
		dto.FromRole = e.FromRole().String()
	}
	if e.ToRole() != 0 {
		dto.ToRole = e.ToRole().String()
	}
	if n, ok := e.SessionCount(); ok {
		dto.SessionCount = &n
	}
	return dto
}

// listAudit serves GET /v1/auth/audit (administrators only).
func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	actor, ok := requestActor(w, r)
	if !ok {
		return
	}
	limit := application.DefaultAuditPageSize
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			s.writeError(w, r, &domain.ValidationError{Field: "limit", Reason: "must be an integer"})
			return
		}
		limit = parsed
	}
	cursor := r.URL.Query().Get("cursor")
	if len(cursor) > maxAuditCursorLength {
		s.writeError(w, r, &domain.ValidationError{Field: "cursor", Reason: "is not a valid cursor"})
		return
	}
	page, err := s.admin.ListAuditEvents(r.Context(), actor, limit, cursor)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	body := auditPageDTO{Events: make([]auditEventDTO, 0, len(page.Events)), NextCursor: page.NextCursor}
	for _, e := range page.Events {
		body.Events = append(body.Events, auditEventOf(e))
	}
	s.writeJSON(w, r, http.StatusOK, body)
}

// UserAdmin is the logout and user administration use case behind the
// session routes.
type UserAdmin interface {
	ChangeRole(ctx context.Context, actor domain.Actor, target domain.ActorID, role domain.Role) (domain.User, error)
	RevokeUserSessions(ctx context.Context, actor domain.Actor, target domain.ActorID) (int, error)
	DisableUser(ctx context.Context, actor domain.Actor, target domain.ActorID) (domain.User, error)
	EnableUser(ctx context.Context, actor domain.Actor, target domain.ActorID) (domain.User, error)
	Logout(ctx context.Context, principal domain.Principal) error
	ListAuditEvents(ctx context.Context, actor domain.Actor, limit int, cursor string) (application.AuditPage, error)
}

// Session route patterns (api/openapi.yaml).
const (
	LogoutPattern         = "POST /v1/auth/logout"
	MePattern             = "GET /v1/me"
	SetUserRolePattern    = "PATCH /v1/users/{userId}/role"
	RevokeSessionsPattern = "POST /v1/users/{userId}/sessions:revoke"
	DisableUserPattern    = "POST /v1/users/{userId}/disable"
	EnableUserPattern     = "POST /v1/users/{userId}/enable"
	ListAuditPattern      = "GET /v1/auth/audit"
)

// maxRoleBodyBytes bounds the PATCH role body; {"role":"administrator"} is
// 24 bytes.
const maxRoleBodyBytes = 1 << 10

// WithUserAdmin serves logout, /v1/me and the user administration routes
// through admin. They need a session principal, so only AUTH_MODE=github
// wires this option; without it the routes do not exist (404).
func WithUserAdmin(admin UserAdmin) Option {
	return func(s *Server) { s.admin = admin }
}

type meDTO struct {
	ActorID string `json:"actorId"`
	Login   string `json:"login"`
	Role    string `json:"role"`
}

type setRoleRequest struct {
	Role string `json:"role"`
}

// userSummaryDTO describes a user who may be disabled, so unlike meDTO it
// is not built from an Actor (a disabled user has none).
type userSummaryDTO struct {
	ActorID  string `json:"actorId"`
	Login    string `json:"login"`
	Role     string `json:"role"`
	Disabled bool   `json:"disabled"`
}

func summaryOf(user domain.User) userSummaryDTO {
	return userSummaryDTO{
		ActorID:  fmt.Sprintf("github:%d", user.GitHubID()),
		Login:    user.Login(),
		Role:     user.Role().String(),
		Disabled: user.Disabled(),
	}
}

type revokedSessionsDTO struct {
	Revoked int `json:"revoked"`
}

func meOf(user domain.User) (meDTO, error) {
	actor, err := domain.ActorFor(user)
	if err != nil {
		return meDTO{}, err //nolint:wrapcheck // a typed domain validation error
	}
	return meDTO{ActorID: string(actor.ID()), Login: user.Login(), Role: user.Role().String()}, nil
}

// requestPrincipal returns the session principal or answers 401.
func requestPrincipal(w http.ResponseWriter, r *http.Request) (domain.Principal, bool) {
	principal, ok := httpserver.PrincipalFromContext(r.Context())
	if !ok || !principal.Valid() {
		writeJSON(w, http.StatusUnauthorized, errorDTO{Error: codeUnauthenticated, Message: messageUnauthenticated, RequestID: requestID(r)})
		return domain.Principal{}, false
	}
	return principal, true
}

// logout revokes the caller's session and clears the cookie (204).
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	principal, ok := requestPrincipal(w, r)
	if !ok {
		return
	}
	if err := s.admin.Logout(r.Context(), principal); err != nil {
		s.writeError(w, r, err)
		return
	}
	http.SetCookie(w, httpserver.ClearSessionCookie(s.cookieSecure))
	w.WriteHeader(http.StatusNoContent)
}

// me returns the caller's identity. The session middleware resolved the user
// on this request, so the role is the live one.
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	principal, ok := requestPrincipal(w, r)
	if !ok {
		return
	}
	body, err := meOf(principal.User())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, body)
}

func (s *Server) setUserRole(w http.ResponseWriter, r *http.Request) {
	actor, ok := requestActor(w, r)
	if !ok {
		return
	}
	var request setRoleRequest
	if !decodeJSONLimited(w, r, &request, maxRoleBodyBytes) {
		return
	}
	role, err := domain.ParseRole(request.Role)
	if err != nil {
		s.writeError(w, r, &domain.ValidationError{Field: "role", Reason: "must be viewer, operator or administrator"})
		return
	}
	user, err := s.admin.ChangeRole(r.Context(), actor, domain.ActorID(r.PathValue("userId")), role)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	body, err := meOf(user)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, body)
}

func (s *Server) revokeUserSessions(w http.ResponseWriter, r *http.Request) {
	actor, ok := requestActor(w, r)
	if !ok {
		return
	}
	revoked, err := s.admin.RevokeUserSessions(r.Context(), actor, domain.ActorID(r.PathValue("userId")))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, revokedSessionsDTO{Revoked: revoked})
}

func (s *Server) disableUser(w http.ResponseWriter, r *http.Request) {
	s.setUserStatus(w, r, s.admin.DisableUser)
}

func (s *Server) enableUser(w http.ResponseWriter, r *http.Request) {
	s.setUserStatus(w, r, s.admin.EnableUser)
}

func (s *Server) setUserStatus(w http.ResponseWriter, r *http.Request, change func(context.Context, domain.Actor, domain.ActorID) (domain.User, error)) {
	actor, ok := requestActor(w, r)
	if !ok {
		return
	}
	user, err := change(r.Context(), actor, domain.ActorID(r.PathValue("userId")))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.writeJSON(w, r, http.StatusOK, summaryOf(user))
}
