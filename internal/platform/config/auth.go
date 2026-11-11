package config

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
)

// AuthMode selects how the orchestrator authenticates requests
// (docs/phase7-auth.md, "GitHub login").
type AuthMode string

const (
	// AuthModeLocal injects the fixed LOCAL_ACTOR_ID on every request. It is
	// for local development only; production must run AuthModeGitHub.
	AuthModeLocal AuthMode = "local"
	// AuthModeGitHub requires a session issued by the GitHub OAuth login.
	AuthModeGitHub AuthMode = "github"
)

// GitHub login settings.
const (
	// defaultAuthMode fails closed: without AUTH_MODE the orchestrator needs
	// the GitHub settings and refuses to start without them. Development and
	// demo runs opt in to AUTH_MODE=local explicitly.
	defaultAuthMode      = AuthModeGitHub
	DefaultGitHubBaseURL = "https://github.com"
	DefaultGitHubAPIURL  = "https://api.github.com"
	// MinAuthStateSecretBytes is the minimum AUTH_STATE_SECRET length: the
	// HMAC-SHA256 key of the login state cookie needs at least 256 bits.
	MinAuthStateSecretBytes = 32
	// GitHubHTTPTimeout bounds each request to GitHub (token exchange, user,
	// one organization page).
	GitHubHTTPTimeout = 10 * time.Second
	// maxGitHubClientIDLength bounds AUTH_GITHUB_CLIENT_ID.
	maxGitHubClientIDLength = 128
	// maxBootstrapAdmins bounds AUTH_BOOTSTRAP_ADMINS.
	maxBootstrapAdmins = 32
)

// redactedSecret replaces a Secret in every rendering.
const redactedSecret = "[REDACTED]"

// Secret holds a credential read from the environment. Every rendering
// (fmt verbs, slog, JSON) prints a fixed placeholder; only Reveal returns the
// value, for the one adapter that needs it.
type Secret struct {
	value string
}

// NewSecret wraps value.
func NewSecret(value string) Secret { return Secret{value: value} }

// Reveal returns the secret value. Call it only where the value is handed to
// the component that uses it, never for logging.
func (s Secret) Reveal() string { return s.value }

// IsZero reports whether the secret is empty.
func (s Secret) IsZero() bool { return s.value == "" }

func (Secret) String() string               { return redactedSecret }
func (Secret) GoString() string             { return redactedSecret }
func (Secret) LogValue() slog.Value         { return slog.StringValue(redactedSecret) }
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redactedSecret + `"`), nil }
func (Secret) MarshalText() ([]byte, error) { return []byte(redactedSecret), nil }
func (Secret) Format(state fmt.State, _ rune) {
	if _, err := io.WriteString(state, redactedSecret); err != nil {
		return // fmt.State writes to a buffer; there is nobody to report to.
	}
}

// AuthConfig configures request authentication of the orchestrator.
type AuthConfig struct {
	// Mode is AUTH_MODE: github (default) or local (development only, set
	// explicitly).
	Mode AuthMode
	// GitHub is set only in github mode.
	GitHub GitHubAuthConfig
	// InsecureLocalBind is true when local mode listens on a non-loopback
	// HTTP_ADDR because ALLOW_INSECURE_LOCAL_AUTH=true; the orchestrator logs
	// a warning at startup.
	InsecureLocalBind bool
}

// GitHubAuthConfig configures the GitHub OAuth login (AUTH_GITHUB_*).
type GitHubAuthConfig struct {
	ClientID     string
	ClientSecret Secret
	// RedirectURL is the registered callback URL of the OAuth app.
	RedirectURL string
	// AllowedOrganization is the one GitHub organization whose members may
	// sign in (case-insensitive).
	AllowedOrganization string
	// BootstrapAdmins are GitHub accounts, matched by numeric ID only,
	// created as administrators on their first login.
	BootstrapAdmins []domain.BootstrapAdmin
	// StateSecret keys the HMAC of the login state cookie.
	StateSecret Secret
	BaseURL     string
	APIURL      string
	HTTPTimeout time.Duration
}

func authEnv() (AuthConfig, error) {
	mode := AuthMode(strings.TrimSpace(os.Getenv("AUTH_MODE")))
	switch mode {
	case "":
		mode = defaultAuthMode
	case AuthModeLocal, AuthModeGitHub:
	default:
		return AuthConfig{}, errors.New("AUTH_MODE must be local or github")
	}
	if mode == AuthModeLocal {
		// The GitHub settings are ignored, not validated: a local run may
		// share an environment file with a github deployment.
		return AuthConfig{Mode: mode}, nil
	}
	github, err := githubAuthEnv()
	if err != nil {
		return AuthConfig{}, err
	}
	return AuthConfig{Mode: mode, GitHub: github}, nil
}

// ApproverAuthConfig tells the rollback-mode Kubernetes MCP how to re-check
// an approval's actor before a mutation. Mode must match the orchestrator's
// AUTH_MODE. github: the approver must currently be an enabled administrator
// in users. local: only an approval by LocalActorID (the orchestrator's fixed
// local actor) is executable.
type ApproverAuthConfig struct {
	Mode         AuthMode
	LocalActorID domain.ActorID
}

// approverAuthEnv reads AUTH_MODE for the rollback MCP. Unlike the
// orchestrator it has no default: an unset or unknown mode refuses to start,
// so the execution-time approver check can never be silently skipped.
func approverAuthEnv() (ApproverAuthConfig, error) {
	switch mode := AuthMode(strings.TrimSpace(os.Getenv("AUTH_MODE"))); mode {
	case AuthModeGitHub:
		return ApproverAuthConfig{Mode: mode}, nil
	case AuthModeLocal:
		return ApproverAuthConfig{Mode: mode, LocalActorID: domain.ActorID(stringEnv("LOCAL_ACTOR_ID", defaultLocalActorID))}, nil
	default:
		return ApproverAuthConfig{}, errors.New("AUTH_MODE must be local or github in rollback mode (no default)")
	}
}

func githubAuthEnv() (GitHubAuthConfig, error) {
	clientID := strings.TrimSpace(os.Getenv("AUTH_GITHUB_CLIENT_ID"))
	if clientID == "" || len(clientID) > maxGitHubClientIDLength || strings.ContainsFunc(clientID, notVisibleASCII) {
		return GitHubAuthConfig{}, errors.New("AUTH_GITHUB_CLIENT_ID is required in github mode and must be printable ASCII")
	}
	// Secrets are not trimmed: the value is used exactly as provided.
	clientSecret := os.Getenv("AUTH_GITHUB_CLIENT_SECRET")
	if strings.TrimSpace(clientSecret) == "" {
		return GitHubAuthConfig{}, errors.New("AUTH_GITHUB_CLIENT_SECRET is required in github mode")
	}
	stateSecret := os.Getenv("AUTH_STATE_SECRET")
	if len(stateSecret) < MinAuthStateSecretBytes {
		return GitHubAuthConfig{}, fmt.Errorf("AUTH_STATE_SECRET is required in github mode and must be at least %d bytes", MinAuthStateSecretBytes)
	}
	redirectURL, err := authURLEnv("AUTH_GITHUB_REDIRECT_URL", "", true)
	if err != nil {
		return GitHubAuthConfig{}, err
	}
	baseURL, err := authURLEnv("AUTH_GITHUB_BASE_URL", DefaultGitHubBaseURL, false)
	if err != nil {
		return GitHubAuthConfig{}, err
	}
	apiURL, err := authURLEnv("AUTH_GITHUB_API_URL", DefaultGitHubAPIURL, false)
	if err != nil {
		return GitHubAuthConfig{}, err
	}
	org := strings.TrimSpace(os.Getenv("AUTH_GITHUB_ALLOWED_ORG"))
	if !domain.IsGitHubLogin(org) {
		return GitHubAuthConfig{}, errors.New("AUTH_GITHUB_ALLOWED_ORG is required in github mode and must be a GitHub organization name")
	}
	admins, err := bootstrapAdminsEnv()
	if err != nil {
		return GitHubAuthConfig{}, err
	}
	return GitHubAuthConfig{
		ClientID:            clientID,
		ClientSecret:        NewSecret(clientSecret),
		RedirectURL:         redirectURL,
		AllowedOrganization: org,
		BootstrapAdmins:     admins,
		StateSecret:         NewSecret(stateSecret),
		BaseURL:             baseURL,
		APIURL:              apiURL,
		HTTPTimeout:         GitHubHTTPTimeout,
	}, nil
}

func bootstrapAdminsEnv() ([]domain.BootstrapAdmin, error) {
	value := strings.TrimSpace(os.Getenv("AUTH_BOOTSTRAP_ADMINS"))
	if value == "" {
		return nil, errors.New("AUTH_BOOTSTRAP_ADMINS is required in github mode")
	}
	parts := strings.Split(value, ",")
	if len(parts) > maxBootstrapAdmins {
		return nil, fmt.Errorf("AUTH_BOOTSTRAP_ADMINS must list at most %d accounts", maxBootstrapAdmins)
	}
	admins := make([]domain.BootstrapAdmin, 0, len(parts))
	for _, part := range parts {
		admin, err := domain.ParseBootstrapAdmin(part)
		if err != nil {
			return nil, errors.New("AUTH_BOOTSTRAP_ADMINS must be a comma-separated list of login:githubID entries")
		}
		admins = append(admins, admin)
	}
	return admins, nil
}

// authURLEnv reads an absolute URL without credentials, query or fragment
// that uses HTTPS unless it targets loopback. A redirect URL (isRedirect)
// must carry a path and is returned verbatim, because GitHub compares it with
// the registered one; a base URL loses its trailing slash.
func authURLEnv(name, fallback string, isRedirect bool) (string, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		value = fallback
	}
	if value == "" {
		return "", fmt.Errorf("%s is required in github mode", name)
	}
	parsed, err := url.Parse(value)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		parsed.Opaque != "" || (isRedirect && parsed.Path == "") {
		// The value is not echoed: a misconfigured URL may carry credentials.
		return "", fmt.Errorf("%s must be an absolute URL without credentials, query, or fragment", name)
	}
	if parsed.Scheme != "https" && (parsed.Scheme != "http" || !loopbackHost(parsed.Hostname())) {
		return "", fmt.Errorf("%s must use HTTPS unless it targets loopback", name)
	}
	if isRedirect {
		return value, nil
	}
	return strings.TrimSuffix(value, "/"), nil
}

// localAuthBind refuses AUTH_MODE=local on a non-loopback HTTP_ADDR: local
// mode makes every request the fixed local administrator, so it must not be
// reachable from another host. ALLOW_INSECURE_LOCAL_AUTH=true overrides the
// refusal; the returned flag tells the caller to warn loudly at startup. An
// empty host (":8080") binds every interface and a hostname is not resolved,
// so both count as non-loopback.
func localAuthBind(auth AuthConfig, httpAddr string) (insecure bool, err error) {
	if auth.Mode != AuthModeLocal {
		return false, nil
	}
	host, _, splitErr := net.SplitHostPort(httpAddr)
	if splitErr == nil && loopbackHost(host) {
		return false, nil
	}
	allow, err := boolEnv("ALLOW_INSECURE_LOCAL_AUTH", false)
	if err != nil {
		return false, err
	}
	if !allow {
		return false, errors.New("AUTH_MODE=local requires a loopback HTTP_ADDR (127.0.0.1, ::1 or localhost); set ALLOW_INSECURE_LOCAL_AUTH=true to override")
	}
	return true, nil
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func notVisibleASCII(r rune) bool { return r <= ' ' || r > '~' }
