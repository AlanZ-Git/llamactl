//go:build windows

package instance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"llamactl/pkg/resourcepolicy"

	"golang.org/x/sys/windows"
)

func TestOwnerWindowsB1StartupFailures(t *testing.T)   { b1RunConsoleTest(t, "startup-failures") }
func TestOwnerWindowsB1StartupQuarantine(t *testing.T) { b1RunConsoleTest(t, "startup-quarantine") }
func TestOwnerWindowsB1PendingCancel(t *testing.T)     { b1RunConsoleTest(t, "pending-cancel") }
func TestOwnerWindowsB1ClaimedAbort(t *testing.T)      { b1RunConsoleTest(t, "claimed-abort") }
func TestOwnerWindowsB1ClaimedUncertain(t *testing.T)  { b1RunConsoleTest(t, "claimed-uncertain") }
func TestOwnerWindowsB1EffectResume(t *testing.T)      { b1RunConsoleTest(t, "effect-resume") }
func TestOwnerWindowsB1Immediate(t *testing.T)         { b1RunConsoleTest(t, "console-immediate") }
func TestOwnerWindowsB1OrdinaryStop(t *testing.T)      { b1RunConsoleTest(t, "ordinary-stop") }
func TestOwnerWindowsB1InheritedControl(t *testing.T)  { b1RunConsoleTest(t, "inherited-control") }
func TestOwnerWindowsB1DispatchError(t *testing.T)     { b1RunConsoleTest(t, "dispatch-error") }
func TestOwnerWindowsB1DispatchBudget(t *testing.T)    { b1RunConsoleTest(t, "dispatch-budget") }

func b1CheckInheritedControl(t *testing.T) {
	f := b1OwnedConsoleFixture(t, "inheritance")
	b1IgnoreEOF(t, f)
	b1AllowCompletion(t, f.dir)
	result := b1AwaitRelease(t, b1Release(f, b1Pause(t, f)))
	if result.state != "released" || result.code != "" {
		t.Error("Ctrl-C inheritance fixture did not complete owned-tree release", result)
	}
	b1AssertReleasedTree(t, f)
}

func b1ProbeArgs(mode, dir string) []string {
	return []string{"-test.run=^TestOwnerWindowsConsoleProbe$", "--", "--owner-console-probe", mode, dir}
}

// Launched only through the real owned-console startup. It uses the production
// bootstrap, backend creation, handler and dispatch state machine; hooks delay
// real claim/syscall phases rather than sending a fake acknowledgement.
func TestOwnerWindowsConsoleProbe(t *testing.T) {
	args := b1HelperArgs("--owner-console-probe")
	if args == nil {
		return
	}
	b1HardDeadline()
	if len(args) != 2 {
		t.Fatal("invalid console probe arguments")
	}
	ipc, err := openConsoleIPC()
	if err != nil {
		t.Fatal(err)
	}
	barrier := func() {
		if err := os.WriteFile(filepath.Join(args[1], "entered"), []byte("entered"), 0600); err != nil {
			os.Exit(93)
		}
		for {
			if _, err := os.Stat(filepath.Join(args[1], "proceed")); err == nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		_ = os.WriteFile(filepath.Join(args[1], "left"), []byte("left"), 0600)
	}
	var before, after func()
	switch args[0] {
	case "no-ready":
		barrier()
		ipc.close()
		return
	case "no-console":
		// This probe already has its own console. Never detach the test manager.
		if ok, _, err := jobKernel.NewProc("FreeConsole").Call(); ok == 0 {
			t.Fatal(err)
		}
	case "backend-job":
		if err := windows.CloseHandle(ipc.backendJob); err != nil {
			t.Fatal(err)
		}
		ipc.backendJob = windows.InvalidHandle // Real creation-time association failure.
	case "pending-cancel":
		before = barrier
	case "claimed-abort", "claimed-uncertain", "effect-resume", "dispatch-budget":
		after = barrier
	case "dispatch-error":
		after = func() {
			// Lose only this probe's private console after readiness. The real
			// GenerateConsoleCtrlEvent call must now fail, without killing backend.
			if ok, _, _ := jobKernel.NewProc("FreeConsole").Call(); ok == 0 {
				os.Exit(93)
			}
		}
	default:
		t.Fatal("unknown console probe")
	}
	code := runConsoleHelper(ipc, before, after)
	if ipc.backendJob == windows.InvalidHandle {
		ipc.backendJob = 0 // The fault sentinel is not an owned handle to close.
	}
	ipc.close()
	os.Exit(int(code))
}

func b1AwaitCondition(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal(description)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func b1AwaitFile(t *testing.T, dir, name string) {
	t.Helper()
	b1AwaitCondition(t, "probe did not reach "+name, func() bool {
		_, err := os.Stat(filepath.Join(dir, name))
		return err == nil
	})
}

func b1Proceed(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "proceed"), []byte("proceed"), 0600); err != nil {
		t.Fatal(err)
	}
}

func b1CheckConsoleStartupFailures(t *testing.T) {
	t.Run("missing-capabilities", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], consoleHelperArg)
		cmd.Env = withoutConsoleBootstrap(os.Environ())
		output, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 125 || len(output) != 0 {
			t.Fatalf("helper marker without capabilities did not fail before main: %v %q", err, output)
		}
	})
	for _, mode := range []string{"job", "helper-create", "helper-arguments", "no-console", "no-ready", "backend-create", "backend-job"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			cmd := b1FixtureCommand("root", "stubborn", dir, "0")
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			stderr, err := cmd.StderrPipe()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				for _, c := range []io.Closer{stdin, stdout, stderr, cmd.Stdin.(io.Closer), cmd.Stdout.(io.Closer), cmd.Stderr.(io.Closer)} {
					_ = c.Close()
				}
			})
			options := ownedConsoleStart{jobAttribute: 0x0002000d, readyTimeout: time.Second}
			switch mode {
			case "job":
				options.jobAttribute = ^uintptr(0)
			case "helper-create":
				options.helperPath = filepath.Join(dir, "missing-helper.exe")
			case "helper-arguments":
				options.helperArgs = []string{consoleHelperArg, "unexpected"}
			case "no-console", "no-ready", "backend-job":
				options.helperArgs = b1ProbeArgs(mode, dir)
			case "backend-create":
				cmd.Path = filepath.Join(dir, "missing-backend.exe")
			}
			var heldJob, heldBackend, heldHelper windows.Handle
			options.afterLaunch = func(tree *ownedTree) {
				if err := windows.DuplicateHandle(windows.CurrentProcess(), tree.job, windows.CurrentProcess(), &heldJob, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
					t.Fatal(err)
				}
				if err := windows.DuplicateHandle(windows.CurrentProcess(), tree.backendJob, windows.CurrentProcess(), &heldBackend, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
					t.Fatal(err)
				}
				if err := windows.DuplicateHandle(windows.CurrentProcess(), tree.helper, windows.CurrentProcess(), &heldHelper, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
					t.Fatal(err)
				}
				b1LogOwnedExit(t, tree, "startup helper created")
				b1LogJobMembers(t, "startup supervisor", heldJob)
				b1LogJobMembers(t, "startup backend", heldBackend)
			}
			t.Cleanup(func() {
				if heldJob != 0 {
					_ = windows.TerminateJobObject(heldJob, 99)
					if err := b1JobEmpty(heldJob, 5*time.Second); err != nil {
						t.Error(err)
					}
					windows.CloseHandle(heldJob)
				}
				if heldBackend != 0 {
					_ = windows.TerminateJobObject(heldBackend, 99)
					if err := b1JobEmpty(heldBackend, 5*time.Second); err != nil {
						t.Error(err)
					}
					windows.CloseHandle(heldBackend)
				}
				if heldHelper != 0 {
					windows.CloseHandle(heldHelper)
				}
			})
			tree, err := startOwnedConsole(cmd, options)
			if err == nil {
				t.Fatal("startup failure accepted")
			}
			if tree != nil {
				b1LogOwnedExit(t, tree, "startup returned error before independent wait")
				waitTreeGone(t, tree)
				b1LogOwnedExit(t, tree, "startup done before independent held-handle check")
			}
			if heldJob != 0 {
				if err := b1JobEmpty(heldJob, time.Second); err != nil {
					t.Fatal(err)
				}
				if err := b1JobEmpty(heldBackend, time.Second); err != nil {
					t.Fatal(err)
				}
				helperEvidence, observationErr := observeOwnedProcess(heldHelper)
				t.Logf("independent startup helper proof: %+v error=%v", helperEvidence, observationErr)
				status, err := windows.WaitForSingleObject(heldHelper, 0)
				if err != nil || status != windows.WAIT_OBJECT_0 {
					t.Fatal("startup helper exit unproved", status, err)
				}
			}
			if _, err := os.Stat(filepath.Join(dir, "root.addr")); !os.IsNotExist(err) {
				t.Fatal("backend ran after failed startup/ownership", err)
			}
		})
	}
}

func b1CheckStartupQuarantine(t *testing.T) {
	f := b1OwnedConsoleFixture(t, "stubborn")
	p := f.inst.process
	p.mu.RLock()
	old, monitor := p.tree, p.monitorDone
	p.mu.RUnlock()
	if err := old.signal(true); err != nil {
		t.Fatal(err)
	}
	waitTreeGone(t, old)
	waitTreeMonitor(t, monitor)
	probeDir := t.TempDir()
	p.launchTree = func(cmd *exec.Cmd) (*ownedTree, error) {
		return startOwnedConsole(cmd, ownedConsoleStart{jobAttribute: 0x0002000d,
			helperArgs: b1ProbeArgs("no-ready", probeDir), readyTimeout: time.Second,
			abortJob: func(windows.Handle) error { return fmt.Errorf("injected startup termination failure") }})
	}
	if err := f.inst.Start(); err == nil {
		t.Fatal("unconfirmed startup reported running")
	}
	p.mu.RLock()
	quarantine := p.tree
	p.mu.RUnlock()
	if quarantine == nil || quarantine == old {
		t.Fatal("failed live startup lost ownership")
	}
	select {
	case <-quarantine.done:
		t.Fatal("quarantine fixture unexpectedly exited")
	default:
	}
	if err := f.inst.Start(); err == nil {
		t.Fatal("started over a live failed startup")
	}
	if f.inst.IsRunning() {
		t.Fatal("failed startup published Running")
	}
	if _, err := p.stdin.(*os.File).Stat(); err == nil {
		t.Error("startup stdin leaked")
	}
	b1Pause(t, f)
	s := f.gate.Snapshot()
	if _, err := f.gate.RequestRelease(s.BootID, s.Revision, "quarantine-immediate", "immediate"); err != nil {
		t.Fatal(err)
	}
	result := b1AwaitRelease(t, b1Release(f, b1NextTicket(t, f)))
	if result.state != "released" {
		t.Fatal(result)
	}
	waitTreeGone(t, quarantine)
}

func b1CheckDispatchBoundary(t *testing.T, mode string) {
	probeDir := t.TempDir()
	f := b1OwnedConsoleFixture(t, "stubborn", func(p *process) {
		p.launchTree = func(cmd *exec.Cmd) (*ownedTree, error) {
			options := ownedConsoleStart{jobAttribute: 0x0002000d, helperArgs: b1ProbeArgs(mode, probeDir), dispatchTimeout: 250 * time.Millisecond}
			if mode == "effect-resume" {
				options.dispatchTimeout = 3 * time.Second
			}
			if mode == "claimed-uncertain" {
				options.stopHelper = func(windows.Handle) error { return fmt.Errorf("injected helper termination failure") }
			}
			return startOwnedConsole(cmd, options)
		}
	})
	b1IgnoreEOF(t, f)
	ticket := b1Pause(t, f)
	release := b1Release(f, ticket)
	b1AwaitFile(t, probeDir, "entered")
	tree := f.inst.process.tree
	if mode == "effect-resume" {
		s := f.gate.Snapshot()
		resumeDone := make(chan error, 1)
		go func() { _, err := f.gate.Update(true, s.BootID, s.Revision); resumeDone <- err }()
		b1AwaitCondition(t, "resume did not latch", func() bool { return f.gate.Snapshot().Revision != s.Revision })
		if f.gate.Snapshot().AllowAI {
			t.Fatal("resume opened ahead of actual console syscall")
		}
		pending := f.gate.Snapshot()
		pauseDone := make(chan error, 1)
		go func() { _, err := f.gate.Update(false, pending.BootID, pending.Revision); pauseDone <- err }()
		b1NextTicket(t, f) // New pause latches even while old syscall is held.
		b1Proceed(t, probeDir)
		b1AwaitRelease(t, release)
		select {
		case err := <-resumeDone:
			if !errors.Is(err, resourcepolicy.ErrConflict) {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("resume remained stuck")
		}
		select {
		case err := <-pauseDone:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("new pause remained stuck")
		}
		if f.gate.Snapshot().AllowAI {
			t.Fatal("old resume defeated new pause")
		}
		for _, role := range b1Roles {
			b1AwaitStatus(t, f.dir, role, b1OneControl)
		}
		return
	}
	result := b1AwaitRelease(t, release)
	if result.state != "failed" || result.code != "termination_failed" {
		t.Fatal("IPC failure manufactured success", result)
	}
	if mode == "claimed-uncertain" {
		tree.mu.Lock()
		pending := tree.signalDone
		tree.mu.Unlock()
		if pending == nil {
			t.Fatal("claimed request lost its effect guard")
		}
		s := f.gate.Snapshot()
		s, err := f.gate.Update(true, s.BootID, s.Revision)
		if !errors.Is(err, resourcepolicy.ErrEffectPending) || s.AllowAI || s.Durable {
			t.Fatal(s, err)
		}
		s, err = f.gate.Update(false, s.BootID, s.Revision)
		if err != nil {
			t.Fatal(err)
		}
		b1NextTicket(t, f)
		if _, err := f.gate.RequestRelease(s.BootID, s.Revision, "uncertain-immediate", "immediate"); err != nil {
			t.Fatal(err)
		}
		if err := b1NextTicket(t, f).CancelWork(); err != nil {
			t.Fatal("guard blocked immediate effect", err)
		}
		b1Proceed(t, probeDir)
		for _, role := range b1Roles {
			b1AwaitStatus(t, f.dir, role, b1OneControl)
		}
		select {
		case <-pending:
		case <-time.After(5 * time.Second):
			t.Fatal("actual syscall completion did not retire guard")
		}
		if f.gate.Snapshot().AllowAI {
			t.Fatal("guard completion automatically resumed")
		}
		s = f.gate.Snapshot()
		if next, err := f.gate.Update(true, s.BootID, s.Revision); err != nil || !next.AllowAI {
			t.Fatal(next, err)
		}
	} else {
		s := f.gate.Snapshot()
		if next, err := f.gate.Update(true, s.BootID, s.Revision); err != nil || !next.AllowAI {
			t.Fatal(next, err)
		}
		if mode == "pending-cancel" {
			b1Proceed(t, probeDir) // Attempted dispatch after resume must be canceled.
			b1AwaitFile(t, probeDir, "left")
			tree.mu.Lock()
			state := atomic.LoadUint32(&tree.console.shared.Command)
			tree.mu.Unlock()
			if state != consoleCanceled {
				t.Fatal("canceled request was claimed", state)
			}
		} else {
			status, err := windows.WaitForSingleObject(tree.helper, 0)
			if err != nil || status != windows.WAIT_OBJECT_0 {
				t.Fatal("dispatcher abort lacked exit evidence", status, err)
			}
		}
		for deadline := time.Now().Add(200 * time.Millisecond); time.Now().Before(deadline); {
			for _, role := range b1Roles {
				e, err := b1ReadStatus(treeAddress(t, f.dir, role))
				if err != nil || !b1NoControl(e) || !e.EOF {
					t.Fatalf("late control or backend force: %s %+v %v", role, e, err)
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	select {
	case <-tree.done:
		t.Fatal("IPC failure automatically forced backend")
	default:
	}
	if f.forced.Load() != 0 {
		t.Fatal("IPC failure escalated to backend force")
	}
}

func b1CheckConsoleDispatchBudget(t *testing.T) {
	probeDir := t.TempDir()
	f := b1OwnedConsoleFixture(t, "stubborn", func(p *process) {
		p.launchTree = func(cmd *exec.Cmd) (*ownedTree, error) {
			return startOwnedConsole(cmd, ownedConsoleStart{jobAttribute: 0x0002000d,
				helperArgs: b1ProbeArgs("dispatch-budget", probeDir),
				stopHelper: func(windows.Handle) error { return fmt.Errorf("injected helper termination failure") }})
		}
	})
	b1IgnoreEOF(t, f)
	ticket := b1Pause(t, f)
	tree := f.inst.process.tree
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	started := time.Now()
	done := make(chan error, 1)
	go func() {
		done <- ticket.Effect(func() error {
			return tree.signalRelease(ctx, false, f.inst.process.signalTree)
		})
	}()
	b1AwaitFile(t, probeDir, "entered")
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("held dispatch incorrectly completed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch ignored the remaining owner budget")
	}
	if time.Since(started) > 900*time.Millisecond {
		t.Fatal("late release received a fresh dispatch/abort wait budget")
	}
	tree.mu.Lock()
	pending := tree.signalDone
	tree.mu.Unlock()
	if pending == nil {
		t.Fatal("deadline dropped an unconfirmed claimed request")
	}
	s := f.gate.Snapshot()
	if next, err := f.gate.Update(true, s.BootID, s.Revision); !errors.Is(err, resourcepolicy.ErrEffectPending) || next.AllowAI {
		t.Fatal("deadline reopened over a pending syscall", next, err)
	}
	b1Proceed(t, probeDir)
	select {
	case <-pending:
	case <-time.After(5 * time.Second):
		t.Fatal("completion did not retire deadline guard")
	}
	for _, role := range b1Roles {
		b1AwaitStatus(t, f.dir, role, b1OneControl)
	}
	if f.forced.Load() != 0 {
		t.Fatal("deadline forced the backend")
	}
}

func b1CheckConsoleDispatchError(t *testing.T) {
	probeDir := t.TempDir()
	f := b1OwnedConsoleFixture(t, "stubborn", func(p *process) {
		p.launchTree = func(cmd *exec.Cmd) (*ownedTree, error) {
			return startOwnedConsole(cmd, ownedConsoleStart{jobAttribute: 0x0002000d, helperArgs: b1ProbeArgs("dispatch-error", probeDir)})
		}
	})
	b1IgnoreEOF(t, f)
	result := b1AwaitRelease(t, b1Release(f, b1Pause(t, f)))
	if result.state != "failed" || result.code != "termination_failed" {
		t.Fatal("actual Ctrl-C API failure was hidden", result)
	}
	tree := f.inst.process.tree
	tree.mu.Lock()
	state, code := atomic.LoadUint32(&tree.console.shared.Command), atomic.LoadUint32(&tree.console.shared.Result)
	tree.mu.Unlock()
	if state != consoleComplete || code == 0 {
		t.Fatal("missing completed syscall failure evidence", state, code)
	}
	for _, role := range b1Roles {
		e, err := b1ReadStatus(treeAddress(t, f.dir, role))
		if err != nil || !b1NoControl(e) || !e.EOF {
			t.Fatalf("dispatch failure stopped backend: %s %+v %v", role, e, err)
		}
	}
	s := f.gate.Snapshot()
	if next, err := f.gate.Update(true, s.BootID, s.Revision); err != nil || !next.AllowAI {
		t.Fatal("completed failed syscall retained a pending guard", next, err)
	}
	if f.forced.Load() != 0 {
		t.Fatal("dispatch error escalated to force")
	}
}

func b1CheckConsoleImmediate(t *testing.T) {
	f := b1OwnedConsoleFixture(t, "stubborn")
	b1Pause(t, f)
	s := f.gate.Snapshot()
	if _, err := f.gate.RequestRelease(s.BootID, s.Revision, "console-immediate", "immediate"); err != nil {
		t.Fatal(err)
	}
	result := b1AwaitRelease(t, b1Release(f, b1NextTicket(t, f)))
	if result.state != "released" || result.code != "" {
		t.Fatal(result)
	}
	waitTreeGone(t, f.inst.process.tree)
	b1AssertHeldExit(t, f.held, false)
	b1LogOwnedExit(t, f.inst.process.tree, "immediate assertions")
	for _, role := range b1Roles {
		if treeReachable(treeAddress(t, f.dir, role)) {
			t.Error("immediate left a backend descendant", role)
		}
	}
	if f.forced.Load() != 1 {
		t.Fatal("immediate did not use the owned Job")
	}
}

func b1CheckOrdinaryStopOnce(t *testing.T) {
	f := b1OwnedConsoleFixture(t, "graceful")
	b1IgnoreEOF(t, f)
	release := b1Release(f, b1Pause(t, f))
	for _, role := range b1Roles {
		b1AwaitStatus(t, f.dir, role, b1OneControl)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- f.inst.Stop() }()
	b1AwaitCondition(t, "ordinary Stop did not reach its signal phase", func() bool { return f.inst.GetStatus() == Stopped })
	for deadline := time.Now().Add(200 * time.Millisecond); time.Now().Before(deadline); {
		for _, role := range b1Roles {
			e, err := b1ReadStatus(treeAddress(t, f.dir, role))
			if err != nil || !b1OneControl(e) {
				t.Fatalf("ordinary Stop repeated Ctrl-C: %s %+v %v", role, e, err)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	b1AllowCompletion(t, f.dir)
	if result := b1AwaitRelease(t, release); result.state != "released" {
		t.Fatal(result)
	}
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ordinary Stop did not complete")
	}
	b1AssertReleasedTree(t, f)
}
