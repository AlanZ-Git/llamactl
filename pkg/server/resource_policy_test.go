package server

import (
	"encoding/json"
	"io"
	"llamactl/pkg/config"
	"llamactl/pkg/instance"
	"llamactl/pkg/resourcepolicy"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func ownerPut(t *testing.T, router http.Handler, allow bool, s resourcepolicy.Snapshot, status int) resourcepolicy.Snapshot {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"allow_ai": allow, "boot_id": s.BootID, "expected_revision": s.Revision})
	w := httptest.NewRecorder()
	router.ServeHTTP(w, admittedRequest("PUT", "/api/v1/resource-policy", string(body)))
	if w.Code != status {
		t.Fatalf("policy PUT: %d %s", w.Code, w.Body.String())
	}
	var next resourcepolicy.Snapshot
	if err := json.Unmarshal(w.Body.Bytes(), &next); err != nil {
		t.Fatal(err)
	}
	return next
}
func ownerGet(t *testing.T, router http.Handler) resourcepolicy.Snapshot {
	t.Helper()
	w := httptest.NewRecorder()
	router.ServeHTTP(w, admittedRequest("GET", "/api/v1/resource-policy", ""))
	if w.Code != 200 {
		t.Fatalf("policy GET: %d %s", w.Code, w.Body.String())
	}
	var s resourcepolicy.Snapshot
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestOwnerPolicyHTTPDuringSSE(t *testing.T) {
	entered, finish := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(finish) })
	router, im, inst, _ := admissionServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" || r.URL.Path == "/models/unload" {
			io.WriteString(w, `{}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		close(entered)
		<-finish
	}))
	s := ownerGet(t, router)
	done := make(chan struct{})
	go func() {
		defer close(done)
		router.ServeHTTP(httptest.NewRecorder(), admittedRequest("POST", "/v1/chat/completions", `{"model":"admitted/synthetic","stream":true}`))
	}()
	awaitAdmission(t, entered)
	s = ownerPut(t, router, false, s, 200)
	if s.AllowAI || s.Release != "unreleased_unknown" {
		t.Fatalf("pause %+v", s)
	}
	payload, _ := json.Marshal(inst.GetOptions())
	for _, a := range []struct {
		method, path, body string
		status             int
	}{
		{"POST", "/v1/chat/completions", `{"model":"admitted/synthetic"}`, 503},
		{"POST", "/llama-cpp/admitted/completion", `{"model":"synthetic"}`, 503},
		{"POST", "/api/v1/instances/admitted/proxy/completion", `{"model":"synthetic"}`, 503},
		{"POST", "/api/v1/instances/admitted/proxy/props", `{}`, 409},
		{"POST", "/api/v1/llama-cpp/admitted/models/synthetic/load", `{}`, 409},
		{"POST", "/api/v1/instances/admitted/start", "", 409},
		{"POST", "/api/v1/instances/admitted/restart", "", 409},
		{"PUT", "/api/v1/instances/admitted", string(payload), 409},
		{"POST", "/api/v1/instances/new", string(payload), 409},
	} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, admittedRequest(a.method, a.path, a.body))
		if w.Code != a.status || !strings.Contains(w.Body.String(), `"ai_paused"`) || !strings.Contains(w.Body.String(), `"retryable":false`) {
			t.Errorf("%s %s: %d %s", a.method, a.path, w.Code, w.Body.String())
		}
	}
	admissionResponse(t, router, "GET", "/api/v1/instances/admitted", "", 200, "")
	admissionResponse(t, router, "GET", "/api/v1/llama-cpp/admitted/models", "", 200, "")
	admissionResponse(t, router, "POST", "/api/v1/instances/admitted/stop", "", 409, "busy")
	once.Do(func() { close(finish) })
	awaitAdmission(t, done)
	admissionResponse(t, router, "POST", "/api/v1/llama-cpp/admitted/models/synthetic/unload", `{"model":"synthetic"}`, 200, "")
	inst.SetStatus(instance.Stopped)
	if _, err := im.StopInstance(inst.Name); err != nil {
		t.Fatal(err)
	}
	if ownerGet(t, router).Release != "unreleased_unknown" {
		t.Fatal("inflight zero falsely reports release")
	}
	s = ownerPut(t, router, true, s, 200)
	if !s.AllowAI || inst.IsRunning() {
		t.Fatal("resume auto-started")
	}
}

func TestOwnerPolicyHTTPAuthAndInput(t *testing.T) {
	for _, required := range []bool{false, true} {
		cfg := config.AppConfig{}
		cfg.Auth.RequireManagementAuth = required
		cfg.Auth.ManagementKeys = []string{"synthetic-management-key"}
		g, err := resourcepolicy.Open("")
		if err != nil {
			t.Fatal(err)
		}
		cfg.ResourcePolicy = g
		router := SetupRouter(NewHandler(nil, nil, cfg, nil, nil))
		initial := ownerGet(t, router)
		valid, _ := json.Marshal(map[string]any{"allow_ai": false, "boot_id": initial.BootID, "expected_revision": initial.Revision})
		for _, key := range []string{"", "synthetic-inference-key"} {
			r := httptest.NewRequest("PUT", "/api/v1/resource-policy", strings.NewReader(string(valid)))
			if key != "" {
				r.Header.Set("Authorization", "Bearer "+key)
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, r)
			if w.Code != 401 {
				t.Fatalf("auth required=%v: %d %s", required, w.Code, w.Body.String())
			}
		}
		for _, a := range []struct {
			body   string
			status int
		}{
			{`{}`, 400}, {`null`, 400}, {`{"allow_ai":null,"boot_id":"x","expected_revision":0}`, 400},
			{`{"allow_ai":false,"boot_id":"x","expected_revision":-1}`, 400},
			{`{"allow_ai":false,"boot_id":"x","expected_revision":0,"release_mode":"immediate"}`, 422},
			{`{"allow_ai":false,"boot_id":"x","expected_revision":0}`, 409},
			{string(valid) + ` {}`, 400},
		} {
			admissionResponse(t, router, "PUT", "/api/v1/resource-policy", a.body, a.status, "")
		}
		if g.Snapshot() != initial {
			t.Fatal("invalid writes changed policy")
		}
		paused := ownerPut(t, router, false, initial, 200)
		ownerPut(t, router, true, initial, 409)
		if g.Snapshot() != paused {
			t.Fatal("stale resume changed policy")
		}
	}
}
