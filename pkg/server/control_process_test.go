package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"llamactl/pkg/config"
	"llamactl/pkg/database"
	"llamactl/pkg/instance"
	"llamactl/pkg/manager"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// This test binary doubles as a synthetic Router. It never opens a model file,
// invokes an engine, or uses a GPU. All native process/gate/preset/probe code runs.
func TestMain(m *testing.M) {
	if os.Getenv("P3_SYNTHETIC_ENGINE") == "1" {
		os.Exit(p3SyntheticEngine())
	}
	os.Exit(m.Run())
}
func p3SyntheticEngine() int {
	var mu sync.Mutex
	model := ""
	state := "unloaded"
	loadSerial := 0
	mode := os.Getenv("P3_SYNTHETIC_MODE")
	modelIDs := []string{"alpha"}
	if os.Getenv("N1_SYNTHETIC_MODELS") == "1" {
		for idx, arg := range os.Args {
			if arg == "--models-preset" && idx+1 < len(os.Args) {
				data, err := os.ReadFile(os.Args[idx+1])
				if err != nil {
					return 82
				}
				modelIDs = nil
				for _, line := range strings.Split(string(data), "\n") {
					if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
						modelIDs = append(modelIDs, strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"))
					}
				}
			}
		}
	}
	generations := 0
	pause := func(r *http.Request, delay time.Duration) bool {
		select {
		case <-time.After(delay):
			return true
		case <-r.Context().Done():
			return false
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+os.Getenv("P3_SYNTHETIC_PORT"))
	if err != nil {
		return 81
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, key := range []string{"X-Ready-Token", "X-Service-Use", "X-Prompt-Template-Version", "X-Prompt-Digest", "X-Request-Timeout-Seconds", "X-Boot-ID", "X-Config-Revision", "X-Recipe-ID", "X-Recipe-Version"} {
			if r.Header.Get(key) != "" {
				w.WriteHeader(500)
				return
			}
		}
		if path := os.Getenv("P3_SYNTHETIC_EVENTS"); path != "" {
			if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600); err == nil {
				_, _ = fmt.Fprintln(f, r.URL.Path)
				_ = f.Close()
			}
		}
		switch r.URL.Path {
		case "/health":
			if mode == "health_error" {
				w.WriteHeader(503)
				return
			}
			io.WriteString(w, `{"status":"ok"}`)
		case "/models/load":
			// A test-owned marker makes a later load fail without changing artifact
			// identity, publication evidence or running a real engine/parser.
			if path := os.Getenv("N1_SYNTHETIC_LOAD_FAILURE_FILE"); path != "" {
				if _, err := os.Stat(path); err == nil {
					w.WriteHeader(500)
					io.WriteString(w, "private/path secret-backend-body")
					return
				}
			}
			var b struct {
				Model string `json:"model"`
			}
			_ = json.NewDecoder(r.Body).Decode(&b)
			mu.Lock()
			model = b.Model
			state = "loading"
			loadSerial++
			serial := loadSerial
			mu.Unlock()
			go func() {
				delay := 150 * time.Millisecond
				if mode == "slow_load" {
					delay = 10 * time.Second
				}
				time.Sleep(delay)
				mu.Lock()
				if model == b.Model && loadSerial == serial {
					state = "loaded"
				}
				mu.Unlock()
			}()
			if mode == "load_error_ack" {
				w.WriteHeader(500)
				io.WriteString(w, "private/path secret-backend-body")
				return
			}
			io.WriteString(w, `{"success":true}`)
		case "/models/unload":
			if mode == "unload_error" {
				w.WriteHeader(500)
				io.WriteString(w, "private/path secret-backend-body")
				return
			}
			if mode == "unload_slow" && !pause(r, 10*time.Second) {
				return
			}
			mu.Lock()
			model = ""
			state = "unloaded"
			loadSerial++
			mu.Unlock()
			io.WriteString(w, `{"success":true}`)
		case "/models":
			if mode == "malformed_models" {
				io.WriteString(w, `{broken private/path secret-backend-body}`)
				return
			}
			mu.Lock()
			s, loadedModel := state, model
			mu.Unlock()
			entries := []map[string]any{}
			for _, id := range modelIDs {
				value := "unloaded"
				if id == loadedModel {
					value = s
				}
				entries = append(entries, map[string]any{"id": id, "status": map[string]string{"value": value}})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": entries})
		case "/props":
			validModel := false
			for _, id := range modelIDs {
				validModel = validModel || r.URL.Query().Get("model") == id
			}
			if !validModel || r.URL.Query().Get("autoload") != "false" {
				w.WriteHeader(400)
				return
			}
			io.WriteString(w, `{"default_generation_settings":{}}`)
		case "/v1/chat/completions", "/completion":
			if os.Getenv("N1_SYNTHETIC_MODELS") == "1" {
				for _, key := range []string{"X-Recipe-ID", "X-Recipe-Version", "X-Boot-ID", "X-Config-Revision", "X-Process-Generation", "X-Load-Generation", "X-Prompt-Template-Version", "X-Prompt-Digest"} {
					w.Header().Add(key, "synthetic-backend-spoof")
					w.Header().Add(key, "second-spoof")
				}
			}
			var b struct {
				Model         string   `json:"model"`
				Stream        bool     `json:"stream"`
				MaxTokens     *int     `json:"max_tokens"`
				Seed          *int64   `json:"seed"`
				Temperature   *float64 `json:"temperature"`
				CachePrompt   *bool    `json:"cache_prompt"`
				StreamOptions struct {
					IncludeUsage bool `json:"include_usage"`
				} `json:"stream_options"`
				Messages []struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"messages"`
			}
			_ = json.NewDecoder(r.Body).Decode(&b)
			if mode != "" {
				valid := b.Stream && b.MaxTokens != nil && *b.MaxTokens >= 1 && *b.MaxTokens <= 256 && b.Seed != nil && *b.Seed >= 0 && b.Temperature != nil && *b.Temperature == 0 && b.CachePrompt != nil && !*b.CachePrompt && b.StreamOptions.IncludeUsage && len(b.Messages) == 1
				if valid {
					valid = b.Messages[0].Role == "user" && fmt.Sprintf("%x", sha256.Sum256([]byte(b.Messages[0].Content))) == instance.DeploymentFixtureDigest()
				}
				if !valid {
					w.WriteHeader(422)
					return
				}
			}
			mu.Lock()
			ready := model == b.Model && state == "loaded"
			mu.Unlock()
			if !ready {
				w.WriteHeader(409)
				return
			}
			if b.Stream {
				mu.Lock()
				generations++
				number := generations
				mu.Unlock()
				if mode == "error" || (mode == "second_error" && number == 2) {
					w.WriteHeader(500)
					io.WriteString(w, "private/path secret-backend-body")
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if mode == "malformed_stream" {
					io.WriteString(w, "data: {broken private/path secret-backend-body}\n\n")
					return
				}
				io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"reasoning_content\":\"synthetic control only\"}}]}\n\n")
				w.(http.Flusher).Flush()
				delay := 120 * time.Millisecond
				if mode == "slow" {
					delay = 10 * time.Second
				}
				if !pause(r, delay) {
					return
				}
				io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"synthetic\"}}]}\n\n")
				w.(http.Flusher).Flush()
				if !pause(r, 300*time.Millisecond) {
					return
				}
				if mode != "no_usage" {
					io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":21,\"completion_tokens\":3},\"timings\":{\"prompt_ms\":5,\"predicted_ms\":8}}\n\n")
				}
				io.WriteString(w, "data: [DONE]\n\n")
			} else {
				_ = json.NewEncoder(w).Encode(map[string]any{"model": b.Model, "choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": "synthetic"}, "finish_reason": "stop"}}})
			}
		default:
			w.WriteHeader(404)
		}
	})}
	go srv.Serve(ln)
	eof := make(chan struct{})
	go func() { io.Copy(io.Discard, os.Stdin); close(eof) }()
	interrupted := make(chan os.Signal, 1)
	signal.Notify(interrupted, os.Interrupt)
	defer signal.Stop(interrupted)
	lifetime := 30 * time.Second
	if os.Getenv("P3_SYNTHETIC_BROWSER") == "1" {
		lifetime = time.Hour
	}
	select {
	case <-eof:
	case <-interrupted:
	case <-time.After(lifetime):
	}
	_ = srv.Close()
	return 0
}

func TestP3ActualHTTPApplyReadyStreamUnloadReloadStop(t *testing.T) {
	contract := readControlAPI(t)
	cfg := config.AppConfig{}
	cfg.DataDir = t.TempDir()
	cfg.Instances.InstancesDir = t.TempDir()
	cfg.Instances.LogsDir = t.TempDir()
	cfg.Instances.PortRange = [2]int{1024, 65535}
	cfg.Instances.MaxInstances = -1
	cfg.Auth.RequireManagementAuth = true
	cfg.Auth.RequireInferenceAuth = true
	cfg.Auth.ManagementKeys = []string{"synthetic-management-key"}
	cfg.Backends.LlamaCpp.Command = os.Args[0]
	db, err := database.Open(&database.Config{Path: filepath.Join(t.TempDir(), "synthetic.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := database.RunMigrations(db); err != nil {
		t.Fatal(err)
	}
	im := manager.New(&cfg, db)
	defer im.Shutdown()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	path := filepath.Join(t.TempDir(), "synthetic.gguf")
	if err := os.WriteFile(path, []byte("not a model"), 0600); err != nil {
		t.Fatal(err)
	}
	var opts instance.Options
	if err := json.Unmarshal([]byte(`{"controlled":true,"environment_policy":"isolated","backend_type":"llama_cpp","backend_options":{"no_models_autoload":true,"models_max":1}}`), &opts); err != nil {
		t.Fatal(err)
	}
	opts.Control = &instance.ControlConfig{Models: map[string]instance.ControlModel{"alpha": {Path: path}}}
	opts.BackendOptions.SetPort(port)
	opts.Environment = map[string]string{"P3_SYNTHETIC_ENGINE": "1", "P3_SYNTHETIC_PORT": strconv.Itoa(port)}
	inst, err := im.CreateInstance("router", &opts)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(SetupRouter(NewHandler(im, nil, cfg, db, nil)))
	defer front.Close()
	client := front.Client()
	client.Timeout = 10 * time.Second
	call := func(ctx context.Context, method, route string, body any, token string) (int, []byte) {
		t.Helper()
		var data []byte
		if body != nil {
			data, _ = json.Marshal(body)
		}
		req, _ := http.NewRequestWithContext(ctx, method, front.URL+route, bytes.NewReader(data))
		req.Header.Set("Authorization", "Bearer synthetic-management-key")
		if token != "" {
			req.Header.Set("X-Ready-Token", token)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, err = io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(route, "/control") || strings.Contains(route, "/operations") {
			path := strings.Replace(route, "/router/", "/{name}/", 1)
			if idx := strings.Index(path, "/operations/"); idx >= 0 {
				path = path[:idx] + "/operations/{id}"
			}
			contract.response(t, method, path, resp.StatusCode, data)
		}
		return resp.StatusCode, data
	}
	cm := im.(manager.ControlManager)
	s, err := cm.ControlStatus("router")
	if err != nil {
		t.Fatal(err)
	}
	status, b := call(context.Background(), "PUT", "/api/v1/instances/router/control", map[string]any{"boot_id": s.BootID, "expected_revision": s.Revision, "verified_stopped": true}, "")
	if status != 200 {
		t.Fatalf("recover %d %s", status, b)
	}
	submit := func(action, key string, extra map[string]any) manager.ControlOperation {
		t.Helper()
		s, _ := cm.ControlStatus("router")
		v := map[string]any{"action": action, "idempotency_key": key, "boot_id": s.BootID, "expected_revision": s.Revision}
		for k, x := range extra {
			v[k] = x
		}
		ctx, cancel := context.WithCancel(context.Background())
		status, b := call(ctx, "POST", "/api/v1/instances/router/operations", v, "")
		cancel()
		if status != 202 {
			t.Fatalf("submit %s: %d %s", action, status, b)
		}
		var op manager.ControlOperation
		if err := json.Unmarshal(b, &op); err != nil {
			t.Fatal(err)
		}
		if op.Deadline.Sub(op.StartedAt) != 600*time.Second {
			t.Fatal("default deadline is not 600s")
		}
		until := time.Now().Add(8 * time.Second)
		for time.Now().Before(until) {
			status, b = call(context.Background(), "GET", "/api/v1/instances/router/operations/"+op.ID, nil, "")
			if status != 200 {
				t.Fatal(string(b))
			}
			_ = json.Unmarshal(b, &op)
			if op.EndedAt != nil {
				if op.State != "succeeded" {
					t.Fatalf("operation failed: %+v", op)
				}
				return op
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("operation timeout")
		return op
	}
	ready := submit("apply", "apply", map[string]any{"model": "alpha", "patch": map[string]any{"context": 2048, "gpu_layers": 0, "flash_attention": false}})
	if ready.Revision != 1 || ready.ReadyToken == "" {
		t.Fatal(ready)
	}
	persisted, err := db.GetByName(context.Background(), "router")
	if err != nil || persisted.GetOptions().ConfigRevision != 1 {
		t.Fatal("persisted revision missing", err)
	}
	for _, route := range []string{"/api/v1/instances/router/start", "/api/v1/instances/router/stop", "/api/v1/instances/router/restart", "/api/v1/llama-cpp/router/models/alpha/load", "/api/v1/instances/router/proxy/models/load"} {
		status, b = call(context.Background(), "POST", route, map[string]string{"model": "alpha"}, "")
		if status != 409 || !strings.Contains(string(b), "maintenance_required") {
			t.Fatalf("native bypass %s: %d %s", route, status, b)
		}
	}
	req, _ := http.NewRequest("POST", front.URL+"/v1/chat/completions", strings.NewReader(`{"model":"router/alpha","stream":true}`))
	req.Header.Set("Authorization", "Bearer synthetic-management-key")
	req.Header.Set("X-Ready-Token", ready.ReadyToken)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("inference: %d", resp.StatusCode)
	}
	reader := bufio.NewReader(resp.Body)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	s, _ = cm.ControlStatus("router")
	status, b = call(context.Background(), "POST", "/api/v1/instances/router/operations", map[string]any{"action": "stop", "boot_id": s.BootID, "expected_revision": s.Revision, "idempotency_key": "busy"}, "")
	if status != 409 || !strings.Contains(string(b), "busy") {
		t.Fatalf("stream maintenance admitted: %d %s", status, b)
	}
	stream, err := io.ReadAll(reader)
	resp.Body.Close()
	if err != nil || !bytes.Contains(stream, []byte("[DONE]")) {
		t.Fatalf("stream completion: %s %v", stream, err)
	}
	submit("unload", "unload", nil)
	status, _ = call(context.Background(), "POST", "/v1/chat/completions", map[string]any{"model": "router/alpha"}, ready.ReadyToken)
	if status != 409 {
		t.Fatalf("unloaded token accepted: %d", status)
	}
	reloaded := submit("load", "load", map[string]any{"model": "alpha"})
	if reloaded.ReadyToken == ready.ReadyToken {
		t.Fatal("reloaded token reused")
	}
	status, _ = call(context.Background(), "POST", "/v1/chat/completions", map[string]any{"model": "router/alpha"}, ready.ReadyToken)
	if status != 409 {
		t.Fatal("old load token accepted")
	}
	status, b = call(context.Background(), "POST", "/v1/chat/completions", map[string]any{"model": "router/alpha"}, reloaded.ReadyToken)
	if status != 200 {
		t.Fatalf("new token refused: %d %s", status, b)
	}
	for _, route := range []string{"/llama-cpp/router/completion", "/llama-cpp/router/v1/chat/completions", "/api/v1/instances/router/proxy/v1/chat/completions"} {
		for _, tc := range []struct {
			model, token string
			want         int
		}{{"beta", reloaded.ReadyToken, 409}, {"alpha", "", 409}, {"alpha", reloaded.ReadyToken, 200}} {
			status, b = call(context.Background(), "POST", route, map[string]any{"model": tc.model}, tc.token)
			if status != tc.want {
				t.Fatalf("inference adapter %s: %d %s", route, status, b)
			}
		}
	}
	submit("stop", "stop", nil)
	for _, stream := range []bool{false, true} {
		out, err := runP3Example(t, front.URL, "router", stream)
		if err != nil || !bytes.Contains(out, []byte("owned stop succeeded")) {
			t.Fatalf("stdlib client stream=%v: %v %s", stream, err, out)
		}
	}
	s, _ = cm.ControlStatus("router")
	if inst.IsRunning() {
		t.Fatal("stop returned before process exit")
	}
	status, b = call(context.Background(), "PUT", "/api/v1/instances/router/control", map[string]any{"boot_id": s.BootID, "expected_revision": s.Revision, "verified_stopped": true}, "")
	if status != 200 {
		t.Fatalf("owned process/port recovery verification: %d %s", status, b)
	}
}
