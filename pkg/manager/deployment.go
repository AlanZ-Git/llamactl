package manager

import (
	"context"
	"encoding/json"
	"errors"
	"llamactl/pkg/instance"
	"llamactl/pkg/resourcepolicy"
	"reflect"
	"runtime/debug"
	"time"
)

type deploymentRecord struct {
	report              DeploymentTest
	scope, key, payload string
	cancel              context.CancelFunc
	cancelled           bool
	cancelRequests      map[string]string
}

func cloneDeployment(r DeploymentTest) DeploymentTest {
	b, _ := json.Marshal(r)
	var out DeploymentTest
	_ = json.Unmarshal(b, &out)
	return out
}

func (im *instanceManager) deploymentIdentity() DeploymentIdentity {
	out := DeploymentIdentity{ApplicationVersion: "unknown", ApplicationCommit: "unknown", ApplicationBuildTime: "unknown", Engine: "llama_cpp", EngineVersion: "unknown", EngineHash: "unknown", ModelHash: "unknown", RuntimeIdentity: "unknown"}
	if cfg := im.globalConfig; cfg != nil {
		if cfg.Version != "" {
			out.ApplicationVersion = cfg.Version
		}
		if cfg.CommitHash != "" {
			out.ApplicationCommit = cfg.CommitHash
		}
		if cfg.BuildTime != "" {
			out.ApplicationBuildTime = cfg.BuildTime
		}
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				if out.ApplicationCommit == "unknown" {
					out.ApplicationCommit = s.Value
				}
			case "vcs.modified":
				v := s.Value == "true"
				out.SourceModified = &v
			}
		}
	}
	return out
}

func (im *instanceManager) SubmitDeploymentTest(name, scope string, data []byte) (DeploymentTest, error) {
	var zero DeploymentTest
	req, err := parseDeploymentRequest(data)
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
	if req.BootID != c.boot {
		return zero, protocolError("epoch_changed")
	}
	if c.tests == nil {
		c.tests = map[string]*deploymentRecord{}
	}
	for id, r := range c.tests {
		if r.report.EndedAt != nil && !c.now().Before(r.report.EndedAt.Add(ControlRetention)) {
			delete(c.tests, id)
			continue
		}
		if r.scope == scope && r.report.Instance == name && r.key == req.Key {
			if r.payload != req.canonical {
				return zero, protocolError("idempotency_conflict")
			}
			return cloneDeployment(r.report), nil
		}
	}
	if c.closed {
		return zero, protocolError("manager_stopping")
	}
	if len(c.tests) >= c.capacity {
		return zero, protocolError("operation_capacity")
	}
	for _, r := range c.tests {
		if r.report.EndedAt == nil {
			return zero, instance.BusyError()
		}
	}
	lease, err := inst.BeginControl(req.BootID, req.Revision, true, false)
	if err != nil {
		return zero, err
	}
	accepted := false
	defer func() {
		if !accepted {
			lease.Close()
		}
	}()
	candidate, err := inst.ControlCandidate(req.Model, req.Patch)
	if err != nil {
		return zero, protocolError("invalid_config")
	}
	before := inst.ControlSnapshot()
	current := inst.GetOptions()
	changed := !reflect.DeepEqual(candidate.Control, current.Control) || !reflect.DeepEqual(candidate.PresetIni, current.PresetIni)
	reuse := before.State == "ready" && before.ActualModel == req.Model
	if reuse {
		changed = !reflect.DeepEqual(candidate.PresetIni, current.PresetIni)
	}
	if before.ActualModel != "" && !reuse {
		return zero, protocolError("model_already_loaded")
	}
	if reuse && changed {
		return zero, protocolError("ready_configuration_conflict")
	}
	if before.Running && before.State != "running" && !reuse {
		return zero, protocolError("model_ownership_unknown")
	}
	if changed {
		if req.Revision >= 9007199254740991 {
			return zero, protocolError("revision_exhausted")
		}
		candidate.ConfigRevision = req.Revision + 1
	}
	now := c.now()
	budget := time.Duration(req.Timeout) * time.Second
	r := &deploymentRecord{scope: scope, key: req.Key, payload: req.canonical, cancelRequests: map[string]string{}}
	r.report = DeploymentTest{ID: newControlID(), BootID: c.boot, Instance: name, Model: req.Model, State: "running", Stage: "accepted", StartedAt: now, Deadline: now.Add(budget), Revision: before.Revision,
		Parameters: before.ConfiguredParameters[req.Model], ParameterDefaults: "omitted_overrides_use_engine_defaults_not_measured", Identity: im.deploymentIdentity(), FixtureID: req.Fixture, FixtureDigest: instance.DeploymentFixtureDigest(),
		MeasuredGenerations: req.Measured, Warmup: req.Warmup, MaxOutputTokens: req.MaxTokens, Seed: req.Seed, Concurrency: 1, LoadMode: "test_owned", CacheCondition: "unknown", Cleanup: "pending", Samples: []instance.GenerationObservation{},
	}
	if reuse {
		r.report.LoadMode = "reused_ready"
		r.report.Cleanup = "not_owned_preserved"
	}
	baseline, ok := im.ResourceSnapshot()
	accumulator := newDeploymentResources(baseline, ok, name, now)
	r.report.Resources = accumulator.result()
	cleanupReserve := 5 * time.Second
	if budget/2 < cleanupReserve {
		cleanupReserve = budget / 2
	}
	ctx, cancel := context.WithDeadline(c.ctx, r.report.Deadline.Add(-cleanupReserve))
	ctx, unbind := lease.BindControlContext(ctx)
	r.cancel = cancel
	c.tests[r.report.ID] = r
	lease.ReserveControl(r.report.ID)
	accepted = true
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer cancel()
		defer unbind()
		defer lease.Close()
		im.runDeploymentTest(ctx, c, r, lease, inst, req, candidate, changed, reuse, accumulator)
	}()
	return cloneDeployment(r.report), nil
}

func (im *instanceManager) GetDeploymentTest(name, scope, id string, admin bool) (DeploymentTest, error) {
	c := im.initControl()
	c.mu.Lock()
	defer c.mu.Unlock()
	r, err := c.deploymentRecord(name, scope, id, admin)
	if err != nil {
		return DeploymentTest{}, err
	}
	out := r.report
	if out.EndedAt == nil {
		out.ElapsedMS = float64(c.now().Sub(out.StartedAt)) / float64(time.Millisecond)
	}
	return cloneDeployment(out), nil
}
func (c *controlLedger) deploymentRecord(name, scope, id string, admin bool) (*deploymentRecord, error) {
	r := c.tests[id]
	if r == nil || r.report.Instance != name || (!admin && r.scope != scope) || (r.report.EndedAt != nil && !c.now().Before(r.report.EndedAt.Add(ControlRetention))) {
		return nil, protocolError("test_unknown_not_resumable")
	}
	return r, nil
}

func (im *instanceManager) CancelDeploymentTest(name, scope, id string, admin bool, data []byte) (DeploymentTest, error) {
	// Cancel has the same scalar envelope; the resource ID binds its target.
	if len(data) > 64<<10 {
		return DeploymentTest{}, protocolError("invalid_config")
	}
	f, err := instance.ControlObject(data, "boot_id", "expected_revision", "idempotency_key")
	var boot, key string
	if err != nil || len(f) != 3 || json.Unmarshal(f["boot_id"], &boot) != nil || boot == "" || len(boot) > 128 || json.Unmarshal(f["idempotency_key"], &key) != nil || !resourcepolicy.ValidReleaseID(key) {
		return DeploymentTest{}, protocolError("invalid_config")
	}
	revision, err := instance.NormalizeControlInteger(f["expected_revision"], 0, 9007199254740991)
	if err != nil {
		return DeploymentTest{}, protocolError("invalid_config")
	}
	f["expected_revision"] = revision
	f["boot_id"], _ = json.Marshal(boot)
	f["idempotency_key"], _ = json.Marshal(key)
	payload, _ := json.Marshal(f)
	c := im.initControl()
	c.mu.Lock()
	defer c.mu.Unlock()
	if boot != c.boot {
		return DeploymentTest{}, protocolError("epoch_changed")
	}
	r, err := c.deploymentRecord(name, scope, id, admin)
	if err != nil {
		return DeploymentTest{}, err
	}
	k := scope + ":" + key
	if previous, found := r.cancelRequests[k]; found {
		if previous != string(payload) {
			return DeploymentTest{}, protocolError("idempotency_conflict")
		}
		return cloneDeployment(r.report), nil
	}
	if len(r.cancelRequests) >= 16 {
		return DeploymentTest{}, protocolError("operation_capacity")
	}
	var expected uint64
	_ = json.Unmarshal(revision, &expected)
	inst, err := im.controlInstance(name)
	if err != nil {
		return DeploymentTest{}, err
	}
	if inst.ControlSnapshot().Revision != expected {
		return DeploymentTest{}, protocolError("revision_conflict")
	}
	r.cancelRequests[k] = string(payload)
	if r.report.EndedAt == nil {
		r.cancelled = true
		r.cancel()
	}
	return cloneDeployment(r.report), nil
}

func (im *instanceManager) runDeploymentTest(ctx context.Context, c *controlLedger, r *deploymentRecord, m *instance.Maintenance, inst *instance.Instance, req deploymentRequest, candidate *instance.Options, changed, reuse bool, a *deploymentResourceAccumulator) {
	update := func(fn func(*DeploymentTest)) { c.mu.Lock(); fn(&r.report); c.mu.Unlock() }
	stageName := "accepted"
	stage := func(s string) { stageName = s; update(func(out *DeploymentTest) { out.Stage = s }) }
	// This reader only observes the SAME shared cache. It never invokes a collector.
	stopRead := make(chan struct{})
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopRead:
				return
			case <-ticker.C:
				s, ok := im.ResourceSnapshot()
				a.observe(s, ok, inst.Name)
				update(func(out *DeploymentTest) { out.Resources = a.result() })
			}
		}
	}()
	ownedLoad := false
	ownedStart := false
	activated := false
	readyStart := time.Now()
	run := func() error {
		if err := m.CheckControl(ctx, true); err != nil {
			return err
		}
		if inst.IsRunning() && !reuse {
			stage("verify_unloaded")
			if err := m.CheckNoLoadedModel(ctx); err != nil {
				return err
			}
		}
		if reuse {
			stage("ready_probe")
			if err := m.ProbeReady(ctx, req.Model); err != nil {
				return protocolErrorContext(ctx, "model_load_failed", err)
			}
		} else {
			if changed {
				stage("prepare")
				prepared, err := m.PrepareOptions(candidate)
				if err != nil {
					return protocolError("preset_sync_failed")
				}
				defer prepared.Close()
				m.ActivateControl(r.report.ID)
				activated = true
				if inst.IsRunning() {
					stage("stop_unloaded_router")
					if err := m.StopControl(ctx); err != nil {
						return protocolErrorContext(ctx, "stop_failed", err)
					}
				}
				stage("persist")
				if err := prepared.PersistControl(ctx, func(next *instance.Instance) error {
					if store, ok := im.db.(interface {
						SaveContext(context.Context, *instance.Instance) error
					}); ok {
						return store.SaveContext(ctx, next)
					}
					if err := ctx.Err(); err != nil {
						return err
					}
					return im.persistInstance(next)
				}); err != nil {
					return err
				}
			}
			if !activated {
				m.ActivateControl(r.report.ID)
				activated = true
			}
			if !inst.IsRunning() {
				stage("start")
				ownedStart = true
				if err := m.StartControl(ctx); err != nil {
					return protocolErrorContext(ctx, "start_failed", err)
				}
			}
			stage("router_health")
			if err := m.WaitRouter(ctx); err != nil {
				return protocolErrorContext(ctx, "start_failed", err)
			}
			if err := m.CheckNoLoadedModel(ctx); err != nil {
				return err
			}
			stage("load")
			if err := m.CheckControl(ctx, true); err != nil {
				return err
			}
			ownedLoad = true // Even a lost acknowledgement can have loaded the model.
			b, err := m.RouterRequest(ctx, "POST", "/models/load", req.Model)
			if err != nil {
				return protocolErrorContext(ctx, "model_load_failed", err)
			}
			var ack struct {
				Success bool `json:"success"`
			}
			if json.Unmarshal(b, &ack) != nil || !ack.Success {
				return protocolError("model_load_failed")
			}
			stage("ready_probe")
			if err := m.ProbeReady(ctx, req.Model); err != nil {
				return protocolErrorContext(ctx, "model_load_failed", err)
			}
			if err := m.PublishReady(ctx, req.Model); err != nil {
				return err
			}
		}
		s := inst.ControlSnapshot()
		ms := float64(time.Since(readyStart)) / float64(time.Millisecond)
		update(func(out *DeploymentTest) {
			out.ReadyDurationMS = &ms
			out.Revision = s.Revision
			out.Parameters = s.ConfiguredParameters[req.Model]
			out.ProcessGeneration = s.ProcessGeneration
			out.LoadGeneration = s.LoadGeneration
		})
		total := req.Measured
		if req.Warmup {
			total++
		}
		for n := 0; n < total; n++ {
			if err := m.CheckControl(ctx, true); err != nil {
				return err
			}
			kind := "subsequent"
			if n == 0 {
				kind = "initial"
				if req.Warmup {
					kind = "warmup"
				}
			} else if req.Warmup && n == 1 {
				kind = "initial"
			}
			stage(kind)
			update(func(out *DeploymentTest) { out.AttemptedGenerations++ })
			observation, err := m.GenerateDeploymentFixture(ctx, req.Model, req.MaxTokens, req.Seed)
			observation.Kind = kind
			if err != nil {
				observation.ErrorCode = deploymentError(ctx, err)
			}
			update(func(out *DeploymentTest) {
				out.Samples = append(out.Samples, observation)
				if err == nil && kind != "warmup" {
					out.CompletedMeasured++
				}
			})
			if err != nil {
				return err
			}
		}
		return m.CheckControl(ctx, true)
	}
	err := run()
	// Capture attempted configuration before cleanup/owner invalidation. Once ready
	// was measured, retain that tested identity rather than a later stopped epoch.
	attempted := inst.ControlSnapshot()
	update(func(out *DeploymentTest) {
		if out.ReadyDurationMS == nil {
			out.Revision = attempted.Revision
			out.Parameters = attempted.ConfiguredParameters[req.Model]
			out.ProcessGeneration = attempted.ProcessGeneration
			out.LoadGeneration = attempted.LoadGeneration
		}
	})
	if err != nil && errors.Is(m.CheckControl(context.Background(), true), resourcepolicy.ErrPaused) {
		err = resourcepolicy.ErrPaused
	}
	errorStage := stageName
	code := ""
	state := "succeeded"
	if err != nil {
		code = deploymentError(ctx, err)
		state = "failed"
		if code == "cancelled" || code == "ai_paused" {
			state = "cancelled"
		} else {
			c.mu.Lock()
			if r.report.CompletedMeasured > 0 {
				state = "partial"
			}
			c.mu.Unlock()
		}
	}
	cleanup := "not_loaded"
	cleanupError := ""
	cleanupMS := float64(0)
	if reuse {
		cleanup = "not_owned_preserved"
	}
	if ownedLoad || (err != nil && ownedStart) {
		stage("cleanup")
		cleanupStart := time.Now()
		// Cancellation/deadline of the run does not suppress necessary cleanup, but
		// the same permit/epoch still prevents old cleanup touching a new generation.
		cleanupDeadline := time.Now().Add(5 * time.Second)
		if r.report.Deadline.Before(cleanupDeadline) {
			cleanupDeadline = r.report.Deadline
		}
		cleanupCtx, cancel := context.WithDeadline(context.Background(), cleanupDeadline)
		var e error
		cleanup = "unloaded_test_owned"
		failureCode := "model_unload_failed"
		if ownedLoad {
			var b []byte
			b, e = m.RouterRequest(cleanupCtx, "POST", "/models/unload", req.Model)
			if e == nil {
				var ack struct {
					Success bool `json:"success"`
				}
				if json.Unmarshal(b, &ack) != nil || !ack.Success {
					e = protocolError("model_unload_failed")
				}
			}
			if e == nil {
				e = m.WaitModel(cleanupCtx, req.Model, "unloaded")
			}
		} else {
			// The run started this Router but never dispatched a model load. Stop only
			// that held tree, never an already-running Router or a foreign listener.
			e = m.StopControl(cleanupCtx)
			cleanup = "stopped_test_owned_router"
			failureCode = "router_stop_failed"
		}
		cancel()
		cleanupMS = float64(time.Since(cleanupStart)) / float64(time.Millisecond)
		if e != nil {
			cleanup = "failed_not_confirmed_released"
			cleanupError = failureCode
			if errors.Is(e, resourcepolicy.ErrPaused) {
				cleanup = "owner_epoch_changed_not_confirmed_released"
				cleanupError = "ai_paused"
			}
			if errors.Is(e, context.DeadlineExceeded) {
				cleanupError = "cleanup_deadline_exceeded"
			}
			if state == "succeeded" {
				state = "partial"
				code = cleanupError
				errorStage = "cleanup"
			}
		}
	}
	if errors.Is(m.CheckControl(context.Background(), true), resourcepolicy.ErrPaused) {
		state = "cancelled"
		code = "ai_paused"
		if err == nil {
			errorStage = "cleanup"
		}
	}
	if activated {
		controlState := "running"
		model := ""
		if !inst.IsRunning() {
			controlState = "stopped"
		}
		if cleanupError != "" {
			controlState = "failed"
			model = req.Model
		}
		if err != nil && !ownedLoad {
			controlState = "failed"
		}
		if code == "ai_paused" {
			controlState = "interrupted"
		}
		m.FinishControl(controlState, model)
	} else if reuse && err != nil && code != "cancelled" && code != "ai_paused" {
		// A failed explicit ready probe/generation is not a fresh readiness assertion.
		// Preserve the preexisting load, but revoke its stale ready capability.
		m.FinishControl("failed", req.Model)
	} else {
		m.ClearControlOperation()
	}
	close(stopRead)
	<-readDone
	s, ok := im.ResourceSnapshot()
	a.observe(s, ok, inst.Name)
	m.Close() // terminal is published ONLY after reservation release
	update(func(out *DeploymentTest) {
		if code != "ai_paused" && (r.cancelled || c.ctx.Err() != nil) {
			state = "cancelled"
			code = "cancelled"
			if err == nil {
				errorStage = "cleanup"
			}
		}
		ended := c.now()
		out.EndedAt = &ended
		out.ElapsedMS = float64(ended.Sub(out.StartedAt)) / float64(time.Millisecond)
		out.State = state
		out.Stage = "finished"
		out.ErrorCode = code
		if code != "" {
			out.ErrorStage = errorStage
		}
		out.Cleanup = cleanup
		out.CleanupError = cleanupError
		out.CleanupDurationMS = cleanupMS
		out.Resources = a.result()
	})
}

func deploymentError(ctx context.Context, err error) string {
	if errors.Is(err, resourcepolicy.ErrPaused) {
		return "ai_paused"
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return "deadline_exceeded"
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	var a *instance.AdmissionError
	if errors.As(err, &a) {
		return a.Code
	}
	return "test_failed"
}

var _ DeploymentTestManager = (*instanceManager)(nil)
