package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tonyperkins/gcp-telemetry-platform/internal/services"
)

// newTestHandlers builds WorkerHandlers with a nil-repo flight service. The
// tested paths either reject before reaching the repo, or push an empty array
// (SaveExternal returns early on len==0), so Firestore is never touched.
func newTestHandlers() *WorkerHandlers {
	flight := services.NewFlightIngestionService(http.DefaultClient, nil, "", "")
	return NewWorkerHandlers(flight, nil)
}

func doPush(t *testing.T, h *WorkerHandlers, auth, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/ingest/flight/push", strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rr := httptest.NewRecorder()
	h.HandleFlightPush(rr, req)
	return rr
}

func TestHandleFlightPush_NotConfigured(t *testing.T) {
	t.Setenv("ENV", "prod")
	t.Setenv("INGEST_PUSH_TOKEN", "") // unset -> endpoint disabled

	rr := doPush(t, newTestHandlers(), "Bearer anything", "[]")
	if rr.Code != http.StatusNotImplemented {
		t.Fatalf("got %d, want %d", rr.Code, http.StatusNotImplemented)
	}
}

func TestHandleFlightPush_Unauthorized(t *testing.T) {
	t.Setenv("ENV", "prod")
	t.Setenv("INGEST_PUSH_TOKEN", "secret-token")

	cases := map[string]string{
		"no header":     "",
		"wrong token":   "Bearer nope",
		"missing prefix": "secret-token",
	}
	for name, auth := range cases {
		t.Run(name, func(t *testing.T) {
			rr := doPush(t, newTestHandlers(), auth, "[]")
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("got %d, want %d", rr.Code, http.StatusUnauthorized)
			}
		})
	}
}

func TestHandleFlightPush_BadJSON(t *testing.T) {
	t.Setenv("ENV", "prod")
	t.Setenv("INGEST_PUSH_TOKEN", "secret-token")

	rr := doPush(t, newTestHandlers(), "Bearer secret-token", "{not valid json")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want %d", rr.Code, http.StatusBadRequest)
	}
}

func TestHandleFlightPush_EmptyArrayOK(t *testing.T) {
	t.Setenv("ENV", "prod")
	t.Setenv("INGEST_PUSH_TOKEN", "secret-token")

	rr := doPush(t, newTestHandlers(), "Bearer secret-token", "[]")
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want %d (body: %s)", rr.Code, http.StatusOK, rr.Body.String())
	}
	if got := strings.TrimSpace(rr.Body.String()); got != `{"ingested":0}` {
		t.Fatalf("body = %q, want %q", got, `{"ingested":0}`)
	}
}

func TestHandleFlightPush_DevBypass(t *testing.T) {
	t.Setenv("ENV", "dev") // dev skips auth entirely
	t.Setenv("INGEST_PUSH_TOKEN", "")

	rr := doPush(t, newTestHandlers(), "", "[]")
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want %d", rr.Code, http.StatusOK)
	}
}
