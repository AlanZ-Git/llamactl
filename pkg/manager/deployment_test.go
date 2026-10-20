package manager

import (
	"encoding/json"
	"llamactl/pkg/instance"
	"llamactl/pkg/resources"
	"strings"
	"testing"
	"time"
)

func deploymentPayload(boot string, revision uint64, key string) []byte {
	b, _ := json.Marshal(map[string]any{"boot_id": boot, "expected_revision": revision, "idempotency_key": key, "model": "alpha", "fixture_id": instance.DeploymentFixtureID})
	return b
}
func TestDeploymentInputDefaultsNormalizationAndBudgets(t *testing.T) {
	base := `"boot_id":"boot","expected_revision":0,"idempotency_key":"key","model":"alpha","fixture_id":"synthetic-text-v1"`
	req, err := parseDeploymentRequest([]byte("{" + base + "}"))
	if err != nil || req.Measured != 3 || req.Warmup || req.MaxTokens != 256 || req.Timeout != 600 || req.Concurrency != 1 || req.Seed != 0 {
		t.Fatalf("defaults: %+v %v", req, err)
	}
	for _, extra := range []string{
		`"measured_generations":0`, `"measured_generations":4`, `"measured_generations":1.5`,
		`"max_output_tokens":0`, `"max_output_tokens":257`, `"timeout_seconds":0`, `"timeout_seconds":601`,
		`"concurrency":2`, `"concurrency":null`, `"warmup":1`, `"warmup":null`, `"patch":null`,
		`"seed":-1`, `"seed":2147483648`, `"prompt":"text"`, `"url":"https://invalid.example"`,
		`"patch":{"raw_ini":"secret"}`, `"patch":{"threads":1,"threads":2}`, `"patch":{"flash_attention":null}`,
		`"expected_revision":0`, `"model":null`, `"fixture_id":"unknown"`,
	} {
		if _, err := parseDeploymentRequest([]byte("{" + base + "," + extra + "}")); err == nil {
			t.Errorf("accepted %s", extra)
		}
	}
	for _, raw := range []string{"null", "[]", "{" + base + "} {}", strings.Repeat(" ", 65537)} {
		if _, err := parseDeploymentRequest([]byte(raw)); err == nil {
			t.Errorf("accepted invalid envelope")
		}
	}
	a, err := parseDeploymentRequest([]byte("{" + base + `,"patch":{"context":0.0,"threads":null,"flash_attention":false},"measured_generations":1e0}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := parseDeploymentRequest([]byte("{" + base + `,"measured_generations":1,"patch":{"flash_attention":false,"threads":null,"context":0}}`))
	if err != nil {
		t.Fatal(err)
	}
	if a.canonical != b.canonical || string(a.Patch["context"]) != "0" || string(a.Patch["threads"]) != "null" || string(a.Patch["flash_attention"]) != "false" {
		t.Fatal("canonical presence/value semantics lost")
	}
	explicit, err := parseDeploymentRequest([]byte("{" + base + `,"warmup":false}`))
	if err != nil || explicit.canonical == req.canonical {
		t.Fatal("missing and explicit false collapsed")
	}
}

func TestDeploymentLedgerBootCASCapacityRetentionScopeAndCancel(t *testing.T) {
	im, inst, backend, store := p3Manager(t)
	c := im.initControl()
	s := inst.ControlSnapshot()
	raw := deploymentPayload(s.BootID, s.Revision, "original")
	req, err := parseDeploymentRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	c.now = func() time.Time { return now }
	cancelled := 0
	c.tests = map[string]*deploymentRecord{"run": {report: DeploymentTest{ID: "run", BootID: s.BootID, Instance: inst.Name, State: "running"}, scope: "scope", key: req.Key, payload: req.canonical, cancel: func() { cancelled++ }, cancelRequests: map[string]string{}}}
	if _, err := im.SubmitDeploymentTest(inst.Name, "scope", deploymentPayload("old", s.Revision, "original")); p3Code(err) != "epoch_changed" {
		t.Fatal(err)
	}
	if _, err := im.SubmitDeploymentTest(inst.Name, "scope", deploymentPayload(s.BootID, s.Revision+1, "original")); p3Code(err) != "idempotency_conflict" {
		t.Fatal(err)
	}
	same, err := im.SubmitDeploymentTest(inst.Name, "scope", raw)
	if err != nil || same.ID != "run" {
		t.Fatal(same, err)
	}
	if _, err := im.GetDeploymentTest(inst.Name, "other", "run", false); p3Code(err) != "test_unknown_not_resumable" {
		t.Fatal(err)
	}
	if _, err := im.GetDeploymentTest("other", "scope", "run", true); p3Code(err) != "test_unknown_not_resumable" {
		t.Fatal(err)
	}
	if _, err := im.GetDeploymentTest(inst.Name, "admin", "run", true); err != nil {
		t.Fatal(err)
	}
	cancel := func(boot string, revision uint64, key string) []byte {
		b, _ := json.Marshal(map[string]any{"boot_id": boot, "expected_revision": revision, "idempotency_key": key})
		return b
	}
	if _, err := im.CancelDeploymentTest(inst.Name, "other", "run", false, cancel(s.BootID, s.Revision, "cancel")); p3Code(err) != "test_unknown_not_resumable" {
		t.Fatal(err)
	}
	if _, err := im.CancelDeploymentTest(inst.Name, "scope", "run", false, cancel("old", s.Revision, "cancel")); p3Code(err) != "epoch_changed" {
		t.Fatal(err)
	}
	if _, err := im.CancelDeploymentTest(inst.Name, "scope", "run", false, cancel(s.BootID, s.Revision+1, "cancel")); p3Code(err) != "revision_conflict" {
		t.Fatal(err)
	}
	payload := cancel(s.BootID, s.Revision, "cancel")
	if _, err := im.CancelDeploymentTest(inst.Name, "scope", "run", false, payload); err != nil {
		t.Fatal(err)
	}
	// Completion is a worker responsibility; repeated cancellation must not dispatch
	// another effect while the first cancellation is still unwinding.
	if _, err := im.CancelDeploymentTest(inst.Name, "scope", "run", false, payload); err != nil {
		t.Fatal(err)
	}
	if cancelled != 1 {
		t.Fatalf("cancel replayed %d times", cancelled)
	}
	if _, err := im.CancelDeploymentTest(inst.Name, "scope", "run", false, cancel(s.BootID, s.Revision+1, "cancel")); p3Code(err) != "idempotency_conflict" {
		t.Fatal(err)
	}
	ended := now
	c.tests["run"].report.EndedAt = &ended
	c.tests["run"].report.State = "cancelled"
	c.capacity = 1
	if _, err := im.SubmitDeploymentTest(inst.Name, "scope", deploymentPayload(s.BootID, s.Revision, "next")); p3Code(err) != "operation_capacity" {
		t.Fatal(err)
	}
	lease, err := inst.BeginControl(s.BootID, s.Revision, true, false)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(ControlRetention + time.Second)
	if _, err := im.GetDeploymentTest(inst.Name, "scope", "run", false); p3Code(err) != "test_unknown_not_resumable" {
		t.Fatal(err)
	}
	if _, err := im.SubmitDeploymentTest(inst.Name, "scope", deploymentPayload(s.BootID, s.Revision, "next")); p3Code(err) != "busy" {
		t.Fatal(err)
	}
	lease.Close()
	if len(c.tests) != 0 {
		t.Fatal("expired terminal not pruned")
	}
	if _, err := im.SubmitDeploymentTest(inst.Name, "scope", deploymentPayload(s.BootID, s.Revision+1, "next")); p3Code(err) != "revision_conflict" {
		t.Fatal(err)
	}
	c.closed = true
	if _, err := im.SubmitDeploymentTest(inst.Name, "scope", raw); p3Code(err) != "manager_stopping" {
		t.Fatal(err)
	}
	backend.mu.Lock()
	requests := len(backend.requests)
	backend.mu.Unlock()
	store.mu.Lock()
	saved := len(store.saved)
	store.mu.Unlock()
	if requests != 0 || saved != 1 {
		t.Fatalf("rejected ledger tests had side effects: requests=%d saved=%d", requests, saved)
	}
}

func TestDeploymentRejectsOtherReadyModelWithoutPreemption(t *testing.T) {
	im, inst, backend, _ := p3Manager(t)
	op := p3Submit(t, im, inst, "existing-beta", "apply", "beta", `{}`)
	if op.State != "succeeded" {
		t.Fatal(op)
	}
	before := inst.ControlSnapshot()
	backend.mu.Lock()
	count := len(backend.requests)
	backend.mu.Unlock()
	_, err := im.SubmitDeploymentTest(inst.Name, "scope", deploymentPayload(before.BootID, before.Revision, "alpha-test"))
	if p3Code(err) != "model_already_loaded" {
		t.Fatal(err)
	}
	after := inst.ControlSnapshot()
	backend.mu.Lock()
	afterCount := len(backend.requests)
	backend.mu.Unlock()
	if after.ActualModel != "beta" || after.ReadyToken != before.ReadyToken || after.Revision != before.Revision || afterCount != count {
		t.Fatal("test preempted another ready model")
	}
}

func TestDeploymentResourcesDisappearingMetricIsMissing(t *testing.T) {
	start := time.Now().UTC()
	first, second := start.Add(time.Second), start.Add(2*time.Second)
	metric := resources.Metric{Value: resources.Number(40), Available: true, Unit: "percent", Source: "synthetic_gpu", Scope: "whole_device", SampledAt: &first}
	a := newDeploymentResources(resources.Snapshot{}, false, "router", start)
	a.observe(resources.Snapshot{SampledAt: &first, GPU: resources.GPU{Devices: []resources.Device{{Index: 0, Utilization: metric}}}}, true, "router")
	a.observe(resources.Snapshot{SampledAt: &second, GPU: resources.GPU{Reason: "collection_failed", Devices: []resources.Device{}}}, true, "router")
	m := a.result().Metrics["device.0.utilization"]
	if m.Observed != 1 || m.Missing != 1 || m.Coverage != 0.5 || m.Source != "synthetic_gpu" || m.Scope != "whole_device" {
		t.Fatalf("disappearing metric coverage: %+v", m)
	}
}

func TestDeploymentResourcesDistinctTimestampsMissingAndStale(t *testing.T) {
	start := time.Now().UTC()
	missing := newDeploymentResources(resources.Snapshot{}, false, "router", start)
	if missing.result().Gap != "baseline_unavailable" || len(missing.result().Metrics) != 0 {
		t.Fatal(missing.result())
	}
	metric := func(value float64, at time.Time) resources.Metric {
		return resources.Metric{Value: resources.Number(value), Available: true, Unit: "percent", Source: "synthetic", Scope: "whole_host", SampledAt: &at}
	}
	at := start.Add(time.Second)
	snapshot := resources.Snapshot{SampledAt: &at, CPU: resources.CPU{Usage: metric(10, at)}}
	a := newDeploymentResources(snapshot, true, "router", start)
	a.observe(snapshot, true, "router")
	a.observe(snapshot, true, "router")
	second := at.Add(time.Second)
	snapshot.SampledAt = &second
	snapshot.CPU.Usage = metric(30, second)
	a.observe(snapshot, true, "router")
	third := second.Add(time.Second)
	snapshot.SampledAt = &third
	snapshot.CPU.Usage = metric(100, third)
	snapshot.CPU.Usage.Stale = true
	a.observe(snapshot, true, "router")
	fourth := third.Add(time.Second)
	snapshot.SampledAt = &fourth
	snapshot.CPU.Usage = resources.Metric{Source: "synthetic", Scope: "whole_host", Unit: "percent", Reason: "missing"}
	a.observe(snapshot, true, "router")
	result := a.result()
	m := result.Metrics["host.cpu"]
	if result.DistinctSnapshots != 4 || m.Observed != 2 || m.Stale != 1 || m.Missing != 1 || m.Mean == nil || *m.Mean != 20 || m.Peak == nil || *m.Peak != 30 || m.Coverage != 0.5 {
		t.Fatalf("aggregation: %+v", m)
	}
	*m.Mean = 900
	if *a.result().Metrics["host.cpu"].Mean != 20 {
		t.Fatal("report aliases mutable accumulator")
	}
	allMissing := newDeploymentResources(resources.Snapshot{}, false, "router", start)
	allMissing.observe(resources.Snapshot{SampledAt: &at}, true, "router")
	result = allMissing.result()
	if result.Metrics["host.cpu"].Mean != nil || result.Metrics["host.cpu"].Peak != nil || result.Gap == "" {
		t.Fatal("missing metrics claimed usable coverage", result)
	}
	// A valid baseline does not make an all-unavailable run a valid measurement.
	allMissing = newDeploymentResources(snapshot, true, "router", start)
	allMissing.observe(resources.Snapshot{SampledAt: &at}, true, "router")
	if allMissing.result().Gap == "" {
		t.Fatal("missing run metrics need explicit gap even with baseline")
	}
}
