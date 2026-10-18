package server

import (
	"context"
	"encoding/json"
	"llamactl/pkg/auth"
	"llamactl/pkg/config"
	"llamactl/pkg/database"
	"llamactl/pkg/instance"
	"llamactl/pkg/manager"
	"llamactl/pkg/resourcepolicy"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOwnerPolicyLegacyReadDoesNotAutostart(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer engine.Close()
	cfg := config.AppConfig{DataDir: t.TempDir()}
	cfg.Instances.InstancesDir = t.TempDir()
	cfg.Instances.LogsDir = t.TempDir()
	cfg.Instances.PortRange = [2]int{1024, 65535}
	cfg.Instances.MaxInstances = -1
	cfg.Instances.DefaultOnDemandStart = true
	cfg.Instances.OnDemandStartTimeout = 1
	cfg.Backends.LlamaCpp.Command = "synthetic-never-executed"
	cfg.Auth.ManagementKeys = []string{"synthetic-management-key"}
	db, err := database.Open(&database.Config{Path: filepath.Join(t.TempDir(), "owner.db"), MaxOpenConnections: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := database.RunMigrations(db); err != nil {
		t.Fatal(err)
	}
	im := manager.New(&cfg, db)
	defer im.Shutdown()
	var opts instance.Options
	if err := json.Unmarshal([]byte(`{"backend_type":"llama_cpp","backend_options":{}}`), &opts); err != nil {
		t.Fatal(err)
	}
	opts.BackendOptions.SetPort(engine.Listener.Addr().(*net.TCPAddr).Port)
	opts.BackendOptions.LlamaServerOptions.Host = "127.0.0.1"
	inst, err := im.CreateInstance("legacy", &opts)
	if err != nil {
		t.Fatal(err)
	}
	router := SetupRouter(NewHandler(im, nil, cfg, db, nil))
	s := ownerGet(t, router)
	s = ownerPut(t, router, false, s, 200)
	for _, path := range []string{"/api/v1/llama-cpp/legacy/models", "/llama-cpp/legacy/props", "/llama-cpp/legacy/slots"} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, admittedRequest("GET", path, ""))
		if w.Code != 204 || strings.Contains(w.Body.String(), "ai_paused") || strings.Contains(w.Body.String(), "instance_start_failed") {
			t.Errorf("read attempted automatic start: %s %d %s", path, w.Code, w.Body.String())
		}
	}
	for _, path := range []string{"/api/v1/instances/legacy/proxy/completion", "/llama-cpp/legacy/completion"} {
		admissionResponse(t, router, "POST", path, `{}`, 503, "ai_paused")
	}
	admissionResponse(t, router, "POST", "/api/v1/llama-cpp/legacy/models/synthetic/load", `{"model":"synthetic"}`, 409, "ai_paused")
	ownerPut(t, router, true, s, 200)
	if inst.IsRunning() {
		t.Fatal("legacy auto-started on resume")
	}
	inst.SetStatus(instance.ShuttingDown)
	admissionResponse(t, router, "POST", "/llama-cpp/legacy/completion", `{}`, 503, "instance_shutting_down")
	admissionResponse(t, router, "POST", "/api/v1/llama-cpp/legacy/models/synthetic/load", `{"model":"synthetic"}`, 503, "instance_shutting_down")
	inst.SetStatus(instance.Stopped)
}

func TestOwnerPolicyLegacyQuery(t *testing.T) {
	var calls atomic.Int64
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("X-Backend-URI", r.URL.RequestURI())
		w.WriteHeader(http.StatusNoContent)
	}))
	defer engine.Close()
	cfg := config.AppConfig{DataDir: t.TempDir()}
	cfg.Instances.InstancesDir = t.TempDir()
	cfg.Instances.LogsDir = t.TempDir()
	cfg.Instances.PortRange = [2]int{1024, 65535}
	cfg.Instances.MaxInstances = -1
	cfg.Backends.LlamaCpp.Command = "synthetic-never-executed"
	cfg.Auth.RequireManagementAuth = true
	cfg.Auth.RequireInferenceAuth = true
	cfg.Auth.ManagementKeys = []string{"synthetic-management-key"}
	db, err := database.Open(&database.Config{Path: filepath.Join(t.TempDir(), "owner.db"), MaxOpenConnections: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := database.RunMigrations(db); err != nil {
		t.Fatal(err)
	}
	im := manager.New(&cfg, db)
	defer im.Shutdown()
	var opts instance.Options
	if err := json.Unmarshal([]byte(`{"backend_type":"llama_cpp","backend_options":{}}`), &opts); err != nil {
		t.Fatal(err)
	}
	opts.BackendOptions.SetPort(engine.Listener.Addr().(*net.TCPAddr).Port)
	opts.BackendOptions.LlamaServerOptions.Host = "127.0.0.1"
	inst, err := im.CreateInstance("legacy", &opts)
	if err != nil {
		t.Fatal(err)
	}
	inst.SetStatus(instance.Running)
	defer inst.SetStatus(instance.Stopped)
	router := SetupRouter(NewHandler(im, nil, cfg, db, nil))
	central := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = inst.ServeHTTP(w, r) })
	queries := []string{
		"/props?model=synthetic&autoload=true",
		"/props?model=synthetic&autoload=1",
		"/props?%6dodel=synthetic&%61utoload=%74rue",
		"/models?reload=1",
		"/models?%72eload=%31",
		"/health?probe=1",
		"/v1/models?reload=1",
		"/slots?probe=1",
		"/metrics?probe=1",
	}
	type proxyRequest struct {
		handler http.Handler
		path    string
		target  string
	}
	var requests []proxyRequest
	for _, target := range queries {
		prefixes := []string{"/api/v1/instances/legacy/proxy", ""}
		if strings.HasPrefix(target, "/props?") || strings.HasPrefix(target, "/slots?") {
			prefixes = append(prefixes, "/llama-cpp/legacy")
		}
		for _, prefix := range prefixes {
			var handler http.Handler = router
			if prefix == "" {
				handler = central // Exercise final admission without adapter reservation.
			}
			requests = append(requests, proxyRequest{handler, prefix + target, target})
		}
	}
	// Native list-models rewrites the path but must retain the query policy.
	requests = append(requests, proxyRequest{router, "/api/v1/llama-cpp/legacy/models?reload=1", "/models?reload=1"})
	s := ownerPut(t, router, false, ownerGet(t, router), 200)
	for _, req := range requests {
		w := httptest.NewRecorder()
		req.handler.ServeHTTP(w, admittedRequest("GET", req.path, ""))
		if w.Code != 409 || !strings.Contains(w.Body.String(), `"error":"ai_paused"`) || !strings.Contains(w.Body.String(), `"retryable":false`) {
			t.Errorf("paused %s: %d %s", req.path, w.Code, w.Body.String())
		}
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("paused query requests reached backend: %d calls", got)
	}
	for _, path := range []string{"/api/v1/instances/legacy/proxy/health", "/api/v1/llama-cpp/legacy/models", "/llama-cpp/legacy/props"} {
		before := calls.Load()
		admissionResponse(t, router, "GET", path, "", 204, "")
		if calls.Load() != before+1 {
			t.Errorf("query-free read did not reach backend: %s", path)
		}
	}
	ownerPut(t, router, true, s, 200)
	for _, req := range requests {
		before := calls.Load()
		w := httptest.NewRecorder()
		req.handler.ServeHTTP(w, admittedRequest("GET", req.path, ""))
		if w.Code != 204 || calls.Load() != before+1 || w.Header().Get("X-Backend-URI") != req.target {
			t.Errorf("resumed %s: %d calls=%d target=%q", req.path, w.Code, calls.Load()-before, w.Header().Get("X-Backend-URI"))
		}
	}
}

func TestOwnerPolicyHTTPPersistenceFailure(t *testing.T) {
	dir := t.TempDir()
	g, err := resourcepolicy.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.AppConfig{ResourcePolicy: g}
	cfg.Auth.ManagementKeys = []string{"synthetic-management-key"}
	router := SetupRouter(NewHandler(nil, nil, cfg, nil, nil))
	if err := os.Mkdir(filepath.Join(dir, "resource-policy.json"), 0700); err != nil {
		t.Fatal(err)
	}
	s := ownerPut(t, router, false, ownerGet(t, router), 500)
	if s.AllowAI || s.Durable || s.Release != "unreleased_unknown" {
		t.Fatalf("pause failure: %+v", s)
	}
	s = ownerPut(t, router, true, s, 500)
	if s.AllowAI || s.Durable {
		t.Fatalf("resume failure: %+v", s)
	}
}

// Confirm a real allow_all inference key is valid for inference but not owner policy.
func TestOwnerPolicyRejectsRealInferenceKey(t *testing.T) {
	db, err := database.Open(&database.Config{Path: filepath.Join(t.TempDir(), "keys.db"), MaxOpenConnections: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := database.RunMigrations(db); err != nil {
		t.Fatal(err)
	}
	hash, err := auth.HashKey("synthetic-inference-key")
	if err != nil {
		t.Fatal(err)
	}
	key := &auth.APIKey{KeyHash: hash, Name: "synthetic", UserID: "system", PermissionMode: auth.PermissionModeAllowAll, CreatedAt: time.Now().Unix(), UpdatedAt: time.Now().Unix()}
	if err := db.CreateKey(context.Background(), key, nil); err != nil {
		t.Fatal(err)
	}
	for _, required := range []bool{false, true} {
		g, err := resourcepolicy.Open("")
		if err != nil {
			t.Fatal(err)
		}
		cfg := config.AppConfig{ResourcePolicy: g}
		cfg.Auth.RequireManagementAuth = required
		cfg.Auth.RequireInferenceAuth = true
		cfg.Auth.ManagementKeys = []string{"synthetic-management-key"}
		h := NewHandler(nil, nil, cfg, db, nil)
		probe := h.authMiddleware.InferenceAuthMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			perm, err := h.authMiddleware.ResolveInstancePermission(r.Context(), 1)
			if err != nil || !perm.CanStart {
				t.Error("real inference key not authenticated")
			}
			w.WriteHeader(204)
		}))
		r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		r.Header.Set("Authorization", "Bearer synthetic-inference-key")
		w := httptest.NewRecorder()
		probe.ServeHTTP(w, r)
		if w.Code != 204 {
			t.Fatalf("inference authentication: %d", w.Code)
		}
		h.authMiddleware.sessions = auth.NewSessionStore(time.Hour)
		sess := h.authMiddleware.sessions.Create("synthetic-owner", "Owner", "owner@example.invalid")
		router := SetupRouter(h)
		s := g.Snapshot()
		body, _ := json.Marshal(map[string]any{"allow_ai": false, "boot_id": s.BootID, "expected_revision": s.Revision})
		r = httptest.NewRequest("PUT", "/api/v1/resource-policy", strings.NewReader(string(body)))
		r.Header.Set("Authorization", "Bearer synthetic-inference-key")
		w = httptest.NewRecorder()
		router.ServeHTTP(w, r)
		if w.Code != 401 || g.Snapshot() != s {
			t.Fatal("allow_all inference key changed owner policy")
		}
		r = httptest.NewRequest("PUT", "/api/v1/resource-policy", strings.NewReader(string(body)))
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sess.ID})
		w = httptest.NewRecorder()
		router.ServeHTTP(w, r)
		if w.Code != 200 || g.Snapshot().AllowAI {
			t.Fatalf("owner session rejected: %d %s", w.Code, w.Body.String())
		}
	}
}
