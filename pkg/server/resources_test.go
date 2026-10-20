package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResourcesManagementReadAndControllerBoundary(t *testing.T) {
	router, _, inst := p3HTTPFixture(t, true)
	call := func(key, method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if key != "" {
			r.Header.Set("Authorization", "Bearer "+key)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	w := call("synthetic-management-key", "GET", "/api/v1/resources", "")
	if w.Code != 200 {
		t.Fatalf("resource route: %d %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("resource snapshots must not be cached by intermediaries")
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"sampled_at", "sample_window_ms", "interval_ms", "target_interval_ms", "stale", "throttled", "gaming", "cpu", "memory", "gpu", "processes"} {
		if _, ok := body[field]; !ok {
			t.Errorf("missing %s", field)
		}
	}
	request, _ := json.Marshal(map[string]any{"name": "synthetic-controller", "controller": true, "permission_mode": "per_instance", "permissions": []map[string]any{{"instance_id": inst.ID}}})
	w = call("synthetic-management-key", "POST", "/api/v1/auth/keys/", string(request))
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	var key CreateKeyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &key); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ method, path string }{{"GET", "/api/v1/resources"}, {"PUT", "/api/v1/resource-policy"}, {"POST", "/api/v1/resource-policy/release"}} {
		if got := call(key.Key, tc.method, tc.path, `{}`); got.Code != 401 {
			t.Fatalf("controller widened: %d %s", got.Code, got.Body.String())
		}
	}
	if got := call("", "GET", "/api/v1/resources", ""); got.Code != 401 {
		t.Fatalf("protocol group became anonymous: %d", got.Code)
	}
	if got := call("synthetic-management-key", "PUT", "/api/v1/resources", `{}`); got.Code != 405 {
		t.Fatalf("resources must be read only: %d", got.Code)
	}
}
