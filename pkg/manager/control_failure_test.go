package manager

import (
	"context"
	"encoding/json"
	"llamactl/pkg/instance"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestP3OnlyOneProtocolRouter(t *testing.T) {
	im, inst, _, _ := p3Manager(t)
	candidate, err := inst.GetOptions().Clone()
	if err != nil {
		t.Fatal(err)
	}
	candidate.BackendOptions.SetPort(0)
	if _, err := im.CreateInstance("second-router", candidate); err == nil {
		t.Fatal("second protocol Router accepted outside single-Router contract")
	}
}

func TestP3PersistencePublicationAndCancelledCommit(t *testing.T) {
	im, inst, _, store := p3Manager(t)
	entered, proceed := make(chan struct{}), make(chan struct{})
	store.mu.Lock()
	store.entered = entered
	store.proceed = proceed
	store.mu.Unlock()
	var starts atomic.Int32
	im.control.start = func(context.Context, *instance.Maintenance, *instance.Instance) error { starts.Add(1); return nil }
	op, err := im.SubmitControl("p3", "scope", p3Payload(t, inst, "persist", "apply", "alpha", `{"context":2048}`))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("did not reach persistence")
	}
	if inst.ControlSnapshot().Revision != 0 || inst.GetOptions().ConfigRevision != 0 {
		t.Fatal("revision visible before persistence succeeded")
	}
	s := im.globalConfig.ResourcePolicy.Snapshot()
	_, err = im.globalConfig.ResourcePolicy.Update(false, s.BootID, s.Revision)
	if err != nil {
		t.Fatal(err)
	}
	close(proceed)
	result := p3Wait(t, im, op.ID)
	if result.State != "interrupted" || starts.Load() != 0 || inst.GetOptions().ConfigRevision != 1 {
		t.Fatalf("cancelled save lost durable truth or started late: %+v starts=%d", result, starts.Load())
	}
}

func TestP3PresetFailurePreservesReadyAndRevision(t *testing.T) {
	im, inst, _, _ := p3Manager(t)
	ready := p3Submit(t, im, inst, "first", "apply", "alpha", `{}`)
	if ready.State != "succeeded" {
		t.Fatal(ready)
	}
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("synthetic blocker"), 0600); err != nil {
		t.Fatal(err)
	}
	original := im.globalConfig.Instances.InstancesDir
	im.globalConfig.Instances.InstancesDir = blocker
	result := p3Submit(t, im, inst, "bad-preset", "apply", "alpha", `{"context":4096}`)
	im.globalConfig.Instances.InstancesDir = original
	if result.ErrorCode != "preset_sync_failed" || result.Stage != "prepare" {
		t.Fatal(result)
	}
	s := inst.ControlSnapshot()
	if s.ReadyToken != ready.ReadyToken || s.Revision != 1 || !inst.IsRunning() {
		t.Fatal("preflight failure changed previous generation", s)
	}
}

func TestP3TwoClientsCASAndRetryWhileBusy(t *testing.T) {
	im, inst, _, _ := p3Manager(t)
	entered, proceed := make(chan struct{}), make(chan struct{})
	im.control.stop = func(_ context.Context, _ *instance.Maintenance, i *instance.Instance) error {
		close(entered)
		<-proceed
		i.SetStatus(instance.Stopped)
		return nil
	}
	body := p3Payload(t, inst, "first", "apply", "alpha", `{"threads":0}`)
	op, err := im.SubmitControl("p3", "scope", body)
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	second := strings.Replace(string(body), `"idempotency_key":"first"`, `"idempotency_key":"second"`, 1)
	if _, err := im.SubmitControl("p3", "second-client", []byte(second)); p3Code(err) != "busy" {
		t.Fatal(err)
	}
	if repeat, err := im.SubmitControl("p3", "scope", body); err != nil || repeat.ID != op.ID {
		t.Fatal("busy rejected accepted retry", err)
	}
	close(proceed)
	if result := p3Wait(t, im, op.ID); result.State != "succeeded" {
		t.Fatal(result)
	}
	if _, err := im.SubmitControl("p3", "second-client", []byte(second)); p3Code(err) != "revision_conflict" {
		t.Fatal(err)
	}
}

func TestP3ManagerLifecycleCancelsLoadAndRejectsNewWork(t *testing.T) {
	im, inst, b, _ := p3Manager(t)
	b.holdLoad = true
	b.loadStarted = make(chan struct{})
	entered := b.loadStarted
	body := p3Payload(t, inst, "load", "apply", "alpha", `{}`)
	op, err := im.SubmitControl("p3", "scope", body)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("load not started")
	}
	im.shutdownControl()
	result := p3Wait(t, im, op.ID)
	if result.State != "interrupted" || inst.ControlSnapshot().ReadyToken != "" {
		t.Fatal(result)
	}
	if repeat, err := im.SubmitControl("p3", "scope", body); err != nil || repeat.ID != op.ID {
		t.Fatal("shutdown discarded idempotency", err)
	}
	if _, err := im.SubmitControl("p3", "scope", p3Payload(t, inst, "new", "stop", "", "")); p3Code(err) != "manager_stopping" {
		t.Fatal(err)
	}
}

func TestP3BootUnknownDoesNotRewriteOrReplay(t *testing.T) {
	im, inst, _, _ := p3Manager(t)
	old := p3Payload(t, inst, "old", "apply", "alpha", `{}`)
	path := filepath.Join(im.globalConfig.Instances.InstancesDir, inst.Name, "preset.ini")
	sentinel := []byte("administrator must inspect this drift\n")
	if err := os.WriteFile(path, sentinel, 0600); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(inst)
	if err != nil {
		t.Fatal(err)
	}
	var saved instance.Instance
	if err := json.Unmarshal(b, &saved); err != nil {
		t.Fatal(err)
	}
	fresh, _ := presetManager(t)
	fresh.globalConfig = im.globalConfig
	fresh.ports = newPortAllocator(1024, 65535)
	if err := fresh.loadInstance(&saved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fresh.shutdownControl)
	s, err := fresh.ControlStatus(inst.Name)
	if err != nil {
		t.Fatal(err)
	}
	if s.BootID == inst.ControlSnapshot().BootID || s.State != "unknown" || s.ReadyToken != "" || s.ActiveOperation != "" {
		t.Fatal("boot restored transient truth", s)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(sentinel) {
		t.Fatal("startup rewrote unverified preset")
	}
	if _, err := fresh.SubmitControl(inst.Name, "scope", old); p3Code(err) != "epoch_changed" {
		t.Fatal(err)
	}
	current, _ := fresh.ControlInstance(inst.Name)
	if _, err := fresh.SubmitControl(inst.Name, "scope", p3Payload(t, current, "blind", "start", "", "")); p3Code(err) != "recovery_required" {
		t.Fatal(err)
	}
	if _, err := fresh.RecoverControl(context.Background(), inst.Name, s.BootID, s.Revision); p3Code(err) != "port_ownership_unknown" {
		t.Fatalf("administrator assertion bypassed live listener: %v", err)
	}
}

func TestP3InvalidParametersDoNotStopOrChangeRevision(t *testing.T) {
	im, inst, _, _ := p3Manager(t)
	ready := p3Submit(t, im, inst, "first", "apply", "alpha", `{}`)
	if ready.State != "succeeded" {
		t.Fatal(ready)
	}
	for n, patch := range []string{`{"batch":0}`, `{"batch":32,"ubatch":64}`, `{"context":-1}`, `{"gpu_layers":1000}`, `{"threads":1.5}`, `{"flash_attention":null}`, `{"flash_attention":0}`, `{"kv_type_v":"q8_0","flash_attention":false}`, `{"path":"forbidden"}`, `{"context":0,"context":1}`} {
		if _, err := im.SubmitControl("p3", "scope", p3Payload(t, inst, string(rune('a'+n)), "apply", "alpha", patch)); err == nil {
			t.Fatalf("invalid patch accepted: %s", patch)
		}
		s := inst.ControlSnapshot()
		if s.Revision != 1 || s.ReadyToken != ready.ReadyToken || !inst.IsRunning() {
			t.Fatal("invalid request had side effects", s)
		}
	}
}
