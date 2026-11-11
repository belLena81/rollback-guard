package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/belLena81/rollback-guard/internal/domain"
)

// The session middleware (internal/platform/httpserver) cannot share the
// error constants of this package, so its fixed responses are pinned here
// against the REST error mapping: a client sees one envelope whichever layer
// rejects the request (docs/refactoring-plan.md, R9 item 6).
func TestSessionMiddlewareErrorsMatchTheRESTErrorMapping(t *testing.T) {
	h := newRoleHarness(t)

	t.Run("unauthenticated", func(t *testing.T) {
		recorder := h.do(t, 0, http.MethodGet, "/v1/me", "")
		wantStatus, wantCode, wantMessage := errorResponse(domain.ErrUnauthenticated)
		got := decodeErrorBody(t, recorder)
		if recorder.Code != wantStatus || got.Error != wantCode || got.Message != wantMessage {
			t.Fatalf("middleware 401 = %d %q %q, REST mapping = %d %q %q",
				recorder.Code, got.Error, got.Message, wantStatus, wantCode, wantMessage)
		}
		if recorder.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Fatalf("WWW-Authenticate = %q", recorder.Header().Get("WWW-Authenticate"))
		}
	})

	t.Run("cross-site cookie write", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/v1/auth/logout", strings.NewReader(""))
		request.Header.Set("Origin", "https://attacker.example")
		request.AddCookie(&http.Cookie{Name: "rollback_guard_session", Value: "x"})
		recorder := httptest.NewRecorder()
		h.handler.ServeHTTP(recorder, request)
		wantStatus, wantCode, _ := errorResponse(&domain.ForbiddenError{})
		got := decodeErrorBody(t, recorder)
		if recorder.Code != wantStatus || got.Error != wantCode {
			t.Fatalf("middleware CSRF rejection = %d %q, REST mapping = %d %q", recorder.Code, got.Error, wantStatus, wantCode)
		}
	})
}
