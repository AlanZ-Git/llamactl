package instance

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"llamactl/pkg/config"
)

func policyFixture(t *testing.T, policy string) (*config.AppConfig, *Options) {
	t.Helper()
	cfg := &config.AppConfig{}
	cfg.Instances.InstancesDir = t.TempDir()
	cfg.Instances.LogsDir = t.TempDir()
	cfg.Backends.LlamaCpp.Command = "synthetic-not-executed"
	var opts Options
	if err := json.Unmarshal([]byte(`{"environment_policy":"`+policy+`","backend_type":"llama_cpp","backend_options":{"port":8123}}`), &opts); err != nil {
		t.Fatal(err)
	}
	return cfg, &opts
}

func TestPolicyChildEnvironment(t *testing.T) {
	for _, policy := range []string{"isolated", "inherit", ""} {
		t.Run("policy="+policy, func(t *testing.T) {
			t.Setenv("LLAMA_ARG_MODEL", "ambient-model")
			t.Setenv("LLAMA_API_KEY", "ambient-synthetic-key")
			t.Setenv("P1_RUNTIME", "host-runtime")
			t.Setenv("P1_EMPTY", "host-value")
			cfg, opts := policyFixture(t, policy)
			cfg.Backends.LlamaCpp.Environment = map[string]string{"P1_RUNTIME": "backend-runtime", "LLAMA_ARG_THREADS": "3"}
			opts.Environment = map[string]string{"P1_RUNTIME": "instance-runtime", "P1_EMPTY": "", "LLAMA_ARG_THREADS": "0"}
			inst := New("env", cfg, opts, nil)
			inst.process.ctx = context.Background()
			cmd, err := inst.process.buildCommand()
			if err != nil {
				t.Fatal(err)
			}
			env := map[string]string{}
			for _, entry := range cmd.Env {
				key, value, _ := strings.Cut(entry, "=")
				if _, exists := env[key]; exists && policy == "isolated" {
					t.Errorf("duplicate key %s", key)
				}
				env[key] = value
			}
			if policy == "isolated" {
				for _, key := range []string{"LLAMA_ARG_MODEL", "LLAMA_API_KEY"} {
					if _, ok := env[key]; ok {
						t.Errorf("inherited %s", key)
					}
				}
				if inst.GetHost() != "127.0.0.1" {
					t.Errorf("host = %q", inst.GetHost())
				}
			} else if env["LLAMA_ARG_MODEL"] != "ambient-model" {
				t.Error("legacy inheritance changed")
			}
			if env["P1_RUNTIME"] != "instance-runtime" || env["P1_EMPTY"] != "" || env["LLAMA_ARG_THREADS"] != "0" {
				t.Error("explicit environment precedence lost")
			}
			if env["PATH"] != os.Getenv("PATH") {
				t.Error("PATH lost")
			}
			if os.Getenv("LLAMA_ARG_MODEL") != "ambient-model" {
				t.Error("parent mutated")
			}
		})
	}
}

func TestPolicyWindowsEnvironmentComparison(t *testing.T) {
	env := mergeChildEnvironment([]string{"Path=runtime", "PATH=last-host", "llama_api_key=ambient", "LlAmA_ARG_MODEL=ambient", "CUDA_PATH=gpu", "=C:=C:\\work"}, true, "windows",
		map[string]string{"Path": "backend", "LLAMA_ARG_THREADS": "2"}, map[string]string{"pAtH": "instance", "llama_arg_threads": "", "EMPTY": ""})
	got := map[string]string{}
	for _, entry := range env {
		if entry == "=C:=C:\\work" {
			continue
		}
		key, value, _ := strings.Cut(entry, "=")
		key = strings.ToUpper(key)
		if _, ok := got[key]; ok {
			t.Fatalf("duplicate Windows key %s", key)
		}
		got[key] = value
	}
	if got["PATH"] != "instance" || got["CUDA_PATH"] != "gpu" {
		t.Fatal("runtime variables or precedence lost")
	}
	if value, ok := got["LLAMA_ARG_THREADS"]; !ok || value != "" {
		t.Fatal("explicit empty lost")
	}
	if _, ok := got["LLAMA_API_KEY"]; ok {
		t.Fatal("ambient Windows auth inherited")
	}
	if _, ok := got["LLAMA_ARG_MODEL"]; ok {
		t.Fatal("ambient Windows model inherited")
	}
}

func TestPolicyValidation(t *testing.T) {
	cases := map[string]func(*config.AppConfig, *Options){
		"unknown policy":  func(_ *config.AppConfig, o *Options) { o.EnvironmentPolicy = "unknown" },
		"remote":          func(_ *config.AppConfig, o *Options) { o.Nodes = map[string]struct{}{"elsewhere": {}} },
		"other backend":   func(_ *config.AppConfig, o *Options) { o.BackendOptions.BackendType = "vllm" },
		"docker override": func(_ *config.AppConfig, o *Options) { b := true; o.DockerEnabled = &b },
		"docker global": func(c *config.AppConfig, _ *Options) {
			c.Backends.LlamaCpp.Docker = &config.DockerSettings{Enabled: true}
		},
		"wildcard host": func(_ *config.AppConfig, o *Options) { o.BackendOptions.LlamaServerOptions.Host = "0.0.0.0" },
		"hostname":      func(_ *config.AppConfig, o *Options) { o.BackendOptions.LlamaServerOptions.Host = "localhost" },
		"host env":      func(_ *config.AppConfig, o *Options) { o.Environment = map[string]string{"LLAMA_ARG_HOST": "::"} },
		"port env":      func(_ *config.AppConfig, o *Options) { o.Environment = map[string]string{"LLAMA_ARG_PORT": "9999"} },
		"key conflict": func(_ *config.AppConfig, o *Options) {
			o.BackendOptions.LlamaServerOptions.APIKey = "synthetic-a"
			o.Environment = map[string]string{"LLAMA_API_KEY": "synthetic-b"}
		},
		"empty key conflict": func(_ *config.AppConfig, o *Options) {
			o.BackendOptions.LlamaServerOptions.APIKey = "synthetic-a"
			o.Environment = map[string]string{"LLAMA_API_KEY": ""}
		},
		"multiple keys": func(_ *config.AppConfig, o *Options) {
			o.BackendOptions.LlamaServerOptions.APIKey = "synthetic-a,synthetic-b"
		},
		"key file": func(_ *config.AppConfig, o *Options) { o.BackendOptions.LlamaServerOptions.APIKeyFile = "never-read" },
		"key file env": func(_ *config.AppConfig, o *Options) {
			o.Environment = map[string]string{"LLAMA_API_KEY_FILE": "never-read"}
		},
		"key arg":  func(c *config.AppConfig, _ *Options) { c.Backends.LlamaCpp.Args = []string{"--api-key=synthetic-a"} },
		"host arg": func(c *config.AppConfig, _ *Options) { c.Backends.LlamaCpp.Args = []string{"--host", "0.0.0.0"} },
		"host extra": func(_ *config.AppConfig, o *Options) {
			o.BackendOptions.LlamaServerOptions.ExtraArgs = map[string]string{"host": "0.0.0.0"}
		},
		"preset routing":  func(_ *config.AppConfig, o *Options) { s := "[*]\nhost = 0.0.0.0\n"; o.PresetIni = &s },
		"external preset": func(_ *config.AppConfig, o *Options) { o.BackendOptions.LlamaServerOptions.ModelsPreset = "never-read" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg, o := policyFixture(t, "isolated")
			mutate(cfg, o)
			if _, err := NewChecked("invalid", cfg, o, nil); err == nil {
				t.Fatal("unsupported configuration accepted")
			}
		})
	}
	for _, host := range []string{"127.0.0.1", "::1"} {
		t.Run(host, func(t *testing.T) {
			cfg, o := policyFixture(t, "isolated")
			o.Environment = map[string]string{"LLAMA_ARG_HOST": host, "LLAMA_API_KEY": "synthetic-same"}
			o.BackendOptions.LlamaServerOptions.APIKey = "synthetic-same"
			inst, err := NewChecked("valid", cfg, o, nil)
			if err != nil {
				t.Fatal(err)
			}
			if inst.GetHost() != host {
				t.Fatal("effective host mismatch")
			}
		})
	}
}

func TestPolicyProxyCredentialBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, policy, expected string
		remote                 bool
	}{
		{"isolated-no-key", "isolated", "", false},
		{"legacy", "inherit", "Bearer synthetic-caller", false},
		{"remote", "inherit", "Bearer synthetic-node", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			received := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received <- r.Header.Get("Authorization")
				w.WriteHeader(204)
			}))
			defer server.Close()
			cfg, o := policyFixture(t, tc.policy)
			host, portText, _ := net.SplitHostPort(server.Listener.Addr().String())
			port, _ := strconv.Atoi(portText)
			o.BackendOptions.LlamaServerOptions.Host = host
			o.BackendOptions.LlamaServerOptions.Port = port
			if tc.remote {
				o.Nodes = map[string]struct{}{"node": {}}
				cfg.Nodes = map[string]config.NodeConfig{"node": {Address: server.URL, APIKey: "synthetic-node"}}
				o.BackendOptions.LlamaServerOptions.APIKey = "synthetic-backend-must-not-leak"
			}
			inst, err := NewChecked("boundary", cfg, o, nil)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest("GET", "/v1/models", nil)
			req.Header.Set("Authorization", "Bearer synthetic-caller")
			if err := inst.ServeHTTP(httptest.NewRecorder(), req); err != nil {
				t.Fatal(err)
			}
			if got := <-received; got != tc.expected {
				t.Errorf("authorization = %q", got)
			}
		})
	}
}

func TestPolicyProxyRefreshesBackendAddressAndKey(t *testing.T) {
	servers := make([]*httptest.Server, 0, 2)
	for _, key := range []string{"synthetic-first", "synthetic-second"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+key {
				w.WriteHeader(401)
				return
			}
			w.WriteHeader(204)
		}))
		defer server.Close()
		servers = append(servers, server)
	}
	cfg, opts := policyFixture(t, "isolated")
	var inst *Instance
	for n, server := range servers {
		host, portText, _ := net.SplitHostPort(server.Listener.Addr().String())
		port, _ := strconv.Atoi(portText)
		opts.BackendOptions.LlamaServerOptions.Host = host
		opts.BackendOptions.LlamaServerOptions.Port = port
		opts.BackendOptions.LlamaServerOptions.APIKey = []string{"synthetic-first", "synthetic-second"}[n]
		if inst == nil {
			var err error
			inst, err = NewChecked("refresh", cfg, opts, nil)
			if err != nil {
				t.Fatal(err)
			}
		} else if err := inst.SetOptions(opts); err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest("GET", "/v1/models", nil)
		request.Header.Set("Authorization", "Bearer synthetic-caller")
		response := httptest.NewRecorder()
		if err := inst.ServeHTTP(response, request); err != nil {
			t.Fatal(err)
		}
		if response.Code != 204 {
			t.Fatalf("generation %d returned %d", n, response.Code)
		}
	}
}

func TestPolicyHealthPinsCredentialToTarget(t *testing.T) {
	cfg, opts := policyFixture(t, "isolated")
	opts.BackendOptions.LlamaServerOptions.APIKey = "synthetic-original"
	var inst *Instance
	var next *Options
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer synthetic-original" {
			t.Error("new generation credential leaked to old health target")
			w.WriteHeader(401)
			return
		}
		if calls == 1 {
			// Simulate an options generation changing during an in-flight health
			// poll, without executing any process or remote operation.
			inst.options.set(next)
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	host, portText, _ := net.SplitHostPort(server.Listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	opts.BackendOptions.LlamaServerOptions.Host = host
	opts.BackendOptions.LlamaServerOptions.Port = port
	var err error
	inst, err = NewChecked("health-generation", cfg, opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	next, err = inst.GetOptions().Clone()
	if err != nil {
		t.Fatal(err)
	}
	next.BackendOptions.LlamaServerOptions.APIKey = "synthetic-next"
	next.BackendOptions.LlamaServerOptions.Port = port + 1
	inst.SetStatus(Running)
	defer inst.SetStatus(Stopped)
	if err := inst.WaitForHealthy(3); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyHealthDoesNotFollowRedirect(t *testing.T) {
	leaked := make(chan bool, 4)
	wrong := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked <- true }))
	defer wrong.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, wrong.URL+"/wrong", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	cfg, o := policyFixture(t, "isolated")
	host, portText, _ := net.SplitHostPort(server.Listener.Addr().String())
	port, _ := strconv.Atoi(portText)
	o.BackendOptions.LlamaServerOptions.Host = host
	o.BackendOptions.LlamaServerOptions.Port = port
	o.BackendOptions.LlamaServerOptions.APIKey = "synthetic-key"
	inst, err := NewChecked("redirect", cfg, o, nil)
	if err != nil {
		t.Fatal(err)
	}
	inst.SetStatus(Running)
	defer inst.SetStatus(Stopped)
	if err := inst.WaitForHealthy(1); err == nil {
		t.Fatal("redirect counted healthy")
	}
	select {
	case <-leaked:
		t.Fatal("health followed wrong path")
	default:
	}
}

func TestPolicyProxyAndHealthCredentials(t *testing.T) {
	for _, source := range []string{"typed", "environment"} {
		t.Run(source, func(t *testing.T) {
			received := make(chan string, 4)
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received <- r.Header.Get("Authorization")
				if r.Header.Get("Authorization") != "Bearer synthetic-backend" {
					w.WriteHeader(401)
					return
				}
				w.WriteHeader(204)
			}))
			defer backend.Close()
			host, portText, _ := net.SplitHostPort(backend.Listener.Addr().String())
			port, _ := strconv.Atoi(portText)
			cfg, opts := policyFixture(t, "isolated")
			opts.BackendOptions.LlamaServerOptions.Host = host
			opts.BackendOptions.LlamaServerOptions.Port = port
			if source == "typed" {
				opts.BackendOptions.LlamaServerOptions.APIKey = "synthetic-backend"
			} else {
				opts.Environment = map[string]string{"LLAMA_API_KEY": "synthetic-backend"}
			}
			inst := New("auth", cfg, opts, nil)
			req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			req.Header.Set("Authorization", "Bearer synthetic-caller")
			w := httptest.NewRecorder()
			if err := inst.ServeHTTP(w, req); err != nil {
				t.Fatal(err)
			}
			if w.Code != 204 {
				t.Errorf("proxy status = %d", w.Code)
			}
			if got := <-received; got != "Bearer synthetic-backend" {
				t.Errorf("wrong proxy credential: %q", got)
			}
			inst.SetStatus(Running)
			if err := inst.WaitForHealthy(1); err != nil {
				t.Error(err)
			}
			inst.SetStatus(Stopped)
			if got := <-received; got != "Bearer synthetic-backend" {
				t.Errorf("wrong health credential: %q", got)
			}
		})
	}
}
