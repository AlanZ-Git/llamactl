package server

import (
	"context"
	"encoding/json"
	"io"
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

	"llamactl/pkg/config"
	"llamactl/pkg/database"
	"llamactl/pkg/instance"
	"llamactl/pkg/manager"
)

// A real manager, SQLite store, router, auth middleware and reverse proxy. Only
// the native engine is replaced by an httptest server; no model is executed.
func admissionServer(t *testing.T, backend http.Handler) (http.Handler, manager.InstanceManager, *instance.Instance, string) {
	t.Helper()
	engine := httptest.NewServer(backend)
	t.Cleanup(engine.Close)
	cfg := config.AppConfig{}
	cfg.Instances.InstancesDir = t.TempDir()
	cfg.Instances.LogsDir = t.TempDir()
	cfg.Instances.PortRange = [2]int{1024, 65535}
	cfg.Instances.MaxInstances = -1
	cfg.Instances.MaxRunningInstances = -1
	cfg.Instances.DefaultAutoRestart = true
	cfg.Instances.DefaultOnDemandStart = true
	cfg.Instances.DefaultIdleTimeout = 1
	cfg.Backends.LlamaCpp.Command = "synthetic-not-executed"
	cfg.Auth.RequireManagementAuth = true
	cfg.Auth.RequireInferenceAuth = true
	cfg.Auth.ManagementKeys = []string{"synthetic-management-key"}
	db, err := database.Open(&database.Config{Path: filepath.Join(t.TempDir(), "admission.db"), MaxOpenConnections: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := database.RunMigrations(db); err != nil {
		t.Fatal(err)
	}
	im := manager.New(&cfg, db)
	t.Cleanup(im.Shutdown)
	var opts instance.Options
	if err := json.Unmarshal([]byte(`{"controlled":true,"environment_policy":"isolated","preset_ini":"[synthetic]\nmodel=synthetic.gguf\n","backend_type":"llama_cpp","backend_options":{"no_models_autoload":true,"models_max":1}}`), &opts); err != nil {
		t.Fatal(err)
	}
	opts.BackendOptions.SetPort(engine.Listener.Addr().(*net.TCPAddr).Port)
	inst, err := im.CreateInstance("admitted", &opts)
	if err != nil {
		t.Fatal(err)
	}
	inst.SetStatus(instance.Running)
	t.Cleanup(func() { inst.SetStatus(instance.Stopped) })
	return SetupRouter(NewHandler(im, nil, cfg, db, nil)), im, inst, filepath.Join(cfg.Instances.InstancesDir, inst.Name, "preset.ini")
}

func admittedRequest(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer synthetic-management-key")
	req.Header.Set("Content-Type", "application/json")
	return req
}
func admissionResponse(t *testing.T, router http.Handler, method, path, body string, status int, code string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, admittedRequest(method, path, body))
	if recorder.Code != status || (code != "" && !strings.Contains(recorder.Body.String(), `"error":"`+code+`"`)) {
		t.Fatalf("%s %s: %d %s; want %d %s", method, path, recorder.Code, recorder.Body.String(), status, code)
	}
}
func awaitAdmission(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("admission barrier timed out")
	}
}

func TestControlledSSEProductionBoundaries(t *testing.T) {
	for _, inferencePath := range []string{"/v1/chat/completions", "/llama-cpp/admitted/v1/chat/completions", "/api/v1/instances/admitted/proxy/completion"} {
		t.Run(inferencePath, func(t *testing.T) {
			entered, finish := make(chan struct{}), make(chan struct{})
			var mutations atomic.Int32
			router, im, inst, preset := admissionServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					_, _ = io.WriteString(w, `{}`)
					return
				}
				if r.URL.Path == "/models/load" || r.URL.Path == "/models/unload" {
					mutations.Add(1)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: first\n\n")
				w.(http.Flusher).Flush()
				close(entered)
				select {
				case <-finish:
				case <-r.Context().Done():
				}
			}))
			defer close(finish)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				router.ServeHTTP(httptest.NewRecorder(), admittedRequest("POST", inferencePath, `{"model":"admitted/synthetic","stream":true}`).WithContext(ctx))
			}()
			awaitAdmission(t, entered)
			oldOptions := inst.GetOptions()
			oldPreset, err := os.ReadFile(preset)
			if err != nil {
				t.Fatal(err)
			}
			candidate, err := oldOptions.Clone()
			if err != nil {
				t.Fatal(err)
			}
			candidate.BackendOptions.SetPort(18011)
			changed := "[other]\nmodel=other.gguf\n"
			candidate.PresetIni = &changed
			payload, _ := json.Marshal(candidate)
			for _, action := range []struct{ method, path, body string }{
				{"PUT", "/api/v1/instances/admitted", string(payload)},
				{"POST", "/api/v1/instances/admitted/start", ""},
				{"POST", "/api/v1/instances/admitted/stop", ""},
				{"POST", "/api/v1/instances/admitted/restart", ""},
				{"DELETE", "/api/v1/instances/admitted", ""},
				{"POST", "/api/v1/instances/admitted", string(payload)},
				{"POST", "/api/v1/llama-cpp/admitted/models/synthetic/load", `{"model":"synthetic"}`},
				{"POST", "/api/v1/llama-cpp/admitted/models/synthetic/unload", `{"model":"synthetic"}`},
				{"POST", "/api/v1/instances/admitted/proxy/models/load", `{"model":"synthetic"}`},
				{"POST", "/api/v1/instances/admitted/proxy/models/unload", `{"model":"synthetic"}`},
			} {
				admissionResponse(t, router, action.method, action.path, action.body, 409, "busy")
			}
			for name, control := range map[string]func() error{
				"update": func() error { _, err := im.UpdateInstance(inst.Name, candidate); return err },
				"stop":   inst.Stop, "restart": inst.Restart, "start": inst.Start,
				"options": func() error { return inst.SetOptions(candidate) },
			} {
				if _, code, ok := instance.AdmissionHTTPError(control()); !ok || code != "busy" {
					t.Fatalf("direct %s was not busy", name)
				}
			}
			admissionResponse(t, router, "GET", "/api/v1/instances/admitted", "", 200, "")
			admissionResponse(t, router, "GET", "/api/v1/instances/admitted/proxy/health", "", 200, "")
			currentPreset, err := os.ReadFile(preset)
			if err != nil || string(currentPreset) != string(oldPreset) || inst.GetOptions() != oldOptions || inst.GetPort() != oldOptions.BackendOptions.GetPort() || inst.GetStatus() != instance.Running || mutations.Load() != 0 {
				t.Fatal("busy call mutated old state or reached engine")
			}
			// Cancellation must unwind the reverse proxy before the shared permit ends.
			cancel()
			awaitAdmission(t, done)
			lease, err := inst.BeginMaintenance()
			if err != nil {
				t.Fatalf("permit leaked after cancel: %v", err)
			}
			lease.Close()
		})
	}
}

func TestControlledMaintenanceProductionBoundaries(t *testing.T) {
	entered, finish := make(chan struct{}), make(chan struct{})
	var finishOnce sync.Once
	defer finishOnce.Do(func() { close(finish) })
	router, _, inst, _ := admissionServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			_, _ = io.WriteString(w, `{}`)
			return
		}
		close(entered)
		<-finish
		_, _ = io.WriteString(w, `{}`)
	}))
	done := make(chan struct{})
	recorder := httptest.NewRecorder()
	go func() {
		defer close(done)
		router.ServeHTTP(recorder, admittedRequest("POST", "/api/v1/llama-cpp/admitted/models/synthetic/load", `{"model":"synthetic"}`))
	}()
	awaitAdmission(t, entered)
	for _, path := range []string{"/v1/chat/completions", "/llama-cpp/admitted/completion", "/api/v1/instances/admitted/proxy/v1/chat/completions"} {
		admissionResponse(t, router, "POST", path, `{"model":"admitted/synthetic"}`, 409, "busy")
	}
	admissionResponse(t, router, "POST", "/api/v1/llama-cpp/admitted/models/synthetic/unload", `{"model":"synthetic"}`, 409, "busy")
	admissionResponse(t, router, "GET", "/api/v1/instances/admitted", "", 200, "")
	admissionResponse(t, router, "GET", "/api/v1/llama-cpp/admitted/models", "", 200, "")
	admissionResponse(t, router, "GET", "/llama-cpp/admitted/props", "", 200, "")
	finishOnce.Do(func() { close(finish) })
	awaitAdmission(t, done)
	if recorder.Code != 200 {
		t.Fatalf("maintenance failed: %d %s", recorder.Code, recorder.Body.String())
	}
	lease, err := inst.BeginMaintenance()
	if err != nil {
		t.Fatal(err)
	}
	lease.Close()
}

func TestControlledProxyRejectsBypassesAndStoppedAutostart(t *testing.T) {
	var calls atomic.Int32
	router, _, inst, _ := admissionServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); _, _ = io.WriteString(w, `{}`) }))
	for _, path := range []string{"/v1/models/load", "/llama-cpp/admitted/v1/models/load", "/api/v1/instances/admitted/proxy/props", "/api/v1/instances/admitted/proxy/slots/0?action=restore", "/api/v1/instances/admitted/proxy/models/load/", "/api/v1/instances/admitted/proxy/unknown"} {
		admissionResponse(t, router, "POST", path, `{"model":"admitted/synthetic"}`, 400, "unsupported_controlled_request")
	}
	for _, body := range []string{`{"model":"admitted/synthetic","autoload":true}`, `{"model":"admitted/synthetic","autoload":false}`, `{"model":"admitted/synthetic","autoload":null}`, `{"model":"admitted/synthetic","extra":{"autoload":true}}`, `{"model":"admitted/synthetic","models_max":2}`} {
		for _, path := range []string{"/v1/chat/completions", "/llama-cpp/admitted/completion", "/api/v1/instances/admitted/proxy/completion"} {
			admissionResponse(t, router, "POST", path, body, 400, "unsupported_controlled_request")
		}
	}
	admissionResponse(t, router, "POST", "/llama-cpp/admitted/completion?autoload=true", `{"model":"synthetic"}`, 400, "unsupported_controlled_request")
	if calls.Load() != 0 {
		t.Fatal("unsupported request reached backend")
	}
	inst.SetStatus(instance.Stopped)
	for _, path := range []string{"/v1/chat/completions", "/llama-cpp/admitted/completion", "/api/v1/instances/admitted/proxy/completion", "/api/v1/llama-cpp/admitted/models/synthetic/load"} {
		admissionResponse(t, router, "POST", path, `{"model":"admitted/synthetic"}`, 503, "instance_not_running")
	}
	admissionResponse(t, router, "GET", "/api/v1/llama-cpp/admitted/models", "", 503, "instance_not_running")
	if calls.Load() != 0 || inst.GetStatus() != instance.Stopped {
		t.Fatal("controlled request auto-started")
	}
}
