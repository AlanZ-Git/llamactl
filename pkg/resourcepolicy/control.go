package resourcepolicy

import "context"

// AcquireCleanup binds a stop/unload to the current epoch, including a paused
// epoch. Check still denies loading; only CheckEpoch/CleanupEffect allow cleanup.
func (g *Gate) AcquireCleanup() *Permit {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	p := &Permit{gate: g, epoch: g.epoch, ctx: ctx, cancel: cancel, cleanup: true, cancelOnPause: true}
	g.permits[p] = struct{}{}
	g.active++
	return p
}
func (p *Permit) CheckEpoch() error {
	if p == nil {
		return nil
	}
	g := p.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	if p.closed || p.epoch != g.epoch {
		return ErrPaused
	}
	return nil
}
func (p *Permit) CleanupEffect(f func() error) error {
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
	if err := p.CheckEpoch(); err != nil {
		return err
	}
	return f()
}
