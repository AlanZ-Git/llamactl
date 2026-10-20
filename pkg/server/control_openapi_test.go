package server

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"
	"llamactl/pkg/instance"
	"llamactl/pkg/manager"
)

// Only the schema vocabulary used by this small additive contract is needed.
// The tests validate actual HTTP bodies, not hand-maintained response fixtures.
type controlAPIDoc map[string]any

func readControlAPI(t *testing.T) controlAPIDoc {
	t.Helper()
	b, err := os.ReadFile("../../docs/control-openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := yaml.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	doc := controlAPIDoc(raw)
	if doc["openapi"] != "3.0.3" {
		t.Fatal("unexpected OpenAPI version")
	}
	return doc
}
func object(v any) map[string]any { m, _ := v.(map[string]any); return m }
func (d controlAPIDoc) resolve(s map[string]any) map[string]any {
	if ref, ok := s["$ref"].(string); ok {
		var value any = map[string]any(d)
		for _, key := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			value = object(value)[key]
		}
		return object(value)
	}
	return s
}
func schemaNumber(v any) float64 {
	switch n := v.(type) {
	case int:
		return float64(n)
	case float64:
		return n
	}
	return math.NaN()
}
func (d controlAPIDoc) validate(s map[string]any, v any) error {
	s = d.resolve(s)
	if s == nil {
		return fmt.Errorf("unresolved schema")
	}
	if v == nil {
		if s["nullable"] == true {
			return nil
		}
		return fmt.Errorf("unexpected null")
	}
	if variants, ok := s["oneOf"].([]any); ok {
		matches := 0
		for _, variant := range variants {
			if d.validate(object(variant), v) == nil {
				matches++
			}
		}
		if matches != 1 {
			return fmt.Errorf("oneOf matched %d variants", matches)
		}
	}
	if variants, ok := s["anyOf"].([]any); ok {
		matches := 0
		for _, variant := range variants {
			if d.validate(object(variant), v) == nil {
				matches++
			}
		}
		if matches == 0 {
			return fmt.Errorf("anyOf matched no variants")
		}
	}
	if excluded, ok := s["not"].(map[string]any); ok && d.validate(excluded, v) == nil {
		return fmt.Errorf("excluded schema matched")
	}
	if choices, ok := s["enum"].([]any); ok {
		found := false
		for _, choice := range choices {
			if reflect.DeepEqual(choice, v) || (!math.IsNaN(schemaNumber(choice)) && schemaNumber(choice) == schemaNumber(v)) {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("value %v not in enum", v)
		}
	}
	switch s["type"] {
	case "object":
		if _, ok := v.(map[string]any); !ok {
			return fmt.Errorf("expected object")
		}
	case "array":
		if _, ok := v.([]any); !ok {
			return fmt.Errorf("expected array")
		}
	case "string":
		if _, ok := v.(string); !ok {
			return fmt.Errorf("expected string")
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("expected boolean")
		}
	case "number":
		if _, ok := v.(float64); !ok {
			return fmt.Errorf("expected number")
		}
	case "integer":
		n, ok := v.(float64)
		if !ok || n != math.Trunc(n) {
			return fmt.Errorf("expected integer")
		}
	}
	if m, ok := v.(map[string]any); ok {
		if required, ok := s["required"].([]any); ok {
			for _, key := range required {
				if _, found := m[key.(string)]; !found {
					return fmt.Errorf("missing %s", key)
				}
			}
		}
		props := object(s["properties"])
		for key, value := range m {
			property, declared := props[key]
			if !declared {
				if s["additionalProperties"] == false {
					return fmt.Errorf("unknown property %s", key)
				}
				property = s["additionalProperties"]
			}
			if child := object(property); child != nil {
				if err := d.validate(child, value); err != nil {
					return fmt.Errorf("%s: %w", key, err)
				}
			}
		}
	}
	if a, ok := v.([]any); ok {
		if max, found := s["maxItems"]; found && len(a) > int(schemaNumber(max)) {
			return fmt.Errorf("too many items")
		}
		if items := object(s["items"]); items != nil {
			for _, item := range a {
				if err := d.validate(items, item); err != nil {
					return err
				}
			}
		}
	}
	if str, ok := v.(string); ok {
		if min, found := s["minLength"]; found && len(str) < int(schemaNumber(min)) {
			return fmt.Errorf("string too short")
		}
		if max, found := s["maxLength"]; found && len(str) > int(schemaNumber(max)) {
			return fmt.Errorf("string too long")
		}
		if pattern, ok := s["pattern"].(string); ok && !regexp.MustCompile(pattern).MatchString(str) {
			return fmt.Errorf("pattern mismatch")
		}
		if s["format"] == "date-time" {
			if _, err := time.Parse(time.RFC3339Nano, str); err != nil {
				return err
			}
		}
	}
	if n, ok := v.(float64); ok {
		if min, found := s["minimum"]; found && n < schemaNumber(min) {
			return fmt.Errorf("below minimum")
		}
		if max, found := s["maximum"]; found && n > schemaNumber(max) {
			return fmt.Errorf("above maximum")
		}
	}
	return nil
}
func (d controlAPIDoc) response(t *testing.T, method, path string, status int, body []byte) {
	t.Helper()
	endpoint := object(object(d["paths"])[path])
	operation := object(endpoint[strings.ToLower(method)])
	response := d.resolve(object(object(operation["responses"])[strconv.Itoa(status)]))
	if response == nil {
		t.Fatalf("undocumented %s %s status %d: %s", method, path, status, body)
	}
	schema := object(object(object(response["content"])["application/json"])["schema"])
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatal(err)
	}
	if err := d.validate(schema, value); err != nil {
		t.Fatalf("%s %s response: %v: %s", method, path, err, body)
	}
}

func TestP3OpenAPIRoutesSchemasAndHTTP(t *testing.T) {
	doc := readControlAPI(t)
	router, im, _ := p3HTTPFixture(t)
	routes := map[string]bool{}
	err := chi.Walk(router.(*chi.Mux), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if strings.Contains(route, "/control") || strings.Contains(route, "/operations") || strings.Contains(route, "/tests") {
			routes[method+" "+route] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for path, item := range object(doc["paths"]) {
		for method := range object(item) {
			if method == "parameters" {
				continue
			}
			key := strings.ToUpper(method) + " " + path
			if !routes[key] {
				t.Errorf("documented route not registered: %s", key)
			}
			delete(routes, key)
		}
	}
	if len(routes) != 0 {
		t.Fatalf("undocumented control routes: %v", routes)
	}
	schemas := object(object(doc["components"])["schemas"])
	patch := object(object(schemas["ControlPatch"])["properties"])
	if len(patch) != len(instance.ControlParameters()) {
		t.Fatal("parameter inventory drift")
	}
	for name, spec := range instance.ControlParameters() {
		p := object(patch[name])
		if p["type"] != spec.Type || p["nullable"] != spec.Nullable {
			t.Fatalf("parameter type/null drift: %s", name)
		}
		if spec.Type == "integer" && (schemaNumber(p["minimum"]) != float64(spec.Minimum) || schemaNumber(p["maximum"]) != float64(spec.Maximum)) {
			t.Fatalf("parameter range drift: %s", name)
		}
		if len(spec.Enum) > 0 {
			got, _ := json.Marshal(p["enum"])
			want, _ := json.Marshal(spec.Enum)
			if string(got) != string(want) {
				t.Fatalf("parameter enum drift: %s", name)
			}
		}
	}
	call := func(method, route, body string, want int) []byte {
		t.Helper()
		w := httptest.NewRecorder()
		router.ServeHTTP(w, admittedRequest(method, route, body))
		if w.Code != want {
			t.Fatalf("%s %s: %d %s", method, route, w.Code, w.Body.String())
		}
		path := strings.Replace(route, "/protocol/", "/{name}/", 1)
		if idx := strings.Index(path, "/operations/"); idx >= 0 {
			path = path[:idx] + "/operations/{id}"
		}
		doc.response(t, method, path, w.Code, w.Body.Bytes())
		return w.Body.Bytes()
	}
	b := call("GET", "/api/v1/instances/protocol/control", "", 200)
	var snapshot instance.ControlSnapshot
	_ = json.Unmarshal(b, &snapshot)
	recovery, _ := json.Marshal(map[string]any{"boot_id": snapshot.BootID, "expected_revision": snapshot.Revision, "verified_stopped": true})
	call("PUT", "/api/v1/instances/protocol/control", string(recovery), 200)
	base := map[string]any{"action": "stop", "boot_id": snapshot.BootID, "expected_revision": snapshot.Revision, "idempotency_key": "contract"}
	body, _ := json.Marshal(base)
	b = call("POST", "/api/v1/instances/protocol/operations", string(body), 202)
	var op manager.ControlOperation
	_ = json.Unmarshal(b, &op)
	call("GET", "/api/v1/instances/protocol/operations/"+op.ID, "", 200)
	// Exercise terminal HTTP schema, rather than accepting only an initial 202 body.
	until := time.Now().Add(time.Second)
	for time.Now().Before(until) {
		op, err = im.(manager.ControlManager).GetControlOperation("protocol", "", op.ID, true)
		if err != nil {
			t.Fatal(err)
		}
		if op.EndedAt != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if op.EndedAt == nil {
		t.Fatal("stop not terminal")
	}
	call("GET", "/api/v1/instances/protocol/operations/"+op.ID, "", 200)
	call("GET", "/api/v1/instances/protocol/operations/missing", "", 404)
	for _, invalid := range []string{`{}`, `{"action":"stop","patch":{}}`, strings.Replace(string(body), `"stop"`, `"erase"`, 1), strings.TrimSuffix(string(body), "}") + `,"extra_args":[]}`} {
		var value any
		_ = json.Unmarshal([]byte(invalid), &value)
		if doc.validate(object(schemas["OperationRequest"]), value) == nil {
			t.Fatal("invalid request passes OpenAPI", invalid)
		}
		call("POST", "/api/v1/instances/protocol/operations", invalid, 422)
	}
	for _, action := range []string{"apply", "start", "stop", "load", "unload"} {
		v := map[string]any{"action": action, "boot_id": "boot", "expected_revision": float64(0), "idempotency_key": "synthetic"}
		if action == "apply" || action == "load" {
			v["model"] = "alpha"
		}
		if action == "apply" {
			v["patch"] = map[string]any{"threads": float64(0), "flash_attention": false}
		}
		if err := doc.validate(object(schemas["OperationRequest"]), v); err != nil {
			t.Fatalf("valid %s request: %v", action, err)
		}
	}
}
