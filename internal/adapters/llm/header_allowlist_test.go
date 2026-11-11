package llm

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

const headerTestResponse = `{
	"id":"completion-1","object":"chat.completion","created":1,"model":"served-model",
	"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"{\"hypothesis\":\"rollout regression\",\"confidence\":0.92,\"evidence_ids\":[\"deployment\"],\"recommendation\":\"rollback\"}","refusal":null}}],
	"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}
}`

func receivedHeaders(t *testing.T, apiKey string) http.Header {
	t.Helper()
	t.Setenv("OPENAI_CUSTOM_HEADERS", "X-Tenant: ambient-tenant\nX-Exfil: ambient-secret\nAuthorization: Bearer ambient-custom")
	t.Setenv("OPENAI_API_KEY", "ambient-key")
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(w, headerTestResponse); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	reasoner, err := NewOpenAICompatibleReasoner(OpenAICompatibleConfig{
		BaseURL: server.URL + "/v1", APIKey: apiKey, Model: "local-model", MaxOutputTokens: 512, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reasoner.Reason(context.Background(), reasoningRequest(t)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "ambient") {
		t.Fatalf("header value logged: %s", logs.String())
	}
	return got
}

func TestCustomHeadersFromEnvironmentNeverReachTheProvider(t *testing.T) {
	allowed := []string{"Accept", "Accept-Encoding", "Authorization", "Content-Length", "Content-Type", "User-Agent"}
	for name, apiKey := range map[string]string{"with api key": "configured-key", "without api key": ""} {
		t.Run(name, func(t *testing.T) {
			headers := receivedHeaders(t, apiKey)
			for header := range headers {
				if !slices.Contains(allowed, header) {
					t.Errorf("non-allow-listed header %q reached the provider", header)
				}
			}
			for header, values := range headers {
				if slices.ContainsFunc(values, func(v string) bool { return strings.Contains(v, "ambient") }) {
					t.Errorf("ambient value reached the provider in %q", header)
				}
			}
			authorization := headers.Get("Authorization")
			switch {
			case apiKey != "" && authorization != "Bearer "+apiKey:
				t.Errorf("Authorization = %q, want the configured key", authorization)
			case apiKey == "" && authorization != "":
				t.Errorf("Authorization = %q, want none", authorization)
			}
			if headers.Get("Content-Type") == "" {
				t.Error("Content-Type was dropped")
			}
		})
	}
}
