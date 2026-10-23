package server

import (
	"context"
	"encoding/json"
	"testing"
)

func TestN1RejectsNullUnknownAndRangesBeforeAcceptance(t *testing.T) {
	f := newN1Fixture(t)
	valid, _ := n1PreparedEvaluation(t, f)
	for name, mutate := range map[string]func(map[string]any){
		"null-retain":   func(v map[string]any) { v["retain_outputs"] = nil },
		"unknown":       func(v map[string]any) { v["raw_backend_url"] = "http://example.invalid" },
		"range":         func(v map[string]any) { v["parameter_ranges"] = map[string]any{"gpu_layers": []int{1, 2}} },
		"zero-requests": func(v map[string]any) { v["budget"] = map[string]any{"requests": 0, "tokens": 8, "bytes": 65536} },
	} {
		raw, _ := json.Marshal(valid)
		var req map[string]any
		_ = json.Unmarshal(raw, &req)
		req["idempotency_key"] = name
		mutate(req)
		status, b := f.call(t, "synthetic-management-key", "POST", "/api/v1/instances/router/evaluation-runs", req)
		if status != 422 {
			t.Fatalf("%s accepted: %d %s", name, status, b)
		}
	}
	rows, err := f.n1Store.List(context.Background(), "", "router", true)
	if err != nil || len(rows) != 0 {
		t.Fatalf("invalid request persisted acceptance: %d %v", len(rows), err)
	}
}
func TestN1RegisterLostResponseIsIdempotent(t *testing.T) {
	f := newN1Fixture(t)
	_, artifact := n1PreparedEvaluation(t, f)
	req := n1Envelope(f, "register-idempotent")
	req["artifact_id"] = artifact["artifact_id"]
	first := n1Call(t, f, "POST", "inventory/register", req, 200)
	second := n1Call(t, f, "POST", "inventory/register", req, 200)
	if first["revision"] != second["revision"] {
		t.Fatal("registration applied twice")
	}
}
