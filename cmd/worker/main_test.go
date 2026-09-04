package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/groovy-sky/chrome-control/internal/flows"
	"github.com/groovy-sky/chrome-control/internal/models"
)

// newTestServer builds a *server suitable for exercising handleFlowRun's
// request-parsing, validation, and concurrency-slot behaviour without
// launching a real browser. Tests that would otherwise reach s.worker.RunFlow
// must be arranged so validation fails or the slot is unavailable first.
func newTestServer(maxConcurrent int) *server {
	return &server{
		logger: slog.New(slog.NewTextHandler(nil, &slog.HandlerOptions{Level: slog.LevelError + 100})),
		slots:  make(chan struct{}, maxConcurrent),
	}
}

func decodeRunResult(t *testing.T, body *httptest.ResponseRecorder) flows.RunResult {
	t.Helper()
	var res flows.RunResult
	if err := json.Unmarshal(body.Body.Bytes(), &res); err != nil {
		t.Fatalf("response body is not valid JSON: %v (%s)", err, body.Body.String())
	}
	return res
}

func TestHandleFlowRun_WrongContentType(t *testing.T) {
	s := newTestServer(1)
	req := httptest.NewRequest(http.MethodPost, "/v1/flows/run", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()

	s.handleFlowRun(rec, req)

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnsupportedMediaType)
	}
	res := decodeRunResult(t, rec)
	if res.Error == nil || res.Error.Code != models.CodeInvalidRequest {
		t.Fatalf("error = %+v, want code %s", res.Error, models.CodeInvalidRequest)
	}
}

func TestHandleFlowRun_MalformedJSON(t *testing.T) {
	s := newTestServer(1)
	req := httptest.NewRequest(http.MethodPost, "/v1/flows/run", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	s.handleFlowRun(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	res := decodeRunResult(t, rec)
	if res.Error == nil || res.Error.Code != models.CodeInvalidRequest {
		t.Fatalf("error = %+v, want code %s", res.Error, models.CodeInvalidRequest)
	}
}

func TestHandleFlowRun_UnknownField(t *testing.T) {
	s := newTestServer(1)
	body := `{"flow":{"version":1,"name":"f","steps":[]},"bogus_field":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/flows/run", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	s.handleFlowRun(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	res := decodeRunResult(t, rec)
	if res.Error == nil || res.Error.Code != models.CodeInvalidRequest {
		t.Fatalf("error = %+v, want code %s", res.Error, models.CodeInvalidRequest)
	}
}

func TestHandleFlowRun_ValidationFailure(t *testing.T) {
	s := newTestServer(1)
	// A flow with no steps fails validation before a slot is acquired or
	// RunFlow is invoked.
	body := `{"flow":{"version":1,"name":"f","steps":[]}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/flows/run", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	s.handleFlowRun(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	res := decodeRunResult(t, rec)
	if res.Error == nil || res.Error.Code != models.CodeInvalidRequest {
		t.Fatalf("error = %+v, want code %s", res.Error, models.CodeInvalidRequest)
	}
	if len(s.slots) != 0 {
		t.Fatalf("slots in use = %d, want 0 (validation failure must not consume a slot)", len(s.slots))
	}
}

func TestHandleFlowRun_Overloaded(t *testing.T) {
	s := newTestServer(1)
	s.slots <- struct{}{} // occupy the only slot

	body := `{"flow":{"version":1,"name":"f","start_url":"https://example.com","steps":[{"id":"s1","type":"wait_visible","locator":{"strategy":"css","value":"#x"}}]}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/flows/run", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	s.handleFlowRun(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	res := decodeRunResult(t, rec)
	if res.Error == nil || res.Error.Code != models.CodeOverloaded {
		t.Fatalf("error = %+v, want code %s", res.Error, models.CodeOverloaded)
	}
}
