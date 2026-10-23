package server

import (
	"encoding/json"
	"llamactl/pkg/database"
	"testing"
)

func TestN1OwnCancelWhilePausedAndExhausted(t *testing.T) {
	f := newN1Fixture(t)
	req, _ := n1PreparedEvaluation(t, f)
	key, _ := n1KeyShape(t, f, "safe-evaluator", []string{"evaluate", "discover"}, false)
	other, _ := n1KeyShape(t, f, "other-evaluator", []string{"evaluate"}, false)
	status, b := f.call(t, key, "POST", "/api/v1/instances/router/evaluation-runs", req)
	if status != 202 {
		t.Fatalf("%d %s", status, b)
	}
	var accepted map[string]any
	json.Unmarshal(b, &accepted)
	id := accepted["run_id"].(string)
	attempt := n1Envelope(f, "safe-attempt")
	attempt["seq"] = 1
	attempt["candidate_id"] = "one"
	attempt["fixture_id"] = "synthetic"
	messages := []map[string]string{{"role": "user", "content": "synthetic"}}
	raw, _ := json.Marshal(messages)
	attempt["messages"] = messages
	attempt["fixture_digest"] = database.N1Digest(raw)
	status, b = f.call(t, key, "POST", "/api/v1/instances/router/evaluation-runs/"+id+"/attempts", attempt)
	if status != 202 {
		t.Fatalf("%d %s", status, b)
	}
	n1Wait(t, f, "evaluation-runs/"+id+"/attempts/1", func(v map[string]any) bool { return v["receipt"] != nil })
	attempt["idempotency_key"] = "exhausted"
	attempt["seq"] = 2
	attempt["expected_revision"] = f.instance.ControlSnapshot().Revision
	status, b = f.call(t, key, "POST", "/api/v1/instances/router/evaluation-runs/"+id+"/attempts", attempt)
	if status != 409 {
		t.Fatalf("expected exhausted rejection: %d %s", status, b)
	}
	var failure map[string]any
	json.Unmarshal(b, &failure)
	if failure["error"] != "budget_exhausted" {
		t.Fatal(failure)
	}
	policy := f.cfg.ResourcePolicy.Snapshot()
	if _, err := f.cfg.ResourcePolicy.Update(false, policy.BootID, policy.Revision); err != nil {
		t.Fatal(err)
	}
	cancel := n1Envelope(f, "safe-cancel")
	cancel["expected_revision"] = 999999
	status, b = f.call(t, other, "POST", "/api/v1/instances/router/evaluation-runs/"+id+"/cancel", cancel)
	if status != 404 {
		t.Fatalf("foreign cancel: %d %s", status, b)
	}
	status, b = f.call(t, key, "POST", "/api/v1/instances/router/evaluation-runs/"+id+"/cancel", cancel)
	if status != 200 {
		t.Fatalf("own paused exhausted cancel: %d %s", status, b)
	}
	n1Wait(t, f, "evaluation-runs/"+id, func(v map[string]any) bool { return v["frozen_at"] != nil })
}
