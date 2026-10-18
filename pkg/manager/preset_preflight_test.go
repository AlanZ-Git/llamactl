package manager

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"llamactl/pkg/config"
	"llamactl/pkg/instance"
)

type presetStore struct {
	fail  bool
	saved int
}

func (s *presetStore) Save(*instance.Instance) error {
	if s.fail {
		return errors.New("synthetic database failure")
	}
	s.saved++
	return nil
}
func (*presetStore) Delete(string) error                    { return nil }
func (*presetStore) LoadAll() ([]*instance.Instance, error) { return nil, nil }
func (*presetStore) Close() error                           { return nil }

func presetManager(t *testing.T) (*instanceManager, *presetStore) {
	t.Helper()
	cfg := &config.AppConfig{}
	cfg.Instances.InstancesDir = filepath.Join(t.TempDir(), "预设 space")
	cfg.Instances.LogsDir = t.TempDir()
	cfg.Instances.MaxInstances = -1
	cfg.Backends.LlamaCpp.Command = "synthetic-not-executed"
	store := &presetStore{}
	return &instanceManager{globalConfig: cfg, db: store, registry: newInstanceRegistry(), ports: newPortAllocator(18000, 18020), remote: newRemoteManager(nil, 0)}, store
}
func presetOptions(t *testing.T, port int) *instance.Options {
	t.Helper()
	var opts instance.Options
	if err := json.Unmarshal([]byte(`{"backend_type":"llama_cpp","preset_ini":"[model]\nmodel = synthetic.gguf\n","backend_options":{}}`), &opts); err != nil {
		t.Fatal(err)
	}
	opts.BackendOptions.SetPort(port)
	return &opts
}
func TestPresetManagerHelperProcess(t *testing.T) {
	marker := os.Getenv("P1_MANAGER_HELPER")
	if marker == "" {
		return
	}
	if err := os.WriteFile(marker, []byte("alive"), 0600); err != nil {
		os.Exit(2)
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

func TestPresetRunningProcessSurvivesPreflight(t *testing.T) {
	im, store := presetManager(t)
	im.globalConfig.Backends.LlamaCpp.Command = os.Args[0]
	im.globalConfig.Backends.LlamaCpp.Args = []string{"-test.run=^TestPresetManagerHelperProcess$", "--"}
	marker := filepath.Join(t.TempDir(), "ready")
	opts := presetOptions(t, 18001)
	opts.EnvironmentPolicy = "isolated"
	opts.Environment = map[string]string{"P1_MANAGER_HELPER": marker}
	inst, err := im.CreateInstance("live", opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := inst.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if inst.IsRunning() {
			if err := inst.Stop(); err != nil {
				t.Error(err)
			}
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("test child did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	original := inst.GetOptions()
	path := filepath.Join(im.globalConfig.Instances.InstancesDir, "live", "preset.ini")
	// Both invalid environment and inaccessible candidate directory must leave
	// the actual running child, its options, its old preset and reservation alone.
	for _, kind := range []string{"invalid-host", "file-preparation"} {
		candidate, err := original.Clone()
		if err != nil {
			t.Fatal(err)
		}
		candidate.BackendOptions.SetPort(18002)
		savedDir := im.globalConfig.Instances.InstancesDir
		if kind == "invalid-host" {
			candidate.BackendOptions.LlamaServerOptions.Host = "0.0.0.0"
		} else {
			blocker := filepath.Join(t.TempDir(), "not-a-directory")
			if err := os.WriteFile(blocker, []byte("collision"), 0600); err != nil {
				t.Fatal(err)
			}
			im.globalConfig.Instances.InstancesDir = blocker
		}
		_, err = im.UpdateInstance("live", candidate)
		im.globalConfig.Instances.InstancesDir = savedDir
		if err == nil {
			t.Fatalf("%s accepted", kind)
		}
		if !inst.IsRunning() || inst.GetOptions() != original || inst.GetPort() != 18001 {
			t.Fatalf("%s disturbed running state", kind)
		}
		bytes, err := os.ReadFile(path)
		if err != nil || string(bytes) != *original.PresetIni {
			t.Fatal("last-good preset changed")
		}
		if len(im.ports.allocated) != 1 || im.ports.allocated[18001] != "live" || store.saved != 1 {
			t.Fatal("preflight changed registry/DB/ports")
		}
	}
}

func TestPresetLaterStartAndDBErrors(t *testing.T) {
	for _, stage := range []string{"start", "database"} {
		t.Run(stage, func(t *testing.T) {
			im, store := presetManager(t)
			inst, err := im.CreateInstance("test", presetOptions(t, 18001))
			if err != nil {
				t.Fatal(err)
			}
			if stage == "start" {
				inst.SetStatus(instance.Running)
			} else {
				store.fail = true
			}
			_, err = im.UpdateInstance("test", presetOptions(t, 18002))
			if err == nil {
				t.Fatal("later stage failure reported success")
			}
			expected := "persist updated"
			if stage == "start" {
				expected = "start instance"
			}
			if !strings.Contains(err.Error(), expected) {
				t.Fatalf("wrong stage: %v", err)
			}
			if inst.GetPort() != 18002 || im.ports.allocated[18002] != "test" || len(im.ports.allocated) != 1 {
				t.Fatal("committed state or new reservation unexpectedly rolled back")
			}
			if inst.IsRunning() {
				t.Fatal("failed start marked running")
			}
		})
	}
}

func TestPresetCreatePreparationFailure(t *testing.T) {
	im, store := presetManager(t)
	if err := os.MkdirAll(filepath.Dir(im.globalConfig.Instances.InstancesDir), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(im.globalConfig.Instances.InstancesDir, []byte("collision"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := im.CreateInstance("test", presetOptions(t, 18001))
	if err == nil {
		t.Fatal("creation reported success after preset preparation failure")
	}
	if im.registry.count() != 0 || len(im.ports.allocated) != 0 || store.saved != 0 {
		t.Fatal("failed creation retained registry/port or saved DB")
	}
}
func TestPresetCreateDBFailure(t *testing.T) {
	im, store := presetManager(t)
	store.fail = true
	_, err := im.CreateInstance("test", presetOptions(t, 18001))
	if err == nil {
		t.Fatal("creation reported success after DB failure")
	}
	if im.registry.count() != 0 || len(im.ports.allocated) != 0 {
		t.Fatal("failed creation retained registry/port")
	}
}
func TestPresetUpdatePreflightPreservesOldState(t *testing.T) {
	for _, clear := range []bool{false, true} {
		t.Run(map[bool]string{false: "write", true: "clear"}[clear], func(t *testing.T) {
			im, store := presetManager(t)
			inst, err := im.CreateInstance("test", presetOptions(t, 18001))
			if err != nil {
				t.Fatal(err)
			}
			original := inst.GetOptions()
			inst.SetStatus(instance.Running)
			defer inst.SetStatus(instance.Stopped)
			path := filepath.Join(im.globalConfig.Instances.InstancesDir, "test", "preset.ini")
			// Keep the last-good bytes in a directory collision; never depend on chmod.
			if err := os.Rename(path, path+".good"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, "owned-fixture"), []byte("untouched"), 0600); err != nil {
				t.Fatal(err)
			}
			candidate := presetOptions(t, 18002)
			if clear {
				candidate.PresetIni = nil
			}
			_, err = im.UpdateInstance("test", candidate)
			if err == nil || !strings.Contains(err.Error(), "prepar") {
				t.Errorf("expected preparation error, got %v", err)
			}
			if inst.GetStatus() != instance.Running || inst.GetOptions() != original || inst.GetPort() != 18001 {
				t.Error("old status/options/port changed")
			}
			if im.ports.allocated[18001] != "test" || len(im.ports.allocated) != 1 {
				t.Error("old reservation lost or candidate reservation leaked")
			}
			if store.saved != 1 {
				t.Error("failed preflight persisted candidate")
			}
			bytes, err := os.ReadFile(path + ".good")
			if err != nil || string(bytes) != *original.PresetIni {
				t.Error("last-good preset changed")
			}
		})
	}
}
func TestPresetRestoreFailureBlocksAutostart(t *testing.T) {
	im, _ := presetManager(t)
	persisted := &instance.Instance{}
	if err := json.Unmarshal([]byte(`{"name":"restore","status":"running","options":{"auto_restart":true,"backend_type":"llama_cpp","preset_ini":"[model]\n","backend_options":{"port":18001}}}`), persisted); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(im.globalConfig.Instances.InstancesDir, "restore", "preset.ini"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := im.loadInstance(persisted); err == nil {
		t.Fatal("restore ignored preset failure")
	}
	im.autoStartInstances()
	if im.registry.count() != 0 || len(im.ports.allocated) != 0 {
		t.Fatal("failed restore became startable")
	}
}
