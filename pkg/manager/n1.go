package manager

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"llamactl/pkg/database"
	"llamactl/pkg/instance"
	"llamactl/pkg/models"
	"llamactl/pkg/resourcepolicy"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type N1Manager interface {
	N1Request(context.Context, string, string, bool, string, string, url.Values, []byte) (int, any, error)
	N1Capability(context.Context, int, int, string) (bool, error)
}
type n1Delivery struct {
	body    string
	digest  string
	expires time.Time
}
type n1Live struct {
	run                            database.ExecutionRun
	reservation                    *instance.Reservation
	inst                           *instance.Instance
	eval                           n1Evaluation
	use                            n1Use
	recipe                         N1Recipe
	record                         database.RecipeRecord
	busy, closing, borrowed, owned bool
	blocked                        bool
	identity                       instance.ControlSnapshot
	loaded                         string
	lastSeen                       time.Time
	cancel                         context.CancelFunc
	terminal                       string
}
type n1Coordinator struct {
	mu         sync.Mutex
	store      *database.N1Store
	inventory  *models.Inventory
	err        error
	active     *n1Live
	deliveries map[string]n1Delivery
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
}

// Callers hold n.mu. Context cancellation is not evidence of time expiry.
func (n *n1Coordinator) stopCause(l *n1Live) string {
	if (n.ctx != nil && n.ctx.Err() != nil) || l.reservation.Context().Err() == context.Canceled {
		return "cancelled"
	}
	if l.reservation.Expired() || (l.run.Kind == "evaluation" && time.Since(l.lastSeen) > 60*time.Second) {
		return "deadline_exceeded"
	}
	return ""
}
func (n *n1Coordinator) beginClosing(l *n1Live, state string) {
	// Establish the first cause before cancelling work. Cleanup can outlive the
	// deadline or be delegated to owner without rewriting that execution fact.
	if l.terminal == "" {
		l.terminal = n.stopCause(l)
		if l.terminal == "" {
			l.terminal = state
		}
	}
	l.closing = true
	l.reservation.BeginClosing()
	if l.cancel != nil {
		l.cancel()
	}
}

func (im *instanceManager) initN1() *n1Coordinator {
	im.n1Once.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		n := &n1Coordinator{ctx: ctx, cancel: cancel, deliveries: map[string]n1Delivery{}}
		im.n1 = n
		provider, ok := im.db.(interface{ N1Store() *database.N1Store })
		if !ok || im.globalConfig == nil || im.globalConfig.N1.Instance == "" {
			n.err = protocolError("n1_unavailable")
			return
		}
		n.store = provider.N1Store()
		n.err = n.store.Interrupt(ctx, im.initControl().boot)
		if n.err != nil {
			return
		}
		n.inventory, n.err = models.NewInventory(im.globalConfig.N1.Roots)
		if n.err != nil {
			return
		}
		artifacts, err := n.store.ArtifactCache(ctx)
		if err != nil {
			n.err = err
			return
		}
		for _, a := range artifacts {
			n.inventory.Remember(a)
		}
		if im.globalConfig.N1.EngineIdentity == "" {
			n.err = protocolError("engine_identity_required")
			return
		}
		n.wg.Add(1)
		go func() {
			defer n.wg.Done()
			ticker := time.NewTicker(200 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					im.n1Tick(n)
				}
			}
		}()
	})
	return im.n1
}
func (im *instanceManager) shutdownN1() {
	n := im.initN1()
	n.cancel()
	n.mu.Lock()
	if l := n.active; l != nil {
		n.beginClosing(l, "cancelled")
		if !l.busy {
			l.busy = true
			n.wg.Add(1)
			go im.n1Finish(n, l)
		}
	}
	n.mu.Unlock()
	n.wg.Wait()
	if n.inventory != nil {
		n.inventory.Close()
	}
}
func (im *instanceManager) n1Tick(n *n1Coordinator) {
	n.mu.Lock()
	defer n.mu.Unlock()
	now := time.Now()
	for k, v := range n.deliveries {
		if !now.Before(v.expires) {
			delete(n.deliveries, k)
		}
	}
	l := n.active
	if l == nil {
		return
	}
	cause := n.stopCause(l)
	if cause == "" && l.terminal == "" && strings.HasPrefix(l.run.Scope, "key:") {
		id, _ := strconv.Atoi(strings.TrimPrefix(l.run.Scope, "key:"))
		cap := "evaluate"
		if l.run.Kind == "switch" {
			cap = "service_use"
		}
		allowed, err := n.store.HasCapability(n.ctx, id, l.inst.ID, cap)
		if err != nil || !allowed {
			cause = "cancelled"
		}
	}
	if cause != "" {
		n.beginClosing(l, cause)
	}
	if l.closing && !l.busy {
		l.busy = true
		n.wg.Add(1)
		go im.n1Finish(n, l)
	}
}
func (im *instanceManager) N1Capability(ctx context.Context, key, inst int, cap string) (bool, error) {
	n := im.initN1()
	if n.err != nil {
		return false, n.err
	}
	return n.store.HasCapability(ctx, key, inst, cap)
}
func n1Error(err error) error {
	if err == nil {
		return nil
	}
	var admission *instance.AdmissionError
	if errors.As(err, &admission) || errors.Is(err, resourcepolicy.ErrPaused) {
		return err
	}
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return protocolError("n1_not_found")
	case errors.Is(err, database.ErrN1Conflict):
		return protocolError("idempotency_conflict")
	case errors.Is(err, database.ErrN1Budget):
		return protocolError("budget_exhausted")
	case errors.Is(err, database.ErrN1State):
		return protocolError("invalid_execution_state")
	}
	return protocolError("n1_storage_or_identity_failed")
}
func n1Decode(body []byte, out any) error {
	if err := instance.StrictJSON(body, out); err != nil {
		return protocolError("invalid_config")
	}
	return nil
}
func n1JSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	var canonical any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if d.Decode(&canonical) != nil {
		return nil
	}
	b, _ = json.Marshal(canonical)
	return b
}
func n1CheckEnvelope(inst *instance.Instance, e n1Envelope, revision bool) error {
	if e.Boot == "" || !resourcepolicy.ValidReleaseID(e.Key) || e.Revision == nil {
		return protocolError("invalid_config")
	}
	s := inst.ControlSnapshot()
	if e.Boot != s.BootID {
		return protocolError("epoch_changed")
	}
	if revision && *e.Revision != s.Revision {
		return protocolError("revision_conflict")
	}
	return nil
}
func (im *instanceManager) N1Request(ctx context.Context, name, scope string, admin bool, method, path string, query url.Values, body []byte) (status int, out any, err error) {
	defer func() { err = n1Error(err) }()
	n := im.initN1()
	if n.err != nil {
		return 0, nil, n.err
	}
	if name != im.globalConfig.N1.Instance {
		return 0, nil, protocolError("n1_not_found")
	}
	inst, err := im.controlInstance(name)
	if err != nil {
		return 0, nil, err
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch parts[0] {
	case "capabilities":
		if !admin {
			return 0, nil, protocolError("administrator_required")
		}
		if len(parts) != 2 || method != "PUT" {
			break
		}
		key, parseErr := strconv.Atoi(parts[1])
		if parseErr != nil || key < 1 {
			return 0, nil, protocolError("invalid_config")
		}
		var req struct {
			Capabilities []string `json:"capabilities"`
		}
		if e := n1Decode(body, &req); e != nil {
			return 0, nil, e
		}
		if e := n.store.SetCapabilities(ctx, key, inst.ID, req.Capabilities); e != nil {
			return 0, nil, e
		}
		return 200, map[string]any{"key_id": key, "instance_id": inst.ID, "capabilities": req.Capabilities}, nil
	case "observation":
		if method != "GET" {
			break
		}
		s, ok := im.ResourceSnapshot()
		if !ok {
			return 200, map[string]any{"available": false, "reason": "sampler_unavailable"}, nil
		}
		s.Processes = nil
		return 200, s, nil
	case "inventory":
		return im.n1Inventory(ctx, n, inst, scope, method, parts, query, body)
	case "recipes":
		return im.n1Recipes(ctx, n, inst, scope, admin, method, parts, query, body)
	case "evaluation-runs", "service-uses":
		kind := "evaluation"
		if parts[0] == "service-uses" {
			kind = "switch"
		}
		if len(parts) == 1 {
			if method == "POST" {
				return im.n1Accept(ctx, n, inst, scope, kind, body)
			}
			if method == "GET" {
				if query.Get("key") != "" {
					r, e := n.store.Lookup(ctx, scope, name, query.Get("accepted_boot"), query.Get("key"))
					if e != nil {
						return 0, nil, e
					}
					return im.n1View(ctx, n, r)
				}
				rows, e := n.store.List(ctx, scope, name, admin)
				return 200, map[string]any{"runs": rows}, e
			}
		}
		if len(parts) < 2 {
			break
		}
		r, e := n.store.Get(ctx, parts[1])
		if e != nil {
			return 0, nil, e
		}
		if r.Instance != name || r.Kind != kind || (!admin && r.Scope != scope) {
			return 0, nil, protocolError("n1_not_found")
		}
		if len(parts) == 2 && method == "GET" {
			return im.n1View(ctx, n, r)
		}
		if len(parts) >= 3 {
			switch parts[2] {
			case "attempts":
				return im.n1Attempts(ctx, n, inst, r, method, parts, body)
			case "analysis":
				if method == "POST" {
					return im.n1Analyze(ctx, n, r, body)
				}
			case "finish", "cancel", "release", "heartbeat", "renew":
				if method == "POST" {
					return im.n1Safety(ctx, n, inst, r, parts[2], body)
				}
			}
		}
	}
	return 0, nil, protocolError("n1_not_found")
}
func (im *instanceManager) n1Inventory(ctx context.Context, n *n1Coordinator, inst *instance.Instance, scope, method string, parts []string, query url.Values, body []byte) (int, any, error) {
	if len(parts) == 1 && method == "GET" {
		s := inst.ControlSnapshot()
		return 200, map[string]any{"roots": n.inventory.Roots(), "boot_id": s.BootID, "revision": s.Revision}, nil
	}
	if len(parts) == 2 && parts[1] == "scans" && method == "POST" {
		var req struct {
			Root         string `json:"root_id"`
			Filename     string `json:"filename"`
			Scan         string `json:"scan_id"`
			Continuation string `json:"continuation"`
		}
		if err := n1Decode(body, &req); err != nil {
			return 0, nil, err
		}
		var s models.InventoryScan
		var err error
		if req.Continuation != "" {
			s, err = n.inventory.Continue(ctx, req.Scan, req.Continuation, req.Filename)
		} else {
			s, err = n.inventory.Scan(ctx, req.Root, req.Filename)
		}
		if err != nil && (err.Error() == "invalid_filename" || err.Error() == "continuation_selection_conflict") {
			return 0, nil, protocolError("invalid_config")
		}
		return 200, s, err
	}
	if len(parts) == 3 && parts[1] == "scans" && method == "GET" {
		offset, _ := strconv.Atoi(query.Get("offset"))
		limit := 100
		if query.Get("limit") != "" {
			limit, _ = strconv.Atoi(query.Get("limit"))
		}
		s, e := n.inventory.Page(parts[2], offset, limit)
		return 200, s, e
	}
	if len(parts) == 2 && method == "POST" {
		var req struct {
			n1Envelope
			Scan      string `json:"scan_id"`
			Candidate string `json:"candidate_id"`
			Projector string `json:"projector_id"`
			Artifact  string `json:"artifact_id"`
		}
		if err := n1Decode(body, &req); err != nil {
			return 0, nil, err
		}
		if err := n1CheckEnvelope(inst, req.n1Envelope, false); err != nil {
			return 0, nil, err
		}
		payload := n1JSON(map[string]any{"action": parts[1], "request": req})
		if parts[1] == "register" {
			if old, e := n.store.Lookup(ctx, scope, inst.Name, req.Boot, req.Key); e == nil {
				if old.PayloadDigest != database.N1Digest(payload) {
					return 0, nil, protocolError("idempotency_conflict")
				}
				if old.FrozenAt == nil {
					return 0, nil, protocolError("recovery_required")
				}
				return 200, old.Terminal, nil
			} else if !errors.Is(e, sql.ErrNoRows) {
				return 0, nil, e
			}
		}
		if err := n1CheckEnvelope(inst, req.n1Envelope, true); err != nil {
			return 0, nil, err
		}
		switch parts[1] {
		case "verify":
			a, err := n.inventory.Verify(ctx, req.Scan, req.Candidate, req.Projector)
			if err != nil {
				return 0, nil, err
			}
			a, err = n.store.SaveArtifact(ctx, a)
			return 200, a, err
		case "register":
			a, err := n.store.Artifact(ctx, req.Artifact)
			if err != nil {
				return 0, nil, err
			}
			path, projector, err := n.inventory.Resolve(ctx, a)
			if err != nil {
				return 0, nil, err
			}
			m, err := inst.BeginControl(req.Boot, *req.Revision, true, false)
			if err != nil {
				return 0, nil, err
			}
			defer m.Close()
			if inst.IsRunning() {
				return 0, nil, instance.BusyError()
			}
			candidate, err := inst.RegistrationCandidate(a.ID, path, projector)
			if err != nil {
				return 0, nil, err
			}
			candidate.ConfigRevision = *req.Revision + 1
			run, _, err := n.store.Accept(ctx, database.ExecutionRun{ID: newControlID(), Kind: "switch", Scope: scope, Instance: inst.Name, Boot: req.Boot, Key: req.Key, Payload: payload, Manifest: payload, Deadline: time.Now().Add(30 * time.Second).UnixMilli(), Budget: database.RunBudget{Requests: 1, Tokens: 1, Bytes: 1}})
			if err != nil {
				return 0, nil, err
			}
			if _, _, err = n.store.Dispatch(ctx, run.ID, "register", payload, 1, 1); err != nil {
				m.FinishControl("failed", "")
				return 0, nil, err
			}
			p, err := m.PrepareOptions(candidate)
			if err != nil {
				m.FinishControl("failed", "")
				return 0, nil, err
			}
			defer p.Close()
			if err = p.PersistControl(ctx, func(next *instance.Instance) error { return im.persistInstance(next) }); err != nil {
				m.FinishControl("failed", "")
				return 0, nil, err
			}
			terminal := map[string]any{"artifact_id": a.ID, "revision": candidate.ConfigRevision, "state": "registered", "operation_id": run.ID}
			if err = n.store.Freeze(ctx, run.ID, "succeeded", "released", n1JSON(terminal)); err != nil {
				m.FinishControl("failed", "")
				return 0, nil, err
			}
			return 200, terminal, nil
		}
	}
	return 0, nil, protocolError("n1_not_found")
}
func (im *instanceManager) n1View(ctx context.Context, n *n1Coordinator, r database.ExecutionRun) (int, any, error) {
	b := n1JSON(r)
	out := map[string]any{}
	_ = json.Unmarshal(b, &out)
	events, err := n.store.Events(ctx, r.ID)
	if err != nil {
		return 0, nil, err
	}
	out["events"] = events
	out["manifest_digest"] = database.N1Digest(r.Manifest)
	out["attempt_receipt_set_digest"] = receiptSetDigest(events)
	if r.FrozenAt != nil && time.Now().UnixMilli() > *r.FrozenAt+int64(24*time.Hour/time.Millisecond) && r.AnalysisState == "awaiting" {
		out["analysis_state"] = "not_available"
	}
	n.mu.Lock()
	l := n.active
	if l != nil && l.run.ID == r.ID {
		out["active_requests"] = l.reservation.ActiveRequests()
		if r.Kind == "switch" && !l.closing && !l.busy {
			s := l.inst.ControlSnapshot()
			out["ready_token"] = s.ReadyToken
			out["use_id"] = r.ID
			out["recipe_id"] = l.record.ID
			out["recipe_version"] = l.record.Version
			out["model"] = l.recipe.ArtifactID
			out["borrowed"] = l.borrowed
			out["request_contract"] = l.recipe.Request
			out["prompt_digest"] = l.recipe.Request.PromptDigest()
			out["identity"] = s
		}
	}
	n.mu.Unlock()
	return 200, out, nil
}
func receiptSetDigest(events []database.ExecutionEvent) string {
	refs := []map[string]any{}
	for _, e := range events {
		if e.Type == "receipt" {
			refs = append(refs, map[string]any{"seq": e.Seq, "digest": e.Digest})
		}
	}
	return database.N1Digest(n1JSON(refs))
}
func n1Limit(value, def, max int64) int64 {
	if value <= 0 {
		return def
	}
	if value > max {
		return max
	}
	return value
}

var _ N1Manager = (*instanceManager)(nil)
var _ = fmt.Sprint
