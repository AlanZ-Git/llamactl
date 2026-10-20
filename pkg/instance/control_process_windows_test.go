//go:build windows

package instance

import (
	"context"
	"testing"
	"time"
)

// Reuses the existing owned-console fixture. No engine/model/GPU is involved.
// The control lease already owns the effect boundary: calling stopSignal (which
// acquires StopEffect on Windows) from it would deadlock even after ctx expires.
func TestP3WindowsControlStopUsesSingleEffectBoundary(t *testing.T) {
	f := b1OwnedConsoleFixture(t, "graceful")
	b1IgnoreEOF(t, f)
	b1AllowCompletion(t, f.dir)
	lease, err := f.inst.BeginWorkMaintenance()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := lease.StopControl(ctx); err != nil {
		t.Fatal(err)
	}
	if f.inst.IsRunning() {
		t.Fatal("control stop returned before exit")
	}
	b1AssertCompletions(t, f.dir)
	b1AssertHeldExit(t, f.held, true)
	tree := f.inst.process.tree
	tree.mu.Lock()
	forces, quit := tree.forceRequests, tree.quitSent
	tree.mu.Unlock()
	if forces != 0 || !quit {
		t.Fatalf("control stop forced or lacked graceful quit: %d %v", forces, quit)
	}
}
