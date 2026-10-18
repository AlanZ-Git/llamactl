package instance

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestPresetCheckedConstructionAndSetErrors(t *testing.T) {
	for _, clear := range []bool{false, true} {
		t.Run(map[bool]string{false: "set", true: "clear"}[clear], func(t *testing.T) {
			cfg, o := policyFixture(t, "isolated")
			text := "[fixture]\ntemp = 0\n"
			o.PresetIni = &text
			inst, err := NewChecked("preset", cfg, o, nil)
			if err != nil {
				t.Fatal(err)
			}
			old := inst.GetOptions()
			candidate, err := old.Clone()
			if err != nil {
				t.Fatal(err)
			}
			if clear {
				candidate.PresetIni = nil
			} else {
				s := "[other]\n"
				candidate.PresetIni = &s
			}
			path := filepath.Join(cfg.Instances.InstancesDir, "preset", "preset.ini")
			if err := os.Rename(path, path+".last-good"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0755); err != nil {
				t.Fatal(err)
			}
			if err := inst.SetOptions(candidate); err == nil {
				t.Fatal("SetOptions swallowed preparation failure")
			}
			if inst.GetOptions() != old {
				t.Fatal("failed SetOptions changed live options")
			}
			if _, err := NewChecked("preset", cfg, candidate, nil); err == nil {
				t.Fatal("constructor swallowed preparation failure")
			}
			matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".preset-*.tmp"))
			if err != nil || len(matches) != 0 {
				t.Fatal("temporary files leaked")
			}
		})
	}
}

func TestPresetPreparedReplacementAndCommitFailure(t *testing.T) {
	cfg, o := policyFixture(t, "isolated")
	text := "[original]\ntemp = 0\n"
	o.PresetIni = &text
	inst, err := NewChecked("preset", cfg, o, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.Instances.InstancesDir, "preset", "preset.ini")
	old := inst.GetOptions()
	candidate, err := old.Clone()
	if err != nil {
		t.Fatal(err)
	}
	replacement := "[replacement]\nflash-attn = false\n"
	candidate.PresetIni = &replacement
	p, err := inst.PrepareOptions(candidate)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	bytes, err := os.ReadFile(path)
	if err != nil || string(bytes) != text {
		t.Fatal("prepare truncated last-good preset")
	}
	inst.SetStatus(Running)
	if err := p.Commit(); err == nil {
		t.Fatal("committed while running")
	}
	inst.SetStatus(Stopped)
	inst.process.monitorDone = make(chan struct{})
	if err := p.Commit(); err == nil {
		t.Fatal("committed before process exit was confirmed")
	}
	inst.process.monitorDone = nil
	// A post-preflight collision is a commit-stage error, not a successful update.
	if err := os.Rename(path, path+".good"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0755); err != nil {
		t.Fatal(err)
	}
	if err := p.Commit(); err == nil {
		t.Fatal("commit ignored destination collision")
	}
	if inst.GetOptions() != old {
		t.Fatal("failed commit changed options")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".good", path); err != nil {
		t.Fatal(err)
	}
	if err := p.Commit(); err != nil {
		t.Fatal(err)
	}
	bytes, err = os.ReadFile(path)
	if err != nil || string(bytes) != replacement {
		t.Fatal("replacement not committed")
	}
	if err := p.Commit(); err == nil {
		t.Fatal("candidate committed twice")
	}
	candidate.PresetIni = nil
	if err := inst.SetOptions(candidate); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("clear left preset behind")
	}
	inst.process.ctx = context.Background()
	cmd, err := inst.process.buildCommand()
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range cmd.Args {
		if arg == "--models-preset" {
			t.Fatal("clear left models-preset argument")
		}
	}
}

func TestPresetStartAndRestartPreparationFailure(t *testing.T) {
	cfg, o := policyFixture(t, "isolated")
	text := "[fixture]\n"
	o.PresetIni = &text
	inst, err := NewChecked("preset", cfg, o, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.Instances.InstancesDir, "preset", "preset.ini")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0755); err != nil {
		t.Fatal(err)
	}
	if err := inst.Start(); err == nil {
		t.Fatal("start ignored preset failure")
	}
	if inst.process.cmd != nil || inst.GetStatus() != Stopped {
		t.Fatal("start reached child execution")
	}
	inst.SetStatus(Running)
	old := inst.GetOptions()
	if err := inst.Restart(); err == nil {
		t.Fatal("restart ignored preflight failure")
	}
	if inst.GetStatus() != Running || inst.GetOptions() != old {
		t.Fatal("restart disturbed old state before preparation")
	}
	inst.SetStatus(Stopped)
}
