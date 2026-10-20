package resourcepolicy

import (
	"context"
	"errors"
	"time"
)

// ReleaseCapacity is a boot-local ledger: IDs never expire or get evicted.
// A full ledger rejects new explicit operations until the next manager boot.
const ReleaseCapacity = 1024
const GracefulTimeout = 30 * time.Second
const ImmediateTimeout = 10 * time.Second

var ErrReleaseCapacity = errors.New("release idempotency capacity exhausted")
var ErrReleaseRequest = errors.New("invalid release request")

// ReleaseOperation contains only owner-safe operation metadata.
type ReleaseOperation struct {
	ID             string     `json:"id"`
	Mode           string     `json:"mode"`
	PolicyRevision uint64     `json:"policy_revision"`
	StartedAt      time.Time  `json:"started_at"`
	EndedAt        *time.Time `json:"ended_at,omitempty"`
	ErrorCode      string     `json:"error_code,omitempty"`
}

// ReleaseTicket is a revocable capability, not an ordinary maintenance lease.
// All destructive calls MUST use Effect; waiting for exit must happen outside it.
type ReleaseTicket struct {
	gate      *Gate
	ctx       context.Context
	cancel    context.CancelFunc
	operation ReleaseOperation
	state     string
}

// SetReleaseRunner is configured once by the manager before publication. Boot
// restoration deliberately does not replay any previous release operation.
func (g *Gate) SetReleaseRunner(run func(*ReleaseTicket)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.releaseRunner = run
}

func ValidReleaseID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func (g *Gate) invalidateReleaseLocked() {
	if t := g.releaseCurrent; t != nil {
		t.cancel()
		if t.operation.EndedAt == nil {
			now := time.Now().UTC()
			t.operation.EndedAt, t.operation.ErrorCode, t.state = &now, "superseded", "unreleased_unknown"
		}
	}
	g.releaseCurrent = nil
}

func (g *Gate) beginReleaseLocked(id, mode string, explicit bool) *ReleaseTicket {
	g.invalidateReleaseLocked()
	timeout, state := GracefulTimeout, "draining"
	if mode == "immediate" {
		timeout, state = ImmediateTimeout, "terminating"
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t := &ReleaseTicket{gate: g, ctx: ctx, cancel: cancel, state: state,
		operation: ReleaseOperation{ID: id, Mode: mode, PolicyRevision: g.state.Revision, StartedAt: time.Now().UTC()}}
	g.releaseCurrent = t
	if explicit {
		g.releaseIDs[id] = t
	}
	g.publishReleaseLocked(t)
	run := g.releaseRunner
	if run != nil {
		go run(t)
	} else {
		// Embedded gates without a manager cannot certify process ownership.
		now := time.Now().UTC()
		t.state, t.operation.ErrorCode, t.operation.EndedAt = "unreleased_unknown", "release_unavailable", &now
		g.publishReleaseLocked(t)
		t.cancel()
	}
	return t
}

func (g *Gate) publishReleaseLocked(t *ReleaseTicket) {
	op := t.operation
	g.state.Release, g.state.ReleaseOperation = t.state, &op
}

// RequestRelease validates CAS before looking up the boot-local idempotency key.
// A repeat returns the original operation in the ordinary snapshot envelope.
func (g *Gate) RequestRelease(boot string, revision uint64, id, mode string) (Snapshot, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if boot != g.state.BootID || revision != g.state.Revision || g.state.AllowAI {
		return g.snapshotLocked(), ErrConflict
	}
	if !ValidReleaseID(id) || (mode != "graceful" && mode != "immediate") {
		return g.snapshotLocked(), ErrReleaseRequest
	}
	if old := g.releaseIDs[id]; old != nil {
		if old.operation.PolicyRevision != revision || old.operation.Mode != mode {
			return g.snapshotLocked(), ErrConflict
		}
		s := g.snapshotLocked()
		op := old.operation
		s.Release, s.ReleaseOperation = old.state, &op
		return s, nil
	}
	if len(g.releaseIDs) >= ReleaseCapacity {
		return g.snapshotLocked(), ErrReleaseCapacity
	}
	// Graceful must not downgrade an active immediate operation.
	if old := g.releaseCurrent; old != nil && old.operation.Mode == "immediate" && old.operation.EndedAt == nil && mode == "graceful" {
		return g.snapshotLocked(), ErrConflict
	}
	g.beginReleaseLocked(id, mode, true)
	return g.snapshotLocked(), nil
}

func (t *ReleaseTicket) Context() context.Context { return t.ctx }
func (t *ReleaseTicket) Mode() string             { return t.operation.Mode }

// Effect serializes only the short OS side effect with resume and OS spawn.
// Validation and the syscall share this boundary; an epoch check followed by an
// unprotected asynchronous kill is deliberately not supported.
func (t *ReleaseTicket) Effect(f func() error) error {
	g := t.gate
	select {
	case g.effects <- struct{}{}:
	case <-t.ctx.Done():
		return t.ctx.Err()
	}
	defer func() { <-g.effects }()
	g.mu.Lock()
	valid := g.releaseCurrent == t && !g.state.AllowAI && t.ctx.Err() == nil
	g.mu.Unlock()
	if !valid {
		return context.Canceled
	}
	return f()
}

func (t *ReleaseTicket) CancelWork() error {
	return t.Effect(func() error {
		g := t.gate
		g.mu.Lock()
		defer g.mu.Unlock()
		for p := range g.permits {
			p.cancel()
		}
		return nil
	})
}

func (t *ReleaseTicket) Drained() bool { return t.gate.Active() == 0 }
func (t *ReleaseTicket) Terminating() {
	g := t.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.releaseCurrent == t && t.ctx.Err() == nil {
		t.state = "terminating"
		g.publishReleaseLocked(t)
	}
}
func (t *ReleaseTicket) Finish(state, code string) {
	g := t.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.releaseCurrent != t {
		return
	}
	if state == "released" && t.ctx.Err() != nil {
		state, code = "unreleased_unknown", "release_timeout"
	}
	now := time.Now().UTC()
	t.state, t.operation.ErrorCode, t.operation.EndedAt = state, code, &now
	g.publishReleaseLocked(t)
	t.cancel()
}

// BindContext connects an admitted call to owner-immediate cancellation without
// making a graceful pause cancel it. Call the returned cleanup at handler exit.
func (p *Permit) BindContext(parent context.Context) (context.Context, func()) {
	if p == nil {
		return parent, func() {}
	}
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(p.ctx, cancel)
	if p.ctx.Err() != nil {
		cancel()
	}
	return ctx, func() { stop(); cancel() }
}

// Spawn publishes a process generation under the same boundary as owner effects.
// Pause still latches while Start is in progress; the release runner cannot miss
// that generation. No policy mutex surrounds process creation.
func (p *Permit) Spawn(f func() error) error {
	if p == nil {
		return f()
	}
	g := p.gate
	select {
	case g.effects <- struct{}{}:
	case <-p.ctx.Done():
		return p.ctx.Err()
	}
	defer func() { <-g.effects }()
	if err := p.Check(); err != nil {
		return err
	}
	return f()
}
