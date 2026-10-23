package manager

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"llamactl/pkg/instance"
	"llamactl/pkg/resourcepolicy"
	"sync"
	"time"
)

const ControlRetention = 24 * time.Hour

// ControlManager is a narrow optional extension; existing embedded managers and
// ordinary instances keep their native interface and behavior.
type ControlManager interface {
	ControlInstance(string) (*instance.Instance, error)
	ControlStatus(string) (instance.ControlSnapshot, error)
	SubmitControl(string, string, []byte) (ControlOperation, error)
	GetControlOperation(string, string, string, bool) (ControlOperation, error)
	RecoverControl(context.Context, string, string, uint64) (instance.ControlSnapshot, error)
}

type ControlOperation struct {
	ID         string     `json:"id"`
	BootID     string     `json:"boot_id"`
	Instance   string     `json:"instance"`
	Action     string     `json:"action"`
	State      string     `json:"state"`
	Stage      string     `json:"stage"`
	ErrorCode  string     `json:"error_code,omitempty"`
	Revision   uint64     `json:"revision"`
	StartedAt  time.Time  `json:"started_at"`
	Deadline   time.Time  `json:"deadline"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
	ReadyToken string     `json:"ready_token,omitempty"`
	Model      string     `json:"model,omitempty"`
}
type controlRecord struct {
	operation ControlOperation
	scope     string
	key       string
	payload   string
}
type controlLedger struct {
	mu       sync.Mutex
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	closed   bool
	boot     string
	records  map[string]*controlRecord
	tests    map[string]*deploymentRecord
	capacity int
	budget   time.Duration
	now      func() time.Time
	// Immutable test seams, scoped to this manager; no app/global hook.
	start func(context.Context, *instance.Maintenance, *instance.Instance) error
	stop  func(context.Context, *instance.Maintenance, *instance.Instance) error
	ready func(context.Context, *instance.Maintenance, string) error
}

func newControlID() string {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(id[:])
}
func protocolError(code string) error { return &instance.AdmissionError{Code: code, Details: code} }

func (im *instanceManager) initControl() *controlLedger {
	im.controlOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		c := &controlLedger{ctx: ctx, cancel: cancel, boot: newControlID(), records: map[string]*controlRecord{}, capacity: 1024, budget: 600 * time.Second, now: time.Now}
		if im.globalConfig != nil {
			cfg := im.globalConfig.Instances
			if cfg.ControlOperationCapacity > 0 {
				c.capacity = cfg.ControlOperationCapacity
			}
			if cfg.ControlOperationTimeoutSeconds > 0 {
				c.budget = time.Duration(cfg.ControlOperationTimeoutSeconds) * time.Second
			}
		}
		c.start = func(ctx context.Context, m *instance.Maintenance, _ *instance.Instance) error {
			return m.StartControl(ctx)
		}
		c.stop = func(ctx context.Context, m *instance.Maintenance, _ *instance.Instance) error {
			return m.StopControl(ctx)
		}
		c.ready = func(ctx context.Context, m *instance.Maintenance, model string) error {
			return m.ProbeReady(ctx, model)
		}
		im.control = c
	})
	return im.control
}

// ControlInstance is a descriptor lookup only, including for authorization. It
// deliberately never calls the native remote-refresh GetInstance path.
func (im *instanceManager) ControlInstance(name string) (*instance.Instance, error) {
	inst, ok := im.registry.get(name)
	if !ok {
		return nil, protocolError("instance_not_found")
	}
	return inst, nil
}
func (im *instanceManager) controlInstance(name string) (*instance.Instance, error) {
	inst, ok := im.registry.get(name)
	if !ok {
		return nil, protocolError("instance_not_found")
	}
	if !inst.ProtocolEnabled() || inst.IsRemote() {
		return nil, protocolError("control_not_enabled")
	}
	inst.InitializeControl(im.initControl().boot)
	return inst, nil
}

// ControlAuthRequired keeps a protocol-enabled service authenticated even when
// legacy groups were configured as anonymous. Registry lookup performs no I/O.
func (im *instanceManager) ControlAuthRequired() bool {
	for _, inst := range im.registry.list() {
		if inst.ProtocolEnabled() {
			return true
		}
	}
	return false
}

func (im *instanceManager) ControlStatus(name string) (instance.ControlSnapshot, error) {
	inst, err := im.controlInstance(name)
	if err != nil {
		return instance.ControlSnapshot{}, err
	}
	s := inst.ControlSnapshot()
	c := im.initControl()
	s.OperationBudgetSeconds, s.OperationCapacity = int64(c.budget/time.Second), c.capacity
	return s, nil
}

type controlRequest struct {
	Action    string                     `json:"action"`
	BootID    string                     `json:"boot_id"`
	Revision  uint64                     `json:"expected_revision"`
	Key       string                     `json:"idempotency_key"`
	Model     string                     `json:"model,omitempty"`
	Patch     map[string]json.RawMessage `json:"patch,omitempty"`
	Timeout   int                        `json:"timeout_seconds,omitempty"`
	canonical string
}

func parseControlRequest(data []byte) (controlRequest, error) {
	var req controlRequest
	if len(data) > 64<<10 {
		return req, protocolError("invalid_config")
	}
	f, err := instance.ControlObject(data, "action", "boot_id", "expected_revision", "idempotency_key", "model", "patch", "timeout_seconds")
	if err != nil {
		return req, protocolError("unsupported_parameter")
	}
	for _, key := range []string{"action", "boot_id", "expected_revision", "idempotency_key"} {
		if f[key] == nil || string(f[key]) == "null" {
			return req, protocolError("invalid_config")
		}
	}
	// Decode scalar fields strictly; preserve optional-field presence in canonical.
	for k, v := range f {
		if string(v) == "null" {
			return req, protocolError("invalid_config")
		}
		if k != "patch" && k != "expected_revision" && k != "timeout_seconds" {
			var s string
			if json.Unmarshal(v, &s) != nil {
				return req, protocolError("invalid_config")
			}
			f[k], _ = json.Marshal(s)
		}
	}
	if json.Unmarshal(f["action"], &req.Action) != nil || json.Unmarshal(f["boot_id"], &req.BootID) != nil || json.Unmarshal(f["idempotency_key"], &req.Key) != nil {
		return req, protocolError("invalid_config")
	}
	// Revision is an integer version, not a float; accept equivalent legal JSON
	// number spellings with the same exact bounded integer value.
	number := func(raw json.RawMessage, max int64) (json.RawMessage, error) {
		// Reuse the schema number normalizer through its public bounded helper.
		return instance.NormalizeControlInteger(raw, 0, max)
	}
	n, err := number(f["expected_revision"], 9007199254740991)
	if err != nil {
		return req, protocolError("invalid_config")
	}
	f["expected_revision"] = n
	_ = json.Unmarshal(n, &req.Revision)
	if v, ok := f["timeout_seconds"]; ok {
		n, err := number(v, 2147483647)
		if err != nil || string(n) == "0" {
			return req, protocolError("invalid_config")
		}
		f["timeout_seconds"] = n
		_ = json.Unmarshal(n, &req.Timeout)
	}
	if v, ok := f["model"]; ok {
		if json.Unmarshal(v, &req.Model) != nil || !resourcepolicy.ValidReleaseID(req.Model) {
			return req, protocolError("invalid_config")
		}
	}
	if !resourcepolicy.ValidReleaseID(req.Key) || req.BootID == "" {
		return req, protocolError("invalid_config")
	}
	switch req.Action {
	case "apply":
		if req.Model == "" || f["patch"] == nil {
			return req, protocolError("invalid_config")
		}
	case "load":
		if req.Model == "" {
			return req, protocolError("invalid_config")
		}
	case "start", "stop", "unload":
	default:
		return req, protocolError("invalid_config")
	}
	if req.Action != "apply" && f["patch"] != nil {
		return req, protocolError("unsupported_parameter")
	}
	if (req.Action == "start" || req.Action == "stop") && f["model"] != nil {
		return req, protocolError("unsupported_parameter")
	}
	if raw, ok := f["patch"]; ok {
		req.Patch, err = instance.NormalizeControlPatch(raw)
		if err != nil {
			return req, protocolError("unsupported_parameter")
		}
		f["patch"], _ = json.Marshal(req.Patch)
	}
	b, _ := json.Marshal(f)
	req.canonical = string(b)
	return req, nil
}

func (c *controlLedger) lookup(scope, name string, req controlRequest) (*controlRecord, error) {
	if req.BootID != c.boot {
		return nil, protocolError("epoch_changed")
	}
	now := c.now()
	for id, rec := range c.records {
		if rec.operation.EndedAt != nil && !now.Before(rec.operation.EndedAt.Add(ControlRetention)) {
			delete(c.records, id)
			continue
		}
		if rec.scope == scope && rec.operation.Instance == name && rec.key == req.Key {
			if rec.payload != req.canonical {
				return nil, protocolError("idempotency_conflict")
			}
			return rec, nil
		}
	}
	return nil, nil
}
func (im *instanceManager) SubmitControl(name, scope string, data []byte) (ControlOperation, error) {
	var zero ControlOperation
	req, err := parseControlRequest(data)
	if err != nil {
		return zero, err
	}
	inst, err := im.controlInstance(name)
	if err != nil {
		return zero, err
	}
	c := im.initControl()
	c.mu.Lock()
	defer c.mu.Unlock()
	old, err := c.lookup(scope, name, req)
	if err != nil {
		return zero, err
	}
	if old != nil {
		return old.operation, nil
	}
	if c.closed {
		return zero, protocolError("manager_stopping")
	}
	if req.Timeout > 0 && time.Duration(req.Timeout)*time.Second > c.budget {
		return zero, protocolError("invalid_deadline")
	}
	if len(c.records) >= c.capacity {
		return zero, protocolError("operation_capacity")
	}
	work := req.Action != "stop" && req.Action != "unload"
	failedApply := req.Action == "apply" && inst.ControlSnapshot().State == "failed"
	lease, err := inst.BeginControl(req.BootID, req.Revision, work, failedApply)
	if err != nil {
		return zero, err
	}
	if req.Action == "start" && inst.IsRunning() {
		lease.Close()
		return zero, protocolError("already_running")
	}
	// Candidate validation has no process side effects. File preparation below is
	// part of the bounded operation and precedes readiness invalidation/stop.
	var candidate *instance.Options
	if req.Action == "apply" || req.Action == "load" {
		candidate, err = inst.ControlCandidate(req.Model, req.Patch)
		if err != nil {
			lease.Close()
			return zero, protocolError("invalid_config")
		}
		if req.Action == "apply" {
			if req.Revision >= 9007199254740991 {
				lease.Close()
				return zero, protocolError("revision_exhausted")
			}
			candidate.ConfigRevision = req.Revision + 1
		}
	}
	snapshot := inst.ControlSnapshot()
	if req.Action == "unload" {
		snapshot.ActualModel = inst.ControlLastModel()
	}
	if req.Action == "load" && snapshot.ActualModel != "" && snapshot.ActualModel != req.Model {
		lease.Close()
		return zero, protocolError("model_already_loaded")
	}
	if req.Action == "unload" && req.Model != "" && req.Model != snapshot.ActualModel {
		lease.Close()
		return zero, protocolError("model_mismatch")
	}
	budget := c.budget
	if req.Timeout > 0 {
		budget = time.Duration(req.Timeout) * time.Second
	}
	now := c.now()
	op := ControlOperation{ID: newControlID(), BootID: c.boot, Instance: name, Action: req.Action, State: "running", Stage: "accepted", Revision: req.Revision, StartedAt: now, Deadline: now.Add(budget)}
	rec := &controlRecord{operation: op, scope: scope, key: req.Key, payload: req.canonical}
	c.records[op.ID] = rec
	lease.ReserveControl(op.ID)
	ctx, cancel := context.WithTimeout(c.ctx, budget)
	ctx, cleanup := lease.BindControlContext(ctx)
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer cancel()
		defer cleanup()
		defer lease.Close()
		im.runControl(ctx, c, rec, lease, inst, req, candidate, snapshot)
	}()
	return op, nil
}
func (im *instanceManager) runControl(ctx context.Context, c *controlLedger, rec *controlRecord, m *instance.Maintenance, inst *instance.Instance, req controlRequest, candidate *instance.Options, before instance.ControlSnapshot) {
	stage := func(s string) { c.mu.Lock(); rec.operation.Stage = s; c.mu.Unlock() }
	var err error
	activated := false
	model := ""
	state := "stopped"
	run := func() error {
		var prepared *instance.PreparedOptions
		if req.Action == "apply" {
			stage("prepare")
			var e error
			prepared, e = m.PrepareOptions(candidate)
			if e != nil {
				return protocolError("preset_sync_failed")
			}
			defer prepared.Close()
		}
		if e := m.CheckControl(ctx, req.Action != "stop" && req.Action != "unload"); e != nil {
			return e
		}
		m.ActivateControl(rec.operation.ID)
		activated = true
		switch req.Action {
		case "apply":
			stage("stop")
			if e := c.stop(ctx, m, inst); e != nil {
				return protocolErrorContext(ctx, "stop_failed", e)
			}
			stage("persist")
			if e := prepared.PersistControl(ctx, func(candidate *instance.Instance) error {
				if store, ok := im.db.(interface {
					SaveContext(context.Context, *instance.Instance) error
				}); ok {
					return store.SaveContext(ctx, candidate)
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				return im.persistInstance(candidate)
			}); e != nil {
				return e
			}
			fallthrough
		case "start":
			stage("start")
			if e := c.start(ctx, m, inst); e != nil {
				return protocolErrorContext(ctx, "start_failed", e)
			}
			if req.Action == "start" {
				stage("router_health")
				if e := m.WaitRouter(ctx); e != nil {
					return protocolErrorContext(ctx, "start_failed", e)
				}
				state = "running"
				return m.CheckControl(ctx, true)
			}
			fallthrough
		case "load":
			if !inst.IsRunning() {
				return protocolError("instance_not_running")
			}
			stage("router_health")
			if e := m.WaitRouter(ctx); e != nil {
				return protocolErrorContext(ctx, "start_failed", e)
			}
			stage("load")
			if before.ActualModel != req.Model || req.Action == "apply" {
				b, e := m.RouterRequest(ctx, "POST", "/models/load", req.Model)
				if e != nil {
					return protocolErrorContext(ctx, "model_load_failed", e)
				}
				var ack struct {
					Success bool `json:"success"`
				}
				if json.Unmarshal(b, &ack) != nil || !ack.Success {
					return protocolError("model_load_failed")
				}
			}
			stage("ready_probe")
			if e := c.ready(ctx, m, req.Model); e != nil {
				return protocolErrorContext(ctx, "model_load_failed", e)
			}
			if e := m.PublishReady(ctx, req.Model); e != nil {
				return e
			}
			state = "ready"
			model = req.Model
		case "unload":
			stage("unload")
			if before.ActualModel != "" {
				b, e := m.RouterRequest(ctx, "POST", "/models/unload", before.ActualModel)
				if e != nil {
					return protocolErrorContext(ctx, "model_unload_failed", e)
				}
				var ack struct {
					Success bool `json:"success"`
				}
				if json.Unmarshal(b, &ack) != nil || !ack.Success {
					return protocolError("model_unload_failed")
				}
				if e := m.WaitModel(ctx, before.ActualModel, "unloaded"); e != nil {
					return protocolErrorContext(ctx, "model_unload_failed", e)
				}
			}
			if inst.IsRunning() {
				state = "running"
			}
			// Unload proves only a model-state transition (or is a no-op). It
			// cannot clear a failed stop/persist or an interrupted generation.
			if before.RecoveryRequired {
				state = before.State
			}
		case "stop":
			stage("stop")
			if e := c.stop(ctx, m, inst); e != nil {
				return protocolErrorContext(ctx, "stop_failed", e)
			}
		}
		return m.CheckControl(ctx, req.Action != "stop" && req.Action != "unload")
	}
	err = run()
	code := ""
	opState := "succeeded"
	if err != nil {
		opState = "failed"
		state = "failed"
		code = "operation_failed"
		var admission *instance.AdmissionError
		if errors.As(err, &admission) {
			code = admission.Code
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			opState = "deadline_exceeded"
			state = "interrupted"
			code = "deadline_exceeded"
		} else if errors.Is(err, context.Canceled) || ctx.Err() != nil || errors.Is(err, resourcepolicy.ErrPaused) {
			opState = "interrupted"
			state = "interrupted"
			code = "interrupted"
		}
	}
	if activated {
		m.FinishControl(state, model)
	} else {
		m.ClearControlOperation()
	}
	s := inst.ControlSnapshot()
	m.Close() // A terminal result means its maintenance reservation has unwound.
	c.mu.Lock()
	defer c.mu.Unlock()
	ended := c.now()
	rec.operation.State = opState
	rec.operation.ErrorCode = code
	rec.operation.EndedAt = &ended
	rec.operation.Revision = s.Revision
	rec.operation.Model = s.ActualModel
	rec.operation.ReadyToken = s.ReadyToken
}
func protocolErrorContext(ctx context.Context, code string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, resourcepolicy.ErrPaused) {
		return err
	}
	return protocolError(code)
}
func (im *instanceManager) GetControlOperation(name, scope, id string, admin bool) (ControlOperation, error) {
	c := im.initControl()
	c.mu.Lock()
	defer c.mu.Unlock()
	rec := c.records[id]
	if rec == nil || rec.operation.Instance != name || (!admin && rec.scope != scope) || (rec.operation.EndedAt != nil && !c.now().Before(rec.operation.EndedAt.Add(ControlRetention))) {
		return ControlOperation{}, protocolError("operation_not_found")
	}
	return rec.operation, nil
}
func (im *instanceManager) RecoverControl(ctx context.Context, name, boot string, revision uint64) (instance.ControlSnapshot, error) {
	inst, err := im.controlInstance(name)
	if err != nil {
		return instance.ControlSnapshot{}, err
	}
	c := im.initControl()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return instance.ControlSnapshot{}, protocolError("manager_stopping")
	}
	if boot != c.boot {
		return instance.ControlSnapshot{}, protocolError("epoch_changed")
	}
	if inst.ControlSnapshot().Revision != revision {
		return instance.ControlSnapshot{}, protocolError("revision_conflict")
	}
	if handled, recoveryErr := im.recoverN1(ctx, inst); handled {
		if recoveryErr != nil {
			return instance.ControlSnapshot{}, recoveryErr
		}
		return inst.ControlSnapshot(), nil
	}
	lease, err := inst.BeginControl(boot, revision, false, true)
	if err != nil {
		return instance.ControlSnapshot{}, err
	}
	defer lease.Close()
	if err := lease.VerifyRecovery(ctx); err != nil {
		return instance.ControlSnapshot{}, err
	}
	s := inst.ControlSnapshot()
	s.OperationBudgetSeconds, s.OperationCapacity = int64(c.budget/time.Second), c.capacity
	return s, nil
}
func (im *instanceManager) shutdownControl() {
	c := im.initControl()
	c.mu.Lock()
	c.closed = true
	c.cancel()
	c.mu.Unlock()
	c.wg.Wait()
}

var _ ControlManager = (*instanceManager)(nil)
