package httpapi

import (
	"context"
	"net/http"

	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/httpserver"
)

// UserAdmin is the logout and user administration use case behind the
// session routes.
type UserAdmin interface {
	ChangeRole(ctx context.Context, actor domain.Actor, target domain.ActorID, role domain.Role) (domain.User, error)
	RevokeUserSessions(ctx context.Context, actor domain.Actor, target domain.ActorID) (int, error)
	Logout(ctx context.Context, principal domain.Principal) error
}

// Session route patterns (api/openapi.yaml).
const (
	LogoutPattern         = "POST /v1/auth/logout"
	MePattern             = "GET /v1/me"
	SetUserRolePattern    = "PATCH /v1/users/{userId}/role"
	RevokeSessionsPattern = "POST /v1/users/{userId}/sessions:revoke"
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
