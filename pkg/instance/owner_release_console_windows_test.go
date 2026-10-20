//go:build windows

package instance

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"llamactl/pkg/backends"
	"llamactl/pkg/config"
	"llamactl/pkg/resourcepolicy"
)

func TestOwnerWindowsB1Graceful(t *testing.T)       { b1RunConsoleTest(t, "graceful") }
func TestOwnerWindowsB1Stubborn(t *testing.T)       { b1RunConsoleTest(t, "stubborn") }
func TestOwnerWindowsB1Repeated(t *testing.T)       { b1RunConsoleTest(t, "repeated") }
func TestOwnerWindowsB1Stale(t *testing.T)          { b1RunConsoleTest(t, "stale") }
func TestOwnerWindowsB1FixtureControl(t *testing.T) { b1RunConsoleTest(t, "fixture-control") }

// The outer test creates this subprocess in an isolated console before any Go
// code runs. No test attaches to a PID or changes the invoking process's console.
func TestOwnerWindowsB1Harness(t *testing.T) {
	args := b1HelperArgs(b1HarnessMarker)
	if args == nil {
		return
	}
	b1HardDeadline()
	if len(args) != 2 {
		t.Fatal("invalid console harness arguments")
	}
	counts := b1InstallControlHandler(t)
	if args[0] == "fixture-control" {
		b1CheckFixtureControl(t, counts)
		return
	}
	sentinelDir := t.TempDir()
	sentinel := b1FixtureCommand("sentinel", "stubborn", sentinelDir, "0")
	sentinel.Stdin, sentinel.Stdout, sentinel.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := sentinel.Start(); err != nil {
		t.Fatal(err)
	}
	sentinelDone := make(chan error, 1)
	go func() { sentinelDone <- sentinel.Wait() }()
	t.Cleanup(func() {
		_ = sentinel.Process.Kill() // Only the exact unrelated test sentinel.
		select {
		case <-sentinelDone:
		case <-time.After(5 * time.Second):
			t.Error("sentinel teardown did not complete")
		}
	})
	b1AwaitStatus(t, sentinelDir, "sentinel", b1NoControl)
	sentinelAddr := treeAddress(t, sentinelDir, "sentinel")
	// Defer runs before test cleanup (including when a case calls Fatal), so
	// backend/sentinel force teardown cannot supply graceful success evidence.
	defer func() {
		for deadline := time.Now().Add(200 * time.Millisecond); time.Now().Before(deadline); {
			evidence, err := b1ReadStatus(sentinelAddr)
			if err != nil || !b1NoControl(evidence) {
				t.Errorf("unrelated manager-console sentinel affected: %+v error=%v", evidence, err)
				break
			}
			if evidence := counts.evidence(false); !b1NoControl(evidence) {
				t.Errorf("backend shutdown reached manager console: %+v", evidence)
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	switch args[0] {
	case "graceful":
		b1CheckGraceful(t)
	case "stubborn":
		b1CheckStubborn(t)
	case "repeated":
		b1CheckRepeated(t)
	case "stale":
		b1CheckStale(t)
	case "startup-failures":
		b1CheckConsoleStartupFailures(t)
	case "startup-quarantine":
		b1CheckStartupQuarantine(t)
	case "pending-cancel", "claimed-abort", "claimed-uncertain", "effect-resume":
		b1CheckDispatchBoundary(t, args[0])
	case "console-immediate":
		b1CheckConsoleImmediate(t)
	case "ordinary-stop":
		b1CheckOrdinaryStopOnce(t)
	case "inherited-control":
		b1CheckInheritedControl(t)
	case "dispatch-error":
		b1CheckConsoleDispatchError(t)
	case "dispatch-budget":
		b1CheckConsoleDispatchBudget(t)
	case "helper-descendants":
		b1CheckHelperWaitsForDescendants(t)
	default:
		t.Fatal("unknown console harness case")
	}
}

func b1NoControl(e b1ConsoleEvidence) bool {
	return e.CtrlC == 0 && e.CtrlBreak == 0 && e.Other == 0
}

func b1OneControl(e b1ConsoleEvidence) bool {
	return e.CtrlC == 1 && e.CtrlBreak == 0 && e.Other == 0
}

type b1SignalAttempt struct {
	force bool
	err   error
}

type b1OwnedFixture struct {
	inst    *Instance
	gate    *resourcepolicy.Gate
	dir     string
	tickets chan *resourcepolicy.ReleaseTicket
	signals chan b1SignalAttempt
	forced  atomic.Uint32
	held    *b1HeldProcesses
}

func b1UnusedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func b1Backend(dir, behavior string) config.BackendSettings {
	cmd := b1FixtureCommand("root", behavior, dir, "{port}")
	return config.BackendSettings{Command: cmd.Path, Args: cmd.Args[1:]}
}

func b1OwnedConsoleFixture(t *testing.T, behavior string, configure ...func(*process)) *b1OwnedFixture {
	t.Helper()
	g, err := resourcepolicy.Open("")
	if err != nil {
		t.Fatal(err)
	}
	f := &b1OwnedFixture{gate: g, dir: t.TempDir(), tickets: make(chan *resourcepolicy.ReleaseTicket, 8), signals: make(chan b1SignalAttempt, 8)}
	g.SetReleaseRunner(func(ticket *resourcepolicy.ReleaseTicket) { f.tickets <- ticket })
	cfg := &config.AppConfig{ResourcePolicy: g}
	cfg.Instances.InstancesDir = t.TempDir()
	cfg.Instances.LogsDir = t.TempDir()
	cfg.Backends.Custom = map[string]config.BackendSettings{"console-fixture": b1Backend(f.dir, behavior)}
	autoRestart := false
	opts := &Options{AutoRestart: &autoRestart, BackendOptions: backends.Options{
		BackendType:         backends.BackendTypeCustom,
		CustomServerOptions: &backends.CustomServerOptions{Name: "console-fixture", Host: "127.0.0.1", Port: b1UnusedPort(t)},
	}}
	f.inst, err = NewChecked("console-owned", cfg, opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Observe, never replace, the real product signal. Configure before Start.
	f.inst.process.signalTree = func(tree *ownedTree, force bool) error {
		if force {
			f.forced.Add(1)
		}
		err := tree.signal(force)
		f.signals <- b1SignalAttempt{force: force, err: err}
		return err
	}
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		entries, err := os.ReadDir(cfg.Instances.LogsDir)
		if err != nil {
			t.Logf("fixture log directory: %v", err)
			return
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				if data, err := os.ReadFile(filepath.Join(cfg.Instances.LogsDir, entry.Name())); err == nil {
					t.Logf("synthetic backend log %s:\n%s", entry.Name(), data)
				}
			}
		}
	})
	t.Cleanup(func() {
		// Cancel a pending test worker, then explicitly clean only the captured
		// owned generation. This is not part of any graceful assertion.
		s := g.Snapshot()
		if _, err := g.Update(true, s.BootID, s.Revision); err != nil {
			t.Errorf("cancel fixture release: %v", err)
		}
		p := f.inst.process
		p.mu.RLock()
		tree, monitor := p.tree, p.monitorDone
		p.mu.RUnlock()
		if tree != nil {
			b1LogOwnedExit(t, tree, "before explicit fixture teardown")
			select {
			case <-tree.done:
			default:
				t.Log("explicit fixture teardown after release assertions")
				if err := tree.signal(true); err != nil {
					t.Error(err)
				}
			}
			waitTreeGone(t, tree)
		}
		waitTreeMonitor(t, monitor)
	})
	for _, setup := range configure {
		setup(f.inst.process)
	}
	if err := f.inst.Start(); err != nil {
		t.Fatal(err)
	}
	for _, role := range b1Roles {
		b1AwaitStatus(t, f.dir, role, func(e b1ConsoleEvidence) bool { return b1NoControl(e) && !e.EOF })
	}
	f.held = b1HoldFixtureProcesses(t, f)
	b1LogOwnedExit(t, f.inst.process.tree, "fixture ready")
	return f
}

func b1NextTicket(t *testing.T, f *b1OwnedFixture) *resourcepolicy.ReleaseTicket {
	t.Helper()
	select {
	case ticket := <-f.tickets:
		return ticket
	case <-time.After(2 * time.Second):
		t.Fatal("release runner did not receive ticket")
		return nil
	}
}

func b1Pause(t *testing.T, f *b1OwnedFixture) *resourcepolicy.ReleaseTicket {
	t.Helper()
	s := f.gate.Snapshot()
	if _, err := f.gate.Update(false, s.BootID, s.Revision); err != nil {
		t.Fatal(err)
	}
	return b1NextTicket(t, f)
}

type b1ReleaseResult struct{ state, code string }

func b1Release(f *b1OwnedFixture, ticket *resourcepolicy.ReleaseTicket) <-chan b1ReleaseResult {
	done := make(chan b1ReleaseResult, 1)
	go func() {
		state, code := f.inst.ReleaseOwned(ticket)
		ticket.Finish(state, code)
		done <- b1ReleaseResult{state, code}
	}()
	return done
}

func b1AwaitRelease(t *testing.T, done <-chan b1ReleaseResult) b1ReleaseResult {
	t.Helper()
	select {
	case result := <-done:
		t.Logf("owner release result: %s/%s", result.state, result.code)
		return result
	case <-time.After(35 * time.Second):
		t.Fatal("owner release outlived its real 30s budget")
		return b1ReleaseResult{}
	}
}

func b1AwaitSignal(t *testing.T, f *b1OwnedFixture) {
	t.Helper()
	select {
	case attempt := <-f.signals:
		if attempt.force || attempt.err != nil {
			t.Fatalf("graceful signal: force=%v error=%v", attempt.force, attempt.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("graceful signal syscall did not finish")
	}
}

func b1IgnoreEOF(t *testing.T, f *b1OwnedFixture) {
	t.Helper()
	f.inst.process.mu.RLock()
	stdin := f.inst.process.stdin
	f.inst.process.mu.RUnlock()
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	for _, role := range b1Roles {
		e := b1AwaitStatus(t, f.dir, role, func(e b1ConsoleEvidence) bool { return e.EOF && b1NoControl(e) })
		t.Logf("%s ignored stdin EOF and is still serving: %+v", role, e)
	}
}

func b1AllowCompletion(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "finish"), []byte("normal cleanup allowed"), 0600); err != nil {
		t.Fatal(err)
	}
}

func b1AssertCompletions(t *testing.T, dir string) {
	t.Helper()
	for _, role := range b1Roles {
		data, err := os.ReadFile(filepath.Join(dir, role+".complete"))
		var evidence b1ConsoleEvidence
		if err != nil {
			abnormal, _ := os.ReadFile(filepath.Join(dir, role+".abnormal"))
			t.Errorf("%s missing normal completion: %v (abnormal receipt=%s)", role, err, abnormal)
			continue
		}
		if err := json.Unmarshal(data, &evidence); err != nil || !evidence.Completed || !evidence.EOF || !b1OneControl(evidence) {
			t.Errorf("%s completion was not one Ctrl-C + ignored EOF + normal exit: %s error=%v", role, data, err)
		}
		t.Logf("%s completion evidence: %s", role, data)
		if treeReachable(treeAddress(t, dir, role)) {
			t.Errorf("%s listener survived normal completion", role)
		}
	}
}

func b1AssertReleasedTree(t *testing.T, f *b1OwnedFixture) {
	t.Helper()
	s := f.gate.Snapshot()
	if s.AllowAI || s.Release != "released" || s.ReleaseOperation == nil || s.ReleaseOperation.ErrorCode != "" {
		t.Errorf("graceful release snapshot lacks blocked/released evidence: %+v", s)
	}
	p := f.inst.process
	p.mu.RLock()
	tree, monitor := p.tree, p.monitorDone
	p.mu.RUnlock()
	select {
	case <-tree.done:
		if err := tree.result(); err != nil {
			t.Errorf("graceful root did not exit normally: %v", err)
		}
	default:
		t.Error("graceful release lacks actual Job-empty evidence before teardown")
	}
	if monitor != nil {
		select {
		case <-monitor:
		default:
			t.Error("graceful generation monitor did not finish")
		}
	}
	b1AssertCompletions(t, f.dir)
	b1AssertHeldExit(t, f.held, true)
	b1LogOwnedExit(t, tree, "graceful assertions")
	tree.mu.Lock()
	forces, quitSent := tree.forceRequests, tree.quitSent
	tree.mu.Unlock()
	if forces != 0 || !quitSent {
		t.Errorf("graceful tree used force or lacked normal helper quit: forces=%d quit=%v", forces, quitSent)
	}
	if n := f.forced.Load(); n != 0 {
		t.Errorf("graceful release issued %d force requests", n)
	}
}

func b1CheckGraceful(t *testing.T) {
	f := b1OwnedConsoleFixture(t, "graceful")
	b1IgnoreEOF(t, f)
	b1AllowCompletion(t, f.dir)
	result := b1AwaitRelease(t, b1Release(f, b1Pause(t, f)))
	if result.state != "released" || result.code != "" {
		t.Errorf("B1: EOF-ignoring Ctrl-C-aware tree must release gracefully, got %s/%s", result.state, result.code)
	}
	b1AssertReleasedTree(t, f)
}

func b1CheckStubborn(t *testing.T) {
	f := b1OwnedConsoleFixture(t, "stubborn")
	b1IgnoreEOF(t, f)
	b1AllowCompletion(t, f.dir) // Even this barrier must not make stubborn exit.
	started := time.Now()
	result := b1AwaitRelease(t, b1Release(f, b1Pause(t, f)))
	if result.state != "unreleased_unknown" || result.code != "release_timeout" {
		t.Errorf("stubborn result: %s/%s", result.state, result.code)
	}
	if time.Since(started) < resourcepolicy.GracefulTimeout-time.Second {
		t.Error("stubborn test did not exercise the actual 30s graceful deadline")
	}
	s := f.gate.Snapshot()
	if s.AllowAI || s.Release != "unreleased_unknown" || s.ReleaseOperation == nil || s.ReleaseOperation.ErrorCode != "release_timeout" {
		t.Errorf("stubborn deadline did not retain blocked/unknown: %+v", s)
	}
	select {
	case <-f.inst.process.tree.done:
		t.Error("stubborn tree exited at graceful deadline")
	default:
	}
	for _, role := range b1Roles {
		e, err := b1ReadStatus(treeAddress(t, f.dir, role))
		if err != nil || !e.EOF || !b1OneControl(e) {
			t.Errorf("%s must remain alive after ignoring one real Ctrl-C: %+v error=%v", role, e, err)
		}
		if _, err := os.Stat(filepath.Join(f.dir, role+".complete")); !os.IsNotExist(err) {
			t.Errorf("%s unexpectedly completed: %v", role, err)
		}
		t.Logf("%s after real graceful deadline: %+v error=%v", role, e, err)
	}
	if n := f.forced.Load(); n != 0 {
		t.Errorf("graceful deadline escalated to %d force requests", n)
	}
}

func b1CheckRepeated(t *testing.T) {
	f := b1OwnedConsoleFixture(t, "graceful")
	b1IgnoreEOF(t, f)
	first := b1Release(f, b1Pause(t, f))
	b1AwaitSignal(t, f)
	for _, role := range b1Roles {
		b1AwaitStatus(t, f.dir, role, b1OneControl)
	}
	s := f.gate.Snapshot()
	s, err := f.gate.RequestRelease(s.BootID, s.Revision, "second-graceful", "graceful")
	if err != nil {
		t.Fatal(err)
	}
	second := b1Release(f, b1NextTicket(t, f))
	b1AwaitSignal(t, f)
	if result := b1AwaitRelease(t, first); result.state == "released" {
		t.Fatal("superseded release reported success while cleanup was held")
	}
	repeat, err := f.gate.RequestRelease(s.BootID, s.Revision, "second-graceful", "graceful")
	if err != nil || repeat.ReleaseOperation == nil || repeat.ReleaseOperation.ID != "second-graceful" {
		t.Fatalf("idempotent repeat changed operation: %+v error=%v", repeat, err)
	}
	// Keep all handlers alive after the second accepted operation. A second
	// Ctrl-C causes abnormal fixture exit; no fast-exit false positive is possible.
	for deadline := time.Now().Add(300 * time.Millisecond); time.Now().Before(deadline); {
		for _, role := range b1Roles {
			e, err := b1ReadStatus(treeAddress(t, f.dir, role))
			if err != nil || !b1OneControl(e) {
				t.Fatalf("repeated graceful reached %s more than once: %+v error=%v", role, e, err)
			}
		}
		select {
		case <-f.tickets:
			t.Fatal("idempotent repeat created another release worker")
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	b1AllowCompletion(t, f.dir)
	if result := b1AwaitRelease(t, second); result.state != "released" || result.code != "" {
		t.Errorf("deduplicated graceful release: %s/%s", result.state, result.code)
	}
	b1AssertReleasedTree(t, f)
}

func b1CheckStale(t *testing.T) {
	f := b1OwnedConsoleFixture(t, "graceful")
	oldTicket := b1Pause(t, f)
	p := f.inst.process
	p.mu.RLock()
	oldTree, oldMonitor := p.tree, p.monitorDone
	p.mu.RUnlock()
	s := f.gate.Snapshot()
	if _, err := f.gate.Update(true, s.BootID, s.Revision); err != nil {
		t.Fatal(err)
	}
	// Explicit test setup, not graceful release evidence. This safety regression
	// must remain runnable even while the old EOF-only product is still RED.
	if err := oldTree.signal(true); err != nil {
		t.Fatal(err)
	}
	waitTreeGone(t, oldTree)
	waitTreeMonitor(t, oldMonitor)
	f.dir = t.TempDir()
	f.inst.globalBackendSettings.Custom["console-fixture"] = b1Backend(f.dir, "graceful")
	opts, err := f.inst.GetOptions().Clone()
	if err != nil {
		t.Fatal(err)
	}
	opts.BackendOptions.CustomServerOptions.Port = b1UnusedPort(t)
	if err := f.inst.SetOptions(opts); err != nil {
		t.Fatal(err)
	}
	if err := f.inst.Start(); err != nil {
		t.Fatal(err)
	}
	p.mu.RLock()
	newTree, newStdin, newContext := p.tree, p.stdin, p.ctx
	p.mu.RUnlock()
	for _, role := range b1Roles {
		b1AwaitStatus(t, f.dir, role, func(e b1ConsoleEvidence) bool { return b1NoControl(e) && !e.EOF })
	}
	if state, _ := f.inst.ReleaseOwned(oldTicket); state == "released" {
		t.Error("stale ticket reported released for the new generation")
	}
	if err := oldTree.signal(false); err != nil {
		t.Errorf("already exited generation signal: %v", err)
	}
	if newTree == oldTree || newContext.Err() != nil {
		t.Error("old release changed new generation identity/context")
	}
	if _, err := newStdin.(*os.File).Stat(); err != nil {
		t.Errorf("stale release closed new stdin: %v", err)
	}
	for deadline := time.Now().Add(300 * time.Millisecond); time.Now().Before(deadline); {
		for _, role := range b1Roles {
			e, err := b1ReadStatus(treeAddress(t, f.dir, role))
			if err != nil || !b1NoControl(e) || e.EOF {
				t.Fatalf("stale release affected new %s: %+v error=%v", role, e, err)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case attempt := <-f.signals:
		t.Errorf("stale ticket entered signal effect: %+v", attempt)
	default:
	}
	select {
	case <-newTree.done:
		t.Error("stale release stopped new tree")
	default:
	}
}

// Calibrate the fixture without relying on product graceful behavior. This is
// the only test-generated broadcast: all recipients are synthetic members of the
// private console created by b1StartHarness, never the invoking terminal console.
func b1CheckFixtureControl(t *testing.T, manager *b1ControlCounts) {
	dir := t.TempDir()
	cmd := b1FixtureCommand("root", "graceful", dir, "0")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	waited := false
	t.Cleanup(func() {
		if !waited {
			_ = cmd.Process.Kill()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("calibration root teardown did not complete")
			}
		}
		// Any orphan descendants are still in the outer containment Job;
		// its waiter detects, reports and terminates leaks on failure.
	})
	for _, role := range b1Roles {
		b1AwaitStatus(t, dir, role, func(e b1ConsoleEvidence) bool { return b1NoControl(e) && !e.EOF })
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	for _, role := range b1Roles {
		b1AwaitStatus(t, dir, role, func(e b1ConsoleEvidence) bool { return b1NoControl(e) && e.EOF })
	}
	b1AllowCompletion(t, dir)
	if ok, _, err := jobKernel.NewProc("GenerateConsoleCtrlEvent").Call(0, 0); ok == 0 {
		t.Fatalf("private-console fixture calibration Ctrl-C: %v", err)
	}
	select {
	case err := <-done:
		waited = true
		if err != nil {
			t.Errorf("fixture calibration did not exit normally: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("real Ctrl-C did not complete fixture calibration")
	}
	if evidence := manager.evidence(false); !b1OneControl(evidence) {
		t.Errorf("calibration console handler receipt: %+v", evidence)
	}
	b1AssertCompletions(t, dir)
}
