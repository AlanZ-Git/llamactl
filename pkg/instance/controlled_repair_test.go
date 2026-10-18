package instance

import (
	"os"
	"path/filepath"
	"testing"
)

func TestControlledStartupPresetRejected(t *testing.T) {
	cfg, opts := controlledFixture(t)
	accepted := 0
	for _, key := range []string{"load-on-startup", "load_on_startup", "LoAd-On_Startup", "--load-on-startup", "__PRESET_LOAD_ON_STARTUP", "__preset-load-on-startup"} {
		for _, value := range []string{"true", "false", "null"} {
			preset := "[synthetic]\nmodel=synthetic.gguf\n" + key + "=" + value + "\n"
			candidate, err := opts.Clone()
			if err != nil {
				t.Fatal(err)
			}
			candidate.PresetIni = &preset
			if _, err := NewChecked("startup", cfg, candidate, nil); err == nil {
				accepted++
			}
			candidate.Controlled = false
			if _, err := NewChecked("legacy", cfg, candidate, nil); err != nil {
				t.Fatalf("legacy preset %s=%s: %v", key, value, err)
			}
		}
	}
	if accepted != 0 {
		t.Fatalf("accepted %d forbidden startup preset candidates", accepted)
	}
}

func TestControlledStartupPresetPreparationPreservesState(t *testing.T) {
	cfg, opts := controlledFixture(t)
	preset := "[synthetic]\nmodel=synthetic.gguf\ntemp=0\nflash-attn=false\n"
	opts.PresetIni = &preset
	inst, err := NewChecked("prepared", cfg, opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	inst.SetStatus(Running)
	defer inst.SetStatus(Stopped)
	old := inst.GetOptions()
	path := filepath.Join(cfg.Instances.InstancesDir, inst.Name, "preset.ini")
	before, err := os.ReadFile(path)
	if err != nil || string(before) != preset {
		t.Fatalf("ordinary zero/false preset changed: %q, %v", before, err)
	}
	for _, key := range []string{"load-on-startup", "load_on_startup", "__PRESET_LOAD_ON_STARTUP"} {
		candidate, err := old.Clone()
		if err != nil {
			t.Fatal(err)
		}
		changed := preset + key + "=false\n"
		candidate.PresetIni = &changed
		candidate.BackendOptions.SetPort(old.BackendOptions.GetPort() + 1)
		prepared, err := inst.PrepareOptions(candidate)
		if prepared != nil {
			prepared.Close()
		}
		if err == nil {
			t.Errorf("preparation accepted %s", key)
		}
		after, readErr := os.ReadFile(path)
		entries, dirErr := os.ReadDir(filepath.Dir(path))
		if readErr != nil || dirErr != nil || string(after) != preset || len(entries) != 1 || inst.GetOptions() != old || inst.GetPort() != old.BackendOptions.GetPort() || inst.GetStatus() != Running {
			t.Fatal("rejected preparation changed old state/preset/port or left a temporary file")
		}
	}
}
