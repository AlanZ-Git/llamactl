package instance_test

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"llamactl/pkg/config"
	"llamactl/pkg/database"
	"llamactl/pkg/instance"
)

// This test subprocess only records argv and waits for stdin EOF. It never
// invokes a model, GPU library, shell, or external service.
func TestPresetArgvHelper(t *testing.T) {
	path := os.Getenv("P1_ARGV_CAPTURE")
	if path == "" {
		return
	}
	data, err := json.Marshal(os.Args)
	if err != nil {
		os.Exit(2)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		os.Exit(3)
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

func TestPresetJSONDatabaseReloadAndFinalArgv(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Unicode 预设 space")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.AppConfig{}
	cfg.Instances.InstancesDir = dir
	cfg.Instances.LogsDir = filepath.Join(dir, "logs")
	cfg.Backends.LlamaCpp.Command = os.Args[0]
	cfg.Backends.LlamaCpp.Args = []string{"-test.run=^TestPresetArgvHelper$", "--"}
	var opts instance.Options
	// Model-scoped numeric zero and boolean false are kept verbatim in the
	// preset. Scalar CLI zero and a negative boolean flag use extra_args.
	input := `{"environment_policy":"isolated","auto_restart":false,"idle_timeout":0,"preset_ini":"[fixture]\nmodel = synthetic model.gguf\ntemp = 0\nflash-attn = false\n","backend_type":"llama_cpp","backend_options":{"extra_args":{"seed":"0","no-warmup":""}}}`
	if err := json.Unmarshal([]byte(input), &opts); err != nil {
		t.Fatal(err)
	}
	capture := filepath.Join(dir, "captured argv.json")
	opts.Environment = map[string]string{"P1_ARGV_CAPTURE": capture}
	// The fixture has no listening server. Port zero avoids checking an unrelated service.
	inst, err := instance.NewChecked("roundtrip", cfg, &opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(&database.Config{Path: filepath.Join(dir, "roundtrip.db"), MaxOpenConnections: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := database.RunMigrations(db); err != nil {
		t.Fatal(err)
	}
	if err := db.Save(inst); err != nil {
		t.Fatal(err)
	}
	loaded, err := db.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 {
		t.Fatalf("loaded %d instances", len(loaded))
	}
	got := loaded[0].GetOptions()
	if got.EnvironmentPolicy != "isolated" || got.AutoRestart == nil || *got.AutoRestart || got.IdleTimeout == nil || *got.IdleTimeout != 0 {
		t.Fatal("policy or explicit false/zero lost")
	}
	if got.PresetIni == nil || *got.PresetIni != *opts.PresetIni || !reflect.DeepEqual(got.BackendOptions.LlamaServerOptions.ExtraArgs, opts.BackendOptions.LlamaServerOptions.ExtraArgs) {
		t.Fatal("model preset or extra_args changed")
	}
	reloaded, err := instance.NewChecked(loaded[0].Name, cfg, got, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := reloaded.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if reloaded.IsRunning() {
			if err := reloaded.Stop(); err != nil {
				t.Error(err)
			}
		}
	}()
	var argv []string
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, err := os.ReadFile(capture)
		if err == nil && json.Unmarshal(data, &argv) == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("synthetic argv recorder did not run")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, flag := range []string{"--seed", "--no-warmup", "--models-preset", "--host"} {
		if !slices.Contains(argv, flag) {
			t.Errorf("missing %s", flag)
		}
	}
	at := slices.Index(argv, "--seed")
	if at < 0 || at+1 >= len(argv) || argv[at+1] != "0" {
		t.Fatal("explicit zero absent from final argv")
	}
	at = slices.Index(argv, "--models-preset")
	if at < 0 || at+1 >= len(argv) {
		t.Fatal("preset path missing")
	}
	path := filepath.Join(dir, "roundtrip", "preset.ini")
	if argv[at+1] != path {
		t.Fatalf("preset argument lost Unicode/spaces: %q", argv[at+1])
	}
	bytes, err := os.ReadFile(path)
	if err != nil || string(bytes) != *opts.PresetIni {
		t.Fatal("derived preset mismatch")
	}
	for _, flag := range []string{"--model", "--temp", "--flash-attn", "--warmup", "--gpu-layers"} {
		if slices.Contains(argv, flag) {
			t.Errorf("unintended global model parameter %s", flag)
		}
	}
}
