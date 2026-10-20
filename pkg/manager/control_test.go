package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"llamactl/pkg/instance"
	"llamactl/pkg/resourcepolicy"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type p3Store struct {
	mu      sync.Mutex
	fail    bool
	entered chan struct{}
	proceed chan struct{}
	saved   []*instance.Options
}

func (s *p3Store) Save(i *instance.Instance) error {
	s.mu.Lock()
	fail, entered, proceed := s.fail, s.entered, s.proceed
	s.mu.Unlock()
	if entered != nil {
		close(entered)
		<-proceed
	}
	if fail {
		return errors.New("synthetic persist failure")
	}
	opts, err := i.GetOptions().Clone()
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.saved = append(s.saved, opts)
	s.mu.Unlock()
	return nil
}
func (*p3Store) Delete(string) error                    { return nil }
func (*p3Store) LoadAll() ([]*instance.Instance, error) { return nil, nil }
func (*p3Store) Close() error                           { return nil }

type p3Backend struct {
	mu          sync.Mutex
	model       string
	status      string
	healthy     bool
	requests    []string
	loadStarted chan struct{}
	holdLoad    bool
}

func (b *p3Backend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	b.requests = append(b.requests, r.Method+" "+r.URL.RequestURI())
	healthy := b.healthy
	if r.URL.Path == "/health" {
		b.mu.Unlock()
		if !healthy {
			w.WriteHeader(503)
		} else {
			fmt.Fprint(w, `{"status":"ok"}`)
		}
		return
	}
	if r.URL.Path == "/props" {
		b.mu.Unlock()
		fmt.Fprint(w, `{"default_generation_settings":{}}`)
		return
	}
	if r.URL.Path == "/models/load" {
		if !healthy {
			b.mu.Unlock()
			w.WriteHeader(503)
			return
		}
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		b.model = body.Model
		b.status = "loaded"
		if b.holdLoad {
			b.status = "loading"
		}
		signal := b.loadStarted
		b.loadStarted = nil
		b.mu.Unlock()
		if signal != nil {
			close(signal)
		}
		fmt.Fprint(w, `{"success":true}`)
		return
	}
	if r.URL.Path == "/models/unload" {
		b.model = ""
		b.status = "unloaded"
		b.mu.Unlock()
		fmt.Fprint(w, `{"success":true}`)
		return
	}
	if r.URL.Path == "/models" {
		model, status := b.model, b.status
		b.mu.Unlock()
		a, z := "unloaded", "unloaded"
		if model == "alpha" {
			a = status
		}
		if model == "beta" {
			z = status
		}
		fmt.Fprintf(w, `{"data":[{"id":"alpha","status":{"value":%q}},{"id":"beta","status":{"value":%q}}]}`, a, z)
		return
	}
	b.mu.Unlock()
	fmt.Fprint(w, `{"ok":true}`)
}
func p3Manager(t *testing.T) (*instanceManager, *instance.Instance, *p3Backend, *p3Store) {
	t.Helper()
	im, _ := presetManager(t)
	store := &p3Store{}
	im.db = store
	g, err := resourcepolicy.Open("")
	if err != nil {
		t.Fatal(err)
	}
	im.globalConfig.ResourcePolicy = g
	backend := &p3Backend{healthy: true, status: "unloaded"}
	srv := httptest.NewServer(backend)
	t.Cleanup(srv.Close)
	im.ports = newPortAllocator(1024, 65535)
	path := filepath.Join(t.TempDir(), "synthetic.gguf")
	if err := os.WriteFile(path, []byte("not a model"), 0600); err != nil {
		t.Fatal(err)
	}
	var opts instance.Options
	if err := json.Unmarshal([]byte(`{"controlled":true,"environment_policy":"isolated","backend_type":"llama_cpp","backend_options":{"no_models_autoload":true,"models_max":1}}`), &opts); err != nil {
		t.Fatal(err)
	}
	opts.Control = &instance.ControlConfig{Models: map[string]instance.ControlModel{"alpha": {Path: path}, "beta": {Path: path}}}
	opts.BackendOptions.SetPort(srv.Listener.Addr().(*net.TCPAddr).Port)
	inst, err := im.CreateInstance("p3", &opts)
	if err != nil {
		t.Fatal(err)
	}
	// Only synthetic lifecycle is replaced. Production persistence, gate, ledger,
	// HTTP load/unload and model-state polling remain exercised.
	c := im.initControl()
	c.start = func(_ context.Context, _ *instance.Maintenance, i *instance.Instance) error {
		i.SetStatus(instance.Running)
		return nil
	}
	c.stop = func(_ context.Context, _ *instance.Maintenance, i *instance.Instance) error {
		i.SetStatus(instance.Stopped)
		return nil
	}
	c.ready = func(ctx context.Context, m *instance.Maintenance, model string) error {
		if err := m.WaitModel(ctx, model, "loaded"); err != nil {
			return err
		}
		_, err := m.RouterRequest(ctx, "GET", "/props", model)
		return err
	}
	// The fake listener is deliberately not an owned OS process. Authorize this
	// test-only stopped state directly; real recovery is tested via HTTP/port checks.
	s := inst.ControlSnapshot()
	m, err := inst.BeginControl(s.BootID, s.Revision, true, true)
	if err != nil {
		t.Fatal(err)
	}
	m.FinishControl("stopped", "")
	m.Close()
	t.Cleanup(im.shutdownControl)
	return im, inst, backend, store
}
func p3Payload(t *testing.T, inst *instance.Instance, key, action, model, patch string) []byte {
	t.Helper()
	s := inst.ControlSnapshot()
	v := map[string]any{"boot_id": s.BootID, "expected_revision": s.Revision, "idempotency_key": key, "action": action}
	if model != "" {
		v["model"] = model
	}
	if patch != "" {
		v["patch"] = json.RawMessage(patch)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func p3Wait(t *testing.T, im *instanceManager, id string) ControlOperation {
	t.Helper()
	until := time.Now().Add(4 * time.Second)
	for time.Now().Before(until) {
		op, err := im.GetControlOperation("p3", "scope", id, true)
		if err != nil {
			t.Fatal(err)
		}
		if op.EndedAt != nil {
			return op
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("operation did not finish")
	return ControlOperation{}
}
func p3Submit(t *testing.T, im *instanceManager, inst *instance.Instance, key, action, model, patch string) ControlOperation {
	t.Helper()
	op, err := im.SubmitControl("p3", "scope", p3Payload(t, inst, key, action, model, patch))
	if err != nil {
		t.Fatal(err)
	}
	return p3Wait(t, im, op.ID)
}
func p3Code(err error) string {
	var a *instance.AdmissionError
	if errors.As(err, &a) {
		return a.Code
	}
	return fmt.Sprint(err)
}

func TestP3ApplyWaitsForRouterHealth(t *testing.T) {
	im, inst, backend, _ := p3Manager(t)
	backend.mu.Lock()
	backend.healthy = false
	backend.mu.Unlock()
	op, err := im.SubmitControl("p3", "scope", p3Payload(t, inst, "apply", "apply", "alpha", `{"gpu_layers":0,"flash_attention":false}`))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	backend.mu.Lock()
	backend.healthy = true
	backend.mu.Unlock()
	result := p3Wait(t, im, op.ID)
	backend.mu.Lock()
	requests := append([]string(nil), backend.requests...)
	backend.mu.Unlock()
	if len(requests) == 0 || requests[0] != "GET /health" {
		t.Fatalf("load dispatched without Router health barrier: %v", requests)
	}
	if result.State != "succeeded" || result.ReadyToken == "" {
		t.Fatalf("transient startup treated as terminal: %+v", result)
	}
}

func TestP3CASIdempotencyAndPersistence(t *testing.T) {
	im, inst, _, store := p3Manager(t)
	body := p3Payload(t, inst, "apply", "apply", "alpha", `{"threads":0,"context":2048,"flash_attention":false}`)
	op, err := im.SubmitControl("p3", "scope", body)
	if err != nil {
		t.Fatal(err)
	}
	result := p3Wait(t, im, op.ID)
	if result.State != "succeeded" {
		t.Fatal(result)
	}
	equivalent := strings.Replace(string(body), `"context":2048`, `"context":2.048e3`, 1)
	repeat, err := im.SubmitControl("p3", "scope", []byte(equivalent))
	if err != nil || repeat.ID != op.ID {
		t.Fatalf("normalized retry after revision change: %+v %v", repeat, err)
	}
	changed := strings.Replace(string(body), `"flash_attention":false`, `"flash_attention":true`, 1)
	if _, err := im.SubmitControl("p3", "scope", []byte(changed)); p3Code(err) != "idempotency_conflict" {
		t.Fatal(err)
	}
	for _, replacement := range []string{`"threads":null`, `"threads":1`} {
		b := strings.Replace(string(body), `"threads":0`, replacement, 1)
		if _, err := im.SubmitControl("p3", "scope", []byte(b)); p3Code(err) != "idempotency_conflict" {
			t.Fatal(err)
		}
	}
	stale := strings.Replace(string(body), `"idempotency_key":"apply"`, `"idempotency_key":"stale"`, 1)
	if _, err := im.SubmitControl("p3", "scope", []byte(stale)); p3Code(err) != "revision_conflict" {
		t.Fatal(err)
	}
	store.mu.Lock()
	persisted := store.saved[len(store.saved)-1]
	store.mu.Unlock()
	if persisted.ConfigRevision != 1 || inst.GetOptions().ConfigRevision != 1 || !strings.Contains(*persisted.PresetIni, "threads = 0") || !strings.Contains(*persisted.PresetIni, "flash-attn = off") {
		t.Fatal("lost explicit values or revision")
	}
	// Lookup precedes busy: an accepted repeat is visible during another use.
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"p3/alpha"}`))
	r.Header.Set("X-Ready-Token", result.ReadyToken)
	release, err := inst.PrepareInferenceRequest(r)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := im.SubmitControl("p3", "scope", []byte(equivalent)); err != nil {
		t.Fatal(err)
	}
	if _, err := im.SubmitControl("p3", "scope", p3Payload(t, inst, "busy", "stop", "", "")); p3Code(err) != "busy" {
		t.Fatal(err)
	}
}

func TestP3CapacityExpiryAndScope(t *testing.T) {
	im, inst, _, _ := p3Manager(t)
	c := im.initControl()
	c.capacity = 1
	op := p3Submit(t, im, inst, "one", "stop", "", "")
	if op.State != "succeeded" {
		t.Fatal(op)
	}
	if _, err := im.SubmitControl("p3", "scope", p3Payload(t, inst, "two", "stop", "", "")); p3Code(err) != "operation_capacity" {
		t.Fatal(err)
	}
	if _, err := im.GetControlOperation("p3", "other", op.ID, false); p3Code(err) != "operation_not_found" {
		t.Fatal(err)
	}
	c.mu.Lock()
	future := op.EndedAt.Add(24*time.Hour + time.Second)
	c.now = func() time.Time { return future }
	c.mu.Unlock()
	if _, err := im.GetControlOperation("p3", "scope", op.ID, false); p3Code(err) != "operation_not_found" {
		t.Fatal(err)
	}
	if result := p3Submit(t, im, inst, "two", "stop", "", ""); result.State != "succeeded" {
		t.Fatal(result)
	}
}

func TestP3PersistFailureDoesNotAcceptRevision(t *testing.T) {
	im, inst, _, store := p3Manager(t)
	store.mu.Lock()
	store.fail = true
	store.mu.Unlock()
	result := p3Submit(t, im, inst, "fail", "apply", "alpha", `{"context":2048}`)
	if result.ErrorCode != "persist_failed" || inst.GetOptions().ConfigRevision != 0 || inst.IsRunning() {
		t.Fatalf("persist failure accepted: %+v", result)
	}
	if _, err := im.SubmitControl("p3", "scope", p3Payload(t, inst, "retry-new", "load", "alpha", "")); p3Code(err) != "recovery_required" {
		t.Fatal(err)
	}
}

func TestP3StopAfterFailureAndPausedUnload(t *testing.T) {
	t.Run("stop-failure", func(t *testing.T) {
		im, inst, _, _ := p3Manager(t)
		im.control.stop = func(context.Context, *instance.Maintenance, *instance.Instance) error {
			return errors.New("synthetic stop failure")
		}
		failed := p3Submit(t, im, inst, "failed", "stop", "", "")
		if failed.ErrorCode != "stop_failed" {
			t.Fatal(failed)
		}
		if _, err := im.SubmitControl("p3", "scope", p3Payload(t, inst, "blocked", "load", "alpha", "")); p3Code(err) != "recovery_required" {
			t.Fatal(err)
		}
		im.control.stop = func(_ context.Context, _ *instance.Maintenance, i *instance.Instance) error {
			i.SetStatus(instance.Stopped)
			return nil
		}
		stopped := p3Submit(t, im, inst, "explicit-stop", "stop", "", "")
		if stopped.State != "succeeded" {
			t.Fatal(stopped)
		}
	})
	t.Run("paused-unload", func(t *testing.T) {
		im, inst, _, _ := p3Manager(t)
		loaded := p3Submit(t, im, inst, "apply", "apply", "alpha", `{}`)
		if loaded.State != "succeeded" {
			t.Fatal(loaded)
		}
		g := im.globalConfig.ResourcePolicy
		s := g.Snapshot()
		if _, err := g.Update(false, s.BootID, s.Revision); err != nil {
			t.Fatal(err)
		}
		result := p3Submit(t, im, inst, "unload", "unload", "", "")
		if result.State != "succeeded" {
			t.Fatal(result)
		}
	})
}

func TestP3InterruptedRequiresAdministratorEvenAfterStop(t *testing.T) {
	im, inst, b, _ := p3Manager(t)
	b.holdLoad = true
	im.control.budget = 40 * time.Millisecond
	result := p3Submit(t, im, inst, "timeout", "apply", "alpha", `{}`)
	if result.State != "deadline_exceeded" {
		t.Fatalf("deadline not enforced: %+v", result)
	}
	stopped := p3Submit(t, im, inst, "stop", "stop", "", "")
	if stopped.State != "succeeded" {
		t.Fatal(stopped)
	}
	if _, err := im.SubmitControl("p3", "scope", p3Payload(t, inst, "blind-replay", "load", "alpha", "")); p3Code(err) != "recovery_required" {
		t.Fatalf("interrupted operation reopened without administrator: %v", err)
	}
}

func TestP3StartRunningDoesNotLoseLoadedIdentity(t *testing.T) {
	im, inst, _, _ := p3Manager(t)
	ready := p3Submit(t, im, inst, "apply", "apply", "alpha", `{}`)
	if ready.State != "succeeded" {
		t.Fatal(ready)
	}
	if _, err := im.SubmitControl("p3", "scope", p3Payload(t, inst, "start-again", "start", "", "")); p3Code(err) != "already_running" {
		t.Fatalf("running start accepted and discards model identity: %v", err)
	}
	if s := inst.ControlSnapshot(); s.ReadyToken != ready.ReadyToken || s.ActualModel != "alpha" {
		t.Fatal("invalid request changed ready state")
	}
}

func TestP3OwnerPauseCancelsDetachedLoad(t *testing.T) {
	im, inst, b, _ := p3Manager(t)
	b.holdLoad = true
	b.loadStarted = make(chan struct{})
	entered := b.loadStarted
	var starts atomic.Int32
	im.control.start = func(_ context.Context, _ *instance.Maintenance, i *instance.Instance) error {
		starts.Add(1)
		i.SetStatus(instance.Running)
		return nil
	}
	op, err := im.SubmitControl("p3", "scope", p3Payload(t, inst, "load", "apply", "alpha", `{}`))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("load not started")
	}
	g := im.globalConfig.ResourcePolicy
	s := g.Snapshot()
	s, err = g.Update(false, s.BootID, s.Revision)
	if err != nil {
		t.Fatal(err)
	}
	result := p3Wait(t, im, op.ID)
	if result.State != "interrupted" {
		t.Fatalf("pause did not cancel load: %+v", result)
	}
	if _, err = g.Update(true, s.BootID, s.Revision); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if starts.Load() != 1 || inst.ControlSnapshot().ReadyToken != "" {
		t.Fatal("resume replayed old work")
	}
}
