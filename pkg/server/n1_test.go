package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
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
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func n1SyntheticGGUF() []byte {
	var b bytes.Buffer
	put := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	str := func(s string) { put(uint64(len(s))); b.WriteString(s) }
	b.WriteString("GGUF")
	put(uint32(3))
	put(uint64(1))
	put(uint64(1))
	str("general.architecture")
	put(uint32(8))
	str("llama")
	str("weight")
	put(uint32(1))
	put(uint64(1))
	put(uint32(0))
	put(uint64(0))
	for b.Len()%32 != 0 {
		b.WriteByte(0)
	}
	put(float32(1))
	return b.Bytes()
}
func newN1Fixture(t *testing.T) *deploymentFixture {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "synthetic.gguf")
	if err := os.WriteFile(path, n1SyntheticGGUF(), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.AppConfig{DataDir: t.TempDir()}
	cfg.Instances.InstancesDir = t.TempDir()
	cfg.Instances.LogsDir = t.TempDir()
	cfg.Instances.PortRange = [2]int{1024, 65535}
	cfg.Instances.MaxInstances = -1
	cfg.Auth.ManagementKeys = []string{"synthetic-management-key"}
	cfg.Auth.RequireManagementAuth = true
	cfg.Auth.RequireInferenceAuth = true
	cfg.Backends.LlamaCpp.Command = os.Args[0]
	cfg.N1 = config.N1Config{Instance: "router", Roots: map[string]string{"fixture": root}, EngineIdentity: "synthetic-engine-v1"}
	db, err := database.Open(&database.Config{Path: filepath.Join(t.TempDir(), "synthetic.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err = database.RunMigrations(db); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	var opts instance.Options
	if err = json.Unmarshal([]byte(`{"controlled":true,"environment_policy":"isolated","backend_type":"llama_cpp","backend_options":{"no_models_autoload":true,"models_max":1}}`), &opts); err != nil {
		t.Fatal(err)
	}
	opts.Control = &instance.ControlConfig{Models: map[string]instance.ControlModel{"alpha": {Path: path}}}
	opts.BackendOptions.SetPort(port)
	events := filepath.Join(t.TempDir(), "synthetic-events")
	opts.Environment = map[string]string{"P3_SYNTHETIC_ENGINE": "1", "P3_SYNTHETIC_PORT": strconv.Itoa(port), "N1_SYNTHETIC_MODELS": "1", "P3_SYNTHETIC_EVENTS": events, "N1_SYNTHETIC_LOAD_FAILURE_FILE": events + ".load-failure"}
	descriptor, err := instance.NewDescriptor("router", cfg, &opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Save(descriptor); err != nil {
		t.Fatal(err)
	}
	im := manager.New(cfg, db)
	t.Cleanup(im.Shutdown)
	cm := im.(manager.ControlManager)
	s, _ := cm.ControlStatus("router")
	if _, err = cm.RecoverControl(context.Background(), "router", s.BootID, s.Revision); err != nil {
		t.Fatal(err)
	}
	inst, _ := im.GetInstance("router")
	front := httptest.NewServer(SetupRouter(NewHandler(im, nil, *cfg, db, nil)))
	t.Cleanup(front.Close)
	front.Client().Timeout = 10 * time.Second
	return &deploymentFixture{front: front, manager: im, instance: inst, cfg: cfg, events: events, n1Store: db.N1Store()}
}
func n1Call(t *testing.T, f *deploymentFixture, method, path string, body any, want int) map[string]any {
	t.Helper()
	status, b := f.call(t, "synthetic-management-key", method, "/api/v1/instances/router/"+path, body)
	if status != want {
		t.Fatalf("%s %s: %d want %d: %s", method, path, status, want, b)
	}
	parts := strings.Split(strings.Split(path, "?")[0], "/")
	if len(parts) > 2 && parts[0] == "inventory" && parts[1] == "scans" {
		parts[2] = "{scan_id}"
	}
	if len(parts) > 3 && parts[0] == "recipes" {
		parts[1] = "{recipe_id}"
		parts[3] = "{version}"
	}
	if len(parts) > 1 && (parts[0] == "evaluation-runs" || parts[0] == "service-uses") {
		parts[1] = "{id}"
		if len(parts) > 3 && parts[2] == "attempts" {
			parts[3] = "{seq}"
		}
	}
	readControlAPI(t).response(t, method, "/api/v1/instances/{name}/"+strings.Join(parts, "/"), status, b)
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func n1Envelope(f *deploymentFixture, key string) map[string]any {
	s := f.instance.ControlSnapshot()
	return map[string]any{"boot_id": s.BootID, "expected_revision": s.Revision, "idempotency_key": key}
}
func n1Wait(t *testing.T, f *deploymentFixture, path string, done func(map[string]any) bool) map[string]any {
	t.Helper()
	until := time.Now().Add(15 * time.Second)
	for time.Now().Before(until) {
		out := n1Call(t, f, "GET", path, nil, 200)
		if done(out) {
			return out
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("N1 timeout", path)
	return nil
}

// This intentionally has an unknown tensor type and no weight payload. Inventory
// may identify/register it, but only an engine load can establish loadability.
func n1IdentityOnlyGGUF() []byte {
	body := n1SyntheticGGUF()
	typ := bytes.Index(body, []byte("weight")) + len("weight") + 4 + 8
	binary.LittleEndian.PutUint32(body[typ:typ+4], 0xffffffff)
	return body[:typ+4+8]
}

func n1RegisterIdentityHTTP(t *testing.T, f *deploymentFixture, body []byte) map[string]any {
	t.Helper()
	before := f.instance.ControlSnapshot()
	path := filepath.Join(f.cfg.N1.Roots["fixture"], "synthetic.gguf")
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	scan := n1Call(t, f, "POST", "inventory/scans", map[string]any{"root_id": "fixture", "filename": "synthetic.gguf"}, 200)
	candidates := scan["candidates"].([]any)
	if len(candidates) != 1 || scan["partial"] != false || scan["filename"] != "synthetic.gguf" {
		t.Fatal(scan)
	}
	candidate := candidates[0].(map[string]any)
	if candidate["name"] != "synthetic.gguf" || candidate["file_state"] != "discovered" {
		t.Fatal(candidate)
	}
	page := n1Call(t, f, "GET", "inventory/scans/"+scan["scan_id"].(string)+"?limit=1", nil, 200)
	if page["filename"] != scan["filename"] || page["next"] != nil || len(page["candidates"].([]any)) != 1 {
		t.Fatal(page)
	}
	req := n1Envelope(f, "identity-verify")
	req["scan_id"], req["candidate_id"] = scan["scan_id"], candidate["candidate_id"]
	artifact := n1Call(t, f, "POST", "inventory/verify", req, 200)
	if artifact["identity_level"] != "verified" || artifact["file_state"] != "complete" {
		t.Fatal(artifact)
	}
	members := artifact["ordered_members"].([]any)
	if len(members) != 1 {
		t.Fatal(members)
	}
	digest := sha256.Sum256(body)
	member := members[0].(map[string]any)
	if member["sha256"] != hex.EncodeToString(digest[:]) || member["size"] != float64(len(body)) {
		t.Fatal(member)
	}
	cached := n1Call(t, f, "POST", "inventory/verify", req, 200)
	if cached["artifact_id"] != artifact["artifact_id"] || cached["manifest_digest"] != artifact["manifest_digest"] {
		t.Fatal("identity cache changed", cached)
	}
	req = n1Envelope(f, "identity-register")
	req["artifact_id"] = artifact["artifact_id"]
	registered := n1Call(t, f, "POST", "inventory/register", req, 200)
	replay := n1Call(t, f, "POST", "inventory/register", req, 200)
	if registered["state"] != "registered" || replay["operation_id"] != registered["operation_id"] || replay["revision"] != registered["revision"] {
		t.Fatal(registered, replay)
	}
	after := f.instance.ControlSnapshot()
	if after.Revision != before.Revision+1 || after.ProcessGeneration != before.ProcessGeneration || after.LoadGeneration != before.LoadGeneration || after.ReadyToken != "" || f.instance.IsRunning() {
		t.Fatalf("registration loaded or changed runtime: before=%+v after=%+v", before, after)
	}
	if _, err := os.Stat(f.events); !os.IsNotExist(err) {
		t.Fatal("registration contacted fake engine", err)
	}
	id := artifact["artifact_id"].(string)
	if f.instance.GetOptions().Control.Models[id].Path != path {
		t.Fatal("registered mapping missing")
	}
	stored, err := f.n1Store.Artifact(context.Background(), id)
	if err != nil || stored.ManifestDigest != artifact["manifest_digest"] || stored.IdentityLevel != "verified" {
		t.Fatalf("persisted identity: %+v %v", stored, err)
	}
	return artifact
}

func TestN1FilenameOpenAPIRequestSchema(t *testing.T) {
	doc := readControlAPI(t)
	endpoint := object(object(doc["paths"])["/api/v1/instances/{name}/inventory/scans"])
	operation := object(endpoint["post"])
	body := object(operation["requestBody"])
	schema := object(object(object(body["content"])["application/json"])["schema"])
	if err := doc.validate(schema, map[string]any{"root_id": "fixture"}); err != nil {
		t.Fatal("missing filename rejected", err)
	}
	for _, name := range []string{"", "x", "ab", "synthetic.gguf", "model.v1.gguf", "model with spaces.gguf"} {
		if err := doc.validate(schema, map[string]any{"root_id": "fixture", "filename": name}); err != nil {
			t.Errorf("valid %q: %v", name, err)
		}
	}
	for _, name := range []any{nil, float64(1), ".", "..", "../model.gguf", "dir/model.gguf", `dir\model.gguf`, "/model.gguf", `C:\model.gguf`, "model.gguf:stream", ".hidden", "model.gguf.", "model.gguf ", "nul\x00.gguf"} {
		if err := doc.validate(schema, map[string]any{"root_id": "fixture", "filename": name}); err == nil {
			t.Errorf("unsafe filename accepted by schema: %q", name)
		}
	}
}

func TestN1HTTPFilenameScanRegistrationContract(t *testing.T) {
	f := newN1Fixture(t)
	root := f.cfg.N1.Roots["fixture"]
	for n := 0; n < 25; n++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("other-%02d.gguf", n)), []byte("malformed sibling"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	artifact := n1RegisterIdentityHTTP(t, f, n1IdentityOnlyGGUF())
	for _, body := range []map[string]any{{"root_id": "fixture"}, {"root_id": "fixture", "filename": ""}} {
		full := n1Call(t, f, "POST", "inventory/scans", body, 200)
		if len(full["candidates"].([]any)) != 26 || full["filename"] != nil {
			t.Fatal("root scan changed", full)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "directory.gguf"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".", "..", "../synthetic.gguf", "/synthetic.gguf", `C:\synthetic.gguf`, "C:synthetic.gguf", `dir\synthetic.gguf`, "dir/synthetic.gguf", "synthetic.gguf:stream", ".hidden", "synthetic.gguf.", "synthetic.gguf ", "nul\x00.gguf", "directory.gguf"} {
		out := n1Call(t, f, "POST", "inventory/scans", map[string]any{"root_id": "fixture", "filename": name}, 422)
		if out["error"] != "invalid_config" {
			t.Fatal(out)
		}
	}
	for _, body := range []string{
		`{"root_id":"fixture","filename":null}`,
		`{"root_id":"fixture","filename":12}`,
		`{"root_id":"fixture","filename":"synthetic.gguf","filename":"other.gguf"}`,
		`{"root_id":"fixture","filename":"synthetic.gguf","path":"other.gguf"}`,
	} {
		n1Call(t, f, "POST", "inventory/scans", body, 422)
	}
	for name, state := range map[string]string{"missing.gguf": "unreadable", "other-00.gguf": "invalid"} {
		s := n1Call(t, f, "POST", "inventory/scans", map[string]any{"root_id": "fixture", "filename": name}, 200)
		cs := s["candidates"].([]any)
		if len(cs) != 1 || cs[0].(map[string]any)["file_state"] != state {
			t.Fatal(s)
		}
		req := n1Envelope(f, "negative-verify")
		req["scan_id"], req["candidate_id"] = s["scan_id"], cs[0].(map[string]any)["candidate_id"]
		n1Call(t, f, "POST", "inventory/verify", req, 409)
	}
	t.Run("blocked_link", func(t *testing.T) {
		if err := os.Symlink("synthetic.gguf", filepath.Join(root, "link.gguf")); err != nil {
			t.Skip(err)
		}
		s := n1Call(t, f, "POST", "inventory/scans", map[string]any{"root_id": "fixture", "filename": "link.gguf"}, 200)
		cs := s["candidates"].([]any)
		if len(cs) != 1 || cs[0].(map[string]any)["file_state"] != "blocked_link" {
			t.Fatal(s)
		}
		req := n1Envelope(f, "link-verify")
		req["scan_id"], req["candidate_id"] = s["scan_id"], cs[0].(map[string]any)["candidate_id"]
		n1Call(t, f, "POST", "inventory/verify", req, 409)
	})
	n1Call(t, f, "POST", "inventory/scans", map[string]any{"root_id": "unapproved", "filename": "synthetic.gguf"}, 409)
	// Discovery still requires authentication; filename is not an authority bypass.
	status, _ := f.call(t, "", "POST", "/api/v1/instances/router/inventory/scans", map[string]any{"root_id": "fixture", "filename": "synthetic.gguf"})
	if status != 401 {
		t.Fatal("unauthenticated scan", status)
	}
	before := f.instance.ControlSnapshot()
	if err := os.WriteFile(filepath.Join(root, "synthetic.gguf"), []byte("identity changed"), 0600); err != nil {
		t.Fatal(err)
	}
	req := n1Envelope(f, "changed-register")
	req["artifact_id"] = artifact["artifact_id"]
	n1Call(t, f, "POST", "inventory/register", req, 409)
	if f.instance.ControlSnapshot().Revision != before.Revision || f.instance.IsRunning() {
		t.Fatal("changed artifact registered or started")
	}
}

func TestN1HTTPFilenameContinuationContract(t *testing.T) {
	f := newN1Fixture(t)
	// Initialize with a live context, then force a bounded partial cursor without
	// filling a directory or depending on HTTP cancellation timing.
	n1Call(t, f, "GET", "inventory", nil, 200)
	for _, selection := range []string{"", "synthetic.gguf"} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		body, _ := json.Marshal(map[string]any{"root_id": "fixture", "filename": selection})
		status, out, err := f.manager.(manager.N1Manager).N1Request(ctx, "router", "admin:test", true, "POST", "inventory/scans", nil, body)
		if err != nil || status != 200 {
			t.Fatal(status, err)
		}
		encoded, _ := json.Marshal(out)
		var scan map[string]any
		if err = json.Unmarshal(encoded, &scan); err != nil {
			t.Fatal(err)
		}
		if scan["partial"] != true || scan["continuation"] == nil {
			t.Fatal(scan)
		}
		req := map[string]any{"scan_id": scan["scan_id"], "continuation": scan["continuation"], "filename": "other.gguf"}
		n1Call(t, f, "POST", "inventory/scans", req, 422)
		delete(req, "filename")
		next := n1Call(t, f, "POST", "inventory/scans", req, 200)
		cs := next["candidates"].([]any)
		if next["partial"] != false || len(cs) != 1 || cs[0].(map[string]any)["name"] != "synthetic.gguf" {
			t.Fatal(next)
		}
		if selection != "" && next["filename"] != selection {
			t.Fatal("continuation lost target", next)
		}
	}
}

func TestN1HTTPIdentityRegistrationThenLoadFailure(t *testing.T) {
	f := newN1Fixture(t)
	artifact := n1RegisterIdentityHTTP(t, f, n1IdentityOnlyGGUF())
	if err := os.WriteFile(f.events+".load-failure", []byte("synthetic load rejection"), 0600); err != nil {
		t.Fatal(err)
	}
	deployment := map[string]any{"context": 2048, "gpu_layers": 0, "flash_attention": false}
	contract := map[string]any{"parameters": map[string]any{"max_tokens": 8}, "template_version": "synthetic-v1"}
	req := n1Envelope(f, "load-failure-run")
	req["candidates"] = []any{map[string]any{"candidate_id": "one", "artifact_id": artifact["artifact_id"], "artifact_manifest_digest": artifact["manifest_digest"], "deployment": deployment, "request": contract}}
	req["wall_seconds"], req["attempt_seconds"], req["cleanup_seconds"] = 60, 20, 5
	req["budget"] = map[string]any{"requests": 1, "tokens": 8, "bytes": 65536}
	req["notice_ref"], req["not_before"] = "synthetic-only-notice", time.Now().Add(-time.Second).UnixMilli()
	req["fixture_version"], req["purpose"] = "synthetic-v1", "synthetic_only"
	req["parameter_ranges"] = map[string]any{"context": []int{2048, 2048}, "gpu_layers": []int{0, 0}}
	run := n1Call(t, f, "POST", "evaluation-runs", req, 202)
	id := run["run_id"].(string)
	attempt := n1Envelope(f, "failed-attempt")
	attempt["seq"], attempt["candidate_id"], attempt["fixture_id"] = 1, "one", "synthetic-load-failure"
	attempt["messages"] = []map[string]string{{"role": "user", "content": "must not reach generation"}}
	messages, _ := json.Marshal(attempt["messages"])
	attempt["fixture_digest"] = database.N1Digest(messages)
	n1Call(t, f, "POST", "evaluation-runs/"+id+"/attempts", attempt, 202)
	result := n1Wait(t, f, "evaluation-runs/"+id+"/attempts/1", func(v map[string]any) bool { return v["receipt"] != nil })
	receipt := result["receipt"].(map[string]any)
	if receipt["state"] != "failed" || receipt["quality"] != "unknown" || receipt["error_code"] == nil || receipt["output_bytes"] != float64(0) || result["output_state"] != "output_unavailable" || result["body"] != nil {
		t.Fatal(result)
	}
	if receipt["identity"].(map[string]any)["ready_token"] != nil {
		t.Fatal("failed load issued ready identity")
	}
	n1Call(t, f, "POST", "evaluation-runs/"+id+"/attempts", attempt, 202)
	n1Call(t, f, "POST", "evaluation-runs/"+id+"/finish", n1Envelope(f, "finish-failure"), 200)
	finished := n1Wait(t, f, "evaluation-runs/"+id, func(v map[string]any) bool { return v["frozen_at"] != nil })
	if finished["state"] != "partial" || finished["cleanup_state"] != "released" || f.instance.IsRunning() || f.instance.ControlSnapshot().ReadyToken != "" {
		t.Fatal(finished)
	}
	if f.count("/models/load") != 1 || f.count("/v1/chat/completions") != 0 {
		t.Fatal("load failure retried or generated")
	}
	replayed := n1Call(t, f, "POST", "evaluation-runs", req, 202)
	if replayed["run_id"] != id || f.count("/models/load") != 1 {
		t.Fatal("run replayed")
	}
	// Identity and registration alone cannot manufacture a publication or use.
	doc := map[string]any{"description": "Identity is not quality", "task_tags": []string{"synthetic"}, "artifact_id": artifact["artifact_id"], "artifact_manifest_digest": artifact["manifest_digest"], "engine_identity": "synthetic-engine-v1", "deployment": deployment, "request": contract, "conclusion_level": "synthetic_only", "evidence": nil}
	recipe := n1Call(t, f, "POST", "recipes", doc, 201)
	recipeID := recipe["recipe_id"].(string)
	n1Call(t, f, "POST", "recipes/"+recipeID+"/versions/1/publish", map[string]any{"expected_version": 1, "conclusion_level": "synthetic_only"}, 409)
	use := n1Envelope(f, "unpublished-use")
	use["recipe_id"], use["version"] = recipeID, 1
	n1Call(t, f, "POST", "service-uses", use, 409)
	// Even a structurally valid findings document cannot promote a failed receipt.
	findings := map[string]any{"schema_version": 1, "run_id": id, "analysis_version": 1, "idempotency_key": "false-success", "manifest_digest": finished["manifest_digest"], "attempt_receipt_set_digest": finished["attempt_receipt_set_digest"], "policy_version": "synthetic-v1", "evaluator_version": "synthetic-v1", "stop_reason": "declared_coverage", "conclusion_level": "synthetic_only", "candidates": []any{map[string]any{"candidate_id": "one", "artifact_manifest_digest": artifact["manifest_digest"], "parameter_digest": receipt["parameter_digest"], "tested_attempt_ids": []int{1}, "quality_result": "synthetic_only", "decision": "shortlisted", "reason": "must be refused", "tested_range": []int{1}, "untested_range": []int{}, "capacity_interval": nil, "limitations": []string{"synthetic only"}}}}
	invalid := n1Call(t, f, "POST", "evaluation-runs/"+id+"/analysis", findings, 422)
	if invalid["error"] != "invalid_quality_claim" {
		t.Fatal(invalid)
	}
	encoded, _ := json.Marshal(finished)
	if bytes.Contains(encoded, []byte("secret-backend-body")) || bytes.Contains(encoded, []byte("private/path")) {
		t.Fatal("backend failure leaked")
	}
}

func TestN1ActualHTTPDiscoverEvaluatePublishUseInferenceRelease(t *testing.T) {
	f := newN1Fixture(t)
	scan := n1Call(t, f, "POST", "inventory/scans", map[string]any{"root_id": "fixture"}, 200)
	candidates := scan["candidates"].([]any)
	candidate := candidates[0].(map[string]any)
	req := n1Envelope(f, "verify")
	req["scan_id"] = scan["scan_id"]
	req["candidate_id"] = candidate["candidate_id"]
	artifact := n1Call(t, f, "POST", "inventory/verify", req, 200)
	req = n1Envelope(f, "register")
	req["artifact_id"] = artifact["artifact_id"]
	n1Call(t, f, "POST", "inventory/register", req, 200)
	deployment := map[string]any{"context": 2048, "gpu_layers": 0, "flash_attention": false}
	contract := map[string]any{"parameters": map[string]any{"max_tokens": 8, "temperature": 0, "chat_template_kwargs": map[string]any{"enable_thinking": false}}, "system_prefix": "synthetic only", "template_version": "synthetic-v1"}
	candidateManifest := map[string]any{"candidate_id": "one", "artifact_id": artifact["artifact_id"], "artifact_manifest_digest": artifact["manifest_digest"], "deployment": deployment, "request": contract}
	req = n1Envelope(f, "evaluation")
	req["candidates"] = []any{candidateManifest}
	req["wall_seconds"] = 60
	req["attempt_seconds"] = 20
	req["cleanup_seconds"] = 5
	req["budget"] = map[string]any{"requests": 1, "tokens": 8, "bytes": 65536}
	req["notice_ref"] = "synthetic-delivered-notice"
	req["not_before"] = time.Now().Add(-time.Second).UnixMilli()
	req["fixture_version"] = "synthetic-v1"
	req["purpose"] = "synthetic_only"
	req["parameter_ranges"] = map[string]any{"context": []int{2048, 2048}, "gpu_layers": []int{0, 0}}
	run := n1Call(t, f, "POST", "evaluation-runs", req, 202)
	id := run["run_id"].(string)
	same := n1Call(t, f, "POST", "evaluation-runs", req, 202)
	if same["run_id"] != id {
		t.Fatal("lost 202 replayed")
	}
	attempt := n1Envelope(f, "attempt-one")
	attempt["seq"] = 1
	attempt["candidate_id"] = "one"
	attempt["fixture_id"] = "synthetic-example"
	attempt["messages"] = []map[string]string{{"role": "user", "content": "test"}}
	messages, _ := json.Marshal(attempt["messages"])
	attempt["fixture_digest"] = database.N1Digest(messages)
	n1Call(t, f, "POST", "evaluation-runs/"+id+"/attempts", attempt, 202)
	result := n1Wait(t, f, "evaluation-runs/"+id+"/attempts/1", func(v map[string]any) bool { return v["receipt"] != nil })
	if result["output_state"] != "available" {
		t.Fatal(result)
	}
	again := n1Call(t, f, "GET", "evaluation-runs/"+id+"/attempts/1", nil, 200)
	if result["body"] != again["body"] {
		t.Fatal("GET consumed delivery")
	}
	receipt := result["receipt"].(map[string]any)
	if receipt["state"] != "succeeded" || receipt["quality"] != "synthetic_only" {
		t.Fatal(receipt)
	}
	n1Call(t, f, "POST", "evaluation-runs/"+id+"/attempts/1/ack", map[string]any{"digest": receipt["output_digest"]}, 200)
	n1Call(t, f, "POST", "evaluation-runs/"+id+"/finish", n1Envelope(f, "finish"), 200)
	finished := n1Wait(t, f, "evaluation-runs/"+id, func(v map[string]any) bool { return v["frozen_at"] != nil })
	if finished["state"] != "succeeded" || finished["cleanup_state"] != "released" {
		t.Fatal(finished)
	}
	findings := map[string]any{"schema_version": 1, "run_id": id, "analysis_version": 1, "idempotency_key": "analysis", "manifest_digest": finished["manifest_digest"], "attempt_receipt_set_digest": finished["attempt_receipt_set_digest"], "policy_version": "synthetic-policy-v1", "evaluator_version": "synthetic-v1", "stop_reason": "declared_coverage", "conclusion_level": "synthetic_only", "candidates": []any{map[string]any{"candidate_id": "one", "artifact_manifest_digest": artifact["manifest_digest"], "parameter_digest": receipt["parameter_digest"], "tested_attempt_ids": []int{1}, "quality_result": "synthetic_only", "decision": "shortlisted", "reason": "synthetic fixture only", "tested_range": []int{1}, "untested_range": []int{}, "capacity_interval": nil, "limitations": []string{"no GPU or business claim"}}}}
	final := n1Call(t, f, "POST", "evaluation-runs/"+id+"/analysis", findings, 200)
	doc := map[string]any{"description": "Synthetic direct-use recipe", "task_tags": []string{"synthetic"}, "artifact_id": artifact["artifact_id"], "artifact_manifest_digest": artifact["manifest_digest"], "engine_identity": "synthetic-engine-v1", "deployment": deployment, "request": contract, "conclusion_level": "synthetic_only", "evidence": map[string]any{"run_id": id, "findings_seq": final["seq"], "findings_digest": final["digest"], "candidate_id": "one"}}
	recipe := n1Call(t, f, "POST", "recipes", doc, 201)
	recipeID := recipe["recipe_id"].(string)
	n1Call(t, f, "POST", fmt.Sprintf("recipes/%s/versions/1/publish", recipeID), map[string]any{"expected_version": 1, "conclusion_level": "synthetic_only"}, 200)
	req = n1Envelope(f, "use")
	req["recipe_id"] = recipeID
	req["version"] = 1
	use := n1Call(t, f, "POST", "service-uses", req, 202)
	useID := use["run_id"].(string)
	ready := n1Wait(t, f, "service-uses/"+useID, func(v map[string]any) bool { return v["ready_token"] != "" && v["ready_token"] != nil })
	body, _ := json.Marshal(map[string]any{"model": "router/" + artifact["artifact_id"].(string), "messages": []map[string]string{{"role": "user", "content": "test"}}})
	request, _ := http.NewRequest("POST", f.front.URL+"/v1/chat/completions", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer synthetic-management-key")
	request.Header.Set("X-Service-Use", useID)
	request.Header.Set("X-Ready-Token", ready["ready_token"].(string))
	request.Header.Set("X-Prompt-Template-Version", "synthetic-v1")
	request.Header.Set("X-Prompt-Digest", ready["prompt_digest"].(string))
	response, err := f.front.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || response.Header.Get("X-Recipe-ID") != recipeID || !bytes.Contains(out, []byte("synthetic")) {
		t.Fatalf("proxy: %d %s %v", response.StatusCode, out, response.Header)
	}
	for _, route := range []string{"/v1/chat/completions", "/llama-cpp/router/v1/chat/completions", "/api/v1/instances/router/proxy/v1/chat/completions"} {
		for _, variant := range []string{"valid", "missing_version", "wrong_version", "wrong_digest", "duplicate_digest", "old_token", "thinking_conflict", "duplicate_thinking", "wrong_system"} {
			t.Run(route+"/"+variant, func(t *testing.T) {
				model := artifact["artifact_id"].(string)
				if route == "/v1/chat/completions" {
					model = "router/" + model
				}
				payload := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"test"}]}`, model)
				switch variant {
				case "thinking_conflict":
					payload = strings.TrimSuffix(payload, "}") + `,"chat_template_kwargs":{"enable_thinking":true}}`
				case "duplicate_thinking":
					payload = strings.TrimSuffix(payload, "}") + `,"chat_template_kwargs":{"enable_thinking":false,"enable_thinking":false}}`
				case "wrong_system":
					payload = fmt.Sprintf(`{"model":%q,"messages":[{"role":"system","content":"not the frozen prefix"},{"role":"user","content":"test"}]}`, model)
				}
				r, _ := http.NewRequest("POST", f.front.URL+route, strings.NewReader(payload))
				r.Header = request.Header.Clone()
				switch variant {
				case "missing_version":
					r.Header.Del("X-Prompt-Template-Version")
				case "wrong_version":
					r.Header.Set("X-Prompt-Template-Version", "old")
				case "wrong_digest":
					r.Header.Set("X-Prompt-Digest", strings.Repeat("0", 64))
				case "duplicate_digest":
					r.Header.Add("X-Prompt-Digest", ready["prompt_digest"].(string))
				case "old_token":
					r.Header.Set("X-Ready-Token", "old")
				}
				resp, err := f.front.Client().Do(r)
				if err != nil {
					t.Fatal(err)
				}
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if variant != "valid" {
					if resp.StatusCode < 400 {
						t.Fatalf("invalid contract forwarded: %d %s", resp.StatusCode, b)
					}
					return
				}
				if resp.StatusCode != 200 {
					t.Fatalf("valid contract rejected: %d %s", resp.StatusCode, b)
				}
				identity := ready["identity"].(map[string]any)
				for key, want := range map[string]string{"X-Recipe-ID": recipeID, "X-Recipe-Version": "1", "X-Boot-ID": identity["boot_id"].(string), "X-Config-Revision": fmt.Sprint(identity["revision"]), "X-Process-Generation": fmt.Sprint(identity["process_generation"]), "X-Load-Generation": fmt.Sprint(identity["load_generation"]), "X-Prompt-Template-Version": "synthetic-v1", "X-Prompt-Digest": ready["prompt_digest"].(string)} {
					if len(resp.Header.Values(key)) != 1 || resp.Header.Get(key) != want {
						t.Errorf("%s=%q want %q", key, resp.Header.Get(key), want)
					}
				}
			})
		}
	}
	for _, route := range []string{"/v1/chat/completions", "/llama-cpp/router/v1/chat/completions", "/api/v1/instances/router/proxy/v1/chat/completions"} {
		t.Run("trickled"+route, func(t *testing.T) {
			conn, err := net.DialTimeout("tcp", strings.TrimPrefix(f.front.URL, "http://"), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
			fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: localhost\r\nContent-Length: 1000\r\nX-Request-Timeout-Seconds: 1\r\n", route)
			for k, values := range request.Header {
				for _, v := range values {
					fmt.Fprintf(conn, "%s: %s\r\n", k, v)
				}
			}
			fmt.Fprint(conn, "\r\n{\"model\":")
			time.Sleep(30 * time.Millisecond)
			statusDone := make(chan struct{})
			go func() { f.instance.ControlSnapshot(); close(statusDone) }()
			select {
			case <-statusDone:
			case <-time.After(250 * time.Millisecond):
				t.Error("trickled real HTTP blocked status")
			}
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatal("hard body deadline failed", err)
			}
			resp.Body.Close()
			if resp.StatusCode < 400 {
				t.Fatalf("trickled invalid body admitted: %d", resp.StatusCode)
			}
			view := n1Call(t, f, "GET", "service-uses/"+useID, nil, 200)
			if view["active_requests"] != float64(0) {
				t.Fatal("unvalidated body counted", view["active_requests"])
			}
		})
	}
	n1Call(t, f, "POST", "service-uses/"+useID+"/release", n1Envelope(f, "release"), 200)
	n1Wait(t, f, "service-uses/"+useID, func(v map[string]any) bool { return v["cleanup_state"] == "released" })
	// A genuinely published synthetic recipe can still fail a later engine load.
	// Keep the same verified bytes/evidence; do not inject an artifact or recipe row.
	if err := os.WriteFile(f.events+".load-failure", []byte("synthetic load rejection"), 0600); err != nil {
		t.Fatal(err)
	}
	loads, generations := f.count("/models/load"), f.count("/v1/chat/completions")
	failedReq := n1Envelope(f, "use-load-failure")
	failedReq["recipe_id"], failedReq["version"] = recipeID, 1
	failed := n1Call(t, f, "POST", "service-uses", failedReq, 202)
	failedID := failed["run_id"].(string)
	terminal := n1Wait(t, f, "service-uses/"+failedID, func(v map[string]any) bool {
		if v["ready_token"] != nil {
			t.Fatal("failed service load issued ready token", v)
		}
		return v["frozen_at"] != nil
	})
	if terminal["state"] != "failed" || terminal["cleanup_state"] != "released" || f.instance.IsRunning() || f.instance.ControlSnapshot().ReadyToken != "" {
		t.Fatal(terminal)
	}
	for _, event := range terminal["events"].([]any) {
		if event.(map[string]any)["event_type"] == "ready" {
			t.Fatal("failed service load persisted ready evidence")
		}
	}
	if f.count("/models/load") != loads+1 || f.count("/v1/chat/completions") != generations {
		t.Fatal("failed service load retried or generated")
	}
	replayed := n1Call(t, f, "POST", "service-uses", failedReq, 202)
	if replayed["run_id"] != failedID || f.count("/models/load") != loads+1 {
		t.Fatal("failed service use replayed")
	}
	encoded, _ := json.Marshal(terminal)
	if bytes.Contains(encoded, []byte("secret-backend-body")) || bytes.Contains(encoded, []byte("private/path")) {
		t.Fatal("service error leaked backend body")
	}
}
