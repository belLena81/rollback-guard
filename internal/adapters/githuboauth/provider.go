// Package githuboauth is the GitHub implementation of
// ports.IdentityProvider: OAuth authorization code with PKCE (S256), then
// GET /user and GET /user/orgs with the access token, which is discarded
// before Exchange returns. oauth2 types never leave the package.
package githuboauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/belLena81/rollback-guard/internal/ports"
)

// Endpoints and request shape (docs/phase7-auth.md, "GitHub login").
const (
	DefaultBaseURL = "https://github.com"
	DefaultAPIURL  = "https://api.github.com"
	authorizePath  = "/login/oauth/authorize"
	tokenPath      = "/login/oauth/access_token" //nolint:gosec // G101: a URL path, not a credential
	userPath       = "/user"
	orgsPath       = "/user/orgs"
	// orgsPerPage is GitHub's maximum page size.
	orgsPerPage = 100
	// MaxOrganizationPages bounds the /user/orgs pages read per login
	// (500 organizations). Organizations past the bound are not seen, which
	// can only deny a login, never grant one.
	MaxOrganizationPages = 5
	// maxResponseBytes bounds every REST response body read.
	maxResponseBytes = 1 << 20
	githubAPIVersion = "2022-11-28"
	githubMediaType  = "application/vnd.github+json"
)

// Scopes requested: organization membership (including private) and the
// primary email.
var Scopes = []string{"read:org", "user:email"}

// rejectedCodeErrors are token-endpoint error codes that mean the code or
// PKCE verifier was refused, so the user must start over.
var rejectedCodeErrors = map[string]struct{}{
	"bad_verification_code": {},
	"invalid_grant":         {},
}

// Config configures the provider. URLs are validated for scheme policy by
// internal/platform/config; New only requires absolute http(s) URLs.
type Config struct {
	ClientID     string
	ClientSecret string //nolint:gosec // G117: held in memory only; Config.Format redacts it
	RedirectURL  string
	BaseURL      string
	APIURL       string
	// Timeout bounds each HTTP request to GitHub.
	Timeout time.Duration
}

// Format prints c for every verb with the client secret redacted.
func (c Config) Format(f fmt.State, _ rune) {
	//nolint:errcheck // fmt.Formatter has no error channel
	_, _ = fmt.Fprintf(f, "githuboauth.Config{ClientID:%s ClientSecret:[REDACTED] RedirectURL:%s BaseURL:%s APIURL:%s Timeout:%s}",
		c.ClientID, c.RedirectURL, c.BaseURL, c.APIURL, c.Timeout)
}

// Provider is a ports.IdentityProvider for GitHub.
type Provider struct {
	oauth  oauth2.Config
	api    *url.URL
	client *http.Client
}

var _ ports.IdentityProvider = (*Provider)(nil)

func New(config Config) (*Provider, error) {
	if config.ClientID == "" || config.ClientSecret == "" {
		return nil, errors.New("github oauth client id and secret are required")
	}
	if config.Timeout <= 0 {
		return nil, errors.New("github oauth timeout must be positive")
	}
	base, err := absoluteURL(config.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("github base url: %w", err)
	}
	api, err := absoluteURL(config.APIURL)
	if err != nil {
		return nil, fmt.Errorf("github api url: %w", err)
	}
	if _, err := absoluteURL(config.RedirectURL); err != nil {
		return nil, fmt.Errorf("github redirect url: %w", err)
	}
	return &Provider{
		oauth: oauth2.Config{
			ClientID:     config.ClientID,
			ClientSecret: config.ClientSecret,
			RedirectURL:  config.RedirectURL,
			Scopes:       Scopes,
			Endpoint: oauth2.Endpoint{
				AuthURL:   base.JoinPath(authorizePath).String(),
				TokenURL:  base.JoinPath(tokenPath).String(),
				AuthStyle: oauth2.AuthStyleInParams,
			},
		},
		api: api,
		client: &http.Client{
			Timeout: config.Timeout,
			// Never follow a redirect with the access token attached.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func absoluteURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("must be an absolute http(s) URL without credentials, query or fragment")
	}
	return parsed, nil
}

// AuthorizationURL returns GitHub's authorize URL for state and the S256
// codeChallenge.
func (p *Provider) AuthorizationURL(state, codeChallenge string) string {
	return p.oauth.AuthCodeURL(state,
		oauth2.SetAuthURLParam("code_challenge", codeChallenge),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
	)
}

// Exchange redeems code with codeVerifier and reads the user and their
// organizations. Errors wrap ports.ErrAuthorizationCodeRejected or
// ports.ErrIdentityProviderFailed and carry only a fixed stage name: never
// the provider's body, the code, or the token.
func (p *Provider) Exchange(ctx context.Context, code, codeVerifier string) (ports.GitHubIdentity, error) {
	token, err := p.oauth.Exchange(context.WithValue(ctx, oauth2.HTTPClient, p.client), code, oauth2.VerifierOption(codeVerifier))
	if err != nil {
		return ports.GitHubIdentity{}, tokenError(err)
	}
	if !token.Valid() || (token.TokenType != "" && !strings.EqualFold(token.TokenType, "bearer")) {
		return ports.GitHubIdentity{}, failed("token")
	}
	// The access token lives only in this frame and the requests below.
	accessToken := token.AccessToken
	var user struct {
		ID    int64   `json:"id"`
		Login string  `json:"login"`
		Email *string `json:"email"`
	}
	if err := p.get(ctx, accessToken, userPath, nil, &user); err != nil {
		return ports.GitHubIdentity{}, err
	}
	organizations, err := p.organizations(ctx, accessToken)
	if err != nil {
		return ports.GitHubIdentity{}, err
	}
	identity := ports.GitHubIdentity{GitHubID: user.ID, Login: user.Login, Organizations: organizations}
	if user.Email != nil {
		identity.Email = *user.Email
	}
	return identity, nil
}

func (p *Provider) organizations(ctx context.Context, accessToken string) ([]string, error) {
	var organizations []string
	for page := 1; page <= MaxOrganizationPages; page++ {
		var batch []struct {
			Login string `json:"login"`
		}
		query := url.Values{"per_page": {strconv.Itoa(orgsPerPage)}, "page": {strconv.Itoa(page)}}
		if err := p.get(ctx, accessToken, orgsPath, query, &batch); err != nil {
			return nil, err
		}
		for _, org := range batch {
			organizations = append(organizations, org.Login)
		}
		if len(batch) < orgsPerPage {
			break
		}
	}
	return organizations, nil
}

// get reads one bounded JSON response from the REST API.
func (p *Provider) get(ctx context.Context, accessToken, path string, query url.Values, into any) error {
	stage := path
	endpoint := p.api.JoinPath(path)
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return failed(stage)
	}
	request.Header.Set("Accept", githubMediaType)
	request.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	request.Header.Set("Authorization", "Bearer "+accessToken)
	response, err := p.client.Do(request) //nolint:gosec // G704: host is the configured, validated API URL
	if err != nil {
		return failed(stage)
	}
	defer func() {
		//nolint:errcheck // drain for connection reuse; the result is already decided
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))
		//nolint:errcheck // closing a fully read body reports nothing actionable
		_ = response.Body.Close()
	}()
	if response.StatusCode != http.StatusOK {
		return failed(stage)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(body) > maxResponseBytes {
		return failed(stage)
	}
	if err := json.Unmarshal(body, into); err != nil {
		return failed(stage)
	}
	return nil
}

// tokenError classifies a token-endpoint failure without keeping its text:
// oauth2.RetrieveError carries the provider's response body.
func tokenError(err error) error {
	var retrieve *oauth2.RetrieveError
	if errors.As(err, &retrieve) {
		if _, ok := rejectedCodeErrors[retrieve.ErrorCode]; ok {
			return fmt.Errorf("github token: %w", ports.ErrAuthorizationCodeRejected)
		}
	}
	return failed("token")
}

func failed(stage string) error {
	return fmt.Errorf("github %s: %w", stage, ports.ErrIdentityProviderFailed)
}
