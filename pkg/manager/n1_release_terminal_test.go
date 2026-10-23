package manager

import (
	"context"
	"llamactl/pkg/instance"
	"testing"
	"time"
)

func TestReviewNormalReleaseMustNotBecomeDeadlineExceeded(t *testing.T) {
	im, n, l := n1WorkerFixture(t)
	n.ctx = context.Background()
	ticked := false
	im.control.stop = func(context.Context, *instance.Maintenance, *instance.Instance) error {
		// Deterministic real scheduler interleaving: release has cancelled the
		// reservation, cleanup still active, deadline remains in the future.
		ticked = true
		im.n1Tick(n)
		return nil
	}
	snap := l.inst.ControlSnapshot()
	revision := snap.Revision
	body := n1JSON(n1Envelope{Boot: snap.BootID, Revision: &revision, Key: "review-explicit-release"})
	status, _, err := im.n1Safety(context.Background(), n, l.inst, l.run, "release", body)
	if err != nil || status != 200 {
		t.Fatalf("release rejected: %d %v", status, err)
	}
	n.wg.Wait()
	result, err := n.store.Get(context.Background(), l.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ticked || !time.Now().Before(time.UnixMilli(result.Deadline)) {
		t.Fatal("invalid probe interleaving or real expiry")
	}
	if result.Cleanup != "released" || result.State != "succeeded" {
		t.Fatalf("explicit normal release mislabeled before deadline: state=%s cleanup=%s deadline_in_future=%v", result.State, result.Cleanup, time.Now().Before(time.UnixMilli(result.Deadline)))
	}
}
