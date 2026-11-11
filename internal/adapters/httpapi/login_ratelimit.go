package httpapi

import (
	"math"
	"net/http"
	"strconv"

	"github.com/belLena81/rollback-guard/internal/platform/httpserver"
)

// messageRateLimited is the one body for every rate-limited request: it does
// not say which client, bucket or limit triggered it.
const messageRateLimited = "too many requests; retry later"

// WithLoginRateLimit limits GET /v1/auth/github/login and /callback per
// client address (RemoteAddr only; forwarding headers are not trusted, so a
// reverse proxy deployment must rate-limit at the proxy). No other route is
// limited.
func WithLoginRateLimit(limiter *httpserver.RateLimiter) Option {
	return func(s *Server) { s.loginLimiter = limiter }
}

func (s *Server) limitLogin(next http.HandlerFunc) http.HandlerFunc {
	if s.loginLimiter == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		allowed, retryAfter := s.loginLimiter.Allow(httpserver.ClientKey(r.RemoteAddr))
		if allowed {
			next(w, r)
			return
		}
		seconds := max(int(math.Ceil(retryAfter.Seconds())), 1)
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		w.Header().Set("Cache-Control", "no-store")
		s.writeJSON(w, r, http.StatusTooManyRequests, errorBody(r, codeRateLimited, messageRateLimited))
	}
}
