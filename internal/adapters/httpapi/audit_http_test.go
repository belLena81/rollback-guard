package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// seedAudit makes the administrator write n audit events, one second apart so
// the newest-first order is unambiguous.
func (h *roleHarness) seedAudit(t *testing.T, n int) {
	t.Helper()
	for range n {
		h.clock.Advance(time.Second)
		response := h.do(t, adminID, http.MethodPost, fmt.Sprintf("/v1/users/github:%d/sessions:revoke", targetID), "")
		if response.Code != http.StatusOK {
			t.Fatalf("seed audit = %d %s", response.Code, response.Body)
		}
	}
}

func (h *roleHarness) auditPage(t *testing.T, query string) auditPageDTO {
	t.Helper()
	response := h.do(t, adminID, http.MethodGet, "/v1/auth/audit"+query, "")
	if response.Code != http.StatusOK {
		t.Fatalf("audit%s = %d %s", query, response.Code, response.Body)
	}
	var page auditPageDTO
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	return page
}

func TestAuditRoleMatrix(t *testing.T) {
	h := newRoleHarness(t)
	h.seedAudit(t, 1)
	tests := []struct {
		name   string
		id     int64
		status int
	}{
		{"viewer is forbidden", viewerID, http.StatusForbidden},
		{"operator is forbidden", operatorID, http.StatusForbidden},
		{"administrator reads", adminID, http.StatusOK},
		{"no token is unauthenticated", 999, http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := h.do(t, tt.id, http.MethodGet, "/v1/auth/audit", "")
			if response.Code != tt.status {
				t.Fatalf("status = %d, want %d (%s)", response.Code, tt.status, response.Body)
			}
			if tt.status != http.StatusOK && strings.Contains(response.Body.String(), "sessions_revoked") {
				t.Fatalf("a refused request leaked audit data: %s", response.Body)
			}
		})
	}
}

func TestAuditPaginatesNewestFirstWithoutGapsOrRepeats(t *testing.T) {
	h := newRoleHarness(t)
	h.seedAudit(t, 5)

	var seen []string
	query := "?limit=2"
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("pagination did not terminate")
		}
		page := h.auditPage(t, query)
		for _, e := range page.Events {
			seen = append(seen, e.OccurredAt)
		}
		if page.NextCursor == "" {
			break
		}
		if len(page.Events) != 2 {
			t.Fatalf("a non-final page held %d events, want 2", len(page.Events))
		}
		query = "?limit=2&cursor=" + url.QueryEscape(page.NextCursor)
	}
	if len(seen) != 5 {
		t.Fatalf("read %d events across pages, want 5: %v", len(seen), seen)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i-1] <= seen[i] {
			t.Fatalf("events not strictly newest first: %v", seen)
		}
	}
}

func TestAuditExactPageBoundaryHasNoNextCursor(t *testing.T) {
	h := newRoleHarness(t)
	h.seedAudit(t, 2)
	page := h.auditPage(t, "?limit=2")
	if len(page.Events) != 2 || page.NextCursor != "" {
		t.Fatalf("page = %d events, cursor %q; want 2 events and no cursor", len(page.Events), page.NextCursor)
	}
}

func TestAuditDefaultLimitAndFields(t *testing.T) {
	h := newRoleHarness(t)
	h.seedAudit(t, 1)
	response := h.do(t, adminID, http.MethodGet, "/v1/auth/audit", "")
	var raw struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &raw); err != nil || len(raw.Events) != 1 {
		t.Fatalf("body = %s", response.Body)
	}
	allowed := map[string]bool{"id": true, "occurredAt": true, "actorId": true, "action": true, "targetUserId": true, "fromRole": true, "toRole": true, "sessionCount": true}
	for field := range raw.Events[0] {
		if !allowed[field] {
			t.Errorf("response carries unexpected field %q", field)
		}
	}
	if raw.Events[0]["action"] != "sessions_revoked" || raw.Events[0]["actorId"] != fmt.Sprintf("github:%d", adminID) {
		t.Errorf("event = %v", raw.Events[0])
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", response.Header().Get("Cache-Control"))
	}
}

func TestAuditRejectsBadQueries(t *testing.T) {
	h := newRoleHarness(t)
	h.seedAudit(t, 1)
	tests := []struct {
		name  string
		query string
	}{
		{"zero limit", "?limit=0"},
		{"negative limit", "?limit=-1"},
		{"limit above max", "?limit=201"},
		{"non-numeric limit", "?limit=abc"},
		{"garbage cursor", "?cursor=%21%21%21"},
		{"well-formed base64 of the wrong shape", "?cursor=" + url.QueryEscape("bm9wZQ")},
		{"cursor id with a NUL byte", "?cursor=" + base64.RawURLEncoding.EncodeToString([]byte("1|a\x00b"))},
		{"cursor id with invalid UTF-8", "?cursor=" + base64.RawURLEncoding.EncodeToString([]byte("1|a\xffb"))},
		{"oversized cursor", "?cursor=" + strings.Repeat("a", 300)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := h.do(t, adminID, http.MethodGet, "/v1/auth/audit"+tt.query, "")
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", response.Code, response.Body)
			}
		})
	}
	if got := h.auditPage(t, "?limit=200"); len(got.Events) != 1 {
		t.Fatalf("limit 200 read %d events, want 1", len(got.Events))
	}
}
