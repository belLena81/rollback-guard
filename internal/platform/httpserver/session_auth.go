package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/belLena81/rollback-guard/internal/domain"
)

// Routes that need no session (docs/phase7-auth.md, "Sessions").
const (
	MetricsPath        = "/metrics"
	GitHubLoginPath    = "/v1/auth/github/login"
	GitHubCallbackPath = "/v1/auth/github/callback"
)

// publicPaths is matched exactly against the request path; every other path
// requires a session. A prefix, a trailing slash or a sub-path is not public.
var publicPaths = map[string]struct{}{
	LivePath:           {},
	ReadyPath:          {},
	MetricsPath:        {},
	GitHubLoginPath:    {},
	GitHubCallbackPath: {},
}

// IsPublicPath reports whether path is served without a session.
func IsPublicPath(path string) bool {
	_, ok := publicPaths[path]
	return ok
}

// SessionResolver turns a raw session token into the live Principal behind
// it. Every authentication failure must match domain.ErrUnauthenticated; any
// other error is treated as an infrastructure failure.
type SessionResolver interface {
	Resolve(ctx context.Context, rawToken string) (domain.Principal, error)
}

type principalContextKey struct{}

// PrincipalFromContext returns the Principal SessionAuthMiddleware resolved
// for the request.
func PrincipalFromContext(ctx context.Context) (domain.Principal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(domain.Principal)
	if !ok || !principal.Valid() {
		return domain.Principal{}, false
	}
	return principal, true
}

// Error codes and fixed messages of the JSON error envelope
// (api/openapi.yaml, components.schemas.Error). The body never depends on why
// authentication failed and never echoes request content.
const (
	codeUnauthenticated    = "unauthenticated"
	codeForbidden          = "forbidden"
	codeInternal           = "internal_error"
	messageUnauthenticated = "authentication required"
	messageCrossSite       = "cross-site request rejected"
	messageInternal        = "internal server error"
	requestIDHeader        = "X-Request-Id"
	bearerScheme           = "Bearer"
)

// SessionAuthMiddleware is the session-backed AuthMiddleware. It resolves
// the session on every request, so a revocation, disable or role change
// applies to the next request. Tokens are read from Authorization: Bearer
// (preferred when the header is present) or from the session cookie.
// Cookie-carried unsafe requests must be same-origin.
type SessionAuthMiddleware struct {
	resolver SessionResolver
	logger   *slog.Logger
}

func NewSessionAuthMiddleware(resolver SessionResolver, logger *slog.Logger) (SessionAuthMiddleware, error) {
	if resolver == nil || logger == nil {
		return SessionAuthMiddleware{}, errors.New("session auth middleware needs a resolver and a logger")
	}
	return SessionAuthMiddleware{resolver: resolver, logger: logger}, nil
}

type tokenCarrier int

const (
	carrierNone tokenCarrier = iota
	carrierBearer
	carrierCookie
)

func (c tokenCarrier) String() string {
	switch c {
	case carrierBearer:
		return "bearer"
	case carrierCookie:
		return "cookie"
	case carrierNone:
		return "none"
	default:
		return "none"
	}
}

func (m SessionAuthMiddleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if IsPublicPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		token, carrier := sessionToken(r)
		if carrier == carrierNone {
			m.unauthenticated(w, r, carrier)
			return
		}
		principal, err := m.resolver.Resolve(r.Context(), token)
		switch {
		case errors.Is(err, domain.ErrUnauthenticated):
			m.unauthenticated(w, r, carrier)
			return
		case err != nil || !principal.Valid():
			// Fail closed, but as a server error: an outage must not look
			// like a logged-out client. The cause may hold infrastructure
			// detail, so only a fixed classification is logged.
			m.logger.ErrorContext(r.Context(), "session resolution failed", "cause", "internal")
			writeAuthError(w, http.StatusInternalServerError, codeInternal, messageInternal)
			return
		}
		if carrier == carrierCookie && unsafeMethod(r.Method) && !sameOrigin(r) {
			m.logger.WarnContext(r.Context(), "cross-site request rejected", "method", r.Method)
			writeAuthError(w, http.StatusForbidden, codeForbidden, messageCrossSite)
			return
		}
		ctx := context.WithValue(r.Context(), actorContextKey{}, principal.Actor())
		ctx = context.WithValue(ctx, principalContextKey{}, principal)
		next.ServeHTTP(w, withoutCredentials(r.WithContext(ctx)))
	})
}

func (m SessionAuthMiddleware) unauthenticated(w http.ResponseWriter, r *http.Request, carrier tokenCarrier) {
	m.logger.DebugContext(r.Context(), "request unauthenticated", "carrier", carrier.String())
	w.Header().Set("WWW-Authenticate", bearerScheme)
	writeAuthError(w, http.StatusUnauthorized, codeUnauthenticated, messageUnauthenticated)
}

// sessionToken returns the request's token and where it came from. A present
// Authorization header wins and must be a single Bearer credential; an
// unusable one is not replaced by the cookie. A malformed carrier yields an
// empty token, which the resolver rejects like any other bad token.
func sessionToken(r *http.Request) (string, tokenCarrier) {
	if values := r.Header.Values("Authorization"); len(values) > 0 {
		if len(values) != 1 {
			return "", carrierBearer
		}
		scheme, token, ok := strings.Cut(values[0], " ")
		if !ok || !strings.EqualFold(scheme, bearerScheme) {
			return "", carrierBearer
		}
		return token, carrierBearer
	}
	var token string
	found := 0
	for _, cookie := range r.Cookies() {
		if cookie.Name == SessionCookieName {
			token = cookie.Value
			found++
		}
	}
	switch found {
	case 0:
		return "", carrierNone
	case 1:
		return token, carrierCookie
	default:
		// Duplicates can be planted by a sibling domain; refuse to pick one.
		return "", carrierCookie
	}
}

func unsafeMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// sameOrigin accepts a request whose Origin (or, without Origin, Referer)
// names the request's own host, or that sends neither header.
func sameOrigin(r *http.Request) bool {
	source := r.Header.Get("Origin")
	if source == "" {
		source = r.Header.Get("Referer")
	}
	if source == "" {
		return true
	}
	parsed, err := url.Parse(source)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return false
	}
	return strings.EqualFold(parsed.Host, r.Host)
}

// withoutCredentials returns r with the Authorization header and the session
// cookie removed, so no handler can read, echo or log the token.
func withoutCredentials(r *http.Request) *http.Request {
	clean := r.Clone(r.Context())
	clean.Header.Del("Authorization")
	cookies := clean.Cookies()
	clean.Header.Del("Cookie")
	for _, cookie := range cookies {
		if cookie.Name != SessionCookieName {
			clean.AddCookie(cookie)
		}
	}
	return clean
}

type authErrorBody struct {
	Error     string `json:"error"`
	Message   string `json:"message"`
	RequestID string `json:"requestId,omitempty"`
}

// writeAuthError writes the repo's JSON error envelope. The request ID is the
// sanitized one the API's request-ID middleware already put on the response.
func writeAuthError(w http.ResponseWriter, status int, code, message string) {
	var body bytes.Buffer
	//nolint:errcheck,errchkjson // a struct of three strings always encodes
	_ = json.NewEncoder(&body).Encode(authErrorBody{Error: code, Message: message, RequestID: w.Header().Get(requestIDHeader)})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if _, err := w.Write(body.Bytes()); err != nil {
		return
	}
}
