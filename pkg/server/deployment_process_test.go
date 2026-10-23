package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"llamactl/pkg/config"
	"llamactl/pkg/database"
	"llamactl/pkg/instance"
	"llamactl/pkg/manager"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

type deploymentFixture struct {
	front    *httptest.Server
	manager  manager.InstanceManager
	instance *instance.Instance
	cfg      *config.AppConfig
	events   string
	contract controlAPIDoc
	n1Store  *database.N1Store
}

// Extends the P3 controlled child helper: actual SQLite, manager, Router process,
// native probes and final embedded WebUI. No model is opened and no GPU is used.
func newDeploymentFixture(t *testing.T, mode string, browser bool) *deploymentFixture {
	t.Helper()
	if mode == "" {
		mode = "success"
	}
	cfg := &config.AppConfig{}
	cfg.DataDir = t.TempDir()
	cfg.Instances.InstancesDir = t.TempDir()
	cfg.Instances.LogsDir = t.TempDir()
	cfg.Instances.PortRange = [2]int{1024, 65535}
	cfg.Instances.MaxInstances = -1
	cfg.Auth.RequireManagementAuth = true
	cfg.Auth.RequireInferenceAuth = true
	cfg.Auth.ManagementKeys = []string{"synthetic-management-key"}
	cfg.Backends.LlamaCpp.Command = os.Args[0]
	db, err := database.Open(&database.Config{Path: filepath.Join(t.TempDir(), "test.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := database.RunMigrations(db); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	modelPath := filepath.Join(t.TempDir(), "synthetic-not-a-model.gguf")
	if err := os.WriteFile(modelPath, []byte("synthetic fixture, never opened by engine"), 0600); err != nil {
		t.Fatal(err)
	}
	var opts instance.Options
	if err := json.Unmarshal([]byte(`{"controlled":true,"environment_policy":"isolated","backend_type":"llama_cpp","backend_options":{"no_models_autoload":true,"models_max":1}}`), &opts); err != nil {
		t.Fatal(err)
	}
	opts.Control = &instance.ControlConfig{Models: map[string]instance.ControlModel{"alpha": {Path: modelPath}}}
	opts.BackendOptions.SetPort(port)
	events := filepath.Join(t.TempDir(), "synthetic-events")
	opts.Environment = map[string]string{"P3_SYNTHETIC_ENGINE": "1", "P3_SYNTHETIC_PORT": strconv.Itoa(port), "P3_SYNTHETIC_MODE": mode, "P3_SYNTHETIC_EVENTS": events}
	if browser {
		opts.Environment["P3_SYNTHETIC_BROWSER"] = "1"
	}
	descriptor, err := instance.NewDescriptor("router", cfg, &opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Save(descriptor); err != nil {
		t.Fatal(err)
	}
	im := manager.New(cfg, db)
	t.Cleanup(im.Shutdown)
	cm := im.(manager.ControlManager)
	s, err := cm.ControlStatus("router")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cm.RecoverControl(context.Background(), "router", s.BootID, s.Revision); err != nil {
		t.Fatal(err)
	}
	inst, err := im.GetInstance("router")
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(SetupRouter(NewHandler(im, nil, *cfg, db, nil)))
	t.Cleanup(front.Close)
	front.Client().Timeout = 15 * time.Second
	return &deploymentFixture{front: front, manager: im, instance: inst, cfg: cfg, events: events, contract: readControlAPI(t)}
}
func (f *deploymentFixture) call(t *testing.T, key, method, path string, body any) (int, []byte) {
	t.Helper()
	var data []byte
	switch v := body.(type) {
	case string:
		data = []byte(v)
	case nil:
	default:
		data, _ = json.Marshal(v)
	}
	req, _ := http.NewRequest(method, f.front.URL+path, bytes.NewReader(data))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := f.front.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err = io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(path, "/tests") && !strings.Contains(path, "format=csv") {
		contractPath := "/api/v1/instances/{name}/tests"
		if strings.Contains(path, "/tests/") {
			contractPath += "/{id}"
		}
		if strings.HasSuffix(path, "/cancel") {
			contractPath += "/cancel"
		}
		f.contract.response(t, method, contractPath, response.StatusCode, data)
	}
	return response.StatusCode, data
}
func (f *deploymentFixture) request(t *testing.T, key string) map[string]any {
	t.Helper()
	s, err := f.manager.(manager.ControlManager).ControlStatus("router")
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"boot_id": s.BootID, "expected_revision": s.Revision, "idempotency_key": key, "model": "alpha", "fixture_id": instance.DeploymentFixtureID, "patch": map[string]any{"context": 2048, "gpu_layers": 0, "flash_attention": false}}
}
func (f *deploymentFixture) submit(t *testing.T, req map[string]any) manager.DeploymentTest {
	t.Helper()
	status, b := f.call(t, "synthetic-management-key", "POST", "/api/v1/instances/router/tests", req)
	if status != 202 {
		t.Fatalf("submit: %d %s", status, b)
	}
	var report manager.DeploymentTest
	if err := json.Unmarshal(b, &report); err != nil {
		t.Fatal(err)
	}
	return report
}
func (f *deploymentFixture) wait(t *testing.T, id string, condition func(manager.DeploymentTest) bool) manager.DeploymentTest {
	t.Helper()
	deadline := time.Now().Add(12 * time.Second)
	var report manager.DeploymentTest
	for time.Now().Before(deadline) {
		status, b := f.call(t, "synthetic-management-key", "GET", "/api/v1/instances/router/tests/"+id, nil)
		if status != 200 {
			t.Fatalf("poll: %d %s", status, b)
		}
		if err := json.Unmarshal(b, &report); err != nil {
			t.Fatal(err)
		}
		if condition(report) {
			return report
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition timed out: %+v", report)
	return report
}
func testEnded(r manager.DeploymentTest) bool { return r.EndedAt != nil }
func (f *deploymentFixture) count(path string) int {
	b, _ := os.ReadFile(f.events)
	return strings.Count(string(b), path+"\n")
}
func (f *deploymentFixture) cancel(t *testing.T, id, key string) manager.DeploymentTest {
	t.Helper()
	s := f.instance.ControlSnapshot()
	status, b := f.call(t, "synthetic-management-key", "POST", "/api/v1/instances/router/tests/"+id+"/cancel", map[string]any{"boot_id": s.BootID, "expected_revision": s.Revision, "idempotency_key": key})
	if status != 200 {
		t.Fatalf("cancel: %d %s", status, b)
	}
	var r manager.DeploymentTest
	_ = json.Unmarshal(b, &r)
	return r
}

func TestDeploymentActualHTTPReportIdempotencyAndReservation(t *testing.T) {
	f := newDeploymentFixture(t, "success", false)
	request := f.request(t, "one")
	request["warmup"] = true
	report := f.submit(t, request)
	f.wait(t, report.ID, func(r manager.DeploymentTest) bool { return r.Stage == "initial" })
	status, b := f.call(t, "synthetic-management-key", "POST", "/v1/chat/completions", map[string]any{"model": "router/alpha", "stream": true})
	if status != 409 || !bytes.Contains(b, []byte("busy")) {
		t.Fatalf("inference not reserved: %d %s", status, b)
	}
	s := f.instance.ControlSnapshot()
	status, b = f.call(t, "synthetic-management-key", "POST", "/api/v1/instances/router/operations", map[string]any{"action": "stop", "boot_id": s.BootID, "expected_revision": s.Revision, "idempotency_key": "busy"})
	if status != 409 || !bytes.Contains(b, []byte("busy")) {
		t.Fatalf("control not reserved: %d %s", status, b)
	}
	report = f.wait(t, report.ID, testEnded)
	if report.State != "succeeded" || report.Cleanup != "unloaded_test_owned" || report.CompletedMeasured != 3 || report.AttemptedGenerations != 4 {
		t.Fatalf("report: %+v", report)
	}
	if report.Revision != 1 || report.LoadMode != "test_owned" || report.CacheCondition != "unknown" {
		t.Fatalf("identity/cache: %+v", report)
	}
	for n, s := range report.Samples {
		if s.TTFTMS == nil || *s.TTFTMS < 100 || !s.Completed || s.PromptTokens == nil || *s.PromptTokens != 21 || s.CompletionTokens == nil || *s.CompletionTokens != 3 || s.ResponseBytes != 9 {
			t.Fatalf("bad measurement %d: %+v", n, s)
		}
	}
	if report.Samples[0].Kind != "warmup" || report.Samples[1].Kind != "initial" || report.Samples[2].Kind != "subsequent" {
		t.Fatal("warmup labels")
	}
	if report.EndedAt == nil || f.instance.ControlSnapshot().ActiveOperation != "" {
		t.Fatal("terminal before reservation release")
	}
	repeated := f.submit(t, request)
	if repeated.ID != report.ID || f.count("/v1/chat/completions") != 4 {
		t.Fatal("idempotent retry duplicated generations")
	}
	request["measured_generations"] = 1
	status, _ = f.call(t, "synthetic-management-key", "POST", "/api/v1/instances/router/tests", request)
	if status != 409 {
		t.Fatal("payload conflict not rejected")
	}
	status, b = f.call(t, "synthetic-management-key", "GET", "/api/v1/instances/router/tests/"+report.ID+"?format=csv", nil)
	if status != 200 {
		t.Fatal(string(b))
	}
	rows, err := csv.NewReader(bytes.NewReader(b)).ReadAll()
	if err != nil || len(rows) < 30 {
		t.Fatalf("csv: %v %s", err, b)
	}
	for _, forbidden := range []string{"synthetic-not-a-model.gguf", "ready_token", "private/path", "secret-backend-body", "List three colors", "synthetic control only"} {
		raw, _ := json.Marshal(report)
		if bytes.Contains(raw, []byte(forbidden)) || bytes.Contains(b, []byte(forbidden)) {
			t.Fatalf("report leaked %s", forbidden)
		}
	}
	doc := readControlAPI(t)
	raw, _ := json.Marshal(report)
	doc.response(t, "GET", "/api/v1/instances/{name}/tests/{id}", 200, raw)
}

func TestDeploymentActualFailuresAndCleanup(t *testing.T) {
	for _, tc := range []struct{ mode, state, cleanup string }{
		{"error", "failed", "unloaded_test_owned"}, {"second_error", "partial", "unloaded_test_owned"},
		{"load_error_ack", "failed", "unloaded_test_owned"}, {"malformed_stream", "failed", "unloaded_test_owned"},
		{"unload_error", "partial", "failed_not_confirmed_released"}, {"unload_slow", "partial", "failed_not_confirmed_released"},
		{"no_usage", "succeeded", "unloaded_test_owned"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			f := newDeploymentFixture(t, tc.mode, false)
			req := f.request(t, "failure")
			report := f.submit(t, req)
			report = f.wait(t, report.ID, testEnded)
			if report.State != tc.state || report.Cleanup != tc.cleanup {
				t.Fatalf("outcome: %+v", report)
			}
			raw, _ := json.Marshal(report)
			if bytes.Contains(raw, []byte("secret-backend-body")) || bytes.Contains(raw, []byte("private/path")) {
				t.Fatal("raw error leaked")
			}
			if tc.mode == "no_usage" && (report.Samples[0].PromptTokens != nil || report.Samples[0].DecodeMS != nil) {
				t.Fatal("invented usage/timings")
			}
			if tc.mode == "error" && f.count("/v1/chat/completions") != 1 {
				t.Fatal("generation retried")
			}
			if tc.mode == "unload_slow" && report.CleanupError != "cleanup_deadline_exceeded" {
				t.Fatal(report.CleanupError)
			}
		})
	}
}

func TestDeploymentActualCancelPauseAndNoReplay(t *testing.T) {
	for _, mode := range []string{"slow_load", "slow"} {
		for _, pauseOwner := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/owner=%v", mode, pauseOwner), func(t *testing.T) {
				f := newDeploymentFixture(t, mode, false)
				report := f.submit(t, f.request(t, "cancel"))
				f.wait(t, report.ID, func(r manager.DeploymentTest) bool {
					if mode == "slow_load" {
						return r.Stage == "ready_probe"
					}
					return r.AttemptedGenerations == 1
				})
				if pauseOwner {
					policy := f.cfg.ResourcePolicy
					s := policy.Snapshot()
					if _, err := policy.Update(false, s.BootID, s.Revision); err != nil {
						t.Fatal(err)
					}
				} else {
					f.cancel(t, report.ID, "cancel-once")
				}
				report = f.wait(t, report.ID, testEnded)
				if report.State != "cancelled" {
					t.Fatalf("cancel outcome: %+v", report)
				}
				if pauseOwner {
					if report.ErrorCode != "ai_paused" {
						t.Fatal(report.ErrorCode)
					}
					policy := f.cfg.ResourcePolicy
					s := policy.Snapshot()
					if _, err := policy.Update(true, s.BootID, s.Revision); err != nil {
						t.Fatal(err)
					}
				} else {
					f.cancel(t, report.ID, "cancel-once")
					if report.Cleanup != "unloaded_test_owned" {
						t.Fatal(report.Cleanup)
					}
				}
				before := f.count("/v1/chat/completions")
				loads := f.count("/models/load")
				time.Sleep(250 * time.Millisecond)
				if before != f.count("/v1/chat/completions") || loads != f.count("/models/load") {
					t.Fatal("cancelled run replayed after resume")
				}
				if mode == "slow_load" && before != 0 {
					t.Fatal("generation dispatched after load cancellation")
				}
			})
		}
	}
}

func TestDeploymentActualReuseReadyConflictAndTimeout(t *testing.T) {
	t.Run("reuse", func(t *testing.T) {
		f := newDeploymentFixture(t, "success", false)
		cm := f.manager.(manager.ControlManager)
		s := f.instance.ControlSnapshot()
		data, _ := json.Marshal(map[string]any{"action": "apply", "model": "alpha", "patch": map[string]any{"context": 2048, "gpu_layers": 0, "flash_attention": false}, "boot_id": s.BootID, "expected_revision": s.Revision, "idempotency_key": "preexisting"})
		op, err := cm.SubmitControl("router", "admin", data)
		if err != nil {
			t.Fatal(err)
		}
		until := time.Now().Add(5 * time.Second)
		for op.EndedAt == nil && time.Now().Before(until) {
			time.Sleep(10 * time.Millisecond)
			op, err = cm.GetControlOperation("router", "admin", op.ID, true)
			if err != nil {
				t.Fatal(err)
			}
		}
		if op.State != "succeeded" {
			t.Fatalf("preexisting load: %+v", op)
		}
		before := f.instance.ControlSnapshot()
		report := f.submit(t, f.request(t, "reuse"))
		report = f.wait(t, report.ID, testEnded)
		if report.LoadMode != "reused_ready" || report.Cleanup != "not_owned_preserved" || f.count("/models/unload") != 0 || f.count("/models/load") != 1 {
			t.Fatalf("preexisting load disturbed: %+v", report)
		}
		after := f.instance.ControlSnapshot()
		if after.ReadyToken != before.ReadyToken || after.Revision != before.Revision {
			t.Fatal("reuse changed ready identity")
		}
		req := f.request(t, "conflict")
		req["patch"] = map[string]any{"context": 4096}
		status, b := f.call(t, "synthetic-management-key", "POST", "/api/v1/instances/router/tests", req)
		if status != 409 || !bytes.Contains(b, []byte("ready_configuration_conflict")) {
			t.Fatalf("ready patch: %d %s", status, b)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		f := newDeploymentFixture(t, "slow", false)
		req := f.request(t, "deadline")
		req["timeout_seconds"] = 2
		report := f.submit(t, req)
		report = f.wait(t, report.ID, testEnded)
		if report.ErrorCode != "deadline_exceeded" || report.Cleanup != "unloaded_test_owned" || report.ElapsedMS > 2500 {
			t.Fatalf("deadline: %+v", report)
		}
	})
}

func TestDeploymentStartedRouterFailureCleanup(t *testing.T) {
	for _, mode := range []string{"health_error", "malformed_models"} {
		t.Run(mode, func(t *testing.T) {
			f := newDeploymentFixture(t, mode, false)
			req := f.request(t, "start-failure")
			// A race-instrumented child has a one-second exit grace period. Leave
			// room for that owned exit instead of confusing it with cleanup failure.
			req["timeout_seconds"] = 4
			report := f.submit(t, req)
			report = f.wait(t, report.ID, testEnded)
			if report.State != "failed" || report.Cleanup != "stopped_test_owned_router" || f.instance.IsRunning() {
				t.Fatalf("owned failed start not cleaned: state=%s cleanup=%s running=%v", report.State, report.Cleanup, f.instance.IsRunning())
			}
			if f.count("/models/load") != 0 || f.count("/v1/chat/completions") != 0 {
				t.Fatal("work continued after startup failure")
			}
		})
	}
}

func TestDeploymentPauseDuringCleanupCancelsAndFreezesIdentity(t *testing.T) {
	f := newDeploymentFixture(t, "unload_slow", false)
	req := f.request(t, "cleanup-pause")
	req["measured_generations"] = 1
	report := f.submit(t, req)
	during := f.wait(t, report.ID, func(r manager.DeploymentTest) bool { return r.Stage == "cleanup" })
	p := f.cfg.ResourcePolicy
	s := p.Snapshot()
	if _, err := p.Update(false, s.BootID, s.Revision); err != nil {
		t.Fatal(err)
	}
	report = f.wait(t, report.ID, testEnded)
	if report.State != "cancelled" || report.ErrorCode != "ai_paused" {
		t.Fatalf("owner cleanup outcome: %s %s", report.State, report.ErrorCode)
	}
	if report.ProcessGeneration != during.ProcessGeneration || report.LoadGeneration != during.LoadGeneration || report.Revision != during.Revision {
		t.Fatal("tested identity replaced by owner invalidation identity")
	}
}

func TestDeploymentBrowserFixture(t *testing.T) {
	if os.Getenv("LLAMACTL_P3C_BROWSER_FIXTURE") != "1" {
		t.Skip("explicit opt-in only; no model/GPU")
	}
	mode := os.Getenv("LLAMACTL_P3C_FIXTURE_MODE")
	switch mode {
	case "", "success", "slow", "slow_load", "error", "unload_error":
	default:
		t.Fatal("unsupported synthetic fixture mode")
	}
	f := newDeploymentFixture(t, mode, true)
	status, html := f.call(t, "", "GET", "/", nil)
	asset := regexp.MustCompile(`src="(\./assets/[^"<>]+\.js)"`).FindSubmatch(html)
	if status != 200 || len(asset) != 2 {
		t.Fatal("built WebUI index not served")
	}
	status, bundle := f.call(t, "", "GET", strings.TrimPrefix(string(asset[1]), "."), nil)
	if status != 200 || !bytes.Contains(bundle, []byte("synthetic-text-v1")) || !bytes.Contains(bundle, []byte("单配置部署测试")) {
		t.Fatal("embedded WebUI does not contain deployment panel; build final WebUI before compiling fixture")
	}
	t.Logf("Embedded final WebUI bundle SHA256: %x", sha256.Sum256(bundle))
	t.Logf("P3C synthetic browser URL: %s ; management key: synthetic-management-key ; mode: %s ; recovered stopped Router: router / alpha", f.front.URL, mode)
	stop := os.Getenv("LLAMACTL_P3C_STOP_FILE")
	t.Logf("Health: authenticated GET %s/api/v1/instances/router/control ; stop by creating the configured stop file (%s), or send SIGINT to this test binary. Owned manager shutdown runs on return. Expires after 30 minutes.", f.front.URL, stop)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)
	defer signal.Stop(signals)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(30 * time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-signals:
			return
		case <-timer.C:
			return
		case <-t.Context().Done():
			return
		case <-ticker.C:
			if stop != "" {
				if _, err := os.Stat(stop); err == nil {
					return
				}
			}
		}
	}
}
