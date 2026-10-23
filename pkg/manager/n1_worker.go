package manager

import (
	"context"
	"encoding/json"
	"errors"
	"llamactl/pkg/database"
	"llamactl/pkg/instance"
	"llamactl/pkg/resourcepolicy"
	"strconv"
	"time"
)

func (im *instanceManager) n1Stage(ctx context.Context, n *n1Coordinator, l *n1Live, stage string, effect func() error) error {
	if _, err := n.store.Append(ctx, l.run.ID, "stage", "", n1JSON(map[string]any{"stage": stage, "at": time.Now().UnixMilli()})); err != nil {
		return err
	}
	baseline, ok := im.ResourceSnapshot()
	started := time.Now()
	a := newDeploymentResources(baseline, ok, l.inst.Name, started)
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				s, ok := im.ResourceSnapshot()
				a.observe(s, ok, l.inst.Name)
			}
		}
	}()
	err := effect()
	close(done)
	<-stopped
	s, ok := im.ResourceSnapshot()
	a.observe(s, ok, l.inst.Name)
	_, persistErr := n.store.Append(context.Background(), l.run.ID, "resources", "", n1JSON(map[string]any{"phase": stage, "start": started, "end": time.Now(), "coverage": a.result(), "conclusion": "observed_only"}))
	if persistErr != nil {
		return persistErr
	}
	return err
}
func (im *instanceManager) n1Apply(ctx context.Context, n *n1Coordinator, l *n1Live, m *instance.Maintenance, c N1Candidate, ref *instance.RecipeRef) error {
	opts, err := im.n1Candidate(ctx, n, l.inst, c, ref)
	if err != nil {
		return err
	}
	if l.inst.IsRunning() {
		if err = m.CheckNoLoadedModel(ctx); err != nil {
			return err
		}
	}
	opts.ConfigRevision = l.inst.GetOptions().ConfigRevision + 1
	prepared, err := m.PrepareOptions(opts)
	if err != nil {
		return err
	}
	defer prepared.Close()
	m.ActivateControl(l.run.ID)
	if err = im.n1Stage(ctx, n, l, "stop", func() error { return im.initControl().stop(ctx, m, l.inst) }); err != nil {
		return err
	}
	if err = im.n1Stage(ctx, n, l, "apply", func() error {
		return prepared.PersistControl(ctx, func(next *instance.Instance) error {
			if store, ok := im.db.(interface {
				SaveContext(context.Context, *instance.Instance) error
			}); ok {
				return store.SaveContext(ctx, next)
			}
			return im.persistInstance(next)
		})
	}); err != nil {
		return err
	}
	l.owned = true
	if err = im.n1Stage(ctx, n, l, "start", func() error { return im.initControl().start(ctx, m, l.inst) }); err != nil {
		return err
	}
	if err = m.WaitRouter(ctx); err != nil {
		return err
	}
	if err = m.CheckNoLoadedModel(ctx); err != nil {
		return err
	}
	l.loaded = c.ArtifactID
	if err = im.n1Stage(ctx, n, l, "load", func() error {
		b, e := m.RouterRequest(ctx, "POST", "/models/load", c.ArtifactID)
		if e != nil {
			return e
		}
		var ack struct {
			Success bool `json:"success"`
		}
		if json.Unmarshal(b, &ack) != nil || !ack.Success {
			return protocolError("model_load_failed")
		}
		return im.initControl().ready(ctx, m, c.ArtifactID)
	}); err != nil {
		return err
	}
	// Recheck the cached identity after load; no second full model hash.
	a, err := n.store.Artifact(ctx, c.ArtifactID)
	if err != nil {
		return err
	}
	if err = n.inventory.Check(ctx, a); err != nil {
		return err
	}
	return m.PublishReady(ctx, c.ArtifactID)
}
func (im *instanceManager) n1Cleanup(n *n1Coordinator, l *n1Live, m *instance.Maintenance, seconds int) (string, error) {
	if l.borrowed {
		return "released", nil
	}
	if !l.owned && l.loaded == "" {
		return "released", nil
	}
	if seconds <= 0 {
		seconds = 5
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(seconds)*time.Second)
	defer cancel()
	if err := m.CheckControl(ctx, false); err != nil {
		return "unknown", err
	}
	// Stop only the process tree started by this operation. Keeping an empty Router
	// is optional; stopping it also gives explicit held-tree/port release evidence.
	err := im.n1Stage(ctx, n, l, "cleanup", func() error {
		if l.loaded != "" {
			b, e := m.RouterRequest(ctx, "POST", "/models/unload", l.loaded)
			if e != nil {
				return e
			}
			var ack struct {
				Success bool `json:"success"`
			}
			if json.Unmarshal(b, &ack) != nil || !ack.Success {
				return protocolError("model_unload_failed")
			}
			if e = m.WaitModel(ctx, l.loaded, "unloaded"); e != nil {
				return e
			}
		}
		return im.initControl().stop(ctx, m, l.inst)
	})
	if err != nil {
		return "unknown", err
	}
	l.loaded = ""
	l.owned = false
	m.FinishControl("stopped", "")
	return "released", nil
}
func n1WorkState(ctx context.Context, err error) string {
	switch {
	case errors.Is(ctx.Err(), context.Canceled), errors.Is(err, context.Canceled), errors.Is(err, resourcepolicy.ErrPaused):
		return "cancelled"
	case errors.Is(ctx.Err(), context.DeadlineExceeded), errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	default:
		return "failed"
	}
}
func (im *instanceManager) n1AttemptWork(ctx context.Context, n *n1Coordinator, l *n1Live, req n1Attempt, candidate N1Candidate, maxBytes int64) {
	defer n.wg.Done()
	m, err := l.reservation.BeginControl(true)
	var generation instance.N1Generation
	cleanup := "released"
	identity := l.inst.ControlSnapshot()
	if err == nil {
		err = im.n1Apply(ctx, n, l, m, candidate, nil)
		if err == nil {
			err = im.n1Stage(ctx, n, l, "inference", func() error {
				var e error
				generation, e = m.GenerateN1(ctx, candidate.ArtifactID, req.Messages, candidate.Request, maxBytes)
				return e
			})
		}
		identity = l.inst.ControlSnapshot()
		l.identity = identity
		l.identity.ReadyToken = ""
	}
	n.mu.Lock()
	if cause := n.stopCause(l); cause != "" {
		n.beginClosing(l, cause)
	} else if n1WorkState(ctx, err) == "cancelled" {
		n.beginClosing(l, "cancelled")
	}
	n.mu.Unlock()
	if m != nil {
		var cleanupErr error
		cleanup, cleanupErr = im.n1Cleanup(n, l, m, l.eval.CleanupSeconds)
		if cleanupErr != nil && err == nil {
			err = cleanupErr
		}
		if cleanup != "released" {
			m.FinishControl("interrupted", candidate.ArtifactID)
		}
		m.Close()
	}
	// Readiness tokens are live capabilities, never persisted evidence.
	identity.ReadyToken = ""
	receipt := n1Receipt{Attempt: req.Seq, Candidate: candidate.ID, ArtifactDigest: candidate.ArtifactDigest, ParameterDigest: candidateDigest(candidate), FixtureDigest: req.FixtureDigest, InputDigest: database.N1Digest(req.Messages), State: "succeeded", Quality: generation.Quality, Cleanup: cleanup, OutputDigest: database.N1Digest(generation.Body), OutputBytes: len(generation.Body), Generation: generation, Identity: identity}
	if err != nil {
		receipt.State = "failed"
		receipt.ErrorCode = deploymentError(ctx, err)
		receipt.Quality = "unknown"
	}
	if receipt.Quality == "" {
		receipt.Quality = "unknown"
	}
	key := strconv.Itoa(req.Seq)
	if l.eval.Retain && len(generation.Body) > 0 {
		_, persistErr := n.store.Append(context.Background(), l.run.ID, "output", key, n1JSON(string(generation.Body)))
		if persistErr != nil {
			im.n1Block(n, l)
			return
		}
	}
	if _, persistErr := n.store.Append(context.Background(), l.run.ID, "receipt", key, n1JSON(receipt)); persistErr != nil {
		im.n1Block(n, l)
		return
	}
	n.mu.Lock()
	if l.cancel != nil {
		l.cancel()
		l.cancel = nil
	}
	if len(generation.Body) > 0 {
		ttl := n1Limit(int64(im.globalConfig.N1.DeliveryTTLSeconds), 300, 300)
		n.deliveries[l.run.ID+":"+key] = n1Delivery{body: string(generation.Body), digest: receipt.OutputDigest, expires: time.Now().Add(time.Duration(ttl) * time.Second)}
	}
	l.busy = false
	if cleanup != "released" {
		n.beginClosing(l, "failed")
	}
	if l.closing {
		l.busy = true
		n.wg.Add(1)
		go im.n1Finish(n, l)
	}
	n.mu.Unlock()
}
func (im *instanceManager) n1Block(n *n1Coordinator, l *n1Live) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.beginClosing(l, "failed")
	l.busy = true
	l.blocked = true
}
func (im *instanceManager) n1Switch(n *n1Coordinator, l *n1Live) {
	defer n.wg.Done()
	ctx, cancel := context.WithTimeout(l.reservation.Context(), time.Duration(im.initControl().budget))
	defer cancel()
	n.mu.Lock()
	l.cancel = cancel
	n.mu.Unlock()
	m, err := l.reservation.BeginControl(true)
	if err == nil {
		if l.borrowed {
			err = im.initControl().ready(ctx, m, l.recipe.ArtifactID)
		} else {
			err = im.n1Apply(ctx, n, l, m, recipeCandidate(l.recipe), &instance.RecipeRef{ID: l.record.ID, Version: l.record.Version})
		}
		if err == nil {
			m.ClearControlOperation()
		}
		l.identity = l.inst.ControlSnapshot()
		l.identity.ReadyToken = ""
		m.Close()
	}
	if err == nil {
		_, err = n.store.Append(context.Background(), l.run.ID, "ready", "switch", n1JSON(map[string]any{"recipe_id": l.record.ID, "version": l.record.Version, "recipe_digest": l.record.Digest, "identity": l.identity, "borrowed": l.borrowed}))
	}
	if err == nil {
		err = l.reservation.ActivateService(instance.RecipeRef{ID: l.record.ID, Version: l.record.Version}, l.recipe.ArtifactID, l.recipe.Request, time.Duration(l.use.IdleSeconds)*time.Second)
	}
	n.mu.Lock()
	l.cancel = nil
	l.busy = false
	if err != nil {
		n.beginClosing(l, n1WorkState(ctx, err))
	}
	if l.closing {
		l.busy = true
		n.wg.Add(1)
		go im.n1Finish(n, l)
	}
	n.mu.Unlock()
}
func (im *instanceManager) n1Finish(n *n1Coordinator, l *n1Live) {
	defer n.wg.Done()
	n.mu.Lock()
	n.beginClosing(l, "succeeded")
	n.mu.Unlock()
	until := time.Now().Add(5 * time.Second)
	for l.reservation.ActiveRequests() != 0 && time.Now().Before(until) {
		time.Sleep(10 * time.Millisecond)
	}
	cleanup := "released"
	var err error
	if l.reservation.ActiveRequests() != 0 {
		cleanup = "unknown"
		err = protocolError("active_requests_not_exited")
	}
	if err == nil && (l.owned || l.loaded != "") {
		var m *instance.Maintenance
		m, err = l.reservation.BeginControl(false)
		if err == nil {
			cleanup, err = im.n1Cleanup(n, l, m, 5)
			m.Close()
		} else {
			cleanup = "unknown"
		}
	}
	if errors.Is(err, resourcepolicy.ErrPaused) {
		cleanup = im.n1DelegateOwner(n, l)
	}
	events, e := n.store.Events(context.Background(), l.run.ID)
	if e != nil {
		im.n1Block(n, l)
		return
	}
	state := "succeeded"
	if l.run.Kind == "evaluation" {
		tested := map[string]bool{}
		failed := false
		for _, event := range events {
			if event.Type != "receipt" {
				continue
			}
			var r n1Receipt
			_ = json.Unmarshal(event.Payload, &r)
			tested[r.Candidate] = true
			failed = failed || r.State != "succeeded"
		}
		if len(tested) != len(l.eval.Candidates) || failed {
			state = "partial"
		}
	}
	n.mu.Lock()
	if l.terminal != "" && l.terminal != "succeeded" {
		state = l.terminal
	}
	n.mu.Unlock()
	execution := state
	if cleanup != "released" {
		state = "partial"
	}
	terminal := n1JSON(map[string]any{"quality": "unknown", "execution_state": execution, "cleanup_state": cleanup, "attempt_receipt_set_digest": receiptSetDigest(events)})
	if err = n.store.Freeze(context.Background(), l.run.ID, state, cleanup, terminal); err != nil {
		im.n1Block(n, l)
		return
	}
	if cleanup == "released" {
		if err = l.reservation.CloseConfirmed(); err != nil {
			im.n1Block(n, l)
			return
		}
		n.mu.Lock()
		if n.active == l {
			n.active = nil
		}
		n.mu.Unlock()
	} else {
		im.n1Block(n, l)
	}
}
func (im *instanceManager) n1DelegateOwner(n *n1Coordinator, l *n1Live) string {
	g := im.globalConfig.ResourcePolicy
	if g == nil {
		return "unknown"
	}
	s := g.Snapshot()
	if s.ReleaseOperation == nil {
		return "unknown"
	}
	op := *s.ReleaseOperation
	identity := l.identity
	identity.ReadyToken = ""
	payload := n1JSON(map[string]any{"release_operation_id": op.ID, "original_run_id": l.run.ID, "original_boot": l.run.Boot, "identity": identity, "delegated_at": time.Now().UnixMilli()})
	if _, err := n.store.Append(context.Background(), l.run.ID, "delegated_owner", op.ID, payload); err != nil {
		return "unknown"
	}
	l.reservation.DelegateOwner()
	until := time.Now().Add(35 * time.Second)
	for time.Now().Before(until) {
		s = g.Snapshot()
		if s.ReleaseOperation == nil || s.ReleaseOperation.ID != op.ID {
			return "unknown"
		}
		if s.ReleaseOperation.EndedAt != nil {
			result := n1JSON(map[string]any{"release_operation_id": op.ID, "owner_receipt": s.ReleaseOperation, "release": s.Release})
			if _, err := n.store.Append(context.Background(), l.run.ID, "owner_release_ref", op.ID, result); err != nil {
				return "unknown"
			}
			if s.Release == "released" {
				l.owned = false
				l.loaded = ""
				return "released"
			}
			return "unknown"
		}
		time.Sleep(20 * time.Millisecond)
	}
	return "unknown"
}
