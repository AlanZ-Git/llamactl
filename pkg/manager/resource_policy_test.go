package manager

import (
	"encoding/json"
	"errors"
	"llamactl/pkg/config"
	"llamactl/pkg/database"
	"llamactl/pkg/instance"
	"llamactl/pkg/resourcepolicy"
	"os"
	"path/filepath"
	"testing"
)

func TestOwnerPolicyStartupSQLite(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "paused", true: "corrupt"}[corrupt], func(t *testing.T) {
			dir := t.TempDir()
			cfg := &config.AppConfig{DataDir: dir}
			cfg.Instances.InstancesDir = filepath.Join(dir, "instances")
			cfg.Instances.LogsDir = filepath.Join(dir, "logs")
			cfg.Instances.PortRange = [2]int{18000, 18020}
			cfg.Instances.MaxInstances = -1
			cfg.Instances.DefaultAutoRestart = true
			cfg.Backends.LlamaCpp.Command = "synthetic-not-executed"
			db, err := database.Open(&database.Config{Path: filepath.Join(dir, "instances.db"), MaxOpenConnections: 1})
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := database.RunMigrations(db); err != nil {
				t.Fatal(err)
			}
			opts := presetOptions(t, 18001)
			body, _ := json.Marshal(map[string]any{"name": "restored", "status": "running", "options": opts})
			persisted := &instance.Instance{}
			if err := json.Unmarshal(body, persisted); err != nil {
				t.Fatal(err)
			}
			if err := db.Save(persisted); err != nil {
				t.Fatal(err)
			}
			policy := `{"version":1,"allow_ai":false,"revision":4}`
			if corrupt {
				policy = "null"
			}
			if err := os.WriteFile(filepath.Join(dir, "resource-policy.json"), []byte(policy), 0600); err != nil {
				t.Fatal(err)
			}
			im := New(cfg, db)
			defer im.Shutdown()
			if cfg.ResourcePolicy == nil || cfg.ResourcePolicy.Snapshot().AllowAI {
				t.Fatal("policy not initialized before restore")
			}
			inst, err := im.GetInstance("restored")
			if err != nil {
				t.Fatal(err)
			}
			if inst.GetStatus() != instance.Stopped {
				t.Fatal("restored running while blocked")
			}
			path := filepath.Join(cfg.Instances.InstancesDir, "restored", "preset.ini")
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("startup side effect: %v", err)
			}
			for name, action := range map[string]func() error{
				"create":  func() error { _, err := im.CreateInstance("new", opts); return err },
				"start":   func() error { _, err := im.StartInstance("restored"); return err },
				"restart": func() error { _, err := im.RestartInstance("restored"); return err },
				"update":  func() error { _, err := im.UpdateInstance("restored", opts); return err },
				"direct":  inst.Start,
			} {
				if err := action(); !errors.Is(err, resourcepolicy.ErrPaused) {
					t.Fatalf("%s bypass: %v", name, err)
				}
			}
			if _, err := im.StopInstance("restored"); err != nil {
				t.Fatal(err)
			}
			s := cfg.ResourcePolicy.Snapshot()
			if _, err := cfg.ResourcePolicy.Update(true, s.BootID, s.Revision); err != nil {
				t.Fatal(err)
			}
			if inst.IsRunning() {
				t.Fatal("resume autostarted")
			}
			if _, err := im.UpdateInstance("restored", opts); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal("explicit apply after resume failed", err)
			}
			if err := im.DeleteInstance("restored"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
