package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"llamactl/pkg/instance"
)

func TestP3ControllerScopeSurvivesDisabledLegacyAuth(t *testing.T) {
	router, im, inst := p3HTTPFixture(t, true)
	var opts instance.Options
	if err := json.Unmarshal([]byte(`{"backend_type":"llama_cpp","backend_options":{"port":18002}}`), &opts); err != nil {
		t.Fatal(err)
	}
	if _, err := im.CreateInstance("other", &opts); err != nil {
		t.Fatal(err)
	}
	call := func(key, method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if key != "" {
			r.Header.Set("Authorization", "Bearer "+key)
		}
		router.ServeHTTP(w, r)
		return w
	}
	body, _ := json.Marshal(map[string]any{"name": "synthetic-controller", "controller": true, "permission_mode": "per_instance", "permissions": []map[string]any{{"instance_id": inst.ID}}})
	w := call("synthetic-management-key", "POST", "/api/v1/auth/keys/", string(body))
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	var key CreateKeyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &key); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/auth/keys/", "/api/v1/config", "/api/v1/instances/other"} {
		w = call(key.Key, "GET", path, "")
		if w.Code != 401 {
			t.Errorf("controller escaped disabled management group %s: %d %s", path, w.Code, w.Body.String())
		}
		anonymous := call("", "GET", path, "")
		if anonymous.Code != 401 {
			t.Errorf("anonymous request escaped protocol authentication: %s %d", path, anonymous.Code)
		}
	}
	w = call(key.Key, "GET", "/v1/models", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), `"other"`) {
		t.Errorf("disabled inference auth lost scope: %d %s", w.Code, w.Body.String())
	}
	w = call(key.Key, "POST", "/llama-cpp/other/completion", `{"model":"alpha"}`)
	if w.Code != 403 {
		t.Errorf("cross-instance inference accepted: %d %s", w.Code, w.Body.String())
	}
	w = call(key.Key, "GET", "/api/v1/instances/protocol/control", "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = call("", "GET", "/api/v1/instances/protocol/control", "")
	if w.Code != 401 {
		t.Fatal("control became anonymous")
	}
}
