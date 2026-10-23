package instance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"llamactl/pkg/resourcepolicy"
)

type RecipeRef struct {
	ID      string `json:"recipe_id"`
	Version int    `json:"version"`
}
type RequestContract struct {
	Parameters      map[string]json.RawMessage `json:"parameters"`
	SystemPrefix    string                     `json:"system_prefix"`
	TemplateVersion string                     `json:"template_version"`
}
type serviceScopeKey struct{}
type serviceReservationKey struct{}

func WithServiceScope(r *http.Request, scope string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), serviceScopeKey{}, scope))
}

// Reservation is a finite upper admission capability, not a Maintenance lease.
// All fields except the immutable context/identity are protected by admission.mu.
type Reservation struct {
	instance              *Instance
	id, scope, kind, boot string
	absolute, idleUntil   time.Time
	idle                  time.Duration
	active                int
	closing, ready        bool
	recipe                RecipeRef
	model                 string
	contract              RequestContract
	promptDigest          string
	permit                *resourcepolicy.Permit
	ctx                   context.Context
	cancel                context.CancelFunc
	unbind                func()
	laneRelease           func()
}

func (i *Instance) ReserveN1(id, scope, kind, boot string, revision uint64, deadline time.Time) (*Reservation, error) {
	if !i.ProtocolEnabled() || (kind != "evaluation" && kind != "service_use") || !deadline.After(time.Now()) || deadline.After(time.Now().Add(30*time.Minute)) {
		return nil, controlError("invalid_reservation")
	}
	permit, err := i.policy.Acquire()
	if err != nil {
		return nil, err
	}
	laneRelease, ok := i.lane.Reserve()
	if !ok {
		permit.Close()
		return nil, BusyError()
	}
	accepted := false
	defer func() {
		if !accepted {
			permit.Close()
			laneRelease()
		}
	}()
	i.admission.mu.Lock()
	defer i.admission.mu.Unlock()
	if i.control.boot != boot {
		return nil, controlError("epoch_changed")
	}
	if i.GetOptions().ConfigRevision != revision {
		return nil, controlError("revision_conflict")
	}
	if i.admission.reservation != nil || i.admission.uses != 0 || len(i.admission.maintenance) != 0 || i.admission.retired || i.admission.transition {
		return nil, BusyError()
	}
	if i.control.state == "unknown" || i.control.state == "" || i.control.state == "failed" || i.control.state == "interrupted" || i.control.interrupted {
		return nil, controlError("recovery_required")
	}
	permit.CancelOnPause()
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	bound, unbind := permit.BindContext(ctx)
	r := &Reservation{instance: i, id: id, scope: scope, kind: kind, boot: boot, absolute: deadline, permit: permit, ctx: bound, cancel: cancel, unbind: unbind, laneRelease: laneRelease}
	i.admission.reservation = r
	accepted = true
	return r, nil
}
func (r *Reservation) Context() context.Context { return r.ctx }

// DelegateOwner drops this work permit but keeps the admission barrier until the
// owner ticket's receipt confirms release. No old-epoch effects are dispatched.
func (r *Reservation) DelegateOwner() { r.BeginClosing(); r.permit.Close() }
func (r *Reservation) VerifyStoppedRecovery(ctx context.Context) error {
	i := r.instance
	m, err := i.beginN1Control(r.boot, i.GetOptions().ConfigRevision, false, true, r)
	if err != nil {
		return err
	}
	defer m.Close()
	return m.VerifyRecovery(ctx)
}
func (r *Reservation) BeginControl(work bool) (*Maintenance, error) {
	if work {
		if err := r.ctx.Err(); err != nil {
			return nil, err
		}
		if err := r.permit.Check(); err != nil {
			return nil, err
		}
	} else if err := r.permit.CheckEpoch(); err != nil {
		return nil, err
	}
	i := r.instance
	return i.beginN1Control(r.boot, i.GetOptions().ConfigRevision, work, false, r)
}
func (r *Reservation) ActivateService(recipe RecipeRef, model string, contract RequestContract, idle time.Duration) error {
	if err := ValidateRequestContract(contract); err != nil {
		return err
	}
	// Freeze an owned copy once; proxy body parsing uses it outside admission.mu.
	raw, err := json.Marshal(contract)
	if err != nil {
		return controlError("invalid_config")
	}
	var frozen RequestContract
	if json.Unmarshal(raw, &frozen) != nil {
		return controlError("invalid_config")
	}
	promptDigest := frozen.PromptDigest()
	if idle <= 0 || idle > 300*time.Second {
		return controlError("invalid_deadline")
	}
	if err := r.permit.Check(); err != nil {
		return err
	}
	i := r.instance
	i.admission.mu.Lock()
	defer i.admission.mu.Unlock()
	if i.admission.reservation != r || r.ready || r.closing || r.kind != "service_use" || r.ctx.Err() != nil || !i.readyCurrentLocked() || i.control.model != model {
		return controlError("use_invalid")
	}
	r.ready = true
	r.recipe = recipe
	r.model = model
	r.contract = frozen
	r.promptDigest = promptDigest
	r.idle = idle
	r.idleUntil = time.Now().Add(idle)
	return nil
}
func (r *Reservation) Renew(idle time.Duration) error {
	if idle <= 0 || idle > 300*time.Second {
		return controlError("invalid_deadline")
	}
	i := r.instance
	i.admission.mu.Lock()
	defer i.admission.mu.Unlock()
	if i.admission.reservation != r || r.closing || !r.ready || r.expiredLocked(time.Now()) || r.ctx.Err() != nil {
		return controlError("use_invalid")
	}
	if err := r.permit.Check(); err != nil {
		return err
	}
	r.idle = idle
	r.idleUntil = time.Now().Add(idle)
	return nil
}
func (r *Reservation) expiredLocked(now time.Time) bool {
	return !now.Before(r.absolute) || (r.ready && r.active == 0 && !now.Before(r.idleUntil))
}

// Expired reports time limits only. Closing and owner revocation also cancel
// Context, but are not deadlines.
func (r *Reservation) Expired() bool {
	i := r.instance
	i.admission.mu.Lock()
	defer i.admission.mu.Unlock()
	return r.expiredLocked(time.Now())
}
func (r *Reservation) ActiveRequests() int {
	i := r.instance
	i.admission.mu.Lock()
	defer i.admission.mu.Unlock()
	return r.active
}
func (r *Reservation) BeginClosing() {
	i := r.instance
	i.admission.mu.Lock()
	r.closing = true
	i.admission.mu.Unlock()
	r.cancel()
}
func (r *Reservation) CloseConfirmed() error {
	i := r.instance
	i.admission.mu.Lock()
	if i.admission.reservation != r {
		i.admission.mu.Unlock()
		return nil
	}
	if r.active != 0 || i.admission.uses != 0 || len(i.admission.maintenance) != 0 {
		i.admission.mu.Unlock()
		return BusyError()
	}
	i.admission.reservation = nil
	r.closing = true
	i.admission.mu.Unlock()
	r.cancel()
	r.unbind()
	r.permit.Close()
	r.laneRelease()
	return nil
}
func (i *Instance) RecipeCandidate(model string, patch map[string]json.RawMessage, ref RecipeRef, digest string) (*Options, error) {
	full := map[string]json.RawMessage{}
	for k, spec := range ControlParameters() {
		if spec.Nullable {
			full[k] = json.RawMessage(`null`)
		}
	}
	// Non-nullable fields are also removed rather than inherited.
	opts, err := i.GetOptions().Clone()
	if err != nil {
		return nil, err
	}
	if opts.Control == nil {
		return nil, controlError("control_not_enabled")
	}
	m, ok := opts.Control.Models[model]
	if !ok {
		return nil, controlError("model_not_found")
	}
	m.Parameters = map[string]json.RawMessage{}
	for k, v := range patch {
		if string(v) != "null" {
			m.Parameters[k] = v
		}
	}
	opts.Control.Models[model] = m
	opts.Control.Target = model
	opts.PresetIni = nil
	opts.ActiveRecipe = &ref
	opts.DeploymentDigest = digest
	if err = opts.ValidateControlled(i.appConfig()); err != nil {
		return nil, err
	}
	return opts, nil
}
func (i *Instance) RegistrationCandidate(model, path, projector string) (*Options, error) {
	opts, err := i.GetOptions().Clone()
	if err != nil {
		return nil, err
	}
	if opts.Control == nil {
		return nil, controlError("control_not_enabled")
	}
	if _, exists := opts.Control.Models[model]; !exists && len(opts.Control.Models) >= 64 {
		return nil, controlError("mapping_capacity")
	}
	opts.Control.Models[model] = ControlModel{Path: path, Projector: projector}
	opts.PresetIni = nil
	opts.ActiveRecipe = nil
	opts.DeploymentDigest = ""
	if err = opts.ValidateControlled(i.appConfig()); err != nil {
		return nil, err
	}
	return opts, nil
}

// Request constraints are enforced on the actual bytes forwarded, for aggregate
// and instance proxy routes alike. Unknown generation controls are rejected.
func applyRequestContract(body []byte, c RequestContract) ([]byte, error) {
	allowed := []string{"model", "messages", "stream", "max_tokens", "temperature", "top_p", "top_k", "min_p", "seed", "chat_template_kwargs"}
	if err := ValidateRequestContract(c); err != nil {
		return nil, err
	}
	var strict map[string]json.RawMessage
	if StrictJSON(body, &strict) != nil {
		return nil, controlError("request_contract_mismatch")
	}
	f, err := ControlObject(body, allowed...)
	if err != nil {
		return nil, controlError("request_contract_mismatch")
	}
	for k, v := range c.Parameters {
		if old, ok := f[k]; ok {
			var a, b any
			da := json.NewDecoder(bytes.NewReader(old))
			da.UseNumber()
			db := json.NewDecoder(bytes.NewReader(v))
			db.UseNumber()
			if da.Decode(&a) != nil || db.Decode(&b) != nil {
				return nil, controlError("request_contract_mismatch")
			}
			aa, _ := json.Marshal(a)
			bb, _ := json.Marshal(b)
			if !bytes.Equal(aa, bb) {
				return nil, controlError("request_contract_mismatch")
			}
		} else {
			f[k] = v
		}
	}
	for _, k := range allowed[3:] {
		if _, ok := f[k]; ok {
			if _, declared := c.Parameters[k]; !declared {
				return nil, controlError("request_contract_mismatch")
			}
		}
	}
	if raw, ok := f["stream"]; ok && string(raw) != "true" && string(raw) != "false" {
		return nil, controlError("request_contract_mismatch")
	}
	var messages []map[string]json.RawMessage
	if json.Unmarshal(f["messages"], &messages) != nil || len(messages) == 0 || len(messages) > 256 {
		return nil, controlError("request_contract_mismatch")
	}
	for _, m := range messages {
		if len(m) != 2 {
			return nil, controlError("request_contract_mismatch")
		}
		var role, content string
		if json.Unmarshal(m["role"], &role) != nil || json.Unmarshal(m["content"], &content) != nil || (role != "user" && role != "assistant" && role != "system") {
			return nil, controlError("request_contract_mismatch")
		}
	}
	if c.SystemPrefix != "" {
		var role, content string
		_ = json.Unmarshal(messages[0]["role"], &role)
		_ = json.Unmarshal(messages[0]["content"], &content)
		if role == "system" {
			if content != c.SystemPrefix {
				return nil, controlError("request_contract_mismatch")
			}
		} else {
			v, _ := json.Marshal(c.SystemPrefix)
			messages = append([]map[string]json.RawMessage{{"role": json.RawMessage(`"system"`), "content": v}}, messages...)
		}
		f["messages"], _ = json.Marshal(messages)
	}
	return json.Marshal(f)
}
func (i *Instance) acquireServiceRequest(w http.ResponseWriter, r *http.Request, aggregate bool) (func(), bool, error) {
	i.admission.mu.Lock()
	u := i.admission.reservation
	if prior, ok := r.Context().Value(serviceReservationKey{}).(*Reservation); ok {
		err := i.checkServiceLocked(r, prior)
		i.admission.mu.Unlock()
		return func() {}, true, err
	}
	if u == nil && r.Header.Get("X-Service-Use") == "" && i.GetOptions().ActiveRecipe == nil {
		i.admission.mu.Unlock()
		return nil, false, nil
	}
	err := i.checkServiceLocked(r, u)
	i.admission.mu.Unlock()
	if err != nil {
		return nil, true, err
	}
	if r.URL.Path != "/v1/chat/completions" && r.URL.Path != "/chat/completions" {
		return nil, true, controlError("request_contract_mismatch")
	}
	if r.URL.RawQuery != "" || r.Header.Get("Content-Encoding") != "" || r.Body == nil {
		return nil, true, controlError("request_contract_mismatch")
	}
	deadline, err := ServiceRequestDeadline(r, u.absolute)
	if err != nil {
		return nil, true, err
	}
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	stop := context.AfterFunc(u.ctx, cancel)
	accepted := false
	defer func() {
		if !accepted {
			stop()
			cancel()
		}
	}()
	// No admission lock and no detached reader goroutine while a client sends bytes.
	body, err := ReadServiceBody(w, r, ctx)
	if err != nil {
		return nil, true, err
	}
	body, err = applyRequestContract(body, u.contract)
	if err != nil {
		return nil, true, err
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(body, &fields)
	var model string
	_ = json.Unmarshal(fields["model"], &model)
	expected := u.model
	if aggregate {
		expected = i.Name + "/" + expected
	}
	if model != expected {
		return nil, true, controlError("model_mismatch")
	}

	i.admission.mu.Lock()
	defer i.admission.mu.Unlock()
	// Revalidate identity, lease, owner, token and closing atomically with admission.
	if err = i.checkServiceLocked(r, u); err != nil {
		return nil, true, err
	}
	if ctx.Err() != nil {
		return nil, true, controlError("deadline_exceeded")
	}
	identity := map[string]string{
		"X-Recipe-ID": u.recipe.ID, "X-Recipe-Version": strconv.Itoa(u.recipe.Version),
		"X-Boot-ID": i.control.boot, "X-Config-Revision": fmt.Sprint(i.GetOptions().ConfigRevision),
		"X-Process-Generation":      fmt.Sprint(i.control.processGeneration),
		"X-Load-Generation":         fmt.Sprint(i.control.loadGeneration),
		"X-Prompt-Template-Version": u.contract.TemplateVersion, "X-Prompt-Digest": u.promptDigest,
	}
	ctx = context.WithValue(ctx, serviceReservationKey{}, u)
	ctx = context.WithValue(ctx, serviceIdentityKey{}, identity)
	*r = *r.WithContext(ctx)
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	u.active++
	i.admission.uses++
	accepted = true
	var once sync.Once
	return func() {
		once.Do(func() {
			stop()
			cancel()
			i.admission.mu.Lock()
			u.active--
			i.admission.uses--
			u.idleUntil = time.Now().Add(u.idle)
			i.admission.mu.Unlock()
		})
	}, true, nil
}

func (i *Instance) checkServiceLocked(r *http.Request, u *Reservation) error {
	if u == nil || i.admission.reservation != u || u.kind != "service_use" || !u.ready || u.closing || u.ctx.Err() != nil || u.expiredLocked(time.Now()) {
		return controlError("use_invalid")
	}
	if scope, _ := r.Context().Value(serviceScopeKey{}).(string); scope != u.scope {
		return controlError("use_invalid")
	}
	if values := r.Header.Values("X-Service-Use"); len(values) != 1 || values[0] != u.id {
		return controlError("use_invalid")
	}
	if err := u.permit.Check(); err != nil {
		return err
	}
	if len(i.admission.maintenance) != 0 || i.admission.retired || i.admission.transition {
		return BusyError()
	}
	values := r.Header.Values("X-Ready-Token")
	if len(values) != 1 || !i.readyCurrentLocked() || i.control.boot != u.boot || subtle.ConstantTimeCompare([]byte(values[0]), []byte(i.control.token)) != 1 {
		return controlError("ready_token_invalid")
	}
	for key, expected := range map[string]string{"X-Prompt-Template-Version": u.contract.TemplateVersion, "X-Prompt-Digest": u.promptDigest} {
		if values := r.Header.Values(key); len(values) != 1 || values[0] != expected {
			return controlError("request_contract_mismatch")
		}
	}
	return nil
}

type serviceIdentityKey struct{}

// PromptDigest binds the client prompt protocol, not an editable backend template.
// The fixed engine's embedded template remains part of engine/artifact identity.
func (c RequestContract) PromptDigest() string {
	b, _ := json.Marshal(map[string]string{"system_prefix": c.SystemPrefix, "template_version": c.TemplateVersion})
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

func ValidateRequestContract(c RequestContract) error {
	if len(c.SystemPrefix) > 16384 || len(c.TemplateVersion) > 128 {
		return controlError("invalid_config")
	}
	for k, v := range c.Parameters {
		switch k {
		case "chat_template_kwargs":
			fields, err := ControlObject(v, "enable_thinking")
			if err != nil || len(fields) != 1 || (string(fields["enable_thinking"]) != "true" && string(fields["enable_thinking"]) != "false") {
				return controlError("invalid_config")
			}
		case "max_tokens":
			if _, err := NormalizeControlInteger(v, 1, 32768); err != nil {
				return err
			}
		case "seed":
			if _, err := NormalizeControlInteger(v, 0, 2147483647); err != nil {
				return err
			}
		case "top_k":
			if _, err := NormalizeControlInteger(v, 0, 1000); err != nil {
				return err
			}
		case "temperature", "top_p", "min_p":
			var f float64
			if json.Unmarshal(v, &f) != nil || string(v) == "null" || f < 0 || f > 2 || (k != "temperature" && f > 1) {
				return controlError("invalid_config")
			}
		default:
			return controlError("unsupported_parameter")
		}
	}
	if len(c.Parameters["max_tokens"]) == 0 {
		return controlError("invalid_config")
	}
	if strings.TrimSpace(c.TemplateVersion) == "" {
		return controlError("invalid_config")
	}
	return nil
}
