package instance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestN1SSEIdleDoesNotReleaseAndClosingWaitsForExit(t *testing.T) {
	entered := make(chan struct{})
	exit := make(chan struct{})
	backendDone := make(chan struct{})
	inst, _ := protocolFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {}\n\n"))
		w.(http.Flusher).Flush()
		close(entered)
		select {
		case <-exit:
		case <-r.Context().Done():
		}
		close(backendDone)
	}))
	token := publishSyntheticReady(t, inst, "alpha")
	s := inst.ControlSnapshot()
	reservation, err := inst.ReserveN1("sse", "scope", "service_use", s.BootID, s.Revision, time.Now().Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err = reservation.ActivateService(RecipeRef{ID: "r", Version: 1}, "alpha", RequestContract{TemplateVersion: "v1", Parameters: map[string]json.RawMessage{"max_tokens": json.RawMessage(`4`)}}, 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"alpha","stream":true,"messages":[{"role":"user","content":"synthetic"}]}`))
	request.Header.Set("X-Service-Use", "sse")
	request.Header.Set("X-Ready-Token", token)
	request.Header.Set("X-Prompt-Template-Version", "v1")
	request.Header.Set("X-Prompt-Digest", reservation.contract.PromptDigest())
	request = WithServiceScope(request, "scope")
	done := make(chan error, 1)
	go func() { done <- inst.ServeHTTP(httptest.NewRecorder(), request) }()
	<-entered
	time.Sleep(40 * time.Millisecond)
	if reservation.Expired() {
		t.Fatal("idle timer expired live SSE")
	}
	if reservation.ActiveRequests() != 1 {
		t.Fatal("SSE not counted")
	}
	if err = reservation.CloseConfirmed(); err == nil {
		t.Fatal("released with active SSE")
	}
	reservation.BeginClosing()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("closing did not cancel forwarding")
	}
	<-backendDone
	close(exit)
	if err = reservation.CloseConfirmed(); err != nil {
		t.Fatal(err)
	}
}
func TestN1RecoveryCannotAdoptRunningReservation(t *testing.T) {
	inst, _ := protocolFixture(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	publishSyntheticReady(t, inst, "alpha")
	s := inst.ControlSnapshot()
	r, err := inst.ReserveN1("r", "s", "evaluation", s.BootID, s.Revision, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	defer r.CloseConfirmed()
	r.BeginClosing()
	if err = r.VerifyStoppedRecovery(context.Background()); err == nil {
		t.Fatal("recovery adopted running process")
	}
}
