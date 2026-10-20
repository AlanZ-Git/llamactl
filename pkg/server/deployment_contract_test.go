package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"llamactl/pkg/instance"
	"llamactl/pkg/manager"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestDeploymentActualAuthScopeIsolation(t *testing.T) {
	f := newDeploymentFixture(t, "slow", false)
	newKey := func(controller bool) string {
		t.Helper()
		status, b := f.call(t, "synthetic-management-key", "POST", "/api/v1/auth/keys/", map[string]any{"name": "synthetic", "controller": controller, "permission_mode": "per_instance", "permissions": []map[string]any{{"instance_id": f.instance.ID}}})
		if status != 201 {
			t.Fatalf("key: %d %s", status, b)
		}
		var key CreateKeyResponse
		if err := json.Unmarshal(b, &key); err != nil {
			t.Fatal(err)
		}
		return key.Key
	}
	owner, other, inference := newKey(true), newKey(true), newKey(false)
	req := f.request(t, "scoped")
	for _, key := range []string{"", inference} {
		status, b := f.call(t, key, "POST", "/api/v1/instances/router/tests", req)
		want := 403
		if key == "" {
			want = 401
		}
		if status != want {
			t.Fatalf("unauthorized test: %d %s", status, b)
		}
	}
	status, b := f.call(t, owner, "POST", "/api/v1/instances/router/tests", req)
	if status != 202 {
		t.Fatalf("controller submit: %d %s", status, b)
	}
	var report manager.DeploymentTest
	_ = json.Unmarshal(b, &report)
	f.wait(t, report.ID, func(r manager.DeploymentTest) bool { return r.AttemptedGenerations == 1 })
	for _, tc := range []struct {
		key  string
		want int
	}{{owner, 200}, {other, 404}, {inference, 403}, {"", 401}, {"synthetic-management-key", 200}} {
		status, b = f.call(t, tc.key, "GET", "/api/v1/instances/router/tests/"+report.ID, nil)
		if status != tc.want {
			t.Fatalf("query scope: %d %s", status, b)
		}
		status, b = f.call(t, tc.key, "GET", "/api/v1/instances/router/tests/"+report.ID+"?format=csv", nil)
		if status != tc.want {
			t.Fatalf("csv scope: %d %s", status, b)
		}
	}
	current := f.instance.ControlSnapshot()
	cancelBody := map[string]any{"boot_id": current.BootID, "expected_revision": current.Revision, "idempotency_key": "scope-cancel"}
	for _, tc := range []struct {
		key  string
		want int
	}{{other, 404}, {inference, 403}, {"", 401}, {owner, 200}} {
		status, b = f.call(t, tc.key, "POST", "/api/v1/instances/router/tests/"+report.ID+"/cancel", cancelBody)
		if status != tc.want {
			t.Fatalf("cancel scope: %d %s", status, b)
		}
	}
	report = f.wait(t, report.ID, testEnded)
	if report.State != "cancelled" {
		t.Fatal(report.State)
	}
	// Fixed-instance permission is checked before exposing even an unrelated report.
	var opts instance.Options
	if err := json.Unmarshal([]byte(`{"backend_type":"llama_cpp"}`), &opts); err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.CreateInstance("other", &opts); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/instances/other/tests/" + report.ID, "/api/v1/instances/other/tests/" + report.ID + "?format=csv"} {
		status, b = f.call(t, owner, "GET", path, nil)
		if status != 403 {
			t.Fatalf("fixed scope escaped: %d %s", status, b)
		}
	}
}

func TestDeploymentActualInvalidBudgetsBeforeSideEffects(t *testing.T) {
	f := newDeploymentFixture(t, "success", false)
	before := f.instance.ControlSnapshot()
	for _, tc := range []struct {
		key   string
		value any
	}{
		{"measured_generations", 4}, {"measured_generations", 0}, {"warmup", 2}, {"warmup", nil}, {"max_output_tokens", 257}, {"max_output_tokens", 0},
		{"timeout_seconds", 601}, {"timeout_seconds", 0}, {"concurrency", 2}, {"seed", -1}, {"fixture_id", "private.txt"}, {"model", "file:///tmp/model"},
		{"prompt", "private text"}, {"patch", map[string]any{"extra_args": "--shell"}}, {"patch", map[string]any{"ubatch": 4096, "batch": 512}}, {"patch", nil},
	} {
		req := f.request(t, "invalid")
		req[tc.key] = tc.value
		status, b := f.call(t, "synthetic-management-key", "POST", "/api/v1/instances/router/tests", req)
		if status != 422 {
			t.Fatalf("invalid %s=%v: %d %s", tc.key, tc.value, status, b)
		}
	}
	req := f.request(t, "invalid")
	raw, _ := json.Marshal(req)
	duplicate := strings.TrimSuffix(string(raw), "}") + `,"model":"alpha"}`
	status, b := f.call(t, "synthetic-management-key", "POST", "/api/v1/instances/router/tests", duplicate)
	if status != 422 {
		t.Fatalf("duplicate: %d %s", status, b)
	}
	after := f.instance.ControlSnapshot()
	if after.Running || after.ActiveOperation != "" || after.Revision != before.Revision || after.ProcessGeneration != before.ProcessGeneration || f.count("/models/load") != 0 {
		t.Fatal("invalid request performed side effects")
	}
	req = f.request(t, "cas")
	req["expected_revision"] = before.Revision + 1
	status, b = f.call(t, "synthetic-management-key", "POST", "/api/v1/instances/router/tests", req)
	if status != 409 || !bytes.Contains(b, []byte("revision_conflict")) {
		t.Fatalf("CAS: %d %s", status, b)
	}
	req["boot_id"] = "previous-boot"
	status, b = f.call(t, "synthetic-management-key", "POST", "/api/v1/instances/router/tests", req)
	if status != 409 || !bytes.Contains(b, []byte("epoch_changed")) {
		t.Fatalf("boot: %d %s", status, b)
	}
}

func TestDeploymentActualDisconnectShutdownAndRestartUnknown(t *testing.T) {
	t.Run("disconnect", func(t *testing.T) {
		f := newDeploymentFixture(t, "success", false)
		req := f.request(t, "disconnected")
		req["measured_generations"] = 1
		raw, _ := json.Marshal(req)
		ctx, cancel := context.WithCancel(context.Background())
		request, _ := http.NewRequestWithContext(ctx, "POST", f.front.URL+"/api/v1/instances/router/tests", bytes.NewReader(raw))
		request.Header.Set("Authorization", "Bearer synthetic-management-key")
		response, err := f.front.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		cancel()
		if err != nil || response.StatusCode != 202 {
			t.Fatalf("accept: %v %s", err, body)
		}
		var report manager.DeploymentTest
		_ = json.Unmarshal(body, &report)
		report = f.wait(t, report.ID, testEnded)
		if report.State != "succeeded" || f.count("/v1/chat/completions") != 1 {
			t.Fatal("disconnect cancelled/replayed run", report.State)
		}
	})
	t.Run("shutdown", func(t *testing.T) {
		f := newDeploymentFixture(t, "slow", false)
		req := f.request(t, "shutdown")
		report := f.submit(t, req)
		f.wait(t, report.ID, func(r manager.DeploymentTest) bool { return r.AttemptedGenerations == 1 })
		f.manager.Shutdown()
		report = f.wait(t, report.ID, testEnded)
		if report.State != "cancelled" || f.instance.IsRunning() || f.instance.ControlSnapshot().ActiveOperation != "" {
			t.Fatal("shutdown did not join owned work")
		}
		other := newDeploymentFixture(t, "success", false)
		status, b := other.call(t, "synthetic-management-key", "GET", "/api/v1/instances/router/tests/"+report.ID, nil)
		if status != 404 || !bytes.Contains(b, []byte("test_unknown_not_resumable")) {
			t.Fatalf("new boot fabricated history: %d %s", status, b)
		}
		status, b = other.call(t, "synthetic-management-key", "POST", "/api/v1/instances/router/tests", req)
		if status != 409 || !bytes.Contains(b, []byte("epoch_changed")) {
			t.Fatalf("new boot replayed request: %d %s", status, b)
		}
		time.Sleep(30 * time.Millisecond)
		if other.count("/models/load") != 0 {
			t.Fatal("restart loaded a model")
		}
	})
}
