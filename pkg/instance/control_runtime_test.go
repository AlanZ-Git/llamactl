package instance

import (
	"context"
	"encoding/json"
	"io"
	"llamactl/pkg/resourcepolicy"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func protocolFixture(t *testing.T, handler http.Handler) (*Instance, *httptest.Server) {
	t.Helper()
	backend := httptest.NewServer(handler)
	t.Cleanup(backend.Close)
	cfg, opts := policyFixture(t, "isolated")
	if err := json.Unmarshal([]byte(`{"controlled":true,"environment_policy":"isolated","backend_type":"llama_cpp","backend_options":{"no_models_autoload":true,"models_max":1}}`), opts); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "synthetic.gguf")
	if err := os.WriteFile(path, []byte("not a model"), 0600); err != nil {
		t.Fatal(err)
	}
	opts.Control = &ControlConfig{Models: map[string]ControlModel{"alpha": {Path: path}, "beta": {Path: path}}}
	opts.BackendOptions.SetPort(backend.Listener.Addr().(*net.TCPAddr).Port)
	inst, err := NewChecked("router", cfg, opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	return inst, backend
}

func publishSyntheticReady(t *testing.T, i *Instance, model string) string {
	t.Helper()
	i.InitializeControl("synthetic-boot")
	s := i.ControlSnapshot()
	m, err := i.BeginControl(s.BootID, s.Revision, true, true)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	i.SetStatus(Running)
	m.ActivateControl("synthetic-op")
	if err := m.PublishReady(context.Background(), model); err != nil {
		t.Fatal(err)
	}
	m.FinishControl("ready", model)
	return i.ControlSnapshot().ReadyToken
}

func TestP3ReadyTokenBindsModelLifecycleAndOwnerEpoch(t *testing.T) {
	var calls atomic.Int32
	inst, _ := protocolFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Ready-Token") != "" {
			t.Error("token forwarded")
		}
		calls.Add(1)
		w.WriteHeader(200)
	}))
	g, err := resourcepolicy.Open("")
	if err != nil {
		t.Fatal(err)
	}
	inst.policy = g
	token := publishSyntheticReady(t, inst, "alpha")
	request := func(model, token string) int {
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"`+model+`"}`))
		if token != "" {
			r.Header.Set("X-Ready-Token", token)
		}
		w := httptest.NewRecorder()
		_ = inst.ServeHTTP(w, r)
		return w.Code
	}
	for _, tc := range []struct{ model, token string }{{"alpha", ""}, {"alpha", "stale"}, {"beta", token}, {"router/alpha", token}} {
		if status := request(tc.model, tc.token); status != 409 {
			t.Fatalf("bad capability accepted: %d", status)
		}
	}
	if request("alpha", token) != 200 || calls.Load() != 1 {
		t.Fatal("valid ready token refused")
	}
	fresh := publishSyntheticReady(t, inst, "alpha")
	if fresh == token || request("alpha", token) != 409 {
		t.Fatal("load generation reused token")
	}
	inst.SetStatus(Stopped)
	inst.SetStatus(Running)
	if request("alpha", fresh) != 409 {
		t.Fatal("process generation reused token")
	}
	fresh = publishSyntheticReady(t, inst, "beta")
	s := g.Snapshot()
	s, err = g.Update(false, s.BootID, s.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if request("beta", fresh) != 503 {
		t.Fatal("owner pause did not dominate inference")
	}
	if _, err = g.Update(true, s.BootID, s.Revision); err != nil {
		t.Fatal(err)
	}
	if request("beta", fresh) != 409 || inst.ControlSnapshot().ReadyToken != "" {
		t.Fatal("resume revived old ready capability")
	}
}

func TestP3RealHTTPStreamHoldsSharedPermit(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		t.Run(fmtBool(disconnect), func(t *testing.T) {
			entered, finish := make(chan struct{}), make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(finish) })
			inst, _ := protocolFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Ready-Token") != "" {
					t.Error("token leaked")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: first\n\n")
				w.(http.Flusher).Flush()
				close(entered)
				select {
				case <-finish:
					io.WriteString(w, "data: [DONE]\n\n")
				case <-r.Context().Done():
				}
			}))
			token := publishSyntheticReady(t, inst, "alpha")
			exited := make(chan struct{})
			front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { defer close(exited); _ = inst.ServeHTTP(w, r) }))
			defer front.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, "POST", front.URL+"/v1/chat/completions", strings.NewReader(`{"model":"alpha","stream":true}`))
			req.Header.Set("X-Ready-Token", token)
			resp, err := front.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			<-entered
			s := inst.ControlSnapshot()
			lease, err := inst.BeginControl(s.BootID, s.Revision, true, false)
			if lease != nil {
				lease.Close()
			}
			if err == nil {
				t.Fatal("SSE released shared use before EOF")
			}
			if disconnect {
				cancel()
				resp.Body.Close()
			} else {
				once.Do(func() { close(finish) })
				if _, err := io.ReadAll(resp.Body); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-exited:
			case <-time.After(time.Second):
				t.Fatal("proxy failed to unwind")
			}
			lease, err = inst.BeginControl(s.BootID, s.Revision, true, false)
			if err != nil {
				t.Fatal(err)
			}
			lease.Close()
		})
	}
}
func fmtBool(b bool) string {
	if b {
		return "disconnect"
	}
	return "eof"
}

func TestP3ReadyProbeRequiresPresetLoadedAndTargetProps(t *testing.T) {
	var loaded, props atomic.Bool
	var requests atomic.Int32
	inst, _ := protocolFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Path {
		case "/health":
			io.WriteString(w, `{"status":"ok"}`)
		case "/models":
			state := "loading"
			if loaded.Load() {
				state = "loaded"
			}
			io.WriteString(w, `{"data":[{"id":"alpha","status":{"value":"`+state+`"}}]}`)
		case "/props":
			if r.URL.Query().Get("model") != "alpha" || r.URL.Query().Get("autoload") != "false" {
				t.Error("target availability probe can autoload")
			}
			if !props.Load() {
				w.WriteHeader(503)
			} else {
				io.WriteString(w, `{"default_generation_settings":{}}`)
			}
		}
	}))
	inst.InitializeControl("boot")
	m, err := inst.BeginControl("boot", 0, true, true)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	inst.SetStatus(Running)
	// Synthetic ownership evidence, no executable/model is launched in this test.
	inst.process.tree = &ownedTree{done: make(chan struct{})}
	for _, step := range []string{"loading", "child-unavailable", "available"} {
		if step != "loading" {
			loaded.Store(true)
		}
		if step == "available" {
			props.Store(true)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
		err := m.ProbeReady(ctx, "alpha")
		cancel()
		if (err == nil) != (step == "available") {
			t.Fatalf("probe %s: %v", step, err)
		}
	}
	before := requests.Load()
	for n := 0; n < 10; n++ {
		if inst.ControlSnapshot().ReadyToken != "" {
			t.Fatal("status polling fabricated ready")
		}
	}
	if requests.Load() != before {
		t.Fatal("GET snapshot has backend side effects")
	}
	path := filepath.Join(inst.globalInstanceSettings.InstancesDir, inst.Name, "preset.ini")
	if err := os.WriteFile(path, []byte("synthetic drift"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.ProbeReady(context.Background(), "alpha"); err == nil {
		t.Fatal("preset drift accepted as ready")
	}
}

func TestP3ReadyProbeRetriesTransientChildUnavailable(t *testing.T) {
	var attempts atomic.Int32
	inst, _ := protocolFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			io.WriteString(w, `{"status":"ok"}`)
		case "/models":
			io.WriteString(w, `{"data":[{"id":"alpha","status":{"value":"loaded"}}]}`)
		case "/props":
			if attempts.Add(1) == 1 {
				w.WriteHeader(503)
				return
			}
			io.WriteString(w, `{"default_generation_settings":{}}`)
		}
	}))
	inst.InitializeControl("boot")
	m, err := inst.BeginControl("boot", 0, true, true)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	inst.SetStatus(Running)
	inst.process.tree = &ownedTree{done: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := m.ProbeReady(ctx, "alpha"); err != nil {
		t.Fatalf("transient target probe failed before total deadline: %v", err)
	}
	if attempts.Load() != 2 {
		t.Fatal("target probe not retried")
	}
}

func TestP3ProtocolCannotUseNativeMaintenance(t *testing.T) {
	inst, _ := protocolFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	lease, err := inst.BeginMaintenance()
	if lease != nil {
		lease.Close()
	}
	if err == nil {
		t.Fatal("native maintenance bypasses revision protocol")
	}
}

func TestP3ProtocolRejectsUnverifiedInference(t *testing.T) {
	calls := 0
	inst, _ := protocolFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(200) }))
	inst.SetStatus(Running)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"alpha"}`))
	w := httptest.NewRecorder()
	_ = inst.ServeHTTP(w, req)
	if w.Code != 409 || calls != 0 {
		t.Fatalf("unverified inference forwarded: status=%d calls=%d", w.Code, calls)
	}
}
