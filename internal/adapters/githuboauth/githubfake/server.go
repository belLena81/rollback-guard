// Package githubfake is a scriptable in-process stand-in for the GitHub
// OAuth and REST endpoints the login uses: authorize, token exchange (with
// PKCE S256 verification), GET /user and GET /user/orgs (paginated). It
// serves base and API URLs from one httptest server. It is for tests and the
// local end-to-end stack only.
package githubfake

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Paths served, matching github.com and api.github.com.
const (
	AuthorizePath = "/login/oauth/authorize"
	TokenPath     = "/login/oauth/access_token" //nolint:gosec // G101: a URL path, not a credential
	UserPath      = "/user"
	OrgsPath      = "/user/orgs"
)

// FailureBodyCanary is in every scripted failure body, so tests can assert
// that provider bodies never reach clients or logs.
const FailureBodyCanary = "fake-github-failure-body-canary"

// defaultOrgsPerPage and maxOrgsPerPage mirror GitHub's pagination.
const (
	defaultOrgsPerPage = 30
	maxOrgsPerPage     = 100
)

// User is a scripted GitHub account.
type User struct {
	ID            int64
	Login         string
	Email         string
	Organizations []string
}

type grant struct {
	login       string
	challenge   string
	redirectURI string
}

// Server is a fake GitHub. All methods are safe for concurrent use.
type Server struct {
	server       *httptest.Server
	clientID     string
	clientSecret string

	mu            sync.Mutex
	users         map[string]User
	signedIn      string
	codes         map[string]grant
	tokens        map[string]string
	issued        []string
	tokenStatus   int
	apiStatus     int
	delay         time.Duration
	counter       int
	tokenRequests int
	apiRequests   int
	lastAuthorize url.Values
}

// New starts a fake GitHub that accepts one OAuth client.
func New(clientID, clientSecret string) *Server {
	s := newServer(clientID, clientSecret)
	s.server = httptest.NewServer(s.handler())
	return s
}

// Listen is New on a chosen loopback address (for example "127.0.0.1:9100";
// port 0 picks a free one). A non-loopback address is refused, so the fake
// can never be exposed to a network.
func Listen(addr, clientID, clientSecret string) (*Server, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("githubfake: address: %w", err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("githubfake: %q is not a loopback IP address", host)
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("githubfake: listen: %w", err)
	}
	s := newServer(clientID, clientSecret)
	s.server = httptest.NewUnstartedServer(s.handler())
	s.server.Listener = listener
	s.server.Start()
	return s, nil
}

func newServer(clientID, clientSecret string) *Server {
	return &Server{
		clientID:     clientID,
		clientSecret: clientSecret,
		users:        map[string]User{},
		codes:        map[string]grant{},
		tokens:       map[string]string{},
	}
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+AuthorizePath, s.authorize)
	mux.HandleFunc("POST "+TokenPath, s.token)
	mux.HandleFunc("GET "+UserPath, s.user)
	mux.HandleFunc("GET "+OrgsPath, s.orgs)
	return s.delayed(mux)
}

// URL is both the OAuth base URL and the REST API URL.
func (s *Server) URL() string { return s.server.URL }

// Close stops the server.
func (s *Server) Close() { s.server.Close() }

// AddUser adds or replaces a user, keyed by login.
func (s *Server) AddUser(user User) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users[strings.ToLower(user.Login)] = user
}

// SignInAs makes the authorize endpoint approve as login.
func (s *Server) SignInAs(login string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.signedIn = strings.ToLower(login)
}

// IssueCode returns an authorization code for login bound to the S256
// challenge and redirect URI, as if the user approved the app.
func (s *Server) IssueCode(login, challenge, redirectURI string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.counter++
	code := fmt.Sprintf("fake-code-%d", s.counter)
	s.codes[code] = grant{login: strings.ToLower(login), challenge: challenge, redirectURI: redirectURI}
	return code
}

// FailToken makes the token endpoint answer status (0 restores success).
func (s *Server) FailToken(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokenStatus = status
}

// FailAPI makes /user and /user/orgs answer status (0 restores success).
func (s *Server) FailAPI(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.apiStatus = status
}

// SetDelay delays every response by d, or until the request ends.
func (s *Server) SetDelay(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delay = d
}

// TokenRequests counts token endpoint requests.
func (s *Server) TokenRequests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokenRequests
}

// APIRequests counts /user and /user/orgs requests.
func (s *Server) APIRequests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.apiRequests
}

// IssuedTokens returns every access token handed out.
func (s *Server) IssuedTokens() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.issued...)
}

// LastAuthorizeQuery returns the query of the latest authorize request.
func (s *Server) LastAuthorizeQuery() url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastAuthorize
}

func (s *Server) delayed(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		delay := s.delay
		s.mu.Unlock()
		if delay > 0 {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-r.Context().Done():
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// authorize approves as the signed-in user and redirects back with a code.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	s.mu.Lock()
	s.lastAuthorize = query
	login := s.signedIn
	if hint := strings.ToLower(query.Get("login")); hint != "" {
		login = hint // GitHub's login parameter suggests the account to use
	}
	_, known := s.users[login]
	s.mu.Unlock()
	if query.Get("client_id") != s.clientID || query.Get("code_challenge_method") != "S256" ||
		query.Get("code_challenge") == "" || query.Get("state") == "" || !known {
		http.Error(w, "authorize rejected", http.StatusBadRequest)
		return
	}
	redirect, err := url.Parse(query.Get("redirect_uri"))
	if err != nil || redirect.Scheme == "" || redirect.Host == "" {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	code := s.IssueCode(login, query.Get("code_challenge"), query.Get("redirect_uri"))
	values := redirect.Query()
	values.Set("code", code)
	values.Set("state", query.Get("state"))
	redirect.RawQuery = values.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound)
}

// token redeems a code. Like GitHub, a bad code is a 200 with an error field.
func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokenRequests++
	if s.tokenStatus != 0 {
		http.Error(w, FailureBodyCanary, s.tokenStatus)
		return
	}
	if err := r.ParseForm(); err != nil {
		writeJSON(w, map[string]string{"error": "invalid_request"})
		return
	}
	if r.PostForm.Get("client_id") != s.clientID || r.PostForm.Get("client_secret") != s.clientSecret {
		writeJSON(w, map[string]string{"error": "incorrect_client_credentials", "error_description": FailureBodyCanary})
		return
	}
	code := r.PostForm.Get("code")
	grant, ok := s.codes[code]
	delete(s.codes, code) // single use, even when the verifier is wrong
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != grant.challenge ||
		(grant.redirectURI != "" && r.PostForm.Get("redirect_uri") != grant.redirectURI) {
		writeJSON(w, map[string]string{"error": "bad_verification_code", "error_description": FailureBodyCanary})
		return
	}
	s.counter++
	token := fmt.Sprintf("gho_fake_access_token_%d", s.counter)
	s.tokens[token] = grant.login
	s.issued = append(s.issued, token)
	writeJSON(w, map[string]string{"access_token": token, "token_type": "bearer", "scope": "read:org,user:email"})
}

// authenticated returns the user behind the request's bearer token.
func (s *Server) authenticated(w http.ResponseWriter, r *http.Request) (User, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.apiRequests++
	if s.apiStatus != 0 {
		http.Error(w, FailureBodyCanary, s.apiStatus)
		return User{}, false
	}
	scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	login, ok := s.tokens[token]
	if !ok || (!strings.EqualFold(scheme, "Bearer") && !strings.EqualFold(scheme, "token")) {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
		return User{}, false
	}
	user, ok := s.users[login]
	if !ok {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return User{}, false
	}
	return user, true
}

func (s *Server) user(w http.ResponseWriter, r *http.Request) {
	user, ok := s.authenticated(w, r)
	if !ok {
		return
	}
	body := map[string]any{"id": user.ID, "login": user.Login, "email": nil}
	if user.Email != "" {
		body["email"] = user.Email
	}
	writeJSON(w, body)
}

func (s *Server) orgs(w http.ResponseWriter, r *http.Request) {
	user, ok := s.authenticated(w, r)
	if !ok {
		return
	}
	perPage := boundedQueryInt(r, "per_page", defaultOrgsPerPage, maxOrgsPerPage)
	page := boundedQueryInt(r, "page", 1, len(user.Organizations)+1)
	start := min((page-1)*perPage, len(user.Organizations))
	end := min(start+perPage, len(user.Organizations))
	orgs := make([]map[string]string, 0, end-start)
	for _, login := range user.Organizations[start:end] {
		orgs = append(orgs, map[string]string{"login": login})
	}
	writeJSON(w, orgs)
}

func boundedQueryInt(r *http.Request, name string, fallback, upper int) int {
	value, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil || value < 1 {
		return fallback
	}
	return min(value, max(upper, 1))
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		return
	}
}
