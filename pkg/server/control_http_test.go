package server

import (
	"encoding/json"
	"llamactl/pkg/config"
	"llamactl/pkg/database"
	"llamactl/pkg/instance"
	"llamactl/pkg/manager"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func p3HTTPFixture(t *testing.T, disableLegacyAuth ...bool) (http.Handler, manager.InstanceManager, *instance.Instance) {
	t.Helper()
	cfg := config.AppConfig{}
	cfg.DataDir = t.TempDir()
	cfg.Instances.InstancesDir = t.TempDir()
	cfg.Instances.LogsDir = t.TempDir()
	cfg.Instances.PortRange = [2]int{18000, 18020}
	cfg.Instances.MaxInstances = -1
	cfg.Auth.RequireManagementAuth = true
	cfg.Auth.RequireInferenceAuth = true
	if len(disableLegacyAuth) != 0 && disableLegacyAuth[0] {
		cfg.Auth.RequireManagementAuth = false
		cfg.Auth.RequireInferenceAuth = false
	}
	cfg.Auth.ManagementKeys = []string{"synthetic-management-key"}
	db, err := database.Open(&database.Config{Path: filepath.Join(t.TempDir(), "synthetic.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.RunMigrations(db); err != nil {
		t.Fatal(err)
	}
	var opts instance.Options
	if err := json.Unmarshal([]byte(`{"controlled":true,"environment_policy":"isolated","backend_type":"llama_cpp","backend_options":{"port":18001,"no_models_autoload":true,"models_max":1},"control":{"models":{"alpha":{"path":"synthetic-not-opened.gguf"}}}}`), &opts); err != nil {
		t.Fatal(err)
	}
	descriptor, err := instance.NewDescriptor("protocol", &cfg, &opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Save(descriptor); err != nil {
		t.Fatal(err)
	}
	im := manager.New(&cfg, db)
	t.Cleanup(im.Shutdown)
	inst, err := im.GetInstance("protocol")
	if err != nil {
		t.Fatal(err)
	}
	return SetupRouter(NewHandler(im, nil, cfg, db, nil)), im, inst
}

func TestP3ControllerRequiresFixedScope(t *testing.T) {
	router, _, _ := p3HTTPFixture(t)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, admittedRequest("POST", "/api/v1/auth/keys/", `{"name":"synthetic-controller","controller":true,"permission_mode":"allow_all"}`))
	if w.Code != 400 {
		t.Fatalf("unbounded controller issued: %d %s", w.Code, w.Body.String())
	}
}

func TestP3ScopedControllerRoutesAndOperationOwnership(t *testing.T) {
	router, im, inst := p3HTTPFixture(t)
	opts := &instance.Options{}
	if err := json.Unmarshal([]byte(`{"backend_type":"llama_cpp","backend_options":{"port":18002}}`), opts); err != nil {
		t.Fatal(err)
	}
	if _, err := im.CreateInstance("other", opts); err != nil {
		t.Fatal(err)
	}
	call := func(key, method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+key)
		router.ServeHTTP(w, r)
		return w
	}
	newKey := func() string {
		b, _ := json.Marshal(map[string]any{"name": "synthetic-controller", "controller": true, "permission_mode": "per_instance", "permissions": []map[string]any{{"instance_id": inst.ID}}})
		w := call("synthetic-management-key", "POST", "/api/v1/auth/keys/", string(b))
		if w.Code != 201 {
			t.Fatalf("controller creation: %d %s", w.Code, w.Body.String())
		}
		var key CreateKeyResponse
		_ = json.Unmarshal(w.Body.Bytes(), &key)
		if !key.Controller {
			t.Fatal("controller discriminator lost")
		}
		return key.Key
	}
	key, otherKey := newKey(), newKey()
	for _, tc := range []struct {
		method, path, body string
		want               int
	}{
		{"GET", "/api/v1/instances/protocol/control", "", 200},
		{"PUT", "/api/v1/instances/protocol/control", `{}`, 403},
		{"GET", "/api/v1/instances/other/control", "", 403},
		{"GET", "/api/v1/instances/protocol", "", 401},
		{"PUT", "/api/v1/instances/protocol", `{}`, 401},
		{"POST", "/api/v1/instances/protocol/start", `{}`, 401},
		{"GET", "/api/v1/auth/keys/", "", 401},
		{"PUT", "/api/v1/resource-policy", `{}`, 401},
	} {
		w := call(key, tc.method, tc.path, tc.body)
		if w.Code != tc.want {
			t.Errorf("%s %s: %d %s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
	w := call(key, "GET", "/v1/models", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), `"other"`) {
		t.Errorf("scope leaked models: %d %s", w.Code, w.Body.String())
	}
	s, err := im.(manager.ControlManager).ControlStatus("protocol")
	if err != nil {
		t.Fatal(err)
	}
	recovery, _ := json.Marshal(map[string]any{"boot_id": s.BootID, "expected_revision": s.Revision, "verified_stopped": true})
	w = call("synthetic-management-key", "PUT", "/api/v1/instances/protocol/control", string(recovery))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	request, _ := json.Marshal(map[string]any{"boot_id": s.BootID, "expected_revision": s.Revision, "idempotency_key": "one", "action": "stop"})
	w = call(key, "POST", "/api/v1/instances/protocol/operations", string(request))
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	var op struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &op)
	for _, tc := range []struct {
		key    string
		status int
	}{{key, 200}, {otherKey, 404}, {"synthetic-management-key", 200}} {
		w = call(tc.key, "GET", "/api/v1/instances/protocol/operations/"+op.ID, "")
		if w.Code != tc.status {
			t.Errorf("operation ownership: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestP3ControlHTTPBootRecoveryAndRetry(t *testing.T) {
	router, _, _ := p3HTTPFixture(t)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, admittedRequest(method, path, body))
		return w
	}
	w := call("GET", "/api/v1/instances/protocol/control", "")
	if w.Code != 200 {
		t.Fatalf("control endpoint missing: %d %s", w.Code, w.Body.String())
	}
	var s struct {
		Boot     string `json:"boot_id"`
		Revision uint64 `json:"revision"`
		State    string `json:"state"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	if s.Boot == "" || s.State != "unknown" {
		t.Fatalf("boot fabricated readiness: %s", w.Body.String())
	}
	req := map[string]any{"boot_id": s.Boot, "expected_revision": s.Revision, "idempotency_key": "stop-1", "action": "stop"}
	b, _ := json.Marshal(req)
	w = call("POST", "/api/v1/instances/protocol/operations", string(b))
	if w.Code != 409 || !strings.Contains(w.Body.String(), "recovery_required") {
		t.Fatalf("new boot replay allowed: %d %s", w.Code, w.Body.String())
	}
	recovery, _ := json.Marshal(map[string]any{"boot_id": s.Boot, "expected_revision": s.Revision, "verified_stopped": true})
	w = call("PUT", "/api/v1/instances/protocol/control", string(recovery))
	if w.Code != 200 {
		t.Fatalf("recovery: %d %s", w.Code, w.Body.String())
	}
	w = call("POST", "/api/v1/instances/protocol/operations", string(b))
	if w.Code != 202 {
		t.Fatalf("stop: %d %s", w.Code, w.Body.String())
	}
	var first map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &first)
	w = call("POST", "/api/v1/instances/protocol/operations", string(b))
	var repeat map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &repeat)
	if w.Code != 202 || first["id"] != repeat["id"] {
		t.Fatalf("retry lost operation: %d %s", w.Code, w.Body.String())
	}
	req["action"] = "unload"
	b, _ = json.Marshal(req)
	w = call("POST", "/api/v1/instances/protocol/operations", string(b))
	if w.Code != 409 {
		t.Fatalf("same key changed payload: %d", w.Code)
	}
}
