//go:build linux

package instance

import (
	"fmt"
	"os/exec"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

var subreaperOnce sync.Once

// Prevent a newly spawned manager child from acquiring a just-reaped PGID
// between Wait4 and the ECHILD probe. Unrelated processes are not waitable children.
var ownedGroups sync.Mutex
var subreaperErr error

// ownedTree is the sole waiter/signaller for one inherited process group. Making
// this manager a subreaper lets it wait for orphaned Router descendants too.
// This changes only the manager process, not system configuration. No process
// table or bare-PID enumeration is used.
type ownedTree struct {
	mu          sync.Mutex
	cmd         *exec.Cmd
	done        chan struct{}
	exited      bool
	rootReaped  bool // guarded by mu; a reaped root PID is never a metrics capability
	waitErr     error
	evidenceErr error
}

func startOwnedTree(cmd *exec.Cmd) (*ownedTree, error) {
	subreaperOnce.Do(func() { subreaperErr = unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0) })
	if subreaperErr != nil {
		return nil, fmt.Errorf("enable owned descendant waiting: %w", subreaperErr)
	}
	setProcAttrs(cmd)
	ownedGroups.Lock()
	defer ownedGroups.Unlock()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	t := &ownedTree{cmd: cmd, done: make(chan struct{})}
	go t.wait()
	return t, nil
}

func (t *ownedTree) signal(force bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.exited {
		return nil
	}
	if t.evidenceErr != nil {
		return t.evidenceErr
	}
	sig := unix.SIGINT
	if force {
		sig = unix.SIGKILL
	}
	// Between wait iterations a live or unreaped child pins the group identity.
	// Reaping the last member and marking exited share this mutex with the signal.
	// No signal can race PGID reuse after the last member has been reaped.
	return unix.Kill(-t.cmd.Process.Pid, sig)
}

func (t *ownedTree) wait() {
	for !t.poll() {
		time.Sleep(10 * time.Millisecond)
	}
}

func (t *ownedTree) poll() bool {
	ownedGroups.Lock()
	defer ownedGroups.Unlock()
	t.mu.Lock()
	defer t.mu.Unlock()
	var status unix.WaitStatus
	pid, err := unix.Wait4(-t.cmd.Process.Pid, &status, unix.WNOHANG, nil)
	if err == unix.EINTR {
		return false
	}
	if pid == t.cmd.Process.Pid {
		t.rootReaped = true
	}
	if pid == t.cmd.Process.Pid && (!status.Exited() || status.ExitStatus() != 0) {
		t.waitErr = fmt.Errorf("owned process exit status %d", status)
	}
	if err == nil && pid > 0 {
		// Probe without reaping again. Nonblocking kernel waits only; a remaining
		// live/unreaped group child pins the PGID until our next iteration.
		var info unix.Siginfo
		for {
			err = unix.Waitid(unix.P_PGID, t.cmd.Process.Pid, &info, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT, nil)
			if err != unix.EINTR {
				break
			}
		}
	}
	if err == unix.ECHILD {
		_ = t.cmd.Wait()            // Expected ECHILD; exec still owns pipe cleanup.
		_ = t.cmd.Process.Release() // Close its pidfd even when Wait returned ECHILD.
		t.exited = true
		close(t.done)
		return true
	}
	if err != nil {
		t.evidenceErr = err
		return true
	}
	return false
}

func (t *ownedTree) supportsRelease() bool { return true }
func (t *ownedTree) result() error         { t.mu.Lock(); defer t.mu.Unlock(); return t.waitErr }
