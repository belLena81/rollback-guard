package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/belLena81/rollback-guard/internal/application"
	"github.com/belLena81/rollback-guard/internal/domain"
	"github.com/belLena81/rollback-guard/internal/platform/httpserver"
	"github.com/belLena81/rollback-guard/internal/ports"
)

// GitHubLogin is the login use case behind the OAuth routes.
type GitHubLogin interface {
	Begin(ctx context.Context) (application.LoginStart, error)
	Complete(ctx context.Context, sealedState, state, code string) (application.LoginResult, error)
}

// The OAuth state cookie (docs/phase7-auth.md, "GitHub login"). It is scoped
// to the two auth routes and is SameSite=Lax because the callback is a
// top-level cross-site redirect from GitHub, which drops Strict cookies.
const (
	LoginStateCookieName = "rollback_guard_oauth_state"
	loginStateCookiePath = "/v1/auth/github"
)

// Fixed login failure messages. They never say which check failed.
const (
	messageLoginInvalid  = "the login request is invalid or expired; start the login again"
	messageLoginDenied   = "login denied"
	messageLoginProvider = "the identity provider request failed"
)

// WithGitHubLogin serves GET /v1/auth/github/login and /callback through
// login. cookieSecure sets Secure on the state and session cookies. Without
// this option the routes do not exist (404).
func WithGitHubLogin(login GitHubLogin, cookieSecure bool) Option {
	return func(s *Server) {
		s.login = login
		s.cookieSecure = cookieSecure
	}
}

type sessionResponseDTO struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
	ActorID   string    `json:"actorId"`
	Login     string    `json:"login"`
	Role      string    `json:"role"`
}

// githubLogin starts the OAuth flow: state cookie, then 302 to GitHub.
func (s *Server) githubLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	start, err := s.login.Begin(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     LoginStateCookieName,
		Value:    start.SealedState,
		Path:     loginStateCookiePath,
		Expires:  start.ExpiresAt,
		MaxAge:   int(application.LoginStateTTL / time.Second),
		Secure:   s.cookieSecure,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, start.AuthorizationURL, http.StatusFound)
}

// githubCallback completes the flow. The state cookie is cleared on every
// outcome; the session token is returned once, as cookie and body.
func (s *Server) githubCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.SetCookie(w, &http.Cookie{
		Name:     LoginStateCookieName,
		Value:    "",
		Path:     loginStateCookiePath,
		Expires:  time.Unix(0, 0).UTC(),
		MaxAge:   -1,
		Secure:   s.cookieSecure,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	query := r.URL.Query()
	result, err := s.login.Complete(r.Context(), loginStateCookie(r), singleValue(query["state"]), singleValue(query["code"]))
	if err != nil {
		s.writeLoginError(w, r, err)
		return
	}
	http.SetCookie(w, httpserver.NewSessionCookie(result.Token, result.Session.ExpiresAt(), s.cookieSecure))
	s.logger.InfoContext(r.Context(), "github login succeeded", "actor_id", string(result.Actor.ID()), "request_id", requestID(r))
	s.writeJSON(w, r, http.StatusOK, sessionResponseDTO{
		Token:     result.Token,
		ExpiresAt: result.Session.ExpiresAt().UTC(),
		ActorID:   string(result.Actor.ID()),
		Login:     result.User.Login(),
		Role:      result.User.Role().String(),
	})
}

// loginStateCookie returns the one state cookie; none or several yield "".
func loginStateCookie(r *http.Request) string {
	var value string
	found := 0
	for _, cookie := range r.Cookies() {
		if cookie.Name == LoginStateCookieName {
			value = cookie.Value
			found++
		}
	}
	if found != 1 {
		return ""
	}
	return value
}

func singleValue(values []string) string {
	if len(values) != 1 {
		return ""
	}
	return values[0]
}

// writeLoginError answers a failed callback with a fixed message per class
// and logs only the class, never the error text, code, state or token.
func (s *Server) writeLoginError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message, cause := loginErrorResponse(err)
	s.logger.WarnContext(r.Context(), "github login failed", "status", status, "cause", cause, "request_id", requestID(r))
	s.writeJSON(w, r, status, errorDTO{Error: code, Message: message, RequestID: requestID(r)})
}

func loginErrorResponse(err error) (int, string, string, string) {
	switch {
	case errors.Is(err, application.ErrInvalidLoginRequest):
		return http.StatusBadRequest, codeValidation, messageLoginInvalid, "invalid_request"
	case errors.Is(err, ports.ErrAuthorizationCodeRejected):
		return http.StatusBadRequest, codeValidation, messageLoginInvalid, "code_rejected"
	case errors.Is(err, application.ErrLoginDenied):
		return http.StatusForbidden, codeForbidden, messageLoginDenied, "not_member"
	case errors.Is(err, domain.ErrUserDisabled):
		return http.StatusForbidden, codeForbidden, messageLoginDenied, "user_disabled"
	case errors.Is(err, ports.ErrIdentityProviderFailed):
		return http.StatusBadGateway, codeBadGateway, messageLoginProvider, "provider_failed"
	default:
		return http.StatusInternalServerError, codeInternal, messageInternal, "internal"
	}
}
