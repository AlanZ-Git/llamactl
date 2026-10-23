package manager

import (
	"context"
	"llamactl/pkg/instance"
)

// Recovery is the existing administrator-verified stopped/tree/port/preset path,
// not a new permission to resume an interrupted run or an owner-paused process.
func (im *instanceManager) recoverN1(ctx context.Context, inst *instance.Instance) (bool, error) {
	n := im.initN1()
	if n.err != nil {
		return false, nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	l := n.active
	if l == nil || l.inst != inst {
		return false, nil
	}
	if !l.blocked {
		return true, instance.BusyError()
	}
	run, err := n.store.Get(ctx, l.run.ID)
	if err != nil {
		return true, err
	}
	if err = l.reservation.VerifyStoppedRecovery(ctx); err != nil {
		return true, err
	}
	if _, err = n.store.Append(ctx, run.ID, "recovery", "", n1JSON(map[string]any{"verified_stopped": true, "boot_id": inst.ControlSnapshot().BootID, "original_run_id": run.ID, "prior_cleanup_state": run.Cleanup})); err != nil {
		return true, err
	}
	if run.FrozenAt == nil {
		if err = n.store.Freeze(ctx, run.ID, "interrupted", "released", n1JSON(map[string]any{"recovery_verified_stopped": true, "quality": "unknown"})); err != nil {
			return true, err
		}
	}
	if err = l.reservation.CloseConfirmed(); err != nil {
		return true, err
	}
	n.active = nil
	return true, nil
}
