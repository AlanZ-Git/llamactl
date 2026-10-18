package instance

import (
	"errors"
	"llamactl/pkg/resourcepolicy"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOwnerPolicyDirectBoundaries(t *testing.T) {
	cfg, opts := policyFixture(t, "isolated")
	g, err := resourcepolicy.Open("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.ResourcePolicy = g
	inst, err := NewChecked("owner", cfg, opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := inst.BeginMaintenance()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	candidate, err := lease.PrepareOptions(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Close()
	s := g.Snapshot()
	if _, err := g.Update(false, s.BootID, s.Revision); err != nil {
		t.Fatal(err)
	}
	for name, action := range map[string]func() error{
		"start": inst.Start, "automatic": inst.StartAutomatic, "restart": inst.Restart,
		"options": func() error { return inst.SetOptions(opts) }, "late start": lease.Start, "late commit": candidate.Commit,
		"construct": func() error { _, err := NewChecked("blocked", cfg, opts, nil); return err },
		"automatic use": func() error {
			release, err := inst.AcquireAutomaticUse()
			if release != nil {
				release()
			}
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := action(); !errors.Is(err, resourcepolicy.ErrPaused) {
				t.Fatalf("not blocked: %v", err)
			}
		})
	}
	for _, path := range []string{"/completion", "/props", "/unknown", "/models/load"} {
		w := httptest.NewRecorder()
		err := inst.ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader(`{"model":"synthetic"}`)))
		if !errors.Is(err, resourcepolicy.ErrPaused) || !strings.Contains(w.Body.String(), `"retryable":false`) {
			t.Fatalf("proxy %s: %v %s", path, err, w.Body.String())
		}
	}
	if err := lease.Stop(); errors.Is(err, resourcepolicy.ErrPaused) {
		t.Fatal("stop blocked by owner policy")
	}
	s = g.Snapshot()
	if _, err := g.Update(true, s.BootID, s.Revision); err != nil {
		t.Fatal(err)
	}
	if err := candidate.Commit(); !errors.Is(err, resourcepolicy.ErrPaused) {
		t.Fatalf("old work revived: %v", err)
	}
	if inst.IsRunning() {
		t.Fatal("resume started instance")
	}
	lease.Close()
	if err := inst.SetOptions(opts); err != nil {
		t.Fatal(err)
	}
}

func TestOwnerPolicyDescriptorNoPreset(t *testing.T) {
	cfg, opts := policyFixture(t, "isolated")
	preset := "[synthetic]\nmodel=synthetic.gguf\n"
	opts.PresetIni = &preset
	g, err := resourcepolicy.Open("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.ResourcePolicy = g
	s := g.Snapshot()
	g.Update(false, s.BootID, s.Revision)
	inst, err := NewDescriptor("descriptor", cfg, opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.Instances.InstancesDir, "descriptor", "preset.ini")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("constructor wrote preset: %v", err)
	}
	if err := inst.Start(); !errors.Is(err, resourcepolicy.ErrPaused) {
		t.Fatalf("descriptor start: %v", err)
	}
	s = g.Snapshot()
	g.Update(true, s.BootID, s.Revision)
	if inst.IsRunning() {
		t.Fatal("descriptor auto-started")
	}
	if err := inst.SetOptions(opts); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != preset {
		t.Fatalf("explicit options: %q %v", data, err)
	}
}
