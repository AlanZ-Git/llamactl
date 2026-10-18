package instance

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"llamactl/pkg/config"
)

func controlledFixture(t *testing.T) (*config.AppConfig, *Options) {
	t.Helper()
	cfg, opts := policyFixture(t, "isolated")
	opts.Controlled = true
	opts.BackendOptions.LlamaServerOptions.NoModelsAutoload = true
	opts.BackendOptions.LlamaServerOptions.ModelsMax = 1
	return cfg, opts
}

func TestControlledDefaultsAndValidation(t *testing.T) {
	cfg, opts := controlledFixture(t)
	cfg.Instances.DefaultAutoRestart, cfg.Instances.DefaultOnDemandStart, cfg.Instances.DefaultIdleTimeout = true, true, 5
	inst, err := NewChecked("defaults", cfg, opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := inst.GetOptions()
	if !got.Controlled || *got.AutoRestart || *got.OnDemandStart || *got.IdleTimeout != 0 {
		t.Fatal("controlled inherited automatic defaults")
	}
	if opts.AutoRestart != nil || opts.OnDemandStart != nil || opts.IdleTimeout != nil {
		t.Fatal("validation mutated caller")
	}
	data, _ := json.Marshal(got)
	var roundtrip Options
	if err := json.Unmarshal(data, &roundtrip); err != nil {
		t.Fatal(err)
	}
	if !roundtrip.Controlled || *roundtrip.AutoRestart || *roundtrip.OnDemandStart || *roundtrip.IdleTimeout != 0 {
		t.Fatal("controlled/false/zero lost")
	}
	for name, change := range map[string]func(*config.AppConfig, *Options){
		"inherit":          func(_ *config.AppConfig, o *Options) { o.EnvironmentPolicy = "inherit" },
		"remote":           func(_ *config.AppConfig, o *Options) { o.Nodes = map[string]struct{}{"remote": {}} },
		"backend":          func(_ *config.AppConfig, o *Options) { o.BackendOptions.BackendType = "vllm" },
		"docker":           func(_ *config.AppConfig, o *Options) { b := true; o.DockerEnabled = &b },
		"restart":          func(_ *config.AppConfig, o *Options) { b := true; o.AutoRestart = &b },
		"on demand":        func(_ *config.AppConfig, o *Options) { b := true; o.OnDemandStart = &b },
		"idle":             func(_ *config.AppConfig, o *Options) { n := 1; o.IdleTimeout = &n },
		"negative idle":    func(_ *config.AppConfig, o *Options) { n := -1; o.IdleTimeout = &n },
		"autoload default": func(_ *config.AppConfig, o *Options) { o.BackendOptions.LlamaServerOptions.NoModelsAutoload = false },
		"autoload enabled": func(_ *config.AppConfig, o *Options) { o.BackendOptions.LlamaServerOptions.ModelsAutoload = true },
		"capacity":         func(_ *config.AppConfig, o *Options) { o.BackendOptions.LlamaServerOptions.ModelsMax = 2 },
		"sleep":            func(_ *config.AppConfig, o *Options) { o.BackendOptions.LlamaServerOptions.SleepIdleSeconds = 1 },
		"raw global":       func(c *config.AppConfig, _ *Options) { c.Backends.LlamaCpp.Args = []string{"--models-autoload"} },
		"raw extra": func(_ *config.AppConfig, o *Options) {
			o.BackendOptions.LlamaServerOptions.ExtraArgs = map[string]string{"models-max": "2"}
		},
		"raw unknown": func(_ *config.AppConfig, o *Options) {
			o.BackendOptions.LlamaServerOptions.ExtraArgs = map[string]string{"future-router-mode": "true"}
		},
		"env global": func(c *config.AppConfig, _ *Options) {
			c.Backends.LlamaCpp.Environment = map[string]string{"LLAMA_ARG_MODELS_AUTOLOAD": "1"}
		},
		"env local":       func(_ *config.AppConfig, o *Options) { o.Environment = map[string]string{"llama_arg_models_max": "2"} },
		"preset capacity": func(_ *config.AppConfig, o *Options) { s := "[*]\nmodels_max = 2\n"; o.PresetIni = &s },
		"preset autoload": func(_ *config.AppConfig, o *Options) { s := "[*]\nno-models-autoload = false\n"; o.PresetIni = &s },
		"preset sleep":    func(_ *config.AppConfig, o *Options) { s := "[*]\nsleep-idle-seconds = 1\n"; o.PresetIni = &s },
	} {
		t.Run(name, func(t *testing.T) {
			c, o := controlledFixture(t)
			change(c, o)
			if _, err := NewChecked("invalid", c, o, nil); err == nil {
				t.Fatal("unsafe controlled configuration accepted")
			}
		})
	}
	// A fixed single model does not require Router flags.
	singleCfg, single := controlledFixture(t)
	single.BackendOptions.LlamaServerOptions.Model = "synthetic.gguf"
	single.BackendOptions.LlamaServerOptions.NoModelsAutoload = false
	single.BackendOptions.LlamaServerOptions.ModelsMax = 0
	if _, err := NewChecked("single", singleCfg, single, nil); err != nil {
		t.Fatal(err)
	}
}

func TestControlledLegacyCompatibility(t *testing.T) {
	for _, policy := range []string{"", "isolated"} {
		cfg, opts := policyFixture(t, policy)
		cfg.Instances.DefaultAutoRestart, cfg.Instances.DefaultOnDemandStart, cfg.Instances.DefaultIdleTimeout = true, true, 7
		inst := New("legacy", cfg, opts, nil)
		got := inst.GetOptions()
		if got.Controlled || !*got.AutoRestart || !*got.OnDemandStart || *got.IdleTimeout != 7 {
			t.Fatal("legacy defaults changed")
		}
		engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "legacy") }))
		defer engine.Close()
		candidate, _ := got.Clone()
		candidate.BackendOptions.LlamaServerOptions.Host = "127.0.0.1"
		candidate.BackendOptions.SetPort(engine.Listener.Addr().(*net.TCPAddr).Port)
		if err := inst.SetOptions(candidate); err != nil {
			t.Fatal(err)
		}
		// Unknown mutable paths and request autoload retain legacy forwarding.
		recorder := httptest.NewRecorder()
		if err := inst.ServeHTTP(recorder, httptest.NewRequest("POST", "/unknown?autoload=true", strings.NewReader(`{"autoload":true}`))); err != nil {
			t.Fatal(err)
		}
		if recorder.Body.String() != "legacy" {
			t.Fatal("legacy proxy restricted")
		}
	}
}

func TestControlledPermitReleaseAndLeaseValidation(t *testing.T) {
	cfg, opts := controlledFixture(t)
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(502) }))
	opts.BackendOptions.SetPort(engine.Listener.Addr().(*net.TCPAddr).Port)
	inst := New("errors", cfg, opts, nil)
	inst.SetStatus(Running)
	for _, transportError := range []bool{false, true} {
		if transportError {
			engine.Close()
		}
		recorder := httptest.NewRecorder()
		_ = inst.ServeHTTP(recorder, httptest.NewRequest("POST", "/completion", strings.NewReader(`{}`)))
		if recorder.Code != 502 {
			t.Fatalf("backend error status=%d", recorder.Code)
		}
		lease, err := inst.BeginMaintenance()
		if err != nil {
			t.Fatalf("backend failure leaked permit: %v", err)
		}
		lease.Close()
		if err := lease.Start(); err == nil {
			t.Fatal("closed lease can start")
		}
	}
	engine.Close()
	inst.SetStatus(Stopped)
	prepared, err := inst.PrepareOptions(opts)
	if err != nil {
		t.Fatal(err)
	}
	prepared.Close()
	if err := prepared.Commit(); err == nil {
		t.Fatal("closed prepared candidate committed")
	}
	if _, err := (&Maintenance{}).PrepareOptions(opts); err == nil {
		t.Fatal("unvalidated lease prepared options")
	}
}

func TestControlledTransitionBlocksLegacyProxy(t *testing.T) {
	cfg, opts := policyFixture(t, "isolated")
	inst := New("transition", cfg, opts, nil)
	candidate, _ := opts.Clone()
	candidate.Controlled = true
	candidate.BackendOptions.LlamaServerOptions.NoModelsAutoload = true
	candidate.BackendOptions.LlamaServerOptions.ModelsMax = 1
	release, err := inst.AcquireAutomaticUse()
	if err != nil {
		t.Fatal(err)
	}
	if err := inst.SetOptions(candidate); err == nil {
		t.Fatal("transition ignored admitted legacy autostart")
	}
	release()
	prepared, err := inst.PrepareOptions(candidate)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	recorder := httptest.NewRecorder()
	err = inst.ServeHTTP(recorder, httptest.NewRequest("POST", "/completion", strings.NewReader(`{}`)))
	if _, code, ok := AdmissionHTTPError(err); !ok || code != "busy" || recorder.Code != 409 {
		t.Fatal("legacy proxy slipped into mode transition")
	}
	if err := prepared.Commit(); err != nil {
		t.Fatal(err)
	}
	if !inst.IsControlled() {
		t.Fatal("transition not committed")
	}
}
