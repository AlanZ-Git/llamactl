package instance_test

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"llamactl/pkg/config"
	"llamactl/pkg/database"
	"llamactl/pkg/instance"
	"llamactl/pkg/manager"
)

func TestControlledSQLiteRestore(t *testing.T) {
	cfg := &config.AppConfig{}
	cfg.Instances.InstancesDir = t.TempDir()
	cfg.Instances.LogsDir = t.TempDir()
	cfg.Instances.PortRange = [2]int{18000, 18020}
	cfg.Instances.MaxInstances = -1
	cfg.Instances.DefaultAutoRestart = true
	cfg.Instances.DefaultOnDemandStart = true
	cfg.Instances.DefaultIdleTimeout = 3
	cfg.Backends.LlamaCpp.Command = "synthetic-not-executed"
	db, err := database.Open(&database.Config{Path: filepath.Join(t.TempDir(), "controlled.db"), MaxOpenConnections: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := database.RunMigrations(db); err != nil {
		t.Fatal(err)
	}
	var opts instance.Options
	if err := json.Unmarshal([]byte(`{"controlled":true,"environment_policy":"isolated","backend_type":"llama_cpp","backend_options":{"port":18001,"no_models_autoload":true,"models_max":1}}`), &opts); err != nil {
		t.Fatal(err)
	}
	original, err := instance.NewChecked("persisted", cfg, &opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	original.SetStatus(instance.Running)
	if err := db.Save(original); err != nil {
		t.Fatal(err)
	}
	saved, err := db.LoadAll()
	if err != nil || len(saved) != 1 {
		t.Fatalf("load: %v (%d)", err, len(saved))
	}
	got := saved[0].GetOptions()
	if !got.Controlled || *got.AutoRestart || *got.OnDemandStart || *got.IdleTimeout != 0 {
		t.Fatal("DB lost controlled defaults")
	}
	im := manager.New(cfg, db)
	defer im.Shutdown()
	restored, err := im.GetInstance("persisted")
	if err != nil {
		t.Fatal(err)
	}
	if restored.GetStatus() != instance.Stopped || !restored.IsControlled() {
		t.Fatal("controlled persisted running state auto-loaded")
	}
}
