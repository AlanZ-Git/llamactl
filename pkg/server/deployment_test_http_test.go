package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDeploymentTestRoutesRejectInvalidBeforeWork(t *testing.T) {
	router, _, inst := p3HTTPFixture(t)
	before := inst.ControlSnapshot()
	for _, body := range []string{`{}`, `{"measured_generations":4}`, `{"prompt":"private"}`, `{"warmup":null}`, `{"model":"alpha","model":"alpha"}`} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, admittedRequest("POST", "/api/v1/instances/protocol/tests", body))
		if w.Code != 422 {
			t.Fatalf("invalid test must be rejected with 422: %d %s", w.Code, w.Body.String())
		}
	}
	after := inst.ControlSnapshot()
	if before.Revision != after.Revision || after.Running || after.ActiveOperation != "" {
		t.Fatal("invalid request changed instance")
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, admittedRequest("GET", "/api/v1/instances/protocol/tests/old-boot-run", ""))
	if w.Code != 404 || !strings.Contains(w.Body.String(), "test_unknown_not_resumable") {
		t.Fatalf("missing run must not imply never executed: %d %s", w.Code, w.Body.String())
	}
}
