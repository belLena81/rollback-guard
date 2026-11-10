package httpserver

import (
	"net/http"
	"time"
)

// SessionCookieName names the browser carrier of the session token. It has
// no __Host- prefix because SESSION_COOKIE_SECURE may be false for plain-HTTP
// local development, and browsers reject __Host- cookies without Secure.
const SessionCookieName = "rollback_guard_session"

// NewSessionCookie returns the cookie that carries rawToken until expiresAt:
// HttpOnly, SameSite=Strict, Path=/, and Secure unless secure is false.
func NewSessionCookie(rawToken string, expiresAt time.Time, secure bool) *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookieName,
		Value:    rawToken,
		Path:     "/",
		Expires:  expiresAt.UTC(),
		Secure:   secure,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	}
}

// ClearSessionCookie returns a cookie that makes the browser drop the
// session cookie (logout). Its attributes match NewSessionCookie so the
// browser replaces the same cookie.
func ClearSessionCookie(secure bool) *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0).UTC(),
		MaxAge:   -1,
		Secure:   secure,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	}
}
