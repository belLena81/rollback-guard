package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWriteJSONReportsEncodeFailureAsInternalError(t *testing.T) {
	recorder := httptest.NewRecorder()

	writeJSON(recorder, http.StatusOK, map[string]any{"status": "ok", "unencodable": make(chan int)})

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q, want application/json", got)
	}
	var body errorDTO
	decoder := json.NewDecoder(recorder.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		t.Fatalf("decode error body %q: %v", recorder.Body.String(), err)
	}
	if body.Error != "internal_error" || body.Message != "internal server error" {
		t.Fatalf("error body = %+v, want the internal_error contract body", body)
	}
}

func TestWriteJSONWritesStatusAndBody(t *testing.T) {
	recorder := httptest.NewRecorder()

	writeJSON(recorder, http.StatusAccepted, map[string]string{"status": "ok"})

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusAccepted)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q, want application/json", got)
	}
	if got, want := recorder.Body.String(), "{\"status\":\"ok\"}\n"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}
