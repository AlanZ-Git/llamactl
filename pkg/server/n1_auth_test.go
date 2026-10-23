package server

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func n1Key(t *testing.T, f *deploymentFixture, name string, caps []string) (string, int) {
	t.Helper()
	return n1KeyShape(t, f, name, caps, true)
}
func n1KeyShape(t *testing.T, f *deploymentFixture, name string, caps []string, controller bool) (string, int) {
	t.Helper()
	status, b := f.call(t, "synthetic-management-key", "POST", "/api/v1/auth/keys/", map[string]any{"name": name, "controller": controller, "permission_mode": "per_instance", "permissions": []map[string]any{{"instance_id": f.instance.ID, "can_start": false, "can_evict": false}}})
	if status != 201 {
		t.Fatalf("create test key: %d %s", status, b)
	}
	var out struct {
		ID  int    `json:"id"`
		Key string `json:"key"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if err := f.n1Store.SetCapabilities(context.Background(), out.ID, f.instance.ID, caps); err != nil {
		t.Fatal(err)
	}
	return out.Key, out.ID
}
func TestN1CapabilitiesMatrixAndAdminGrant(t *testing.T) {
	f := newN1Fixture(t)
	key, id := n1Key(t, f, "old-controller", nil)
	routes := []struct {
		cap, method, path string
		body              any
		want              int
	}{
		{"discover", "GET", "inventory", nil, 200},
		{"evaluate", "POST", "inventory/verify", map[string]any{}, 422},
		{"recipe_write", "POST", "recipes", map[string]any{}, 422},
		{"recipe_publish", "POST", "recipes/missing/versions/1/publish", map[string]any{}, 404},
		{"service_use", "POST", "service-uses", map[string]any{}, 422},
		{"observe", "GET", "observation", nil, 200},
	}
	for _, route := range routes {
		status, _ := f.call(t, key, route.method, "/api/v1/instances/router/"+route.path, route.body)
		if status != 403 {
			t.Fatalf("old controller inherited %s: %d", route.cap, status)
		}
	}
	for _, route := range routes {
		if err := f.n1Store.SetCapabilities(context.Background(), id, f.instance.ID, []string{route.cap}); err != nil {
			t.Fatal(err)
		}
		status, b := f.call(t, key, route.method, "/api/v1/instances/router/"+route.path, route.body)
		if status != route.want {
			t.Fatalf("explicit %s: %d %s", route.cap, status, b)
		}
		for _, other := range routes {
			if other.cap == route.cap {
				continue
			}
			status, _ = f.call(t, key, other.method, "/api/v1/instances/router/"+other.path, other.body)
			if status != 403 {
				t.Fatalf("%s implied %s", route.cap, other.cap)
			}
		}
	}
	route := fmt.Sprintf("/api/v1/instances/router/capabilities/%d", id)
	status, b := f.call(t, "synthetic-management-key", "PUT", route, map[string]any{"capabilities": []string{"discover", "evaluate"}})
	if status != 200 {
		t.Fatalf("admin grant: %d %s", status, b)
	}
	status, _ = f.call(t, key, "PUT", route, map[string]any{"capabilities": []string{"recipe_publish"}})
	if status != 403 {
		t.Fatal("nonadmin granted capability", status)
	}
}
func TestN1OwnCancelIgnoresStaleRevisionAndRevocationCleans(t *testing.T) {
	f := newN1Fixture(t)
	request, _ := n1PreparedEvaluation(t, f)
	key, id := n1Key(t, f, "evaluator", []string{"evaluate"})
	other, _ := n1Key(t, f, "other-evaluator", []string{"evaluate"})
	status, b := f.call(t, key, "POST", "/api/v1/instances/router/evaluation-runs", request)
	if status != 202 {
		t.Fatalf("accept %d %s", status, b)
	}
	var run map[string]any
	_ = json.Unmarshal(b, &run)
	runID := run["run_id"].(string)
	cancel := n1Envelope(f, "cancel")
	cancel["expected_revision"] = 999999
	status, _ = f.call(t, other, "POST", "/api/v1/instances/router/evaluation-runs/"+runID+"/cancel", cancel)
	if status != 404 {
		t.Fatal("foreign cancel", status)
	}
	status, b = f.call(t, key, "POST", "/api/v1/instances/router/evaluation-runs/"+runID+"/cancel", cancel)
	if status != 200 {
		t.Fatalf("safe stale cancel: %d %s", status, b)
	}
	n1Wait(t, f, "evaluation-runs/"+runID, func(v map[string]any) bool { return v["frozen_at"] != nil })
	request["idempotency_key"] = "revoked-run"
	status, b = f.call(t, key, "POST", "/api/v1/instances/router/evaluation-runs", request)
	if status != 202 {
		t.Fatalf("accept second: %d %s", status, b)
	}
	_ = json.Unmarshal(b, &run)
	runID = run["run_id"].(string)
	if err := f.n1Store.SetCapabilities(context.Background(), id, f.instance.ID, nil); err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(3 * time.Second)
	for time.Now().Before(until) {
		r, err := f.n1Store.Get(context.Background(), runID)
		if err == nil && r.State == "cancelled" && r.Cleanup == "released" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("capability revocation did not proactively clean")
}
func n1PreparedEvaluation(t *testing.T, f *deploymentFixture) (map[string]any, map[string]any) {
	t.Helper()
	scan := n1Call(t, f, "POST", "inventory/scans", map[string]any{"root_id": "fixture"}, 200)
	candidate := scan["candidates"].([]any)[0].(map[string]any)
	req := n1Envelope(f, "verify-helper")
	req["scan_id"] = scan["scan_id"]
	req["candidate_id"] = candidate["candidate_id"]
	a := n1Call(t, f, "POST", "inventory/verify", req, 200)
	req = n1Envelope(f, "evaluation-helper")
	req["candidates"] = []any{map[string]any{"candidate_id": "one", "artifact_id": a["artifact_id"], "artifact_manifest_digest": a["manifest_digest"], "deployment": map[string]any{"gpu_layers": 0}, "request": map[string]any{"parameters": map[string]any{"max_tokens": 8}, "system_prefix": "", "template_version": "synthetic"}}}
	req["wall_seconds"] = 60
	req["attempt_seconds"] = 20
	req["cleanup_seconds"] = 5
	req["budget"] = map[string]any{"requests": 1, "tokens": 8, "bytes": 65536}
	req["notice_ref"] = "synthetic-delivered"
	req["not_before"] = time.Now().Add(-time.Second).UnixMilli()
	req["fixture_version"] = "synthetic"
	req["purpose"] = "synthetic_only"
	req["parameter_ranges"] = map[string]any{"gpu_layers": []int{0, 0}}
	return req, a
}
