//go:build !linux && !windows

package instance

import (
	"os/exec"
	"sync"
)

// Other platforms retain ordinary root-process lifecycle behavior. Owner release
// cannot certify a tree there, so it fails closed rather than claiming release.
type ownedTree struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	done    chan struct{}
	waitErr error
}

func startOwnedTree(cmd *exec.Cmd) (*ownedTree, error) {
	setProcAttrs(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	t := &ownedTree{cmd: cmd, done: make(chan struct{})}
	go func() { err := cmd.Wait(); t.mu.Lock(); t.waitErr = err; close(t.done); t.mu.Unlock() }()
	return t, nil
}
func (t *ownedTree) signal(force bool) error {
	if force {
		return t.cmd.Process.Kill()
	}
	signalStop(t.cmd)
	return nil
}
func (t *ownedTree) supportsRelease() bool { return false }
func (t *ownedTree) result() error         { t.mu.Lock(); defer t.mu.Unlock(); return t.waitErr }
