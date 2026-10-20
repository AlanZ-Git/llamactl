//go:build windows

package instance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestOwnerWindowsB1ExitEvidence(t *testing.T) {
	if (ownedExitEvidence{}).complete() {
		t.Fatal("missing held-process observations counted as completion")
	}
	finished := ownedExitEvidence{
		BackendKnown: true,
		Root:         ownedProcessEvidence{PID: 10, Wait: windows.WAIT_OBJECT_0},
		Helper:       ownedProcessEvidence{PID: 20, Wait: windows.WAIT_OBJECT_0},
	}
	if !finished.complete() {
		t.Fatal("empty Jobs plus both signaled handles were rejected")
	}
	for _, role := range []string{"root", "helper"} {
		t.Run(role+"-still-live", func(t *testing.T) {
			e := finished
			if role == "root" {
				e.Root.Wait = uint32(windows.WAIT_TIMEOUT)
			} else {
				e.Helper.Wait = uint32(windows.WAIT_TIMEOUT)
			}
			// Even an apparent zero exit code cannot override an unsignaled
			// process object after Job accounting has already reached zero.
			if e.complete() {
				t.Fatal("Active==0 fabricated process exit")
			}
		})
	}
	quitting := finished
	quitting.Helper.Wait = uint32(windows.WAIT_TIMEOUT)
	quitting.Supervisor = ownedJobEvidence{Active: 2, Assigned: 2, PIDs: []uintptr{20, 30}}
	if !quitting.mayQuitHelper() || quitting.complete() {
		t.Fatal("console-scope cardinality incorrectly substituted for backend scope")
	}
	for _, live := range []ownedJobEvidence{
		{Active: 1},
		{Assigned: 1, PIDs: []uintptr{40}},
	} {
		e := quitting
		e.Backend = live
		if e.mayQuitHelper() || e.complete() {
			t.Fatal("helper may quit with backend membership remaining", live)
		}
	}
	quitting.Root.Wait = uint32(windows.WAIT_TIMEOUT)
	if quitting.mayQuitHelper() {
		t.Fatal("helper may quit before held backend root exits")
	}
	unfinished := finished
	unfinished.Supervisor.PIDs = []uintptr{30}
	if unfinished.complete() {
		t.Fatal("empty accounting overrode a nonempty Job process list")
	}
}

func TestOwnerWindowsB1HelperWaitsForDescendants(t *testing.T) {
	b1RunConsoleTest(t, "helper-descendants")
}

// This test keeps actual handles, independent of the product's done channel and
// accounting parser. PID-based opens are observation-only, after checking the
// live fixture identity and Job membership; they are never termination targets.
type b1HeldProcesses struct {
	processes map[string]windows.Handle
	supervisor windows.Handle
	backend windows.Handle
}

func b1HoldFixtureProcesses(t *testing.T, f *b1OwnedFixture) *b1HeldProcesses {
	t.Helper()
	held := &b1HeldProcesses{processes: make(map[string]windows.Handle)}
	t.Cleanup(func() {
		for _, h := range held.processes {
			windows.CloseHandle(h)
		}
		if held.supervisor != 0 {
			windows.CloseHandle(held.supervisor)
		}
		if held.backend != 0 {
			windows.CloseHandle(held.backend)
		}
	})
	tree := f.inst.process.tree
	tree.mu.Lock()
	err := windows.DuplicateHandle(windows.CurrentProcess(), tree.job, windows.CurrentProcess(), &held.supervisor, 0, false, windows.DUPLICATE_SAME_ACCESS)
	if err == nil {
		err = windows.DuplicateHandle(windows.CurrentProcess(), tree.backendJob, windows.CurrentProcess(), &held.backend, 0, false, windows.DUPLICATE_SAME_ACCESS)
	}
	for role, source := range map[string]windows.Handle{"root": tree.process, "helper": tree.helper} {
		if err != nil {
			break
		}
		var copy windows.Handle
		err = windows.DuplicateHandle(windows.CurrentProcess(), source, windows.CurrentProcess(), &copy, 0, false, windows.DUPLICATE_SAME_ACCESS)
		if err == nil {
			held.processes[role] = copy
		}
	}
	tree.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range b1Roles {
		evidence, err := b1ReadStatus(treeAddress(t, f.dir, role))
		if err != nil || evidence.PID == 0 {
			t.Fatalf("missing live %s identity: %+v %v", role, evidence, err)
		}
		h := held.processes[role]
		if h == 0 {
			h, err = windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, evidence.PID)
			if err != nil {
				t.Fatal(err)
			}
			held.processes[role] = h
		}
		pid, err := windows.GetProcessId(h)
		if err != nil || pid != evidence.PID {
			t.Fatalf("%s held identity differs from live fixture: %d/%d %v", role, pid, evidence.PID, err)
		}
		for _, job := range []windows.Handle{held.supervisor, held.backend} {
			if member, err := processInOwnedJob(h, job); err != nil || !member {
				t.Fatalf("%s missing owned Job membership: %v %v", role, member, err)
			}
		}
		t.Logf("owned fixture identity: role=%s pid=%d supervisor=true backend=true", role, pid)
	}
	if member, err := processInOwnedJob(held.processes["helper"], held.supervisor); err != nil || !member {
		t.Fatal("helper lost supervisor membership", member, err)
	}
	if member, err := processInOwnedJob(held.processes["helper"], held.backend); err != nil || member {
		t.Fatal("helper incorrectly counted as a backend descendant", member, err)
	}
	b1LogJobMembers(t, "ready supervisor", held.supervisor)
	b1LogJobMembers(t, "ready backend", held.backend)
	return held
}

func b1LogJobMembers(t *testing.T, phase string, job windows.Handle) {
	t.Helper()
	evidence, err := observeOwnedJob(job)
	t.Logf("%s Job observation: %+v error=%v", phase, evidence, err)
	if err != nil {
		return
	}
	for _, id := range evidence.PIDs {
		if uint64(id) > uint64(^uint32(0)) {
			t.Logf("%s invalid diagnostic PID %d", phase, id)
			continue
		}
		h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(id))
		if err != nil {
			t.Logf("%s PID %d read-only open: %v", phase, id, err)
			continue
		}
		member, memberErr := processInOwnedJob(h, job)
		process, processErr := observeOwnedProcess(h)
		image := "unavailable"
		var imageErr error
		if memberErr == nil && member {
			buffer := make([]uint16, 32768)
			size := uint32(len(buffer))
			imageErr = windows.QueryFullProcessImageName(h, 0, &buffer[0], &size)
			if imageErr == nil {
				image = filepath.Base(windows.UTF16ToString(buffer[:size]))
			}
		}
		t.Logf("%s member=%v member_error=%v process=%+v process_error=%v image=%q image_error=%v", phase, member, memberErr, process, processErr, image, imageErr)
		windows.CloseHandle(h)
	}
}

func b1LogOwnedExit(t *testing.T, tree *ownedTree, phase string) {
	t.Helper()
	tree.mu.Lock()
	evidence, err := tree.lastExit, tree.evidenceErr
	if !tree.exited {
		evidence, err = tree.observeExitLocked()
	}
	beforeQuit, forces := tree.beforeQuit, tree.forceRequests
	tree.mu.Unlock()
	t.Logf("%s owned exit observation: %+v error=%v; before-helper-quit=%+v; force_requests=%d", phase, evidence, err, beforeQuit, forces)
}

func b1AssertHeldExit(t *testing.T, held *b1HeldProcesses, normal bool) {
	t.Helper()
	for role, h := range held.processes {
		evidence, err := observeOwnedProcess(h)
		t.Logf("held exit proof: role=%s observation=%+v error=%v", role, evidence, err)
		if err != nil || evidence.Wait != windows.WAIT_OBJECT_0 {
			t.Errorf("%s held process has not exited", role)
		} else if normal && evidence.ExitCode != 0 {
			t.Errorf("%s did not exit normally: %d", role, evidence.ExitCode)
		}
	}
	for name, job := range map[string]windows.Handle{"supervisor": held.supervisor, "backend": held.backend} {
		evidence, err := observeOwnedJob(job)
		t.Logf("held %s Job exit proof: %+v error=%v", name, evidence, err)
		if err != nil || !evidence.empty() {
			t.Errorf("%s Job is not empty", name)
		}
	}
}

func b1CheckHelperWaitsForDescendants(t *testing.T) {
	f := b1OwnedConsoleFixture(t, "graceful")
	tree := f.inst.process.tree
	if err := os.WriteFile(filepath.Join(f.dir, "leave-root"), []byte("exit root only"), 0600); err != nil {
		t.Fatal(err)
	}
	status, err := windows.WaitForSingleObject(f.held.processes["root"], 5000)
	if err != nil || status != windows.WAIT_OBJECT_0 {
		t.Fatal("root-only fixture exit was not observed", status, err)
	}
	for deadline := time.Now().Add(200 * time.Millisecond); time.Now().Before(deadline); {
		tree.mu.Lock()
		quit := tree.quitSent
		tree.mu.Unlock()
		if quit {
			t.Fatal("helper quit while backend descendants remained")
		}
		for _, role := range []string{"helper", "child", "grandchild"} {
			status, err := windows.WaitForSingleObject(f.held.processes[role], 0)
			if err != nil || status != uint32(windows.WAIT_TIMEOUT) {
				t.Fatal("descendant-retention fixture lost "+role, status, err)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	b1LogOwnedExit(t, tree, "root exited, descendants retained")
	b1AllowCompletion(t, f.dir)
	result := b1AwaitRelease(t, b1Release(f, b1Pause(t, f)))
	if result.state != "released" || result.code != "" {
		t.Error("remaining descendants did not release gracefully", result)
	}
	for _, role := range []string{"child", "grandchild"} {
		data, err := os.ReadFile(filepath.Join(f.dir, role+".complete"))
		if err != nil {
			t.Error("missing descendant completion", role, err)
			continue
		}
		var evidence b1ConsoleEvidence
		if err := json.Unmarshal(data, &evidence); err != nil || !evidence.Completed || !evidence.EOF || !b1OneControl(evidence) {
			t.Errorf("invalid descendant normal completion: %s %s error=%v", role, data, err)
		}
		t.Logf("descendant normal completion: %s %s", role, data)
	}
	b1AssertHeldExit(t, f.held, true)
	b1LogOwnedExit(t, tree, "descendants completed")
	tree.mu.Lock()
	forces, quit := tree.forceRequests, tree.quitSent
	tree.mu.Unlock()
	if forces != 0 || !quit {
		t.Errorf("descendant release lacked natural helper quit: forces=%d quit=%v", forces, quit)
	}
}
