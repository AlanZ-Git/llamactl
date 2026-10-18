package manager

import (
	"encoding/json"
	"sync"
	"testing"

	"llamactl/pkg/instance"
)

func controlledOptions(t *testing.T, port int) *instance.Options {
	t.Helper()
	opts := presetOptions(t, port)
	opts.Controlled = true
	opts.EnvironmentPolicy = "isolated"
	opts.BackendOptions.LlamaServerOptions.NoModelsAutoload = true
	opts.BackendOptions.LlamaServerOptions.ModelsMax = 1
	return opts
}

type admissionBlockingStore struct {
	presetStore
	entered chan struct{}
	finish  chan struct{}
}

func (s *admissionBlockingStore) Save(inst *instance.Instance) error {
	if s.entered != nil {
		close(s.entered)
		<-s.finish
	}
	return s.presetStore.Save(inst)
}

func TestControlledOverlappingUpdatesExactlyOne(t *testing.T) {
	im, _ := presetManager(t)
	store := &admissionBlockingStore{}
	im.db = store
	inst, err := im.CreateInstance("controlled", controlledOptions(t, 18001))
	if err != nil {
		t.Fatal(err)
	}
	store.entered, store.finish = make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(store.finish) })
	result := make(chan error, 1)
	go func() { _, err := im.UpdateInstance(inst.Name, controlledOptions(t, 18002)); result <- err }()
	<-store.entered // Commit succeeded; persistence still holds the production lease.
	_, secondErr := im.UpdateInstance(inst.Name, controlledOptions(t, 18003))
	if _, code, ok := instance.AdmissionHTTPError(secondErr); !ok || code != "busy" {
		t.Fatalf("second update: %v", secondErr)
	}
	if inst.GetPort() != 18002 || len(im.ports.allocated) != 1 || im.ports.allocated[18002] != inst.Name {
		t.Fatal("loser changed reservation/options")
	}
	if _, err := im.GetInstance(inst.Name); err != nil {
		t.Fatal("status cannot be observed")
	}
	once.Do(func() { close(store.finish) })
	if err := <-result; err != nil {
		t.Fatalf("first update: %v", err)
	}
	if store.saved != 2 {
		t.Fatalf("expected create + exactly one update; saves=%d", store.saved)
	}
}

func TestControlledModeTransitionsAndRetirement(t *testing.T) {
	im, _ := presetManager(t)
	legacy := presetOptions(t, 18001)
	legacy.EnvironmentPolicy = "isolated"
	inst, err := im.CreateInstance("transition", legacy)
	if err != nil {
		t.Fatal(err)
	}
	inst.SetStatus(instance.Running)
	_, err = im.UpdateInstance(inst.Name, controlledOptions(t, 18002))
	if _, code, ok := instance.AdmissionHTTPError(err); !ok || code != "maintenance_required" {
		t.Fatalf("running transition: %v", err)
	}
	if inst.GetPort() != 18001 || inst.IsControlled() || len(im.ports.allocated) != 1 {
		t.Fatal("running transition changed state")
	}
	inst.SetStatus(instance.Stopped)
	if _, err := im.UpdateInstance(inst.Name, controlledOptions(t, 18001)); err != nil {
		t.Fatal(err)
	}
	if !inst.IsControlled() {
		t.Fatal("stopped transition not applied")
	}
	inst.SetStatus(instance.Running)
	if _, err := im.UpdateInstance(inst.Name, legacy); err == nil {
		t.Fatal("running disable accepted")
	}
	inst.SetStatus(instance.Stopped)
	if _, err := im.UpdateInstance(inst.Name, legacy); err != nil {
		t.Fatal(err)
	}
	if inst.IsControlled() {
		t.Fatal("stopped disable not applied")
	}
	if err := im.DeleteInstance(inst.Name); err != nil {
		t.Fatal(err)
	}
	if err := inst.Start(); err == nil {
		t.Fatal("stale reference can start deleted instance")
	}
	if err := inst.SetOptions(legacy); err == nil {
		t.Fatal("stale reference can change deleted options")
	}
}

func TestControlledRestoreAndAutomaticActions(t *testing.T) {
	im, _ := presetManager(t)
	im.globalConfig.Instances.DefaultAutoRestart = true
	im.globalConfig.Instances.DefaultOnDemandStart = true
	im.globalConfig.Instances.DefaultIdleTimeout = 1
	for _, state := range []string{"running", "restarting", "shutting_down", "failed"} {
		opts := controlledOptions(t, 18001)
		body, _ := json.Marshal(map[string]any{"name": "restored", "status": state, "options": opts})
		persisted := &instance.Instance{}
		if err := json.Unmarshal(body, persisted); err != nil {
			t.Fatal(err)
		}
		if err := im.loadInstance(persisted); err != nil {
			t.Fatal(err)
		}
		inst, _ := im.GetInstance("restored")
		im.autoStartInstances()
		if inst.GetStatus() != instance.Stopped || *inst.GetOptions().AutoRestart || *inst.GetOptions().OnDemandStart || *inst.GetOptions().IdleTimeout != 0 {
			t.Fatal("restore enabled automatic behavior")
		}
		if err := inst.StartAutomatic(); err == nil {
			t.Fatal("direct automatic start accepted")
		}
		if _, err := im.StartInstanceAutomatic(inst.Name); err == nil {
			t.Fatal("manager automatic start accepted")
		}
		inst.SetStatus(instance.Running)
		if inst.ShouldTimeout() {
			t.Fatal("controlled idle eviction enabled")
		}
		lifecycle := newLifecycleManager(im.registry, im, 0, true)
		lifecycle.ticker.Stop()
		if err := lifecycle.evictLRU(""); err == nil {
			t.Fatal("controlled instance evicted")
		}
		inst.SetStatus(instance.Stopped)
		if err := im.DeleteInstance(inst.Name); err != nil {
			t.Fatal(err)
		}
	}
	invalid := controlledOptions(t, 18001)
	enabled := true
	invalid.AutoRestart = &enabled
	body, _ := json.Marshal(map[string]any{"name": "invalid", "status": "running", "options": invalid})
	persisted := &instance.Instance{}
	if err := json.Unmarshal(body, persisted); err != nil {
		t.Fatal(err)
	}
	if err := im.loadInstance(persisted); err == nil {
		t.Fatal("invalid persisted controlled mode restored")
	}
	if im.registry.count() != 0 || len(im.ports.allocated) != 0 {
		t.Fatal("invalid restore retained resources")
	}
}
