package instance

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type controlRuntime struct {
	boot              string
	state             string
	active            string
	model             string
	token             string
	fingerprint       string
	processGeneration uint64
	loadGeneration    uint64
	ownerRevision     uint64
	previous          *Options
	interrupted       bool
}

type ControlSnapshot struct {
	ProtocolVersion             string                                `json:"protocol_version"`
	Actions                     []string                              `json:"actions"`
	OperationBudgetSeconds      int64                                 `json:"operation_budget_seconds"`
	OperationCapacity           int                                   `json:"operation_capacity"`
	IdempotencyRetentionSeconds int64                                 `json:"idempotency_retention_seconds"`
	Admission                   string                                `json:"admission"`
	BootID                      string                                `json:"boot_id"`
	Revision                    uint64                                `json:"revision"`
	State                       string                                `json:"state"`
	Running                     bool                                  `json:"running"`
	Target                      string                                `json:"target_model,omitempty"`
	ActualModel                 string                                `json:"actual_model,omitempty"`
	ReadyToken                  string                                `json:"ready_token,omitempty"`
	ActiveOperation             string                                `json:"active_operation,omitempty"`
	ProcessGeneration           uint64                                `json:"process_generation"`
	LoadGeneration              uint64                                `json:"load_generation"`
	Models                      []string                              `json:"models"`
	Parameters                  map[string]ParameterCapability        `json:"parameters"`
	ConfiguredParameters        map[string]map[string]json.RawMessage `json:"configured_parameters"`
	ReadyProbe                  string                                `json:"ready_probe"`
	RecoveryRequired            bool                                  `json:"recovery_required"`
}

func ProtocolRequired() error {
	return &AdmissionError{Code: "maintenance_required", Details: "use control/operations with boot_id and expected_revision"}
}
func controlError(code string) error { return &AdmissionError{Code: code, Details: code} }
func (i *Instance) ProtocolEnabled() bool {
	opts := i.GetOptions()
	return opts != nil && opts.Control != nil
}

func opaqueControlID() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func optionsFingerprint(opts *Options) string {
	b, _ := json.Marshal(opts)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// InitializeControl never reconstructs readiness or replays an old operation.
func (i *Instance) InitializeControl(boot string) {
	i.admission.mu.Lock()
	defer i.admission.mu.Unlock()
	if i.control.boot == "" {
		i.control.boot = boot
		i.control.state = "unknown"
	}
}

func (i *Instance) ControlSnapshot() ControlSnapshot {
	i.admission.mu.Lock()
	defer i.admission.mu.Unlock()
	opts := i.GetOptions()
	c := i.control
	s := ControlSnapshot{BootID: c.boot, State: c.state, Running: i.IsRunning(), ActualModel: c.model, ActiveOperation: c.active, ProcessGeneration: c.processGeneration, LoadGeneration: c.loadGeneration, Parameters: ControlParameters(), ReadyProbe: "router_health+loaded+target_props_no_autoload", ConfiguredParameters: map[string]map[string]json.RawMessage{}}
	s.ProtocolVersion = "p3a/1"
	s.Actions = []string{"apply", "start", "stop", "load", "unload"}
	s.OperationBudgetSeconds, s.OperationCapacity, s.IdempotencyRetentionSeconds = 600, 1024, 86400
	s.Admission = "allowed"
	if i.policy != nil {
		s.Admission = i.policy.Snapshot().Admission
	}
	if s.State == "" {
		s.State = "unknown"
	}
	if opts != nil {
		s.Revision = opts.ConfigRevision
		if opts.Control != nil {
			s.Target = opts.Control.Target
			for id, m := range opts.Control.Models {
				s.Models = append(s.Models, id)
				cp := map[string]json.RawMessage{}
				for k, v := range m.Parameters {
					cp[k] = append(json.RawMessage(nil), v...)
				}
				s.ConfiguredParameters[id] = cp
			}
		}
	}
	sort.Strings(s.Models)
	if s.State == "ready" {
		if !i.readyCurrentLocked() {
			if i.readyIdentityLocked() {
				s.State = "running"
			} else {
				s.State = "unknown"
				s.ActualModel = ""
			}
		} else {
			s.ReadyToken = c.token
		}
	}
	if c.interrupted && c.active == "" {
		s.State = "interrupted"
		s.ReadyToken = ""
	}
	if s.State == "running" && c.active == "" && !s.Running {
		s.State = "unknown"
		s.ActualModel = ""
	}
	s.RecoveryRequired = s.State == "unknown" || s.State == "interrupted" || s.State == "failed"
	return s
}
func (i *Instance) readyIdentityLocked() bool {
	c := &i.control
	return c.token != "" && i.IsRunning() && c.fingerprint == optionsFingerprint(i.GetOptions())
}
func (i *Instance) readyCurrentLocked() bool {
	c := &i.control
	if !i.readyIdentityLocked() {
		return false
	}
	if i.policy != nil {
		s := i.policy.Snapshot()
		if !s.AllowAI || s.Revision != c.ownerRevision {
			return false
		}
	}
	return true
}

// BeginControl obtains the SAME maintenance gate as native lifecycle and proxy
// use. The caller serializes its ledger separately, but that ledger grants no use.
func (i *Instance) BeginControl(boot string, revision uint64, work, recovery bool) (*Maintenance, error) {
	return i.beginN1Control(boot, revision, work, recovery, nil)
}

func (i *Instance) beginN1Control(boot string, revision uint64, work, recovery bool, reservation *Reservation) (*Maintenance, error) {
	if !i.ProtocolEnabled() {
		return nil, controlError("control_not_enabled")
	}
	m, err := i.beginReservedMaintenance(work, reservation)
	if err != nil {
		return nil, err
	}
	m.protocol = true
	if m.permit == nil && i.policy != nil {
		m.permit = i.policy.AcquireCleanup()
	}
	i.admission.mu.Lock()
	switch {
	case i.control.boot != boot:
		err = controlError("epoch_changed")
	case i.GetOptions().ConfigRevision != revision:
		err = controlError("revision_conflict")
	case !recovery && (i.control.state == "unknown" || i.control.state == ""):
		err = controlError("recovery_required")
	case !recovery && work && (i.control.interrupted || i.control.state == "interrupted" || i.control.state == "failed" || (i.control.state == "running" && !i.IsRunning())):
		err = controlError("recovery_required")
	case !recovery && work && i.control.state == "ready" && !i.readyIdentityLocked():
		err = controlError("recovery_required")
	}
	i.admission.mu.Unlock()
	if err != nil {
		m.Close()
		return nil, err
	}
	return m, nil
}

func (m *Maintenance) BindControlContext(ctx context.Context) (context.Context, func()) {
	// Control work is cancelled even by graceful pause; ordinary SSE isn't.
	m.permit.CancelOnPause()
	return m.permit.BindContext(ctx)
}
func (m *Maintenance) CheckControl(ctx context.Context, work bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.valid(); err != nil {
		return err
	}
	if work {
		return m.checkPolicy()
	}
	// Stop/unload are allowed while paused, but a lease from an earlier allowed
	// epoch must never dispatch a new side effect after resume.
	return m.permit.CheckEpoch()
}

// ControlLastModel is only cleanup metadata, never a new readiness assertion.
func (i *Instance) ControlLastModel() string {
	i.admission.mu.Lock()
	defer i.admission.mu.Unlock()
	return i.control.model
}
func (m *Maintenance) ReserveControl(id string) {
	i := m.instance
	i.admission.mu.Lock()
	defer i.admission.mu.Unlock()
	i.control.active = id
}
func (m *Maintenance) ClearControlOperation() {
	i := m.instance
	i.admission.mu.Lock()
	defer i.admission.mu.Unlock()
	i.control.active = ""
}
func (m *Maintenance) ActivateControl(id string) {
	i := m.instance
	i.admission.mu.Lock()
	defer i.admission.mu.Unlock()
	i.control.previous = i.GetOptions()
	i.control.active = id
	i.control.token = ""
	i.control.loadGeneration++
	i.control.state = "running"
}
func (m *Maintenance) FinishControl(state, model string) {
	i := m.instance
	i.admission.mu.Lock()
	defer i.admission.mu.Unlock()
	i.control.state = state
	if state == "interrupted" {
		i.control.interrupted = true
	}
	i.control.active = ""
	i.control.model = model
	if state != "ready" {
		i.control.token = ""
	}
}
func (m *Maintenance) PublishReady(ctx context.Context, model string) error {
	if err := m.CheckControl(ctx, true); err != nil {
		return err
	}
	i := m.instance
	i.admission.mu.Lock()
	defer i.admission.mu.Unlock()
	if !i.IsRunning() {
		return controlError("process_not_running")
	}
	if err := m.permit.Check(); err != nil {
		return err
	}
	i.control.model = model
	i.control.token = opaqueControlID()
	i.control.fingerprint = optionsFingerprint(i.GetOptions())
	if i.policy != nil {
		i.control.ownerRevision = i.policy.Snapshot().Revision
	}
	i.control.state = "ready"
	return nil
}

// VerifyRecovery only opens a stopped, administrator-verified generation. It
// never adopts an unknown process, regenerates a preset, starts or probes a model.
func (m *Maintenance) VerifyRecovery(ctx context.Context) error {
	i := m.instance
	if err := ctx.Err(); err != nil {
		return err
	}
	if i.IsRunning() {
		return controlError("recovery_requires_stopped")
	}
	if i.process != nil {
		i.process.mu.RLock()
		defer i.process.mu.RUnlock()
		if i.process.tree != nil {
			select {
			case <-i.process.tree.done:
			default:
				return controlError("owned_tree_not_exited")
			}
		}
		if i.process.monitorDone != nil {
			return controlError("owned_tree_not_exited")
		}
	}
	if err := releasedPort(ctx, i.GetHost(), i.GetPort()); err != nil {
		return controlError("port_ownership_unknown")
	}
	opts := i.GetOptions()
	data, err := os.ReadFile(filepath.Join(i.globalInstanceSettings.InstancesDir, i.Name, "preset.ini"))
	if err != nil && !os.IsNotExist(err) {
		return controlError("preset_sync_failed")
	}
	if err == nil && (opts.PresetIni == nil || string(data) != *opts.PresetIni) {
		return controlError("preset_mismatch")
	}
	i.admission.mu.Lock()
	i.control.interrupted = false
	i.admission.mu.Unlock()
	m.FinishControl("stopped", "")
	return nil
}

// PersistControl commits the derived file, saves a detached candidate, and only
// then publishes options/revision. File/DB/process are explicitly not a transaction.
func (p *PreparedOptions) PersistControl(ctx context.Context, save func(*Instance) error) error {
	m := p.lease
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.CheckControl(ctx, true); err != nil {
		return err
	}
	i := p.instance
	if i.process != nil {
		i.process.mu.RLock()
		defer i.process.mu.RUnlock()
		if i.process.monitorDone != nil {
			return controlError("stop_failed")
		}
		if i.process.tree != nil {
			select {
			case <-i.process.tree.done:
			default:
				return controlError("stop_failed")
			}
		}
	}
	if i.IsRunning() {
		return controlError("stop_failed")
	}
	if p.closed || p.committed {
		return controlError("invalid_candidate")
	}
	if err := p.preset.commitPolicy(m.permit); err != nil {
		return controlError("preset_sync_failed")
	}
	if err := m.CheckControl(ctx, true); err != nil {
		return err
	}
	candidate := &Instance{ID: i.ID, Name: i.Name, Created: i.Created, status: newStatus(Stopped), options: newOptions(p.opts)}
	if err := save(candidate); err != nil {
		return controlError("persist_failed")
	}
	// Even if cancellation happened during synchronous persistence, publish the
	// successfully saved revision; do not start further work with that old permit.
	i.admission.mu.Lock()
	i.options.set(p.opts)
	i.admission.mu.Unlock()
	if i.proxy != nil {
		i.proxy.clear()
	}
	p.committed = true
	return m.CheckControl(ctx, true)
}

func (i *Instance) validateReadyRequest(r *http.Request, aggregate bool) error {
	if !i.ProtocolEnabled() {
		return nil
	}
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return controlError("invalid_model")
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	var obj map[string]json.RawMessage
	var model string
	if json.Unmarshal(b, &obj) != nil || json.Unmarshal(obj["model"], &model) != nil {
		return controlError("model_mismatch")
	}
	i.admission.mu.Lock()
	defer i.admission.mu.Unlock()
	expected := i.control.model
	if aggregate {
		expected = i.Name + "/" + expected
	}
	if model != expected {
		return controlError("model_mismatch")
	}
	values := r.Header.Values("X-Ready-Token")
	if len(values) != 1 || values[0] == "" || !i.readyCurrentLocked() || subtle.ConstantTimeCompare([]byte(values[0]), []byte(i.control.token)) != 1 {
		return controlError("ready_token_invalid")
	}
	return nil
}

// RouterRequest is internal to an admitted operation. Never expose arbitrary
// paths or request bodies to a controller. Redirects and proxy env are disabled.
func (m *Maintenance) RouterRequest(ctx context.Context, method, path, model string) ([]byte, error) {
	work := method == "POST" && path != "/models/unload"
	if err := m.CheckControl(ctx, work); err != nil {
		return nil, err
	}
	switch path {
	case "/health", "/models", "/props", "/models/load", "/models/unload":
	default:
		return nil, fmt.Errorf("unsupported internal route")
	}
	i := m.instance
	endpoint := "http://" + net.JoinHostPort(i.GetHost(), fmt.Sprint(i.GetPort())) + path
	var body io.Reader
	if method == "POST" {
		b, _ := json.Marshal(map[string]string{"model": model})
		body = bytes.NewReader(b)
	} else if path == "/props" {
		endpoint += "?model=" + url.QueryEscape(model) + "&autoload=false"
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key := i.backendAPIKey(i.GetOptions()); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	// Child /props and explicit lifecycle calls may legitimately wait for model
	// initialization. Their bound is the operation context, not a hidden 5s limit.
	timeout := 5 * time.Second
	if path == "/props" || method == "POST" {
		timeout = 0
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("backend status %d", resp.StatusCode)
	}
	return b, nil
}

func (m *Maintenance) ProbeReady(ctx context.Context, model string) error {
	i := m.instance
	if !i.IsRunning() {
		return controlError("process_not_running")
	}
	if i.process == nil {
		return controlError("process_not_running")
	}
	i.process.mu.RLock()
	tree := i.process.tree
	i.process.mu.RUnlock()
	if tree == nil {
		return controlError("process_not_owned")
	}
	select {
	case <-tree.done:
		return controlError("process_not_running")
	default:
	}
	opts := i.GetOptions()
	b, err := os.ReadFile(filepath.Join(i.globalInstanceSettings.InstancesDir, i.Name, "preset.ini"))
	if err != nil || opts.PresetIni == nil || string(b) != *opts.PresetIni {
		return controlError("preset_mismatch")
	}
	if _, err = m.RouterRequest(ctx, "GET", "/health", ""); err != nil {
		return err
	}
	if err = m.WaitModel(ctx, model, "loaded"); err != nil {
		return err
	}
	// b11717's proxy_get routes /props by exact model and respects autoload=false.
	// This is a child availability probe, NOT a claim of /health?model routing.
	for {
		if err := m.CheckControl(ctx, true); err != nil {
			return err
		}
		if !i.IsRunning() {
			return controlError("process_not_running")
		}
		b, err = m.RouterRequest(ctx, "GET", "/props", model)
		if err == nil {
			var props map[string]json.RawMessage
			if json.Unmarshal(b, &props) != nil || props["default_generation_settings"] == nil || string(props["default_generation_settings"]) == "null" {
				return controlError("target_probe_failed")
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (m *Maintenance) WaitRouter(ctx context.Context) error {
	for {
		if err := m.CheckControl(ctx, true); err != nil {
			return err
		}
		if !m.instance.IsRunning() {
			return controlError("process_not_running")
		}
		if _, err := m.RouterRequest(ctx, "GET", "/health", ""); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (m *Maintenance) WaitModel(ctx context.Context, model, wanted string) error {
	for {
		b, err := m.RouterRequest(ctx, "GET", "/models", "")
		if err == nil {
			var response struct {
				Data []struct {
					ID     string `json:"id"`
					Status struct {
						Value  string `json:"value"`
						Failed bool   `json:"failed"`
					} `json:"status"`
				} `json:"data"`
			}
			if json.Unmarshal(b, &response) != nil {
				return controlError("model_status_invalid")
			}
			found := false
			for _, entry := range response.Data {
				if entry.ID != model && entry.Status.Value == "loaded" {
					return controlError("unexpected_loaded_model")
				}
				if entry.ID == model {
					found = true
					if entry.Status.Failed {
						return controlError("model_load_failed")
					}
					if entry.Status.Value == wanted {
						return nil
					}
				}
			}
			if !found {
				return controlError("model_not_found")
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
