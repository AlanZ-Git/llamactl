//go:build windows

package instance

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"llamactl/pkg/resourcepolicy"

	"golang.org/x/sys/windows"
)

var jobKernel = windows.NewLazySystemDLL("kernel32.dll")
var createJob = jobKernel.NewProc("CreateJobObjectW")
var setJob = jobKernel.NewProc("SetInformationJobObject")
var queryJob = jobKernel.NewProc("QueryInformationJobObject")
var terminateJob = jobKernel.NewProc("TerminateJobObject")

type ownedTree struct {
	mu             sync.Mutex
	job            windows.Handle
	backendJob     windows.Handle
	process        windows.Handle
	helper         windows.Handle
	console        *consoleIPC
	done           chan struct{}
	exited         bool
	waitErr        error
	evidenceErr    error
	startupErr     error
	quitSent       bool
	graceful       bool
	signalErr      error
	signalDone     chan struct{}
	signalDeadline time.Time
	policy         *resourcepolicy.Gate
	forceRequests  uint32
	lastExit       ownedExitEvidence
	beforeQuit     ownedExitEvidence
	// Immutable after startup; native tests inject dispatch/abort failures.
	dispatchTimeout time.Duration
	stopHelper      func(windows.Handle) error
	abortJob        func(windows.Handle) error
}

type ownedConsoleStart struct {
	jobAttribute    uintptr
	helperPath      string
	helperArgs      []string
	readyTimeout    time.Duration
	dispatchTimeout time.Duration
	stopHelper      func(windows.Handle) error
	abortJob        func(windows.Handle) error
	afterLaunch     func(*ownedTree)
}

func startOwnedTree(cmd *exec.Cmd) (*ownedTree, error) {
	return startOwnedTreeAttribute(cmd, 0x0002000d)
}

func startOwnedTreeAttribute(cmd *exec.Cmd, jobListAttribute uintptr) (*ownedTree, error) {
	return startOwnedConsole(cmd, ownedConsoleStart{jobAttribute: jobListAttribute})
}

// The helper is assigned to a non-breakaway supervisor Job during creation.
// Backend creation inherits that Job and explicitly adds a backend-only child
// Job. The supervisor Job handle never leaves the manager, including quarantine.
func startOwnedConsole(cmd *exec.Cmd, options ownedConsoleStart) (*ownedTree, error) {
	if cmd.Err != nil {
		return nil, cmd.Err
	}
	job, err := createOwnedJob()
	if err != nil {
		return nil, err
	}
	backendJob, err := createOwnedJob()
	if err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	retained := false
	defer func() {
		if !retained {
			windows.CloseHandle(backendJob)
			windows.CloseHandle(job)
		}
	}()
	ipc, err := newConsoleIPC(cmd)
	if err != nil {
		return nil, err
	}
	defer func() {
		if !retained {
			ipc.close()
		}
	}()
	// The helper receives only a temporary assign/query capability for the
	// backend Job, never the supervisor Job or a Job termination capability.
	const assignAndQueryJob = 0x0001 | 0x0004
	if err := windows.DuplicateHandle(windows.CurrentProcess(), backendJob, windows.CurrentProcess(), &ipc.backendJob, assignAndQueryJob, false, 0); err != nil {
		return nil, err
	}
	executable := options.helperPath
	if executable == "" {
		executable, err = os.Executable()
		if err != nil {
			return nil, err
		}
	}
	args := options.helperArgs
	if args == nil {
		args = []string{consoleHelperArg}
	}
	helperCmd := exec.Command(executable, args...)
	helperCmd.Stdin, helperCmd.Stdout, helperCmd.Stderr = cmd.Stdin, cmd.Stdout, cmd.Stderr
	helper, err := createConsoleProcess(helperCmd, job, options.jobAttribute, windows.CREATE_NEW_CONSOLE, ipc.handles(), func(handles []windows.Handle) []string {
		values := make([]string, len(handles))
		for n, handle := range handles {
			values[n] = strconv.FormatUint(uint64(handle), 10)
		}
		return append(withoutConsoleBootstrap(os.Environ()), consoleBootstrapEnv+"="+strings.Join(values, ","))
	})
	if err != nil {
		return nil, fmt.Errorf("create owned console: %w", err)
	}
	tree := &ownedTree{job: job, backendJob: backendJob, process: helper, helper: helper, console: ipc, done: make(chan struct{}),
		dispatchTimeout: options.dispatchTimeout, stopHelper: options.stopHelper, abortJob: options.abortJob}
	if tree.abortJob == nil {
		tree.abortJob = func(h windows.Handle) error { return windows.TerminateJobObject(h, 125) }
	}
	if tree.dispatchTimeout == 0 {
		tree.dispatchTimeout = 2 * time.Second
	}
	if tree.stopHelper == nil {
		tree.stopHelper = func(h windows.Handle) error { return windows.TerminateProcess(h, 125) }
	}
	retained = true
	if options.afterLaunch != nil {
		options.afterLaunch(tree)
	}
	budget := options.readyTimeout
	if budget == 0 {
		budget = 3 * time.Second
	}
	status, waitErr := windows.WaitForMultipleObjects([]windows.Handle{ipc.ready, helper}, false, uint32(budget.Milliseconds()))
	if waitErr != nil || status != windows.WAIT_OBJECT_0 || atomic.LoadUint32(&ipc.shared.Startup) != 1 {
		return tree, tree.abortStartup(fmt.Errorf("owned console startup was not confirmed"))
	}
	var root windows.Handle
	if err := windows.DuplicateHandle(helper, windows.Handle(ipc.shared.Root), windows.CurrentProcess(), &root, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
		return tree, tree.abortStartup(fmt.Errorf("owned backend handle transfer failed: %w", err))
	}
	tree.process = root
	for _, membership := range []struct {
		process windows.Handle
		job     windows.Handle
		want    bool
	}{{helper, job, true}, {helper, backendJob, false}, {root, job, true}, {root, backendJob, true}} {
		member, err := processInOwnedJob(membership.process, membership.job)
		if err != nil || member != membership.want {
			return tree, tree.abortStartup(fmt.Errorf("owned console/backend Job membership was not confirmed"))
		}
	}
	for _, stream := range []any{cmd.Stdin, cmd.Stdout, cmd.Stderr} {
		_ = stream.(*os.File).Close()
	}
	go tree.wait()
	return tree, nil
}

func (t *ownedTree) abortStartup(cause error) error {
	t.startupErr = cause
	// This is a failed, unpublished startup, not a graceful release escalation.
	// Keep the Job/tree if exit cannot be confirmed; process.start publishes that
	// quarantine even alongside an error, preventing a replacement spawn.
	for _, job := range []windows.Handle{t.job, t.backendJob} {
		if err := t.abortJob(job); err != nil {
			t.startupErr = fmt.Errorf("%w; startup termination failed: %v", t.startupErr, err)
		}
	}
	go t.wait()
	select {
	case <-t.done:
	case <-time.After(2 * time.Second):
	}
	return t.startupErr
}

func (t *ownedTree) bindReleasePolicy(g *resourcepolicy.Gate) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.policy = g // Bound before publication, never rebound to another generation.
}

// Called inside ticket.Effect; ordinary Stop uses the same gate's StopEffect.
// Carry the remaining owner deadline through the existing signal failure seam,
// rather than giving a late, already-drained release another three seconds.
func (t *ownedTree) signalRelease(ctx context.Context, force bool, send func(*ownedTree, bool) error) error {
	if force {
		return send(t, true)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	t.mu.Lock()
	previous := t.signalDeadline
	t.signalDeadline, _ = ctx.Deadline()
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		t.signalDeadline = previous
		t.mu.Unlock()
	}()
	return send(t, false)
}

func (t *ownedTree) signalWaitMilliseconds(budget time.Duration) uint32 {
	if !t.signalDeadline.IsZero() {
		if remaining := time.Until(t.signalDeadline); remaining < budget {
			budget = remaining
		}
	}
	if budget <= 0 {
		return 0
	}
	return uint32(budget.Milliseconds())
}

func (t *ownedTree) stopSignal() error {
	return t.policy.StopEffect(func() error { return t.signal(false) })
}

func (t *ownedTree) finishSignalLocked() {
	if t.signalDone != nil {
		close(t.signalDone)
		t.signalDone = nil
	}
}

func (t *ownedTree) signal(force bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.exited {
		return nil
	}
	if force {
		t.forceRequests++
		var firstErr error
		for _, job := range []windows.Handle{t.job, t.backendJob} {
			if ok, _, err := terminateJob.Call(uintptr(job), 1); ok == 0 && firstErr == nil {
				firstErr = fmt.Errorf("terminate owned Job: %w", err)
			}
		}
		return firstErr
	}
	if t.startupErr != nil {
		return t.startupErr
	}
	if t.graceful {
		return t.signalErr // Different operations and ordinary Stop share this latch.
	}
	if !t.signalDeadline.IsZero() && !time.Now().Before(t.signalDeadline) {
		return context.DeadlineExceeded
	}
	t.graceful = true
	if t.quitSent {
		return nil // Backend subtree exited; helper quit is already requested.
	}
	t.signalDone = make(chan struct{})
	t.policy.TrackOSSignal(t.signalDone)
	c := t.console
	atomic.StoreUint32(&c.shared.Command, consolePending)
	sendErr := windows.SetEvent(c.request)
	if sendErr == nil {
		_, _ = windows.WaitForMultipleObjects([]windows.Handle{c.ack, t.helper}, false, t.signalWaitMilliseconds(t.dispatchTimeout))
	}
	// Completion is the actual syscall return, not merely an IPC acknowledgement.
	if atomic.LoadUint32(&c.shared.Command) == consoleComplete {
		t.finishSignalLocked()
		if atomic.LoadUint32(&c.shared.Result) != 0 {
			t.signalErr = fmt.Errorf("owned console Ctrl-C request failed")
		}
		return t.signalErr
	}
	t.signalErr = fmt.Errorf("owned console Ctrl-C completion unconfirmed")
	if atomic.CompareAndSwapUint32(&c.shared.Command, consolePending, consoleCanceled) {
		t.finishSignalLocked() // Helper can no longer claim this request.
		return t.signalErr
	}
	// A claimed request cannot be revoked by closing IPC. Abort only the exact
	// dispatcher, never the backend or live Job, and require actual exit evidence.
	if status, err := windows.WaitForSingleObject(t.helper, 0); err != nil || status != windows.WAIT_OBJECT_0 {
		_ = t.stopHelper(t.helper)
		_, _ = windows.WaitForSingleObject(t.helper, t.signalWaitMilliseconds(time.Second))
	}
	if t.signalSettledLocked() {
		t.finishSignalLocked()
	} else {
		// Keep a logical effect guard, not the mutex/effects token. Resume will
		// fail closed until a real completion/exit observation closes this guard.
		go t.observeSignalCompletion()
	}
	return t.signalErr
}

func (t *ownedTree) signalSettledLocked() bool {
	if t.exited {
		return true
	}
	state := atomic.LoadUint32(&t.console.shared.Command)
	if state == consoleComplete || state == consoleCanceled {
		return true
	}
	status, err := windows.WaitForSingleObject(t.helper, 0)
	return err == nil && status == windows.WAIT_OBJECT_0
}

func (t *ownedTree) observeSignalCompletion() {
	for {
		t.mu.Lock()
		if t.signalDone == nil || t.signalSettledLocked() {
			t.finishSignalLocked()
			t.mu.Unlock()
			return
		}
		t.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
}

func (t *ownedTree) wait() {
	for {
		t.mu.Lock()
		evidence, err := t.observeExitLocked()
		t.lastExit, t.evidenceErr = evidence, err
		if err == nil && evidence.mayQuitHelper() && !t.quitSent {
			// The backend-only Job, not an assumed supervisor process count,
			// proves there are no live backend descendants left to interrupt.
			t.beforeQuit = evidence
			if err := windows.SetEvent(t.console.quit); err != nil {
				t.evidenceErr = err
			} else {
				t.quitSent = true
			}
		}
		if err == nil && evidence.complete() {
			// Accounting/list removal can precede a process object becoming
			// signaled. Neither STILL_ACTIVE nor ActiveProcesses==0 is an exit
			// proof for either held process handle.
			if evidence.Root.ExitCode != 0 {
				t.waitErr = fmt.Errorf("owned process exited with code %d", evidence.Root.ExitCode)
			} else if evidence.Helper.ExitCode != 0 {
				t.waitErr = fmt.Errorf("owned console helper exited with code %d", evidence.Helper.ExitCode)
			}
			t.exited = true
			t.finishSignalLocked()
			windows.CloseHandle(t.process)
			if t.helper != t.process {
				windows.CloseHandle(t.helper)
			}
			t.console.close()
			windows.CloseHandle(t.backendJob)
			windows.CloseHandle(t.job)
			close(t.done)
			t.mu.Unlock()
			return
		}
		t.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
}

func (t *ownedTree) supportsRelease() bool { return true }
func (t *ownedTree) result() error         { t.mu.Lock(); defer t.mu.Unlock(); return t.waitErr }
