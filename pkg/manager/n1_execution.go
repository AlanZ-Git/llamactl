package manager

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"llamactl/pkg/database"
	"llamactl/pkg/instance"
	"runtime"
	"strconv"
	"time"
)

func (im *instanceManager) n1Candidate(ctx context.Context, n *n1Coordinator, inst *instance.Instance, c N1Candidate, ref *instance.RecipeRef) (*instance.Options, error) {
	if runtime.GOOS == "windows" && !im.globalConfig.N1.AdministratorReadOnlyPlacement {
		return nil, protocolError("read_only_placement_required")
	}
	a, err := n.store.Artifact(ctx, c.ArtifactID)
	if err != nil {
		return nil, err
	}
	if a.ManifestDigest != c.ArtifactDigest {
		return nil, protocolError("artifact_changed")
	}
	path, projector, err := n.inventory.Resolve(ctx, a)
	if err != nil {
		return nil, err
	}
	opts, err := inst.RegistrationCandidate(a.ID, path, projector)
	if err != nil {
		return nil, err
	}
	patch, err := instance.NormalizeControlPatch(n1JSON(c.Deployment))
	if err != nil {
		return nil, protocolError("unsupported_parameter")
	}
	m := opts.Control.Models[a.ID]
	m.Parameters = patch
	opts.Control.Models[a.ID] = m
	opts.Control.Target = a.ID
	opts.PresetIni = nil
	opts.ActiveRecipe = ref
	opts.DeploymentDigest = candidateDigest(c)
	if err = opts.ValidateControlled(im.globalConfig); err != nil {
		return nil, protocolError("invalid_config")
	}
	return opts, nil
}
func (im *instanceManager) n1Accept(ctx context.Context, n *n1Coordinator, inst *instance.Instance, scope, kind string, body []byte) (int, any, error) {
	var eval n1Evaluation
	var use n1Use
	var envelope n1Envelope
	var recipe N1Recipe
	var row database.RecipeRecord
	var budget database.RunBudget
	wall := 0
	var canonical json.RawMessage
	cfg := im.globalConfig.N1
	if kind == "evaluation" {
		if err := n1Decode(body, &eval); err != nil {
			return 0, nil, err
		}
		envelope = eval.n1Envelope
		wall = eval.WallSeconds
		budget = eval.Budget
		if wall < 2 || int64(wall) > n1Limit(int64(cfg.MaxWallSeconds), 1800, 1800) || eval.CleanupSeconds < 1 || eval.CleanupSeconds > 30 || eval.AttemptSeconds <= eval.CleanupSeconds || eval.AttemptSeconds > wall || budget.Requests < 1 || budget.Requests > n1Limit(cfg.MaxRequests, 64, 256) || budget.Tokens < 1 || budget.Tokens > n1Limit(cfg.MaxTokens, 32768, 1048576) || budget.Bytes < 1 || budget.Bytes > n1Limit(cfg.MaxBytes, 16<<20, 16<<20) || budget.ReservedRequests != 0 || budget.ReservedTokens != 0 || budget.ReservedBytes != 0 || eval.Notice == "" || len(eval.Notice) > 512 || eval.NotBefore <= 0 || time.Now().UnixMilli() < eval.NotBefore || eval.FixtureVersion == "" || eval.Purpose != "synthetic_only" || len(eval.Candidates) < 1 || len(eval.Candidates) > 64 || eval.Ranges == nil {
			return 0, nil, protocolError("invalid_test_budget")
		}
		seen := map[string]bool{}
		for idx := range eval.Candidates {
			c := &eval.Candidates[idx]
			if c.ID == "" || seen[c.ID] || len(c.ID) > 128 {
				return 0, nil, protocolError("invalid_config")
			}
			seen[c.ID] = true
			patch, err := instance.NormalizeControlPatch(n1JSON(c.Deployment))
			if err != nil {
				return 0, nil, protocolError("unsupported_parameter")
			}
			c.Deployment = patch
			if err = instance.ValidateRequestContract(c.Request); err != nil {
				return 0, nil, err
			}
			for key, raw := range patch {
				spec := instance.ControlParameters()[key]
				if spec.Type != "integer" || string(raw) == "null" {
					continue
				}
				bounds, ok := eval.Ranges[key]
				var value int64
				_ = json.Unmarshal(raw, &value)
				if !ok || bounds[0] > bounds[1] || bounds[0] < spec.Minimum || bounds[1] > spec.Maximum || value < bounds[0] || value > bounds[1] {
					return 0, nil, protocolError("parameter_range_mismatch")
				}
			}
		}
		for key, bounds := range eval.Ranges {
			spec, ok := instance.ControlParameters()[key]
			if !ok || spec.Type != "integer" || bounds[0] < spec.Minimum || bounds[1] > spec.Maximum || bounds[0] > bounds[1] {
				return 0, nil, protocolError("parameter_range_mismatch")
			}
		}
		canonical = n1JSON(eval)
	} else {
		if err := n1Decode(body, &use); err != nil {
			return 0, nil, err
		}
		envelope = use.n1Envelope
		if use.IdleSeconds == 0 {
			use.IdleSeconds = 60
		}
		if use.IdleSeconds < 1 || use.IdleSeconds > 300 || use.Version < 1 {
			return 0, nil, protocolError("invalid_config")
		}
		wall = int(n1Limit(int64(cfg.ServiceAbsoluteSeconds), 1800, 1800))
		budget = database.RunBudget{Requests: 1, Tokens: 1, Bytes: 1}
		canonical = n1JSON(use)
	}
	// Lookup precedes revision and busy checks: accepted work advances revisions.
	if envelope.Boot != im.initControl().boot {
		return 0, nil, protocolError("epoch_changed")
	}
	if old, err := n.store.Lookup(ctx, scope, inst.Name, envelope.Boot, envelope.Key); err == nil {
		if old.Kind != kind || old.PayloadDigest != database.N1Digest(canonical) {
			return 0, nil, protocolError("idempotency_conflict")
		}
		return 202, old, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return 0, nil, err
	}
	if err := n1CheckEnvelope(inst, envelope, true); err != nil {
		return 0, nil, err
	}
	// Applicability is checked only for new work, never for an accepted retry.
	if kind == "evaluation" {
		for _, c := range eval.Candidates {
			if _, err := im.n1Candidate(ctx, n, inst, c, nil); err != nil {
				return 0, nil, err
			}
		}
	} else {
		var err error
		row, err = n.store.GetRecipe(ctx, inst.Name, use.RecipeID, use.Version)
		if err != nil {
			return 0, nil, err
		}
		if row.Publication != "published" {
			return 0, nil, protocolError("recipe_unpublished")
		}
		if err = json.Unmarshal(row.Document, &recipe); err != nil {
			return 0, nil, err
		}
		if err = im.n1RecipeApplicable(ctx, n, recipe); err != nil {
			return 0, nil, err
		}
		if err = im.n1RecipeEvidence(ctx, n, recipe); err != nil {
			return 0, nil, err
		}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.active != nil {
		return 0, nil, instance.BusyError()
	}
	if n.ctx.Err() != nil {
		return 0, nil, protocolError("manager_stopping")
	}
	// This first implementation deliberately binds one configured controlled Router.
	for _, other := range im.registry.list() {
		if other != inst && !other.IsRemote() && other.IsRunning() {
			return 0, nil, instance.BusyError()
		}
	}
	s := inst.ControlSnapshot()
	borrowed := false
	if s.ActualModel != "" {
		if kind != "switch" || s.State != "ready" || inst.GetOptions().ActiveRecipe == nil || *inst.GetOptions().ActiveRecipe != (instance.RecipeRef{ID: row.ID, Version: row.Version}) || s.ActualModel != recipe.ArtifactID {
			return 0, nil, protocolError("model_ownership_unknown")
		}
		borrowed = true
	}
	id := newControlID()
	deadline := time.Now().Add(time.Duration(wall) * time.Second)
	reservation, err := inst.ReserveN1(id, scope, map[bool]string{true: "evaluation", false: "service_use"}[kind == "evaluation"], envelope.Boot, *envelope.Revision, deadline)
	if err != nil {
		return 0, nil, err
	}
	for _, other := range im.registry.list() {
		if other != inst && !other.IsRemote() && other.IsRunning() {
			reservation.CloseConfirmed()
			return 0, nil, instance.BusyError()
		}
	}
	manifest := n1JSON(map[string]any{"schema_version": 1, "engine_identity": cfg.EngineIdentity, "request": json.RawMessage(canonical)})
	r, _, err := n.store.Accept(ctx, database.ExecutionRun{ID: id, Kind: kind, Scope: scope, Instance: inst.Name, Boot: envelope.Boot, Key: envelope.Key, Payload: canonical, Manifest: manifest, Deadline: deadline.UnixMilli(), Budget: budget})
	if err != nil {
		reservation.CloseConfirmed()
		return 0, nil, err
	}
	l := &n1Live{run: r, reservation: reservation, inst: inst, eval: eval, use: use, recipe: recipe, record: row, lastSeen: time.Now(), borrowed: borrowed, identity: s}
	n.active = l
	if kind == "switch" {
		if _, _, err = n.store.Dispatch(ctx, id, "switch", n1JSON(map[string]any{"recipe_id": row.ID, "version": row.Version, "recipe_digest": row.Digest, "borrowed": borrowed}), 1, 1); err != nil {
			n.beginClosing(l, "failed")
			return 0, nil, err
		}
		l.busy = true
		n.wg.Add(1)
		go im.n1Switch(n, l)
	}
	return 202, r, nil
}
func (im *instanceManager) n1Attempts(ctx context.Context, n *n1Coordinator, inst *instance.Instance, r database.ExecutionRun, method string, parts []string, body []byte) (int, any, error) {
	if r.Kind != "evaluation" {
		return 0, nil, protocolError("n1_not_found")
	}
	if len(parts) >= 4 {
		seq, err := strconv.Atoi(parts[3])
		if err != nil || seq < 1 {
			return 0, nil, protocolError("invalid_config")
		}
		events, err := n.store.Events(ctx, r.ID)
		if err != nil {
			return 0, nil, err
		}
		var dispatch *database.ExecutionEvent
		var receipt *n1Receipt
		var retained string
		for idx, e := range events {
			switch e.Type {
			case "dispatch":
				var d struct {
					Seq int `json:"seq"`
				}
				_ = json.Unmarshal(e.Payload, &d)
				if d.Seq == seq {
					copy := events[idx]
					dispatch = &copy
				}
			case "receipt":
				var v n1Receipt
				_ = json.Unmarshal(e.Payload, &v)
				if v.Attempt == seq {
					receipt = &v
				}
			case "output":
				if e.Key == strconv.Itoa(seq) {
					_ = json.Unmarshal(e.Payload, &retained)
				}
			}
		}
		if dispatch == nil {
			return 0, nil, protocolError("n1_not_found")
		}
		if len(parts) == 5 && parts[4] == "ack" && method == "POST" {
			var ack struct {
				Digest string `json:"digest"`
			}
			if err = n1Decode(body, &ack); err != nil {
				return 0, nil, err
			}
			if receipt == nil || ack.Digest != receipt.OutputDigest {
				return 0, nil, protocolError("output_digest_mismatch")
			}
			if _, err = n.store.Append(ctx, r.ID, "ack", strconv.Itoa(seq), n1JSON(ack)); err != nil {
				return 0, nil, err
			}
			n.mu.Lock()
			delete(n.deliveries, r.ID+":"+strconv.Itoa(seq))
			n.mu.Unlock()
			return 200, map[string]any{"acknowledged": true}, nil
		}
		if len(parts) == 4 && method == "GET" {
			out := map[string]any{"dispatch": dispatch, "receipt": receipt, "output_state": "pending"}
			if receipt != nil {
				out["output_state"] = "output_unavailable"
				n.mu.Lock()
				d, ok := n.deliveries[r.ID+":"+strconv.Itoa(seq)]
				n.mu.Unlock()
				if ok && time.Now().Before(d.expires) {
					out["body"] = d.body
					out["output_state"] = "available"
				} else if retained != "" {
					out["body"] = retained
					out["output_state"] = "retained"
				}
			}
			return 200, out, nil
		}
		return 0, nil, protocolError("n1_not_found")
	}
	if method != "POST" || len(parts) != 3 {
		return 0, nil, protocolError("n1_not_found")
	}
	var req n1Attempt
	if err := n1Decode(body, &req); err != nil {
		return 0, nil, err
	}
	if err := n1CheckEnvelope(inst, req.n1Envelope, false); err != nil {
		return 0, nil, err
	}
	if req.Seq < 1 || req.Seq > 256 || req.Fixture == "" || len(req.Messages) == 0 || req.FixtureDigest != database.N1Digest(req.Messages) {
		return 0, nil, protocolError("invalid_config")
	}
	// Canonicalize the declared messages before comparing their sample digest.
	var msgs any
	if err := instance.StrictJSON(req.Messages, &msgs); err != nil {
		return 0, nil, protocolError("invalid_config")
	}
	canonicalMessages := n1JSON(msgs)
	if req.FixtureDigest != database.N1Digest(canonicalMessages) {
		return 0, nil, protocolError("fixture_digest_mismatch")
	}
	req.Messages = canonicalMessages
	payload := n1JSON(map[string]any{"seq": req.Seq, "candidate_id": req.Candidate, "fixture_id": req.Fixture, "fixture_digest": req.FixtureDigest, "request_digest": database.N1Digest(n1JSON(req))})
	events, err := n.store.Events(ctx, r.ID)
	if err != nil {
		return 0, nil, err
	}
	dispatchCount := 0
	for _, e := range events {
		if e.Type != "dispatch" {
			continue
		}
		dispatchCount++
		if e.Key == req.Key {
			if e.Digest != database.N1Digest(payload) {
				return 0, nil, protocolError("idempotency_conflict")
			}
			return 202, e, nil
		}
	}
	if req.Seq != dispatchCount+1 {
		return 0, nil, protocolError("attempt_sequence_conflict")
	}
	if err = n1CheckEnvelope(inst, req.n1Envelope, true); err != nil {
		return 0, nil, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	l := n.active
	if l == nil || l.run.ID != r.ID || l.closing {
		return 0, nil, protocolError("run_not_active")
	}
	if l.busy {
		return 0, nil, instance.BusyError()
	}
	if l.reservation.Expired() {
		return 0, nil, protocolError("deadline_exceeded")
	}
	if l.reservation.Context().Err() != nil {
		return 0, nil, protocolError("run_not_active")
	}
	var candidate *N1Candidate
	for idx := range l.eval.Candidates {
		if l.eval.Candidates[idx].ID == req.Candidate {
			candidate = &l.eval.Candidates[idx]
			break
		}
	}
	if candidate == nil {
		return 0, nil, protocolError("candidate_not_declared")
	}
	var maxTokens int64
	_ = json.Unmarshal(candidate.Request.Parameters["max_tokens"], &maxTokens)
	maxBytes := min(n1Limit(im.globalConfig.N1.MaxBodyBytes, 1<<20, 1<<20), r.Budget.Bytes-r.Budget.ReservedBytes)
	if maxBytes < 1 {
		return 0, nil, database.ErrN1Budget
	}
	dispatch, replay, err := n.store.Dispatch(ctx, r.ID, req.Key, payload, maxTokens, maxBytes)
	if err != nil {
		return 0, nil, err
	}
	if replay {
		return 202, dispatch, nil
	}
	l.busy = true
	l.lastSeen = time.Now()
	workDeadline := time.Now().Add(time.Duration(l.eval.AttemptSeconds-l.eval.CleanupSeconds) * time.Second)
	absolute := time.UnixMilli(l.run.Deadline).Add(-time.Duration(l.eval.CleanupSeconds) * time.Second)
	if absolute.Before(workDeadline) {
		workDeadline = absolute
	}
	work, cancel := context.WithDeadline(l.reservation.Context(), workDeadline)
	l.cancel = cancel
	n.wg.Add(1)
	go im.n1AttemptWork(work, n, l, req, *candidate, maxBytes)
	return 202, dispatch, nil
}
func (im *instanceManager) n1Safety(ctx context.Context, n *n1Coordinator, inst *instance.Instance, r database.ExecutionRun, action string, body []byte) (int, any, error) {
	var req struct {
		n1Envelope
		IdleSeconds int `json:"idle_seconds"`
	}
	if err := n1Decode(body, &req); err != nil {
		return 0, nil, err
	}
	if err := n1CheckEnvelope(inst, req.n1Envelope, false); err != nil {
		return 0, nil, err
	}
	if r.Boot != req.Boot {
		return 0, nil, protocolError("epoch_changed")
	}
	if r.FrozenAt != nil {
		return 200, r, nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	l := n.active
	if l == nil || l.run.ID != r.ID {
		return 0, nil, protocolError("recovery_required")
	}
	if action == "heartbeat" || action == "renew" {
		if l.closing || l.reservation.Expired() || l.reservation.Context().Err() != nil {
			return 0, nil, protocolError("use_invalid")
		}
		if action == "renew" {
			idle := req.IdleSeconds
			if idle == 0 {
				idle = 60
			}
			if err := l.reservation.Renew(time.Duration(idle) * time.Second); err != nil {
				return 0, nil, err
			}
		}
		l.lastSeen = time.Now()
		return 200, r, nil
	}
	if _, err := n.store.Append(ctx, r.ID, "safety", req.Key, n1JSON(map[string]any{"action": action, "boot_id": req.Boot})); err != nil {
		return 0, nil, err
	}
	state := "succeeded"
	if action == "cancel" {
		state = "cancelled"
	}
	n.beginClosing(l, state)
	if !l.busy {
		l.busy = true
		n.wg.Add(1)
		go im.n1Finish(n, l)
	}
	return 200, r, nil
}

var _ = fmt.Sprint
