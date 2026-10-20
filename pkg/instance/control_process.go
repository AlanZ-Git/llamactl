package instance

import (
	"context"
	"fmt"
)

func (m *Maintenance) StartControl(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.CheckControl(ctx, true); err != nil {
		return err
	}
	if m.instance.IsRunning() {
		return nil
	}
	return m.instance.process.startPermitContext(ctx, m.permit)
}

// StopControl holds maintenance until confirmed exit or a terminal failure. It
// never forces, adopts a foreign process, or starts a detached late stop goroutine.
func (m *Maintenance) StopControl(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.CheckControl(ctx, false); err != nil {
		return err
	}
	i := m.instance
	p := i.process
	if p == nil {
		return fmt.Errorf("local process unavailable")
	}
	p.mu.Lock()
	tree, done, stdin := p.tree, p.monitorDone, p.stdin
	if tree == nil {
		running := i.IsRunning()
		p.mu.Unlock()
		if running {
			return controlError("process_not_owned")
		}
		return nil
	}
	select {
	case <-tree.done:
		p.mu.Unlock()
		if done != nil {
			select {
			case <-done:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	default:
	}
	i.SetStatus(ShuttingDown)
	p.mu.Unlock()
	if err := m.CheckControl(ctx, false); err != nil {
		return err
	}
	signal := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if stdin != nil {
			_ = stdin.Close()
		}
		// CleanupEffect already owns the owner signal boundary. stopSignal
		// would reacquire it on Windows; use the existing non-reentrant path
		// and carry the operation deadline through console dispatch instead.
		return tree.signalRelease(ctx, false, func(tree *ownedTree, force bool) error {
			return tree.signal(force)
		})
	}
	if m.permit != nil {
		if err := m.permit.CleanupEffect(signal); err != nil {
			return err
		}
	} else {
		if err := signal(); err != nil {
			return err
		}
	}
	select {
	case <-tree.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tree != tree {
		return controlError("process_generation_changed")
	}
	i.SetStatus(Stopped)
	return nil
}
