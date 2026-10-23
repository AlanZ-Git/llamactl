package server

import (
	"encoding/json"
	"llamactl/pkg/database"
	"testing"
)

func TestN1ForeignImportSameSnapshotIsIdempotent(t *testing.T) {
	f := newN1Fixture(t)
	eval, artifact := n1PreparedEvaluation(t, f)
	candidate := eval["candidates"].([]any)[0].(map[string]any)
	doc := map[string]any{"description": "Synthetic foreign import", "task_tags": []string{"synthetic"}, "artifact_id": artifact["artifact_id"], "artifact_manifest_digest": artifact["manifest_digest"], "engine_identity": "synthetic-engine-v1", "deployment": candidate["deployment"], "request": candidate["request"], "conclusion_level": "unverified", "evidence": nil}
	recipe := n1Call(t, f, "POST", "recipes", doc, 201)
	snapshot := n1Call(t, f, "GET", "recipes/"+recipe["recipe_id"].(string)+"/versions/1/export", nil, 200)
	if names, ok := snapshot["recipe"].(map[string]any)["portable_members"].([]any); !ok || len(names) != 1 || names[0] != "synthetic.gguf" {
		t.Fatal("export lacks portable member names", snapshot)
	}
	snapshot["recipe_id"] = "synthetic-foreign-id"
	req := map[string]any{"dry_run": true, "snapshot": snapshot}
	preview := n1Call(t, f, "POST", "recipes/import", req, 200)
	if preview["publication_state"] != "draft" {
		t.Fatal(preview)
	}
	req["dry_run"] = false
	first := n1Call(t, f, "POST", "recipes/import", req, 201)
	if first["recipe"].(map[string]any)["foreign"] != true {
		t.Fatal("import inherited local validation")
	}
	again := n1Call(t, f, "POST", "recipes/import", req, 200)
	if again["idempotent"] != true {
		t.Fatal("repeat import not idempotent", again)
	}
	n1Call(t, f, "POST", "recipes/synthetic-foreign-id/versions/1/publish", map[string]any{"expected_version": 1, "conclusion_level": "unverified"}, 409)
	snapshot["recipe"].(map[string]any)["description"] = "conflicting synthetic description"
	payload, _ := json.Marshal(snapshot["recipe"])
	snapshot["recipe_digest"] = database.N1Digest(payload)
	writer, _ := n1Key(t, f, "write-without-discover", []string{"recipe_write"})
	status, body := f.call(t, writer, "POST", "/api/v1/instances/router/recipes/import", req)
	if status != 409 {
		t.Fatalf("conflict %d %s", status, body)
	}
	var conflict map[string]any
	_ = json.Unmarshal(body, &conflict)
	if _, leaked := conflict["existing_recipe_digest"]; leaked {
		t.Fatal("write-only scope learned existing recipe identity", conflict)
	}
}
