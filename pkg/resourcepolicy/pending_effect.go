package resourcepolicy

import "errors"

// ErrEffectPending contains no OS diagnostics or helper identity. An explicit
// resume can be retried with a fresh revision after dispatch completion is known.
var ErrEffectPending = errors.New("previous OS signal completion is unconfirmed")

// TrackOSSignal extends the effect boundary if a cross-process syscall cannot be
// proved completed or canceled within its bounded synchronous budget. Call only
// from Effect or StopEffect, before publishing the request. Close done only on
// cancellation-before-claim, actual syscall completion, or dispatcher exit.
// It does not cancel work, change admission, force a process, or reopen the gate.
func (g *Gate) TrackOSSignal(done <-chan struct{}) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pendingEffectsLocked()
	if g.pendingEffects == nil {
		g.pendingEffects = make(map[<-chan struct{}]struct{})
	}
	g.pendingEffects[done] = struct{}{}
}

func (g *Gate) pendingEffectsLocked() bool {
	for done := range g.pendingEffects {
		select {
		case <-done:
			delete(g.pendingEffects, done)
		default:
		}
	}
	return len(g.pendingEffects) != 0
}

// StopEffect orders an ordinary Stop's syscall with owner resume too. Stop has
// no release ticket, but must not introduce a new uncertain dispatch between a
// resume's pre-persistence check and final reopen. Reject that concurrent Stop
// effect while resume is in progress; pause and owner immediate remain usable.
func (g *Gate) StopEffect(f func() error) error {
	if g == nil {
		return f()
	}
	g.effects <- struct{}{}
	defer func() { <-g.effects }()
	g.mu.Lock()
	resuming := g.resuming != 0 && !g.state.AllowAI
	g.mu.Unlock()
	if resuming {
		return ErrPaused
	}
	return f()
}
