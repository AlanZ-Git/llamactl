package server

import "testing"

func TestN1OpenAPIIncludesDurableAndDeliveryRoutes(t *testing.T) {
	doc := readControlAPI(t)
	for _, path := range []string{
		"/api/v1/instances/{name}/inventory",
		"/api/v1/instances/{name}/inventory/scans",
		"/api/v1/instances/{name}/inventory/scans/{scan_id}",
		"/api/v1/instances/{name}/inventory/verify",
		"/api/v1/instances/{name}/inventory/register",
		"/api/v1/instances/{name}/recipes",
		"/api/v1/instances/{name}/recipes/{recipe_id}/versions/{version}",
		"/api/v1/instances/{name}/recipes/{recipe_id}/versions/{version}/publish",
		"/api/v1/instances/{name}/recipes/{recipe_id}/versions/{version}/withdraw",
		"/api/v1/instances/{name}/recipes/{recipe_id}/versions/{version}/export",
		"/api/v1/instances/{name}/recipes/import",
		"/api/v1/instances/{name}/evaluation-runs",
		"/api/v1/instances/{name}/evaluation-runs/{id}",
		"/api/v1/instances/{name}/evaluation-runs/{id}/attempts",
		"/api/v1/instances/{name}/evaluation-runs/{id}/attempts/{seq}",
		"/api/v1/instances/{name}/evaluation-runs/{id}/attempts/{seq}/ack",
		"/api/v1/instances/{name}/evaluation-runs/{id}/analysis",
		"/api/v1/instances/{name}/evaluation-runs/{id}/finish",
		"/api/v1/instances/{name}/evaluation-runs/{id}/cancel",
		"/api/v1/instances/{name}/evaluation-runs/{id}/heartbeat",
		"/api/v1/instances/{name}/service-uses",
		"/api/v1/instances/{name}/service-uses/{id}",
		"/api/v1/instances/{name}/service-uses/{id}/release",
		"/api/v1/instances/{name}/service-uses/{id}/renew",
		"/api/v1/instances/{name}/observation",
		"/api/v1/instances/{name}/capabilities/{key_id}",
	} {
		if object(doc["paths"])[path] == nil {
			t.Fatalf("missing N1 route %s", path)
		}
	}
	f := newN1Fixture(t)
	status, body := f.call(t, "synthetic-management-key", "POST", "/api/v1/instances/router/inventory/scans", map[string]any{"root_id": "fixture"})
	doc.response(t, "POST", "/api/v1/instances/{name}/inventory/scans", status, body)
}
