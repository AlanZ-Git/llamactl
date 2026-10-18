package instance

import (
	"errors"
	"llamactl/pkg/resourcepolicy"
	"os"
	"path/filepath"
	"testing"
)

func TestOwnerPolicyPauseBetweenPresetStageAndCommit(t *testing.T) {
	cfg, opts := policyFixture(t, "isolated")
	old := "[old]\nmodel=old.gguf\n"
	opts.PresetIni = &old
	if err := syncPresetIni("late", opts, cfg.Instances.InstancesDir); err != nil {
		t.Fatal(err)
	}
	g, err := resourcepolicy.Open("")
	if err != nil {
		t.Fatal(err)
	}
	permit, err := g.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	defer permit.Close()
	next := "[next]\nmodel=next.gguf\n"
	opts.PresetIni = &next
	staged, err := preparePresetIni("late", opts, cfg.Instances.InstancesDir)
	if err != nil {
		t.Fatal(err)
	}
	defer staged.cleanup()
	s := g.Snapshot()
	if _, err := g.Update(false, s.BootID, s.Revision); err != nil {
		t.Fatal(err)
	}
	if err := staged.commitPolicy(permit); !errors.Is(err, resourcepolicy.ErrPaused) {
		t.Fatalf("late commit: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(cfg.Instances.InstancesDir, "late", "preset.ini"))
	if err != nil || string(data) != old {
		t.Fatalf("preset changed: %q %v", data, err)
	}
}
