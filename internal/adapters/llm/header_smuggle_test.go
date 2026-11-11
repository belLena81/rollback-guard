package llm

import (
	"net/http"
	"slices"
	"testing"
)

type captureTransport struct{ got http.Header }

func (c *captureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.got = r.Header.Clone()
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: r}, nil
}

func TestHeaderAllowListRejectsSmuggledAuthorization(t *testing.T) {
	const key = "real-key"
	tests := []struct {
		name   string
		header http.Header
		want   []string
	}{
		{"exact bearer kept", http.Header{"Authorization": {"Bearer " + key}}, []string{"Bearer " + key}},
		{"second value rejected", http.Header{"Authorization": {"Bearer " + key, "Bearer evil"}}, nil},
		{"lowercase key beside valid one dropped", http.Header{"Authorization": {"Bearer " + key}, "authorization": {"Bearer evil"}}, []string{"Bearer " + key}},
		{"wrong bearer dropped", http.Header{"Authorization": {"Bearer other"}}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next := &captureTransport{}
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://example.invalid/", http.NoBody)
			if err != nil {
				t.Fatal(err)
			}
			request.Header = tt.header
			if _, err := (&headerAllowListTransport{apiKey: key, next: next}).RoundTrip(request); err != nil { //nolint:bodyclose // the stub response has no body
				t.Fatal(err)
			}
			if got := next.got["Authorization"]; !slices.Equal(got, tt.want) {
				t.Fatalf("Authorization = %v, want %v", got, tt.want)
			}
			for name := range next.got {
				if name != http.CanonicalHeaderKey(name) {
					t.Fatalf("non-canonical header key %q reached the provider", name)
				}
			}
		})
	}
}
