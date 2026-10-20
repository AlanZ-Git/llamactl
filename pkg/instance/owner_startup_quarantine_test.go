//go:build linux || windows

package instance

import (
	"errors"
	"os"
	"os/exec"
	"testing"
)

func TestOwnerFailedStartupRetainsLiveGeneration(t *testing.T) {
	inst, gate, _ := treeFixture(t, "stubborn")
	p := inst.process
	p.mu.RLock()
	old, monitor := p.tree, p.monitorDone
	p.mu.RUnlock()
	if err := old.signal(true); err != nil {
		t.Fatal(err)
	}
	waitTreeGone(t, old)
	waitTreeMonitor(t, monitor)

	dir := t.TempDir()
	backend := inst.globalBackendSettings.Custom["synthetic"]
	backend.Args = []string{"-test.run=^TestOwnerTreeHelper$", "--", "--owner-tree-fixture", "root", "stubborn", dir, "{port}"}
	inst.globalBackendSettings.Custom["synthetic"] = backend
	injected := errors.New("startup exit evidence unavailable")
	launches := 0
	p.launchTree = func(cmd *exec.Cmd) (*ownedTree, error) {
		launches++
		tree, err := startOwnedTree(cmd)
		if err != nil {
			return tree, err
		}
		// This is a genuinely live owned tree, not a status-only simulation.
		return tree, injected
	}
	if err := inst.Start(); !errors.Is(err, injected) {
		t.Fatal("startup failure was hidden", err)
	}
	p.mu.RLock()
	quarantine := p.tree
	p.mu.RUnlock()
	if quarantine == nil || quarantine == old {
		t.Fatal("failed startup dropped its live generation")
	}
	for _, role := range []string{"root", "child", "grandchild"} {
		treeAddress(t, dir, role)
	}
	if inst.IsRunning() {
		t.Fatal("failed startup published Running")
	}
	if _, err := p.stdin.(*os.File).Stat(); err == nil {
		t.Error("failed startup retained its stdin")
	}
	if err := inst.Start(); err == nil || launches != 1 {
		t.Fatal("replacement startup bypassed quarantine", err, launches)
	}
	ticket := releaseTicket(t, gate, "immediate")
	state, code := inst.ReleaseOwned(ticket)
	ticket.Finish(state, code)
	if state != "released" || code != "" {
		t.Fatal("owner could not stop its failed startup", state, code)
	}
	waitTreeGone(t, quarantine)
	for _, role := range []string{"root", "child", "grandchild"} {
		if treeReachable(treeAddress(t, dir, role)) {
			t.Error("failed startup descendant survived immediate", role)
		}
	}
}
